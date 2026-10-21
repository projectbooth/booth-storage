package eventbus

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestSubject(t *testing.T) {
	got := Subject("acme-analytics", "storage.object.created")
	want := "booth.acme-analytics.storage.object.created"
	if got != want {
		t.Fatalf("Subject() = %q, want %q", got, want)
	}
}

func TestNoopPublisher_WarnsOnce(t *testing.T) {
	var mu sync.Mutex
	var calls int
	p := NewNoopPublisher(func(eventType string) {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Publish(context.Background(), "ws", "storage.object.created", nil); err != nil {
				t.Errorf("NoopPublisher.Publish returned an error: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("warn called %d times, want exactly 1", calls)
	}
}

func TestNoopPublisher_NilWarnNeverPanics(t *testing.T) {
	p := NewNoopPublisher(nil)
	if err := p.Publish(context.Background(), "ws", "storage.object.created", nil); err != nil {
		t.Fatalf("Publish returned an error: %v", err)
	}
}

// These tests run against a real NATS/JetStream server, not an in-process fake: the
// point is proving the envelope actually lands on the subject a real consumer would
// read, in a real stream, under a real server's own ack semantics. CI starts a NATS
// container; locally see hack/docker-compose.emulators.yml.
//
//	BOOTH_TEST_NATS_URL=nats://127.0.0.1:14222
func requireNATS(t *testing.T) string {
	t.Helper()
	url := os.Getenv("BOOTH_TEST_NATS_URL")
	if url == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_NATS_URL is not set")
		}
		t.Skip("BOOTH_TEST_NATS_URL not set; skipping NATS tests (see hack/docker-compose.emulators.yml)")
	}
	return url
}

// ensureStream provisions ADR 0026's BOOTH_EVENTS stream if it isn't there yet. A real
// cluster has booth-core do this once; there is no core in this test emulator to do it,
// so the test takes over that one-time setup itself.
func ensureStream(t *testing.T, js nats.JetStreamContext) {
	t.Helper()
	_, err := js.StreamInfo("BOOTH_EVENTS")
	if err == nil {
		return
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     "BOOTH_EVENTS",
		Subjects: []string{"booth.>"},
	}); err != nil {
		t.Fatalf("provisioning BOOTH_EVENTS stream: %v", err)
	}
}

func TestNATSPublisher_PublishLandsInTheStream(t *testing.T) {
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

	pub, err := Connect(url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pub.Close()

	subject := Subject("acme-analytics", "storage.object.created")
	sub, err := js.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	data := map[string]any{
		"backendId": "lake",
		"path":      "finance/dev/inbox/statement.csv",
		"size":      float64(1234),
		"principal": "streamlit:acme-analytics:finance-dev",
		"via":       "app-upload",
	}
	ctx, cancel := context.WithTimeout(context.Background(), PublishTimeout)
	defer cancel()
	if err := pub.Publish(ctx, "acme-analytics", "storage.object.created", data); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("reading back the published message: %v", err)
	}
	var got envelope
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshaling envelope: %v", err)
	}
	if got.Workspace != "acme-analytics" {
		t.Errorf("Workspace = %q, want %q", got.Workspace, "acme-analytics")
	}
	if got.EventType != "storage.object.created" {
		t.Errorf("EventType = %q, want %q", got.EventType, "storage.object.created")
	}
	if got.PublishedBy != ModuleID {
		t.Errorf("PublishedBy = %q, want %q", got.PublishedBy, ModuleID)
	}
	if got.PublishedAt.IsZero() {
		t.Error("PublishedAt is zero")
	}
	gotData, ok := got.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data is %T, want map[string]any", got.Data)
	}
	for k, want := range data {
		if gotData[k] != want {
			t.Errorf("Data[%q] = %v, want %v", k, gotData[k], want)
		}
	}
}

func TestNATSPublisher_PublishSurfacesAFailure(t *testing.T) {
	url := requireNATS(t)

	pub, err := Connect(url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pub.Close()

	// An already-expired context proves js.Publish's failure actually propagates out of
	// Publish rather than being swallowed — the whole reason this publisher uses
	// js.Publish (which waits for the stream's ack) instead of a plain fire-and-forget
	// nc.Publish that would report success before anything is known about delivery.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := pub.Publish(ctx, "acme-analytics", "storage.object.created", nil); err == nil {
		t.Fatal("Publish succeeded with an already-expired context; want an error")
	}
}
