package uploadgrant

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
	grants map[string]Grant // keyed by ID
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{grants: make(map[string]Grant)} }

func (m *MemoryStore) Ping(context.Context) error { return nil }

func scopeKey(workspace, principal, backendID, prefix string) string {
	return workspace + "\x00" + principal + "\x00" + backendID + "\x00" + prefix
}

func (m *MemoryStore) Upsert(_ context.Context, g Grant) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	target := scopeKey(g.Workspace, g.Principal, g.BackendID, g.Prefix)
	for id, existing := range m.grants {
		if scopeKey(existing.Workspace, existing.Principal, existing.BackendID, existing.Prefix) == target {
			delete(m.grants, id) // replaced outright, including its ID
			break
		}
	}
	m.grants[g.ID] = clone(g)
	return clone(g), nil
}

func (m *MemoryStore) Get(_ context.Context, workspace, id string) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[id]
	if !ok || g.Workspace != workspace {
		return Grant{}, ErrNotFound
	}
	return clone(g), nil
}

func (m *MemoryStore) List(_ context.Context, workspace, principal string) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Grant{}
	for _, g := range m.grants {
		if g.Workspace != workspace {
			continue
		}
		if principal != "" && g.Principal != principal {
			continue
		}
		out = append(out, clone(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) Delete(_ context.Context, workspace, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[id]
	if !ok || g.Workspace != workspace {
		return ErrNotFound
	}
	delete(m.grants, id)
	return nil
}

func (m *MemoryStore) DeleteByPrincipal(_ context.Context, workspace, principal string) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Grant{}
	for id, g := range m.grants {
		if g.Workspace == workspace && g.Principal == principal {
			out = append(out, clone(g))
			delete(m.grants, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func clone(g Grant) Grant {
	g.Extensions = append([]string(nil), g.Extensions...)
	g.CreatedAt = g.CreatedAt.Truncate(time.Microsecond)
	return g
}
