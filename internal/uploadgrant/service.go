package uploadgrant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Service is upload grants' business logic: validated CRUD plus the admission lookup
// internal/api's create-only write route calls on every request. It is the only thing
// the HTTP layer talks to.
type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

// Ping reports whether the store is reachable, for the health check.
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// CreateInput is a request to grant one principal a create-only write into one prefix.
type CreateInput struct {
	Principal  string
	BackendID  string
	Prefix     string
	Extensions []string
	MaxBytes   int64
}

// Create validates and stores a grant. A second Create for the same (workspace,
// principal, backendID, prefix) replaces the first (Store.Upsert) — the owner "saving"
// a target is just this call, mirroring booth-api's write-grants contract.
func (s *Service) Create(ctx context.Context, workspace, grantedBy string, in CreateInput) (Grant, error) {
	if err := ValidatePrincipal(in.Principal, workspace); err != nil {
		return Grant{}, err
	}
	if strings.TrimSpace(in.BackendID) == "" {
		return Grant{}, invalid("backendId", "is required")
	}
	prefix, err := backend.CleanPrefix(in.Prefix)
	if err != nil {
		return Grant{}, invalid("prefix", "%v", err)
	}
	exts, err := ValidateExtensions(in.Extensions)
	if err != nil {
		return Grant{}, err
	}
	if in.MaxBytes <= 0 {
		return Grant{}, invalid("maxBytes", "must be greater than zero")
	}

	id, err := uuid.NewRandom()
	if err != nil {
		return Grant{}, fmt.Errorf("generating grant id: %w", err)
	}
	g := Grant{
		ID: id.String(), Workspace: workspace, Principal: in.Principal, BackendID: in.BackendID,
		Prefix: prefix, Extensions: exts, MaxBytes: in.MaxBytes, GrantedBy: grantedBy, CreatedAt: s.now(),
	}
	return s.store.Upsert(ctx, g)
}

// Get returns one grant.
func (s *Service) Get(ctx context.Context, workspace, id string) (Grant, error) {
	return s.store.Get(ctx, workspace, id)
}

// List returns every grant in the workspace, or only principal's if non-empty.
func (s *Service) List(ctx context.Context, workspace, principal string) ([]Grant, error) {
	return s.store.List(ctx, workspace, principal)
}

// Delete removes one grant by ID.
func (s *Service) Delete(ctx context.Context, workspace, id string) error {
	return s.store.Delete(ctx, workspace, id)
}

// RevokePrincipal removes every grant principal holds in the workspace, across every
// backend, and returns what was removed (idempotent: empty, not an error, the second
// time). booth-streamlit calls this on take-ownership (ADR 0117 "Reconciled design" item
// 1) and must fail closed on anything but success — this call has no partial-failure
// mode of its own to report back.
func (s *Service) RevokePrincipal(ctx context.Context, workspace, principal string) ([]Grant, error) {
	return s.store.DeleteByPrincipal(ctx, workspace, principal)
}

// MatchForUpload is the admission lookup (ADR 0117 point 8a): is there a live grant that
// admits principal's create-only write of path into backendID? path must already be
// backend.CleanPath-cleaned. A match requires the grant's prefix to equal path's
// directory exactly — "the cleaned path exactly one segment below the grant prefix" — so
// a grant over "incoming/" never admits "incoming/sub/x.csv", only "incoming/x.csv".
func (s *Service) MatchForUpload(ctx context.Context, workspace, principal, backendID, path string) (Grant, bool, error) {
	grants, err := s.store.List(ctx, workspace, principal)
	if err != nil {
		return Grant{}, false, err
	}
	dir := dirOf(path)
	for _, g := range grants {
		if g.BackendID == backendID && g.Prefix == dir {
			return g, true, nil
		}
	}
	return Grant{}, false, nil
}

// dirOf returns the directory portion of a cleaned object path: everything up to and
// including the final "/", or "" if path has no "/" (an object directly at the root).
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i+1]
	}
	return ""
}
