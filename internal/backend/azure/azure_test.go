package azure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/backendtest"
)

// Azurite's well-known, publicly documented development account. Not a secret — it's
// the fixed credential every Azurite instance ships with.
const (
	azuriteAccount = "devstoreaccount1"
	azuriteKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

var containerSeq atomic.Int64

func requireAzurite(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("BOOTH_TEST_AZURITE_ENDPOINT")
	if endpoint == "" {
		// CI sets this so a missing emulator fails loudly instead of skipping green.
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_AZURITE_ENDPOINT is not set")
		}
		t.Skip("BOOTH_TEST_AZURITE_ENDPOINT not set; skipping Azure tests (see hack/docker-compose.emulators.yml)")
	}
	return endpoint
}

func newContainer(t *testing.T, endpoint string) string {
	t.Helper()
	name := fmt.Sprintf("booth-conf-%d-%d", os.Getpid(), containerSeq.Add(1))
	cred, err := container.NewSharedKeyCredential(azuriteAccount, azuriteKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := container.NewClientWithSharedKeyCredential(strings.TrimRight(endpoint, "/")+"/"+name, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(context.Background(), nil); err != nil {
		t.Fatalf("creating container: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Delete(context.Background(), nil) })
	return name
}

func newBackend(t *testing.T, endpoint, containerName, prefix string) *Backend {
	t.Helper()
	b, err := New(
		Config{AccountName: azuriteAccount, Container: containerName, Endpoint: endpoint, Prefix: prefix},
		Credentials{AccountKey: azuriteKey},
	)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformance(t *testing.T) {
	endpoint := requireAzurite(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		return newBackend(t, endpoint, newContainer(t, endpoint), "")
	})
}

func TestConformance_WithPrefix(t *testing.T) {
	endpoint := requireAzurite(t)
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		return newBackend(t, endpoint, newContainer(t, endpoint), "tenant/data")
	})
}

func TestCheck_Failures(t *testing.T) {
	endpoint := requireAzurite(t)

	t.Run("missing container", func(t *testing.T) {
		b := newBackend(t, endpoint, "no-such-container-booth", "")
		err := b.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "container does not exist") {
			t.Errorf("Check error = %v, want a container-does-not-exist message", err)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		name := newContainer(t, endpoint)
		b, err := New(
			Config{AccountName: azuriteAccount, Container: name, Endpoint: endpoint},
			// Valid base64, wrong key: signature check must fail server-side.
			Credentials{AccountKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="},
		)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Errorf("Check error = %v, want an access-denied message", err)
		}
	})
}

func TestNew_CredentialRules(t *testing.T) {
	cfg := Config{AccountName: "acct", Container: "cont"}
	cases := []struct {
		name    string
		creds   Credentials
		wantErr string
	}{
		{"none", Credentials{}, "a credential is required"},
		{"both", Credentials{AccountKey: "a", SASToken: "b"}, "not both"},
		{"malformed key", Credentials{AccountKey: "not base64!!"}, "invalid accountKey"},
		{"sas ok", Credentials{SASToken: "?sv=2022&sig=x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(cfg, tc.creds)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok", Config{AccountName: "myaccount", Container: "my-container"}, ""},
		{"ok emulator endpoint with path", Config{AccountName: "devstoreaccount1", Container: "c-1", Endpoint: "http://127.0.0.1:10000/devstoreaccount1"}, ""},
		{"uppercase account", Config{AccountName: "MyAccount", Container: "my-container"}, "accountName"},
		{"short container", Config{AccountName: "myaccount", Container: "ab"}, "container"},
		{"double hyphen container", Config{AccountName: "myaccount", Container: "my--container"}, "container"},
		{"endpoint with query", Config{AccountName: "myaccount", Container: "my-container", Endpoint: "https://x.example?sig=1"}, "query string"},
		{"endpoint no scheme", Config{AccountName: "myaccount", Container: "my-container", Endpoint: "x.example"}, "http(s)"},
		{"traversal prefix", Config{AccountName: "myaccount", Container: "my-container", Prefix: "a/../b"}, "prefix"},
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

// Hierarchical-namespace accounts (ADR 0037) can expose directories through the Blob API as
// zero-length blobs tagged hdi_isfolder=true. Azurite has no HNS mode, so simulate the
// marker directly: it must not appear as a file, while real objects beside it still do.
func TestList_SkipsHierarchicalNamespaceFolderMarkers(t *testing.T) {
	endpoint := requireAzurite(t)
	name := newContainer(t, endpoint)
	b := newBackend(t, endpoint, name, "")

	if _, err := b.Write(context.Background(), "data/real.csv", strings.NewReader("x"), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	marker := b.container.NewBlockBlobClient("data/emptydir")
	yes := "true"
	if _, err := marker.Upload(context.Background(), streaming.NopCloser(strings.NewReader("")), &blockblob.UploadOptions{Metadata: map[string]*string{"hdi_isfolder": &yes}}); err != nil {
		t.Fatal(err)
	}

	for _, recursive := range []bool{true, false} {
		res, err := b.List(context.Background(), backend.ListOptions{Prefix: "data", Recursive: recursive})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Entries {
			if e.Path == "data/emptydir" {
				t.Errorf("recursive=%v: directory marker listed as a file: %+v", recursive, e)
			}
		}
		if len(res.Entries) != 1 || res.Entries[0].Path != "data/real.csv" {
			t.Errorf("recursive=%v: entries = %+v, want only data/real.csv", recursive, res.Entries)
		}
	}
}
