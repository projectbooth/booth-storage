package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// openCacheTTL bounds how long a constructed backend client is reused. Cache entries are
// also invalidated by the record's UpdatedAt changing, but a credential rotated
// out-of-band (someone editing the Secret directly) doesn't bump UpdatedAt — the TTL is
// what eventually picks that up.
const openCacheTTL = 10 * time.Minute

// Service is the registry's business logic: validated CRUD over registered backends
// plus opening one by ID. It is the only thing the HTTP layer talks to.
type Service struct {
	meta     MetadataStore
	creds    CredentialStore
	fsPolicy FilesystemPolicy
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedBackend
}

type cachedBackend struct {
	updatedAt time.Time
	loadedAt  time.Time
	backend   backend.Backend
}

// NewService assembles a Service over the given stores.
func NewService(meta MetadataStore, creds CredentialStore, fsPolicy FilesystemPolicy) *Service {
	return &Service{
		meta:     meta,
		creds:    creds,
		fsPolicy: fsPolicy,
		now:      func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		cache:    make(map[string]cachedBackend),
	}
}

// FilesystemEnabled reports whether the filesystem kind can be registered at all, so the
// admin UI can hide it rather than offer a kind that will always be refused.
func (s *Service) FilesystemEnabled() bool { return s.fsPolicy.Enabled() }

// Ping reports whether the metadata store is reachable, for the health check.
func (s *Service) Ping(ctx context.Context) error { return s.meta.Ping(ctx) }

// CreateInput is a request to register a backend.
type CreateInput struct {
	ID          string
	DisplayName string
	Kind        backend.Kind
	Config      json.RawMessage
	Credentials map[string]string
}

// UpdateInput is a request to change a backend. Nil fields mean "leave unchanged" —
// in particular, omitting Credentials keeps the stored ones, so an admin can rename a
// backend or fix a bucket without re-entering secrets they can no longer see.
type UpdateInput struct {
	DisplayName *string
	Config      json.RawMessage
	Credentials map[string]string
}

func key(workspace, id string) string { return workspace + "/" + id }

// Create registers a new backend. The ID is chosen by the admin and is the stable
// address every downstream module uses (ADR 0035), so it cannot change afterwards.
func (s *Service) Create(ctx context.Context, workspace, actor string, in CreateInput) (Record, error) {
	if err := ValidateWorkspace(workspace); err != nil {
		return Record{}, err
	}
	if err := ValidateID(in.ID); err != nil {
		return Record{}, err
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		name = in.ID
	}
	if len(name) > 100 {
		return Record{}, invalid("displayName", "must be at most 100 characters")
	}
	if !in.Kind.Valid() {
		return Record{}, invalid("kind", "unsupported kind %q (supported: %s)", in.Kind, kindList())
	}
	cfg, err := normalizeConfig(in.Kind, in.Config, workspace, s.fsPolicy)
	if err != nil {
		return Record{}, err
	}
	if err := validateCredentials(in.Kind, in.Credentials); err != nil {
		return Record{}, err
	}
	// A filesystem backend's directory must be usable *now*: a bad path is a 4xx here rather
	// than a 502 on every later read and write. A missing leaf (with an existing parent) is
	// created; see checkFilesystemRoot for why only the leaf.
	if in.Kind == backend.KindFilesystem {
		if _, err := checkFilesystemRoot(cfg, true); err != nil {
			return Record{}, err
		}
	}
	// Build (without contacting the service) to catch combinations the field-level
	// checks can't see, e.g. an Azure backend with no credential at all.
	if _, err := buildBackend(in.Kind, cfg, in.Credentials, workspace, s.fsPolicy); err != nil {
		return Record{}, invalid("", "%v", err)
	}

	now := s.now()
	rec := Record{
		Workspace: workspace, ID: in.ID, DisplayName: name, Kind: in.Kind, Config: cfg,
		HasCredentials: len(in.Credentials) > 0, CreatedBy: actor, CreatedAt: now, UpdatedAt: now,
	}

	// Reserve the ID first. Writing the credentials first would let a duplicate-ID
	// request overwrite the existing backend's Secret before being rejected.
	if err := s.meta.Create(ctx, rec); err != nil {
		return Record{}, err
	}
	if len(in.Credentials) > 0 {
		if err := s.creds.Put(ctx, workspace, in.ID, in.Credentials); err != nil {
			if derr := s.meta.Delete(ctx, workspace, in.ID); derr != nil {
				log.Printf("registry: rolling back backend %s after credential write failed: %v", key(workspace, in.ID), derr)
			}
			return Record{}, fmt.Errorf("storing credentials: %w", err)
		}
	}
	return rec, nil
}

// preparedUpdate is a validated, not-yet-persisted change to an existing backend.
type preparedUpdate struct {
	rec Record
	// creds are the credentials the updated backend will actually use: the newly
	// supplied set if the request replaces them, otherwise the ones already stored.
	creds        map[string]string
	replaceCreds bool
	newCreds     map[string]string
}

// prepareUpdate validates an update against the current record without persisting
// anything, shared by Update and TestUpdate so "test" and "save" can never disagree
// about what a given request means.
func (s *Service) prepareUpdate(ctx context.Context, workspace, id string, in UpdateInput) (preparedUpdate, error) {
	rec, err := s.Get(ctx, workspace, id)
	if err != nil {
		return preparedUpdate{}, err
	}
	prev := rec.UpdatedAt

	if in.DisplayName != nil {
		name := strings.TrimSpace(*in.DisplayName)
		if name == "" {
			name = id
		}
		if len(name) > 100 {
			return preparedUpdate{}, invalid("displayName", "must be at most 100 characters")
		}
		rec.DisplayName = name
	}
	if len(in.Config) > 0 {
		cfg, err := normalizeConfig(rec.Kind, in.Config, workspace, s.fsPolicy)
		if err != nil {
			return preparedUpdate{}, err
		}
		rec.Config = cfg
	}

	p := preparedUpdate{rec: rec, creds: in.Credentials, replaceCreds: in.Credentials != nil, newCreds: in.Credentials}
	if p.replaceCreds {
		if err := validateCredentials(rec.Kind, in.Credentials); err != nil {
			return preparedUpdate{}, err
		}
		p.rec.HasCredentials = len(in.Credentials) > 0
	} else if rec.HasCredentials {
		// Not replacing: validate the new config against the credentials already stored.
		stored, err := s.creds.Get(ctx, workspace, id)
		if err != nil && !errors.Is(err, ErrNoCredentials) {
			return preparedUpdate{}, fmt.Errorf("reading stored credentials: %w", err)
		}
		p.creds = stored
	}
	if _, err := buildBackend(rec.Kind, p.rec.Config, p.creds, workspace, s.fsPolicy); err != nil {
		return preparedUpdate{}, invalid("", "%v", err)
	}

	// Strictly increasing, so the Open cache (keyed on UpdatedAt) can never mistake a
	// changed record for an unchanged one, even for two updates in the same microsecond.
	p.rec.UpdatedAt = s.now()
	if !p.rec.UpdatedAt.After(prev) {
		p.rec.UpdatedAt = prev.Add(time.Microsecond)
	}
	return p, nil
}

// Update changes an existing backend. Kind and ID are immutable.
func (s *Service) Update(ctx context.Context, workspace, id string, in UpdateInput) (Record, error) {
	p, err := s.prepareUpdate(ctx, workspace, id, in)
	if err != nil {
		return Record{}, err
	}
	if p.rec.Kind == backend.KindFilesystem && len(in.Config) > 0 {
		if _, err := checkFilesystemRoot(p.rec.Config, true); err != nil {
			return Record{}, err
		}
	}

	// Credentials first: if the metadata write then fails, the worst case is a Secret
	// that's newer than the record, which the next successful update reconciles.
	if p.replaceCreds {
		if len(p.newCreds) == 0 {
			if err := s.creds.Delete(ctx, workspace, id); err != nil {
				return Record{}, fmt.Errorf("removing credentials: %w", err)
			}
		} else if err := s.creds.Put(ctx, workspace, id, p.newCreds); err != nil {
			return Record{}, fmt.Errorf("storing credentials: %w", err)
		}
	}
	if err := s.meta.Update(ctx, p.rec); err != nil {
		return Record{}, err
	}
	s.invalidate(workspace, id)
	return p.rec, nil
}

// TestUpdate checks connectivity for what an Update would produce, without saving it —
// the edit form's "test connection". Credentials the request leaves out are taken from
// the stored ones, since an admin editing a bucket name can't re-enter a secret they
// were never shown.
func (s *Service) TestUpdate(ctx context.Context, workspace, id string, in UpdateInput) error {
	p, err := s.prepareUpdate(ctx, workspace, id, in)
	if err != nil {
		return err
	}
	if p.rec.Kind == backend.KindFilesystem && len(in.Config) > 0 {
		st, err := checkFilesystemRoot(p.rec.Config, false) // testing never creates anything
		if err != nil {
			return asTestFailure(err)
		}
		if st == rootWillBeCreated {
			return nil // it doesn't exist yet, but saving will create it
		}
	}
	b, err := buildBackend(p.rec.Kind, p.rec.Config, p.creds, workspace, s.fsPolicy)
	if err != nil {
		return invalid("", "%v", err)
	}
	return b.Check(ctx)
}

// Delete unregisters a backend and its credentials. It never touches the data in the
// underlying store — removing a registration must not be able to destroy anything.
func (s *Service) Delete(ctx context.Context, workspace, id string) error {
	if err := ValidateWorkspace(workspace); err != nil {
		return err
	}
	if err := s.meta.Delete(ctx, workspace, id); err != nil {
		return err
	}
	s.invalidate(workspace, id)
	// The record is already gone. A leftover Secret is inert and is overwritten if the
	// ID is ever reused, so log rather than fail a delete that has effectively succeeded.
	if err := s.creds.Delete(ctx, workspace, id); err != nil {
		log.Printf("registry: backend %s deleted but its credentials could not be removed: %v", key(workspace, id), err)
	}
	return nil
}

// Get returns one backend's record.
func (s *Service) Get(ctx context.Context, workspace, id string) (Record, error) {
	if err := ValidateWorkspace(workspace); err != nil {
		return Record{}, err
	}
	return s.meta.Get(ctx, workspace, id)
}

// List returns every backend registered in the workspace, ordered by ID.
func (s *Service) List(ctx context.Context, workspace string) ([]Record, error) {
	if err := ValidateWorkspace(workspace); err != nil {
		return nil, err
	}
	return s.meta.List(ctx, workspace)
}

// Open returns a ready-to-use backend for a registered ID — the entry point the data API
// uses. Constructed clients are cached until the record changes.
func (s *Service) Open(ctx context.Context, workspace, id string) (backend.Backend, error) {
	rec, err := s.Get(ctx, workspace, id)
	if err != nil {
		return nil, err
	}

	k := key(workspace, id)
	s.mu.Lock()
	c, ok := s.cache[k]
	s.mu.Unlock()
	if ok && c.updatedAt.Equal(rec.UpdatedAt) && s.now().Sub(c.loadedAt) < openCacheTTL {
		return c.backend, nil
	}

	var creds map[string]string
	if rec.HasCredentials {
		creds, err = s.creds.Get(ctx, workspace, id)
		if err != nil {
			if errors.Is(err, ErrNoCredentials) {
				return nil, fmt.Errorf("backend %q is registered but its credentials are missing — an admin needs to re-enter them", id)
			}
			return nil, fmt.Errorf("reading credentials: %w", err)
		}
	}
	b, err := buildBackend(rec.Kind, rec.Config, creds, workspace, s.fsPolicy)
	if err != nil {
		return nil, fmt.Errorf("opening backend %q: %w", id, err)
	}

	s.mu.Lock()
	s.cache[k] = cachedBackend{updatedAt: rec.UpdatedAt, loadedAt: s.now(), backend: b}
	s.mu.Unlock()
	return b, nil
}

// Test checks connectivity for a not-yet-saved spec, so an admin can verify a form
// before committing it. Nothing is persisted.
func (s *Service) Test(ctx context.Context, workspace string, in CreateInput) error {
	if err := ValidateWorkspace(workspace); err != nil {
		return err
	}
	if !in.Kind.Valid() {
		return invalid("kind", "unsupported kind %q (supported: %s)", in.Kind, kindList())
	}
	cfg, err := normalizeConfig(in.Kind, in.Config, workspace, s.fsPolicy)
	if err != nil {
		return err
	}
	if err := validateCredentials(in.Kind, in.Credentials); err != nil {
		return err
	}
	if in.Kind == backend.KindFilesystem {
		st, err := checkFilesystemRoot(cfg, false) // testing never creates anything
		if err != nil {
			return asTestFailure(err)
		}
		if st == rootWillBeCreated {
			return nil // it doesn't exist yet, but saving will create it
		}
	}
	b, err := buildBackend(in.Kind, cfg, in.Credentials, workspace, s.fsPolicy)
	if err != nil {
		return invalid("", "%v", err)
	}
	return b.Check(ctx)
}

// TestSaved checks connectivity for an already-registered backend.
func (s *Service) TestSaved(ctx context.Context, workspace, id string) error {
	b, err := s.Open(ctx, workspace, id)
	if err != nil {
		return err
	}
	return b.Check(ctx)
}

// asTestFailure turns a root-directory validation problem into a plain error. Registering
// refuses such a path with a 4xx, but *testing* one is a question ("would this work?") whose
// honest answer is "no, because ...", which the API reports as a failed connection rather than
// a malformed request — the same way an unreachable bucket is reported.
func asTestFailure(err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return errors.New(ve.Message)
	}
	return err
}

func (s *Service) invalidate(workspace, id string) {
	s.mu.Lock()
	delete(s.cache, key(workspace, id))
	s.mu.Unlock()
}
