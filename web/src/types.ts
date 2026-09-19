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
