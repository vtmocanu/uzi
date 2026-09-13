import { test } from "node:test";
import assert from "node:assert/strict";

import { buildProviderLaunchPlan } from "./provider-launch-plan.js";

test("P provider launch keeps the production contract separate from a rootless executable", () => {
  const executableBin = "/tmp/rootless-cache/0.153.2/bin/codex";
  const plan = buildProviderLaunchPlan(
    {
      CODEX_BIN: "/opt/uzi-codex/0.153.2/bin/codex",
      SUPERVISOR_BIN: "/usr/local/bin/uzi-codex-supervisor",
      PROVIDER_CHILD_ARGV: ["app-server"],
    },
    {
      executableBin,
      provider: { name: "openai", baseUrl: "http://127.0.0.1:1", envKey: "FAKE_PROVIDER_API_KEY" },
      model: "test-model",
      cwd: "/work/repo",
      ownedDataRoot: "/work/data",
    },
  );

  assert.equal(plan.spec.codexBin, "/opt/uzi-codex/0.153.2/bin/codex");
  assert.equal(plan.executableBin, executableBin);
  assert.notEqual(plan.spec.codexBin, plan.executableBin);
});
