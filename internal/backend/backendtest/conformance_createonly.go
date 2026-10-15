package backendtest

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// ADR 0117 point 8/8a: create-only writes, atomic per kind, never check-then-write.

var createOnlyTests = map[string]func(*testing.T, backend.Backend){
	"CreateOnlySucceedsOnNewKey":        createOnlySucceedsOnNewKey,
	"CreateOnlyRejectsExistingObject":   createOnlyRejectsExistingObject,
	"CreateOnlyNeverClobbersOnConflict": createOnlyNeverClobbersOnConflict,
	"CreateOnlyConcurrentSameKeyRace":   createOnlyConcurrentSameKeyRace,
}

func createOnlySucceedsOnNewKey(t *testing.T, b backend.Backend) {
	info, err := b.Write(ctx(t), "fresh.txt", bytes.NewReader([]byte("first")), backend.WriteOptions{CreateOnly: true})
	if err != nil {
		t.Fatalf("Write(CreateOnly) on an empty key: %v", err)
	}
	if info.Path != "fresh.txt" || info.Size != int64(len("first")) {
		t.Errorf("info = %+v", info)
	}
	data, _ := readAll(t, b, "fresh.txt")
	if string(data) != "first" {
		t.Errorf("read back %q, want %q", data, "first")
	}
}

func createOnlyRejectsExistingObject(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "taken.txt", "original")

	_, err := b.Write(ctx(t), "taken.txt", bytes.NewReader([]byte("overwrite attempt")), backend.WriteOptions{CreateOnly: true})
	if !errors.Is(err, backend.ErrPreconditionFailed) {
		t.Fatalf("Write(CreateOnly) over an existing object error = %v, want ErrPreconditionFailed", err)
	}
}

// createOnlyNeverClobbersOnConflict proves the rejected write didn't touch the original
// bytes even transiently — the thing a check-then-write race could get wrong.
func createOnlyNeverClobbersOnConflict(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "keep.txt", "the original, kept")
	if _, err := b.Write(ctx(t), "keep.txt", bytes.NewReader([]byte("x")), backend.WriteOptions{CreateOnly: true}); !errors.Is(err, backend.ErrPreconditionFailed) {
		t.Fatalf("Write(CreateOnly) error = %v, want ErrPreconditionFailed", err)
	}
	data, info := readAll(t, b, "keep.txt")
	if string(data) != "the original, kept" || info.Size != int64(len("the original, kept")) {
		t.Errorf("the original object was altered: %q (size %d)", data, info.Size)
	}
}

// createOnlyConcurrentSameKeyRace is the test the design note called for: many callers
// racing a create-only write at the same key, exactly one of which may win. Run with
// -race to also catch any internal data race in a kind's own implementation.
func createOnlyConcurrentSameKeyRace(t *testing.T, b backend.Backend) {
	const n = 8
	var wg sync.WaitGroup
	var succeeded, precondFailed, other atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Write(ctx(t), "race.txt", strings.NewReader("writer "+string(rune('A'+i))), backend.WriteOptions{CreateOnly: true})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, backend.ErrPreconditionFailed):
				precondFailed.Add(1)
			default:
				other.Add(1)
				t.Errorf("writer %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded.Load() != 1 {
		t.Errorf("successful create-only writes = %d, want exactly 1 (got %d precondition-failed, %d other)", succeeded.Load(), precondFailed.Load(), other.Load())
	}
	if precondFailed.Load()+succeeded.Load()+other.Load() != n {
		t.Errorf("accounted for %d of %d attempts", precondFailed.Load()+succeeded.Load()+other.Load(), n)
	}
	if _, _, err := b.Read(ctx(t), "race.txt"); err != nil {
		t.Errorf("the winning write left nothing readable: %v", err)
	}
}
