import { test } from "node:test";
import assert from "node:assert/strict";
import { phaseSettings } from "./phase-model-driver.js";
import type { CodexNotification } from "../../agent/src/codex/transport.js";

test("phase probe preserves sticky effort, runtime-default null and missing observations separately", () => {
  const update = (threadId: string, settings: Record<string, unknown>): CodexNotification => ({
    kind: "activity", method: "thread/settings/updated", params: { threadId, threadSettings: settings },
  });
  const records = phaseSettings([
    update("other", { model: "secret-other-model", effort: "high" }),
    update("root", { model: "A", effort: "high", account: "must-not-be-printed" }),
    { kind: "turn_started", method: "turn/started", threadId: "root", turnId: "t", params: {} },
    update("root", { model: "B", effort: null }), update("root", { model: "B" }),
  ], "root", "t");
  assert.deepEqual(records, [
    { phase: "before-turn", model: "A", effortPresent: true, effort: "high" },
    { phase: "turn", model: "B", effortPresent: true, effort: null },
    { phase: "turn", model: "B", effortPresent: false, effort: "absent-or-invalid" },
  ]);
  assert.equal(JSON.stringify(records).includes("must-not-be-printed"), false);
  assert.deepEqual(phaseSettings([], "root", "t"), []);
});
