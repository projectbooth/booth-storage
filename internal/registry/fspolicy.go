package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// WorkspacePlaceholder in a configured root is replaced by the registering workspace's
// slug, which is what makes a root per-workspace-private.
const WorkspacePlaceholder = "{workspace}"

// FilesystemPolicy decides which server directories a workspace owner may register as a
// filesystem backend. It exists because registering a filesystem backend is otherwise
// "let a tenant pick any directory on the server": the platform operator, not a
// workspace owner, must decide what disk is shareable.
//
// Roots is the operator-configured allow-list (BOOTH_STORAGE_FILESYSTEM_ROOTS). A
// registered rootPath must equal or sit inside one of them. A root containing
// "{workspace}" (e.g. "/data/{workspace}") expands per workspace, so workspace "acme"
// can only reach /data/acme/... and never /data/other/... A root without the
// placeholder is shared by every workspace — an explicit operator choice, appropriate
// for a single-tenant install.
//
// The zero value allows nothing: the filesystem kind is disabled until an operator
// opts in.
type FilesystemPolicy struct {
	Roots []string
}

// Enabled reports whether any root is configured.
func (p FilesystemPolicy) Enabled() bool { return len(p.Roots) > 0 }

// Check returns nil if rootPath may be registered by workspace. The path must be
// absolute; it is checked both lexically and, where it already exists, after resolving
// symlinks, so a symlink planted inside an allowed root can't point a registration at
// somewhere outside it.
func (p FilesystemPolicy) Check(workspace, rootPath string) error {
	if !p.Enabled() {
		return errors.New("filesystem backends are not enabled on this deployment (the operator has not configured any allowed filesystem roots)")
	}
	if err := ValidateWorkspace(workspace); err != nil {
		return err
	}
	if !filepath.IsAbs(rootPath) {
		return fmt.Errorf("rootPath %q must be an absolute path", rootPath)
	}
	target := filepath.Clean(rootPath)

	for _, root := range p.Roots {
		allowed := filepath.Clean(strings.ReplaceAll(root, WorkspacePlaceholder, workspace))
		if !within(allowed, target) {
			continue
		}
		// Lexically inside. Now make sure symlinks don't lead out. The target itself may
		// not exist yet (a directory the operator hasn't created), but a symlink in one of
		// its *ancestors* can still redirect it — so resolve the deepest ancestor that does
		// exist and compare that, not just the full path.
		resolvedTarget, terr := resolveExisting(target)
		resolvedAllowed, aerr := resolveExisting(allowed)
		if terr == nil && aerr == nil && !within(resolvedAllowed, resolvedTarget) {
			continue
		}
		return nil
	}
	return fmt.Errorf("rootPath %q is outside the directories this workspace may use%s", rootPath, p.hint(workspace))
}

// hint names the workspace's allowed roots, so a refused admin learns where to point
// instead of guessing. Roots are operator-chosen, non-secret paths.
func (p FilesystemPolicy) hint(workspace string) string {
	expanded := make([]string, len(p.Roots))
	for i, r := range p.Roots {
		expanded[i] = filepath.Clean(strings.ReplaceAll(r, WorkspacePlaceholder, workspace))
	}
	return " (allowed: " + strings.Join(expanded, ", ") + ")"
}

// resolveExisting resolves symlinks in path as far as the path exists: it finds the longest
// existing ancestor, resolves that, and re-appends the not-yet-existing remainder. Unlike
// filepath.EvalSymlinks, it doesn't give up when the final component is missing — which
// is exactly the case where a symlinked ancestor is most dangerous.
func resolveExisting(path string) (string, error) {
	rest := ""
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err // reached the filesystem root without finding anything that exists
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether target equals base or is nested under it.
func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
