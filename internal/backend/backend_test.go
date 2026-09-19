package backend

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanPath(t *testing.T) {
	valid := []string{"a", "a/b/c.txt", "with space.txt", "unicode-é-日本.txt", ".hidden", "a/.hidden/b", "a..b", "trailing-dots.."}
	for _, p := range valid {
		got, err := CleanPath(p)
		if err != nil || got != p {
			t.Errorf("CleanPath(%q) = %q, %v; want it accepted unchanged", p, got, err)
		}
	}

	invalid := map[string]string{
		"empty":              "",
		"parent":             "..",
		"parent prefix":      "../x",
		"parent middle":      "a/../b",
		"parent suffix":      "a/..",
		"current dir":        ".",
		"current dir middle": "a/./b",
		"empty segment":      "a//b",
		"leading slash":      "/a",
		"double leading":     "//a",
		"trailing slash":     "a/",
		"backslash":          `a\b`,
		"windows traversal":  `..\x`,
		"nul":                "a\x00b",
		"too long":           strings.Repeat("a", maxPathLen+1),
	}
	for name, p := range invalid {
		if got, err := CleanPath(p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%s: CleanPath(%q) = %q, %v; want ErrInvalidPath", name, p, got, err)
		}
	}

	// Exactly at the limit is fine.
	if _, err := CleanPath(strings.Repeat("a", maxPathLen)); err != nil {
		t.Errorf("path of exactly %d bytes rejected: %v", maxPathLen, err)
	}
}

func TestCleanPrefix(t *testing.T) {
	ok := map[string]string{"": "", "a": "a/", "a/": "a/", "a/b": "a/b/", "a/b/": "a/b/"}
	for in, want := range ok {
		got, err := CleanPrefix(in)
		if err != nil || got != want {
			t.Errorf("CleanPrefix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../", "a/../b", "/a", "//", "a//b", `a\b`, "a/./b"} {
		if _, err := CleanPrefix(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanPrefix(%q) error = %v, want ErrInvalidPath", in, err)
		}
	}
}

func TestListOptions_EffectiveLimit(t *testing.T) {
	cases := map[int]int{-5: DefaultListLimit, 0: DefaultListLimit, 1: 1, 50: 50, MaxListLimit: MaxListLimit, MaxListLimit + 1: MaxListLimit, 1 << 30: MaxListLimit}
	for in, want := range cases {
		if got := (ListOptions{Limit: in}).EffectiveLimit(); got != want {
			t.Errorf("EffectiveLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestKindValid(t *testing.T) {
	for _, k := range Kinds {
		if !k.Valid() {
			t.Errorf("%s reported invalid", k)
		}
	}
	for _, k := range []Kind{"", "ftp", "S3", "hdfs"} {
		if k.Valid() {
			t.Errorf("%q reported valid", k)
		}
	}
	if len(Kinds) != 4 {
		t.Errorf("ADR 0013 requires exactly four kinds at v0, have %d", len(Kinds))
	}
}
