package config

import (
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{
		"BOOTH_HTTP_ADDR", "BOOTH_OIDC_ISSUER_URL", "BOOTH_OIDC_CLIENT_ID", "BOOTH_OIDC_REQUIRE_AUDIENCE",
		"BOOTH_POSTGRES_DSN", "BOOTH_NAMESPACE", "BOOTH_STORAGE_FILESYSTEM_ROOTS",
		"BOOTH_STORAGE_MAX_UPLOAD_BYTES", "BOOTH_STORAGE_DEV_MEMORY", "BOOTH_OIDC_GROUPS_CLAIM",
		"BOOTH_WORKLOAD_ISSUER_URL",
	} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

var base = map[string]string{
	"BOOTH_OIDC_ISSUER_URL": "https://idp.example/realms/booth",
	"BOOTH_OIDC_CLIENT_ID":  "booth-storage",
	"BOOTH_POSTGRES_DSN":    "postgres://x",
}

func with(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, base)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Namespace != "booth-storage" || cfg.DevMemory || cfg.MaxUploadBytes != 0 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.FilesystemRoots) != 0 {
		t.Errorf("filesystem roots must default to none (kind disabled), got %v", cfg.FilesystemRoots)
	}
	if cfg.OIDC.RequireAudience {
		t.Error("audience requirement should default off, matching booth-core")
	}
	if cfg.OIDC.GroupsClaim != "groups" {
		t.Errorf("groups claim = %q, want booth-core's default \"groups\"", cfg.OIDC.GroupsClaim)
	}
	if cfg.OIDC.WorkloadIssuerURL != "" {
		t.Errorf("workload issuer = %q, want none by default (the IdP is the only trusted issuer)", cfg.OIDC.WorkloadIssuerURL)
	}
}

func TestLoad_WorkloadIssuer(t *testing.T) {
	setEnv(t, with(map[string]string{"BOOTH_WORKLOAD_ISSUER_URL": "http://booth-core.booth:8080"}))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.WorkloadIssuerURL != "http://booth-core.booth:8080" {
		t.Errorf("workload issuer = %q", cfg.OIDC.WorkloadIssuerURL)
	}
}

func TestLoad_Overrides(t *testing.T) {
	setEnv(t, with(map[string]string{
		"BOOTH_HTTP_ADDR":                ":9090",
		"BOOTH_NAMESPACE":                "storage-ns",
		"BOOTH_OIDC_REQUIRE_AUDIENCE":    "true",
		"BOOTH_STORAGE_FILESYSTEM_ROOTS": " /data/{workspace} , C:\\shared ,, ",
		"BOOTH_STORAGE_MAX_UPLOAD_BYTES": "1048576",
		"BOOTH_OIDC_GROUPS_CLAIM":        "memberships",
	}))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.Namespace != "storage-ns" || !cfg.OIDC.RequireAudience || cfg.MaxUploadBytes != 1048576 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.OIDC.GroupsClaim != "memberships" {
		t.Errorf("groups claim override not applied: %q", cfg.OIDC.GroupsClaim)
	}
	if strings.Join(cfg.FilesystemRoots, "|") != `/data/{workspace}|C:\shared` {
		t.Errorf("roots = %q; want trimmed, empties dropped, Windows drive colon preserved", cfg.FilesystemRoots)
	}
}

func TestLoad_RequiredSettings(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no issuer", map[string]string{"BOOTH_OIDC_CLIENT_ID": "c", "BOOTH_POSTGRES_DSN": "x"}, "BOOTH_OIDC_ISSUER_URL"},
		{"no client id", map[string]string{"BOOTH_OIDC_ISSUER_URL": "i", "BOOTH_POSTGRES_DSN": "x"}, "BOOTH_OIDC_CLIENT_ID"},
		{"no database and not dev mode", map[string]string{"BOOTH_OIDC_ISSUER_URL": "i", "BOOTH_OIDC_CLIENT_ID": "c"}, "BOOTH_POSTGRES_DSN"},
		{"bad upload cap", with(map[string]string{"BOOTH_STORAGE_MAX_UPLOAD_BYTES": "lots"}), "BOOTH_STORAGE_MAX_UPLOAD_BYTES"},
		{"negative upload cap", with(map[string]string{"BOOTH_STORAGE_MAX_UPLOAD_BYTES": "-1"}), "BOOTH_STORAGE_MAX_UPLOAD_BYTES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want mention of %s", err, tc.want)
			}
		})
	}
}

// Dev mode is the only way to run without a database, and must be an explicit opt-in.
func TestLoad_DevMemoryRelaxesOnlyThePostgresRequirement(t *testing.T) {
	setEnv(t, map[string]string{"BOOTH_OIDC_ISSUER_URL": "i", "BOOTH_OIDC_CLIENT_ID": "c", "BOOTH_STORAGE_DEV_MEMORY": "true"})
	cfg, err := Load()
	if err != nil || !cfg.DevMemory {
		t.Fatalf("dev memory mode rejected: %v", err)
	}

	setEnv(t, map[string]string{"BOOTH_OIDC_CLIENT_ID": "c", "BOOTH_STORAGE_DEV_MEMORY": "true"})
	if _, err := Load(); err == nil {
		t.Error("dev mode must not waive the OIDC requirement")
	}
}
