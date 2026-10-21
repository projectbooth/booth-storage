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
	"github.com/projectbooth/booth-storage/internal/eventbus"
	"github.com/projectbooth/booth-storage/internal/registry"
	"github.com/projectbooth/booth-storage/internal/seaweedfslease"
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
	// SeaweedFSLeases persists every SeaweedFS IAM lease the seaweedfs-iam credential
	// path mints (ADR 0118 ruling 3) — see internal/credentialbroker.Deps.Leases.
	SeaweedFSLeases seaweedfslease.Store
	// CredentialBrokerCredential is this module's copy of the shared secret core presents
	// to the ADR 0080 credential-broker provider route (POST /internal/credentials, see
	// internal/credentialbroker) — the route is always mounted, but empty means every call
	// is refused: there is no meaningful "unconfigured, so allow" state for it.
	CredentialBrokerCredential string
	// Events publishes storage.object.created after a successful object write (ADR
	// 0123). Required (never nil): cmd/storage/main.go supplies eventbus.NewNoopPublisher
	// when no bus is configured, so this field is never the zero value in practice.
	Events eventbus.Publisher
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
		Leases:     deps.SeaweedFSLeases,
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
			r.Head("/api/backends/{id}/objects/*", s.handleStatObject)
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
			// update route — narrowing a grant is delete-and-recreate. revoke-principal
			// is deliberately NOT here — see below.
			r.Get("/upload-grants", s.handleAdminListUploadGrants)
			r.Post("/upload-grants", s.handleAdminCreateUploadGrant)
			r.Delete("/upload-grants/{id}", s.handleAdminDeleteUploadGrant)
		})

		// revoke-principal (ADR 0117 item 1a) sits outside the owner-only /api/admin
		// group on purpose: it also accepts booth-streamlit's revoke-only
		// grants-service identity (auth.Identity.IsGrantsService), which IsAdmin would
		// refuse. handleRevokeUploadGrantsByPrincipal does its own inline check for
		// exactly those two identities and nothing else.
		r.Post("/api/admin/upload-grants/revoke-principal", s.handleRevokeUploadGrantsByPrincipal)
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

// handleReadObject is GET .../objects/*. With no Range header it serves the whole
// object (200), same as always. With one it serves exactly that byte range (206) once
// Stat has confirmed it lies within the object's current size — a plain client never
// has to know ranging exists, and a Range-aware one (Spark/Hadoop S3A, a browser video
// scrubber) gets the granular reads it needs without downloading the whole object.
func (s *server) handleReadObject(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	path := objectPath(r, chi.URLParam(r, "id"))

	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		s.serveWholeObject(w, r, b, path)
		return
	}

	// The real size has to be known before a range can be validated or clamped, and
	// before the Content-Range header's total can be written — Stat first, every time.
	info, err := b.Stat(r.Context(), path)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	rg, parsed, unsatisfiable := parseRange(rangeHeader, info.Size)
	switch {
	case unsatisfiable:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
		auth.WriteError(w, http.StatusRequestedRangeNotSatisfiable, "the requested range lies outside the object's size")
	case !parsed:
		// Unparseable, or more than one range: RFC 9110 section 14.2 lets a server
		// ignore a Range header it doesn't support and answer the whole object instead
		// of rejecting the request.
		s.serveWholeObject(w, r, b, path)
	default:
		s.serveObjectRange(w, r, b, path, rg, info.Size)
	}
}

func (s *server) serveWholeObject(w http.ResponseWriter, r *http.Request, b backend.Backend, path string) {
	rc, info, err := b.Read(r.Context(), path)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer rc.Close()

	setObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	// Headers are sent; a mid-stream failure can only truncate, which the client detects
	// via Content-Length. Nothing more useful to do than log it.
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("streaming object %q: %v", path, err)
	}
}

func (s *server) serveObjectRange(w http.ResponseWriter, r *http.Request, b backend.Backend, path string, rg backend.ByteRange, totalSize int64) {
	rc, info, err := b.ReadRange(r.Context(), path, rg)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer rc.Close()

	setObjectHeaders(w, info)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rg.Start, rg.End, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(rg.End-rg.Start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("streaming object %q (range %d-%d): %v", path, rg.Start, rg.End, err)
	}
}

// handleStatObject is HEAD .../objects/*: the same metadata and headers as a GET,
// without the body.
func (s *server) handleStatObject(w http.ResponseWriter, r *http.Request) {
	b, ok := s.openBackend(w, r)
	if !ok {
		return
	}
	path := objectPath(r, chi.URLParam(r, "id"))
	info, err := b.Stat(r.Context(), path)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	setObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

// setObjectHeaders sets the headers shared by a whole-object GET, a ranged GET and a
// HEAD. Stored objects are user-supplied content served from this module's origin, so
// this also neutralizes anything a browser might try to execute or sniff: force a
// download, forbid MIME sniffing, and sandbox any document that does get rendered.
func setObjectHeaders(w http.ResponseWriter, info backend.ObjectInfo) {
	h := w.Header()
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": lastSegment(info.Path)}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Accept-Ranges", "bytes")
	if info.ETag != "" {
		h.Set("ETag", info.ETag)
	}
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
}

// parseRange parses a single-range "Range: bytes=..." request header (RFC 9110 section
// 14.1.2) against an object of the given size. parsed is false when the header is
// absent, malformed, or names more than one range (multi-range isn't supported) — the
// caller falls back to serving the whole object, per section 14.2. unsatisfiable is true
// when the (successfully parsed) range's start lies at or past size, which the caller
// must answer 416 for rather than serve anything.
func parseRange(header string, size int64) (r backend.ByteRange, parsed, unsatisfiable bool) {
	const prefix = "bytes="
	spec, ok := strings.CutPrefix(header, prefix)
	if !ok || strings.Contains(spec, ",") {
		return backend.ByteRange{}, false, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return backend.ByteRange{}, false, false
	}
	startStr, endStr := spec[:dash], spec[dash+1:]

	if startStr == "" {
		// A suffix range, "bytes=-N": the last N bytes of the object.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return backend.ByteRange{}, false, false
		}
		if size == 0 {
			return backend.ByteRange{}, true, true
		}
		start := size - n
		if start < 0 {
			start = 0
		}
		return backend.ByteRange{Start: start, End: size - 1}, true, false
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return backend.ByteRange{}, false, false
	}
	if start >= size {
		return backend.ByteRange{}, true, true
	}
	end := size - 1
	if endStr != "" {
		e, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || e < start {
			return backend.ByteRange{}, false, false
		}
		if e < end {
			end = e
		}
	}
	return backend.ByteRange{Start: start, End: end}, true, false
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
	s.publishObjectCreated(id, backendID, clean, info.Size, grant != nil)
	writeJSON(w, http.StatusOK, info)
}

// publishObjectCreated sends storage.object.created (ADR 0123) after a write this
// handler has already committed successfully. Best-effort: bounded by its own timeout,
// detached from the request context so a client that's already moved on can't cancel
// it early, and never surfaced to the caller — the write already succeeded, and ADR
// 0123 point 6 names a slower cron/interval trigger as the safety net for a missed
// event, not this module retrying or queuing one. via distinguishes an app's
// grant-admitted create-only upload from an editor/owner write; this handler has no way
// to further tell a UI click from a scripted API call using the same token (see
// docs/design-event-triggers.md §2, open question 1).
func (s *server) publishObjectCreated(id auth.Identity, backendID, path string, size int64, viaGrant bool) {
	via := "editor"
	if viaGrant {
		via = "app-upload"
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventbus.PublishTimeout)
	defer cancel()
	data := map[string]any{
		"backendId": backendID,
		"path":      path,
		"size":      size,
		"principal": id.Subject,
		"via":       via,
	}
	if err := s.Events.Publish(ctx, id.Workspace, "storage.object.created", data); err != nil {
		log.Printf("eventbus: publishing storage.object.created failed workspace=%s backend=%s path=%q: %v", id.Workspace, backendID, path, err)
	}
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

// handleRevokeUploadGrantsByPrincipal mirrors booth-api's
// POST /api/write-grants/revoke-principal exactly: revokes every live grant the
// principal holds in the workspace, across every backend, in one call. The
// {"revoked", "items"} shape and idempotency (0 the second time) are what let a caller
// fail closed on anything but success.
//
// Two identities may call this, and nothing else may (ADR 0117 item 1/1a):
//   - A workspace owner, same as every other upload-grants admin route, revoking
//     whatever principal it names.
//   - booth-streamlit's own revoke-only grants-service identity
//     (auth.Identity.IsGrantsService), which holds no admin role at all. It can only
//     ever reach this one route (every other route gates on CanRead/CanWrite/IsAdmin,
//     none of which that identity satisfies), and even here it is restricted to
//     principals shaped streamlit:<its own workspace>:<appId> — it cannot revoke
//     anything outside what booth-streamlit itself could have declared. This is how
//     booth-streamlit revokes an app's upload grants on take-ownership without ever
//     holding a person's token (it is reached through core's iframe proxy, never a
//     bearer token of its own).
func (s *server) handleRevokeUploadGrantsByPrincipal(w http.ResponseWriter, r *http.Request) {
	var req revokePrincipalRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := identity(r)
	switch {
	case id.IsAdmin():
		// unrestricted, as today.
	case id.IsGrantsService():
		if err := uploadgrant.ValidatePrincipal(req.Principal, id.Workspace); err != nil {
			auth.WriteError(w, http.StatusForbidden, "the grants-service identity may only revoke streamlit:<its workspace>:<appId> principals")
			return
		}
	default:
		auth.WriteError(w, http.StatusForbidden, "only a workspace owner or booth-streamlit's own revoke-only service identity may revoke upload grants")
		return
	}
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
	case errors.Is(err, backend.ErrInvalidRange):
		auth.WriteError(w, http.StatusRequestedRangeNotSatisfiable, err.Error())
	case errors.Is(err, context.Canceled):
		// The client went away; nothing to report to anyone.
		w.WriteHeader(499)
	default:
		log.Printf("storage backend error: %v", err)
		auth.WriteError(w, http.StatusBadGateway, err.Error())
	}
}
