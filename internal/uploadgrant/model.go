// Package uploadgrant owns booth-storage's own record of which app may make a
// create-only write into which backend prefix (ADR 0117 "Reconciled design" item 1/8a):
// storage holds this grant, not booth-core — a third copy of data two modules already
// own was the thing the reconciled design rejected.
//
// A grant is keyed by (workspace, principal, backend, prefix); principal is the literal
// subject the app's workload token carries (ADR 0117, ruled 2026-10-15), e.g.
// "streamlit:acme:a2b3c4". It never grants write access on its own — internal/api's
// admission rule also needs the live token and the role it carries.
package uploadgrant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrNotFound means no grant with that ID exists in the workspace.
	ErrNotFound = errors.New("upload grant not found")
)

// ValidationError is a caller-fixable problem with a request (HTTP 400/422), as opposed
// to an infrastructure failure.
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

// Grant is one upload grant's persisted state (ADR 0117 point 8a).
type Grant struct {
	ID         string
	Workspace  string
	Principal  string
	BackendID  string
	Prefix     string // "" or ends in "/" — see backend.CleanPrefix
	Extensions []string
	MaxBytes   int64
	GrantedBy  string
	CreatedAt  time.Time
}

// Store persists Grants. Implementations must be safe for concurrent use.
type Store interface {
	// Upsert creates g, or — if a grant already exists for the same (Workspace,
	// Principal, BackendID, Prefix) — replaces it outright with g's own ID, Extensions,
	// MaxBytes, GrantedBy and CreatedAt (ADR 0117: "a second grant for the same app and
	// API replaces the first", mirroring booth-api's write-grants contract). Returns the
	// grant as stored.
	Upsert(ctx context.Context, g Grant) (Grant, error)
	// Get returns ErrNotFound if absent.
	Get(ctx context.Context, workspace, id string) (Grant, error)
	// List returns every grant in the workspace, optionally narrowed to one principal
	// (used by both the admin GET and the admission lookup), ordered by ID.
	List(ctx context.Context, workspace, principal string) ([]Grant, error)
	// Delete returns ErrNotFound if absent.
	Delete(ctx context.Context, workspace, id string) error
	// DeleteByPrincipal removes every grant the principal holds in the workspace,
	// across every backend, and returns what was removed. Idempotent: an empty slice
	// and a nil error when the principal holds nothing.
	DeleteByPrincipal(ctx context.Context, workspace, principal string) ([]Grant, error)
	// Ping reports whether the store is reachable, for the health check.
	Ping(ctx context.Context) error
}

// principalRE is ADR 0117's literal app-token subject shape (ruled 2026-10-15):
// "streamlit:<workspace>:<appId>". <appId> matches booth-api's own contract
// (docs/contracts/app-writes.md): [A-Za-z0-9._-]{1,128}.
var principalRE = regexp.MustCompile(`^streamlit:([a-z0-9-]+):([A-Za-z0-9._-]{1,128})$`)

// principalWorkspace returns the workspace segment of a well-formed principal, and
// whether it parsed at all.
func principalWorkspace(principal string) (string, bool) {
	m := principalRE.FindStringSubmatch(principal)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ValidatePrincipal checks principal is a well-formed app-token subject for workspace —
// mirroring booth-api's "400: the principal isn't an app in this workspace".
func ValidatePrincipal(principal, workspace string) error {
	ws, ok := principalWorkspace(principal)
	if !ok {
		return invalid("principal", "must be an app's workload-token subject, streamlit:<workspace>:<appId>")
	}
	if ws != workspace {
		return invalid("principal", "names workspace %q, not %q", ws, workspace)
	}
	return nil
}

// extensionRE bounds one file extension: lowercase letters/digits, no leading dot.
var extensionRE = regexp.MustCompile(`^[a-z0-9]{1,15}$`)

// ValidateExtensions normalizes and checks a grant's extension allowlist: non-empty,
// each a bare lowercase extension (no dot), deduplicated.
func ValidateExtensions(exts []string) ([]string, error) {
	if len(exts) == 0 {
		return nil, invalid("extensions", "at least one file extension is required")
	}
	seen := make(map[string]bool, len(exts))
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimPrefix(e, "."))
		if !extensionRE.MatchString(e) {
			return nil, invalid("extensions", "%q must be 1-15 lowercase letters or digits, no dot", e)
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out, nil
}

// HasExtension reports whether name's extension (case-insensitive, no dot) is in exts.
func HasExtension(name string, exts []string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return false
	}
	ext := strings.ToLower(name[i+1:])
	for _, e := range exts {
		if e == ext {
			return true
		}
	}
	return false
}
