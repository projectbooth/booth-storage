// Package seaweedfslease is booth-storage's own record of every SeaweedFS IAM user and
// access key the seaweedfs-iam credential-broker path (internal/backend/s3) has minted
// (ADR 0118 ruling 3): a bare IAM access key has no native expiry, unlike MinIO's
// expiring service accounts, so this module tracks a lease per mint and a sweeper
// (internal/seaweedfssweep) deletes what's expired. See docs/design-seaweedfs-
// provider.md section 4.
package seaweedfslease

import (
	"context"
	"time"
)

// Lease is one minted SeaweedFS IAM user+key. Recorded before the credential is ever
// returned to a caller (internal/backend/s3.MintRequest.RecordLease) — a crash between
// minting and recording would otherwise leave an untracked, unsweepable credential.
type Lease struct {
	ID          string
	Workspace   string
	BackendID   string
	IAMUserName string
	AccessKeyID string
	Prefix      string // "" or ends in "/" — see backend.CleanPrefix
	Access      string // "read" | "readwrite"
	ExpiresAt   time.Time
	CreatedAt   time.Time
	SweptAt     *time.Time // nil until the sweeper confirms the user and key are gone
}

// Store persists Leases. Implementations must be safe for concurrent use.
type Store interface {
	// Record inserts one freshly minted lease. l.ID must be unique (the broker's own
	// lease ID, shared with the credential response).
	Record(ctx context.Context, l Lease) error
	// PendingSweep returns every lease whose ExpiresAt is before now and that hasn't
	// been confirmed swept yet, oldest ExpiresAt first, up to limit entries.
	PendingSweep(ctx context.Context, now time.Time, limit int) ([]Lease, error)
	// MarkSwept confirms a lease's SeaweedFS-side user and key are gone. A lease that no
	// longer exists (already deleted some other way) is not an error.
	MarkSwept(ctx context.Context, id string, at time.Time) error
	// ActiveCount reports how many leases are neither swept nor yet expired — the
	// sweeper's own informational "how many live leases right now" figure (ADR 0118's
	// "metric for outstanding users").
	ActiveCount(ctx context.Context, now time.Time) (int, error)
	// Ping reports whether the store is reachable.
	Ping(ctx context.Context) error
}
