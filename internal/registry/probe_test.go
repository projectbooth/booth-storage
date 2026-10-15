package registry

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// fakeCreateOnlyBackend simulates a registered S3-compatible server's write behaviour
// under WriteOptions.CreateOnly, without any real network: honors (or doesn't) the
// precondition exactly the way a real server would, so probeCreateOnly's own logic can be
// tested without depending on a live MinIO (that wire-level behaviour is covered by
// internal/backend/backendtest's conformance suite against the real, pinned test mirror).
type fakeCreateOnlyBackend struct {
	backend.Backend // unused methods panic if ever called
	honoursPrecondition,
	failUnexpectedly bool
	keys map[string]bool
}

func newFakeCreateOnlyBackend(honours bool) *fakeCreateOnlyBackend {
	return &fakeCreateOnlyBackend{honoursPrecondition: honours, keys: map[string]bool{}}
}

func (f *fakeCreateOnlyBackend) Write(_ context.Context, path string, _ io.Reader, opts backend.WriteOptions) (backend.ObjectInfo, error) {
	if f.failUnexpectedly {
		return backend.ObjectInfo{}, errors.New("simulated server failure")
	}
	if opts.CreateOnly && f.keys[path] && f.honoursPrecondition {
		return backend.ObjectInfo{}, backend.ErrPreconditionFailed
	}
	f.keys[path] = true
	return backend.ObjectInfo{Path: path}, nil
}

func (f *fakeCreateOnlyBackend) DeleteObject(_ context.Context, path string) error {
	delete(f.keys, path)
	return nil
}

func TestProbeCreateOnly_ServerHonoursPrecondition(t *testing.T) {
	if err := probeCreateOnly(testCtx, newFakeCreateOnlyBackend(true)); err != nil {
		t.Errorf("probe = %v, want nil for a server that honours If-None-Match", err)
	}
}

func TestProbeCreateOnly_ServerSilentlyOverwrites(t *testing.T) {
	err := probeCreateOnly(testCtx, newFakeCreateOnlyBackend(false))
	if err == nil {
		t.Fatal("probe = nil, want an error for a server that accepts a second create-only write")
	}
	if !strings.Contains(err.Error(), "does not enforce") {
		t.Errorf("error = %v, want it to say the server does not enforce the precondition", err)
	}
}

func TestProbeCreateOnly_UnexpectedFailure(t *testing.T) {
	b := newFakeCreateOnlyBackend(true)
	b.failUnexpectedly = true
	if err := probeCreateOnly(testCtx, b); err == nil {
		t.Fatal("probe = nil, want an error when the first write itself fails")
	}
}

func TestProbeCreateOnly_CleansUpProbeKey(t *testing.T) {
	b := newFakeCreateOnlyBackend(true)
	_ = probeCreateOnly(testCtx, b)
	if len(b.keys) != 0 {
		t.Errorf("probe left %v behind, want the probe key removed", b.keys)
	}
}

func TestSupportsCreateOnly(t *testing.T) {
	cases := []struct {
		kind     backend.Kind
		verified bool
		want     bool
	}{
		{backend.KindFilesystem, false, true},
		{backend.KindAzure, false, true},
		{backend.KindGCS, false, true},
		{backend.KindS3, false, false},
		{backend.KindS3, true, true},
	}
	for _, tc := range cases {
		rec := Record{Kind: tc.kind, CreateOnlyVerified: tc.verified}
		if got := SupportsCreateOnly(rec); got != tc.want {
			t.Errorf("SupportsCreateOnly(kind=%s verified=%v) = %v, want %v", tc.kind, tc.verified, got, tc.want)
		}
	}
}
