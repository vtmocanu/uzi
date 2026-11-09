import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { randomUUID } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { loadConfig } from "../src/config.js";
import { buildWorkerGitCache, buildRecoveryLiveTiming, buildAfterPublicationEnrolled } from "../src/main.js";
import { GitCache } from "../src/git.js";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

test("startup uses the live worker GitCache factory", () => {
  const main = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");
  assert.match(main, /const git = buildWorkerGitCache\(config, log, \(runId\) => outbox\.hasPhysicalTerminalProtection\(runId\), authority\)/);
  assert.equal((main.match(/new GitCache\(/g) ?? []).length, 1);
});

test("fast recovery redrive is opt-in and ignored with a warning outside stub", () => {
  const warnings: string[] = [];
  const log = nullLogger();
  log.warn = message => { warnings.push(message); };
  assert.equal(buildRecoveryLiveTiming("stub", log, {}), undefined);
  assert.equal(buildRecoveryLiveTiming("stub", log, { UZI_E2E_RECOVERY_FAST_REDRIVE: "500" }), undefined);
  assert.equal(buildRecoveryLiveTiming("sdk", log, { UZI_E2E_RECOVERY_FAST_REDRIVE: "1" }), undefined);
  assert.deepEqual(warnings, ["UZI_E2E_RECOVERY_FAST_REDRIVE ignored outside the stub executor"]);
  assert.deepEqual(buildRecoveryLiveTiming("stub", log, { UZI_E2E_RECOVERY_FAST_REDRIVE: "1" }),
    { liveBackoffBaseMs: 1000, liveBackoffCapMs: 5000 });
  const main = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");
  assert.match(main, /recoveryLiveTiming: buildRecoveryLiveTiming\(config.executor, log\)/);
});

test("invalid decoded configuration stops worker startup with a clear error", () => {
  const child = spawnSync(process.execPath, ["--import", "tsx", "src/main.ts"], {
    cwd: new URL("..", import.meta.url),
    env: { ...process.env, UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: "fixture-token",
      UZI_RECOVERY_DECODED_LIMIT_BYTES: "invalid-startup-cap" },
    encoding: "utf8", timeout: 20_000,
  });
  assert.ifError(child.error);
  assert.equal(child.status, 1, child.stderr);
  assert.match(child.stderr, /UZI_RECOVERY_DECODED_LIMIT_BYTES must be decimal bytes in the range 1\.\.68719476736/);
  assert.doesNotMatch(child.stderr, /invalid-startup-cap/);
});

test("direct GitCache caps default to 8 GiB and reject invalid numbers", () => {
  assert.equal(new GitCache("/unused", nullLogger()).recoveryDecodedLimitBytes, 8589934592);
  for (const value of [0, -1, 1.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1, 68719476737, null]) {
    assert.throws(() => new GitCache("/unused", nullLogger(), undefined, {
      recoveryDecodedLimitBytes: value as number,
    }), { message: "UZI_RECOVERY_DECODED_LIMIT_BYTES must be decimal bytes in the range 1..68719476736" });
  }
  for (const value of [1, 8589934592, 68719476736]) {
    assert.equal(new GitCache("/unused", nullLogger(), undefined, { recoveryDecodedLimitBytes: value }).recoveryDecodedLimitBytes, value);
  }
});

for (const override of [undefined, "4096", "1"]) {
  test(`production startup injects parsed cap ${override ?? "default"} into real Git verification`, async () => {
    const fx = makeFixture();
    try {
      const config = loadConfig({ UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: "fixture-token",
        UZI_DATA_DIR: fx.dataDir, UZI_RECOVERY_DECODED_LIMIT_BYTES: override });
      const protectedRuns: string[] = [];
      const protection = async (runId: string) => { protectedRuns.push(runId); return runId === "protected"; };
      const refuse = async (): Promise<never> => { throw new Error("hooks are not exercised by this test"); };
      const cache = buildWorkerGitCache(config, nullLogger(), protection, {
        hooks: { admitSourceUse: refuse, beginBareMutation: refuse, assertBareClear: refuse, beginDestructive: refuse },
      });
      const expected = override === undefined ? 8589934592 : Number(override);
      assert.equal(cache.recoveryDecodedLimitBytes, expected, "startup effective cap");
      const internals = cache as any;
      assert.equal(await internals.terminalRecordProtection("protected"), true);
      assert.equal(await internals.terminalRecordProtection("other"), false);
      assert.deepEqual(protectedRuns, ["protected", "other"]);
      const bare = await cache.ensureClone(fx.originPath);
      const tip = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      const verify = () => cache.verifyRecoveryClosure(bare, tip);
      if (expected === 1) await assert.rejects(verify(), RecoveryClosureLimitError);
      else assert.equal(await verify(), true);
      await cache.withRecoveryOperation(new AbortController().signal, Date.now() + 10_000, async () => {
        assert.equal(internals.recoveryOperations.getStore().closureBudget.limit, expected, "startup operation budget");
        if (expected === 1) await assert.rejects(verify(), RecoveryClosureLimitError);
        else assert.equal(await verify(), true);
      });
    } finally { fx.cleanup(); }
  });
}

test("publication cut builder is absent for sdk/codex and inert without its exclusive sentinel", async () => {
  const log = nullLogger();
  log.info = () => { throw new Error("unexpected readiness"); };
  assert.equal(buildAfterPublicationEnrolled("sdk", log), undefined);
  // Claude and Codex both use the sdk executor configuration.
  assert.equal(buildAfterPublicationEnrolled(loadConfig({ UZI_API_URL: "http://api:8080",
    UZI_WORKER_TOKEN: "fixture-token", UZI_EXECUTOR: "sdk" }).executor, log), undefined);
  const callback = buildAfterPublicationEnrolled("stub", log)!;
  const before = process.getActiveResourcesInfo().sort();
  for (const issueDescription of ["", "UZI_STUB_OUTBOX_CUT_BARRIER", "UZI_STUB_OUTBOX_FIRST_CUT_BARRIER"]) {
    await callback({ runId: "invalid-inert-id", claimGeneration: 1, issueDescription });
  }
  assert.deepEqual(process.getActiveResourcesInfo().sort(), before);
  const main = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");
  assert.match(main, /afterPublicationEnrolled: buildAfterPublicationEnrolled\(config.executor, log\)/);
});

test("publication sentinel announces only stable identity fields after arming the marker wait", async () => {
  const runId = randomUUID(), marker = `/tmp/cdr-outbox-cut-${runId}`;
  const log = nullLogger();
  const lines: unknown[] = [];
  log.info = (message, fields) => {
    lines.push({ message, fields });
    fs.writeFileSync(marker, "cut");
  };
  try {
    await buildAfterPublicationEnrolled("stub", log)!({ runId, claimGeneration: 4,
      issueDescription: "UZI_STUB_OUTBOX_PUBLICATION_CUT_BARRIER" });
    assert.deepEqual(lines, [{ message: "stub: outbox publication enrolled waiting for API cut",
      fields: { run_id: runId, claim_generation: 4 } }]);
    assert.equal(fs.existsSync(marker), false);
  } finally { fs.rmSync(marker, { force: true }); }
});
