import { describe, expect, it } from "vitest";
import models from "../../../fixtures/codex-models/models.json";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "./codexModels";

const sorted = (xs: readonly string[]) => [...xs].sort();

// PRD #3245 §1: the web exports equal the shared vocabulary fixture exactly, as sets.
describe("codexModels vs fixtures/codex-models/models.json", () => {
  it("CODEX_CURATED_MODELS equals the fixture's curated set", () => {
    expect(sorted(CODEX_CURATED_MODELS)).toEqual(sorted(models.curated));
    expect(new Set(CODEX_CURATED_MODELS).size).toBe(CODEX_CURATED_MODELS.length);
  });

  it("CODEX_ROLE_MODELS equals the fixture's role set", () => {
    expect(sorted(CODEX_ROLE_MODELS)).toEqual(sorted(models.role));
    expect(new Set(CODEX_ROLE_MODELS).size).toBe(CODEX_ROLE_MODELS.length);
  });
});
