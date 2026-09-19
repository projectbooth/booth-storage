package gcs

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/backendtest"
)

var bucketSeq atomic.Int64

// fake-gcs-server runs in-process, so the GCS path is exercised on every push with no
// container. Unlike the S3 fakes, it implements the real JSON API including resumable
// uploads, which is what the large-object case goes through.
func newFake(t *testing.T) *fakestorage.Server {
	t.Helper()
	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatalf("starting fake GCS server: %v", err)
	}
	t.Cleanup(server.Stop)
	return server
}

func newBackend(t *testing.T, server *fakestorage.Server, prefix string) *Backend {
	t.Helper()
	bucket := fmt.Sprintf("booth-conf-%d", bucketSeq.Add(1))
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	b, err := NewFromClient(server.Client(), Config{Bucket: bucket, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformance(t *testing.T) {
	server := newFake(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend { return newBackend(t, server, "") })
}

func TestConformance_WithPrefix(t *testing.T) {
	server := newFake(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend { return newBackend(t, server, "tenant/data") })
}

func TestCheck_MissingBucket(t *testing.T) {
	server := newFake(t)
	b, err := NewFromClient(server.Client(), Config{Bucket: "no-such-bucket-booth"})
	if err != nil {
		t.Fatal(err)
	}
	err = b.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bucket does not exist") {
		t.Errorf("Check error = %v, want a bucket-does-not-exist message", err)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok", Config{Bucket: "my-bucket.example"}, ""},
		{"empty", Config{}, "bucket"},
		{"uppercase", Config{Bucket: "My-Bucket"}, "bucket"},
		{"too short", Config{Bucket: "ab"}, "bucket"},
		{"traversal prefix", Config{Bucket: "my-bucket", Prefix: "../x"}, "prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// Only plain service-account keys are accepted: external-account credential files can
// make the Google client fetch arbitrary URLs or run a local command, which a stored,
// admin-supplied credential must never be able to trigger on this server.
func TestValidateServiceAccountJSON(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string
	}{
		{"ok", `{"type":"service_account","client_email":"a@b.iam","private_key":"-----BEGIN"}`, ""},
		{"not json", `nope`, "not valid JSON"},
		{"external account (executable source)", `{"type":"external_account","credential_source":{"executable":{"command":"/bin/sh -c evil"}}}`, "must be a service_account"},
		{"authorized user", `{"type":"authorized_user"}`, "must be a service_account"},
		{"missing key", `{"type":"service_account","client_email":"a@b.iam"}`, "missing client_email or private_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServiceAccountJSON(tc.json)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	if _, err := New(Config{Bucket: "my-bucket"}, Credentials{ServiceAccountJSON: `{"type":"external_account"}`}); err == nil {
		t.Error("New accepted an external_account credential")
	}
}
