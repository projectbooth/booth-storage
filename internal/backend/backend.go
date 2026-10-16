// Package backend defines the one generic read/write/list surface every storage kind
// implements (ADR 0013: S3-compatible, filesystem, Azure Blob, GCS), so the HTTP layer
// and every downstream module can address a registered backend without knowing what's
// behind it (ADR 0035: keyed by backend ID, never "the" backend).
//
// Read, write and list, plus the folder operations the file browser needs (ADR 0037/0038):
// create folder, delete, and rename/move. Every kind normalizes to the same folder model —
// real directories on the filesystem, synthesized folders from "/"-delimited keys on the
// flat-key object stores — so callers never see which kind they are talking to.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Kind identifies which of the four supported storage technologies a backend is.
type Kind string

const (
	KindS3         Kind = "s3"         // S3-compatible object storage (AWS S3, MinIO, Ceph, ...)
	KindFilesystem Kind = "filesystem" // a directory on the server's own filesystem
	KindAzure      Kind = "azure"      // Azure Blob Storage
	KindGCS        Kind = "gcs"        // Google Cloud Storage
)

// Kinds lists every supported kind, in the order the UI presents them.
var Kinds = []Kind{KindS3, KindFilesystem, KindAzure, KindGCS}

func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

var (
	// ErrNotFound means the requested object doesn't exist.
	ErrNotFound = errors.New("object not found")
	// ErrInvalidPath means a caller-supplied path violated the rules in CleanPath.
	ErrInvalidPath = errors.New("invalid path")
	// ErrConflict means the operation would clobber something that already exists (a move
	// onto an existing object or folder, or a folder where a file is).
	ErrConflict = errors.New("already exists")
	// ErrPreconditionFailed means a WriteOptions.CreateOnly write found an object already
	// at the path (ADR 0117 point 8/8a). Distinct from ErrConflict (move/folder clobber):
	// a caller branches on this one to answer 412, not 409.
	ErrPreconditionFailed = errors.New("object already exists (create-only write)")
	// ErrInvalidRange means a ReadRange's range does not lie within the object's current
	// size. The HTTP layer resolves and clamps a Range request against Stat's reported
	// size before ever calling ReadRange, so this is a backend's own backstop — only
	// reachable if the object's size changed in the gap between the two calls.
	ErrInvalidRange = errors.New("requested range not satisfiable")
)

// ObjectInfo describes one entry returned by List/Read/Write. Path is always relative
// to the backend's own root (any configured key prefix is stripped) and uses forward
// slashes on every kind, including the filesystem kind on Windows hosts.
type ObjectInfo struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime,omitzero"`
	ContentType string    `json:"contentType,omitempty"`
	// ETag is the storage kind's own entity tag for the object's current content,
	// already formatted as an HTTP ETag header value (double-quoted; "W/"-prefixed when
	// a kind can only approximate one from metadata rather than real content — see
	// QuoteETag and each kind's own Read/Stat). Empty when a kind has no cheap way to
	// report one without an extra round trip (gcs's Read: see its own comment).
	ETag string `json:"etag,omitempty"`
	// IsDir marks a "directory" entry from a non-recursive listing: a key prefix on
	// object stores, a real directory on the filesystem. Path ends in "/" when set.
	IsDir bool `json:"isDir,omitempty"`
}

// QuoteETag formats a storage kind's native ETag token as an HTTP ETag header value: a
// quoted validator per RFC 9110 section 8.8.1. Idempotent — a token that already carries
// its own quotes (as Azure's does) comes back unchanged. Empty input returns "".
func QuoteETag(raw string) string {
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return ""
	}
	return `"` + raw + `"`
}

// ByteRange is one inclusive byte range of an object, for ReadRange's ranged GET (HTTP
// 206). End is always >= Start. Callers resolve and clamp a range against the object's
// real size (via Stat) before calling ReadRange — a backend never has to guess.
type ByteRange struct {
	Start int64
	End   int64 // inclusive
}

// ListOptions controls a List call.
type ListOptions struct {
	// Prefix restricts the listing to one directory-like prefix: "" for the root, or
	// "a/b" / "a/b/" for a nested one (both normalize to "a/b/"). It is a directory
	// boundary, not a raw string prefix — "a/b" never matches "a/bc/x".
	Prefix string
	// Recursive lists every object under Prefix and returns no directory entries.
	// When false, only Prefix's direct children are returned, with sub-prefixes
	// reported as IsDir entries.
	Recursive bool
	// Limit caps how many entries one call returns; <=0 means DefaultListLimit.
	Limit int
	// Cursor is the opaque NextCursor from a previous call, to continue a listing.
	Cursor string
	// IncludeFolderMarkers makes a recursive listing also return the zero-byte placeholder
	// objects ("a/b/") that stand for empty folders on flat-key stores. Internal: the
	// folder delete/move helpers need them to move or remove empty folders; the HTTP API
	// never sets it, so callers normally never see markers.
	IncludeFolderMarkers bool
}

const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
)

// EffectiveLimit clamps Limit into [1, MaxListLimit], defaulting when unset.
func (o ListOptions) EffectiveLimit() int {
	switch {
	case o.Limit <= 0:
		return DefaultListLimit
	case o.Limit > MaxListLimit:
		return MaxListLimit
	default:
		return o.Limit
	}
}

// ListResult is one page of a listing. NextCursor is empty when there are no more
// entries. Cursors are opaque and only meaningful to the backend that issued them.
type ListResult struct {
	Entries    []ObjectInfo `json:"entries"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// WriteOptions carries optional metadata for a Write.
type WriteOptions struct {
	ContentType string
	// CreateOnly requires the write to fail with ErrPreconditionFailed rather than
	// replace an existing object at path (ADR 0117 point 8: "If-None-Match: *", 412 if
	// the object exists). Every kind enforces this atomically, using that kind's own
	// native precondition (filesystem: a hard link into place; S3, Azure, GCS: their own
	// conditional-write precondition) — never by checking existence first and writing
	// second, which would leave a race between the two.
	CreateOnly bool
}

// Backend is the generic storage surface. Implementations must be safe for concurrent
// use by multiple goroutines.
type Backend interface {
	// List returns one page of entries under opts.Prefix. Listing a prefix that has
	// nothing under it is not an error — it returns an empty page.
	List(ctx context.Context, opts ListOptions) (ListResult, error)

	// Read opens the object at path. The caller must Close the returned reader. A
	// missing object yields ErrNotFound.
	Read(ctx context.Context, path string) (io.ReadCloser, ObjectInfo, error)

	// ReadRange opens one inclusive byte range of the object at path, for a 206 Partial
	// Content response (Spark/Hadoop S3A-style clients rely on granular ranged reads for
	// columnar predicate pushdown). The caller must Close the returned reader. A missing
	// object yields ErrNotFound; a range outside the object's current size yields
	// ErrInvalidRange (ordinarily never reached — see ErrInvalidRange's own comment).
	ReadRange(ctx context.Context, path string, r ByteRange) (io.ReadCloser, ObjectInfo, error)

	// Stat returns one object's metadata without opening its content — the no-body
	// counterpart to Read, for a HEAD request and for resolving a ranged GET's
	// Content-Range against the object's real size before touching the body. A missing
	// object yields ErrNotFound.
	Stat(ctx context.Context, path string) (ObjectInfo, error)

	// Write stores r's bytes at path, replacing any existing object (unless
	// opts.CreateOnly, see WriteOptions), and returns the stored object's info. A failed
	// or interrupted write must not leave a partial object visible under path.
	Write(ctx context.Context, path string, r io.Reader, opts WriteOptions) (ObjectInfo, error)

	// Mkdir creates an empty folder at path. On the filesystem that is a real directory; on
	// flat-key stores it is the conventional zero-byte "path/" placeholder object (ADR 0038).
	// Creating a folder that already exists is not an error. ErrConflict if a file is in the way.
	Mkdir(ctx context.Context, path string) error

	// DeleteObject removes one object. ErrNotFound if there is none. Directories and
	// folders are not objects: use DeleteFolder.
	DeleteObject(ctx context.Context, path string) error

	// DeleteFolder removes a folder and everything under it, returning how many objects
	// were removed. The backend root cannot be deleted. ErrNotFound if the folder holds
	// nothing and doesn't exist.
	DeleteFolder(ctx context.Context, path string) (int, error)

	// MoveObject renames/moves one object. The destination must not exist (ErrConflict):
	// a rename must never silently destroy data. ErrNotFound if the source is missing.
	MoveObject(ctx context.Context, from, to string) error

	// MoveFolder renames/moves a folder and everything under it, with the same
	// no-clobber rule. Moving a folder into itself is ErrInvalidPath. Atomic on the
	// filesystem; on flat-key stores it is copy-then-delete per object and therefore not
	// atomic (a failure leaves the source intact and cleans up the partial copy).
	MoveFolder(ctx context.Context, from, to string) error

	// Check verifies the backend is reachable and its credentials work, without
	// changing anything. Used by the admin view's "test connection" action.
	Check(ctx context.Context) error
}

const maxPathLen = 1024

// CleanPath validates and normalizes an object path supplied by a caller. Every kind
// applies it before touching storage, so path rules are identical across kinds and a
// traversal attempt is rejected here rather than depending on each backend's own
// handling.
//
// Rules: relative, forward-slash separated, no empty/"."/".." segments, no NUL or
// backslash, at most 1024 bytes. A leading "/" is rejected rather than stripped: if
// "/a" and "a" both worked, one object would have two addresses, and a client that
// accidentally sent a doubled slash would silently write somewhere it didn't intend.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if err := checkSegments(p); err != nil {
		return "", err
	}
	if strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("%w: object path must not end in '/'", ErrInvalidPath)
	}
	return p, nil
}

// CleanPrefix is CleanPath's counterpart for listing prefixes: "" is allowed (the
// root), and a single trailing "/" is normalized on — the result is "" or ends in "/".
// A leading "/" is rejected for the same reason as in CleanPath.
func CleanPrefix(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	if err := checkSegments(p); err != nil {
		return "", err
	}
	return p + "/", nil
}

// CleanFolder validates a folder path for Mkdir/DeleteFolder/MoveFolder and returns it in
// the "a/b/" prefix form. Unlike CleanPrefix the root ("") is rejected: none of those
// operations may target the backend root itself.
func CleanFolder(p string) (string, error) {
	prefix, err := CleanPrefix(p)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return "", fmt.Errorf("%w: a folder path is required", ErrInvalidPath)
	}
	return prefix, nil
}

func checkSegments(p string) error {
	if len(p) > maxPathLen {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, maxPathLen)
	}
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("%w: contains NUL or backslash", ErrInvalidPath)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: empty path segment", ErrInvalidPath)
		case ".", "..":
			return fmt.Errorf("%w: %q segment not allowed", ErrInvalidPath, seg)
		}
	}
	return nil
}
