import { describe, expect, it } from "vitest";
import type { EffectiveSettings } from "../../lib/api";
import { MODELS_FIELDS } from "../../lib/modelsDraft";
import { predictionText } from "./modelsLabels";

const harnessField = MODELS_FIELDS.find((f) => f.kind === "harness")!;
const withStart = (harness: unknown) => ({ start: [{ choice: "default", harness }] }) as unknown as EffectiveSettings;

describe("Default harness prediction", () => {
  it("names an automatic harness once, then how it is chosen", () => {
    expect(predictionText(harnessField, withStart({ value: null, source: "chosen_at_create", notes: ["automatic_weekly_reset"] })))
      .toBe("Automatic · chosen by the soonest weekly reset");
    expect(predictionText(harnessField, withStart({ value: null, source: "chosen_at_create", notes: [] })))
      .toBe("Automatic · chosen when the run is created");
  });
});
