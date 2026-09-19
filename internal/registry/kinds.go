package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/azure"
	"github.com/projectbooth/booth-storage/internal/backend/filesystem"
	"github.com/projectbooth/booth-storage/internal/backend/gcs"
	"github.com/projectbooth/booth-storage/internal/backend/s3"
)

// maxCredentialBytes bounds the total size of a backend's credential values. A GCS
// service-account key is ~2.5 KB; anything near this cap is a mistake.
const maxCredentialBytes = 32 << 10

// normalizeConfig strictly decodes raw as kind's config, validates it, and returns the
// canonical JSON to persist. Strict (unknown fields rejected) so a typo like "buket"
// fails loudly at registration rather than silently producing a broken backend.
func normalizeConfig(kind backend.Kind, raw json.RawMessage, workspace string, fsPolicy FilesystemPolicy) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}

	var (
		cfg any
		err error
	)
	switch kind {
	case backend.KindS3:
		var c s3.Config
		if err = decodeStrict(raw, &c); err == nil {
			err = c.Validate()
		}
		cfg = c
	case backend.KindAzure:
		var c azure.Config
		if err = decodeStrict(raw, &c); err == nil {
			err = c.Validate()
		}
		cfg = c
	case backend.KindGCS:
		var c gcs.Config
		if err = decodeStrict(raw, &c); err == nil {
			err = c.Validate()
		}
		cfg = c
	case backend.KindFilesystem:
		var c filesystem.Config
		if err = decodeStrict(raw, &c); err == nil {
			c.RootPath = filepath.Clean(c.RootPath)
			if c.RootPath == "." { // Clean("") == "."
				err = fmt.Errorf("rootPath is required")
			} else {
				err = fsPolicy.Check(workspace, c.RootPath)
			}
		}
		cfg = c
	default:
		return nil, invalid("kind", "unsupported kind %q (supported: %s)", kind, kindList())
	}
	if err != nil {
		return nil, invalid("config", "%v", err)
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}
	return out, nil
}

func decodeStrict(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("invalid config: trailing data")
	}
	return nil
}

func kindList() string {
	names := make([]string, len(backend.Kinds))
	for i, k := range backend.Kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// validateCredentials checks a credential set is acceptable for kind: only known keys,
// the right combinations, sane sizes. It does not contact the service — that is what
// the "test connection" action is for.
func validateCredentials(kind backend.Kind, creds map[string]string) error {
	allowed := map[backend.Kind][]string{
		backend.KindS3:         {s3.CredAccessKeyID, s3.CredSecretAccessKey, s3.CredSessionToken},
		backend.KindAzure:      {azure.CredAccountKey, azure.CredSASToken},
		backend.KindGCS:        {gcs.CredServiceAccountJSON},
		backend.KindFilesystem: nil,
	}[kind]

	total := 0
	for k, v := range creds {
		if !contains(allowed, k) {
			if len(allowed) == 0 {
				return invalid("credentials", "the %s kind takes no credentials", kind)
			}
			return invalid("credentials", "unknown credential %q for the %s kind (allowed: %s)", k, kind, strings.Join(sorted(allowed), ", "))
		}
		if strings.TrimSpace(v) == "" {
			return invalid("credentials."+k, "must not be empty (omit it instead)")
		}
		total += len(v)
	}
	if total > maxCredentialBytes {
		return invalid("credentials", "credential values total more than %d bytes", maxCredentialBytes)
	}

	switch kind {
	case backend.KindS3:
		hasID, hasSecret := creds[s3.CredAccessKeyID] != "", creds[s3.CredSecretAccessKey] != ""
		if hasID != hasSecret {
			return invalid("credentials", "%s and %s must be provided together (or neither, for anonymous access)", s3.CredAccessKeyID, s3.CredSecretAccessKey)
		}
		if creds[s3.CredSessionToken] != "" && !hasID {
			return invalid("credentials", "%s requires %s and %s", s3.CredSessionToken, s3.CredAccessKeyID, s3.CredSecretAccessKey)
		}
	case backend.KindAzure:
		hasKey, hasSAS := creds[azure.CredAccountKey] != "", creds[azure.CredSASToken] != ""
		if hasKey == hasSAS {
			return invalid("credentials", "provide exactly one of %s or %s", azure.CredAccountKey, azure.CredSASToken)
		}
	case backend.KindGCS:
		if v := creds[gcs.CredServiceAccountJSON]; v != "" {
			if err := gcs.ValidateServiceAccountJSON(v); err != nil {
				return invalid("credentials."+gcs.CredServiceAccountJSON, "%v", err)
			}
		}
	}
	return nil
}

// buildBackend constructs the concrete backend for a stored record.
func buildBackend(kind backend.Kind, cfg json.RawMessage, creds map[string]string, workspace string, fsPolicy FilesystemPolicy) (backend.Backend, error) {
	switch kind {
	case backend.KindS3:
		var c s3.Config
		if err := decodeStrict(cfg, &c); err != nil {
			return nil, err
		}
		return s3.New(c, s3.Credentials{
			AccessKeyID:     creds[s3.CredAccessKeyID],
			SecretAccessKey: creds[s3.CredSecretAccessKey],
			SessionToken:    creds[s3.CredSessionToken],
		})
	case backend.KindAzure:
		var c azure.Config
		if err := decodeStrict(cfg, &c); err != nil {
			return nil, err
		}
		return azure.New(c, azure.Credentials{AccountKey: creds[azure.CredAccountKey], SASToken: creds[azure.CredSASToken]})
	case backend.KindGCS:
		var c gcs.Config
		if err := decodeStrict(cfg, &c); err != nil {
			return nil, err
		}
		return gcs.New(c, gcs.Credentials{ServiceAccountJSON: creds[gcs.CredServiceAccountJSON]})
	case backend.KindFilesystem:
		var c filesystem.Config
		if err := decodeStrict(cfg, &c); err != nil {
			return nil, err
		}
		// Re-checked at open time, not just registration: the operator may have
		// tightened the allowed roots since this backend was registered.
		if err := fsPolicy.Check(workspace, c.RootPath); err != nil {
			return nil, err
		}
		return filesystem.New(c)
	}
	return nil, fmt.Errorf("unsupported kind %q", kind)
}

// Location renders a short, human-readable, non-secret address for a record, shown in
// lists so two backends of the same kind can be told apart.
func Location(kind backend.Kind, cfg json.RawMessage) string {
	switch kind {
	case backend.KindS3:
		var c s3.Config
		if json.Unmarshal(cfg, &c) == nil {
			return "s3://" + c.Bucket + slashPrefix(c.Prefix)
		}
	case backend.KindAzure:
		var c azure.Config
		if json.Unmarshal(cfg, &c) == nil {
			return "azure://" + c.AccountName + "/" + c.Container + slashPrefix(c.Prefix)
		}
	case backend.KindGCS:
		var c gcs.Config
		if json.Unmarshal(cfg, &c) == nil {
			return "gs://" + c.Bucket + slashPrefix(c.Prefix)
		}
	case backend.KindFilesystem:
		var c filesystem.Config
		if json.Unmarshal(cfg, &c) == nil {
			return c.RootPath
		}
	}
	return ""
}

func slashPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
