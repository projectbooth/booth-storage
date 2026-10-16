package seaweedfslease

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-process Store: unit tests and the explicit local-dev mode
// (BOOTH_STORAGE_DEV_MEMORY) — state vanishes on restart, so it must never be what a
// real deployment runs on.
type MemoryStore struct {
	mu     sync.Mutex
	leases map[string]Lease // keyed by ID
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{leases: make(map[string]Lease)} }

func (m *MemoryStore) Ping(context.Context) error { return nil }

func (m *MemoryStore) Record(_ context.Context, l Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leases[l.ID] = clone(l)
	return nil
}

func (m *MemoryStore) PendingSweep(_ context.Context, now time.Time, limit int) ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Lease{}
	for _, l := range m.leases {
		if l.SweptAt == nil && l.ExpiresAt.Before(now) {
			out = append(out, clone(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) MarkSwept(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return nil
	}
	l.SweptAt = &at
	m.leases[id] = l
	return nil
}

func (m *MemoryStore) ActiveCount(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.leases {
		if l.SweptAt == nil && !l.ExpiresAt.Before(now) {
			n++
		}
	}
	return n, nil
}

func clone(l Lease) Lease {
	if l.SweptAt != nil {
		t := *l.SweptAt
		l.SweptAt = &t
	}
	return l
}
