// Package contract validates booth-storage's own manifest — the BoothModule custom
// resource its Helm chart templates — against contracts/module-manifest.md's schema, and
// checks the chart's RBAC stays as narrow as ADR 0020 needs. Per
// contracts/testing-strategy.md this runs against a rendered template (via `helm
// template`), not a deployed cluster: no cluster needed, but a `helm` binary is, which
// CI's ci.yml sets up before running this alongside the rest of the unit/contract layer.
package contract

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

type boothModule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ID                string   `yaml:"id"`
		DisplayName       string   `yaml:"displayName"`
		Icon              string   `yaml:"icon"`
		Version           string   `yaml:"version"`
		ContractVersion   string   `yaml:"contractVersion"`
		HasOwnUI          bool     `yaml:"hasOwnUi"`
		UIIntegrationMode string   `yaml:"uiIntegrationMode"`
		HealthCheckPath   string   `yaml:"healthCheckPath"`
		RequiredScopes    []string `yaml:"requiredScopes"`
		NavGroup          string   `yaml:"navGroup"`
		NavPath           string   `yaml:"navPath"`
		AdminNavPath      string   `yaml:"adminNavPath"`
		Database          *struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"database"`
		ServiceRef struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"serviceRef"`
	} `yaml:"spec"`
}

var requiredValues = []string{
	"--set", "oidc.issuerUrl=https://keycloak.example.com/realms/booth",
	"--set", "oidc.clientId=booth-storage",
}

func helmTemplate(t *testing.T, showOnly string, extra ...string) []byte {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; this contract test runs in CI where it is (see .github/workflows/ci.yml)")
	}

	chartDir := filepath.Join("..", "..", "charts", "booth-storage")
	args := []string{"template", "storage-contract-test", chartDir, "--namespace", "booth-storage"}
	args = append(args, requiredValues...)
	args = append(args, extra...)
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

func renderBoothModule(t *testing.T) boothModule {
	t.Helper()
	var m boothModule
	out := helmTemplate(t, "templates/boothmodule.yaml")
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsing rendered BoothModule: %v\nrendered:\n%s", err, out)
	}
	return m
}

// TestManifest_RequiredFields checks every field contracts/module-manifest.md marks
// required is actually populated in our rendered manifest.
func TestManifest_RequiredFields(t *testing.T) {
	m := renderBoothModule(t)

	if m.APIVersion != "booth.projectbooth.io/v1alpha1" || m.Kind != "BoothModule" {
		t.Errorf("apiVersion/kind = %s/%s, want booth.projectbooth.io/v1alpha1 BoothModule (ADR 0019)", m.APIVersion, m.Kind)
	}
	// The manifest contract: id "matches the repo name minus booth-".
	if m.Spec.ID != "storage" {
		t.Errorf("spec.id = %q, want storage", m.Spec.ID)
	}
	if m.Spec.DisplayName == "" {
		t.Error("spec.displayName is required but empty")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(m.Spec.Version) {
		t.Errorf("spec.version = %q, want semver", m.Spec.Version)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(m.Spec.ContractVersion) {
		t.Errorf("spec.contractVersion = %q, want semver", m.Spec.ContractVersion)
	}
	if m.Spec.HealthCheckPath == "" || m.Spec.HealthCheckPath[0] != '/' {
		t.Errorf("spec.healthCheckPath = %q, want a URL path", m.Spec.HealthCheckPath)
	}
	if m.Spec.ServiceRef.Name == "" || m.Spec.ServiceRef.Port == 0 {
		t.Errorf("spec.serviceRef is required (name+port) but got %+v", m.Spec.ServiceRef)
	}
}

// TestManifest_UI checks the "required if hasOwnUi" rules (contracts/module-manifest.md)
// and the nav placement this module's brief and ADRs 0017/0023/0036 call for.
func TestManifest_UI(t *testing.T) {
	m := renderBoothModule(t)

	if !m.Spec.HasOwnUI {
		t.Fatal("spec.hasOwnUi = false, want true (booth-storage ships regular and admin views)")
	}
	if m.Spec.UIIntegrationMode != "native" {
		t.Errorf("spec.uiIntegrationMode = %q, want native (ui-integration.md lists booth-storage's config UI as a native-mode default; ADR 0030)", m.Spec.UIIntegrationMode)
	}
	if m.Spec.NavGroup != "manage" {
		t.Errorf("spec.navGroup = %q, want manage (agent brief; ADR 0017)", m.Spec.NavGroup)
	}
	if m.Spec.NavPath == "" {
		t.Error("spec.navPath is required when hasOwnUi is true")
	}
}

// TestManifest_DistinctAdminView is ADR 0036: booth-storage declares a separate admin
// route, distinct from the regular one, so the shell can gate it by workspace role.
func TestManifest_DistinctAdminView(t *testing.T) {
	m := renderBoothModule(t)

	if m.Spec.AdminNavPath == "" {
		t.Fatal("spec.adminNavPath is empty: ADR 0036 requires a distinct admin view for backend/credential management")
	}
	if m.Spec.AdminNavPath == m.Spec.NavPath {
		t.Errorf("adminNavPath equals navPath (%q); the admin view must be a distinct route", m.Spec.NavPath)
	}
	if m.Spec.AdminNavPath[0] != '/' {
		t.Errorf("adminNavPath = %q, want a path starting with /", m.Spec.AdminNavPath)
	}
}

// TestManifest_HealthPathMatchesProbe guards a subtle drift: the path core polls and the
// path the pod's readiness probe hits should be the same real-readiness endpoint.
func TestManifest_HealthPathMatchesProbe(t *testing.T) {
	m := renderBoothModule(t)
	dep := helmTemplate(t, "templates/deployment.yaml")
	if !regexp.MustCompile(`readinessProbe:\s+httpGet:\s+path: ` + regexp.QuoteMeta(m.Spec.HealthCheckPath) + `\b`).Match(dep) {
		t.Errorf("readinessProbe does not use the manifest's healthCheckPath %q:\n%s", m.Spec.HealthCheckPath, dep)
	}
	if bytes.Contains(dep, []byte("livenessProbe:\n            httpGet:\n              path: "+m.Spec.HealthCheckPath+"\n")) {
		t.Error("livenessProbe uses the database-aware health path; restarting the pod can't fix a database outage")
	}
}

type role struct {
	Kind  string `yaml:"kind"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// TestRBAC_NarrowlyScoped guards ADR 0020's trust boundary: booth-storage may manage
// Secrets in its own namespace and nothing else — no ClusterRole, no wildcard, and no
// list/watch (it only ever addresses a Secret by its derived name).
func TestRBAC_NarrowlyScoped(t *testing.T) {
	out := helmTemplate(t, "templates/rbac.yaml")

	dec := yaml.NewDecoder(bytes.NewReader(out))
	var roles []role
	for {
		var r role
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("parsing rendered rbac: %v", err)
		}
		if r.Kind == "Role" || r.Kind == "ClusterRole" {
			roles = append(roles, r)
		}
	}

	if len(roles) != 1 || roles[0].Kind != "Role" {
		t.Fatalf("want exactly one namespaced Role and no ClusterRole, got %+v", roles)
	}
	if len(roles[0].Rules) != 1 {
		t.Fatalf("Role has %d rules, want exactly 1 (secrets)", len(roles[0].Rules))
	}
	rule := roles[0].Rules[0]
	if len(rule.Resources) != 1 || rule.Resources[0] != "secrets" {
		t.Errorf("Role grants %v, want only secrets", rule.Resources)
	}
	want := map[string]bool{"get": true, "create": true, "update": true, "delete": true}
	for _, v := range rule.Verbs {
		if !want[v] {
			t.Errorf("Role grants verb %q; only get/create/update/delete are needed", v)
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Errorf("Role is missing verbs %v", want)
	}
}

// TestManifest_DeclaresDatabase is ADR 0053/0054: booth-storage asks booth-core to provision its
// PostgreSQL database and role, rather than expecting an operator-supplied DSN to show up some
// other way. Omitting the field means core provisions nothing at all.
func TestManifest_DeclaresDatabase(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Database == nil || !m.Spec.Database.Enabled {
		t.Fatalf("spec.database = %+v, want {enabled: true} (module-manifest.md, ADR 0053)", m.Spec.Database)
	}

	// ...and the pod reads exactly the Secret core delivers: name and `dsn` key per
	// core-platform-api.md's "Shared PostgreSQL" section.
	dep := helmTemplate(t, "templates/deployment.yaml")
	if !regexp.MustCompile(`secretKeyRef:\s+name: booth-database-credentials\s+key: dsn`).Match(dep) {
		t.Errorf("BOOTH_POSTGRES_DSN is not read from booth-database-credentials/dsn:\n%s", dep)
	}
}

// TestChart_OwnDatabaseMode: an operator who brings their own database turns core's
// provisioning off. Then the manifest must NOT ask for one, and the Secret has to be named —
// rendering without it must fail loudly rather than produce a pod stuck waiting on a Secret
// nobody is going to create.
func TestChart_OwnDatabaseMode(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	chartDir := filepath.Join("..", "..", "charts", "booth-storage")
	base := append([]string{"template", "x", chartDir}, requiredValues...)

	out, err := exec.Command("helm", append(base, "--set", "postgres.provisionedByCore=false")...).CombinedOutput()
	if err == nil {
		t.Fatalf("chart rendered with provisionedByCore=false and no Secret named:\n%s", out)
	}
	if !bytes.Contains(out, []byte("postgres.dsnSecret.name is required")) {
		t.Errorf("failure message not actionable:\n%s", out)
	}

	own := helmTemplate(t, "", "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db", "--set", "postgres.dsnSecret.key=url")
	if !regexp.MustCompile(`secretKeyRef:\s+name: my-db\s+key: url`).Match(own) {
		t.Errorf("own-database Secret not used:\n%s", own)
	}
	var m boothModule
	if err := yaml.Unmarshal(helmTemplate(t, "templates/boothmodule.yaml", "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db"), &m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Database != nil {
		t.Errorf("manifest still requests a core-provisioned database in own-database mode: %+v", m.Spec.Database)
	}
}

// TestChart_FilesystemDisabledByDefault: the filesystem kind must be opt-in — no roots
// env var is rendered unless the operator configures some.
func TestChart_FilesystemDisabledByDefault(t *testing.T) {
	if bytes.Contains(helmTemplate(t, "templates/deployment.yaml"), []byte("BOOTH_STORAGE_FILESYSTEM_ROOTS")) {
		t.Error("BOOTH_STORAGE_FILESYSTEM_ROOTS rendered with no roots configured")
	}
	with := helmTemplate(t, "templates/deployment.yaml", "--set", "filesystem.roots[0]=/data/{workspace}")
	if !bytes.Contains(with, []byte(`value: "/data/{workspace}"`)) {
		t.Errorf("configured root not rendered:\n%s", with)
	}
}

// The module re-derives roles from the token's groups claim (decision 0004), so the claim
// name must reach the pod and default to booth-core's.
func TestChart_PassesGroupsClaim(t *testing.T) {
	if !bytes.Contains(helmTemplate(t, "templates/deployment.yaml"), []byte("BOOTH_OIDC_GROUPS_CLAIM")) {
		t.Fatal("BOOTH_OIDC_GROUPS_CLAIM not rendered")
	}
	if !regexp.MustCompile(`BOOTH_OIDC_GROUPS_CLAIM\s+value: "groups"`).Match(helmTemplate(t, "templates/deployment.yaml")) {
		t.Error("default groups claim should be \"groups\", matching booth-core")
	}
	if !bytes.Contains(helmTemplate(t, "templates/deployment.yaml", "--set", "oidc.groupsClaim=memberships"), []byte(`value: "memberships"`)) {
		t.Error("oidc.groupsClaim override not rendered")
	}
}
