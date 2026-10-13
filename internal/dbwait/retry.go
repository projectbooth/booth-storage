// Package dbwait rides out the first-connection refusal to PostgreSQL at process startup
// (booth-architecture/ARCHITECTURE.md item 66(c)): on the homelab, a freshly-scheduled pod's
// first connections can be refused for a few seconds while the NetworkPolicy allow set catches
// up. Retry retries only genuine connection-level failures (refused, timed out, a DNS failure,
// or Postgres's own "the database system is starting up") with bounded exponential backoff.
// Anything else -- wrong credentials, a missing database, a broken query -- is a real failure
// and is never retried. This mirrors the shape of booth-core/internal/dbwait/retry.go (not
// imported: separate modules), with the added error classification booth-storage needs because
// its own constructor also runs migrations, whose SQL errors must fail immediately.
package dbwait

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultMaxElapsed bounds the total time Retry spends riding out the startup window before
// giving up: comfortably longer than the few-second refusal window item 66 observed, short
// enough that a genuinely unreachable server still fails loudly within about a minute.
const DefaultMaxElapsed = 60 * time.Second

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// Retry calls fn, which should attempt exactly one connection or connection-dependent
// operation, retrying with exponential backoff (starting at 500ms, doubling up to 8s) as long
// as fn's error looks like a transient connection-level failure (see isRetryableConnectionError)
// and ctx isn't done and maxElapsed hasn't passed since the first attempt. A non-retryable
// error -- wrong credentials, a missing database, a SQL error -- is returned immediately,
// unretried. Giving up after maxElapsed wraps the last error with the attempt count. Each
// retry is logged with what, the attempt number, the error, and the delay before the next try.
func Retry(ctx context.Context, what string, maxElapsed time.Duration, fn func() error) error {
	deadline := time.Now().Add(maxElapsed)
	backoff := initialBackoff

	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isRetryableConnectionError(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: still failing after %s (%d attempts): %w", what, maxElapsed, attempt, err)
		}

		log.Printf("%s: connection-level failure (attempt %d: %v); retrying in %s", what, attempt, err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// isRetryableConnectionError reports whether err looks like a transient failure to establish
// or open a PostgreSQL connection -- refused, timed out, a DNS failure, or the server itself
// answering "the database system is starting up" (SQLSTATE 57P03, class "operator
// intervention") -- as opposed to something a retry can never fix: wrong credentials, a
// missing database, or a broken query/migration. A *pgconn.PgError means the server actually
// answered, so every code except 57P03 is treated as final.
func isRetryableConnectionError(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" // cannot_connect_now: "the database system is starting up"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true // covers *net.OpError (refused) and dial/i-o timeouts
	}

	// Fallback for a refusal/timeout that didn't come through as a typed net.Error (observed
	// to happen depending on how pgconn's dialer wraps the underlying OS error).
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "i/o timeout")
}
