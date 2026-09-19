// Package filesystem implements backend.Backend over a directory on the server's own
// filesystem (ADR 0013's "local/server filesystem" kind).
//
// Every operation goes through os.Root, which confines path resolution to the
// registered root directory at the OS-API level — `..` segments and symlinks pointing
// outside the root are rejected by the standard library, not by string checks here.
// backend.CleanPath still runs first so path rules are identical to the other kinds,
// but it is defense in depth, not the security boundary. Which directories may be
// registered at all is a separate, earlier policy (registry.FilesystemPolicy).
package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Config is the filesystem kind's registration config.
type Config struct {
	// RootPath is the absolute directory this backend is confined to.
	RootPath string `json:"rootPath"`
}

// tempPrefix marks in-flight write files. They are hidden from listings so a
// concurrent List never observes a half-written object.
const tempPrefix = ".booth-tmp-"

// Backend is a backend.Backend over one directory tree.
type Backend struct {
	rootPath string
}

// New returns a backend confined to cfg.RootPath. It does not touch the disk; Check
// reports whether the directory is actually usable.
func New(cfg Config) (*Backend, error) {
	if cfg.RootPath == "" {
		return nil, errors.New("rootPath is required")
	}
	if !filepath.IsAbs(cfg.RootPath) {
		return nil, fmt.Errorf("rootPath %q must be absolute", cfg.RootPath)
	}
	return &Backend{rootPath: filepath.Clean(cfg.RootPath)}, nil
}

// openRoot opens the confining root fresh for each operation rather than holding one
// open: if the directory is replaced or remounted, a cached handle would keep pointing
// at the old inode.
func (b *Backend) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(b.rootPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("root directory %q does not exist", b.rootPath)
		}
		return nil, fmt.Errorf("opening root directory: %w", err)
	}
	return root, nil
}

func (b *Backend) Check(ctx context.Context) error {
	root, err := b.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	f, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("root directory is not readable: %w", err)
	}
	defer f.Close()
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("root directory is not readable: %w", err)
	}
	return nil
}

func (b *Backend) Read(ctx context.Context, p string) (io.ReadCloser, backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return nil, backend.ObjectInfo{}, err
	}
	root, err := b.openRoot()
	if err != nil {
		return nil, backend.ObjectInfo{}, err
	}
	defer root.Close()

	f, err := root.Open(filepath.FromSlash(clean))
	if err != nil {
		return nil, backend.ObjectInfo{}, mapErr(err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, backend.ObjectInfo{}, mapErr(err)
	}
	if !st.Mode().IsRegular() {
		// A directory is not an object; treating it as missing matches how the
		// object-store kinds behave (a bare prefix has no object behind it).
		f.Close()
		return nil, backend.ObjectInfo{}, backend.ErrNotFound
	}
	// The open file handle stays valid after root.Close() — the root only confines
	// path resolution at open time.
	return f, infoFor(clean, st), nil
}

func (b *Backend) Write(ctx context.Context, p string, r io.Reader, opts backend.WriteOptions) (backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return backend.ObjectInfo{}, err
	}
	root, err := b.openRoot()
	if err != nil {
		return backend.ObjectInfo{}, err
	}
	defer root.Close()

	dir := path.Dir(clean)
	if dir != "." {
		if err := mkdirAll(root, dir); err != nil {
			return backend.ObjectInfo{}, mapErr(err)
		}
	}

	// Write to a sibling temp file and rename into place, so readers only ever see the
	// old object or the complete new one, and a failed/interrupted write leaves nothing.
	tmpName, err := tempName(dir)
	if err != nil {
		return backend.ObjectInfo{}, err
	}
	tmp, err := root.OpenFile(filepath.FromSlash(tmpName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return backend.ObjectInfo{}, mapErr(err)
	}
	cleanupTmp := func() {
		tmp.Close()
		_ = root.Remove(filepath.FromSlash(tmpName))
	}

	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		cleanupTmp()
		return backend.ObjectInfo{}, fmt.Errorf("writing object: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanupTmp()
		return backend.ObjectInfo{}, fmt.Errorf("syncing object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = root.Remove(filepath.FromSlash(tmpName))
		return backend.ObjectInfo{}, fmt.Errorf("closing object: %w", err)
	}

	if err := root.Rename(filepath.FromSlash(tmpName), filepath.FromSlash(clean)); err != nil {
		_ = root.Remove(filepath.FromSlash(tmpName))
		return backend.ObjectInfo{}, fmt.Errorf("%w: cannot store object at %q (a directory may already exist there): %v", backend.ErrInvalidPath, clean, err)
	}

	st, err := root.Stat(filepath.FromSlash(clean))
	if err != nil {
		return backend.ObjectInfo{}, mapErr(err)
	}
	return infoFor(clean, st), nil
}

func (b *Backend) List(ctx context.Context, opts backend.ListOptions) (backend.ListResult, error) {
	prefix, err := backend.CleanPrefix(opts.Prefix)
	if err != nil {
		return backend.ListResult{}, err
	}
	root, err := b.openRoot()
	if err != nil {
		return backend.ListResult{}, err
	}
	defer root.Close()

	limit := opts.EffectiveLimit()
	startDir := strings.TrimSuffix(prefix, "/")
	if startDir == "" {
		startDir = "."
	}

	// Collect limit+1 entries: the extra one only tells us whether another page exists.
	var entries []backend.ObjectInfo
	full := func() bool { return len(entries) > limit }

	if opts.Recursive {
		err = walk(ctx, root, startDir, opts.Cursor, &entries, full)
	} else {
		err = listDir(root, startDir, prefix, opts.Cursor, &entries, full)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return backend.ListResult{}, nil // nothing under this prefix — not an error
		}
		return backend.ListResult{}, mapErr(err)
	}

	res := backend.ListResult{Entries: entries}
	if len(entries) > limit {
		res.Entries = entries[:limit]
		res.NextCursor = entries[limit-1].Path
	}
	if res.Entries == nil {
		res.Entries = []backend.ObjectInfo{}
	}
	return res, nil
}

// ---- folder and mutation operations (ADR 0038) ----

func (b *Backend) Mkdir(ctx context.Context, p string) error {
	folder, err := backend.CleanFolder(p)
	if err != nil {
		return err
	}
	dir := strings.TrimSuffix(folder, "/")
	root, err := b.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	if err := mkdirAll(root, dir); err != nil {
		return mapErr(err)
	}
	// mkdirAll ignores "already exists", which is also what a *file* in the way reports.
	if st, err := root.Stat(filepath.FromSlash(dir)); err != nil || !st.IsDir() {
		return fmt.Errorf("%w: %q exists and is not a folder", backend.ErrConflict, dir)
	}
	return nil
}

func (b *Backend) DeleteObject(ctx context.Context, p string) error {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return err
	}
	root, err := b.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	st, err := root.Lstat(filepath.FromSlash(clean))
	if err != nil {
		return mapErr(err)
	}
	if !st.Mode().IsRegular() {
		return backend.ErrNotFound // a directory is not an object
	}
	return mapErr(root.Remove(filepath.FromSlash(clean)))
}

func (b *Backend) DeleteFolder(ctx context.Context, p string) (int, error) {
	folder, err := backend.CleanFolder(p)
	if err != nil {
		return 0, err
	}
	dir := strings.TrimSuffix(folder, "/")
	root, err := b.openRoot()
	if err != nil {
		return 0, err
	}
	defer root.Close()

	st, err := root.Lstat(filepath.FromSlash(dir))
	if err != nil {
		return 0, mapErr(err)
	}
	if !st.IsDir() {
		return 0, backend.ErrNotFound
	}
	files := 0
	_ = fs.WalkDir(root.FS(), dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files++
		}
		return nil
	})
	if err := root.RemoveAll(filepath.FromSlash(dir)); err != nil {
		return 0, mapErr(err)
	}
	return files, nil
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
	return b.move(f, t, false)
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
	if strings.HasPrefix(t, f) {
		return fmt.Errorf("%w: cannot move a folder into itself", backend.ErrInvalidPath)
	}
	return b.move(strings.TrimSuffix(f, "/"), strings.TrimSuffix(t, "/"), true)
}

// move renames src to dst with the no-clobber rule. On a real filesystem a rename is
// atomic, so there is no partial state to clean up. (Checking the destination first and
// renaming second is not race-free; a concurrent creator could win in between. That is an
// accepted v0 limitation: a losing rename fails or replaces an empty directory, it cannot
// escape the root.)
func (b *Backend) move(src, dst string, wantDir bool) error {
	root, err := b.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	st, err := root.Lstat(filepath.FromSlash(src))
	if err != nil {
		return mapErr(err)
	}
	if st.IsDir() != wantDir || (!wantDir && !st.Mode().IsRegular()) {
		return backend.ErrNotFound
	}
	if src == dst {
		return nil
	}
	if _, err := root.Lstat(filepath.FromSlash(dst)); err == nil {
		return fmt.Errorf("%w: %q already exists", backend.ErrConflict, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return mapErr(err)
	}
	if parent := path.Dir(dst); parent != "." {
		if err := mkdirAll(root, parent); err != nil {
			return mapErr(err)
		}
	}
	return mapErr(root.Rename(filepath.FromSlash(src), filepath.FromSlash(dst)))
}

// listDir lists one directory level (non-recursive mode).
func listDir(root *os.Root, dir, prefix, cursor string, out *[]backend.ObjectInfo, full func() bool) error {
	f, err := root.Open(filepath.FromSlash(dir))
	if err != nil {
		return err
	}
	defer f.Close()

	dirEntries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })

	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, tempPrefix) {
			continue
		}
		entryPath := prefix + name
		switch {
		case de.IsDir():
			entryPath += "/"
			if !afterCursor(entryPath, cursor) {
				continue
			}
			*out = append(*out, backend.ObjectInfo{Path: entryPath, IsDir: true})
		case de.Type().IsRegular():
			if !afterCursor(entryPath, cursor) {
				continue
			}
			info, err := de.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // removed while listing
				}
				return err
			}
			*out = append(*out, infoFor(entryPath, info))
		default:
			continue // symlinks, devices, sockets: never surfaced as objects
		}
		if full() {
			return nil
		}
	}
	return nil
}

// walk lists every regular file under startDir in tree pre-order, name-sorted per
// directory (fs.WalkDir's contract).
func walk(ctx context.Context, root *os.Root, startDir, cursor string, out *[]backend.ObjectInfo, full func() bool) error {
	errDone := errors.New("page full")

	err := fs.WalkDir(root.FS(), startDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == startDir {
				return err
			}
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if strings.HasPrefix(d.Name(), tempPrefix) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Resume optimization: skip a whole subtree that sorts entirely before the
			// cursor, unless the cursor lives inside it.
			if cursor != "" && p != "." && comparePaths(p, cursor) < 0 && !strings.HasPrefix(cursor, p+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !afterCursor(p, cursor) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, fs.ErrNotExist) {
				return nil
			}
			return ierr
		}
		*out = append(*out, infoFor(p, info))
		if full() {
			return errDone
		}
		return nil
	})
	if errors.Is(err, errDone) {
		return nil
	}
	return err
}

// afterCursor reports whether entryPath sorts strictly after the cursor. An empty
// cursor means "from the start".
func afterCursor(entryPath, cursor string) bool {
	return cursor == "" || comparePaths(entryPath, cursor) > 0
}

// comparePaths orders two slash-separated paths component by component, which is the
// order both fs.WalkDir and a name-sorted directory listing produce — plain string
// comparison would disagree ("a-b" < "a/x" as strings, but tree order visits "a/x"
// first) and break cursor resumption. A trailing slash (directory marker) is ignored.
func comparePaths(a, b string) int {
	as := strings.Split(strings.TrimSuffix(a, "/"), "/")
	bs := strings.Split(strings.TrimSuffix(b, "/"), "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := strings.Compare(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

func mkdirAll(root *os.Root, dir string) error {
	var built string
	for _, seg := range strings.Split(dir, "/") {
		built = path.Join(built, seg)
		if err := root.Mkdir(filepath.FromSlash(built), 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

func tempName(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := tempPrefix + hex.EncodeToString(b[:])
	if dir == "." {
		return name, nil
	}
	return dir + "/" + name, nil
}

func infoFor(p string, st fs.FileInfo) backend.ObjectInfo {
	return backend.ObjectInfo{
		Path:        p,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		ContentType: mime.TypeByExtension(path.Ext(p)),
	}
}

// mapErr converts filesystem errors into the package-independent errors callers
// branch on.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return backend.ErrNotFound
	case strings.Contains(err.Error(), "path escapes from parent"):
		// os.Root refused to follow a symlink (or ..) out of the root.
		return fmt.Errorf("%w: path resolves outside the backend root", backend.ErrInvalidPath)
	default:
		return err
	}
}

// ctxReader aborts an in-progress copy when the request's context is cancelled, so an
// abandoned upload doesn't keep writing to disk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
