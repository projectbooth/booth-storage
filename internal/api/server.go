// Package api is booth-storage's HTTP surface, reached through booth-core's gateway at
// /modules/storage/* once installed (the gateway strips that prefix, so routes here
// start at /api/...).
//
// Three tiers, matching ADR 0035/0036 and the workspace roles of ADR 0025:
//
//   - Regular view (any role): list registered backends, and list/read their objects.
//   - Data writes (editor, owner): write objects, create folders, rename/move, delete
//     (ADR 0038). Viewers are read-only.
//   - Admin view (owner only): register, edit, remove backends and their credentials.
//     Mounted under /api/admin so the permission boundary is one route group, enforced
//     server-side no matter what any UI shows (ADR 0036).
//
// Credentials only ever flow inward on the admin routes and are never returned by any
// route — responses say whether credentials are set, never what they are (ADR 0020).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/projectbooth/booth-storage/internal/auth"
	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/credentialbroker"
	"github.com/projectbooth/booth-storage/internal/registry"
	"github.com/projectbooth/booth-storage/internal/uploadgrant"
)

// maxJSONBody bounds admin request bodies; a GCS service-account key is the largest
// legitimate payload at a few KB.
const maxJSONBody = 256 << 10

// Deps is everything the HTTP layer needs, assembled by cmd/storage/main.go.
type Deps struct {
	Verifier auth.TokenVerifier
	Registry *registry.Service
	// Grants holds upload grants (ADR 0117 "Reconciled design" item 1/8a): which app
	// principal may make a create-only write into which backend prefix. Drives both the
	// owner-only /api/admin/upload-grants management routes and the admission check in
	// handleWriteObject.
	Grants *uploadgrant.Service
	// MaxUploadBytes caps one object write; 0 means unlimited.
	MaxUploadBytes int64
	// CredentialBrokerCredential is this module's copy of the shared secret core presents
	// to the ADR 0080 credential-broker provider route (POST /internal/credentials, see
	// internal/credentialbroker) — the route is always mounted, but empty means every call
	// is refused: there is no meaningful "unconfigured, so allow" state for it.
	CredentialBrokerCredential string
}

func NewRouter(deps Deps) http.Handler {
	s := &server{deps}

	r := chi.NewRouter()
	r.Use(middleware.Logger) // stdout logging only, per ADR 0022 — no logging API to integrate against
	r.Use(middleware.Recoverer)

	// Unauthenticated. /healthz is the healthCheckPath declared in this module's
	// BoothModule manifest (contracts/module-manifest.md) for booth-core to poll, so it
	// reflects real readiness (can we reach our database?). /livez is for the kubelet's
	// liveness probe and deliberately does not: restarting the pod because Postgres
	// blipped would only make an outage worse.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// ADR 0080's credential-broker provider route: called directly by booth-core (never
	// through the gateway), authenticated by its own shared-secret mechanism rather than
	// auth.Middleware's OIDC verification — see internal/credentialbroker's package doc.
	r.Post(credentialbroker.ProviderPath, credentialbroker.NewHandler(credentialbroker.Deps{
		Credential: deps.CredentialBrokerCredential,
		Registry:   deps.Registry,
	}).ServeHTTP)

	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(deps.Verifier))

		// Regular view + generic data API.
		r.Group(func(r chi.Router) {
			r.Use(auth.Require(auth.Identity.CanRead, "your workspace role does not permit reading storage"))
			r.Get("/api/kinds", s.handleKinds)
			r.Get("/api/backends", s.handleListBackends)
			r.Get("/api/backends/{id}", s.handleGetBackend)
			r.Get("/api/backends/{id}/objects", s.handleListObjects)
			r.Get("/api/backends/{id}/objects/*", s.handleReadObject)
		})
		// PUT .../objects/* does its own authorization inline (ADR 0117 point 8a),
		// rather than sitting behind auth.Require(CanWrite): editors and owners may
		// always write, unchanged; a viewer-capped app workload token may additionally
		// make exactly one create-only write into a prefix where a live upload grant
		// admits it. Every other write route below is untouched and still CanWrite-gated,
		// so a plain PUT, DELETE and folder delete stay 403 for such a token by
		// construction — see handleWriteObject.
		r.Put("/api/backends/{id}/objects/*", s.handleWriteObject)

		r.Group(func(r chi.Router) {
			r.Use(auth.Require(auth.Identity.CanWrite, "only workspace editors and owners can write objects"))
			r.Delete("/api/backends/{id}/objects/*", s.handleDeleteObject)
			r.Post("/api/backends/{id}/folders", s.handleCreateFolder)
			r.Delete("/api/backends/{id}/folders/*", s.handleDeleteFolder)
			r.Post("/api/backends/{id}/move", s.handleMove)
		})

		// Admin view: owner only.
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(auth.Require(auth.Identity.IsAdmin, "only workspace owners can manage storage backends and credentials"))
			r.Get("/backends", s.handleAdminList)
			r.Post("/backends", s.handleAdminCreate)
			r.Get("/backends/{id}", s.handleAdminGet)
			r.Put("/backends/{id}", s.handleAdminUpdate)
			r.Delete("/backends/{id}", s.handleAdminDelete)
			r.Post("/backends/{id}/test", s.handleAdminTestSaved)
			r.Post("/test-connection", s.handleAdminTestNew)

			// Upload grants (ADR 0117 "Reconciled design" item 1/8a): owner-only, no
			// update route — narrowing a grant is delete-and-recreate.
			r.Get("/upload-grants", s.handleAdminListUploadGrants)
			r.Post("/upload-grants", s.handleAdminCreateUploadGrant)
			r.Delete("/upload-grants/{id}", s.handleAdminDeleteUploadGrant)
			r.Post("/upload-grants/revoke-principal", s.handleAdminRevokeUploadGrantsByPrincipal)
		})
	})

	return r
}

type server struct{ Deps }

// ---- response shapes -------------------------------------------------------

// BackendSummary is what the regular view and downstream modules see: enough to choose a
// backend by, with no configuration detail beyond a short location.
type BackendSummary struct {
	ID          string       `json:"id"`
	DisplayName string       `json:"displayName"`
	Kind        backend.Kind `json:"kind"`
	Location    string       `json:"location"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

// AdminBackend adds the full non-secret configuration, for the admin view only.
type AdminBackend struct {
	BackendSummary
	Config         json.RawMessage `json:"config"`
	CredentialsSet bool            `json:"credentialsSet"`
	CreatedBy      string          `json:"createdBy,omitempty"`
	// CreateOnlySupported reports whether an app's create-only upload (ADR 0117 point 8)
	// can be admitted against this backend right now. Always true except for an S3
	// backend that hasn't yet passed the capability probe ("test connection" runs it).
	CreateOnlySupported bool `json:"createOnlySupported"`
}

func summarize(r registry.Record) BackendSummary {
	return BackendSummary{
		ID: r.ID, DisplayName: r.DisplayName, Kind: r.Kind,
		Location: registry.Location(r.Kind, r.Config), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func adminView(r registry.Record) AdminBackend {
	return AdminBackend{
		BackendSummary: summarize(r), Config: r.Config, CredentialsSet: r.HasCredentials, CreatedBy: r.CreatedBy,
		CreateOnlySupported: registry.SupportsCreateOnly(r),
	}
}

// ---- health ----------------------------------------------------------------

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Registry.Ping(ctx); err != nil {
		log.Printf("health check failed: metadata store unreachable: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "checks": map[string]string{"database": "unreachable"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "checks": map[string]string{"database": "ok"}})
}

// ---- regular view ----------------------------------------------------------

// handleKinds tells the UI which kinds can actually be registered on this deployment, so
// it never offers the filesystem kind when the operator hasn't enabled it.
func (s *server) handleKinds(w http.ResponseWriter, r *http.Request) {
	kinds := make([]backend.Kind, 0, len(backend.Kinds))
	for _, k := range backend.Kinds {
		if k == backend.KindFilesystem && !s.Registry.FilesystemEnabled() {
			continue
		}
		kinds = append(kinds, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": kinds, "filesystemEnabled": s.Registry.FilesystemEnabled()})
}

func (s *server) handleListBackends(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	recs, err := s.Registry.List(r.Context(), id.Workspace)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	out := make([]BackendSummary, len(recs))
	for i, rec := range recs {
		out[i] = summarize(rec)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleGetBackend(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Registry.Get(r.Context(), identity(r).Workspace, chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(rec))
}

// ---- data API --------------------------------------------------------------

func (s *server) openBackend(w http.ResponseWriter, r *http.Request) (backend.Backend, bool) {
	b, err := s.Registry.Open(r.Context(), identity(r).Workspace, chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryError(w, err)
		return nil, false
	}
	return b, true
}

func (s *server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	opts := backend.ListOptions{Prefix: q.Get("prefix"), Cursor: q.Get("cursor"), Recursive: q.Get("recursive") == "true"}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			auth.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	res, err := b.List(r.Context(), opts)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// objectPath extracts the object path from a wildcard route. It reads the decoded URL
// path, so %2F and friends arrive as the characters they encode and are then subject to
// the same backend.CleanPath rules as everything else — an encoded ".." is rejected
// exactly like a literal one.
func objectPath(r *http.Request, id string) string {
	return strings.TrimPrefix(r.URL.Path, "/api/backends/"+id+"/objects/")
}

func (s *server) handleReadObject(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	path := objectPath(r, chi.URLParam(r, "id"))
	rc, info, err := b.Read(r.Context(), path)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer rc.Close()

	// Stored objects are user-supplied content served from this module's origin, so
	// neutralize anything a browser might try to execute or sniff: force a download,
	// forbid MIME sniffing, and sandbox any document that does get rendered.
	h := w.Header()
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": lastSegment(info.Path)}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
	// Headers are sent; a mid-stream failure can only truncate, which the client detects
	// via Content-Length. Nothing more useful to do than log it.
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("streaming object %q: %v", path, err)
	}
}

// handleWriteObject is PUT .../objects/*. It is deliberately not behind
// auth.Require(CanWrite): editors and owners may always write (unchanged); a
// viewer-capped app workload token may additionally make exactly one create-only write
// ("If-None-Match: *") into a prefix where internal/uploadgrant holds a live grant for
// its principal (ADR 0117 point 8a). Every other shape — a plain PUT from such a token,
// or any request from a role that is neither editor/owner nor an admitted grant — falls
// through to the same 403 CanWrite used to guard everywhere, so DELETE and folder-delete
// (genuinely still CanWrite-gated, unchanged) are not the only things keeping an app
// token from doing more than its one admitted call.
func (s *server) handleWriteObject(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	backendID := chi.URLParam(r, "id")
	clean, err := backend.CleanPath(objectPath(r, backendID))
	if err != nil {
		writeBackendError(w, err)
		return
	}
	createOnly := r.Header.Get("If-None-Match") == "*"

	var grant *uploadgrant.Grant
	if !id.CanWrite() {
		if !createOnly {
			auth.WriteError(w, http.StatusForbidden, "only workspace editors and owners can write objects")
			return
		}
		// Not an editor/owner, but asking for a create-only write: the only shape a
		// viewer-capped app workload token is ever admitted through. auth.Middleware
		// already guarantees id.Role == RoleViewer here (CanWrite is false, and a token
		// granting no role was rejected before this handler ran).
		g, ok, merr := s.Grants.MatchForUpload(r.Context(), id.Workspace, id.Subject, backendID, clean)
		if merr != nil {
			log.Printf("upload grant lookup failed workspace=%s backend=%s path=%q by=%s: %v", id.Workspace, backendID, clean, id.Subject, merr)
			auth.WriteError(w, http.StatusBadGateway, "could not verify the upload grant")
			return
		}
		if !ok {
			auth.WriteError(w, http.StatusForbidden, "no live upload grant admits this write")
			return
		}
		if !uploadgrant.HasExtension(clean, g.Extensions) {
			auth.WriteError(w, http.StatusUnsupportedMediaType, "this file extension is not allowed by the upload grant")
			return
		}
		grant = &g
	}

	if createOnly {
		// 501 until an S3 backend has proven it honours the precondition (ADR 0117
		// point 8a) — this applies to every caller, not only an admitted app: nothing
		// may ever fall back to check-then-write.
		rec, rerr := s.Registry.Get(r.Context(), id.Workspace, backendID)
		if rerr != nil {
			writeRegistryError(w, rerr)
			return
		}
		if !registry.SupportsCreateOnly(rec) {
			auth.WriteError(w, http.StatusNotImplemented, "this backend has not passed the create-only capability probe")
			return
		}
	}

	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}

	maxBytes := s.MaxUploadBytes
	if grant != nil && (maxBytes <= 0 || grant.MaxBytes < maxBytes) {
		maxBytes = grant.MaxBytes
	}
	body := io.Reader(r.Body)
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBytes)
	}

	info, err := b.Write(r.Context(), clean, body, backend.WriteOptions{
		ContentType: cleanContentType(r.Header.Get("Content-Type")), CreateOnly: createOnly,
	})
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			auth.WriteError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("object exceeds the %d byte upload limit", maxBytes))
		case errors.Is(err, backend.ErrPreconditionFailed):
			auth.WriteError(w, http.StatusPreconditionFailed, "an object already exists at this path")
		default:
			writeBackendError(w, err)
		}
		if grant != nil {
			log.Printf("audit: app upload rejected workspace=%s backend=%s path=%q principal=%s grant=%s: %v", id.Workspace, backendID, clean, id.Subject, grant.ID, err)
		}
		return
	}
	if grant != nil {
		log.Printf("audit: app upload workspace=%s backend=%s path=%q principal=%s grant=%s size=%d", id.Workspace, backendID, clean, id.Subject, grant.ID, info.Size)
	}
	writeJSON(w, http.StatusOK, info)
}

// ---- folder operations and mutations (ADR 0038) ----
//
// Every route here sits behind auth.Require(CanWrite), so a viewer is rejected with 403
// before any handler runs — the server-side half of the "hide the controls in the UI *and*
// enforce on the server" requirement.

type pathRequest struct {
	Path string `json:"path"`
}

func (s *server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	var req pathRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := b.Mkdir(r.Context(), req.Path); err != nil {
		writeBackendError(w, err)
		return
	}
	folder, _ := backend.CleanFolder(req.Path)
	writeJSON(w, http.StatusCreated, map[string]string{"path": folder})
}

func (s *server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	path := objectPath(r, chi.URLParam(r, "id"))
	if err := b.DeleteObject(r.Context(), path); err != nil {
		writeBackendError(w, err)
		return
	}
	id := identity(r)
	log.Printf("audit: object deleted workspace=%s backend=%s path=%q by=%s", id.Workspace, chi.URLParam(r, "id"), path, id.Subject)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	backendID := chi.URLParam(r, "id")
	path := strings.TrimPrefix(r.URL.Path, "/api/backends/"+backendID+"/folders/")
	n, err := b.DeleteFolder(r.Context(), path)
	id := identity(r)
	if err != nil {
		// A partial failure may already have removed objects; record what we know.
		log.Printf("audit: folder delete failed workspace=%s backend=%s path=%q removed=%d by=%s: %v", id.Workspace, backendID, path, n, id.Subject, err)
		writeBackendError(w, err)
		return
	}
	log.Printf("audit: folder deleted workspace=%s backend=%s path=%q objects=%d by=%s", id.Workspace, backendID, path, n, id.Subject)
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

type moveRequest struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Folder bool   `json:"folder"`
}

func (s *server) handleMove(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	var req moveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var err error
	if req.Folder {
		err = b.MoveFolder(r.Context(), req.From, req.To)
	} else {
		err = b.MoveObject(r.Context(), req.From, req.To)
	}
	id := identity(r)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	log.Printf("audit: moved workspace=%s backend=%s from=%q to=%q folder=%t by=%s", id.Workspace, chi.URLParam(r, "id"), req.From, req.To, req.Folder, id.Subject)
	writeJSON(w, http.StatusOK, map[string]any{"from": req.From, "to": req.To, "folder": req.Folder})
}

// cleanContentType keeps only a well-formed media type, so a hostile header can't smuggle
// odd bytes into stored object metadata.
func cleanContentType(v string) string {
	if v == "" {
		return ""
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return mime.FormatMediaType(mt, params)
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---- admin view ------------------------------------------------------------

type createRequest struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName"`
	Kind        backend.Kind      `json:"kind"`
	Config      json.RawMessage   `json:"config"`
	Credentials map[string]string `json:"credentials"`
}

type updateRequest struct {
	DisplayName *string           `json:"displayName"`
	Config      json.RawMessage   `json:"config"`
	Credentials map[string]string `json:"credentials"`
}

func (u updateRequest) input() registry.UpdateInput {
	return registry.UpdateInput{DisplayName: u.DisplayName, Config: u.Config, Credentials: u.Credentials}
}

func (s *server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	recs, err := s.Registry.List(r.Context(), identity(r).Workspace)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	out := make([]AdminBackend, len(recs))
	for i, rec := range recs {
		out[i] = adminView(rec)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Registry.Get(r.Context(), identity(r).Workspace, chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminView(rec))
}

func (s *server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := identity(r)
	rec, err := s.Registry.Create(r.Context(), id.Workspace, id.Subject, registry.CreateInput{
		ID: req.ID, DisplayName: req.DisplayName, Kind: req.Kind, Config: req.Config, Credentials: req.Credentials,
	})
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	// Audit trail on stdout (ADR 0022). Records who changed what — never any credential.
	log.Printf("audit: backend created workspace=%s id=%s kind=%s by=%s credentials=%t", rec.Workspace, rec.ID, rec.Kind, id.Subject, rec.HasCredentials)
	writeJSON(w, http.StatusCreated, adminView(rec))
}

func (s *server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := identity(r)
	rec, err := s.Registry.Update(r.Context(), id.Workspace, chi.URLParam(r, "id"), req.input())
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	log.Printf("audit: backend updated workspace=%s id=%s by=%s credentialsReplaced=%t", rec.Workspace, rec.ID, id.Subject, req.Credentials != nil)
	writeJSON(w, http.StatusOK, adminView(rec))
}

func (s *server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	backendID := chi.URLParam(r, "id")
	if err := s.Registry.Delete(r.Context(), id.Workspace, backendID); err != nil {
		writeRegistryError(w, err)
		return
	}
	log.Printf("audit: backend deleted workspace=%s id=%s by=%s", id.Workspace, backendID, id.Subject)
	w.WriteHeader(http.StatusNoContent)
}

// TestResult reports a connectivity check. A failed check is a normal, expected answer
// ("your credentials are wrong"), not an API error, so it comes back as 200 with ok=false
// for the UI to show inline; only a malformed request is a 4xx.
type TestResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *server) handleAdminTestNew(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := s.Registry.Test(ctx, identity(r).Workspace, registry.CreateInput{Kind: req.Kind, Config: req.Config, Credentials: req.Credentials})
	writeTestResult(w, err)
}

// handleAdminTestSaved tests a registered backend. An optional body previews an edit
// without saving it (credentials left out are taken from the stored ones).
func (s *server) handleAdminTestSaved(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ws, id := identity(r).Workspace, chi.URLParam(r, "id")

	var req updateRequest
	if r.ContentLength != 0 {
		if !decodeBody(w, r, &req) {
			return
		}
	}
	var err error
	if req.DisplayName == nil && len(req.Config) == 0 && req.Credentials == nil {
		err = s.Registry.TestSaved(ctx, ws, id)
	} else {
		err = s.Registry.TestUpdate(ctx, ws, id, req.input())
	}
	writeTestResult(w, err)
}

func writeTestResult(w http.ResponseWriter, err error) {
	var ve *registry.ValidationError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, TestResult{OK: true})
	case errors.Is(err, registry.ErrNotFound), errors.As(err, &ve):
		writeRegistryError(w, err)
	default:
		writeJSON(w, http.StatusOK, TestResult{OK: false, Error: err.Error()})
	}
}

// ---- upload grants (ADR 0117 "Reconciled design" item 1/8a) ----------------

// uploadGrantView is the JSON shape of one grant, returned by every grant route.
type uploadGrantView struct {
	ID         string    `json:"id"`
	Workspace  string    `json:"workspace"`
	Principal  string    `json:"principal"`
	BackendID  string    `json:"backendId"`
	Prefix     string    `json:"prefix"`
	Extensions []string  `json:"extensions"`
	MaxBytes   int64     `json:"maxBytes"`
	GrantedBy  string    `json:"grantedBy"`
	CreatedAt  time.Time `json:"createdAt"`
}

func grantView(g uploadgrant.Grant) uploadGrantView {
	return uploadGrantView{
		ID: g.ID, Workspace: g.Workspace, Principal: g.Principal, BackendID: g.BackendID,
		Prefix: g.Prefix, Extensions: g.Extensions, MaxBytes: g.MaxBytes, GrantedBy: g.GrantedBy, CreatedAt: g.CreatedAt,
	}
}

func (s *server) handleAdminListUploadGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.Grants.List(r.Context(), identity(r).Workspace, r.URL.Query().Get("principal"))
	if err != nil {
		writeGrantError(w, err)
		return
	}
	out := make([]uploadGrantView, len(grants))
	for i, g := range grants {
		out[i] = grantView(g)
	}
	writeJSON(w, http.StatusOK, out)
}

type createUploadGrantRequest struct {
	Principal  string   `json:"principal"`
	BackendID  string   `json:"backendId"`
	Prefix     string   `json:"prefix"`
	Extensions []string `json:"extensions"`
	MaxBytes   int64    `json:"maxBytes"`
}

// handleAdminCreateUploadGrant creates a grant, or — for the same (principal, backend,
// prefix) — replaces one that already exists (uploadgrant.Service.Create). This is the
// single call booth-streamlit's "Write targets" panel makes to save an upload target.
func (s *server) handleAdminCreateUploadGrant(w http.ResponseWriter, r *http.Request) {
	var req createUploadGrantRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := identity(r)
	if _, err := s.Registry.Get(r.Context(), id.Workspace, req.BackendID); err != nil {
		writeRegistryError(w, err)
		return
	}
	g, err := s.Grants.Create(r.Context(), id.Workspace, id.Subject, uploadgrant.CreateInput{
		Principal: req.Principal, BackendID: req.BackendID, Prefix: req.Prefix, Extensions: req.Extensions, MaxBytes: req.MaxBytes,
	})
	if err != nil {
		writeGrantError(w, err)
		return
	}
	log.Printf("audit: upload grant created workspace=%s id=%s principal=%s backend=%s prefix=%q by=%s", g.Workspace, g.ID, g.Principal, g.BackendID, g.Prefix, id.Subject)
	writeJSON(w, http.StatusCreated, grantView(g))
}

func (s *server) handleAdminDeleteUploadGrant(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	grantID := chi.URLParam(r, "id")
	if err := s.Grants.Delete(r.Context(), id.Workspace, grantID); err != nil {
		writeGrantError(w, err)
		return
	}
	log.Printf("audit: upload grant deleted workspace=%s id=%s by=%s", id.Workspace, grantID, id.Subject)
	w.WriteHeader(http.StatusNoContent)
}

type revokePrincipalRequest struct {
	Principal string `json:"principal"`
}

// handleAdminRevokeUploadGrantsByPrincipal mirrors booth-api's
// POST /api/write-grants/revoke-principal exactly: revokes every live grant the
// principal holds in the workspace, across every backend, in one call. booth-streamlit
// calls this on take-ownership and must fail closed on anything but success — the
// {"revoked", "items"} shape and idempotency (0 the second time) are what let it do that.
func (s *server) handleAdminRevokeUploadGrantsByPrincipal(w http.ResponseWriter, r *http.Request) {
	var req revokePrincipalRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := identity(r)
	revoked, err := s.Grants.RevokePrincipal(r.Context(), id.Workspace, req.Principal)
	if err != nil {
		writeGrantError(w, err)
		return
	}
	items := make([]uploadGrantView, len(revoked))
	for i, g := range revoked {
		items[i] = grantView(g)
	}
	log.Printf("audit: upload grants revoked workspace=%s principal=%s count=%d by=%s", id.Workspace, req.Principal, len(revoked), id.Subject)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": len(revoked), "items": items})
}

// writeGrantError maps uploadgrant errors onto HTTP statuses.
func writeGrantError(w http.ResponseWriter, err error) {
	var ve *uploadgrant.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: ve.Message, Field: ve.Field})
	case errors.Is(err, uploadgrant.ErrNotFound):
		auth.WriteError(w, http.StatusNotFound, "upload grant not found")
	default:
		log.Printf("upload grant error: %v", err)
		auth.WriteError(w, http.StatusBadGateway, err.Error())
	}
}

// ---- helpers ---------------------------------------------------------------

func identity(r *http.Request) auth.Identity {
	id, _ := auth.FromContext(r.Context()) // present: every route runs behind auth.Middleware
	return id
}

// decodeBody strictly decodes a bounded JSON request body, so a typo'd field is an error
// instead of being silently ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			auth.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		auth.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

// writeRegistryError maps registry errors onto HTTP statuses.
func writeRegistryError(w http.ResponseWriter, err error) {
	var ve *registry.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: ve.Message, Field: ve.Field})
	case errors.Is(err, registry.ErrNotFound):
		auth.WriteError(w, http.StatusNotFound, "backend not found")
	case errors.Is(err, registry.ErrExists):
		auth.WriteError(w, http.StatusConflict, "a backend with that id already exists in this workspace")
	default:
		log.Printf("registry error: %v", err)
		auth.WriteError(w, http.StatusBadGateway, err.Error())
	}
}

// writeBackendError maps errors from a storage backend onto HTTP statuses. A failure
// talking to the underlying store is 502 (the store, not this module, is what failed) and
// carries the store's actionable message ("access denied — check the credentials").
func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backend.ErrNotFound):
		auth.WriteError(w, http.StatusNotFound, "object not found")
	case errors.Is(err, backend.ErrInvalidPath):
		auth.WriteError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, backend.ErrConflict):
		auth.WriteError(w, http.StatusConflict, err.Error())
	case errors.Is(err, backend.ErrPreconditionFailed):
		auth.WriteError(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, context.Canceled):
		// The client went away; nothing to report to anyone.
		w.WriteHeader(499)
	default:
		log.Printf("storage backend error: %v", err)
		auth.WriteError(w, http.StatusBadGateway, err.Error())
	}
}
