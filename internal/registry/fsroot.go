package registry

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectbooth/booth-storage/internal/backend/filesystem"
)

// rootStatus is what checkFilesystemRoot found.
type rootStatus int

const (
	// rootReady: the directory exists.
	rootReady rootStatus = iota
	// rootWillBeCreated: it doesn't exist, but its parent does, so registering will create it.
	rootWillBeCreated
)

// checkFilesystemRoot makes a filesystem backend's rootPath usable at registration time, so a
// bad path is a 4xx now instead of a 502 on every later read and write (found live by
// booth-e2e: a nonexistent rootPath registered fine, then nothing worked).
//
// The rule is deliberately narrower than "mkdir -p":
//
//   - the path exists and is a directory: fine;
//   - the path exists but is not a directory: refused;
//   - the path is missing but its PARENT is a directory: the leaf is created (when create is
//     true). This is the common case — an operator mounted /data, the allow-list is
//     /data/{workspace}, and nobody created /data/acme;
//   - the parent is missing too: refused, and nothing is created.
//
// Refusing when the parent is missing is what makes creating the leaf safe. If a volume
// failed to mount, "create the whole path" would silently make the directories on the
// container's ephemeral disk, and a workspace would happily store data there that vanishes on
// restart. With the parent required, a missing mount fails loudly at registration.
//
// The caller has already passed the path through FilesystemPolicy.Check (inside an allowed
// root, symlinks resolved), so nothing here can create a directory outside one. With
// create=false nothing on disk is touched — used by "test connection", which must have no
// side effects.
func checkFilesystemRoot(cfg json.RawMessage, create bool) (rootStatus, error) {
	var c filesystem.Config
	if err := json.Unmarshal(cfg, &c); err != nil {
		return rootReady, invalid("config", "%v", err)
	}
	root := filepath.Clean(c.RootPath)

	st, err := os.Stat(root)
	switch {
	case err == nil && st.IsDir():
		return rootReady, nil
	case err == nil:
		return rootReady, invalid("config", "rootPath %q exists but is not a directory", root)
	case !errors.Is(err, fs.ErrNotExist):
		return rootReady, invalid("config", "cannot access rootPath %q: %v", root, err)
	}

	parent := filepath.Dir(root)
	pst, perr := os.Stat(parent)
	switch {
	case perr != nil && errors.Is(perr, fs.ErrNotExist):
		return rootReady, invalid("config",
			"rootPath %q does not exist, and neither does its parent directory %q — the directory (or the volume it lives on) has to be mounted or created by the platform operator first",
			root, parent)
	case perr != nil:
		return rootReady, invalid("config", "cannot access %q: %v", parent, perr)
	case !pst.IsDir():
		return rootReady, invalid("config", "rootPath %q cannot be created: its parent %q is not a directory", root, parent)
	}

	if !create {
		return rootWillBeCreated, nil
	}
	// Single-level Mkdir, never MkdirAll: the parent was just confirmed to exist.
	if err := os.Mkdir(root, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return rootReady, invalid("config",
			"rootPath %q does not exist and could not be created: %v — check the volume is writable by the storage module's user", root, err)
	}
	return rootReady, nil
}
