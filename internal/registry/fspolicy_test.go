package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemPolicy_ZeroValueAllowsNothing(t *testing.T) {
	err := FilesystemPolicy{}.Check("acme", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("Check = %v, want a 'not enabled' refusal", err)
	}
}

func TestFilesystemPolicy_PerWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	p := FilesystemPolicy{Roots: []string{filepath.Join(base, WorkspacePlaceholder)}}

	allowed := []string{
		filepath.Join(base, "acme"),
		filepath.Join(base, "acme", "nested", "deeper"),
		filepath.Join(base, "acme", ".", "x"), // cleaned lexically
	}
	for _, d := range allowed {
		if err := p.Check("acme", d); err != nil {
			t.Errorf("Check(acme, %s) = %v, want allowed", d, err)
		}
	}

	refused := map[string]string{
		"another workspace":        filepath.Join(base, "globex"),
		"prefix-sibling lookalike": filepath.Join(base, "acme-evil"),
		"the base itself":          base,
		"parent traversal":         filepath.Join(base, "acme", "..", "globex"),
		"unrelated absolute path":  filepath.Join(os.TempDir(), "elsewhere"),
	}
	for name, d := range refused {
		if err := p.Check("acme", d); err == nil {
			t.Errorf("%s (%s) was allowed", name, d)
		}
	}

	if err := p.Check("acme", "not/absolute"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path error = %v", err)
	}
}

func TestFilesystemPolicy_SharedRootAndMultipleRoots(t *testing.T) {
	shared, scratch := t.TempDir(), t.TempDir()
	p := FilesystemPolicy{Roots: []string{shared, filepath.Join(scratch, WorkspacePlaceholder)}}

	if err := p.Check("acme", filepath.Join(shared, "anything")); err != nil {
		t.Errorf("shared root: %v", err)
	}
	if err := p.Check("globex", filepath.Join(shared, "anything")); err != nil {
		t.Errorf("shared root is by design usable by every workspace: %v", err)
	}
	if err := p.Check("acme", filepath.Join(scratch, "acme", "x")); err != nil {
		t.Errorf("second root, own workspace: %v", err)
	}
	if err := p.Check("acme", filepath.Join(scratch, "globex", "x")); err == nil {
		t.Error("second root, another workspace's directory was allowed")
	}
}

// A workspace slug is interpolated into a path, so a hostile one must be rejected
// before it can reshape the path.
func TestFilesystemPolicy_RejectsHostileWorkspaceSlug(t *testing.T) {
	base := t.TempDir()
	p := FilesystemPolicy{Roots: []string{filepath.Join(base, WorkspacePlaceholder)}}
	for _, ws := range []string{"..", "../x", "a/b", "", "UPPER", "a b"} {
		if err := p.Check(ws, filepath.Join(base, "x")); err == nil {
			t.Errorf("workspace %q was accepted", ws)
		}
	}
}

// A symlink inside an allowed root that points outside it must not let a registration
// escape: the lexical path looks fine, only resolving symlinks reveals the escape.
func TestFilesystemPolicy_SymlinkEscapeRefused(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "acme"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "acme", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlinks on this host (Windows without privilege?): %v", err)
	}
	p := FilesystemPolicy{Roots: []string{filepath.Join(base, WorkspacePlaceholder)}}

	if err := p.Check("acme", link); err == nil {
		t.Error("a symlink pointing outside the allowed root was accepted")
	}
	if err := p.Check("acme", filepath.Join(link, "sub")); err == nil {
		t.Error("a path through an escaping symlink was accepted")
	}

	// A symlink that stays inside the root is fine.
	inside := filepath.Join(base, "acme", "real")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	okLink := filepath.Join(base, "acme", "alias")
	if err := os.Symlink(inside, okLink); err != nil {
		t.Fatal(err)
	}
	if err := p.Check("acme", okLink); err != nil {
		t.Errorf("an in-root symlink was refused: %v", err)
	}
}
