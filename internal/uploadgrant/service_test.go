package uploadgrant

import (
	"errors"
	"testing"
)

func newTestService() *Service { return NewService(NewMemoryStore()) }

func TestService_CreateValidation(t *testing.T) {
	s := newTestService()
	cases := []struct {
		name  string
		in    CreateInput
		field string
	}{
		{"wrong workspace in principal", CreateInput{Principal: "streamlit:globex:app1", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 1}, "principal"},
		{"not an app principal", CreateInput{Principal: "alice", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 1}, "principal"},
		{"booth-api sidecar principal rejected here", CreateInput{Principal: "apiwrite:some-api", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 1}, "principal"},
		{"missing backend", CreateInput{Principal: "streamlit:acme:app1", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 1}, "backendId"},
		{"bad prefix traversal", CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "../x", Extensions: []string{"csv"}, MaxBytes: 1}, ""},
		{"no extensions", CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "a", MaxBytes: 1}, "extensions"},
		{"bad extension with embedded dot", CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "a", Extensions: []string{"c.sv"}, MaxBytes: 1}, ""},
		{"zero maxBytes", CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 0}, "maxBytes"},
		{"negative maxBytes", CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: -1}, "maxBytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Create(testCtx, "acme", "alice", tc.in)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v (%T), want *ValidationError", err, err)
			}
			if tc.field != "" && ve.Field != tc.field {
				t.Errorf("field = %q, want %q (message: %s)", ve.Field, tc.field, ve.Message)
			}
		})
	}
}

func TestService_CreateNormalizesExtensionsAndPrefix(t *testing.T) {
	s := newTestService()
	g, err := s.Create(testCtx, "acme", "alice", CreateInput{
		Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "incoming",
		Extensions: []string{"CSV", ".PNG", "csv"}, MaxBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Prefix != "incoming/" {
		t.Errorf("prefix = %q, want %q", g.Prefix, "incoming/")
	}
	if len(g.Extensions) != 2 || g.Extensions[0] != "csv" || g.Extensions[1] != "png" {
		t.Errorf("extensions = %v, want [csv png] (lowercased, no dot, deduplicated)", g.Extensions)
	}
	if g.GrantedBy != "alice" {
		t.Errorf("grantedBy = %q", g.GrantedBy)
	}
}

func TestService_CreateReplacesSameScope(t *testing.T) {
	s := newTestService()
	in := CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "incoming", Extensions: []string{"csv"}, MaxBytes: 1024}
	first, err := s.Create(testCtx, "acme", "alice", in)
	if err != nil {
		t.Fatal(err)
	}
	in.MaxBytes = 2048
	second, err := s.Create(testCtx, "acme", "bob", in)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Error("the replacement grant kept the old ID")
	}
	if _, err := s.Get(testCtx, "acme", first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the replaced grant should be gone: %v", err)
	}
	all, _ := s.List(testCtx, "acme", "streamlit:acme:app1")
	if len(all) != 1 || all[0].MaxBytes != 2048 || all[0].GrantedBy != "bob" {
		t.Errorf("after replace: %+v", all)
	}
}

func TestService_RevokePrincipalIsIdempotent(t *testing.T) {
	s := newTestService()
	if _, err := s.Create(testCtx, "acme", "alice", CreateInput{
		Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "a", Extensions: []string{"csv"}, MaxBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.RevokePrincipal(testCtx, "acme", "streamlit:acme:app1")
	if err != nil || len(revoked) != 1 {
		t.Fatalf("RevokePrincipal = %+v, %v; want 1", revoked, err)
	}
	again, err := s.RevokePrincipal(testCtx, "acme", "streamlit:acme:app1")
	if err != nil || len(again) != 0 {
		t.Errorf("second RevokePrincipal = %+v, %v; want 0, nil", again, err)
	}
}

// ---- admission (MatchForUpload) --------------------------------------------

func mustCreate(t *testing.T, s *Service, in CreateInput) Grant {
	t.Helper()
	g, err := s.Create(testCtx, "acme", "alice", in)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestMatchForUpload_ExactPrefixAdmits(t *testing.T) {
	s := newTestService()
	want := mustCreate(t, s, CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "incoming", Extensions: []string{"csv"}, MaxBytes: 1024})

	got, ok, err := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "incoming/file.csv")
	if err != nil || !ok || got.ID != want.ID {
		t.Fatalf("MatchForUpload = %+v, %v, %v; want the grant to admit", got, ok, err)
	}
}

func TestMatchForUpload_RootPrefixAdmitsOnlyTopLevel(t *testing.T) {
	s := newTestService()
	mustCreate(t, s, CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "", Extensions: []string{"csv"}, MaxBytes: 1024})

	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "file.csv"); !ok {
		t.Error("a root-prefix grant should admit a top-level path")
	}
	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "sub/file.csv"); ok {
		t.Error("a root-prefix grant must not admit one level deeper")
	}
}

func TestMatchForUpload_NoGrantRefuses(t *testing.T) {
	s := newTestService()
	if _, ok, err := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "incoming/f.csv"); ok || err != nil {
		t.Errorf("MatchForUpload with no grants = ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestMatchForUpload_NestedPathNeverMatchesShallowerGrant(t *testing.T) {
	s := newTestService()
	mustCreate(t, s, CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "incoming", Extensions: []string{"csv"}, MaxBytes: 1024})

	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "incoming/sub/f.csv"); ok {
		t.Error("a grant over incoming/ must not admit a write two segments below it")
	}
	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "lake", "other/f.csv"); ok {
		t.Error("a write outside the grant's prefix must not be admitted")
	}
}

func TestMatchForUpload_WrongBackendOrPrincipalRefuses(t *testing.T) {
	s := newTestService()
	mustCreate(t, s, CreateInput{Principal: "streamlit:acme:app1", BackendID: "lake", Prefix: "incoming", Extensions: []string{"csv"}, MaxBytes: 1024})

	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app1", "other-backend", "incoming/f.csv"); ok {
		t.Error("a grant on one backend must not admit a write to another")
	}
	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "streamlit:acme:app2", "lake", "incoming/f.csv"); ok {
		t.Error("a grant for one principal must not admit another's write")
	}
	// A human's token subject can never equal an app principal, so it can never match —
	// the real guard against this admission path being reachable by a person at all.
	if _, ok, _ := s.MatchForUpload(testCtx, "acme", "alice", "lake", "incoming/f.csv"); ok {
		t.Error("a human subject matched an app grant")
	}
}

func TestHasExtension(t *testing.T) {
	cases := []struct {
		name string
		exts []string
		want bool
	}{
		{"report.csv", []string{"csv", "png"}, true},
		{"REPORT.CSV", []string{"csv"}, true}, // case-insensitive
		{"report.CSV", []string{"csv"}, true},
		{"report.txt", []string{"csv"}, false},
		{"noext", []string{"csv"}, false},
		{"trailing.", []string{"csv"}, false},
		{".hidden", []string{"csv"}, false}, // "hidden" is not in the allowlist
	}
	for _, tc := range cases {
		if got := HasExtension(tc.name, tc.exts); got != tc.want {
			t.Errorf("HasExtension(%q, %v) = %v, want %v", tc.name, tc.exts, got, tc.want)
		}
	}
}
