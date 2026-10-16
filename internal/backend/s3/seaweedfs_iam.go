package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/google/uuid"
)

// ADR 0118: SeaweedFS is the homelab S3 store; booth-storage mints scoped IAM keys for
// it rather than using STS (docs/design-seaweedfs-provider.md). SeaweedFS's embedded IAM
// API is served on the same port as its S3 API (confirmed by both prior spikes,
// docs/design-homelab-warehouse-store.md and docs/design-seaweedfs-iam-spike.md), so this
// file reuses cfg.hostAndTLS() exactly as the MinIO admin path does, just pointed at an
// IAM-shaped AWS SDK client instead of madmin.

// defaultSeaweedFSLeaseTTL is used only when a mint request carries no usable TTL at all
// (the broker always sets one — ADR 0088's 5-minute default — so this is a defensive
// floor against a degenerate zero/negative value, not a real capability limit the way
// MinIO's 15-minute service-account minimum is: the seaweedfs-iam path has no such floor
// and otherwise honors req.TTL exactly, per the design note).
const defaultSeaweedFSLeaseTTL = 5 * time.Minute

// seaweedfsIAMClient builds an AWS IAM client pointed at this backend's own S3/IAM
// endpoint, authenticated as admin. Credentials are supplied directly (never resolved
// from the environment or a shared config file) so this never picks up unrelated
// ambient AWS credentials on the host running this process.
func (cfg Config) seaweedfsIAMClient(admin Credentials) (*iam.Client, error) {
	host, secure, err := cfg.hostAndTLS()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScopeNotSupported, err)
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	endpoint := scheme + "://" + host
	awsCfg := aws.Config{
		Region:      "us-east-1", // SeaweedFS ignores this; the SDK requires a non-empty value
		Credentials: awscreds.NewStaticCredentialsProvider(admin.AccessKeyID, admin.SecretAccessKey, ""),
	}
	return iam.NewFromConfig(awsCfg, func(o *iam.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	}), nil
}

// mintSeaweedFSIAM mints an ephemeral IAM user, attaches a policy scoped to keyPrefix,
// and issues that user's one access key — a genuine bare access-key/secret-key pair with
// no session token and no Expiration field (confirmed by both prior spikes). The lease
// is recorded via req.RecordLease before this returns; any failure after the user is
// created unwinds it rather than leaving an unsweepable credential live on SeaweedFS.
func (cfg Config) mintSeaweedFSIAM(ctx context.Context, admin Credentials, req MintRequest, keyPrefix string) (MintedCredential, error) {
	if req.RecordLease == nil {
		return MintedCredential{}, fmt.Errorf("%w: no lease recorder configured for the seaweedfs-iam credential path", ErrScopeNotSupported)
	}
	client, err := cfg.seaweedfsIAMClient(admin)
	if err != nil {
		return MintedCredential{}, err
	}

	username := "booth-" + uuid.NewString()
	if _, err := client.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String(username)}); err != nil {
		return MintedCredential{}, fmt.Errorf("creating seaweedfs iam user: %w", err)
	}

	pol, err := json.Marshal(scopedPolicy(cfg.Bucket, keyPrefix, req.Access))
	if err != nil {
		_, _ = client.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String(username)})
		return MintedCredential{}, fmt.Errorf("building scoped policy: %w", err)
	}
	// scopedPolicy's action list (s3:GetObject, plus s3:PutObject/s3:DeleteObject/
	// s3:AbortMultipartUpload for readwrite) never includes s3:ListMultipartUploadParts —
	// SeaweedFS rejects that action with MalformedPolicyDocument (coordinator ruling,
	// 2026-10-16) — see TestScopedPolicy_NeverListsMultipartUploadParts.
	if _, err := client.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName: aws.String(username), PolicyName: aws.String("scope"), PolicyDocument: aws.String(string(pol)),
	}); err != nil {
		_, _ = client.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String(username)})
		return MintedCredential{}, fmt.Errorf("attaching scoped policy: %w", err)
	}

	created, err := client.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String(username)})
	if err != nil {
		_, _ = client.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String(username)})
		return MintedCredential{}, fmt.Errorf("creating seaweedfs iam access key: %w", err)
	}
	accessKeyID := aws.ToString(created.AccessKey.AccessKeyId)
	secretAccessKey := aws.ToString(created.AccessKey.SecretAccessKey)

	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultSeaweedFSLeaseTTL
	}
	now := time.Now()
	expiry := now.Add(ttl)

	if err := req.RecordLease(ctx, SeaweedFSLease{
		IAMUserName: username, AccessKeyID: accessKeyID, Prefix: keyPrefix, Access: req.Access,
		ExpiresAt: expiry, CreatedAt: now,
	}); err != nil {
		_, _ = client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(username), AccessKeyId: aws.String(accessKeyID)})
		_, _ = client.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String(username)})
		return MintedCredential{}, fmt.Errorf("recording seaweedfs lease (mint undone): %w", err)
	}

	return MintedCredential{
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey,
		Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, KeyPrefix: keyPrefix, PathStyle: cfg.PathStyle,
		ExpiresAt: expiry,
	}, nil
}

// DeleteSeaweedFSIAMLease removes one previously minted IAM user and access key — the
// sweeper's own cleanup call (internal/seaweedfssweep), and the same operation
// mintSeaweedFSIAM's own undo path performs inline on a record failure. Key first, then
// user: a user with an attached access key typically can't be deleted until the key is
// removed (mirroring real AWS IAM, which SeaweedFS's IAM API is modeled on); deleting
// the key first also means a crash before the DeleteUser call leaves a harmless keyless
// orphaned user rather than a meaningless userless orphaned key.
//
// "No such user"/"no such key" is treated as success: idempotent, since a previous sweep
// may have deleted this lease's SeaweedFS state already and crashed before marking it
// swept.
func DeleteSeaweedFSIAMLease(ctx context.Context, cfg Config, admin Credentials, iamUserName, accessKeyID string) error {
	client, err := cfg.seaweedfsIAMClient(admin)
	if err != nil {
		return err
	}
	if _, err := client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(iamUserName), AccessKeyId: aws.String(accessKeyID)}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("deleting seaweedfs iam access key: %w", err)
	}
	if _, err := client.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String(iamUserName)}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("deleting seaweedfs iam user: %w", err)
	}
	return nil
}

func isNoSuchEntity(err error) bool {
	var nse *types.NoSuchEntityException
	return errors.As(err, &nse)
}
