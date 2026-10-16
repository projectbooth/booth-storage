package api

import (
	"strings"
	"testing"
)

// Ranged GET (206) and HEAD/stat, over the HTTP surface — the conformance suite proves
// every backend kind honours these at the storage layer; these tests prove the routing,
// header-setting and RFC 9110 Range-parsing fallback behaviour this package owns.

const rangeBody = "0123456789abcdefghij" // 20 bytes

func setupRangeObject(t *testing.T) (*testEnv, string) {
	t.Helper()
	e := newTestEnv(t)
	e.registerFS(t, "acme", "scratch")
	expect(t, e.do(t, bob, "PUT", "/api/backends/scratch/objects/range.txt", rangeBody, "Content-Type", "text/plain"), 200)
	return e, "/api/backends/scratch/objects/range.txt"
}

func TestRangedGet_WholeObjectAdvertisesRangingAndETag(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil)
	expect(t, r, 200)
	if r.Body.String() != rangeBody {
		t.Errorf("body = %q", r.Body.String())
	}
	h := r.Header()
	if h.Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", h.Get("Accept-Ranges"))
	}
	if h.Get("ETag") == "" {
		t.Error("ETag missing on a plain GET of a filesystem-backed object")
	}
	if h.Get("Content-Length") != "20" {
		t.Errorf("Content-Length = %q, want 20", h.Get("Content-Length"))
	}
}

func TestRangedGet_MiddleSlice(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=5-9")
	expect(t, r, 206)
	if r.Body.String() != rangeBody[5:10] {
		t.Errorf("body = %q, want %q", r.Body.String(), rangeBody[5:10])
	}
	h := r.Header()
	if h.Get("Content-Range") != "bytes 5-9/20" {
		t.Errorf("Content-Range = %q, want %q", h.Get("Content-Range"), "bytes 5-9/20")
	}
	if h.Get("Content-Length") != "5" {
		t.Errorf("Content-Length = %q, want 5", h.Get("Content-Length"))
	}
	if h.Get("Accept-Ranges") != "bytes" || h.Get("ETag") == "" {
		t.Errorf("missing Accept-Ranges/ETag on a 206: %v", h)
	}
}

func TestRangedGet_OpenEndedRange(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=15-")
	expect(t, r, 206)
	if r.Body.String() != rangeBody[15:] {
		t.Errorf("body = %q, want %q", r.Body.String(), rangeBody[15:])
	}
	if got := r.Header().Get("Content-Range"); got != "bytes 15-19/20" {
		t.Errorf("Content-Range = %q, want bytes 15-19/20", got)
	}
}

func TestRangedGet_SuffixRange(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=-5")
	expect(t, r, 206)
	if want := rangeBody[len(rangeBody)-5:]; r.Body.String() != want {
		t.Errorf("body = %q, want %q", r.Body.String(), want)
	}
	if got := r.Header().Get("Content-Range"); got != "bytes 15-19/20" {
		t.Errorf("Content-Range = %q, want bytes 15-19/20", got)
	}
}

func TestRangedGet_SuffixRangeLongerThanObjectClampsToWhole(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=-1000")
	expect(t, r, 206)
	if r.Body.String() != rangeBody {
		t.Errorf("body = %q, want the whole object", r.Body.String())
	}
	if got := r.Header().Get("Content-Range"); got != "bytes 0-19/20" {
		t.Errorf("Content-Range = %q, want bytes 0-19/20", got)
	}
}

func TestRangedGet_EndPastSizeClampsToLastByte(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=10-1000")
	expect(t, r, 206)
	if r.Body.String() != rangeBody[10:] {
		t.Errorf("body = %q, want %q", r.Body.String(), rangeBody[10:])
	}
	if got := r.Header().Get("Content-Range"); got != "bytes 10-19/20" {
		t.Errorf("Content-Range = %q, want bytes 10-19/20", got)
	}
}

func TestRangedGet_UnsatisfiableRangeIs416(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=100-200")
	expect(t, r, 416)
	if got := r.Header().Get("Content-Range"); got != "bytes */20" {
		t.Errorf("Content-Range = %q, want bytes */20", got)
	}
}

func TestRangedGet_StartExactlyAtSizeIs416(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "GET", path, nil, "Range", "bytes=20-25")
	expect(t, r, 416)
}

// A Range header this package doesn't support (multiple ranges, or garbage) is ignored
// per RFC 9110 section 14.2: the server answers the whole object rather than reject it.
func TestRangedGet_UnsupportedOrMalformedRangeFallsBackToWholeObject(t *testing.T) {
	e, path := setupRangeObject(t)
	for _, header := range []string{
		"bytes=0-4,10-14", // multi-range: not supported
		"items=0-4",       // wrong unit
		"bytes=",          // empty spec
		"bytes=abc-def",   // not integers
		"bytes=5-2",       // end before start
		"nonsense",
	} {
		t.Run(header, func(t *testing.T) {
			r := e.do(t, carol, "GET", path, nil, "Range", header)
			expect(t, r, 200)
			if r.Body.String() != rangeBody {
				t.Errorf("body = %q, want the whole object", r.Body.String())
			}
			if r.Header().Get("Content-Range") != "" {
				t.Errorf("Content-Range = %q, want none on a fallback to the whole object", r.Header().Get("Content-Range"))
			}
		})
	}
}

func TestRangedGet_MissingObjectIs404(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "scratch")
	r := e.do(t, carol, "GET", "/api/backends/scratch/objects/nope.txt", nil, "Range", "bytes=0-4")
	expect(t, r, 404)
}

func TestHeadObject_MatchesGetMetadataWithNoBody(t *testing.T) {
	e, path := setupRangeObject(t)
	r := e.do(t, carol, "HEAD", path, nil)
	expect(t, r, 200)
	if r.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", r.Body.String())
	}
	h := r.Header()
	if h.Get("Content-Length") != "20" {
		t.Errorf("Content-Length = %q, want 20", h.Get("Content-Length"))
	}
	if h.Get("Accept-Ranges") != "bytes" || h.Get("ETag") == "" {
		t.Errorf("missing Accept-Ranges/ETag on HEAD: %v", h)
	}
	if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
		t.Errorf("Content-Disposition = %q", h.Get("Content-Disposition"))
	}
}

func TestHeadObject_MissingIs404(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "scratch")
	expect(t, e.do(t, carol, "HEAD", "/api/backends/scratch/objects/nope.txt", nil), 404)
}

// A viewer-capped app workload token (ADR 0117) never gets write access, but ranged
// reads and HEAD are ordinary CanRead operations open to every role, same as a plain GET.
func TestRangedGetAndHead_ViewerAllowed(t *testing.T) {
	e, path := setupRangeObject(t)
	expect(t, e.do(t, carol, "GET", path, nil, "Range", "bytes=0-3"), 206)
	expect(t, e.do(t, carol, "HEAD", path, nil), 200)
}
