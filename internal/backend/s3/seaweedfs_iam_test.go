package s3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ---- pure logic: no real SeaweedFS needed ---------------------------------------------

func TestConfig_Validate_UnknownCredentialProviderRejected(t *testing.T) {
	cfg := Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake", CredentialProvider: "bogus"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want an error for an unrecognized credentialProvider")
	}
}

func TestConfig_Validate_KnownCredentialProvidersAccepted(t *testing.T) {
	for _, cp := range []string{"", CredentialProviderMinIOAdmin, CredentialProviderSeaweedFSIAM} {
		cfg := Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake", CredentialProvider: cp}
		if err := cfg.Validate(); err != nil {
			t.Errorf("credentialProvider %q: %v", cp, err)
		}
	}
}

func TestMintScopedCredential_SeaweedFSRequiresEndpoint(t *testing.T) {
	cfg := Config{Bucket: "lake", CredentialProvider: CredentialProviderSeaweedFSIAM} // Endpoint == ""
	admin := Credentials{AccessKeyID: "admin", SecretAccessKey: "adminsecret"}
	_, err := cfg.MintScopedCredential(context.Background(), admin, MintRequest{Path: "t1", Access: AccessRead, TTL: time.Minute})
	if !errors.Is(err, ErrScopeNotSupported) {
		t.Fatalf("err = %v, want ErrScopeNotSupported", err)
	}
}

// Regression (booth-spark, 2026-10-16): AllowSessionToken=true used to be refused
// outright on the seaweedfs-iam path — but "allowed" means "fine if the credential
// naturally has one, not required" (see MintRequest.AllowSessionToken's own doc
// comment), the same way the MinIO path reads it; a bare key already satisfies that.
// Spark's planner sends options.sessionToken "allowed", and the credential sidecars
// send no options at all (also "allowed", the more permissive default) — both were
// broken by the old refusal. This must NOT come back as ErrScopeNotSupported: proven
// here by pointing at an address nothing listens on, so a mint that actually proceeds
// fails with a network-level error instead — a different failure than the old
// short-circuit, and proof the request got past the AllowSessionToken gate at all.
func TestMintScopedCredential_SeaweedFSAllowSessionTokenIsNotRefused(t *testing.T) {
	cfg := Config{Endpoint: "http://127.0.0.1:1", Bucket: "lake", CredentialProvider: CredentialProviderSeaweedFSIAM}
	admin := Credentials{AccessKeyID: "admin", SecretAccessKey: "adminsecret"}
	_, err := cfg.MintScopedCredential(context.Background(), admin, MintRequest{
		Path: "t1", Access: AccessRead, TTL: time.Minute, AllowSessionToken: true,
		RecordLease: func(context.Context, SeaweedFSLease) error { return nil },
	})
	if err == nil {
		t.Fatal("expected a network-level failure against an unreachable endpoint, got nil")
	}
	if errors.Is(err, ErrScopeNotSupported) {
		t.Fatalf("err = %v, want a network failure, not ErrScopeNotSupported — AllowSessionToken=true must not be refused on this path", err)
	}
}

// No real server is reachable at this address, so if this test ever got as far as
// attempting a real IAM call, it would hang/fail slowly rather than fail instantly —
// confirming RecordLease is checked before any network call, not just that the overall
// mint eventually fails.
func TestMintScopedCredential_SeaweedFSNoLeaseRecorderRefusedWithoutTouchingNetwork(t *testing.T) {
	cfg := Config{Endpoint: "http://127.0.0.1:1", Bucket: "lake", CredentialProvider: CredentialProviderSeaweedFSIAM}
	admin := Credentials{AccessKeyID: "admin", SecretAccessKey: "adminsecret"}

	start := time.Now()
	_, err := cfg.MintScopedCredential(context.Background(), admin, MintRequest{
		Path: "t1", Access: AccessRead, TTL: time.Minute, RecordLease: nil,
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v — looks like it tried to reach the network instead of refusing immediately", elapsed)
	}
	if !errors.Is(err, ErrScopeNotSupported) {
		t.Fatalf("err = %v, want ErrScopeNotSupported", err)
	}
}

// scopedPolicy is reused verbatim for the seaweedfs-iam path's attached IAM policy
// (section 2 of docs/design-seaweedfs-provider.md). SeaweedFS rejects a policy
// containing s3:ListMultipartUploadParts outright (MalformedPolicyDocument, coordinator
// ruling 2026-10-16) — this must stay true even if scopedPolicy's action list changes.
func TestScopedPolicy_NeverListsMultipartUploadParts(t *testing.T) {
	for _, access := range []string{AccessRead, AccessReadWrite} {
		pol := scopedPolicy("lake", "t1", access)
		for _, stmt := range pol["Statement"].([]map[string]any) {
			actions, ok := stmt["Action"].([]string)
			if !ok {
				continue
			}
			for _, a := range actions {
				if a == "s3:ListMultipartUploadParts" {
					t.Fatalf("access %q: policy must never grant s3:ListMultipartUploadParts (SeaweedFS rejects it)", access)
				}
			}
		}
	}
}

// ---- real SeaweedFS: embedded IAM, actually enforced -----------------------------------
//
// Same real-dependency policy as s3_test.go/requireMinIO: the IAM API's prefix
// enforcement and the bare key's lack of an Expiration field are genuine server
// behavior, not something worth faking. Needs BOOTH_TEST_SEAWEEDFS_ENDPOINT/
// BOOTH_TEST_SEAWEEDFS_ADMIN_ACCESS_KEY/BOOTH_TEST_SEAWEEDFS_ADMIN_SECRET_KEY (see
// hack/docker-compose.emulators.yml) and is skipped, not faked, without them.

type seaweedfsEnv struct {
	endpoint, adminAccess, adminSecret string
}

func requireSeaweedFS(t *testing.T) seaweedfsEnv {
	t.Helper()
	env := seaweedfsEnv{
		endpoint:    os.Getenv("BOOTH_TEST_SEAWEEDFS_ENDPOINT"),
		adminAccess: os.Getenv("BOOTH_TEST_SEAWEEDFS_ADMIN_ACCESS_KEY"),
		adminSecret: os.Getenv("BOOTH_TEST_SEAWEEDFS_ADMIN_SECRET_KEY"),
	}
	if env.endpoint == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_SEAWEEDFS_ENDPOINT is not set")
		}
		t.Skip("BOOTH_TEST_SEAWEEDFS_ENDPOINT not set; skipping SeaweedFS IAM tests (see hack/docker-compose.emulators.yml)")
	}
	return env
}

func (e seaweedfsEnv) iamClient(t *testing.T, cfg Config) *iam.Client {
	t.Helper()
	client, err := cfg.seaweedfsIAMClient(Credentials{AccessKeyID: e.adminAccess, SecretAccessKey: e.adminSecret})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var seaweedfsBucketSeq int64

func (e seaweedfsEnv) newBucket(t *testing.T) string {
	t.Helper()
	seaweedfsBucketSeq++
	name := "seaweedfs-iam-test-" + time.Now().Format("150405") + "-" + string(rune('a'+seaweedfsBucketSeq%26))
	c, err := minio.New(strings.TrimPrefix(e.endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(e.adminAccess, e.adminSecret, ""), BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MakeBucket(context.Background(), name, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("creating test bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = c.RemoveObject(context.Background(), name, "t1/data.txt", minio.RemoveObjectOptions{})
		_ = c.RemoveBucket(context.Background(), name)
	})
	return name
}

func TestMintScopedCredential_SeaweedFSIAM_FullLifecycle(t *testing.T) {
	env := requireSeaweedFS(t)
	bucket := env.newBucket(t)
	admin := Credentials{AccessKeyID: env.adminAccess, SecretAccessKey: env.adminSecret}
	cfg := Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true, CredentialProvider: CredentialProviderSeaweedFSIAM}
	ctx := context.Background()

	var recorded SeaweedFSLease
	minted, err := cfg.MintScopedCredential(ctx, admin, MintRequest{
		Path: "t1", Access: AccessReadWrite, TTL: 90 * time.Second,
		RecordLease: func(_ context.Context, lease SeaweedFSLease) error {
			recorded = lease
			return nil
		},
	})
	if err != nil {
		t.Fatalf("MintScopedCredential: %v", err)
	}
	if recorded.IAMUserName == "" || recorded.AccessKeyID != minted.AccessKeyID {
		t.Fatalf("lease not recorded correctly: %+v (minted access key %q)", recorded, minted.AccessKeyID)
	}
	if minted.SessionToken != "" {
		t.Errorf("session token = %q, want none (bare pair only)", minted.SessionToken)
	}
	if got, want := minted.ExpiresAt.Sub(time.Now()), 90*time.Second; got > want || got < want-10*time.Second {
		t.Errorf("expiry ~%v from now, want ~90s (no 15-minute MinIO-style floor on this path)", got)
	}

	scoped, err := minio.New(strings.TrimPrefix(env.endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(minted.AccessKeyID, minted.SecretAccessKey, ""), BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.PutObject(ctx, bucket, "t1/data.txt", strings.NewReader("hello"), 5, minio.PutObjectOptions{}); err != nil {
		t.Fatalf("PUT inside granted prefix should succeed: %v", err)
	}
	if _, err := scoped.PutObject(ctx, bucket, "other/data.txt", strings.NewReader("hello"), 5, minio.PutObjectOptions{}); err == nil {
		t.Error("PUT outside the granted prefix should be refused, got no error")
	}

	// Sweeper-equivalent cleanup, then confirm the user is actually gone (not just that
	// the call returned no error): GetUser against the real admin IAM client.
	if err := DeleteSeaweedFSIAMLease(ctx, cfg, admin, recorded.IAMUserName, recorded.AccessKeyID); err != nil {
		t.Fatalf("DeleteSeaweedFSIAMLease: %v", err)
	}
	iamClient := env.iamClient(t, cfg)
	if _, err := iamClient.GetUser(ctx, &iam.GetUserInput{UserName: aws.String(recorded.IAMUserName)}); err == nil {
		t.Error("iam user still exists after DeleteSeaweedFSIAMLease")
	}
	// Idempotent: deleting an already-deleted lease (the "a previous sweep crashed
	// before marking swept_at" case) must succeed, not error.
	if err := DeleteSeaweedFSIAMLease(ctx, cfg, admin, recorded.IAMUserName, recorded.AccessKeyID); err != nil {
		t.Errorf("second DeleteSeaweedFSIAMLease (already gone) = %v, want nil (idempotent)", err)
	}
}

// "Test connection" (handleAdminTestNew/TestSaved, internal/api/server.go) is just
// Backend.Check against whatever credentials were given — credentialProvider only
// changes how MintScopedCredential later mints a *scoped* key, never how this module
// itself authenticates. So a seaweedfs-iam-configured backend must already pass Check
// using nothing but the bare admin access key/secret, with no session token at all —
// confirming that directly here, against the real server.
func TestCheck_SeaweedFSIAMBackend_AdminKeyOnlyNoSessionToken(t *testing.T) {
	env := requireSeaweedFS(t)
	bucket := env.newBucket(t)
	admin := Credentials{AccessKeyID: env.adminAccess, SecretAccessKey: env.adminSecret} // no SessionToken
	cfg := Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true, CredentialProvider: CredentialProviderSeaweedFSIAM}

	b, err := New(cfg, admin)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Check(context.Background()); err != nil {
		t.Fatalf("Check (what \"Test connection\" calls) = %v, want success using only the admin key", err)
	}
}

// Real-server confirmation of the fix, for each session-token mode a real caller
// actually sends (booth-spark's planner: "allowed"; the credential sidecars: no options
// at all, i.e. also "allowed"; Lakekeeper: "forbidden" — see
// internal/credentialbroker's provider_test.go for the same three shapes exercised
// through the broker handler itself, not just this backend).
func TestMintScopedCredential_SeaweedFSIAM_AllowSessionTokenMintsBareKey(t *testing.T) {
	env := requireSeaweedFS(t)
	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("AllowSessionToken=%v", allow), func(t *testing.T) {
			bucket := env.newBucket(t)
			admin := Credentials{AccessKeyID: env.adminAccess, SecretAccessKey: env.adminSecret}
			cfg := Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true, CredentialProvider: CredentialProviderSeaweedFSIAM}

			var recorded SeaweedFSLease
			minted, err := cfg.MintScopedCredential(context.Background(), admin, MintRequest{
				Path: "t1", Access: AccessRead, TTL: time.Minute, AllowSessionToken: allow,
				RecordLease: func(_ context.Context, lease SeaweedFSLease) error { recorded = lease; return nil },
			})
			if err != nil {
				t.Fatalf("MintScopedCredential(AllowSessionToken=%v) = %v, want success", allow, err)
			}
			if minted.AccessKeyID == "" || minted.SecretAccessKey == "" {
				t.Fatalf("minted = %+v, want a real bare key", minted)
			}
			if minted.SessionToken != "" {
				t.Errorf("SessionToken = %q, want none — seaweedfs-iam never issues one, allowed or not", minted.SessionToken)
			}
			_ = DeleteSeaweedFSIAMLease(context.Background(), cfg, admin, recorded.IAMUserName, recorded.AccessKeyID)
		})
	}
}

func TestMintScopedCredential_SeaweedFSIAM_UndoOnRecordFailure(t *testing.T) {
	env := requireSeaweedFS(t)
	bucket := env.newBucket(t)
	admin := Credentials{AccessKeyID: env.adminAccess, SecretAccessKey: env.adminSecret}
	cfg := Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true, CredentialProvider: CredentialProviderSeaweedFSIAM}
	ctx := context.Background()

	recordErr := errors.New("injected lease-store failure")
	var mintedUsername string
	_, err := cfg.MintScopedCredential(ctx, admin, MintRequest{
		Path: "t1", Access: AccessRead, TTL: time.Minute,
		RecordLease: func(_ context.Context, lease SeaweedFSLease) error {
			mintedUsername = lease.IAMUserName
			return recordErr
		},
	})
	if err == nil {
		t.Fatal("want an error when RecordLease fails")
	}
	if mintedUsername == "" {
		t.Fatal("RecordLease was never called — nothing to verify was undone")
	}

	iamClient := env.iamClient(t, cfg)
	if _, err := iamClient.GetUser(ctx, &iam.GetUserInput{UserName: aws.String(mintedUsername)}); err == nil {
		t.Error("iam user still exists after a failed RecordLease — mint should have been undone")
	}
}
