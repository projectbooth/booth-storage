// Package seaweedfssweep is the sweeper ADR 0118 ruling 3 requires: a bare SeaweedFS IAM
// access key has no native expiry, unlike MinIO's expiring service accounts, so this
// module deletes what internal/seaweedfslease records as expired. Runs in-process
// alongside the HTTP server (cmd/storage/main.go) — no new deployment unit.
package seaweedfssweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/s3"
	"github.com/projectbooth/booth-storage/internal/registry"
	"github.com/projectbooth/booth-storage/internal/seaweedfslease"
)

// DefaultInterval is docs/design-seaweedfs-provider.md section 4's recommended sweep
// cadence: short enough that "lease plus one sweep interval" (ADR 0118 ruling 3) stays
// tight against the broker's 5-minute default TTL ceiling (ADR 0088), not so short it
// hammers SeaweedFS's IAM API with mostly-empty polls.
const DefaultInterval = time.Minute

// batchSize bounds one cycle's work so a huge backlog can't make a single cycle run
// forever and starve the next poll; the next cycle picks up whatever's left.
const batchSize = 200

// Deps is what the sweeper needs.
type Deps struct {
	Leases   seaweedfslease.Store
	Registry *registry.Service
	// Interval defaults to DefaultInterval when zero.
	Interval time.Duration
	// Now is overridden in tests; defaults to time.Now.
	Now func() time.Time
}

// Run polls on Deps.Interval until ctx is cancelled, sweeping expired leases each cycle.
// Blocking — call it in its own goroutine.
func Run(ctx context.Context, deps Deps) {
	deps = withDefaults(deps)
	t := time.NewTicker(deps.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepOnce(ctx, deps)
		}
	}
}

func withDefaults(deps Deps) Deps {
	if deps.Interval <= 0 {
		deps.Interval = DefaultInterval
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return deps
}

// cycleResult is sweepOnce's own return, for tests — production only logs it.
type cycleResult struct {
	swept, failed, active int
	oldestBacklogAge      time.Duration
}

// sweepOnce runs exactly one cycle: fail-closed (ADR 0118 ruling 3) means one lease that
// can't be cleaned up (SeaweedFS unreachable, a real API error) is logged and left for
// the next cycle — it never stops other leases in the batch from being swept, and never
// crashes the process or falsely marks success.
func sweepOnce(ctx context.Context, deps Deps) cycleResult {
	deps = withDefaults(deps)
	now := deps.Now()

	pending, err := deps.Leases.PendingSweep(ctx, now, batchSize)
	if err != nil {
		log.Printf("audit: seaweedfs sweep: listing pending leases: %v", err)
		return cycleResult{}
	}

	var res cycleResult
	if len(pending) > 0 {
		// PendingSweep orders oldest ExpiresAt first, so this is the true backlog age
		// at the start of the cycle regardless of how this cycle's sweeps turn out.
		res.oldestBacklogAge = now.Sub(pending[0].ExpiresAt)
	}

	for _, lease := range pending {
		if err := sweepLease(ctx, deps, lease); err != nil {
			res.failed++
			log.Printf("audit: seaweedfs sweep failed lease=%s workspace=%s backend=%s user=%s: %v",
				lease.ID, lease.Workspace, lease.BackendID, lease.IAMUserName, err)
			continue
		}
		if err := deps.Leases.MarkSwept(ctx, lease.ID, now); err != nil {
			res.failed++
			log.Printf("audit: seaweedfs sweep: lease=%s marking swept: %v", lease.ID, err)
			continue
		}
		res.swept++
	}

	if active, err := deps.Leases.ActiveCount(ctx, now); err == nil {
		res.active = active
	} else {
		log.Printf("audit: seaweedfs sweep: counting active leases: %v", err)
	}

	log.Printf("audit: seaweedfs sweep swept=%d failed=%d outstanding=%d oldest_outstanding_age=%s",
		res.swept, res.failed, res.active, res.oldestBacklogAge.Round(time.Second))
	return res
}

// sweepLease deletes one lease's SeaweedFS IAM user and access key. A backend that no
// longer exists, or was reconfigured to a different kind, has nothing left to clean up
// on SeaweedFS's side — treated as success, same as "no such user"/"no such key".
func sweepLease(ctx context.Context, deps Deps, lease seaweedfslease.Lease) error {
	rec, creds, err := deps.Registry.RawCredentials(ctx, lease.Workspace, lease.BackendID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("reading backend: %w", err)
	}
	if rec.Kind != backend.KindS3 {
		return nil
	}

	var cfg s3.Config
	if err := json.Unmarshal(rec.Config, &cfg); err != nil {
		return fmt.Errorf("unparseable backend config: %w", err)
	}
	admin := s3.Credentials{AccessKeyID: creds[s3.CredAccessKeyID], SecretAccessKey: creds[s3.CredSecretAccessKey]}
	return s3.DeleteSeaweedFSIAMLease(ctx, cfg, admin, lease.IAMUserName, lease.AccessKeyID)
}
