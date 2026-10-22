import { describe, expect, it } from "vitest";
import contract from "../../../fixtures/model-settings/contract.json";
import { MODEL_SETTING_SOURCE_LABELS, modelSettingSourceLabel } from "./modelSettings";

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
    expect(modelSettingSourceLabel("effort", "builtin")).toBe("unknown");
    expect(modelSettingSourceLabel("", "")).toBe("unknown");
  });
});
