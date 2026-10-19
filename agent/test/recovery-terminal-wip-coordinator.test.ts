import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash, createHmac } from "node:crypto";
import { RecoveryCoordinator, canonicalJson, type ClosedSourceObservation, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { recordingLogger } from "./helpers.js";

const runId = "00000000-0000-4000-8000-000000000031";
const holdId = "00000000-0000-4000-8000-000000000032";
const repoId = "00000000-0000-4000-8000-000000000033";
const workerId = "00000000-0000-4000-8000-000000000034";
const workerToken = "origin-admission-fixture";
const sha = "a".repeat(40), origin = "b".repeat(64);
const digest = (value: unknown) => createHash("sha256").update(canonicalJson(value)).digest("hex");

async function fixture(t: test.TestContext, mode: string) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/terminal-origin-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const recoveryRoot = path.join(root, "recovery");
  const ledgerRoot = path.join(root, "recovery-maintenance");
  const dir = path.join(recoveryRoot, runId);
  const clonePath = path.join(root, "source");
  await fs.mkdir(clonePath);
  const sourceFiles = ["HEAD", "index", "config", "tracking", "owed", "WIP"];
  for (const name of sourceFiles) await fs.writeFile(path.join(clonePath, name), "original-" + name);
  const genesis = { runId, generation: 1, clonePath, attemptId: "20261018T120000Z-g1-1234567890abcdef" };
  const observation: ClosedSourceObservation = {
    genesis, terminal: "normal_failed",
    originalContext: { barePath: path.join(root, "repo.git"), branch: "task", key: "key", kind: "issue",
      defaultBranch: "main", cloneUrl: "https://example.com/org/repo.git", repoId, baseCommit: "c".repeat(40),
      context: { runId, generation: 1, barePath: path.join(root, "repo.git"), branch: "task", kind: "issue",
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) } } },
    family: { originals: [{ ...genesis, identity: "1:2" }],
      observedExecution: { runId, generation: 1 }, metadata: "original-metadata" },
  };
  let stopped = false, registration = true, chargedRegistration = true, closed = false, executing = false, open = true;
  let originChanged = false;
  let metadata = observation.family!.metadata;
  let repo = repoId, quiescent = true, quiesceCalls = 0, holdCalls = 0, headCalls = 0, sourceHead = sha;
  let chargedRpc = 0, discoveryRpc = 0;
  let afterCharge: (() => Promise<void> | void) | undefined;
  const signal = new AbortController();
  const token = Object.freeze({}), chargedToken = Object.freeze({});
  const events: string[] = [];
  const client: RecoveryArchiveClient = {
    observeTerminalMaintenanceHold: async () => {
      holdCalls++;
      // Model the strict client's two actual transports, each charged once.
      for (const attempt of [1, 1]) {
        MaintenanceResources.consumeIfActive("rpcAttempts", attempt);
        if (MaintenanceResources.discoveryActive()) discoveryRpc += attempt;
        else chargedRpc += attempt;
      }
      if (closed || repo !== repoId) return { ok: false, reason: "invalid_exact_hold" };
      return { ok: true, token: holdCalls === 1 ? token : chargedToken, hold: { run_id: runId, generation: 1, hold_id: holdId, repo_id: repo,
        original_worker_id: workerId, original_worker_identity: "human-label",
        live_worker_id: workerId, live_run_id: runId, inventory_guarded: true, state: "open",
        run_status: "failed", current_claim_generation: 1, claim_released: true } };
    },
    isCurrentTerminalMaintenanceHold: candidate => registration &&
      (candidate === token || (candidate === chargedToken && chargedRegistration)),
    listRecoveryHolds: async () => { throw new Error("advisory listing forbidden"); },
    reserveRecoveryCapture: async () => { throw new Error("reserve forbidden"); },
    uploadRecoveryBundle: async () => { throw new Error("upload forbidden"); },
    getRecoveryCaptureStatus: async () => { throw new Error("status forbidden"); },
    releaseRecoveryCustody: async () => { throw new Error("FINAL forbidden"); },
  };
  const git = {
    matchMaintenanceSourceFamily: async () => {
      if (!MaintenanceResources.discoveryActive()) {
        events.push("paid-scan");
        assert.ok((await table()).cycles[0].slots.length >= 1, "proof follows durable charge");
        if (afterCharge) { const action = afterCharge; afterCharge = undefined; await action(); }
      }
      return stopped ? undefined : { sources: observation.family!.originals, metadata };
    },
    maintenanceOriginDigest: async () => originChanged ? "f".repeat(64) : origin,
    worktreeHead: async () => { headCalls++; assert.equal(MaintenanceResources.discoveryActive(), false); return sourceHead; },
  };
  const log = recordingLogger();
  const make = (): RecoveryCoordinator => new RecoveryCoordinator({
    recoveryRoot, workerToken, client, git: git as never, log: log.logger, isExecuting: () => executing,
    withTerminalSourceBoundary: async (_run, _paths, action, boundarySignal) => {
      assert.ok(boundarySignal instanceof AbortSignal);
      assert.equal(boundarySignal.aborted, false);
      if (!open || executing) return { entered: false };
      return { entered: true, value: await action({
        open: () => open && !executing,
        isLivePath: () => executing,
        quiesce: async () => { throw new Error("bridge owns quiescence"); },
        captureWip: mode === "missing-bridge" ? undefined : async (_path, oldHead, callbacks) => {
          quiesceCalls++; events.push("quiesce");
          if (!quiescent) throw new Error("not quiescent");
          events.push("initial-quiescence");
          if (mode === "initial-stop") stopped = true;
          await callbacks.beforeStep("status", (mode === "missing-phase" ? undefined :
            mode === "wrong-phase" ? "unknown" : mode === "dispatch-first" ? "dispatch" : "prepare") as "prepare");
          const pinned = await coordinator.inspect(runId);
          assert.equal(pinned.length, 1);
          assert.equal(pinned[0]!.sourceSha, sha);
          assert.equal(pinned[0]!.state, "pinned");
          events.push("authenticated-before-raw");
          for (const step of ["status", "add", "commit"] as const) {
            if (step !== "status") await callbacks.beforeStep(step, "prepare");
            if ((step === "add" && mode === "before-add-stop") ||
                (step === "commit" && mode === "before-commit-stop")) closed = true;
            if (mode === "local-" + step + "-prepare-stop") stopped = true;
            if (mode === "local-" + step + "-prepare-origin") originChanged = true;
            if (mode === "local-" + step + "-prepare-head") sourceHead = "f".repeat(40);
            if (mode === "local-" + step + "-prepare-token") chargedRegistration = false;
            if (mode.startsWith("local-" + step + "-prepare-")) await callbacks.beforeStep(step, "prepare");
            if (mode === "local-status-dispatch-stop") stopped = true;
            if (mode === "local-status-dispatch-origin") originChanged = true;
            if (mode === "local-status-dispatch-head") sourceHead = "f".repeat(40);
            if (mode === "local-status-dispatch-token") chargedRegistration = false;
            await callbacks.beforeStep(step, (step === "add" && mode === "late-missing-phase" ? undefined :
              step === "add" && mode === "late-wrong-phase" ? "unknown" : "dispatch") as "dispatch");
            events.push("git-" + step);
            if (step === "commit") sourceHead = "e".repeat(40);
            if (step === "commit" && mode === "post-commit-stop") closed = true;
            if (step === "commit" && mode === "post-commit-custody") registration = false;
            if (mode === "local-after-" + step + "-stop") stopped = true;
            if (mode === "local-after-" + step + "-origin") originChanged = true;
            if (mode === "local-after-" + step + "-head") sourceHead = "f".repeat(40);
            if (mode === "local-after-" + step + "-token") chargedRegistration = false;
            await callbacks.afterRunnerGit(step);
          }
          if (mode === "return-stop") closed = true;
          if (mode === "return-family") stopped = true;
          if (mode === "return-origin") originChanged = true;
          if (mode === "return-custody") repo = "other";
          if (mode === "wrong-head") sourceHead = "f".repeat(40);
          return (mode === "invalid-kind" ? { kind: "other", head: sourceHead, parent: oldHead } :
            mode === "invalid-parent" ? { kind: "committed", head: sourceHead, parent: "f".repeat(40) } :
            mode === "invalid-clean" ? { kind: "clean", head: sourceHead, parent: null } :
            mode === "invalid-sha" ? { kind: "committed", head: "bad", parent: oldHead } :
            { kind: "committed", head: "e".repeat(40), parent: oldHead }) as never;
        },
      }) };
    },
  });
  const coordinator = make();
  const table = async () => JSON.parse(await fs.readFile(path.join(ledgerRoot, "complete.json"), "utf8")).value;
  const run = (target = coordinator, obs = observation) => target.resumeTerminalSource(runId, Promise.resolve(obs),
    obs.family!.observedExecution.generation, signal.signal);
  const prefixes = async () => (await fs.readdir(dir).catch(() => [])).filter(n => /^maintenance-v1-.+\.json$/.test(n));
  const chargeCount = async () => ((await table().catch(() => undefined))?.cycles ?? []).reduce(
    (n: number, cycle: { slots: unknown[] }) => n + cycle.slots.length, 0);
  const unchanged = async () => {
    for (const name of sourceFiles) assert.equal(await fs.readFile(path.join(clonePath, name), "utf8"), "original-" + name);
  };
  const seal = async (record: RecoveryRecord) => {
    await fs.mkdir(dir, { recursive: true });
    const key = createHmac("sha256", workerToken).update("uzi-recovery-journal-v1").digest();
    await fs.writeFile(path.join(dir, record.captureId + ".json"), JSON.stringify({ ...record,
      mac: createHmac("sha256", key).update(canonicalJson(record)).digest("hex") }));
  };
  return { root, dir, ledgerRoot, recoveryRoot, client, git, coordinator, make, run, observation, prefixes, table,
    chargeCount, unchanged, seal, events, signal, log,
    stopped: () => { stopped = true; }, close: () => { closed = true; },
    registration: () => { registration = false; }, crossRepo: () => { repo = "other"; },
    active: () => { executing = true; }, boundaryClosed: () => { open = false; },
    unquiescent: () => { quiescent = false; }, headChanged: () => { sourceHead = "e".repeat(40); },
    afterCharge: (action: () => Promise<void> | void) => { afterCharge = action; },
    metadata: (value: string) => { metadata = value; },
    counts: () => ({ quiesceCalls, holdCalls, headCalls, chargedRpc, discoveryRpc }),
    tokenState: () => [client.isCurrentTerminalMaintenanceHold!(token),
      client.isCurrentTerminalMaintenanceHold!(chargedToken)] };
}

for (const mode of ["committed", "missing-bridge", "initial-stop", "before-add-stop", "before-commit-stop", "post-commit-stop",
  "post-commit-custody", "return-stop", "return-family", "return-origin", "return-custody", "wrong-head",
  "invalid-kind", "invalid-parent", "invalid-clean", "invalid-sha"]) {
  test("terminal WIP coordinator: " + mode, async t => {
    const f = await fixture(t, mode);
    await f.run();
    assert.equal(await f.chargeCount(), mode === "missing-bridge" ? 0 : 1);
    if (mode === "missing-bridge") { assert.deepEqual(f.events, []); return; }
    const cycle = (await f.table()).cycles[0];
    assert.equal(cycle.fenced, true);
    assert.equal(cycle.slots.length, 1);
    const records = await f.coordinator.inspect(runId);
    if (mode === "initial-stop") {
      assert.equal(records.length, 0);
      assert.ok(!f.events.includes("git-status"));
      return;
    }
    assert.equal(records.length, 1);
    const record = records[0]!;
    assert.equal(record.sourceSha, sha);
    assert.equal(record.attemptedHeadSha, undefined);
    assert.equal(record.heldPolicy, undefined);
    assert.ok(f.events.indexOf("initial-quiescence") < f.events.indexOf("authenticated-before-raw"));
    assert.ok(f.events.indexOf("authenticated-before-raw") < f.events.indexOf("git-status"));
    const expected = [digest(record.captureId)];
    if (mode === "committed") expected.push(digest(["maintenance-wip-v1", cycle.identity, "e".repeat(40)]));
    assert.deepEqual(cycle.outputs, expected);
    if (mode === "before-add-stop") assert.ok(!f.events.includes("git-add"));
    if (mode === "before-commit-stop") assert.ok(!f.events.includes("git-commit"));
    await f.run(f.make());
    assert.equal(await f.chargeCount(), 1);
    assert.deepEqual((await f.table()).cycles[0].outputs, expected);
  });
}

for (const directories of [0, 1, 2]) test("terminal WIP charged transports with " + directories + " cold directories", async t => {
  const f = await fixture(t, "committed");
  if (directories < 2) await fs.mkdir(f.recoveryRoot);
  if (directories === 0) await fs.mkdir(f.dir);
  await f.run();
  assert.equal((await f.table()).cycles[0].outputs.length, 2);
  assert.equal(f.counts().chargedRpc, 12 + 2 * directories);
  assert.equal(f.counts().discoveryRpc, 4);
  t.diagnostic(JSON.stringify(f.counts()));
});

for (const mode of ["missing-phase", "wrong-phase", "dispatch-first", "late-missing-phase", "late-wrong-phase"])
test("terminal WIP refuses unspecified callback authority: " + mode, async t => {
  const f = await fixture(t, mode);
  await f.run();
  const cycle = (await f.table()).cycles[0];
  assert.equal(cycle.fenced, true);
  assert.equal(cycle.slots.length, 1);
  assert.equal(cycle.outputs.length, mode.startsWith("late-") ? 1 : 0);
  assert.ok(!f.events.includes(mode.startsWith("late-") ? "git-add" : "git-status"));
  assert.equal(f.counts().discoveryRpc, 4);
});

for (const point of ["status-dispatch", "add-prepare", "commit-prepare", "after-status", "after-add", "after-commit"])
for (const change of ["stop", "origin", "head", "token"]) {
  // Commit's post-proof intentionally omits old HEAD until the verified result.
  if (point === "after-commit" && change === "head") continue;
  test("terminal WIP local read-only refusal: " + point + " " + change, async t => {
    const f = await fixture(t, "local-" + point + "-" + change);
    await f.run();
    const cycle = (await f.table()).cycles[0];
    assert.equal(cycle.fenced, true);
    assert.equal(cycle.slots.length, 1);
    assert.equal(cycle.outputs.length, 1, "only immutable original prefix is indexed");
    if (change === "token") assert.deepEqual(f.tokenState(), [true, false], "only the charged observation is invalid");
    const next = point === "after-commit" ? undefined :
      point === "commit-prepare" || point === "after-add" ? "commit" :
      point === "status-dispatch" ? "status" : "add";
    if (next) assert.ok(!f.events.includes("git-" + next));
  });
}
