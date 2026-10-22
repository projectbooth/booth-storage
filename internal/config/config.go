// Package config loads booth-storage's runtime configuration from environment
// variables. Every value maps 1:1 to a Helm chart value/env var, mirroring booth-core's
// and booth-module-store's own internal/config — there is no config file format of our
// own to version.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/projectbooth/booth-storage/internal/auth"
)

// Config is booth-storage's full runtime configuration.
type Config struct {
	// HTTPAddr is the address the HTTP server listens on.
	HTTPAddr string

	// OIDC is the identity-provider configuration used to independently re-verify a
	// forwarded bearer token (core-platform-api.md's defense-in-depth requirement).
	OIDC auth.OIDCConfig

	// PostgresDSN is the connection string for this module's own database on the shared
	// PostgreSQL cluster (ADR 0014), holding backend metadata (never credentials).
	PostgresDSN string

	// Namespace is the Kubernetes namespace credential Secrets are written to (ADR
	// 0020) — this module's own namespace, populated from the pod's downward API.
	Namespace string

	// FilesystemRoots is the operator's allow-list of server directories a workspace
	// may register as a filesystem backend. A root containing "{workspace}" expands
	// per workspace. Empty (the default) disables the filesystem kind entirely.
	FilesystemRoots []string

	// MaxUploadBytes caps a single object write; 0 means unlimited.
	MaxUploadBytes int64

	// DevMemory swaps PostgreSQL and Kubernetes Secrets for in-process memory stores,
	// so the module can run on a laptop with no cluster or database. State and
	// credentials vanish on restart: it exists for local development only.
	DevMemory bool

	// CredentialBrokerCredential is this module's own copy of the shared secret
	// booth-core presents when calling the ADR 0080 credential-broker provider endpoint
	// (contracts/credential-broker.md, ADR 0088) — the booth-credential-broker-provider-
	// credentials Secret's `credential` key, delivered once the chart declares
	// `providesCredentials: {kinds: ["s3"]}`. Empty disables the route: every call is
	// refused rather than trusted on an unconfigured, always-true comparison.
	CredentialBrokerCredential string

	// NATSURL is a direct, unauthenticated event-bus URL — a local test stand-in, or
	// eventBus.credentialsSecret.enabled: false. Never the real platform bus, which
	// always authenticates (see EventBusDir). Takes precedence over EventBusDir if both
	// are somehow set.
	NATSURL string

	// EventBusDir is the directory booth-core's event-bus credentials Secret is mounted
	// under (ADR 0050's booth-event-bus-credentials, mounted optional — see
	// charts/booth-storage/values.yaml's eventBus block), holding "url" and
	// "nats.creds". Empty disables publishing until BOOTH_STORAGE_DEV_MEMORY or the
	// chart turns eventBus off entirely. Unlike CredentialBrokerCredential, there is a
	// meaningful "unconfigured (yet), so skip for now" state here: nothing downstream
	// depends on this event for correctness (ADR 0123 point 6 names a slower
	// cron/interval trigger as the safety net for a missed one), so
	// internal/eventbus.LazyPublisher polls this directory and connects lazily rather
	// than this module gating its own startup on it, the way booth-api's own ADR 0123
	// wiring of the same Secret does.
	EventBusDir string
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:                   getEnv("BOOTH_HTTP_ADDR", ":8080"),
		PostgresDSN:                os.Getenv("BOOTH_POSTGRES_DSN"),
		Namespace:                  getEnv("BOOTH_NAMESPACE", "booth-storage"),
		FilesystemRoots:            splitNonEmpty(os.Getenv("BOOTH_STORAGE_FILESYSTEM_ROOTS")),
		DevMemory:                  os.Getenv("BOOTH_STORAGE_DEV_MEMORY") == "true",
		CredentialBrokerCredential: os.Getenv("BOOTH_CREDENTIAL_BROKER_CREDENTIAL"),
		NATSURL:                    os.Getenv("BOOTH_NATS_URL"),
		EventBusDir:                os.Getenv("BOOTH_EVENT_BUS_DIR"),
		OIDC: auth.OIDCConfig{
			IssuerURL:       os.Getenv("BOOTH_OIDC_ISSUER_URL"),
			ClientID:        os.Getenv("BOOTH_OIDC_CLIENT_ID"),
			RequireAudience: os.Getenv("BOOTH_OIDC_REQUIRE_AUDIENCE") == "true",
			GroupsClaim:     getEnv("BOOTH_OIDC_GROUPS_CLAIM", auth.DefaultGroupsClaim),
			// Optional: booth-core's issuer URL, to also accept its workload tokens (ADR 0056).
			WorkloadIssuerURL: os.Getenv("BOOTH_WORKLOAD_ISSUER_URL"),
			// Optional key-fetch override (ADR 0108): see auth.OIDCConfig.JWKSURL's doc comment.
			JWKSURL: os.Getenv("BOOTH_OIDC_JWKS_URL"),
		},
	}

	if v := os.Getenv("BOOTH_STORAGE_MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("BOOTH_STORAGE_MAX_UPLOAD_BYTES must be a non-negative integer, got %q", v)
		}
		cfg.MaxUploadBytes = n
	}

	if cfg.OIDC.IssuerURL == "" {
		return Config{}, fmt.Errorf("BOOTH_OIDC_ISSUER_URL is required")
	}
	if cfg.OIDC.ClientID == "" {
		return Config{}, fmt.Errorf("BOOTH_OIDC_CLIENT_ID is required")
	}
	if !cfg.DevMemory && cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_POSTGRES_DSN is required (set BOOTH_STORAGE_DEV_MEMORY=true only for local development)")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitNonEmpty splits a comma-separated list. Comma rather than the OS path-list
// separator because ':' appears inside Windows drive paths.
func splitNonEmpty(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
