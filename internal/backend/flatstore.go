package backend

import (
	"context"
	"fmt"
	"strings"
)

// maxFolderObjects bounds a single folder delete/move on a flat-key store. These are
// list-then-act loops over every object under a prefix, so an enormous folder would run
// for a very long time inside one HTTP request; refusing is safer than half-finishing.
const maxFolderObjects = 100_000

// FlatStore is what the flat-key object stores (S3, GCS, Azure Blob) provide beyond
// Backend so folder delete/move — which don't exist natively there — can be built once,
// generically, instead of three times. Keys are relative to the backend root and, unlike
// public paths, may end in "/" (the folder placeholder objects).
type FlatStore interface {
	Backend
	// CopyKey copies one object, overwriting the destination.
	CopyKey(ctx context.Context, from, to string) error
	// DeleteKey removes one object by raw key; a missing key is not an error.
	DeleteKey(ctx context.Context, key string) error
	// KeyExists reports whether an object with exactly this key exists.
	KeyExists(ctx context.Context, key string) (bool, error)
}

// keysUnder lists every key under prefix, folder placeholders included.
func keysUnder(ctx context.Context, s FlatStore, prefix string, limit int) ([]string, error) {
	var keys []string
	cursor := ""
	for {
		res, err := s.List(ctx, ListOptions{Prefix: prefix, Recursive: true, Limit: MaxListLimit, Cursor: cursor, IncludeFolderMarkers: true})
		if err != nil {
			return nil, err
		}
		for _, e := range res.Entries {
			keys = append(keys, e.Path)
		}
		if len(keys) > limit {
			return nil, fmt.Errorf("folder holds more than %d objects — too large to change through one request", limit)
		}
		if res.NextCursor == "" {
			return keys, nil
		}
		cursor = res.NextCursor
	}
}

// DeleteFolderFlat removes every object under folder ("a/b/"), placeholders included.
func DeleteFolderFlat(ctx context.Context, s FlatStore, folder string) (int, error) {
	keys, err := keysUnder(ctx, s, folder, maxFolderObjects)
	if err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, ErrNotFound
	}
	removed := 0
	for _, k := range keys {
		if err := s.DeleteKey(ctx, k); err != nil {
			return removed, fmt.Errorf("deleted %d of %d objects before failing on %q: %w", removed, len(keys), k, err)
		}
		if !strings.HasSuffix(k, "/") {
			removed++ // placeholders are bookkeeping, not objects the user knows about
		}
	}
	return removed, nil
}

// MoveObjectFlat renames one object as copy-then-delete, refusing to overwrite.
func MoveObjectFlat(ctx context.Context, s FlatStore, from, to string) error {
	if from == to {
		return nil
	}
	if ok, err := s.KeyExists(ctx, from); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	if ok, err := s.KeyExists(ctx, to); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: %q already exists", ErrConflict, to)
	}
	if err := s.CopyKey(ctx, from, to); err != nil {
		return err
	}
	return s.DeleteKey(ctx, from)
}

// MoveFolderFlat renames a folder ("a/" -> "b/") by copying every object under it and then
// deleting the originals. Not atomic, so it is ordered to fail safe: all copies are made
// first, and if any fails the copies already made are removed and the source is untouched;
// sources are deleted only once every copy has succeeded.
func MoveFolderFlat(ctx context.Context, s FlatStore, from, to string) error {
	if from == to {
		return nil
	}
	if strings.HasPrefix(to, from) {
		return fmt.Errorf("%w: cannot move a folder into itself", ErrInvalidPath)
	}
	keys, err := keysUnder(ctx, s, from, maxFolderObjects)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrNotFound
	}
	probe, err := s.List(ctx, ListOptions{Prefix: to, Recursive: true, Limit: 1, IncludeFolderMarkers: true})
	if err != nil {
		return err
	}
	if len(probe.Entries) > 0 {
		return fmt.Errorf("%w: folder %q already exists", ErrConflict, to)
	}

	var copied []string
	rollback := func() {
		for _, k := range copied {
			_ = s.DeleteKey(context.WithoutCancel(ctx), k)
		}
	}
	for _, k := range keys {
		dst := to + strings.TrimPrefix(k, from)
		if err := s.CopyKey(ctx, k, dst); err != nil {
			rollback()
			return fmt.Errorf("copying %q: %w (nothing was moved)", k, err)
		}
		copied = append(copied, dst)
	}
	for i, k := range keys {
		if err := s.DeleteKey(ctx, k); err != nil {
			return fmt.Errorf("copied to %q but could not remove %d of %d originals (first failure %q): %w", to, len(keys)-i, len(keys), k, err)
		}
	}
	return nil
}
