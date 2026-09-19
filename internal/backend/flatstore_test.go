package backend

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// memFlat is a tiny in-memory flat-key store with injectable failures, to test the generic
// folder helpers' failure handling — something real stores can't be made to do on demand.
type memFlat struct {
	keys         map[string]bool
	failCopyOn   string // copy to this destination key fails
	failDeleteOn string // delete of this key fails
}

func newMemFlat(keys ...string) *memFlat {
	m := &memFlat{keys: map[string]bool{}}
	for _, k := range keys {
		m.keys[k] = true
	}
	return m
}

func (m *memFlat) sorted() []string {
	var out []string
	for k := range m.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memFlat) List(_ context.Context, o ListOptions) (ListResult, error) {
	prefix, _ := CleanPrefix(o.Prefix)
	var res ListResult
	for _, k := range m.sorted() {
		if strings.HasPrefix(k, prefix) {
			res.Entries = append(res.Entries, ObjectInfo{Path: k})
			if o.Limit > 0 && len(res.Entries) >= o.Limit {
				break
			}
		}
	}
	return res, nil
}
func (m *memFlat) CopyKey(_ context.Context, from, to string) error {
	if to == m.failCopyOn {
		return errors.New("injected copy failure")
	}
	m.keys[to] = true
	return nil
}
func (m *memFlat) DeleteKey(_ context.Context, k string) error {
	if k == m.failDeleteOn {
		return errors.New("injected delete failure")
	}
	delete(m.keys, k)
	return nil
}
func (m *memFlat) KeyExists(_ context.Context, k string) (bool, error) { return m.keys[k], nil }

// Unused parts of Backend.
func (m *memFlat) Read(context.Context, string) (io.ReadCloser, ObjectInfo, error) {
	return nil, ObjectInfo{}, ErrNotFound
}
func (m *memFlat) Write(context.Context, string, io.Reader, WriteOptions) (ObjectInfo, error) {
	return ObjectInfo{}, nil
}
func (m *memFlat) Check(context.Context) error                       { return nil }
func (m *memFlat) Mkdir(context.Context, string) error               { return nil }
func (m *memFlat) DeleteObject(context.Context, string) error        { return nil }
func (m *memFlat) DeleteFolder(context.Context, string) (int, error) { return 0, nil }
func (m *memFlat) MoveObject(context.Context, string, string) error  { return nil }
func (m *memFlat) MoveFolder(context.Context, string, string) error  { return nil }

func TestMoveFolderFlat_CopyFailureRollsBackAndKeepsSource(t *testing.T) {
	m := newMemFlat("src/a.txt", "src/b.txt", "src/sub/c.txt", "other.txt")
	m.failCopyOn = "dst/sub/c.txt" // the third copy fails

	err := MoveFolderFlat(context.Background(), m, "src/", "dst/")
	if err == nil || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("error = %v, want a 'nothing was moved' failure", err)
	}
	want := []string{"other.txt", "src/a.txt", "src/b.txt", "src/sub/c.txt"}
	if got := m.sorted(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("store = %v, want the untouched source and no leftover copies %v", got, want)
	}
}

func TestMoveFolderFlat_DeleteFailureNeverLosesData(t *testing.T) {
	m := newMemFlat("src/a.txt", "src/b.txt")
	m.failDeleteOn = "src/b.txt"

	err := MoveFolderFlat(context.Background(), m, "src/", "dst/")
	if err == nil {
		t.Fatal("expected an error when an original can't be removed")
	}
	// Every object still exists somewhere: at worst duplicated, never lost.
	for _, k := range []string{"dst/a.txt", "dst/b.txt", "src/b.txt"} {
		if !m.keys[k] {
			t.Errorf("%s missing; store = %v", k, m.sorted())
		}
	}
}

func TestMoveFolderFlat_Guards(t *testing.T) {
	ctx := context.Background()
	if err := MoveFolderFlat(ctx, newMemFlat("a/x"), "a/", "a/b/"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("into itself = %v", err)
	}
	if err := MoveFolderFlat(ctx, newMemFlat("a/x", "b/y"), "a/", "b/"); !errors.Is(err, ErrConflict) {
		t.Errorf("onto existing = %v", err)
	}
	if err := MoveFolderFlat(ctx, newMemFlat("a/x"), "nope/", "z/"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing source = %v", err)
	}
	// "ab/" must not count as being inside "a/".
	m := newMemFlat("a/x", "ab/y")
	if err := MoveFolderFlat(ctx, m, "a/", "ab2/"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.sorted(), ","); got != "ab/y,ab2/x" {
		t.Errorf("store = %s", got)
	}
}

func TestDeleteFolderFlat_CountsObjectsNotPlaceholders(t *testing.T) {
	m := newMemFlat("f/a.txt", "f/sub/", "f/sub/b.txt", "f/empty/", "g/keep.txt")
	n, err := DeleteFolderFlat(context.Background(), m, "f/")
	if err != nil || n != 2 {
		t.Fatalf("DeleteFolderFlat = %d, %v; want 2 objects", n, err)
	}
	if got := strings.Join(m.sorted(), ","); got != "g/keep.txt" {
		t.Errorf("store = %s", got)
	}
	if _, err := DeleteFolderFlat(context.Background(), m, "f/"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestMoveObjectFlat(t *testing.T) {
	ctx := context.Background()
	m := newMemFlat("a", "b")
	if err := MoveObjectFlat(ctx, m, "a", "b"); !errors.Is(err, ErrConflict) {
		t.Errorf("onto existing = %v", err)
	}
	if err := MoveObjectFlat(ctx, m, "zzz", "c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := MoveObjectFlat(ctx, m, "a", "c"); err != nil || m.keys["a"] || !m.keys["c"] {
		t.Errorf("rename failed: %v %v", err, m.sorted())
	}
}
