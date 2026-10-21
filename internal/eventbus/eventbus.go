// Package eventbus publishes booth-storage's async notification events (ADR 0007) onto
// the shared NATS JetStream bus, in the fixed envelope ADR 0026 defines. Today this
// module publishes exactly one event type, storage.object.created (ADR 0123) — see
// internal/api's handleWriteObject.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// PublishTimeout bounds how long a single Publish call may block the HTTP request that
// triggered it. A write has already succeeded by the time Publish is called — nothing
// about the write's own result should wait long for the bus (docs/design-event-triggers.md §3).
const PublishTimeout = 2 * time.Second

// ModuleID is this module's manifest id, the envelope's "publishedBy" (ADR 0026).
const ModuleID = "storage"

// Publisher publishes one event of eventType onto workspace's subject, in the ADR 0026
// envelope. An implementation must never treat its own failure as fatal to a caller —
// see each implementation's own doc comment for what it does instead.
type Publisher interface {
	Publish(ctx context.Context, workspace, eventType string, data any) error
}

// envelope is ADR 0026's fixed payload shape, regardless of event type.
type envelope struct {
	Workspace   string    `json:"workspace"`
	EventType   string    `json:"eventType"`
	PublishedAt time.Time `json:"publishedAt"`
	PublishedBy string    `json:"publishedBy"`
	Data        any       `json:"data"`
}

// Subject returns the ADR 0026 subject string for an event in workspace.
func Subject(workspace, eventType string) string {
	return fmt.Sprintf("booth.%s.%s", workspace, eventType)
}

// NATSPublisher publishes onto a real JetStream-backed NATS connection. It never
// provisions or configures the stream itself (ADR 0026: BOOTH_EVENTS, subject filter
// booth.>, owned by booth-core) — only ever publishes onto a subject under that filter.
type NATSPublisher struct {
	js nats.JetStreamContext
	nc *nats.Conn
}

// Connect dials url and returns a ready NATSPublisher. Reconnection after the initial
// dial is nats.go's own built-in behavior (unbounded retries); Connect itself only fails
// fast if the first dial can't succeed at all.
func Connect(url string) (*NATSPublisher, error) {
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %q: %w", url, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("opening JetStream context: %w", err)
	}
	return &NATSPublisher{js: js, nc: nc}, nil
}

// Close closes the underlying connection.
func (p *NATSPublisher) Close() {
	if p.nc != nil {
		p.nc.Close()
	}
}

// Publish marshals data into the ADR 0026 envelope and publishes it to
// booth.<workspace>.<eventType>, waiting (bounded by ctx) for the stream's own ack that
// the message was actually stored — a plain fire-and-forget publish would report success
// even when the stream itself is unreachable, which is the one failure this module most
// wants to be able to tell apart from "published."
func (p *NATSPublisher) Publish(ctx context.Context, workspace, eventType string, data any) error {
	env := envelope{
		Workspace:   workspace,
		EventType:   eventType,
		PublishedAt: time.Now().UTC(),
		PublishedBy: ModuleID,
		Data:        data,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling %s envelope: %w", eventType, err)
	}
	if _, err := p.js.Publish(Subject(workspace, eventType), body, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publishing %s: %w", eventType, err)
	}
	return nil
}

// NoopPublisher drops every event, successfully. Used when no bus is configured
// (BOOTH_NATS_URL unset — local development and BOOTH_STORAGE_DEV_MEMORY, matching this
// module's existing "optional infra: log once, then degrade quietly" convention, e.g.
// config.Load's filesystem-roots warning). Publish never returns an error from here:
// there is nothing a caller could usefully do differently on this path, and the first
// call logs once via the warn callback so the gap is visible at startup, not silent.
type NoopPublisher struct {
	warn   func(eventType string)
	warned atomic.Bool
}

// NewNoopPublisher returns a Publisher that drops everything, calling warn exactly once
// (on its first Publish call) so the operator sees why events never arrive. warn may be
// nil to drop silently (used by tests that don't care).
func NewNoopPublisher(warn func(eventType string)) *NoopPublisher {
	return &NoopPublisher{warn: warn}
}

func (p *NoopPublisher) Publish(ctx context.Context, workspace, eventType string, data any) error {
	if p.warn != nil && p.warned.CompareAndSwap(false, true) {
		p.warn(eventType)
	}
	return nil
}
