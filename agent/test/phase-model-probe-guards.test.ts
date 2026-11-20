import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import os from "node:os";
import { liveArgs, probeError } from "./phase-model-live.js";

test("phase live probes refuse without the exact opt-in and named credential variable", () => {
  const argv = ["--credential-env", "PROBE_CREDENTIAL", "--model-a", "A", "--model-b", "B", "--effort-a", "high", "--effort-b", "medium"];
  assert.throws(() => liveArgs(argv, {}), /UZI_PHASE_MODEL_LIVE/);
  assert.throws(() => liveArgs(argv, { UZI_PHASE_MODEL_LIVE: "1" }), /credential variable/);
  assert.throws(() => liveArgs(argv, { UZI_PHASE_MODEL_LIVE: "true", PROBE_CREDENTIAL: "fixture" }), /UZI_PHASE_MODEL_LIVE/);
  assert.equal(liveArgs(argv, { UZI_PHASE_MODEL_LIVE: "1", PROBE_CREDENTIAL: "fixture" }).variable, "PROBE_CREDENTIAL");
  assert.throws(() => liveArgs([...argv, "--model-a", "C"], { UZI_PHASE_MODEL_LIVE: "1", PROBE_CREDENTIAL: "fixture" }), /duplicate/);
});
test("phase live probe errors redact the supplied secret and account identities", () => {
  const secret = "synthetic-private-probe-value";
  const text = probeError(new Error(`${secret} account test@example.invalid refresh_token=fixture-refresh`), secret);
  assert.ok(!text.includes(secret));
  assert.ok(!text.includes("test@example.invalid"));
  assert.ok(!text.includes("fixture-refresh"));
});

test("both maintainer entrypoints refuse before runtime startup without opt-in or a credential", () => {
  for (const file of ["test/phase-model-live-claude.probe.ts", "../e2e/codex-m4/phase-model-live-codex.probe.ts"]) {
    const argv = ["--credential-env", file.includes("claude") ? "CLAUDE_CODE_OAUTH_TOKEN" : "CODEX_HOME",
      "--model-a", "A", "--model-b", "B", "--effort-a", "high", "--effort-b", "medium"];
    for (const optedIn of [false, true]) {
      const run = spawnSync(process.execPath, ["--import", "tsx", file, ...argv], {
        env: { PATH: process.env.PATH, HOME: os.tmpdir(), ...(optedIn ? { UZI_PHASE_MODEL_LIVE: "1" } : {}) },
        encoding: "utf8", timeout: 10_000,
      });
      assert.equal(run.status, 1);
      assert.match(run.stderr, optedIn ? /named credential variable is absent/ : /UZI_PHASE_MODEL_LIVE=1/);
      assert.equal(run.stdout, "");
    }
  }
});
