import type { BackendKind } from "./types";

// Per-kind form definitions for the admin view. The server is the authority on what's
// valid (internal/registry/kinds.go) — these describe the *shape* of the form and
// build the request payload; anything they let through the server re-validates, and
// its field-level errors are shown against the matching input.

export interface FieldDef {
  key: string;
  label: string;
  help?: string;
  placeholder?: string;
  required?: boolean;
  type?: "text" | "checkbox";
}

export interface CredentialFieldDef extends FieldDef {
  /** Render as a masked input. Every credential is treated as secret. */
  multiline?: boolean;
}

export interface KindDef {
  kind: BackendKind;
  label: string;
  description: string;
  configFields: FieldDef[];
  credentialFields: CredentialFieldDef[];
  /** Plain-language rule for the credential fields, shown above them. */
  credentialHelp?: string;
}

export const KIND_DEFS: Record<BackendKind, KindDef> = {
  s3: {
    kind: "s3",
    label: "S3-compatible",
    description: "AWS S3, MinIO, Ceph, or any S3-compatible object store.",
    configFields: [
      { key: "bucket", label: "Bucket", required: true },
      {
        key: "endpoint",
        label: "Endpoint",
        placeholder: "https://s3.amazonaws.com",
        help: "Full URL of the service. Leave empty for AWS S3.",
      },
      { key: "region", label: "Region", placeholder: "us-east-1" },
      { key: "prefix", label: "Key prefix", help: "Confine this backend to one prefix inside the bucket." },
      { key: "pathStyle", label: "Use path-style addressing", type: "checkbox", help: "Needed by MinIO and most self-hosted stores." },
    ],
    credentialFields: [
      { key: "accessKeyId", label: "Access key ID" },
      { key: "secretAccessKey", label: "Secret access key" },
      { key: "sessionToken", label: "Session token", help: "Optional, for temporary credentials." },
    ],
    credentialHelp: "Provide the access key ID and secret together, or leave both empty for anonymous access.",
  },
  filesystem: {
    kind: "filesystem",
    label: "Filesystem",
    description: "A directory on the storage server's own filesystem.",
    configFields: [
      {
        key: "rootPath",
        label: "Root directory",
        required: true,
        placeholder: "/data/my-workspace/scratch",
        help: "Absolute path, inside a directory your platform operator has made available. If it doesn't exist yet it is created, provided its parent directory exists.",
      },
    ],
    credentialFields: [],
  },
  azure: {
    kind: "azure",
    label: "Azure Blob Storage",
    description: "An Azure Blob Storage container.",
    configFields: [
      { key: "accountName", label: "Storage account", required: true },
      { key: "container", label: "Container", required: true },
      { key: "endpoint", label: "Endpoint", help: "Only for sovereign clouds, private endpoints or an emulator. Leave empty for public Azure." },
      { key: "prefix", label: "Blob prefix", help: "Confine this backend to one prefix inside the container." },
    ],
    credentialFields: [
      { key: "accountKey", label: "Account key" },
      { key: "sasToken", label: "SAS token" },
    ],
    credentialHelp: "Provide exactly one: the account key, or a SAS token.",
  },
  gcs: {
    kind: "gcs",
    label: "Google Cloud Storage",
    description: "A Google Cloud Storage bucket.",
    configFields: [
      { key: "bucket", label: "Bucket", required: true },
      { key: "prefix", label: "Object prefix", help: "Confine this backend to one prefix inside the bucket." },
    ],
    credentialFields: [
      {
        key: "serviceAccountJson",
        label: "Service account key (JSON)",
        multiline: true,
        help: "Leave empty to use the server's Application Default Credentials (e.g. GKE Workload Identity).",
      },
    ],
  },
};

export const ALL_KINDS: BackendKind[] = ["s3", "filesystem", "azure", "gcs"];

/** Form values are strings (or booleans for checkboxes), keyed by field key. */
export type FieldValues = Record<string, string | boolean>;

/** Builds the non-secret `config` payload: empty strings and false checkboxes are
 *  dropped so the server sees only what was actually set, and values are trimmed. */
export function buildConfig(kind: BackendKind, values: FieldValues): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of KIND_DEFS[kind].configFields) {
    const v = values[f.key];
    if (f.type === "checkbox") {
      if (v === true) out[f.key] = true;
    } else if (typeof v === "string" && v.trim() !== "") {
      out[f.key] = v.trim();
    }
  }
  return out;
}

/** Builds the `credentials` payload from the credential inputs, or undefined to omit it.
 *
 *  Omitting matters: on an edit, an omitted `credentials` tells the server to KEEP the
 *  stored ones. The form can't show existing secrets (the server never returns them), so
 *  "left blank" must mean "unchanged", not "cleared". Only when the user typed at least
 *  one credential is a payload sent — and then it replaces the whole stored set. Values
 *  are deliberately not trimmed: a secret's whitespace is its own business. */
export function buildCredentials(kind: BackendKind, values: FieldValues): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const f of KIND_DEFS[kind].credentialFields) {
    const v = values[f.key];
    if (typeof v === "string" && v !== "") out[f.key] = v;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** Seeds form values from a saved backend's non-secret config, for the edit form. */
export function valuesFromConfig(kind: BackendKind, config: Record<string, unknown>): FieldValues {
  const out: FieldValues = {};
  for (const f of KIND_DEFS[kind].configFields) {
    const v = config[f.key];
    if (f.type === "checkbox") out[f.key] = v === true;
    else out[f.key] = typeof v === "string" ? v : "";
  }
  return out;
}

const ID_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** Mirrors the server's backend-ID rule (registry.ValidateID), so the form can flag a
 *  bad ID before a round trip. The server still enforces it. */
export function isValidBackendId(id: string): boolean {
  return ID_RE.test(id);
}

/** Suggests an ID from a display name: lowercase, hyphenated, trimmed to the rule. */
export function suggestId(displayName: string): string {
  return displayName
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/g, "");
}
