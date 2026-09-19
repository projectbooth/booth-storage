// Public entry point for @projectbooth/storage-ui (ADR 0030). Anything booth-design (or any
// future consumer) needs is re-exported here — internal components/helpers under
// src/components, src/api, etc. are not part of the public API and can change freely.
//
// Consumers must also import this package's stylesheet once
// (`@projectbooth/storage-ui/dist/style.css`) — see this repo's README for why it's a
// separate import rather than auto-injected.
import "./library.css";

export { StorageApp, StorageBrowseApp, StorageAdminApp } from "./StorageApp";
export type { StorageAppProps } from "./StorageApp";
export type { WorkspaceRole, BackendKind, BackendSummary } from "./types";
export type { ViewName } from "./navigation";
