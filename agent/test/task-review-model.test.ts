import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";

import { CODEX_TASK_REVIEW_MODEL } from "../src/codex/task-review-model.js";

// The built-in task-review model is pinned to fixtures/model-settings/task-review.json, which the
// API's agenttmpl.CodexTaskReviewModel test reads too, so the effective-settings preview and the
// worker cannot drift.
describe("task review built-in model", () => {
  it("CODEX_TASK_REVIEW_MODEL equals the fixture's codex builtin", async () => {
    const fx = JSON.parse(
      await fs.readFile(new URL("../../fixtures/model-settings/task-review.json", import.meta.url), "utf8"),
    ) as { builtins: { claude: string; codex: string } };
    assert.equal(CODEX_TASK_REVIEW_MODEL, fx.builtins.codex);
    assert.equal(fx.builtins.claude, "", "the Claude review pass sends no model");
  });
});
