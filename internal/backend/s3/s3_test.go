package s3

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/backendtest"
)

// These tests run against a real MinIO server, not an in-process fake: the S3 client
// switches between signed-chunk and unsigned payload framing depending on transport,
// and the available Go fakes don't implement the former, so they silently store framing
// bytes as object content — passing or failing for reasons unrelated to real S3
// behavior. CI starts a MinIO container; locally see hack/docker-compose.emulators.yml.
//
//	BOOTH_TEST_S3_ENDPOINT=http://127.0.0.1:19000
//	BOOTH_TEST_S3_ACCESS_KEY=...  BOOTH_TEST_S3_SECRET_KEY=...
var bucketSeq atomic.Int64

type minioEnv struct {
	endpoint, access, secret string
}

func requireMinIO(t *testing.T) minioEnv {
	t.Helper()
	env := minioEnv{
		endpoint: os.Getenv("BOOTH_TEST_S3_ENDPOINT"),
		access:   os.Getenv("BOOTH_TEST_S3_ACCESS_KEY"),
		secret:   os.Getenv("BOOTH_TEST_S3_SECRET_KEY"),
	}
	if env.endpoint == "" {
		// CI sets this so a missing emulator fails loudly instead of skipping green.
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_S3_ENDPOINT is not set")
		}
		t.Skip("BOOTH_TEST_S3_ENDPOINT not set; skipping S3 tests (see hack/docker-compose.emulators.yml)")
	}
	return env
}

func (e minioEnv) admin(t *testing.T) *minio.Client {
	t.Helper()
	c, err := minio.New(strings.TrimPrefix(e.endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(e.access, e.secret, ""), BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newBucket creates an isolated bucket and removes it (and its contents) afterwards.
func (e minioEnv) newBucket(t *testing.T) string {
	t.Helper()
	client := e.admin(t)
	bucket := fmt.Sprintf("booth-conf-%d-%d", os.Getpid(), bucketSeq.Add(1))
	if err := client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	t.Cleanup(func() {
		for o := range client.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = client.RemoveObject(context.Background(), bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = client.RemoveBucket(context.Background(), bucket)
	})
	return bucket
}

func (e minioEnv) backend(t *testing.T, bucket, prefix string) *Backend {
	t.Helper()
	b, err := New(Config{Endpoint: e.endpoint, Bucket: bucket, Prefix: prefix, PathStyle: true},
		Credentials{AccessKeyID: e.access, SecretAccessKey: e.secret})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformance(t *testing.T) {
	env := requireMinIO(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		return env.backend(t, env.newBucket(t), "")
	})
}

// The same suite again with a configured key prefix, since every code path strips/adds
// it and that's the easiest place for an off-by-one ("a/b" vs "a/b/") to hide.
func TestConformance_WithPrefix(t *testing.T) {
	env := requireMinIO(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		return env.backend(t, env.newBucket(t), "tenant/data")
	})
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok minimal (AWS default endpoint)", Config{Bucket: "b"}, ""},
		{"ok custom endpoint", Config{Bucket: "b", Endpoint: "http://minio:9000"}, ""},
		{"missing bucket", Config{}, "bucket is required"},
		{"endpoint without scheme", Config{Bucket: "b", Endpoint: "minio:9000"}, "full http(s):// URL"},
		{"endpoint with path", Config{Bucket: "b", Endpoint: "http://minio:9000/base"}, "must not include a path"},
		{"traversal in prefix", Config{Bucket: "b", Prefix: "../x"}, "prefix"},
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

// Two backends sharing one bucket under different prefixes must be fully isolated from
// each other: neither lists nor reaches the other's keys, and paths are prefix-relative.
func TestPrefixConfinesBackend(t *testing.T) {
	env := requireMinIO(t)
	bucket := env.newBucket(t)
	ctx := context.Background()

	teamA, teamB, whole := env.backend(t, bucket, "team-a"), env.backend(t, bucket, "team-b/"), env.backend(t, bucket, "")
	if _, err := teamA.Write(ctx, "report.csv", strings.NewReader("a"), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := teamB.Write(ctx, "report.csv", strings.NewReader("b"), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	res, err := teamA.List(ctx, backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "report.csv" {
		t.Errorf("team-a sees %+v, want only its own report.csv (prefix stripped)", res.Entries)
	}

	all, err := whole.List(ctx, backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Entries) != 2 {
		t.Errorf("unprefixed backend sees %+v, want both teams' objects under their prefixes", all.Entries)
	}

	rc, _, err := teamB.Read(ctx, "report.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "b" {
		t.Errorf("team-b read %q, want its own object", got)
	}
}

func TestCheck_Failures(t *testing.T) {
	env := requireMinIO(t)

	t.Run("missing bucket", func(t *testing.T) {
		b := env.backend(t, "no-such-bucket-booth", "")
		err := b.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "bucket does not exist") {
			t.Errorf("Check error = %v, want a bucket-does-not-exist message", err)
		}
	})

	t.Run("wrong credentials", func(t *testing.T) {
		bucket := env.newBucket(t)
		b, err := New(Config{Endpoint: env.endpoint, Bucket: bucket, PathStyle: true},
			Credentials{AccessKeyID: env.access, SecretAccessKey: "definitely-wrong"})
		if err != nil {
			t.Fatal(err)
		}
		err = b.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Errorf("Check error = %v, want an access-denied message", err)
		}
	})
}
