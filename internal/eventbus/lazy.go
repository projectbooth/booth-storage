package eventbus

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// urlFile and credsFile are the two file names booth-core's event-bus credentials
// Secret is mounted under (ADR 0050) — see charts/booth-storage/values.yaml's eventBus
// block and templates/deployment.yaml's event-bus-credentials volume.
const (
	urlFile   = "url"
	credsFile = "nats.creds"
)

// LazyPublisher wraps a Publisher that may not be connectable yet. booth-core writes
// the event-bus credentials Secret asynchronously, and this module never gates its own
// startup or writes on it existing (unlike booth-api's own ADR 0123 wiring of the same
// Secret). A LazyPublisher starts as a no-op and polls dir for the two files above,
// swapping itself for a real NATSPublisher the first time both are present and a
// connection actually succeeds. Safe for concurrent Publish calls.
type LazyPublisher struct {
	dir string

	mu      sync.RWMutex
	current Publisher
}

// NewLazyPublisher returns a LazyPublisher that starts as a NoopPublisher — so Publish
// is always safe to call immediately — and does not itself start polling; call Run to
// do that (typically in its own goroutine, for the life of the process).
func NewLazyPublisher(dir string, warn func(eventType string)) *LazyPublisher {
	return &LazyPublisher{dir: dir, current: NewNoopPublisher(warn)}
}

// Publish delegates to whichever Publisher is current: the no-op until Run connects,
// the real one after.
func (lp *LazyPublisher) Publish(ctx context.Context, workspace, eventType string, data any) error {
	lp.mu.RLock()
	cur := lp.current
	lp.mu.RUnlock()
	return cur.Publish(ctx, workspace, eventType, data)
}

// Run polls dir every interval until ctx is done or a connection succeeds, whichever
// comes first. Once connected, nats.go's own reconnect logic covers the connection from
// then on; if the Secret is later deleted and recreated with different credentials,
// that's a rare enough operator action to answer the same way this module answers every
// other Secret changing shape under it — a pod restart — rather than polling forever.
func (lp *LazyPublisher) Run(ctx context.Context, interval time.Duration) {
	if lp.tryConnect() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if lp.tryConnect() {
				return
			}
		}
	}
}

func (lp *LazyPublisher) tryConnect() bool {
	raw, err := os.ReadFile(filepath.Join(lp.dir, urlFile))
	if err != nil {
		return false
	}
	url := strings.TrimSpace(string(raw))
	if url == "" {
		return false
	}
	creds := filepath.Join(lp.dir, credsFile)
	if _, err := os.Stat(creds); err != nil {
		return false
	}
	pub, err := ConnectWithCreds(url, creds)
	if err != nil {
		log.Printf("eventbus: found event-bus credentials but could not connect to %s: %v", url, err)
		return false
	}
	lp.mu.Lock()
	lp.current = pub
	lp.mu.Unlock()
	log.Printf("eventbus: connected to the event bus at %s", url)
	return true
}

// Close closes the underlying connection, if Run ever established one.
func (lp *LazyPublisher) Close() {
	lp.mu.RLock()
	cur := lp.current
	lp.mu.RUnlock()
	if pub, ok := cur.(*NATSPublisher); ok {
		pub.Close()
	}
}
