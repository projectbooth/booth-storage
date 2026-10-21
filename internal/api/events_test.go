package api

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ADR 0123: storage.object.created is published after a successful write, and only
// after one.

type fakePublish struct {
	Workspace, EventType string
	Data                 map[string]any
}

type fakePublisher struct {
	mu    sync.Mutex
	calls []fakePublish
	err   error
}

func (f *fakePublisher) Publish(_ context.Context, workspace, eventType string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, _ := data.(map[string]any)
	f.calls = append(f.calls, fakePublish{workspace, eventType, d})
	return f.err
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePublisher) last(t *testing.T) fakePublish {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no publish call was recorded")
	}
	return f.calls[len(f.calls)-1]
}

func TestObjectCreatedPublished_EditorWrite(t *testing.T) {
	pub := &fakePublisher{}
	e := newTestEnv(t, func(d *Deps) { d.Events = pub })
	e.registerFS(t, "acme", "lake")

	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/report.csv", "a,b\n1,2\n"), 200)

	if pub.count() != 1 {
		t.Fatalf("publish called %d times, want 1", pub.count())
	}
	got := pub.last(t)
	if got.Workspace != "acme" || got.EventType != "storage.object.created" {
		t.Errorf("envelope = %+v", got)
	}
	if got.Data["backendId"] != "lake" || got.Data["path"] != "report.csv" {
		t.Errorf("data = %+v", got.Data)
	}
	if got.Data["principal"] != "bob" {
		t.Errorf("principal = %v, want bob", got.Data["principal"])
	}
	if got.Data["via"] != "editor" {
		t.Errorf("via = %v, want editor", got.Data["via"])
	}
	if got.Data["size"] != int64(len("a,b\n1,2\n")) {
		t.Errorf("size = %v, want %d", got.Data["size"], len("a,b\n1,2\n"))
	}
}

func TestObjectCreatedPublished_AppUploadShapeAndVia(t *testing.T) {
	pub := &fakePublisher{}
	e := newTestEnv(t, withAppTokens, func(d *Deps) { d.Events = pub })
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	expect(t, e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "a,b\n1,2\n", "If-None-Match", "*"), 200)

	if pub.count() != 1 {
		t.Fatalf("publish called %d times, want 1", pub.count())
	}
	got := pub.last(t)
	if got.Data["via"] != "app-upload" {
		t.Errorf("via = %v, want app-upload", got.Data["via"])
	}
	if got.Data["principal"] != "streamlit:acme:app1" {
		t.Errorf("principal = %v, want streamlit:acme:app1", got.Data["principal"])
	}
	if got.Data["path"] != "incoming/report.csv" {
		t.Errorf("path = %v, want incoming/report.csv", got.Data["path"])
	}
}

func TestObjectCreatedNotPublished_OnPreconditionFailure(t *testing.T) {
	pub := &fakePublisher{}
	e := newTestEnv(t, func(d *Deps) { d.Events = pub })
	e.registerFS(t, "acme", "lake")

	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "first", "If-None-Match", "*"), 200)
	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "second", "If-None-Match", "*"), 412)

	if pub.count() != 1 {
		t.Fatalf("publish called %d times, want exactly 1 (not for the 412)", pub.count())
	}
}

func TestObjectCreatedNotPublished_OnForbidden(t *testing.T) {
	pub := &fakePublisher{}
	e := newTestEnv(t, func(d *Deps) { d.Events = pub })
	e.registerFS(t, "acme", "lake")

	expect(t, e.do(t, carol, "PUT", "/api/backends/lake/objects/a.txt", "x"), 403)

	if pub.count() != 0 {
		t.Fatalf("publish called %d times, want 0 for a 403", pub.count())
	}
}

func TestObjectCreatedPublishFailure_DoesNotAffectTheResponse(t *testing.T) {
	pub := &fakePublisher{err: errors.New("bus unreachable")}
	e := newTestEnv(t, func(d *Deps) { d.Events = pub })
	e.registerFS(t, "acme", "lake")

	// A failing publish must not change the write's own outcome (ADR 0123 point 6: a
	// missed event is the pipeline trigger's problem to cover with a safety net, never
	// this module's write path's problem).
	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "x"), 200)
	if pub.count() != 1 {
		t.Fatalf("publish called %d times, want 1 (still attempted, even though it errors)", pub.count())
	}
}
