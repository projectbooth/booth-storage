package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// probeKeyPrefix marks the probe object as internal bookkeeping, not a real upload — the
// same convention the filesystem kind uses for its own in-flight temp files.
const probeKeyPrefix = ".booth-create-only-probe-"

// probeCreateOnly proves whether b's server actually enforces a create-only write
// (ADR 0117 point 8a): write a short-lived key with WriteOptions.CreateOnly, then write it
// again the same way. A server that honours the precondition accepts the first write and
// answers ErrPreconditionFailed on the second; a server that silently overwrites fails
// this probe, and nothing downstream will trust it (Record.CreateOnlyVerified stays false).
//
// Only called for the S3 kind (Service.TestSaved): the other three kinds' atomicity comes
// from the OS or from a documented, always-enforced service precondition, not from
// whichever S3-compatible server an operator happened to register.
func probeCreateOnly(ctx context.Context, b backend.Backend) error {
	key, err := probeKey()
	if err != nil {
		return fmt.Errorf("generating probe key: %w", err)
	}
	defer func() { _ = b.DeleteObject(ctx, key) }() // best-effort cleanup either way

	if _, err := b.Write(ctx, key, strings.NewReader(""), backend.WriteOptions{CreateOnly: true}); err != nil {
		return fmt.Errorf("first create-only write (should succeed on an empty key): %w", err)
	}
	_, err = b.Write(ctx, key, strings.NewReader(""), backend.WriteOptions{CreateOnly: true})
	switch {
	case err == nil:
		return errors.New("the server accepted a second create-only write to the same key: it does not enforce If-None-Match")
	case errors.Is(err, backend.ErrPreconditionFailed):
		return nil
	default:
		return fmt.Errorf("second create-only write failed with an unexpected error: %w", err)
	}
}

func probeKey() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return probeKeyPrefix + hex.EncodeToString(b[:]), nil
}
