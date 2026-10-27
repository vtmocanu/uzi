import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Executor, RunContext } from "../src/executor.js";
import type { RecoveryJournalEntry } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { recordingLogger } from "./helpers.js";
import { acknowledgedCompletionInventory } from "./runner-completion-inventory-fixture.js";
import { api, git, gitlabClaim, fakeGitlab, fx, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, spyPublishLands } from "./runner-codex-sinks-fixture.js";

installHarness();

type Flight = {
  executor: Executor; worktreePath: string; barePath?: string; branch?: string;
  attemptId?: string; runnerClone?: { path: string; attemptId?: string };
  preserveRecoveryClone?: boolean; stoppedSourceKept?: boolean;
  deferredTerminalRelease?: unknown; terminalResolved?: boolean;
  completedRetainedSuccessor?: RecoveryJournalEntry; retainedLocalCustody?: boolean;
};
type Seam = {
  reconcileWorkerMemory(flight: Flight): Promise<void>;
  terminalProcessProof(flight: Flight): boolean;
  recovery: ReturnType<typeof acknowledgedCompletionInventory>["coordinator"];
};

function barrier() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

function command(repo: string, ...args: string[]): string {
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
  return execFileSync("git", ["-C", repo, ...args], {
    env: { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

// Top-level controls: nested t.test would reinstall the shared harness.
// Synthetic setup selects finally's rare flight branches BEFORE terminalSource is
// captured. Everything from execute's terminal disposal through Git's destructive
// dispatch is production code, including the live registry witness and pinned rm.
// Successor authority is a real RecoveryCoordinator upload/FINAL/MAC journal using
// the existing synthesized bundle producer; neither ACK nor ledger is hand-written.
for (const branch of ["no-bare", "verified-successor"] as const) {
  for (const revoke of [false, true]) {
    it(`execute terminal dispatch: ${branch} ${revoke ? "revoked" : "live"}`, async t => {
      const rig = codexRig();
      const publication = spyPublishLands();
      t.after(publication);
      const claim = gitlabClaim(285690, { claim_generation: 3 });
      const entered = barrier(), release = barrier();
      let flight!: Flight, context!: RunContext;
      let tree = "", bare = "", journal = "", config: Buffer | undefined;
      let predecessor = "", predecessorBytes = "", predecessorInode = 0;
      let dispatches = 0, acknowledgements = 0, rootDisposals = 0;
      const inventory = acknowledgedCompletionInventory(claim.run_id, 3);
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        context = ctx;
        fs.writeFileSync(path.join(ctx.worktreePath, "dispatch.txt"), "original dirty bytes\n");
        api.overrideStateStatus(claim.run_id, "failed");
        throw new TransientRecoveryError("terminal dispatch fixture");
      }, undefined, rig.capture);
      const log = recordingLogger();
      const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
      const seam = runner as unknown as Seam;
      const reconcile = seam.reconcileWorkerMemory.bind(runner);
      t.mock.method(seam, "reconcileWorkerMemory", async (actual: Flight) => {
        const result = await reconcile(actual);
        if (actual.deferredTerminalRelease && !tree) {
          flight = actual;
          bare = actual.barePath!;
          journal = `uzi-recovery.${actual.branch!}.clone`;
          // Do not turn the recovery release into the behavior under test.
          // Select the terminal dispatch using this actual execute flight.
          actual.deferredTerminalRelease = undefined;
          actual.preserveRecoveryClone = false;
          actual.stoppedSourceKept = false;
          if (branch === "no-bare") {
            actual.barePath = undefined;
          } else {
            predecessor = actual.worktreePath;
            predecessorBytes = fs.readFileSync(path.join(predecessor, "dispatch.txt"), "utf8");
            predecessorInode = fs.statSync(predecessor).ino;
            fs.chmodSync(path.dirname(bare), 0o700);
            fs.chmodSync(bare, 0o700);
            const key = "issue-285690";
            const source = { runId: claim.run_id, clonePath: predecessor };
            const reservation = await git.reserveRecoveryIteration(bare, actual.branch!, key, source, 1);
            const tip = command(predecessor, "rev-parse", "HEAD");
            await git.recordRecoveryCapture(bare, actual.branch!, key, source, reservation.attempts, tip);
            const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 3, "0123456789abcdef");
            const successor = await git.prepareRecoverySuccessor(bare, actual.branch!, key, source, reservation.attempts, attemptId);
            actual.completedRetainedSuccessor = await git.completeRecoveryEpisode(bare, actual.branch!, key,
              { runId: claim.run_id, clonePath: successor.path, attemptId },
              { settled: true, processedEvents: 1, attemptId });
            actual.worktreePath = successor.path;
            actual.runnerClone = successor;
            actual.attemptId = attemptId;
            actual.terminalResolved = true;
            actual.retainedLocalCustody = false;
            claim.inventory_guarded = true;
            fs.writeFileSync(path.join(successor.path, "dispatch.txt"), "successor bytes\n");
            await inventory.acknowledge(context, command(successor.path, "rev-parse", "HEAD"));
            seam.recovery = inventory.coordinator;
          }
          tree = actual.worktreePath;
          fs.mkdirSync(skillsPluginDir(tree), { recursive: true });
          fs.writeFileSync(path.join(skillsPluginDir(tree), "proof.marker"), "paired skills\n");
          config = fs.readFileSync(path.join(bare, "config"));
        }
        return result;
      });
      const reserved = rig.registry.reserveLaunch("provider");
      assert.equal(reserved.kind, "reserved");
      if (reserved.kind === "reserved") rig.registry.registerRoot(reserved.reservation, {
        kind: "provider", reap: async () => ({ ok: true }),
        dispose: async () => { rootDisposals++; },
      });
      if (branch === "no-bare") {
        const remove = git.removeRunnerClone.bind(git);
        let insideRemoval = false;
        t.mock.method(git, "removeRunnerClone", async (...args: Parameters<typeof remove>) => {
          dispatches++;
          assert.equal(args[0], tree);
          assert.equal(seam.terminalProcessProof(flight), true);
          insideRemoval = true;
          try { return await remove(...args); } finally { insideRemoval = false; }
        });
        const open = fsp.open.bind(fsp);
        t.mock.method(fsp, "open", async (...args: Parameters<typeof open>) => {
          const handle = await open(...args);
          if (insideRemoval && String(args[0]) === path.dirname(tree)) {
            entered.resolve();
            await release.promise;
          }
          return handle;
        });
      } else {
        const retire = git.retireRunnerClone.bind(git);
        t.mock.method(git, "retireRunnerClone", async (...args: Parameters<typeof retire>) => {
          dispatches++;
          assert.equal(args[1], tree);
          assert.equal(seam.terminalProcessProof(flight), true);
          const verified = args[4]!.verifiedSuccessor!;
          assert.deepEqual(verified.expected, flight.completedRetainedSuccessor);
          const acknowledged = verified.acknowledged;
          // Pause delivery of the ACTUAL runner callback result, after its real
          // inventoryCleanupState and post-await check. This isolates Git's next
          // processProof recheck from acknowledged's independent live guard.
          verified.acknowledged = async () => {
            const value = await acknowledged();
            acknowledgements++;
            // The second check is writeRecovery's pre-commit guard. Omitting
            // runner forwarding then actually publishes the predecessor anchor
            // before a later independent FINAL check can refuse.
            if (acknowledgements === 2) {
              assert.equal(value, true, "actual runner FINAL callback authorized this exact successor");
              entered.resolve();
              await release.promise;
            }
            return value;
          };
          return retire(...args);
        });
      }
      const execution = runner.execute(claim);
      try {
        assert.equal(await Promise.race([entered.promise.then(() => true), execution.then(() => false)]), true,
          "execute must reach the actual paused destructive dispatch: " + JSON.stringify({ tree, dispatches, log: log.lines }));
        assert.equal(rootDisposals, 1, "registered root was genuinely disposed");
        assert.equal(seam.terminalProcessProof(flight), true);
        assert.equal(fs.existsSync(tree), true);
        if (revoke) {
          assert.deepEqual(rig.registry.reserveLaunch("provider"), { kind: "denied", reason: "admission_closed" });
          assert.equal(seam.terminalProcessProof(flight), false, "denied post-disposal admission revokes the actual facade");
        }
      } finally {
        release.resolve();
        await execution;
      }
      assert.equal(dispatches, 1, "actual execute finally selected exactly the intended Git dispatch");
      if (branch === "verified-successor") assert.ok(acknowledgements > 0);
      assert.equal(fs.existsSync(tree), revoke, "revoked forwarding retains clone; live counterpart deletes it");
      assert.equal(fs.existsSync(path.join(skillsPluginDir(tree), "proof.marker")), revoke,
        "paired skills obey actual terminal proof");
      if (revoke) {
        assert.equal(fs.readFileSync(path.join(tree, "dispatch.txt"), "utf8"), branch === "no-bare" ? "original dirty bytes\n" : "successor bytes\n");
        const currentConfig = fs.readFileSync(path.join(bare, "config"));
        assert.ok(config);
        if (!currentConfig.equals(config)) {
          let mismatch = 0;
          while (mismatch < Math.min(currentConfig.length, config.length) && currentConfig[mismatch] === config[mismatch]) mismatch++;
          assert.fail(`journal and attempt ledger changed at byte ${mismatch} (expected length ${config.length}, actual ${currentConfig.length})`);
        }
        assert.ok(command(bare, "config", "--get", journal));
      }
      if (predecessor) {
        assert.equal(fs.readFileSync(path.join(predecessor, "dispatch.txt"), "utf8"), predecessorBytes);
        assert.equal(fs.statSync(predecessor).ino, predecessorInode, "predecessor custody remains in place");
        assert.equal(await git.classifyOwnerClonePath(bare, flight.branch!, "issue-285690", claim.run_id, predecessor), "canonical");
        assert.ok(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id));
      }
      assert.ok(tree.startsWith(fx.dataDir + path.sep));
    });
  }
}
