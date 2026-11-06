import { describe, expect, it } from "vitest";
import contract from "../../../fixtures/model-settings/contract.json";
import { codexRuntimeDefaultLabel, MODEL_SETTING_SOURCE_LABELS, modelSettingSourceLabel } from "./modelSettings";
import type { CatalogDefault, EffortSettingCell } from "./apiTypes";

const cellWith = (catalog_default?: unknown): EffortSettingCell => ({
  harness: "codex", effort: null, default_effort: "medium", default_source: "builtin",
  resolved_effort: "medium", source: "builtin", mode: "explicit",
  ...(catalog_default === undefined ? {} : { catalog_default: catalog_default as CatalogDefault }),
});
const known = (over: Partial<CatalogDefault> = {}): CatalogDefault => ({
  state: "known", effort: "medium", model: "gpt-6.1-sol", runtime_version: "0.164.0", observed_at: "2026-11-05T09:30:00Z", ...over,
});
const UNKNOWN = "Runtime default · catalog default unknown until a Codex run reports it";

// Issue #3251: the Codex Runtime default option text.
describe("codexRuntimeDefaultLabel", () => {
  it("renders a known default with its model and Codex version", () => {
    expect(codexRuntimeDefaultLabel(cellWith(known()))).toBe("Runtime default · catalog default: medium (gpt-6.1-sol, Codex 0.164.0)");
  });
  it("spells out none and ultra", () => {
    expect(codexRuntimeDefaultLabel(cellWith(known({ effort: "none" })))).toBe(
      "Runtime default · catalog default: none (no reasoning level requested) (gpt-6.1-sol, Codex 0.164.0)");
    expect(codexRuntimeDefaultLabel(cellWith(known({ effort: "ultra" })))).toBe(
      "Runtime default · catalog default: ultra (Codex substitutes another level) (gpt-6.1-sol, Codex 0.164.0)");
  });
  it("adds the version caveat when workers disagree", () => {
    expect(codexRuntimeDefaultLabel(cellWith(known({ state: "varies" })))).toBe(
      "Runtime default · catalog default: medium (gpt-6.1-sol, Codex 0.164.0) · may differ on workers still on another version");
  });
  it("reads unknown for unknown, absent and unrecognised shapes", () => {
    expect(codexRuntimeDefaultLabel(cellWith({ state: "unknown", effort: null, model: null, runtime_version: null, observed_at: null }))).toBe(UNKNOWN);
    expect(codexRuntimeDefaultLabel(undefined)).toBe(UNKNOWN);
    expect(codexRuntimeDefaultLabel(cellWith())).toBe(UNKNOWN);
    expect(codexRuntimeDefaultLabel(cellWith(known({ state: "bogus" as CatalogDefault["state"] })))).toBe(UNKNOWN);
  });
  it("reads unknown when a known or varies default is malformed", () => {
    for (const state of ["known", "varies"] as const) {
      for (const field of ["effort", "model", "runtime_version"] as const) {
        expect(codexRuntimeDefaultLabel(cellWith(known({ state, [field]: null }))), `${state}/${field} null`).toBe(UNKNOWN);
        expect(codexRuntimeDefaultLabel(cellWith({ ...known({ state }), [field]: 7 })), `${state}/${field} number`).toBe(UNKNOWN);
        expect(codexRuntimeDefaultLabel(cellWith(known({ state, [field]: "" }))), `${state}/${field} empty`).toBe(UNKNOWN);
      }
    }
    expect(codexRuntimeDefaultLabel(cellWith("known"))).toBe(UNKNOWN);
  });
});

// PRD #3245: the TS label table is pinned to fixtures/model-settings/contract.json,
// the same rows the Go CLI's modelSettingSource reads.
describe("modelSettingSourceLabel vs fixtures/model-settings/contract.json", () => {
  it("renders every fixture (field, token) row with its label", () => {
    let ran = 0;
    for (const row of contract.source_labels) {
      expect(modelSettingSourceLabel(row.field, row.token), `${row.field}/${row.token}`).toBe(row.label);
      ran++;
    }
    expect(ran).toBe(contract.source_labels.length);
    expect(ran).toBeGreaterThan(0);
  });

  it("carries no row the fixture lacks", () => {
    expect(MODEL_SETTING_SOURCE_LABELS).toEqual(contract.source_labels);
  });

  it("renders an unknown (field, token) as unknown", () => {
    expect(modelSettingSourceLabel("model", "harness")).toBe("unknown");
    expect(modelSettingSourceLabel("effort", "bogus")).toBe("unknown");
    expect(modelSettingSourceLabel("", "")).toBe("unknown");
  });
});
