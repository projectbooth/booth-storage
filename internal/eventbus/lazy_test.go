package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// placeholderCredsFile builds a syntactically well-formed NATS .creds file (the format
// booth-core's nats.creds key is mounted as), around a real, checksum-valid nkey seed
// (the client parses and checksums the seed itself before ever dialing). The JWT half
// is a placeholder, not a real credential signed by any account: the test NATS server
// runs with no auth/account resolver configured, so nothing ever validates it against
// an account — only that the file parses. What's being proven here is that
// LazyPublisher reads the files and dials once they're both present, exactly as it
// would with a real credential against the real platform bus.
func placeholderCredsFile(t *testing.T) string {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("generating a throwaway nkey: %v", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatalf("extracting the seed: %v", err)
	}
	return fmt.Sprintf(`-----BEGIN NATS USER JWT-----
eyJhbGciOiJlZDI1NTE5LW5rZXkiLCJ0eXAiOiJKV1QifQ.eyJqdGkiOiJwbGFjZWhvbGRlciJ9.cGxhY2Vob2xkZXI
------END NATS USER JWT------

************************* IMPORTANT *************************
NKEY Seed printed below can be used to sign and prove identity.
NKEYS are sensitive and should be treated as secrets.

-----BEGIN USER NKEY SEED-----
%s
------END USER NKEY SEED------

*************************************************************
`, seed)
}

func TestLazyPublisher_NoFiles_StaysNoop(t *testing.T) {
	dir := t.TempDir()
	lp := NewLazyPublisher(dir, nil)

	if lp.tryConnect() {
		t.Fatal("tryConnect succeeded with an empty directory")
	}
	if err := lp.Publish(context.Background(), "ws", "storage.object.created", nil); err != nil {
		t.Fatalf("Publish (still a no-op) returned an error: %v", err)
	}
}

func TestLazyPublisher_OnlyOneFilePresent_StaysNoop(t *testing.T) {
	dir := t.TempDir()
	lp := NewLazyPublisher(dir, nil)

	if err := os.WriteFile(filepath.Join(dir, urlFile), []byte("nats://127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lp.tryConnect() {
		t.Fatal("tryConnect succeeded with only the url file present, no creds file")
	}

	dir2 := t.TempDir()
	lp2 := NewLazyPublisher(dir2, nil)
	if err := os.WriteFile(filepath.Join(dir2, credsFile), []byte(placeholderCredsFile(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if lp2.tryConnect() {
		t.Fatal("tryConnect succeeded with only the creds file present, no url file")
	}
}

func TestLazyPublisher_ConnectsOnceBothFilesAppear_NoRestartNeeded(t *testing.T) {
	url := requireNATS(t)

	setup, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting to set up the stream: %v", err)
	}
	defer setup.Close()
	js, err := setup.JetStream()
	if err != nil {
		t.Fatalf("opening JetStream context: %v", err)
	}
	ensureStream(t, js)

	subject := Subject("acme-analytics", "storage.object.created")
	// DeliverNew: see eventbus_test.go's identical comment — this subject persists
	// across test runs within the same server.
	sub, err := js.SubscribeSync(subject, nats.DeliverNew())
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	dir := t.TempDir()
	lp := NewLazyPublisher(dir, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lp.Run(ctx, 20*time.Millisecond)
	defer lp.Close()

	// Before the credentials exist, publishing is a silent no-op — the whole point of
	// this module never gating writes on the Secret being there yet.
	if err := lp.Publish(context.Background(), "acme-analytics", "storage.object.created", map[string]any{"early": true}); err != nil {
		t.Fatalf("Publish before credentials exist returned an error: %v", err)
	}

	// Simulate booth-core writing the Secret sometime after this process started: the
	// kubelet projects a Secret's keys into an already-mounted volume as separate
	// files, which is what this reproduces.
	if err := os.WriteFile(filepath.Join(dir, urlFile), []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credsFile), []byte(placeholderCredsFile(t)), 0o600); err != nil {
		t.Fatal(err)
	}

	// NoopPublisher.Publish never errors, so success/failure of Publish can't be used to
	// detect the swap; check the concrete type directly instead (same package).
	deadline := time.Now().Add(5 * time.Second)
	for {
		lp.mu.RLock()
		_, connected := lp.current.(*NATSPublisher)
		lp.mu.RUnlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LazyPublisher never connected after both files appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := lp.Publish(context.Background(), "acme-analytics", "storage.object.created", map[string]any{"late": true}); err != nil {
		t.Fatalf("Publish after connecting returned an error: %v", err)
	}

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("reading back the published message: %v", err)
	}
	var got envelope
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshaling envelope: %v", err)
	}
	data, _ := got.Data.(map[string]any)
	if data["late"] != true {
		t.Errorf("Data = %+v, want the publish made after both files appeared", got.Data)
	}
}
