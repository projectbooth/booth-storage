import { describe, expect, it } from "vitest";
import {
  ALL_KINDS,
  KIND_DEFS,
  buildConfig,
  buildCredentials,
  isValidBackendId,
  suggestId,
  valuesFromConfig,
} from "../kinds";

describe("every v0 kind is defined (ADR 0013)", () => {
  it("covers all four kinds, each with a location field", () => {
    expect(ALL_KINDS).toEqual(["s3", "filesystem", "azure", "gcs"]);
    for (const k of ALL_KINDS) {
      expect(KIND_DEFS[k].kind).toBe(k);
      expect(KIND_DEFS[k].configFields.length).toBeGreaterThan(0);
      expect(KIND_DEFS[k].configFields.some((f) => f.required)).toBe(true);
    }
  });

  it("only kinds that authenticate declare credential fields", () => {
    expect(KIND_DEFS.filesystem.credentialFields).toEqual([]);
    expect(KIND_DEFS.s3.credentialFields.map((f) => f.key)).toEqual(["accessKeyId", "secretAccessKey", "sessionToken"]);
    expect(KIND_DEFS.azure.credentialFields.map((f) => f.key)).toEqual(["accountKey", "sasToken"]);
    expect(KIND_DEFS.gcs.credentialFields.map((f) => f.key)).toEqual(["serviceAccountJson"]);
  });

  // Credential keys and config keys must never overlap, or a secret could be routed into
  // the persisted, non-secret config.
  it("keeps secret and non-secret field keys disjoint for every kind", () => {
    for (const k of ALL_KINDS) {
      const cfg = new Set(KIND_DEFS[k].configFields.map((f) => f.key));
      for (const c of KIND_DEFS[k].credentialFields) expect(cfg.has(c.key)).toBe(false);
    }
  });
});

describe("buildConfig", () => {
  it("drops empty strings and unchecked boxes, and trims", () => {
    expect(buildConfig("s3", { bucket: "  data  ", endpoint: "", region: "   ", prefix: "x/", pathStyle: false })).toEqual({
      bucket: "data",
      prefix: "x/",
    });
    expect(buildConfig("s3", { bucket: "b", pathStyle: true })).toEqual({ bucket: "b", pathStyle: true });
  });

  it("ignores values that aren't fields of the kind, and never emits credential keys", () => {
    const out = buildConfig("gcs", { bucket: "b", serviceAccountJson: "{secret}", accessKeyId: "AKIA", bogus: "x" });
    expect(out).toEqual({ bucket: "b" });
  });

  it("builds a config for each kind", () => {
    expect(buildConfig("filesystem", { rootPath: "/data/acme/x" })).toEqual({ rootPath: "/data/acme/x" });
    expect(buildConfig("azure", { accountName: "acct", container: "c", endpoint: "", prefix: "p" })).toEqual({
      accountName: "acct",
      container: "c",
      prefix: "p",
    });
  });
});

describe("buildCredentials — blank means keep, never clear", () => {
  it("returns undefined when nothing was typed, so an edit leaves stored credentials alone", () => {
    expect(buildCredentials("s3", {})).toBeUndefined();
    expect(buildCredentials("s3", { accessKeyId: "", secretAccessKey: "" })).toBeUndefined();
    expect(buildCredentials("filesystem", { rootPath: "/x" })).toBeUndefined();
  });

  it("returns only the fields that were typed", () => {
    expect(buildCredentials("s3", { accessKeyId: "AKIA", secretAccessKey: "s3cret", sessionToken: "" })).toEqual({
      accessKeyId: "AKIA",
      secretAccessKey: "s3cret",
    });
  });

  it("does not trim secrets (whitespace can be part of one)", () => {
    expect(buildCredentials("azure", { accountKey: " k== " })).toEqual({ accountKey: " k== " });
  });

  it("ignores non-credential fields and checkbox values", () => {
    expect(buildCredentials("s3", { bucket: "b", pathStyle: true })).toBeUndefined();
  });
});

describe("valuesFromConfig", () => {
  it("seeds an edit form from saved non-secret config", () => {
    expect(valuesFromConfig("s3", { bucket: "b", endpoint: "http://m", pathStyle: true, extra: "ignored" })).toEqual({
      bucket: "b",
      endpoint: "http://m",
      region: "",
      prefix: "",
      pathStyle: true,
    });
  });
});

describe("backend IDs", () => {
  it("validates like the server", () => {
    for (const ok of ["a", "lake", "my-lake-2", "a1", "0abc", "a".repeat(63)]) expect(isValidBackendId(ok)).toBe(true);
    for (const bad of ["", "Lake", "-a", "a-", "a_b", "a b", "a/b", "a".repeat(64), "../x"]) expect(isValidBackendId(bad)).toBe(false);
  });

  it("suggests a valid ID from a display name", () => {
    expect(suggestId("My Data Lake!")).toBe("my-data-lake");
    expect(suggestId("  Scratch / temp  ")).toBe("scratch-temp");
    expect(suggestId("!!!")).toBe("");
    for (const name of ["Q3 Reports", "ÄÖÜ data", "a".repeat(100)]) {
      const id = suggestId(name);
      if (id) expect(isValidBackendId(id)).toBe(true);
    }
  });
});
