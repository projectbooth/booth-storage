// Package gcs implements backend.Backend over a Google Cloud Storage bucket (ADR 0013).
package gcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Config is the gcs kind's non-secret registration config.
type Config struct {
	Bucket string `json:"bucket"`
	// Prefix confines this backend to one object-name prefix inside the bucket.
	Prefix string `json:"prefix,omitempty"`
}

// CredServiceAccountJSON is the credential secret key, shared with the registry's
// validation. When absent, Application Default Credentials are used — the right choice
// on GKE with Workload Identity, where no key material should be stored at all.
const CredServiceAccountJSON = "serviceAccountJson"

// Credentials are the gcs kind's secret fields, delivered from a Kubernetes Secret
// (ADR 0020).
type Credentials struct {
	ServiceAccountJSON string
}

var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// Validate checks the config is self-consistent without touching the network.
func (c Config) Validate() error {
	if !bucketRE.MatchString(c.Bucket) {
		return errors.New("bucket must be a valid GCS bucket name (3-222 characters: lowercase letters, digits, '.', '_', '-')")
	}
	if _, err := backend.CleanPrefix(c.Prefix); err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	return nil
}

// ValidateServiceAccountJSON checks a supplied key file is well-formed and is a plain
// service-account key. Other Google credential types are refused on purpose:
// external-account files can instruct the client to fetch arbitrary URLs or execute a
// local command to obtain a token, which is not something a stored, admin-supplied
// credential should be able to make this server do.
func ValidateServiceAccountJSON(raw string) error {
	var probe struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return errors.New("serviceAccountJson is not valid JSON")
	}
	if probe.Type != "service_account" {
		return fmt.Errorf("serviceAccountJson must be a service_account key file (got type %q)", probe.Type)
	}
	if probe.ClientEmail == "" || probe.PrivateKey == "" {
		return errors.New("serviceAccountJson is missing client_email or private_key")
	}
	return nil
}

// Backend is a backend.Backend over one bucket (optionally confined to a prefix).
type Backend struct {
	bucket *storage.BucketHandle
	prefix string // "" or ends in "/"
}

// New builds a client for cfg. It doesn't contact the service; Check does.
func New(cfg Config, creds Credentials) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var opts []option.ClientOption
	if creds.ServiceAccountJSON != "" {
		if err := ValidateServiceAccountJSON(creds.ServiceAccountJSON); err != nil {
			return nil, err
		}
		opts = append(opts, option.WithCredentialsJSON([]byte(creds.ServiceAccountJSON)))
	}
	// The client outlives any single request, so it must not be tied to one's context.
	client, err := storage.NewClient(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating gcs client (with no serviceAccountJson this uses Application Default Credentials): %w", err)
	}
	return NewFromClient(client, cfg)
}

// NewFromClient wraps an existing client — how tests point the backend at an emulator.
func NewFromClient(client *storage.Client, cfg Config) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	prefix, _ := backend.CleanPrefix(cfg.Prefix)
	return &Backend{bucket: client.Bucket(cfg.Bucket), prefix: prefix}, nil
}

func (b *Backend) Check(ctx context.Context) error {
	// Listing one object proves the bucket exists and the credential can list — the
	// permission browsing needs. Bucket.Attrs would need storage.buckets.get, which
	// object-only roles (Storage Object Viewer/Admin) don't include.
	it := b.bucket.Objects(ctx, &storage.Query{Prefix: b.prefix, Delimiter: "/"})
	_, err := iterator.NewPager(it, 1, "").NextPage(&[]*storage.ObjectAttrs{})
	return describe(err)
}

func (b *Backend) List(ctx context.Context, opts backend.ListOptions) (backend.ListResult, error) {
	prefix, err := backend.CleanPrefix(opts.Prefix)
	if err != nil {
		return backend.ListResult{}, err
	}
	q := &storage.Query{Prefix: b.prefix + prefix}
	if !opts.Recursive {
		q.Delimiter = "/"
	}

	var attrs []*storage.ObjectAttrs
	next, err := iterator.NewPager(b.bucket.Objects(ctx, q), opts.EffectiveLimit(), opts.Cursor).NextPage(&attrs)
	if err != nil {
		return backend.ListResult{}, describe(err)
	}

	out := backend.ListResult{Entries: make([]backend.ObjectInfo, 0, len(attrs)), NextCursor: next}
	for _, a := range attrs {
		if a.Prefix != "" { // a delimiter-collapsed "directory"
			out.Entries = append(out.Entries, backend.ObjectInfo{Path: strings.TrimPrefix(a.Prefix, b.prefix), IsDir: true})
			continue
		}
		if strings.HasSuffix(a.Name, "/") && !(opts.Recursive && opts.IncludeFolderMarkers) {
			continue // folder-marker objects aren't objects
		}
		out.Entries = append(out.Entries, infoFor(strings.TrimPrefix(a.Name, b.prefix), a))
	}
	return out, nil
}

func (b *Backend) Read(ctx context.Context, p string) (io.ReadCloser, backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return nil, backend.ObjectInfo{}, err
	}
	r, err := b.bucket.Object(b.prefix + clean).NewReader(ctx)
	if err != nil {
		return nil, backend.ObjectInfo{}, describe(err)
	}
	return r, backend.ObjectInfo{
		Path:        clean,
		Size:        r.Attrs.Size,
		ModTime:     r.Attrs.LastModified.UTC(),
		ContentType: r.Attrs.ContentType,
	}, nil
}

func (b *Backend) Write(ctx context.Context, p string, r io.Reader, opts backend.WriteOptions) (backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return backend.ObjectInfo{}, err
	}

	// Cancelling the writer's context is the only correct way to abort an upload.
	// Calling Close() after a failed copy is a trap: it hands the uploader a clean EOF,
	// which it can't tell apart from a finished stream, so a truncated object gets
	// committed — reproduced against fake-gcs-server (a 64 KiB fragment of a failed
	// upload became visible). On cancel the client library aborts the in-flight upload
	// itself (Writer.monitorCancel), so there is nothing to Close and nothing leaks.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := b.bucket.Object(b.prefix + clean).NewWriter(wctx)
	w.ContentType = opts.ContentType
	if _, err := io.Copy(w, r); err != nil {
		cancel()
		return backend.ObjectInfo{}, describe(err)
	}
	if err := w.Close(); err != nil {
		return backend.ObjectInfo{}, describe(err)
	}
	return infoFor(clean, w.Attrs()), nil
}

// ---- folder and mutation operations (ADR 0038) ----

var _ backend.FlatStore = (*Backend)(nil)

func (b *Backend) Mkdir(ctx context.Context, path string) error {
	folder, err := backend.CleanFolder(path)
	if err != nil {
		return err
	}
	// The conventional zero-byte "folder/" placeholder object. Closing a writer that was
	// never written to creates an empty object.
	return describe(b.bucket.Object(b.prefix + folder).NewWriter(ctx).Close())
}

func (b *Backend) DeleteObject(ctx context.Context, path string) error {
	clean, err := backend.CleanPath(path)
	if err != nil {
		return err
	}
	return describe(b.bucket.Object(b.prefix + clean).Delete(ctx)) // ErrObjectNotExist -> ErrNotFound
}

func (b *Backend) DeleteFolder(ctx context.Context, path string) (int, error) {
	folder, err := backend.CleanFolder(path)
	if err != nil {
		return 0, err
	}
	return backend.DeleteFolderFlat(ctx, b, folder)
}

func (b *Backend) MoveObject(ctx context.Context, from, to string) error {
	f, err := backend.CleanPath(from)
	if err != nil {
		return err
	}
	t, err := backend.CleanPath(to)
	if err != nil {
		return err
	}
	return backend.MoveObjectFlat(ctx, b, f, t)
}

func (b *Backend) MoveFolder(ctx context.Context, from, to string) error {
	f, err := backend.CleanFolder(from)
	if err != nil {
		return err
	}
	t, err := backend.CleanFolder(to)
	if err != nil {
		return err
	}
	return backend.MoveFolderFlat(ctx, b, f, t)
}

func (b *Backend) CopyKey(ctx context.Context, from, to string) error {
	// Server-side rewrite: handles any size, and the bytes never pass through this process.
	_, err := b.bucket.Object(b.prefix + to).CopierFrom(b.bucket.Object(b.prefix + from)).Run(ctx)
	return describe(err)
}

func (b *Backend) DeleteKey(ctx context.Context, key string) error {
	err := b.bucket.Object(b.prefix + key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return describe(err)
}

func (b *Backend) KeyExists(ctx context.Context, key string) (bool, error) {
	_, err := b.bucket.Object(b.prefix + key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return err == nil, describe(err)
}

func infoFor(p string, a *storage.ObjectAttrs) backend.ObjectInfo {
	return backend.ObjectInfo{Path: p, Size: a.Size, ModTime: a.Updated.UTC(), ContentType: a.ContentType}
}

// describe maps service errors onto the package-independent errors callers branch on,
// with messages an admin can act on.
func describe(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return backend.ErrNotFound
	case errors.Is(err, storage.ErrBucketNotExist):
		return errors.New("bucket does not exist")
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.New("access denied by Google Cloud Storage — check the service account and its bucket permissions")
		case http.StatusNotFound:
			return errors.New("bucket does not exist")
		}
	}
	return err
}
