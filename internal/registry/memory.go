package registry

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-process MetadataStore. It exists for unit tests and the explicit
// local-dev mode (BOOTH_STORAGE_DEV_MEMORY) — state vanishes on restart, so it must
// never be what a real deployment runs on.
type MemoryStore struct {
	mu   sync.Mutex
	recs map[string]Record
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{recs: make(map[string]Record)} }

func (m *MemoryStore) Create(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(r.Workspace, r.ID)
	if _, ok := m.recs[k]; ok {
		return ErrExists
	}
	m.recs[k] = clone(r)
	return nil
}

func (m *MemoryStore) Get(_ context.Context, workspace, id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[key(workspace, id)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return clone(r), nil
}

func (m *MemoryStore) List(_ context.Context, workspace string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Record{}
	for _, r := range m.recs {
		if r.Workspace == workspace {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) Update(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(r.Workspace, r.ID)
	cur, ok := m.recs[k]
	if !ok {
		return ErrNotFound
	}
	// Only the mutable fields change, mirroring what the SQL implementation does.
	cur.DisplayName, cur.Config, cur.HasCredentials, cur.UpdatedAt = r.DisplayName, r.Config, r.HasCredentials, r.UpdatedAt
	m.recs[k] = clone(cur)
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, workspace, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(workspace, id)
	if _, ok := m.recs[k]; !ok {
		return ErrNotFound
	}
	delete(m.recs, k)
	return nil
}

func (m *MemoryStore) Ping(context.Context) error { return nil }

func clone(r Record) Record {
	r.Config = append([]byte(nil), r.Config...)
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.Truncate(time.Microsecond), r.UpdatedAt.Truncate(time.Microsecond)
	return r
}

// MemoryCredentials is an in-process CredentialStore, for the same purposes and with the
// same warning as MemoryStore: credentials held only in this process's memory.
type MemoryCredentials struct {
	mu    sync.Mutex
	creds map[string]map[string]string
}

func NewMemoryCredentials() *MemoryCredentials {
	return &MemoryCredentials{creds: make(map[string]map[string]string)}
}

func (m *MemoryCredentials) Put(_ context.Context, workspace, id string, creds map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(creds))
	for k, v := range creds {
		cp[k] = v
	}
	m.creds[key(workspace, id)] = cp
	return nil
}

func (m *MemoryCredentials) Get(_ context.Context, workspace, id string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[key(workspace, id)]
	if !ok {
		return nil, ErrNoCredentials
	}
	cp := make(map[string]string, len(c))
	for k, v := range c {
		cp[k] = v
	}
	return cp, nil
}

func (m *MemoryCredentials) Delete(_ context.Context, workspace, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.creds, key(workspace, id))
	return nil
}
