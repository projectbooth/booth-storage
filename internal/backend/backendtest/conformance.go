// Package backendtest is the conformance suite every backend.Backend implementation
// runs, so "read/write/list works" is asserted identically for all four storage kinds
// (ADR 0013) instead of each kind getting its own subtly different test file.
//
// It lives in a non-_test package so the per-kind packages can import it; it only
// depends on the testing package, never on any test-only build tag.
package backendtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Factory returns a fresh backend that is empty and isolated from every other call's
// result (its own bucket/container/prefix/directory), so subtests never see each
// other's objects regardless of execution order.
type Factory func(t *testing.T) backend.Backend

// Run executes the full conformance suite against backends produced by newBackend.
func Run(t *testing.T, newBackend Factory) {
	t.Helper()

	tests := map[string]func(*testing.T, backend.Backend){
		"CheckSucceeds":             checkSucceeds,
		"WriteThenRead":             writeThenRead,
		"WriteOverwrites":           writeOverwrites,
		"WriteEmptyObject":          writeEmptyObject,
		"WriteNestedPath":           writeNestedPath,
		"ReadMissingIsNotFound":     readMissingIsNotFound,
		"InvalidPathsRejected":      invalidPathsRejected,
		"ListEmptyPrefix":           listEmptyPrefix,
		"ListNonRecursiveShowsDirs": listNonRecursiveShowsDirs,
		"ListRecursive":             listRecursive,
		"ListPrefixIsDirBoundary":   listPrefixIsDirBoundary,
		"ListPagination":            listPagination,
		"FailedWriteLeavesNoObject": failedWriteLeavesNoObject,
		"LargeObjectRoundTrip":      largeObjectRoundTrip,
		"ContentIsBinarySafe":       contentIsBinarySafe,
		"SpecialCharactersInPath":   specialCharactersInPath,
	}

	for name, fn := range writeTests { // ADR 0038 operations, conformance_write.go
		tests[name] = fn
	}

	// Sorted for deterministic ordering of test output.
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fn := tests[name]
		t.Run(name, func(t *testing.T) {
			fn(t, newBackend(t))
		})
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

func mustWrite(t *testing.T, b backend.Backend, path, content string) {
	t.Helper()
	if _, err := b.Write(ctx(t), path, bytes.NewReader([]byte(content)), backend.WriteOptions{}); err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
}

func readAll(t *testing.T, b backend.Backend, path string) ([]byte, backend.ObjectInfo) {
	t.Helper()
	rc, info, err := b.Read(ctx(t), path)
	if err != nil {
		t.Fatalf("Read(%q): %v", path, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q body: %v", path, err)
	}
	return data, info
}

func paths(entries []backend.ObjectInfo) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	sort.Strings(out)
	return out
}

func assertPaths(t *testing.T, got []backend.ObjectInfo, want ...string) {
	t.Helper()
	sort.Strings(want)
	g := paths(got)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Errorf("listed paths = %v, want %v", g, want)
	}
}

func checkSucceeds(t *testing.T, b backend.Backend) {
	if err := b.Check(ctx(t)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func writeThenRead(t *testing.T, b backend.Backend) {
	info, err := b.Write(ctx(t), "greeting.txt", bytes.NewReader([]byte("hello, booth")), backend.WriteOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info.Path != "greeting.txt" || info.Size != int64(len("hello, booth")) {
		t.Errorf("Write info = %+v, want path greeting.txt size %d", info, len("hello, booth"))
	}

	data, rinfo := readAll(t, b, "greeting.txt")
	if string(data) != "hello, booth" {
		t.Errorf("read back %q, want %q", data, "hello, booth")
	}
	if rinfo.Path != "greeting.txt" || rinfo.Size != int64(len("hello, booth")) {
		t.Errorf("Read info = %+v", rinfo)
	}
}

func writeOverwrites(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "f.txt", "first version, longer")
	mustWrite(t, b, "f.txt", "second")

	data, info := readAll(t, b, "f.txt")
	if string(data) != "second" || info.Size != int64(len("second")) {
		t.Errorf("after overwrite got %q (size %d), want %q", data, info.Size, "second")
	}
}

func writeEmptyObject(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "empty", "")
	data, info := readAll(t, b, "empty")
	if len(data) != 0 || info.Size != 0 {
		t.Errorf("empty object read back %d bytes (size %d)", len(data), info.Size)
	}
}

func writeNestedPath(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "a/b/c/deep.txt", "deep")
	data, _ := readAll(t, b, "a/b/c/deep.txt")
	if string(data) != "deep" {
		t.Errorf("read back %q", data)
	}
}

func readMissingIsNotFound(t *testing.T, b backend.Backend) {
	_, _, err := b.Read(ctx(t), "nope.txt")
	if !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Read(missing) error = %v, want ErrNotFound", err)
	}
	_, _, err = b.Read(ctx(t), "no/such/dir/nope.txt")
	if !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Read(missing nested) error = %v, want ErrNotFound", err)
	}
}

func invalidPathsRejected(t *testing.T, b backend.Backend) {
	bad := []string{"", "../escape", "a/../../escape", "a/./b", "a//b", "/../x", "/leading", "//double", "trailing/", "back\\slash", "nul\x00byte"}
	for _, p := range bad {
		if _, _, err := b.Read(ctx(t), p); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("Read(%q) error = %v, want ErrInvalidPath", p, err)
		}
		if _, err := b.Write(ctx(t), p, bytes.NewReader([]byte("x")), backend.WriteOptions{}); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("Write(%q) error = %v, want ErrInvalidPath", p, err)
		}
	}
	for _, p := range []string{"../", "a/../b", "a//b", "/leading", "//", "back\\slash"} {
		if _, err := b.List(ctx(t), backend.ListOptions{Prefix: p}); !errors.Is(err, backend.ErrInvalidPath) {
			t.Errorf("List(prefix %q) error = %v, want ErrInvalidPath", p, err)
		}
	}
}

func listEmptyPrefix(t *testing.T, b backend.Backend) {
	res, err := b.List(ctx(t), backend.ListOptions{})
	if err != nil {
		t.Fatalf("List root of empty backend: %v", err)
	}
	if len(res.Entries) != 0 || res.NextCursor != "" {
		t.Errorf("empty backend listed %+v", res)
	}

	res, err = b.List(ctx(t), backend.ListOptions{Prefix: "does/not/exist"})
	if err != nil {
		t.Fatalf("List of nonexistent prefix should be empty, not an error: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("nonexistent prefix listed %+v", res.Entries)
	}
}

func seedTree(t *testing.T, b backend.Backend) {
	for _, p := range []string{"top.txt", "data/one.csv", "data/two.csv", "data/sub/three.csv", "logs/app.log"} {
		mustWrite(t, b, p, "x")
	}
}

func listNonRecursiveShowsDirs(t *testing.T, b backend.Backend) {
	seedTree(t, b)

	root, err := b.List(ctx(t), backend.ListOptions{})
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	assertPaths(t, root.Entries, "top.txt", "data/", "logs/")
	for _, e := range root.Entries {
		if isDirPath(e.Path) != e.IsDir {
			t.Errorf("entry %q: IsDir = %v, but path trailing slash says otherwise", e.Path, e.IsDir)
		}
	}

	// A prefix works with or without its trailing slash.
	for _, prefix := range []string{"data", "data/"} {
		res, err := b.List(ctx(t), backend.ListOptions{Prefix: prefix})
		if err != nil {
			t.Fatalf("List(prefix %q): %v", prefix, err)
		}
		assertPaths(t, res.Entries, "data/one.csv", "data/two.csv", "data/sub/")
	}
}

func listRecursive(t *testing.T, b backend.Backend) {
	seedTree(t, b)

	all, err := b.List(ctx(t), backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatalf("List recursive: %v", err)
	}
	assertPaths(t, all.Entries, "top.txt", "data/one.csv", "data/two.csv", "data/sub/three.csv", "logs/app.log")
	for _, e := range all.Entries {
		if e.IsDir {
			t.Errorf("recursive listing returned a directory entry %q", e.Path)
		}
	}

	under, err := b.List(ctx(t), backend.ListOptions{Prefix: "data", Recursive: true})
	if err != nil {
		t.Fatalf("List recursive under data: %v", err)
	}
	assertPaths(t, under.Entries, "data/one.csv", "data/two.csv", "data/sub/three.csv")
}

func listPrefixIsDirBoundary(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "a/b/x.txt", "x")
	mustWrite(t, b, "a/bc/y.txt", "y")

	res, err := b.List(ctx(t), backend.ListOptions{Prefix: "a/b", Recursive: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertPaths(t, res.Entries, "a/b/x.txt")
}

func listPagination(t *testing.T, b backend.Backend) {
	want := []string{"p/0", "p/1", "p/2", "p/3", "p/4"}
	for _, p := range want {
		mustWrite(t, b, p, "x")
	}

	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		res, err := b.List(ctx(t), backend.ListOptions{Prefix: "p", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		if len(res.Entries) > 2 {
			t.Fatalf("page %d returned %d entries, limit was 2", page, len(res.Entries))
		}
		got = append(got, paths(res.Entries)...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paginated listing = %v, want each of %v exactly once", got, want)
	}
}

// erroringReader yields some bytes and then fails, simulating a client that drops the
// connection mid-upload.
type erroringReader struct {
	sent int
}

var errSimulated = errors.New("simulated mid-upload failure")

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.sent >= 64*1024 {
		return 0, errSimulated
	}
	n := len(p)
	if n > 32*1024 {
		n = 32 * 1024
	}
	for i := 0; i < n; i++ {
		p[i] = 'z'
	}
	r.sent += n
	return n, nil
}

func failedWriteLeavesNoObject(t *testing.T, b backend.Backend) {
	if _, err := b.Write(ctx(t), "partial.bin", &erroringReader{}, backend.WriteOptions{}); err == nil {
		t.Fatal("Write with a failing reader returned nil error")
	}

	if _, _, err := b.Read(ctx(t), "partial.bin"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("after a failed write, Read error = %v, want ErrNotFound (a partial object must never be visible)", err)
	}

	res, err := b.List(ctx(t), backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("after a failed write, listing shows %v", paths(res.Entries))
	}
}

// largeObjectRoundTrip is sized past the 16 MiB multipart threshold the S3 client uses
// for unknown-length streams, so the multipart upload path is actually exercised.
func largeObjectRoundTrip(t *testing.T, b backend.Backend) {
	if testing.Short() {
		t.Skip("skipping large object test in -short mode")
	}
	payload := make([]byte, 17<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	info, err := b.Write(ctx(t), "big.bin", bytes.NewReader(payload), backend.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Write reported size %d, want %d", info.Size, len(payload))
	}

	got, _ := readAll(t, b, "big.bin")
	if !bytes.Equal(got, payload) {
		t.Errorf("large object corrupted in round trip (got %d bytes, want %d)", len(got), len(payload))
	}
}

func contentIsBinarySafe(t *testing.T, b backend.Backend) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	if _, err := b.Write(ctx(t), "bytes.bin", bytes.NewReader(payload), backend.WriteOptions{ContentType: "application/octet-stream"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := readAll(t, b, "bytes.bin")
	if !bytes.Equal(got, payload) {
		t.Error("binary content altered in round trip")
	}
}

func specialCharactersInPath(t *testing.T, b backend.Backend) {
	for _, p := range []string{"with space.txt", "unicode-é-日本.txt", "plus+and&amp=eq.txt", "dir with space/file.txt"} {
		mustWrite(t, b, p, p)
		got, _ := readAll(t, b, p)
		if string(got) != p {
			t.Errorf("%q read back %q", p, got)
		}
	}
	res, err := b.List(ctx(t), backend.ListOptions{Recursive: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertPaths(t, res.Entries, "with space.txt", "unicode-é-日本.txt", "plus+and&amp=eq.txt", "dir with space/file.txt")
}

func isDirPath(p string) bool { return len(p) > 0 && p[len(p)-1] == '/' }
