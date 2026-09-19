import { useState, type FormEvent } from "react";
import {
  ApiError,
  createBackend,
  testNewBackend,
  testSavedBackend,
  updateBackend,
  type ApiContext,
} from "../api/client";
import {
  KIND_DEFS,
  buildConfig,
  buildCredentials,
  isValidBackendId,
  suggestId,
  valuesFromConfig,
  type FieldValues,
} from "../kinds";
import type { AdminBackend, BackendKind, CreateBackendRequest, TestResult, UpdateBackendRequest } from "../types";
import { Banner, Button, Field, inputClass } from "./ui";

/** Create or edit one backend. Two properties matter more than the layout:
 *
 *  1. Credentials are write-only. The server never returns them, so an edit form cannot
 *     show what's stored; a blank credential input therefore means "keep what's stored",
 *     never "clear it" (see buildCredentials). Clearing is an explicit checkbox.
 *  2. The server is the authority. Client checks only save a round trip; any error the
 *     server names a field for is shown against that input. */
export function BackendForm({
  ctx,
  existing,
  kinds,
  onSaved,
  onCancel,
}: {
  ctx: ApiContext;
  /** The backend being edited; absent when creating. */
  existing?: AdminBackend;
  /** Kinds this deployment lets you register (the filesystem kind may be disabled). */
  kinds: BackendKind[];
  onSaved: (b: AdminBackend) => void;
  onCancel: () => void;
}) {
  const editing = existing !== undefined;

  const [kind, setKind] = useState<BackendKind>(existing?.kind ?? kinds[0] ?? "s3");
  const [displayName, setDisplayName] = useState(existing?.displayName ?? "");
  const [id, setId] = useState(existing?.id ?? "");
  const [idTouched, setIdTouched] = useState(false);
  const [config, setConfig] = useState<FieldValues>(existing ? valuesFromConfig(existing.kind, existing.config) : {});
  const [creds, setCreds] = useState<FieldValues>({});
  const [clearCreds, setClearCreds] = useState(false);

  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<TestResult | null>(null);
  const [serverError, setServerError] = useState<ApiError | null>(null);
  const [showLocalErrors, setShowLocalErrors] = useState(false);

  const def = KIND_DEFS[kind];
  const typedCredentials = buildCredentials(kind, creds) !== undefined;

  // ---- local validation (saves a round trip; the server re-validates) ----
  const localErrors: Record<string, string> = {};
  if (!editing && !isValidBackendId(id)) {
    localErrors.id = id ? "Use lowercase letters, digits and hyphens (start and end with a letter or digit)." : "Required";
  }
  for (const f of def.configFields) {
    if (f.required && typeof config[f.key] === "string" && !(config[f.key] as string).trim()) localErrors[f.key] = "Required";
    if (f.required && config[f.key] === undefined) localErrors[f.key] = "Required";
  }

  // Server errors are mapped onto inputs by the field name the API returns.
  const serverField = serverError?.field ?? "";
  const errorFor = (key: string): string | undefined =>
    (showLocalErrors ? localErrors[key] : undefined) ??
    (serverField === key || serverField === `credentials.${key}` ? serverError?.message : undefined);
  const generalError =
    serverError && !["id", "displayName", ...def.configFields.map((f) => f.key), ...def.credentialFields.map((f) => f.key), ...def.credentialFields.map((f) => `credentials.${f.key}`)].includes(serverField)
      ? serverError.message
      : null;

  function touch() {
    setTestResult(null);
    setServerError(null);
  }

  function changeKind(next: BackendKind) {
    setKind(next);
    setConfig({});
    setCreds({});
    setShowLocalErrors(false);
    touch();
  }

  function credentialsForRequest(): Record<string, string> | undefined {
    const typed = buildCredentials(kind, creds);
    if (typed) return typed;
    // Explicit, opt-in clearing on edit: an empty object replaces the stored set with none.
    if (editing && clearCreds) return {};
    return undefined;
  }

  function createRequest(): CreateBackendRequest {
    const c = credentialsForRequest();
    return {
      id,
      displayName: displayName.trim() || undefined,
      kind,
      config: buildConfig(kind, config),
      ...(c ? { credentials: c } : {}),
    };
  }

  function updateRequest(): UpdateBackendRequest {
    const c = credentialsForRequest();
    return { displayName: displayName.trim() || undefined, config: buildConfig(kind, config), ...(c ? { credentials: c } : {}) };
  }

  async function runTest() {
    setShowLocalErrors(true);
    if (Object.keys(localErrors).length > 0) return;
    setTesting(true);
    setTestResult(null);
    setServerError(null);
    try {
      setTestResult(editing ? await testSavedBackend(ctx, existing.id, updateRequest()) : await testNewBackend(ctx, createRequest()));
    } catch (err) {
      setServerError(err instanceof ApiError ? err : new ApiError(0, err instanceof Error ? err.message : String(err)));
    } finally {
      setTesting(false);
    }
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setShowLocalErrors(true);
    if (Object.keys(localErrors).length > 0) return;
    setSaving(true);
    setServerError(null);
    try {
      const saved = editing ? await updateBackend(ctx, existing.id, updateRequest()) : await createBackend(ctx, createRequest());
      onSaved(saved);
    } catch (err) {
      setServerError(err instanceof ApiError ? err : new ApiError(0, err instanceof Error ? err.message : String(err)));
      setSaving(false);
    }
  }

  const busy = saving || testing;
  const title = editing ? `Edit ${existing.displayName}` : "Add a storage backend";

  return (
    <form
      onSubmit={submit}
      aria-label={title}
      noValidate
      className="flex flex-col gap-4 rounded-lg border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900"
    >
      <h3 className="text-base font-semibold text-slate-900 dark:text-slate-100">{title}</h3>

      {generalError && <Banner tone="error">{generalError}</Banner>}

      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="bf-kind" label="Kind" help={editing ? "A backend's kind can't be changed. Register a new one instead." : def.description}>
          {(p) => (
            <select
              {...p}
              className={inputClass}
              value={kind}
              disabled={editing || busy}
              onChange={(e) => changeKind(e.target.value as BackendKind)}
            >
              {(editing ? [kind] : kinds).map((k) => (
                <option key={k} value={k}>
                  {KIND_DEFS[k].label}
                </option>
              ))}
            </select>
          )}
        </Field>

        <Field id="bf-name" label="Display name" error={errorFor("displayName")}>
          {(p) => (
            <input
              {...p}
              className={inputClass}
              type="text"
              value={displayName}
              disabled={busy}
              onChange={(e) => {
                setDisplayName(e.target.value);
                if (!editing && !idTouched) setId(suggestId(e.target.value));
                touch();
              }}
            />
          )}
        </Field>

        <Field
          id="bf-id"
          label="ID"
          required
          error={errorFor("id")}
          help={
            editing
              ? "The ID is how other modules address this backend, so it can't be changed."
              : "How other modules address this backend. Permanent once created."
          }
        >
          {(p) => (
            <input
              {...p}
              className={`${inputClass} font-mono`}
              type="text"
              value={id}
              disabled={editing || busy}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => {
                setId(e.target.value);
                setIdTouched(true);
                touch();
              }}
            />
          )}
        </Field>
      </div>

      <fieldset className="flex flex-col gap-3" disabled={busy}>
        <legend className="mb-1 text-sm font-medium text-slate-700 dark:text-slate-200">Location</legend>
        {def.configFields.map((f) =>
          f.type === "checkbox" ? (
            <div key={f.key} className="flex flex-col gap-1">
              <label className="flex items-center gap-2 text-sm text-slate-700 dark:text-slate-200">
                <input
                  type="checkbox"
                  checked={config[f.key] === true}
                  onChange={(e) => {
                    setConfig((c) => ({ ...c, [f.key]: e.target.checked }));
                    touch();
                  }}
                />
                {f.label}
              </label>
              {f.help && <p className="text-xs text-slate-500 dark:text-slate-400">{f.help}</p>}
            </div>
          ) : (
            <Field key={f.key} id={`bf-cfg-${f.key}`} label={f.label} help={f.help} required={f.required} error={errorFor(f.key)}>
              {(p) => (
                <input
                  {...p}
                  className={inputClass}
                  type="text"
                  value={(config[f.key] as string | undefined) ?? ""}
                  placeholder={f.placeholder}
                  autoComplete="off"
                  spellCheck={false}
                  onChange={(e) => {
                    setConfig((c) => ({ ...c, [f.key]: e.target.value }));
                    touch();
                  }}
                />
              )}
            </Field>
          ),
        )}
      </fieldset>

      {def.credentialFields.length > 0 && (
        <fieldset className="flex flex-col gap-3" disabled={busy}>
          <legend className="mb-1 text-sm font-medium text-slate-700 dark:text-slate-200">Credentials</legend>
          <p className="text-xs text-slate-500 dark:text-slate-400">
            {def.credentialHelp}{" "}
            {editing
              ? existing.credentialsSet
                ? "Credentials are stored and can't be shown. Leave these blank to keep them; enter values to replace them all."
                : "No credentials are stored."
              : "Stored as a Kubernetes Secret, never in the database."}
          </p>
          {def.credentialFields.map((f) => (
            <Field key={f.key} id={`bf-cred-${f.key}`} label={f.label} help={f.help} error={errorFor(f.key)}>
              {(p) =>
                f.multiline ? (
                  <textarea
                    {...p}
                    className={`${inputClass} min-h-24 font-mono`}
                    value={(creds[f.key] as string | undefined) ?? ""}
                    placeholder={editing && existing.credentialsSet ? "Leave blank to keep the stored value" : undefined}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(e) => {
                      setCreds((c) => ({ ...c, [f.key]: e.target.value }));
                      touch();
                    }}
                  />
                ) : (
                  <input
                    {...p}
                    className={inputClass}
                    type="password"
                    value={(creds[f.key] as string | undefined) ?? ""}
                    placeholder={editing && existing.credentialsSet ? "Leave blank to keep the stored value" : undefined}
                    autoComplete="new-password"
                    spellCheck={false}
                    onChange={(e) => {
                      setCreds((c) => ({ ...c, [f.key]: e.target.value }));
                      touch();
                    }}
                  />
                )
              }
            </Field>
          ))}
          {editing && existing.credentialsSet && (
            <label className="flex items-center gap-2 text-sm text-slate-700 dark:text-slate-200">
              <input
                type="checkbox"
                checked={clearCreds && !typedCredentials}
                disabled={typedCredentials}
                onChange={(e) => {
                  setClearCreds(e.target.checked);
                  touch();
                }}
              />
              Remove the stored credentials
            </label>
          )}
        </fieldset>
      )}

      {testResult && (testResult.ok ? <Banner tone="success">Connection succeeded.</Banner> : <Banner tone="error">Connection failed: {testResult.error}</Banner>)}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="primary" disabled={busy}>
          {saving ? "Saving…" : editing ? "Save changes" : "Add backend"}
        </Button>
        <Button onClick={runTest} disabled={busy}>
          {testing ? "Testing…" : "Test connection"}
        </Button>
        <Button onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
