import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { SinkGate } from "../src/sink-gate.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Warn entries in this matrix already have real caller coverage:
// runner-pause-park.test.ts: "Claude owner pause publishes current work despite
// false/throw command settlement"; runner-gate-checkpoint-witness.test.ts:
// "claude-false/claude-throw: only a confirmed recoverable publish presents a witness".
// These strict entries use plain Claude executors and keep runFreeCapture real.
for (const site of ["credential exit", "credential deferred loop"] as const) {
  for (const outcome of ["false", "throw", "confirmed"] as const) {
    it(`capture policy: ${site} / ${outcome}`, { timeout: 60_000 }, async t => {
      const mask = process.umask(0o022);
      t.after(() => { process.umask(mask); });
      const claim = gitlabClaim(285201, { claim_generation: 3 });
      const log = recordingLogger();
      let entered = 0, settlements = 0, waits = 0;
      let released = outcome === "confirmed";
      let clone = "";
      const session = path.join(homeDir, claim.run_id);
      let markers = 0, fetches = 0, publications = 0;
      const marker = git.commitWipMarker.bind(git);
      t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
        markers++; return marker(...args);
      });
      const fetch = git.fetchAgentBranch.bind(git);
      t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof fetch>) => {
        fetches++; return fetch(...args);
      });
      const publish = client.publishCheckpoint.bind(client);
      t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
        publications++; return publish(...args);
      });
      const executor: Executor = {
        run: async ctx => {
          entered++;
          clone = ctx.worktreePath;
          fs.mkdirSync(session, { recursive: true, mode: 0o700 });
          fs.writeFileSync(path.join(session, "session"), "transcript", { mode: 0o600 });
          fs.writeFileSync(path.join(clone, "ONLY_COPY.txt"), "uncommitted work\n");
          api.setOwnershipStatus(claim.run_id, site === "credential exit" ? "paused" : "running", 3);
          if (site === "credential exit") api.overrideStateStatus(claim.run_id, "paused");
          throw Object.assign(new Error("credential deferred"), { name: "CodexBoundaryError", deferral: "vault_locked" });
        },
        settleWorkerCommands: async () => {
          if (!entered) return { confirmed: true };
          settlements++;
          if (released) return { confirmed: true };
          if (outcome === "throw") throw new Error("command protocol failure");
          return { confirmed: false };
        },
      };
      const runner = runnerWith(() => ({ executor, homeDir: session }), fakeGitlab().gitlab,
        undefined, log.logger, { recoveryRetryMs: 1, checkpointIntervalMs: 0 });
      const owner = runner as unknown as { waitRecoveryRetry(): Promise<void> };
      // Each incomplete iteration must preserve custody. Seven exceeds the old five-proof cap.
      t.mock.method(owner, "waitRecoveryRetry", async () => {
        waits++;
        assert.equal(markers, 0);
        assert.equal(fetches, 0);
        assert.equal(publications, 0);
        assert.equal(api.states.some(s => ["recovery_wait", "failed"].includes(s.body.status)), false);
        assert.equal(fs.readFileSync(path.join(clone, "ONLY_COPY.txt"), "utf8"), "uncommitted work\n");
        assert.equal(fs.readFileSync(path.join(session, "session"), "utf8"), "transcript");
        if (waits === 7) released = true;
        if (waits > 10) { runner.shutdown(); throw new Error("capture did not recover after settlement release"); }
      });
      try {
        assert.equal(claim.secrets.codex, undefined);
        assert.equal(executor.safety, undefined);
        assert.equal(executor.withCredentialFreeCapture, undefined);
        await runner.execute(claim);
        assert.equal(entered, 1);
        assert.ok(settlements > 0, "the targeted caller reached command settlement after executor entry");
        if (site === "credential exit" && outcome !== "confirmed") {
          assert.equal(waits, 0, "held ownership takes exit capture before the deferred loop");
          assert.deepEqual({ markers, fetches, publications }, { markers: 0, fetches: 0, publications: 0 });
          assert.equal(fs.readFileSync(path.join(clone, "ONLY_COPY.txt"), "utf8"), "uncommitted work\n");
          assert.equal(fs.readFileSync(path.join(session, "session"), "utf8"), "transcript");
          assert.ok(log.lines.some(line => (line as { msg?: string }).msg ===
            "credential recovery park: the run is not running and cannot resume here; retaining work, not parking"));
        } else {
          assert.ok(markers > 0 && fetches > 0 && publications > 0, "confirmed control enters the real capture and publication body");
          assert.equal(waits, outcome === "confirmed" ? 0 : 7);
        }
        assert.equal(api.states.filter(s => s.body.status === "recovery_wait").length,
          site === "credential exit" ? 0 : 1);
        assert.equal(api.states.some(s => s.body.status === "failed"), false);
      } finally { runner.shutdown(); process.umask(mask); }
    });
  }
}

for (const outcome of ["false", "throw", "confirmed"] as const) {
  it(`capture policy: credential-free wrapper / ${outcome}`, async t => {
    const claim = gitlabClaim(285202);
    let settlements = 0, sourceOps = 0;
    const executor: Executor = {
      run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async () => {
        settlements++;
        if (outcome === "throw") throw new Error("command protocol failure");
        return { confirmed: outcome === "confirmed" };
      },
    };
    // Minimal caller flight: keep the real gate, wrapper, and body. A status read is
    // the admission witness; unreadable status deliberately ends the body before mutation.
    const flight = {
      runId: claim.run_id, executor, runLog: nullLogger(), barePath: "fixture-bare",
      worktreePath: "fixture-source", branch: "issue-285202",
      survivingTickGroups: [],
      sinkGate: new SinkGate(),
      preserveRecoveryClone: false, preserveSession: false,
    };
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const owner = runner as unknown as {
      quiesceRun: (...args: unknown[]) => Promise<{ blocked: boolean }>;
      captureRecoveryRestorePoint: (claim: unknown, flight: unknown, log: unknown, site: string,
        opts: { credentialFree: true }) => Promise<unknown>;
    };
    t.mock.method(owner, "quiesceRun", async () => ({ blocked: false }));
    t.mock.method(git, "worktreeStatus", async () => { sourceOps++; return null; });
    assert.equal(claim.secrets.codex, undefined);
    assert.equal(executor.safety, undefined);
    assert.equal(executor.withCredentialFreeCapture, undefined);
    const result = await owner.captureRecoveryRestorePoint(claim, flight, flight.runLog,
      "recovery_capture", { credentialFree: true });
    assert.equal(settlements, 1, "wrapper itself reaches settlement without heldCapture");
    if (outcome === "confirmed") {
      assert.equal(sourceOps, 1, "confirmed settlement admits the actual body's source read");
      assert.deepEqual(result, { verified: false, published: false });
    } else {
      assert.equal(sourceOps, 0);
      assert.deepEqual(result, { verified: false, published: false, residueBlocked: true,
        residueDetail: "credential-free capture unverified" });
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
    }
  });
}
