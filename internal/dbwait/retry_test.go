package dbwait

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// captureLog redirects the standard logger for the duration of fn and returns what it wrote.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

var errRefused = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

// TestRetry_FirstAttemptsFailThenSucceed is the real-world shape item 66 found: the first few
// attempts hit a transient connection-level failure, then a later one succeeds. Retry must ride
// out the failures and succeed once they stop, without ever treating an early one as fatal.
func TestRetry_FirstAttemptsFailThenSucceed(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), "connecting to test db", 5*time.Second, func() error {
		attempts++
		if attempts < 3 {
			return errRefused
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry() = %v, want nil once the connection succeeds", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want exactly 3 (2 failures then a success)", attempts)
	}
}

// TestRetry_GivesUpAfterMaxElapsed confirms a connection that never comes up still fails --
// loudly, with the underlying error wrapped and the attempt count named -- rather than
// retrying forever.
func TestRetry_GivesUpAfterMaxElapsed(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), "connecting to nowhere", 1200*time.Millisecond, func() error {
		attempts++
		return errRefused
	})
	if err == nil {
		t.Fatal("Retry() = nil, want an error once maxElapsed has passed")
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("Retry() = %v, want it to wrap the last error", err)
	}
	if !strings.Contains(err.Error(), "connecting to nowhere") {
		t.Errorf("Retry() = %v, want it to name what it was trying to do", err)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("Retry() = %v, want it to report the attempt count", err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want more than 1 before giving up", attempts)
	}
}

// TestRetry_NonRetryableFailsAtOnce confirms an error that isn't a connection-level failure
// (here, a missing database -- SQLSTATE 3D000) is returned immediately, with no backoff and no
// second attempt: a retry could never fix it.
func TestRetry_NonRetryableFailsAtOnce(t *testing.T) {
	wantErr := &pgconn.PgError{Code: "3D000", Message: `database "booth_storage" does not exist`}
	attempts := 0
	start := time.Now()
	err := Retry(context.Background(), "connecting to test db", time.Minute, func() error {
		attempts++
		return wantErr
	})
	elapsed := time.Since(start)

	if !errors.Is(err, wantErr) {
		t.Errorf("Retry() = %v, want the original error returned unwrapped-enough to match", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (a non-retryable error must not be retried)", attempts)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %s, want Retry to return immediately for a non-retryable error", elapsed)
	}
}

// TestRetry_StopsOnContextCancellation confirms Retry doesn't keep sleeping past a canceled
// context, even if maxElapsed hasn't been reached yet.
func TestRetry_StopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	start := time.Now()
	err := Retry(ctx, "x", time.Minute, func() error {
		attempts++
		return errRefused
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Retry() = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %s, want Retry to return promptly once ctx is canceled", elapsed)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (fn runs once before the first ctx check)", attempts)
	}
}

// TestRetry_LogsEachRetry confirms an operator watching logs at boot sees the wait: what was
// being attempted, the attempt number, the error, and the delay.
func TestRetry_LogsEachRetry(t *testing.T) {
	logged := captureLog(t, func() {
		_ = Retry(context.Background(), "connecting to test db", 600*time.Millisecond, func() error {
			return errRefused
		})
	})
	for _, want := range []string{"connecting to test db", "attempt", "retrying in"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output = %q, want it to contain %q", logged, want)
		}
	}
}

func TestIsRetryableConnectionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", errRefused, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, true},
		{"dns failure", &net.DNSError{Err: "no such host", Name: "postgres.booth-storage.svc", IsNotFound: true}, true},
		{"database starting up", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}, true},
		{"wrong password", &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, false},
		{"missing database", &pgconn.PgError{Code: "3D000", Message: "database does not exist"}, false},
		{"sql syntax error", &pgconn.PgError{Code: "42601", Message: "syntax error"}, false},
		{"unrelated error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableConnectionError(tc.err); got != tc.want {
				t.Errorf("isRetryableConnectionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
