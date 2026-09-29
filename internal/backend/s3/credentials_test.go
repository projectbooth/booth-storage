package s3

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// ---- pure logic: no real MinIO needed ------------------------------------------------

func TestMintScopedCredential_RealAWSIsAlwaysRefused(t *testing.T) {
	cfg := Config{Bucket: "prod"} // Endpoint == "" means real AWS S3
	admin := Credentials{AccessKeyID: "AKIA...", SecretAccessKey: "secret"}

	cases := []struct {
		name              string
		allowSessionToken bool
	}{
		{"bare pair requested (Lakekeeper's static-key shape) — structurally impossible on AWS", false},
		{"session token allowed — not implemented (no assume-role-arn configured)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cfg.MintScopedCredential(context.Background(), admin, MintRequest{
				Path: "t1", Access: AccessRead, TTL: 5 * time.Minute, AllowSessionToken: tc.allowSessionToken,
			})
			if !errors.Is(err, ErrScopeNotSupported) {
				t.Fatalf("err = %v, want ErrScopeNotSupported", err)
			}
		})
	}
}

func TestMintScopedCredential_ValidatesBeforeTouchingAnything(t *testing.T) {
	cfg := Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake"}
	admin := Credentials{AccessKeyID: "admin", SecretAccessKey: "adminsecret"}

	cases := []struct {
		name string
		req  MintRequest
	}{
		{"bad access value", MintRequest{Path: "t1", Access: "admin", TTL: time.Minute}},
		{"path traversal", MintRequest{Path: "../../etc", Access: AccessRead, TTL: time.Minute}},
		{"leading slash", MintRequest{Path: "/t1", Access: AccessRead, TTL: time.Minute}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cfg.MintScopedCredential(context.Background(), admin, tc.req)
			if !errors.Is(err, ErrScopeNotSupported) {
				t.Fatalf("err = %v, want ErrScopeNotSupported", err)
			}
		})
	}
}

func TestMintScopedCredential_NoAdminCredentialsIsRefused(t *testing.T) {
	cfg := Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake"}
	_, err := cfg.MintScopedCredential(context.Background(), Credentials{}, MintRequest{Path: "t1", Access: AccessRead, TTL: time.Minute})
	if !errors.Is(err, ErrScopeNotSupported) {
		t.Fatalf("err = %v, want ErrScopeNotSupported", err)
	}
}

// scopedPolicy is unexported but tested directly (same package) — this is the shape that
// must match MinIO's two real quirks found by booth-lakehouse's ADR 0084 first pass:
// GetBucketLocation as its own unconditional statement, and ListBucket's StringLike
// condition carrying the prefix instead.
func TestScopedPolicy(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		pol := scopedPolicy("lake", "warehouses/t1", AccessRead)
		stmts := pol["Statement"].([]map[string]any)
		if len(stmts) != 3 {
			t.Fatalf("got %d statements, want 3", len(stmts))
		}
		actions := stmts[0]["Action"].([]string)
		if len(actions) != 1 || actions[0] != "s3:GetObject" {
			t.Errorf("read actions = %v, want just GetObject", actions)
		}
		if got := stmts[0]["Resource"].([]string)[0]; got != "arn:aws:s3:::lake/warehouses/t1/*" {
			t.Errorf("object resource = %q", got)
		}
		locStmt := stmts[1]
		if _, hasCondition := locStmt["Condition"]; hasCondition {
			t.Error("GetBucketLocation statement must have no Condition at all — MinIO rejects s3:prefix on it")
		}
		if got := locStmt["Resource"].([]string)[0]; got != "arn:aws:s3:::lake" {
			t.Errorf("GetBucketLocation resource = %q, want the bucket, not a prefixed key", got)
		}
		cond := stmts[2]["Condition"].(map[string]any)["StringLike"].(map[string]any)["s3:prefix"].([]string)
		if len(cond) != 2 || cond[0] != "warehouses/t1" || cond[1] != "warehouses/t1/*" {
			t.Errorf("ListBucket condition = %v", cond)
		}
	})

	t.Run("readwrite adds write actions", func(t *testing.T) {
		pol := scopedPolicy("lake", "t1", AccessReadWrite)
		actions := pol["Statement"].([]map[string]any)[0]["Action"].([]string)
		want := map[string]bool{"s3:GetObject": true, "s3:PutObject": true, "s3:DeleteObject": true, "s3:AbortMultipartUpload": true}
		if len(actions) != len(want) {
			t.Fatalf("actions = %v", actions)
		}
		for _, a := range actions {
			if !want[a] {
				t.Errorf("unexpected action %q", a)
			}
		}
	})

	// A backend/request with no prefix at all (whole-bucket grant) must not produce a
	// leading-slash resource ("bucket//*"), which would match no real object key and
	// silently deny everything.
	t.Run("empty prefix means the whole bucket, not a leading slash", func(t *testing.T) {
		pol := scopedPolicy("lake", "", AccessRead)
		stmts := pol["Statement"].([]map[string]any)
		if got := stmts[0]["Resource"].([]string)[0]; got != "arn:aws:s3:::lake/*" {
			t.Errorf("object resource = %q, want arn:aws:s3:::lake/*", got)
		}
		cond := stmts[2]["Condition"].(map[string]any)["StringLike"].(map[string]any)["s3:prefix"].([]string)
		if len(cond) != 1 || cond[0] != "*" {
			t.Errorf("ListBucket condition = %v, want just a wildcard", cond)
		}
	})
}

// ---- real MinIO: expiring service accounts, actually enforced ------------------------
//
// Same real-dependency policy as s3_test.go: the available in-process S3 fakes don't
// implement enough of MinIO's admin API (service accounts, policy enforcement) to be
// worth trusting here, so this needs BOOTH_TEST_S3_ENDPOINT (see hack/docker-compose.
// emulators.yml) and is skipped, not faked, without it.

func TestMintScopedCredential_RealMinIO(t *testing.T) {
	env := requireMinIO(t)
	bucket := env.newBucket(t)
	admin := Credentials{AccessKeyID: env.access, SecretAccessKey: env.secret}
	cfg := Config{Endpoint: env.endpoint, Bucket: bucket, Prefix: "warehouses", PathStyle: true}
	ctx := context.Background()

	// Seed one object inside the granted scope and one outside it, so "in scope" isn't
	// vacuously true because nothing exists yet.
	full := env.backend(t, bucket, "warehouses")
	if _, err := full.Write(ctx, "t1/data.parquet", bytes.NewReader([]byte("inside")), backend.WriteOptions{}); err != nil {
		t.Fatalf("seeding in-scope object: %v", err)
	}
	outside := env.backend(t, bucket, "other")
	if _, err := outside.Write(ctx, "secret.txt", bytes.NewReader([]byte("outside")), backend.WriteOptions{}); err != nil {
		t.Fatalf("seeding out-of-scope object: %v", err)
	}

	minted, err := cfg.MintScopedCredential(ctx, admin, MintRequest{Path: "t1", Access: AccessRead, TTL: time.Minute, AllowSessionToken: false})
	if err != nil {
		t.Fatalf("MintScopedCredential: %v", err)
	}

	if minted.SessionToken != "" {
		t.Errorf("session token = %q, want none for AllowSessionToken=false", minted.SessionToken)
	}
	if minted.AccessKeyID == admin.AccessKeyID || minted.SecretAccessKey == admin.SecretAccessKey {
		t.Error("minted credential must not be the admin's own")
	}
	if minted.Bucket != bucket || minted.Endpoint != env.endpoint || !minted.PathStyle || minted.KeyPrefix != "warehouses/t1" {
		t.Errorf("resolved location = %+v", minted)
	}
	// MinIO's own floor (booth-lakehouse's ADR 0084 first pass): a request for 1 minute is
	// clamped up to at least 15.
	if until := time.Until(minted.ExpiresAt); until < 14*time.Minute {
		t.Errorf("expiresAt = %v (%v from now), want clamped up to at least MinIO's 15-minute floor", minted.ExpiresAt, until)
	}

	client, err := minio.New(strings.TrimPrefix(env.endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(minted.AccessKeyID, minted.SecretAccessKey, minted.SessionToken), BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.StatObject(ctx, bucket, "warehouses/t1/data.parquet", minio.StatObjectOptions{}); err != nil {
		t.Errorf("granted credential could not read the in-scope object: %v", err)
	}
	if _, err := client.StatObject(ctx, bucket, "warehouses/other/secret.txt", minio.StatObjectOptions{}); err == nil {
		t.Error("granted credential could read an object outside its scope — the policy is too wide")
	}
	if _, err := client.PutObject(ctx, bucket, "warehouses/t1/write-attempt.txt", bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err == nil {
		t.Error("a read-only grant could write — the policy is too wide")
	}
}

func TestMintScopedCredential_RealMinIO_ReadWriteAndOptionsAllowed(t *testing.T) {
	env := requireMinIO(t)
	bucket := env.newBucket(t)
	admin := Credentials{AccessKeyID: env.access, SecretAccessKey: env.secret}
	cfg := Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true}
	ctx := context.Background()

	minted, err := cfg.MintScopedCredential(ctx, admin, MintRequest{Path: "", Access: AccessReadWrite, TTL: 20 * time.Minute, AllowSessionToken: true})
	if err != nil {
		t.Fatalf("MintScopedCredential: %v", err)
	}
	// A 20-minute request is already above the 15-minute floor, so it should come back
	// close to what was asked, not silently shortened or lengthened.
	if until := time.Until(minted.ExpiresAt); until < 18*time.Minute || until > 21*time.Minute {
		t.Errorf("expiresAt = %v from now, want close to the requested 20 minutes", until)
	}

	client, err := minio.New(strings.TrimPrefix(env.endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(minted.AccessKeyID, minted.SecretAccessKey, minted.SessionToken), BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(ctx, bucket, "root-level.txt", bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err != nil {
		t.Errorf("a readwrite grant on the whole bucket (no prefix) could not write: %v", err)
	}
}
