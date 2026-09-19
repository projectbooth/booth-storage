package backendtest

import (
	"bytes"
	"errors"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// ADR 0038: create folder, delete, rename/move. Held to identical semantics on every kind.

var writeTests = map[string]func(*testing.T, backend.Backend){
	"MkdirCreatesEmptyFolder":        mkdirCreatesEmptyFolder,
	"MkdirIsIdempotent":              mkdirIsIdempotent,
	"MkdirRejectsInvalidPaths":       mkdirRejectsInvalidPaths,
	"DeleteObjectRemovesOnlyThatOne": deleteObjectRemovesOnlyThatOne,
	"DeleteObjectMissingIsNotFound":  deleteObjectMissingIsNotFound,
	"DeleteFolderRemovesTree":        deleteFolderRemovesTree,
	"DeleteFolderHonoursDirBoundary": deleteFolderHonoursDirBoundary,
	"DeleteFolderMissingAndRoot":     deleteFolderMissingAndRoot,
	"MoveObjectRenames":              moveObjectRenames,
	"MoveObjectNeverClobbers":        moveObjectNeverClobbers,
	"MoveFolderMovesWholeTree":       moveFolderMovesWholeTree,
	"MoveFolderNeverClobbers":        moveFolderNeverClobbers,
	"MoveFolderEdgeCases":            moveFolderEdgeCases,
}

func dirEntries(t *testing.T, b backend.Backend, prefix string) map[string]bool {
	t.Helper()
	res, err := b.List(ctx(t), backend.ListOptions{Prefix: prefix})
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	out := map[string]bool{}
	for _, e := range res.Entries {
		out[e.Path] = e.IsDir
	}
	return out
}

func mkdirCreatesEmptyFolder(t *testing.T, b backend.Backend) {
	if err := b.Mkdir(ctx(t), "reports/2026"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// The empty folder is visible and navigable...
	if got := dirEntries(t, b, ""); !got["reports/"] {
		t.Errorf("root listing = %v, want reports/ as a folder", got)
	}
	if got := dirEntries(t, b, "reports"); !got["reports/2026/"] {
		t.Errorf("reports listing = %v, want reports/2026/ as a folder", got)
	}
	if got := dirEntries(t, b, "reports/2026"); len(got) != 0 {
		t.Errorf("new folder is not empty: %v", got)
	}
	// ...but the placeholder is bookkeeping, never a listed object.
	all, err := b.List(ctx(t), backend.ListOptions{Recursive: true})
	if err != nil || len(all.Entries) != 0 {
		t.Errorf("recursive listing of a tree of empty folders = %+v, %v; want nothing", all.Entries, err)
	}
	// And a file can be written into it and listed there.
	mustWrite(t, b, "reports/2026/q1.csv", "x")
	if got := dirEntries(t, b, "reports/2026"); len(got) != 1 || got["reports/2026/q1.csv"] {
		t.Errorf("folder contents = %v", got)
	}
}

func mkdirIsIdempotent(t *testing.T, b backend.Backend) {
	for i := 0; i < 2; i++ {
		if err := b.Mkdir(ctx(t), "same"); err != nil {
			t.Fatalf("Mkdir #%d: %v", i+1, err)
		}
	}
	mustWrite(t, b, "keep/f.txt", "x")
	if err := b.Mkdir(ctx(t), "keep"); err != nil { // an existing, non-empty folder
		t.Fatalf("Mkdir on existing folder: %v", err)
	}
	if data, _ := readAll(t, b, "keep/f.txt"); string(data) != "x" {
		t.Error("Mkdir on an existing folder damaged its contents")
	}
}

func mkdirRejectsInvalidPaths(t *testing.T, b backend.Backend) {
	for _, p := range []string{"", "/", "..", "a/../b", "a//b", "/abs", "back\\slash", "nul\x00byte"} {
		if err := b.Mkdir(ctx(t), p); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("Mkdir(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func deleteObjectRemovesOnlyThatOne(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "a/x.txt", "x")
	mustWrite(t, b, "a/y.txt", "y")
	if err := b.DeleteObject(ctx(t), "a/x.txt"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, _, err := b.Read(ctx(t), "a/x.txt"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("deleted object still readable: %v", err)
	}
	if data, _ := readAll(t, b, "a/y.txt"); string(data) != "y" {
		t.Error("sibling object was affected")
	}
}

func deleteObjectMissingIsNotFound(t *testing.T, b backend.Backend) {
	if err := b.DeleteObject(ctx(t), "nope.txt"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("DeleteObject(missing) = %v, want ErrNotFound", err)
	}
	mustWrite(t, b, "dir/f.txt", "x")
	// A folder is not an object: DeleteObject must not remove it (or what's in it).
	if err := b.DeleteObject(ctx(t), "dir"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("DeleteObject(folder) = %v, want ErrNotFound", err)
	}
	if _, _, err := b.Read(ctx(t), "dir/f.txt"); err != nil {
		t.Errorf("DeleteObject on a folder damaged its contents: %v", err)
	}
	for _, p := range []string{"", "../x", "a//b", "/x", "trailing/"} {
		if err := b.DeleteObject(ctx(t), p); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("DeleteObject(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func deleteFolderRemovesTree(t *testing.T, b backend.Backend) {
	seedTree(t, b)
	if err := b.Mkdir(ctx(t), "data/emptysub"); err != nil {
		t.Fatal(err)
	}

	n, err := b.DeleteFolder(ctx(t), "data")
	if err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if n != 3 { // one.csv, two.csv, sub/three.csv — empty-folder bookkeeping is not counted
		t.Errorf("DeleteFolder reported %d objects removed, want 3", n)
	}
	if got := dirEntries(t, b, ""); got["data/"] || len(got) != 2 {
		t.Errorf("root after delete = %v, want only top.txt and logs/", got)
	}
	all, _ := b.List(ctx(t), backend.ListOptions{Recursive: true})
	assertPaths(t, all.Entries, "top.txt", "logs/app.log")
}

func deleteFolderHonoursDirBoundary(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "a/b/x.txt", "x")
	mustWrite(t, b, "a/bc/y.txt", "y")
	if _, err := b.DeleteFolder(ctx(t), "a/b"); err != nil {
		t.Fatal(err)
	}
	if data, _ := readAll(t, b, "a/bc/y.txt"); string(data) != "y" {
		t.Error("deleting folder a/b also removed the sibling a/bc")
	}
}

func deleteFolderMissingAndRoot(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "keep.txt", "x")
	if _, err := b.DeleteFolder(ctx(t), "no/such"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("DeleteFolder(missing) = %v, want ErrNotFound", err)
	}
	// The backend root can never be deleted, however it is spelled.
	for _, p := range []string{"", "/", "."} {
		if _, err := b.DeleteFolder(ctx(t), p); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("DeleteFolder(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
	if _, _, err := b.Read(ctx(t), "keep.txt"); err != nil {
		t.Errorf("rejected root delete still removed data: %v", err)
	}
	// A file is not a folder.
	if _, err := b.DeleteFolder(ctx(t), "keep.txt"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("DeleteFolder(file) = %v, want ErrNotFound", err)
	}
	if _, _, err := b.Read(ctx(t), "keep.txt"); err != nil {
		t.Errorf("DeleteFolder on a file removed it: %v", err)
	}
}

func moveObjectRenames(t *testing.T, b backend.Backend) {
	if _, err := b.Write(ctx(t), "old/name.csv", bytes.NewReader([]byte("payload")), backend.WriteOptions{ContentType: "text/csv"}); err != nil {
		t.Fatal(err)
	}
	if err := b.MoveObject(ctx(t), "old/name.csv", "new/place/renamed.csv"); err != nil {
		t.Fatalf("MoveObject: %v", err)
	}
	data, info := readAll(t, b, "new/place/renamed.csv")
	if string(data) != "payload" || info.Path != "new/place/renamed.csv" {
		t.Errorf("moved object = %q (%+v)", data, info)
	}
	if _, _, err := b.Read(ctx(t), "old/name.csv"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("source still present after move: %v", err)
	}
	if err := b.MoveObject(ctx(t), "missing.txt", "x.txt"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("MoveObject(missing) = %v, want ErrNotFound", err)
	}
	for _, bad := range [][2]string{{"a", "../b"}, {"../a", "b"}, {"a", "/b"}, {"a", ""}, {"a", "b/"}} {
		if err := b.MoveObject(ctx(t), bad[0], bad[1]); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("MoveObject(%q, %q) = %v, want ErrInvalidPath", bad[0], bad[1], err)
		}
	}
}

// A rename must never silently destroy data: moving onto an existing object fails and
// leaves both objects exactly as they were.
func moveObjectNeverClobbers(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "src.txt", "source")
	mustWrite(t, b, "dst.txt", "precious")
	if err := b.MoveObject(ctx(t), "src.txt", "dst.txt"); !errors.Is(err, backend.ErrConflict) {
		t.Fatalf("MoveObject onto existing = %v, want ErrConflict", err)
	}
	if data, _ := readAll(t, b, "dst.txt"); string(data) != "precious" {
		t.Errorf("destination was overwritten: %q", data)
	}
	if data, _ := readAll(t, b, "src.txt"); string(data) != "source" {
		t.Errorf("source was damaged: %q", data)
	}
}

func moveFolderMovesWholeTree(t *testing.T, b backend.Backend) {
	seedTree(t, b)
	if err := b.Mkdir(ctx(t), "data/emptysub"); err != nil {
		t.Fatal(err)
	}
	if err := b.MoveFolder(ctx(t), "data", "archive/2026"); err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}

	moved, err := b.List(ctx(t), backend.ListOptions{Prefix: "archive", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, moved.Entries, "archive/2026/one.csv", "archive/2026/two.csv", "archive/2026/sub/three.csv")
	if got := dirEntries(t, b, "archive/2026"); !got["archive/2026/emptysub/"] {
		t.Errorf("empty subfolder was lost in the move: %v", got)
	}
	if data, _ := readAll(t, b, "archive/2026/sub/three.csv"); string(data) != "x" {
		t.Error("moved content changed")
	}
	if got := dirEntries(t, b, ""); got["data/"] {
		t.Errorf("source folder still listed after move: %v", got)
	}
	// Everything outside the moved folder is untouched.
	if _, _, err := b.Read(ctx(t), "top.txt"); err != nil {
		t.Errorf("unrelated object affected: %v", err)
	}
}

func moveFolderNeverClobbers(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "from/a.txt", "a")
	mustWrite(t, b, "to/existing.txt", "keep me")
	if err := b.MoveFolder(ctx(t), "from", "to"); !errors.Is(err, backend.ErrConflict) {
		t.Fatalf("MoveFolder onto an existing folder = %v, want ErrConflict", err)
	}
	if data, _ := readAll(t, b, "to/existing.txt"); string(data) != "keep me" {
		t.Error("destination folder contents changed")
	}
	if data, _ := readAll(t, b, "from/a.txt"); string(data) != "a" {
		t.Error("source folder was damaged by a rejected move")
	}
	// An existing but empty destination folder also counts as taken.
	if err := b.Mkdir(ctx(t), "emptydst"); err != nil {
		t.Fatal(err)
	}
	if err := b.MoveFolder(ctx(t), "from", "emptydst"); !errors.Is(err, backend.ErrConflict) {
		t.Errorf("MoveFolder onto an empty existing folder = %v, want ErrConflict", err)
	}
}

func moveFolderEdgeCases(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "a/x.txt", "x")
	mustWrite(t, b, "ab/y.txt", "y")

	if err := b.MoveFolder(ctx(t), "a", "a/inner"); !errors.Is(err, backend.ErrInvalidPath) {
		t.Errorf("moving a folder into itself = %v, want ErrInvalidPath", err)
	}
	if err := b.MoveFolder(ctx(t), "missing", "x"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("MoveFolder(missing) = %v, want ErrNotFound", err)
	}
	for _, bad := range [][2]string{{"", "x"}, {"a", ""}, {"a", "../b"}, {"/a", "b"}} {
		if err := b.MoveFolder(ctx(t), bad[0], bad[1]); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("MoveFolder(%q, %q) = %v, want ErrInvalidPath", bad[0], bad[1], err)
		}
	}
	// "a" is a directory-boundary prefix: moving it must not drag "ab" along, and a sibling
	// whose name merely starts the same is not "inside" the source.
	if err := b.MoveFolder(ctx(t), "a", "ab2"); err != nil {
		t.Fatalf("MoveFolder(a -> ab2): %v", err)
	}
	if data, _ := readAll(t, b, "ab/y.txt"); string(data) != "y" {
		t.Error("moving folder a disturbed sibling folder ab")
	}
	if data, _ := readAll(t, b, "ab2/x.txt"); string(data) != "x" {
		t.Error("a was not moved to ab2")
	}
	// A file is not a folder.
	mustWrite(t, b, "file.txt", "f")
	if err := b.MoveFolder(ctx(t), "file.txt", "elsewhere"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("MoveFolder(file) = %v, want ErrNotFound", err)
	}
}
