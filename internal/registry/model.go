// Package registry owns booth-storage's collection of registered backends (ADR 0035):
// which backends a workspace has, how each is configured, and how to open one by ID.
//
// The data model is deliberately split in two, following ADR 0020:
//
//   - Non-secret metadata (id, display name, kind, config such as bucket or root path)
//     lives in PostgreSQL (ADR 0014) via MetadataStore.
//   - Credentials never touch the database. They live in Kubernetes Secrets, one per
//     backend, written and read by CredentialStore — booth-storage acting as the
//     "granting module" ADR 0020 names.
//
// A workspace's backends are a collection keyed by (workspace, id) — there is no
// "current backend" concept anywhere in this package.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
)

var (
	// ErrNotFound means no backend with that ID exists in the workspace.
	ErrNotFound = errors.New("backend not found")
	// ErrExists means a backend with that ID already exists in the workspace.
	ErrExists = errors.New("backend already exists")
	// ErrNoCredentials means a backend's credential Secret is missing — either the
	// backend needs none (callers treat it as "empty") or it was deleted out from under us.
	ErrNoCredentials = errors.New("no credentials stored")
)

// ValidationError is a caller-fixable problem with a request (HTTP 400/422), as opposed
// to an infrastructure failure (HTTP 5xx).
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Record is one registered backend's persisted, non-secret state.
type Record struct {
	Workspace   string          `json:"workspace"`
	ID          string          `json:"id"`
	DisplayName string          `json:"displayName"`
	Kind        backend.Kind    `json:"kind"`
	Config      json.RawMessage `json:"config"`
	// HasCredentials records whether a credential Secret was written for this backend,
	// so the admin view can show "credentials set" without reading the Secret.
	HasCredentials bool `json:"hasCredentials"`
	// CreateOnlyVerified records whether this backend has passed the one-time
	// create-only capability probe (ADR 0117 point 8a): only meaningful for an S3
	// backend, where honouring "If-None-Match: *" depends on the specific
	// S3-compatible server, not on this module's SDK. Set by Service.TestSaved, and
	// reset to false whenever the backend's Config changes (a different endpoint or
	// bucket is an unproven server until tested again). Ignored for every other kind —
	// see SupportsCreateOnly.
	CreateOnlyVerified bool      `json:"-"`
	CreatedBy          string    `json:"createdBy,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// SupportsCreateOnly reports whether a create-only write (ADR 0117 point 8) may be
// attempted against rec right now. Filesystem, Azure and GCS are always reliable — their
// atomicity comes from the OS or the service's own documented, enforced precondition, not
// from anything this module has to prove. An S3 backend is only as reliable as whichever
// S3-compatible server an operator registered, so it needs CreateOnlyVerified first.
func SupportsCreateOnly(rec Record) bool {
	return rec.Kind != backend.KindS3 || rec.CreateOnlyVerified
}

// MetadataStore persists Records. Implementations must be safe for concurrent use and
// must keep (Workspace, ID) unique.
type MetadataStore interface {
	// Create inserts r, returning ErrExists if (Workspace, ID) is taken.
	Create(ctx context.Context, r Record) error
	// Get returns ErrNotFound if absent.
	Get(ctx context.Context, workspace, id string) (Record, error)
	// List returns every record in the workspace, ordered by ID.
	List(ctx context.Context, workspace string) ([]Record, error)
	// Update replaces the mutable fields (DisplayName, Config, HasCredentials,
	// CreateOnlyVerified, UpdatedAt) of an existing record, returning ErrNotFound if absent.
	Update(ctx context.Context, r Record) error
	// Delete returns ErrNotFound if absent.
	Delete(ctx context.Context, workspace, id string) error
	// SetCreateOnlyVerified records the outcome of the create-only capability probe
	// (ADR 0117 point 8a) without touching UpdatedAt — it is a fact learned about the
	// backend, not an edit to it, so it must not invalidate the Open cache or bump the
	// record's "last changed" story. ErrNotFound if absent.
	SetCreateOnlyVerified(ctx context.Context, workspace, id string, verified bool) error
	// Ping reports whether the store is reachable, for the health check.
	Ping(ctx context.Context) error
}

// CredentialStore persists each backend's secret fields (ADR 0020). Values are opaque
// key/value strings whose meaning belongs to the backend kind.
type CredentialStore interface {
	// Put replaces the credentials for a backend wholesale.
	Put(ctx context.Context, workspace, id string, creds map[string]string) error
	// Get returns ErrNoCredentials if none are stored.
	Get(ctx context.Context, workspace, id string) (map[string]string, error)
	// Delete is idempotent: removing credentials that don't exist is not an error.
	Delete(ctx context.Context, workspace, id string) error
}

var (
	// workspaceRE is the workspace slug shape from ADR 0025. It is re-validated here, not
	// just trusted from the gateway header, because the slug is interpolated into
	// filesystem paths (FilesystemPolicy) and Secret names.
	workspaceRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	// idRE bounds a backend ID to something safe in URLs, Secret labels and logs.
	idRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidateWorkspace checks a workspace slug.
func ValidateWorkspace(ws string) error {
	if len(ws) == 0 || len(ws) > 63 || !workspaceRE.MatchString(ws) {
		return invalid("workspace", "must be a slug of lowercase letters, digits and hyphens (max 63 characters)")
	}
	return nil
}

// ValidateID checks a backend ID.
func ValidateID(id string) error {
	if !idRE.MatchString(id) {
		return invalid("id", "must be 1-63 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit")
	}
	return nil
}
