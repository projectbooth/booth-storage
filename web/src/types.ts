// Mirrors internal/api/server.go and internal/backend/backend.go's JSON shapes — kept as
// hand-written types rather than generated, since this repo has no shared-schema tooling
// (the same trade-off booth-module-store made). Keep the two in sync by hand.

export type BackendKind = "s3" | "filesystem" | "azure" | "gcs";

/** Caller's role in the active workspace (ADR 0025). Matches contracts/ui-integration.md's
 *  NativeModuleProps contract (ADR 0031). */
export type WorkspaceRole = "owner" | "editor" | "viewer";

/** What the regular view and downstream modules see: enough to choose a backend by. */
export interface BackendSummary {
  id: string;
  displayName: string;
  kind: BackendKind;
  /** Short, non-secret address, e.g. "s3://bucket/prefix" — tells same-kind backends apart. */
  location: string;
  createdAt: string;
  updatedAt: string;
}

/** Admin-only detail: the full non-secret configuration. Credentials are never present —
 *  only whether any are stored (ADR 0020). */
export interface AdminBackend extends BackendSummary {
  config: Record<string, unknown>;
  credentialsSet: boolean;
  createdBy?: string;
  /** Whether a create-only upload (ADR 0117 point 8) can be admitted against this
   *  backend right now. Always true except an S3 backend that hasn't yet passed the
   *  create-only capability probe ("Test" on the Backends panel runs it). */
  createOnlySupported?: boolean;
}

export interface ObjectEntry {
  /** Relative to the backend root; ends in "/" for a directory entry. */
  path: string;
  size: number;
  modTime?: string;
  contentType?: string;
  isDir?: boolean;
}

export interface ListResult {
  entries: ObjectEntry[];
  nextCursor?: string;
}

export interface KindsResponse {
  kinds: BackendKind[];
  filesystemEnabled: boolean;
}

export interface CreateBackendRequest {
  id: string;
  displayName?: string;
  kind: BackendKind;
  config: Record<string, unknown>;
  credentials?: Record<string, string>;
}

/** Omitted `credentials` keeps the stored ones; an object replaces them wholesale
 *  (an empty object clears them). */
export interface UpdateBackendRequest {
  displayName?: string;
  config?: Record<string, unknown>;
  credentials?: Record<string, string>;
}

export interface TestResult {
  ok: boolean;
  error?: string;
}

// ---- upload grants (ADR 0117 "Reconciled design" item 1/8a) ----------------
// Mirrors docs/contracts/upload-grants.md.

/** One app's permission to make a create-only write into one backend prefix. */
export interface UploadGrant {
  id: string;
  workspace: string;
  /** The app's workload-token subject: "streamlit:<workspace>:<appId>". */
  principal: string;
  backendId: string;
  /** "" (the backend root) or ends in "/". */
  prefix: string;
  /** Lowercase, no leading dot, e.g. ["csv", "png"]. */
  extensions: string[];
  maxBytes: number;
  grantedBy: string;
  createdAt: string;
}

export interface CreateUploadGrantRequest {
  principal: string;
  backendId: string;
  prefix: string;
  extensions: string[];
  maxBytes: number;
}
