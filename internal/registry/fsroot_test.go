package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Found live by booth-e2e: registering a filesystem backend whose rootPath didn't exist
// returned 201, and then every read and write against it failed with a 502. A bad root is now
// refused at registration; a missing leaf whose parent exists is created.

func TestFilesystemRoot_MissingLeafIsCreatedAndUsable(t *testing.T) {
	e := newEnv(t)
	parent := e.wsDir(t, "acme", "mounted") // stands in for the operator's mounted volume
	leaf := filepath.Join(parent, "scratch")

	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(leaf)}); err != nil {
		t.Fatalf("Create with a missing leaf under an existing parent: %v", err)
	}
	if st, err := os.Stat(leaf); err != nil || !st.IsDir() {
		t.Fatalf("directory was not created: %v", err)
	}

	// The whole point: the backend actually works afterwards, instead of 502ing forever.
	b, err := e.svc.Open(testCtx, "acme", "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(testCtx, "hello.txt", strings.NewReader("hi"), backend.WriteOptions{}); err != nil {
		t.Fatalf("write to a freshly created root: %v", err)
	}
	if res, err := b.List(testCtx, backend.ListOptions{}); err != nil || len(res.Entries) != 1 {
		t.Errorf("list = %+v, %v", res, err)
	}
}

func TestFilesystemRoot_MissingParentIsRefusedAndNothingIsCreated(t *testing.T) {
	e := newEnv(t)
	e.wsDir(t, "acme", "exists") // the workspace dir exists; the path below has a missing parent
	missingParent := filepath.Join(e.fsRoot, "acme", "unmounted", "scratch")

	_, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(missingParent)})
	ve := asValidation(t, err)
	if ve.Field != "config" || !strings.Contains(ve.Message, "neither does its parent") {
		t.Errorf("error = %+v, want a config error naming the missing parent", ve)
	}
	// Refusing (rather than mkdir -p) is what keeps a failed volume mount from silently
	// turning into data written to the container's ephemeral disk.
	if _, statErr := os.Stat(filepath.Join(e.fsRoot, "acme", "unmounted")); statErr == nil {
		t.Error("a directory was created despite the parent being missing")
	}
	if _, gerr := e.svc.Get(testCtx, "acme", "s"); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("a refused registration left a record behind: %v", gerr)
	}
}

func TestFilesystemRoot_ExistingFileIsRefused(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "d")
	file := filepath.Join(dir, "im-a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(file)})
	if ve := asValidation(t, err); !strings.Contains(ve.Message, "not a directory") {
		t.Errorf("error = %v, want 'not a directory'", ve)
	}
	// A path *under* a file can't be created either.
	_, err = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s2", Kind: backend.KindFilesystem, Config: fsConfig(filepath.Join(file, "sub"))})
	asValidation(t, err)
	if data, _ := os.ReadFile(file); string(data) != "x" {
		t.Error("the file was damaged")
	}
}

func TestFilesystemRoot_ExistingDirectoryIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "keep")
	if err := os.WriteFile(filepath.Join(dir, "precious.txt"), []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(dir)}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "precious.txt")); string(data) != "p" {
		t.Error("registering an existing directory changed its contents")
	}
}

// The policy still runs first: nothing outside an allowed root is ever created, even when
// the path would otherwise be creatable.
func TestFilesystemRoot_PolicyRunsBeforeCreation(t *testing.T) {
	e := newEnv(t)
	other := e.wsDir(t, "globex", "theirs")
	victim := filepath.Join(other, "planted") // creatable — but it is another workspace's tree

	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(victim)}); err == nil {
		t.Fatal("registered a path inside another workspace's root")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Error("a directory was created inside another workspace's root")
	}
}

func TestFilesystemRoot_UpdateCreatesTheNewRootOrLeavesEverythingAlone(t *testing.T) {
	e := newEnv(t)
	parent := e.wsDir(t, "acme", "vol")
	first := e.wsDir(t, "acme", "vol/first")
	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(first)}); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(parent, "second")
	if _, err := e.svc.Update(testCtx, "acme", "s", UpdateInput{Config: fsConfig(second)}); err != nil {
		t.Fatalf("update to a creatable root: %v", err)
	}
	if st, err := os.Stat(second); err != nil || !st.IsDir() {
		t.Errorf("new root was not created: %v", err)
	}

	unfixable := filepath.Join(e.fsRoot, "acme", "gone", "third")
	if _, err := e.svc.Update(testCtx, "acme", "s", UpdateInput{Config: fsConfig(unfixable)}); err == nil {
		t.Fatal("update to an uncreatable root succeeded")
	}
	rec, _ := e.svc.Get(testCtx, "acme", "s")
	if !strings.Contains(string(rec.Config), "second") {
		t.Errorf("a refused update changed the record: %s", rec.Config)
	}
}

// "Test connection" must never have side effects: it reports a creatable path as fine, but
// leaves the disk exactly as it found it.
func TestFilesystemRoot_TestingNeverCreatesAnything(t *testing.T) {
	e := newEnv(t)
	parent := e.wsDir(t, "acme", "vol")
	leaf := filepath.Join(parent, "would-be-created")

	if err := e.svc.Test(testCtx, "acme", CreateInput{Kind: backend.KindFilesystem, Config: fsConfig(leaf)}); err != nil {
		t.Errorf("Test of a creatable path: %v", err)
	}
	if _, err := os.Stat(leaf); err == nil {
		t.Error("Test created the directory")
	}

	// And an uncreatable one is a failing test with the reason — not a passing one, and not a
	// malformed-request error (create is what returns the validation error).
	err := e.svc.Test(testCtx, "acme", CreateInput{Kind: backend.KindFilesystem, Config: fsConfig(filepath.Join(e.fsRoot, "acme", "nowhere", "leaf"))})
	var ve *ValidationError
	if err == nil || errors.As(err, &ve) || !strings.Contains(err.Error(), "neither does its parent") {
		t.Errorf("Test of an uncreatable path = %v, want a plain failure naming the missing parent", err)
	}

	// Same for previewing an edit of a saved backend.
	first := e.wsDir(t, "acme", "vol/first")
	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "s", Kind: backend.KindFilesystem, Config: fsConfig(first)}); err != nil {
		t.Fatal(err)
	}
	preview := filepath.Join(parent, "preview-only")
	if err := e.svc.TestUpdate(testCtx, "acme", "s", UpdateInput{Config: fsConfig(preview)}); err != nil {
		t.Errorf("TestUpdate of a creatable path: %v", err)
	}
	if _, err := os.Stat(preview); err == nil {
		t.Error("TestUpdate created the directory")
	}
}
