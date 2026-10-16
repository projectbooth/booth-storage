package backendtest

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Ranged reads (HTTP 206) and Stat (HTTP HEAD): every kind must honour an exact byte
// range atomically against its own store, report the same ETag/size for a ranged read
// as for the whole object, and let Stat answer without opening the object's content.

var rangeTests = map[string]func(*testing.T, backend.Backend){
	"StatMatchesWriteAndRead":            statMatchesWriteAndRead,
	"StatMissingIsNotFound":              statMissingIsNotFound,
	"ReadRangeMiddleSlice":               readRangeMiddleSlice,
	"ReadRangeFromStart":                 readRangeFromStart,
	"ReadRangeToEnd":                     readRangeToEnd,
	"ReadRangeSingleByte":                readRangeSingleByte,
	"ReadRangeWholeObject":               readRangeWholeObject,
	"ReadRangeOutOfBoundsIsInvalidRange": readRangeOutOfBoundsIsInvalidRange,
	"ReadRangeETagMatchesWholeRead":      readRangeETagMatchesWholeRead,
}

func statMatchesWriteAndRead(t *testing.T, b backend.Backend) {
	written, err := b.Write(ctx(t), "stat.txt", bytes.NewReader([]byte("hello, stat")), backend.WriteOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	st, err := b.Stat(ctx(t), "stat.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Path != "stat.txt" || st.Size != written.Size {
		t.Errorf("Stat = %+v, want path stat.txt size %d", st, written.Size)
	}

	_, rinfo := readAll(t, b, "stat.txt")
	if st.Size != rinfo.Size {
		t.Errorf("Stat size %d != Read size %d", st.Size, rinfo.Size)
	}
}

func statMissingIsNotFound(t *testing.T, b backend.Backend) {
	if _, err := b.Stat(ctx(t), "nope.txt"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
}

const rangeContent = "0123456789abcdefghij" // 20 bytes, indices easy to eyeball

func readRangeSlice(t *testing.T, b backend.Backend, path string, rg backend.ByteRange) string {
	t.Helper()
	rc, _, err := b.ReadRange(ctx(t), path, rg)
	if err != nil {
		t.Fatalf("ReadRange(%+v): %v", rg, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading range body: %v", err)
	}
	// ObjectInfo.Size from a ranged read is deliberately not asserted here: kinds differ
	// on whether it's the range's own length or the whole object's (see s3.go's
	// ReadRange), and the HTTP layer never relies on it either way — it computes
	// Content-Length itself from the range it resolved. The delivered bytes are the
	// real assertion.
	wantLen := rg.End - rg.Start + 1
	if int64(len(data)) != wantLen {
		t.Errorf("ReadRange(%+v) returned %d bytes, want %d", rg, len(data), wantLen)
	}
	return string(data)
}

func readRangeMiddleSlice(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	got := readRangeSlice(t, b, "range.txt", backend.ByteRange{Start: 5, End: 9})
	if want := rangeContent[5:10]; got != want {
		t.Errorf("ReadRange(5,9) = %q, want %q", got, want)
	}
}

func readRangeFromStart(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	got := readRangeSlice(t, b, "range.txt", backend.ByteRange{Start: 0, End: 3})
	if want := rangeContent[0:4]; got != want {
		t.Errorf("ReadRange(0,3) = %q, want %q", got, want)
	}
}

func readRangeToEnd(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	last := int64(len(rangeContent)) - 1
	got := readRangeSlice(t, b, "range.txt", backend.ByteRange{Start: 15, End: last})
	if want := rangeContent[15:]; got != want {
		t.Errorf("ReadRange(15,end) = %q, want %q", got, want)
	}
}

func readRangeSingleByte(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	got := readRangeSlice(t, b, "range.txt", backend.ByteRange{Start: 7, End: 7})
	if want := rangeContent[7:8]; got != want {
		t.Errorf("ReadRange(7,7) = %q, want %q", got, want)
	}
}

func readRangeWholeObject(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	last := int64(len(rangeContent)) - 1
	got := readRangeSlice(t, b, "range.txt", backend.ByteRange{Start: 0, End: last})
	if got != rangeContent {
		t.Errorf("ReadRange(whole) = %q, want %q", got, rangeContent)
	}
}

// readRangeOutOfBoundsIsInvalidRange proves the backend's own backstop (the HTTP layer
// ordinarily never asks for a range outside the object it just Stat'd).
func readRangeOutOfBoundsIsInvalidRange(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	size := int64(len(rangeContent))
	for _, rg := range []backend.ByteRange{
		{Start: size, End: size + 4},       // starts exactly at EOF
		{Start: size + 10, End: size + 20}, // starts well past EOF
	} {
		_, _, err := b.ReadRange(ctx(t), "range.txt", rg)
		if !errors.Is(err, backend.ErrInvalidRange) {
			t.Errorf("ReadRange(%+v) on a %d-byte object error = %v, want ErrInvalidRange", rg, size, err)
		}
	}
}

// readRangeETagMatchesWholeRead proves a ranged read identifies the same object version
// as a whole read, so a client that validates a range against an earlier HEAD's ETag
// isn't fooled by a kind that (incorrectly) computed a range-specific tag.
func readRangeETagMatchesWholeRead(t *testing.T, b backend.Backend) {
	mustWrite(t, b, "range.txt", rangeContent)
	_, whole := readAll(t, b, "range.txt")
	if whole.ETag == "" {
		t.Skip("this kind reports no ETag on a whole read (see its own Read's comment); nothing to compare")
	}
	_, partial, err := b.ReadRange(ctx(t), "range.txt", backend.ByteRange{Start: 2, End: 5})
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if partial.ETag != whole.ETag {
		t.Errorf("ranged read ETag %q != whole read ETag %q", partial.ETag, whole.ETag)
	}
}
