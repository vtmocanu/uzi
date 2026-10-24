import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { rmTreePinned } from "../src/rmtree.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, git, gitlabClaim, fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, spyPublishLands } from "./runner-codex-sinks-fixture.js";
import { recordingLogger } from "./helpers.js";

installHarness();

type Flight = {
  executor: Executor; worktreePath: string; preserveRecoveryClone?: boolean;
  sourceExitDenied?: boolean; stoppedSourceKept?: boolean; terminalDisposal?: unknown;
  deferredTerminalRelease?: { reason: string }; terminalReleaseOwner?: { veto: boolean };
};
type Seam = {
  retainStoppedSource(flight: Flight, executor: Executor, reason: string,
    handler?: { record: string }): Promise<unknown>;
  terminalProcessProof(flight: Flight): boolean;
};
function barrier() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

const modes = ["healthy", "dirty", "rejected", "incomplete", "foreign-result", "swapped-executor",
  "swapped-facade", "swapped-source", "swapped-generation", "swapped-attempt", "revoked",
  "independent-keep", "keep-during-coverage", "skipped", "skipped-operation", "skipped-executor",
  "skipped-receipt", "skipped-facade", "skipped-identity-swap", "skills-revoked", "late-cancel", "claude"] as const;
for (const [index, mode] of modes.entries()) {
  it(`terminal source release: ${mode}`, async t => {
    const lifecycle = new AbortController();
    const rig = codexRig({ lifecycleSignal: lifecycle.signal });
    const log = recordingLogger();
    const publication = spyPublishLands();
    t.after(publication);
    const claim = gitlabClaim(285600 + index, { claim_generation: 3 });
    const entered = barrier(), release = barrier();
    let flight: Flight | undefined;
    let insideRegion = false, disposeInsideRegion = false, entryReads = 0, checkingEntry = false;
    let deferred = false, coveredCalls = 0;
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) =>
      boundary(request, async permit => {
        insideRegion = true;
        try { return await action(permit); } finally { insideRegion = false; }
      }));
    const capture: typeof rig.capture = (ms, action, purpose, signal, attempt) =>
      rig.capture(ms, async held => {
        insideRegion = true;
        try { return await action(held); } finally { insideRegion = false; }
      }, purpose, signal, attempt);
    for (const name of ["worktreeHead", "worktreeStatus", "verifyStoppedSourceCovered"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (checkingEntry) entryReads++;
        return Reflect.apply(original, git, args);
      });
    }
    const behavior: Executor["run"] = async ctx => {
      ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(ctx.worktreePath, "TERMINAL.txt"), "captured bytes\n");
      fs.mkdirSync(skillsPluginDir(ctx.worktreePath), { recursive: true });
      fs.writeFileSync(path.join(skillsPluginDir(ctx.worktreePath), "proof.marker"), "paired skills\n");
      api.overrideStateStatus(claim.run_id, "failed");
      throw new TransientRecoveryError("terminal proof recovery");
    };
    const executor: Executor = mode === "claude" ? { run: behavior } : new FakeCodexExecutor(rig.safety, behavior, undefined, capture);
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, log.logger, {
      recoveryRetryMs: 1,
      teardownTestDeps: { removeTreePinned: async (parent, name, opts) => {
        if (mode === "skills-revoked" && name === path.basename(skillsPluginDir(flight!.worktreePath))) {
          assert.equal(seam.terminalProcessProof(flight!), true);
          rig.registry.reserveLaunch("provider");
        }
        return rmTreePinned(parent, name, opts);
      } },
    });
    const seam = runner as unknown as Seam;
    const retain = seam.retainStoppedSource.bind(runner);
    t.mock.method(seam, "retainStoppedSource", async (actual: Flight, exec: Executor, reason: string, handler?: { record: string }) => {
      flight = actual;
      checkingEntry = mode !== "claude" && !actual.terminalDisposal && handler?.record === "none";
      try {
        const result = await retain(actual, exec, reason, handler);
        if (checkingEntry) deferred = actual.deferredTerminalRelease?.reason === reason;
        return result;
      } finally { checkingEntry = false; }
    });
    const verify = git.verifyStoppedSourceCovered.bind(git);
    t.mock.method(git, "verifyStoppedSourceCovered", async (...args: Parameters<typeof verify>) => {
      coveredCalls++;
      if (mode === "keep-during-coverage") flight!.preserveRecoveryClone = true;
      return verify(...args);
    });
    // A real registered root supplies the terminal barrier. Reap is separate from
    // disposal, so its final write is observed only after the deferred entry.
    const reserved = rig.registry.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind === "reserved") rig.registry.registerRoot(reserved.reservation, {
      kind: "provider", reap: async () => ({ ok: true }),
      dispose: async () => {
        entered.resolve();
        await release.promise;
        if (mode === "dirty") fs.writeFileSync(path.join(flight!.worktreePath, "FINAL.txt"), "last writer\n");
        if (mode === "incomplete") throw new Error("private-error-sentinel");
      },
    });
    if (mode.startsWith("skipped")) {
      const memory = runner as unknown as { retireWorkerMemory(flight: Flight): void };
      const retireMemory = memory.retireWorkerMemory.bind(runner);
      t.mock.method(memory, "retireWorkerMemory", (actual: Flight) => {
        retireMemory(actual);
        if (mode === "skipped-facade") { executor.safety = undefined; return; }
        if (mode === "skipped-identity-swap") {
          executor.safety = undefined;
          actual.executor = { run: behavior };
          return;
        }
        if (mode === "skipped-receipt") {
          Object.assign(runner, { memoryRuntime: { sinkReceipts: [{ context: { flight: actual } }] } });
        } else {
          const pending = mode === "skipped-operation" ? { operation: Promise.resolve(), settled: false }
            : mode === "skipped-executor" ? { executorRun: Promise.resolve(), executorSettled: false }
            : { closing: Promise.resolve(), closingSettled: false };
          Object.assign(actual, { workerMemory: { ...pending, feedback: new Map() } });
        }
      });
    }
    const dispose = rig.safety.dispose.bind(rig.safety);
    t.mock.method(rig.safety, "dispose", async (request: Parameters<typeof dispose>[0]) => {
      disposeInsideRegion ||= insideRegion;
      if (mode === "rejected" || mode === "foreign-result") {
        entered.resolve();
        await release.promise;
      }
      if (mode === "independent-keep") flight!.preserveRecoveryClone = true;
      if (mode === "rejected") throw new Error("private-error-sentinel");
      if (mode === "foreign-result") return codexRig().safety.dispose(request);
      const result = await dispose(request);
      if (mode === "swapped-executor") flight!.executor = { run: behavior, safety: rig.safety };
      if (mode === "swapped-facade") executor.safety = codexRig().safety;
      if (mode === "swapped-source") flight!.worktreePath += "-swapped";
      if (mode === "swapped-generation") Object.assign(flight!, { claimGeneration: 4 });
      if (mode === "swapped-attempt") Object.assign(flight!, { attemptId: "different-attempt" });
      if (mode === "late-cancel") (flight as unknown as { cancel: AbortController }).cancel.abort();
      return result;
    });
    let tree = "";
    const retire = git.retireRunnerClone.bind(git);
    t.mock.method(git, "retireRunnerClone", async (...args: Parameters<typeof retire>) => {
      if (mode === "revoked") {
        // Revoke the real facade's live registry evidence after coverage, at dispatch.
        assert.equal(seam.terminalProcessProof(flight!), true);
        rig.registry.reserveLaunch("provider");
        assert.equal(seam.terminalProcessProof(flight!), false);
      }
      return retire(...args);
    });
    const execution = runner.execute(claim);
    let entryError: unknown;
    if (mode !== "claude" && !mode.startsWith("skipped")) {
      await entered.promise;
      try {
        assert.ok(flight, "actual recovery handler supplied the flight");
        tree = flight.worktreePath;
        assert.equal(deferred, true, "release request is recorded before terminal disposal");
        assert.equal(entryReads, 0, "handler entry defers every release-dependent source read");
        assert.equal(flight.preserveRecoveryClone, true);
      } catch (error) { entryError = error; } finally { release.resolve(); }
    }
    await execution;
    if (entryError !== undefined) throw entryError;
    assert.equal(disposeInsideRegion, false, "terminal disposal runs outside A/B scopes");
    assert.ok(flight);
    if (mode === "claude") {
      assert.equal(flight.terminalDisposal, undefined);
      assert.equal(flight.deferredTerminalRelease, undefined);
      return;
    }
    if (mode.startsWith("skipped")) tree = flight.worktreePath;
    assert.equal(flight.sourceExitDenied, true, "terminal proof never grants closed-source consumer authority");
    assert.equal(flight.deferredTerminalRelease, undefined, "request is consumed once");
    if (["healthy", "dirty", "keep-during-coverage", "revoked", "skills-revoked"].includes(mode))
      assert.equal(coveredCalls, 1, "the postdisposal coverage path was exercised");
    assert.equal(fs.existsSync(tree), mode !== "healthy" && mode !== "skills-revoked", JSON.stringify({ coveredCalls, veto: flight.terminalReleaseOwner?.veto, disposalRecorded: flight.terminalDisposal !== undefined }));
    assert.equal(fs.existsSync(path.join(skillsPluginDir(tree), "proof.marker")), mode !== "healthy", "paired skills obey the live disposal guard");
    if (["rejected", "incomplete", "foreign-result", "swapped-executor", "swapped-facade", "swapped-source", "swapped-generation", "swapped-attempt", "late-cancel"].includes(mode) || mode.startsWith("skipped")) {
      assert.equal(coveredCalls, 0, "invalid disposal cannot reach coverage");
      assert.ok(JSON.stringify(log.lines).includes("codex terminal disposal unproven"));
      assert.ok(!JSON.stringify(log.lines).includes("private-error-sentinel"));
    }
  });
}
