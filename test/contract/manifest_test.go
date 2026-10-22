// Package contract validates booth-storage's own manifest — the BoothModule custom
// resource its Helm chart templates — against contracts/module-manifest.md's schema, and
// checks the chart's RBAC stays as narrow as ADR 0020 needs. Per
// contracts/testing-strategy.md this runs against a rendered template (via `helm
// template`), not a deployed cluster: no cluster needed, but a `helm` binary is, which
// CI's ci.yml sets up before running this alongside the rest of the unit/contract layer.
package contract

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
		ProvidesCredentials *struct {
			Kinds []string `yaml:"kinds"`
		} `yaml:"providesCredentials"`
		Events *struct {
			Publish []string `yaml:"publish"`
		} `yaml:"events"`
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

// Trusting booth-core as a second token issuer (ADR 0056) is opt-in: nothing is rendered
// unless the operator names core's issuer URL.
func TestChart_WorkloadIssuer(t *testing.T) {
	if bytes.Contains(helmTemplate(t, "templates/deployment.yaml"), []byte("BOOTH_WORKLOAD_ISSUER_URL")) {
		t.Error("BOOTH_WORKLOAD_ISSUER_URL rendered by default; the IdP should be the only trusted issuer")
	}
	got := helmTemplate(t, "templates/deployment.yaml", "--set", "oidc.workloadIssuerUrl=http://booth-core.booth:8080")
	if !regexp.MustCompile(`BOOTH_WORKLOAD_ISSUER_URL\s+value: "http://booth-core.booth:8080"`).Match(got) {
		t.Errorf("oidc.workloadIssuerUrl not rendered:\n%s", got)
	}
}

// ADR 0123/0050: the manifest declares events.publish whenever eventBus.enabled (the
// default), regardless of how the credential is sourced, matching booth-api's own
// `events: {publish: [api.write.committed]}`.
func TestChart_EventsManifest(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Events == nil || len(m.Spec.Events.Publish) != 1 || m.Spec.Events.Publish[0] != "storage.object.created" {
		t.Errorf("spec.events = %+v, want publish: [storage.object.created] by default", m.Spec.Events)
	}

	out := helmTemplate(t, "templates/boothmodule.yaml", "--set", "eventBus.enabled=false")
	var disabled boothModule
	if err := yaml.Unmarshal(out, &disabled); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if disabled.Spec.Events != nil {
		t.Errorf("spec.events = %+v, want nil when eventBus.enabled=false", disabled.Spec.Events)
	}
}

// ADR 0123/0050: by default, this module mounts booth-core's event-bus credentials
// Secret optionally and connects lazily (internal/eventbus.LazyPublisher) — it must
// never gate startup the way booth-api's own wiring of the same Secret does.
func TestChart_EventBusCredentialsSecretMountIsOptional(t *testing.T) {
	got := helmTemplate(t, "templates/deployment.yaml")
	if !bytes.Contains(got, []byte("BOOTH_EVENT_BUS_DIR")) {
		t.Fatal("BOOTH_EVENT_BUS_DIR not rendered by default")
	}
	if !bytes.Contains(got, []byte("optional: true")) {
		t.Error("the event-bus-credentials Secret volume must be mounted optional: true")
	}
	if !regexp.MustCompile(`secretName:\s*booth-event-bus-credentials`).Match(got) {
		t.Errorf("secretName not rendered:\n%s", got)
	}
	if bytes.Contains(got, []byte("BOOTH_NATS_URL")) {
		t.Error("BOOTH_NATS_URL should not be rendered when sourcing from the credentials Secret (the default)")
	}
}

// Disabling the credentials Secret falls back to a direct, required URL — for a bus
// with authentication off, matching booth-api's own escape hatch.
func TestChart_EventBusCredentialsSecretDisabledUsesDirectURL(t *testing.T) {
	got := helmTemplate(t, "templates/deployment.yaml", "--set", "eventBus.credentialsSecret.enabled=false", "--set", "nats.url=nats://test-bus:4222")
	if !regexp.MustCompile(`BOOTH_NATS_URL\s+value: "nats://test-bus:4222"`).Match(got) {
		t.Errorf("nats.url override not rendered:\n%s", got)
	}
	if bytes.Contains(got, []byte("BOOTH_EVENT_BUS_DIR")) || bytes.Contains(got, []byte("event-bus-credentials")) {
		t.Error("the credentials Secret mount should not be rendered when credentialsSecret.enabled=false")
	}
}

// eventBus.enabled=false must leave no trace: no manifest declaration, no env var, no mount.
func TestChart_EventBusDisabled(t *testing.T) {
	got := helmTemplate(t, "templates/deployment.yaml", "--set", "eventBus.enabled=false")
	for _, s := range []string{"BOOTH_EVENT_BUS_DIR", "BOOTH_NATS_URL", "event-bus-credentials"} {
		if bytes.Contains(got, []byte(s)) {
			t.Errorf("%s rendered with eventBus.enabled=false; want no trace", s)
		}
	}
}

// The ADR 0108 key-fetch override is unconditionally rendered (like groupsClaim), empty by
// default so ordinary discovery is unchanged, overridable per deployment.
func TestChart_JWKSURL(t *testing.T) {
	if !bytes.Contains(helmTemplate(t, "templates/deployment.yaml"), []byte("BOOTH_OIDC_JWKS_URL")) {
		t.Fatal("BOOTH_OIDC_JWKS_URL not rendered")
	}
	if !regexp.MustCompile(`BOOTH_OIDC_JWKS_URL\s+value: ""`).Match(helmTemplate(t, "templates/deployment.yaml")) {
		t.Error("default jwksUrl should be empty (ordinary discovery, unchanged)")
	}
	got := helmTemplate(t, "templates/deployment.yaml", "--set", "oidc.jwksUrl=http://keycloak.booth.svc:8080/realms/booth/protocol/openid-connect/certs")
	if !bytes.Contains(got, []byte(`value: "http://keycloak.booth.svc:8080/realms/booth/protocol/openid-connect/certs"`)) {
		t.Errorf("oidc.jwksUrl override not rendered:\n%s", got)
	}
}

// Providing the ADR 0080/0088 credential broker is opt-in (docs/decisions/0006): neither the
// manifest field nor the Secret-sourced env var appears unless the operator turns it on.
func TestChart_CredentialBrokerDisabledByDefault(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.ProvidesCredentials != nil {
		t.Errorf("spec.providesCredentials = %+v, want nil by default", m.Spec.ProvidesCredentials)
	}
	if bytes.Contains(helmTemplate(t, "templates/deployment.yaml"), []byte("BOOTH_CREDENTIAL_BROKER_CREDENTIAL")) {
		t.Error("BOOTH_CREDENTIAL_BROKER_CREDENTIAL rendered by default; this is a new privileged capability that must be opted into")
	}
}

func TestChart_CredentialBrokerEnabled(t *testing.T) {
	var m boothModule
	if err := yaml.Unmarshal(helmTemplate(t, "templates/boothmodule.yaml", "--set", "credentialBroker.enabled=true"), &m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.ProvidesCredentials == nil || len(m.Spec.ProvidesCredentials.Kinds) != 1 || m.Spec.ProvidesCredentials.Kinds[0] != "s3" {
		t.Fatalf("spec.providesCredentials = %+v, want {kinds: [s3]}", m.Spec.ProvidesCredentials)
	}

	dep := helmTemplate(t, "templates/deployment.yaml", "--set", "credentialBroker.enabled=true")
	if !regexp.MustCompile(`secretKeyRef:\s+name: booth-credential-broker-provider-credentials\s+key: credential`).Match(dep) {
		t.Errorf("BOOTH_CREDENTIAL_BROKER_CREDENTIAL is not read from booth-credential-broker-provider-credentials/credential:\n%s", dep)
	}
}

// valueAtPath walks a dotted values.yaml path (e.g. "oidc.jwksUrl") through a generic
// YAML tree and reports whether it resolves to a leaf (which is all core's merge needs:
// a default to be absent-or-present under, per ADR 0113 item 8).
func valueAtPath(root map[string]any, dotted string) (any, bool) {
	cur := any(root)
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestChart_IdentityKeysAnnotation is the ADR 0113 / module-manifest.md "Chart
// annotation: identity keys" contract check: the annotation must be valid JSON, and
// every path it names must actually exist (with a default) in values.yaml, or core's
// install-time merge (and a plain `helm template`/CLI install) would silently do nothing
// for a typo'd path.
func TestChart_IdentityKeysAnnotation(t *testing.T) {
	chartBytes, err := os.ReadFile(filepath.Join("..", "..", "charts", "booth-storage", "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	if err := yaml.Unmarshal(chartBytes, &chart); err != nil {
		t.Fatalf("parsing Chart.yaml: %v", err)
	}
	raw, ok := chart.Annotations["booth.projectbooth.io/identity-keys"]
	if !ok {
		t.Fatal("Chart.yaml is missing the booth.projectbooth.io/identity-keys annotation (ADR 0113)")
	}

	var mapping map[string]string
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		t.Fatalf("booth.projectbooth.io/identity-keys is not valid JSON: %v\nvalue: %s", err, raw)
	}
	if len(mapping) == 0 {
		t.Fatal("booth.projectbooth.io/identity-keys decoded to an empty object")
	}

	knownKeys := map[string]bool{
		"issuerUrl": true, "clientId": true, "jwksUrl": true,
		"workloadIssuerUrl": true, "groupsClaim": true, "iframeIssuerUrl": true,
	}

	valuesBytes, err := os.ReadFile(filepath.Join("..", "..", "charts", "booth-storage", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
		t.Fatalf("parsing values.yaml: %v", err)
	}

	for key, path := range mapping {
		if !knownKeys[key] {
			t.Errorf("identity-keys annotation names unknown shared setting %q (want one of issuerUrl, clientId, jwksUrl, workloadIssuerUrl, groupsClaim, iframeIssuerUrl)", key)
		}
		if _, ok := valueAtPath(values, path); !ok {
			t.Errorf("identity-keys annotation maps %q to values.yaml path %q, which has no default in values.yaml", key, path)
		}
	}

	for _, want := range []struct{ sharedKey, path string }{
		{"issuerUrl", "oidc.issuerUrl"},
		{"clientId", "oidc.clientId"},
		{"jwksUrl", "oidc.jwksUrl"},
		{"workloadIssuerUrl", "oidc.workloadIssuerUrl"},
		{"groupsClaim", "oidc.groupsClaim"},
	} {
		if got := mapping[want.sharedKey]; got != want.path {
			t.Errorf("identity-keys[%q] = %q, want %q", want.sharedKey, got, want.path)
		}
	}
}
