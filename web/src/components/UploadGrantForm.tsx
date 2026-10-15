import { useState, type FormEvent } from "react";
import { ApiError, createUploadGrant, type ApiContext } from "../api/client";
import type { AdminBackend, UploadGrant } from "../types";
import { Banner, Button, Field, inputClass } from "./ui";

/** A link into this page (from booth-streamlit's "Write targets" panel) may prefill the
 *  form via query parameters — see docs/contracts/upload-grants.md's "URL prefill". */
export interface UploadGrantPrefill {
  principal?: string;
  backendId?: string;
  prefix?: string;
}

function parseExtensions(raw: string): string[] {
  return raw
    .split(",")
    .map((e) => e.trim())
    .filter((e) => e.length > 0);
}

/** Grant one app a create-only write into one backend prefix (ADR 0117 point 8a). A
 *  second grant for the same principal/backend/prefix replaces the first outright —
 *  the server does that, not this form — so "create" is also how an owner edits one. */
export function UploadGrantForm({
  ctx,
  backends,
  prefill,
  onSaved,
  onCancel,
}: {
  ctx: ApiContext;
  /** Which backends an owner may pick: every registered one. */
  backends: AdminBackend[];
  prefill?: UploadGrantPrefill;
  onSaved: (g: UploadGrant) => void;
  onCancel: () => void;
}) {
  const [principal, setPrincipal] = useState(prefill?.principal ?? "");
  const [backendId, setBackendId] = useState(prefill?.backendId ?? backends[0]?.id ?? "");
  const [prefix, setPrefix] = useState(prefill?.prefix ?? "");
  const [extensionsRaw, setExtensionsRaw] = useState("");
  const [maxBytes, setMaxBytes] = useState("");

  const [saving, setSaving] = useState(false);
  const [serverError, setServerError] = useState<ApiError | null>(null);
  const [showLocalErrors, setShowLocalErrors] = useState(false);

  const extensions = parseExtensions(extensionsRaw);
  const maxBytesNum = Number(maxBytes);

  const localErrors: Record<string, string> = {};
  if (!/^streamlit:[a-z0-9-]+:[A-Za-z0-9._-]{1,128}$/.test(principal)) {
    localErrors.principal = "Must be the app's workload-token subject: streamlit:<workspace>:<appId>";
  }
  if (!backendId) localErrors.backendId = "Required";
  if (extensions.length === 0) localErrors.extensions = "At least one file extension is required";
  if (!maxBytes || !Number.isFinite(maxBytesNum) || maxBytesNum <= 0) localErrors.maxBytes = "Must be a whole number of bytes greater than zero";

  const serverField = serverError?.field ?? "";
  const errorFor = (key: string): string | undefined =>
    (showLocalErrors ? localErrors[key] : undefined) ?? (serverField === key ? serverError?.message : undefined);
  const generalError = serverError && !["principal", "backendId", "prefix", "extensions", "maxBytes"].includes(serverField) ? serverError.message : null;

  const selectedBackend = backends.find((b) => b.id === backendId);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setShowLocalErrors(true);
    if (Object.keys(localErrors).length > 0) return;
    setSaving(true);
    setServerError(null);
    try {
      const saved = await createUploadGrant(ctx, { principal, backendId, prefix, extensions, maxBytes: maxBytesNum });
      onSaved(saved);
    } catch (err) {
      setServerError(err instanceof ApiError ? err : new ApiError(0, err instanceof Error ? err.message : String(err)));
      setSaving(false);
    }
  }

  return (
    <form
      onSubmit={submit}
      aria-label="Grant upload access"
      noValidate
      className="flex flex-col gap-4 rounded-lg border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900"
    >
      <h3 className="text-base font-semibold text-slate-900 dark:text-slate-100">Grant upload access</h3>
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Lets one app make a single create-only write into one folder — it can never overwrite, delete, or reach anywhere else. A
        second grant for the same app, backend and folder replaces this one.
      </p>

      {generalError && <Banner tone="error">{generalError}</Banner>}

      <Field
        id="ugf-principal"
        label="App principal"
        required
        error={errorFor("principal")}
        help="The app's workload-token subject, from booth-streamlit's Write targets panel: streamlit:<workspace>:<appId>."
      >
        {(p) => (
          <input
            {...p}
            className={`${inputClass} font-mono`}
            type="text"
            value={principal}
            autoComplete="off"
            spellCheck={false}
            disabled={saving}
            onChange={(e) => {
              setPrincipal(e.target.value);
              setServerError(null);
            }}
          />
        )}
      </Field>

      <Field id="ugf-backend" label="Backend" required error={errorFor("backendId")}>
        {(p) => (
          <select
            {...p}
            className={inputClass}
            value={backendId}
            disabled={saving || backends.length === 0}
            onChange={(e) => {
              setBackendId(e.target.value);
              setServerError(null);
            }}
          >
            {backends.length === 0 && <option value="">No backends registered</option>}
            {backends.map((b) => (
              <option key={b.id} value={b.id}>
                {b.displayName} ({b.id})
              </option>
            ))}
          </select>
        )}
      </Field>
      {selectedBackend && selectedBackend.createOnlySupported === false && (
        <Banner tone="info">
          This backend hasn't passed the create-only capability probe yet — uploads will fail with "not implemented" until an
          owner re-runs "Test" for it on the Backends panel.
        </Banner>
      )}

      <Field id="ugf-prefix" label="Folder" error={errorFor("prefix")} help='Where the app may upload, e.g. "incoming". Leave blank for the backend root.'>
        {(p) => (
          <input
            {...p}
            className={`${inputClass} font-mono`}
            type="text"
            value={prefix}
            placeholder="incoming"
            autoComplete="off"
            spellCheck={false}
            disabled={saving}
            onChange={(e) => {
              setPrefix(e.target.value);
              setServerError(null);
            }}
          />
        )}
      </Field>

      <Field
        id="ugf-extensions"
        label="Allowed file extensions"
        required
        error={errorFor("extensions")}
        help='Comma-separated, no dots, e.g. "csv, png".'
      >
        {(p) => (
          <input
            {...p}
            className={inputClass}
            type="text"
            value={extensionsRaw}
            placeholder="csv, png"
            autoComplete="off"
            spellCheck={false}
            disabled={saving}
            onChange={(e) => {
              setExtensionsRaw(e.target.value);
              setServerError(null);
            }}
          />
        )}
      </Field>

      <Field id="ugf-maxbytes" label="Maximum file size (bytes)" required error={errorFor("maxBytes")} help="e.g. 26214400 for 25 MiB.">
        {(p) => (
          <input
            {...p}
            className={inputClass}
            type="number"
            min={1}
            step={1}
            value={maxBytes}
            disabled={saving}
            onChange={(e) => {
              setMaxBytes(e.target.value);
              setServerError(null);
            }}
          />
        )}
      </Field>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="primary" disabled={saving}>
          {saving ? "Saving…" : "Grant access"}
        </Button>
        <Button onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
