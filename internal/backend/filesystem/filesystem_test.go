package filesystem

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/backendtest"
)

func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(Config{RootPath: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend { return newBackend(t) })
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("empty rootPath accepted")
	}
	if _, err := New(Config{RootPath: "relative/dir"}); err == nil {
		t.Error("relative rootPath accepted")
	}
}

func TestCheck_MissingRoot(t *testing.T) {
	b, err := New(Config{RootPath: filepath.Join(t.TempDir(), "does-not-exist")})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded for a missing root directory")
	}
	// Writes must not silently create the root itself — only directories inside it.
	if _, err := b.Write(context.Background(), "x", bytes.NewReader([]byte("x")), backend.WriteOptions{}); err == nil {
		t.Fatal("Write created/used a nonexistent root")
	}
}

// A symlink inside the root pointing outside it must not be readable or writable
// through the backend — the whole point of building on os.Root.
func TestSymlinkEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("cannot create symlinks on this host (Windows without privilege?): %v", err)
	}
	b, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := b.Read(context.Background(), "escape/secret.txt"); err == nil {
		t.Fatal("read through an escaping symlink succeeded")
	} else if !errors.Is(err, backend.ErrInvalidPath) {
		t.Errorf("escaping read error = %v, want ErrInvalidPath", err)
	}

	if _, err := b.Write(context.Background(), "escape/planted.txt", bytes.NewReader([]byte("x")), backend.WriteOptions{}); err == nil {
		t.Fatal("write through an escaping symlink succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "planted.txt")); statErr == nil {
		t.Fatal("file was planted outside the backend root")
	}

	// The symlink itself is not surfaced as an object or directory.
	res, err := b.List(context.Background(), backend.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("symlink surfaced in listing: %+v", res.Entries)
	}
}

func TestWriteInFlightTempFilesHiddenAndCleaned(t *testing.T) {
	root := t.TempDir()
	b, _ := New(Config{RootPath: root})

	// A stray temp file (e.g. from a crashed writer) must not show up as an object.
	if err := os.WriteFile(filepath.Join(root, tempPrefix+"deadbeef"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(context.Background(), "real.txt", bytes.NewReader([]byte("ok")), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	res, err := b.List(context.Background(), backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "real.txt" {
		t.Errorf("listing = %+v, want only real.txt", res.Entries)
	}

	// A successful write leaves no temp file of its own behind.
	dirEntries, _ := os.ReadDir(root)
	temps := 0
	for _, e := range dirEntries {
		if len(e.Name()) >= len(tempPrefix) && e.Name()[:len(tempPrefix)] == tempPrefix {
			temps++
		}
	}
	if temps != 1 { // only the stray one we planted
		t.Errorf("found %d temp files, want just the 1 planted", temps)
	}
}

func TestWriteOverDirectoryFails(t *testing.T) {
	b := newBackend(t)
	if _, err := b.Write(context.Background(), "d/file.txt", bytes.NewReader([]byte("x")), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(context.Background(), "d", bytes.NewReader([]byte("x")), backend.WriteOptions{}); err == nil {
		t.Fatal("writing an object over an existing directory succeeded")
	}
	// The directory's contents are untouched.
	if _, _, err := b.Read(context.Background(), "d/file.txt"); err != nil {
		t.Errorf("existing object damaged: %v", err)
	}
}

func TestReadDirectoryIsNotFound(t *testing.T) {
	b := newBackend(t)
	if _, err := b.Write(context.Background(), "d/file.txt", bytes.NewReader([]byte("x")), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Read(context.Background(), "d"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Read(directory) error = %v, want ErrNotFound", err)
	}
}

// Tree order and plain string order disagree for names like "a-b" vs "a/x" ('-' sorts
// before '/'); a cursor scheme built on string comparison would drop or repeat entries
// at the seam. Paginate one at a time across exactly that shape.
func TestRecursivePaginationAcrossTreeOrderSeam(t *testing.T) {
	b := newBackend(t)
	want := []string{"a-b.txt", "a/x.txt", "a/y.txt", "a.txt", "b/z/deep.txt", "c.txt"}
	for _, p := range want {
		if _, err := b.Write(context.Background(), p, bytes.NewReader([]byte("x")), backend.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	for i := 0; i < 20; i++ {
		res, err := b.List(context.Background(), backend.ListOptions{Recursive: true, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Entries {
			seen[e.Path]++
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	for _, p := range want {
		if seen[p] != 1 {
			t.Errorf("%q seen %d times across pages, want exactly 1 (seen=%v)", p, seen[p], seen)
		}
	}
}
