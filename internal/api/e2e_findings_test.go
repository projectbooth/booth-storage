package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for two findings from booth-e2e's first full run. Both were client-shaped
// situations that surfaced as 5xx; each now gets a status a client can act on.

// Finding: registering a filesystem backend whose rootPath doesn't exist returned 201, then
// every read and write against it returned 502.
func TestRegisteringAMissingFilesystemRoot(t *testing.T) {
	t.Run("a missing leaf under an existing parent is created, and the backend works", func(t *testing.T) {
		e := newTestEnv(t)
		parent := e.wsDir(t, "acme", "vol")
		leaf := filepath.Join(parent, "scratch")

		expect(t, e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
			"id": "s", "kind": "filesystem", "config": map[string]string{"rootPath": leaf},
		}), 201)
		if st, err := os.Stat(leaf); err != nil || !st.IsDir() {
			t.Fatalf("directory not created: %v", err)
		}
		// The reported symptom: everything after registration 502'd. It must now just work.
		expect(t, e.do(t, bob, "PUT", "/api/backends/s/objects/hello.txt", "hi"), 200)
		r := e.do(t, carol, "GET", "/api/backends/s/objects/hello.txt", nil)
		expect(t, r, 200)
		if r.Body.String() != "hi" {
			t.Errorf("read back %q", r.Body.String())
		}
		expect(t, e.do(t, carol, "GET", "/api/backends/s/objects", nil), 200)
	})

	t.Run("a missing parent is a 4xx naming the problem, and nothing is registered or created", func(t *testing.T) {
		e := newTestEnv(t)
		e.wsDir(t, "acme", "vol")
		missing := filepath.Join(e.fsRoot, "acme", "unmounted", "scratch")

		r := e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
			"id": "s", "kind": "filesystem", "config": map[string]string{"rootPath": missing},
		})
		expect(t, r, 422)
		if !strings.Contains(r.errorMessage(), "neither does its parent") {
			t.Errorf("message = %q", r.errorMessage())
		}
		expect(t, e.do(t, carol, "GET", "/api/backends/s", nil), 404) // no half-registered backend
		if _, err := os.Stat(filepath.Join(e.fsRoot, "acme", "unmounted")); err == nil {
			t.Error("a directory was created despite the missing parent")
		}
	})

	t.Run("a rootPath that is a file is a 4xx", func(t *testing.T) {
		e := newTestEnv(t)
		dir := e.wsDir(t, "acme", "vol")
		file := filepath.Join(dir, "afile")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
			"id": "s", "kind": "filesystem", "config": map[string]string{"rootPath": file},
		})
		expect(t, r, 422)
		if !strings.Contains(r.errorMessage(), "not a directory") {
			t.Errorf("message = %q", r.errorMessage())
		}
	})
}

// Finding: GET .../objects?prefix=hello.txt, where hello.txt is a file, returned a 502
// ("readdirent: not a directory"). A plausible-but-wrong prefix is not a server error.
func TestFileUsedAsAFolder(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "x")
	expect(t, e.do(t, bob, "PUT", "/api/backends/x/objects/hello.txt", "hi"), 200)

	for _, q := range []string{"prefix=hello.txt", "prefix=hello.txt%2F", "prefix=hello.txt%2Fsub", "prefix=hello.txt&recursive=true"} {
		r := e.do(t, carol, "GET", "/api/backends/x/objects?"+q, nil)
		expect(t, r, 200) // an empty page: nothing lives under a file
		if !strings.Contains(r.Body.String(), `"entries":[]`) {
			t.Errorf("?%s = %s, want an empty entries list", q, r.Body.String())
		}
	}

	// A path *through* a file: nothing there (404) for reads and deletes...
	expect(t, e.do(t, carol, "GET", "/api/backends/x/objects/hello.txt/inner", nil), 404)
	expect(t, e.do(t, bob, "DELETE", "/api/backends/x/objects/hello.txt/inner", nil), 404)
	// ...and a conflict the caller can resolve (409) when trying to create beneath a file.
	expect(t, e.do(t, bob, "PUT", "/api/backends/x/objects/hello.txt/inner", "x"), 409)
	expect(t, e.do(t, bob, "POST", "/api/backends/x/folders", map[string]any{"path": "hello.txt/sub"}), 409)

	// None of it harmed the file.
	if data, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || string(data) != "hi" {
		t.Errorf("hello.txt = %q, %v", data, err)
	}
	// The per-path read the e2e suite switched to still works.
	if r := e.do(t, carol, "GET", "/api/backends/x/objects/hello.txt", nil); r.Body.String() != "hi" {
		t.Errorf("per-path read = %q", r.Body.String())
	}
}
