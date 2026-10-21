import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { SteeringChannel } from "../src/steering.js";
import { recordingLogger } from "./helpers.js";
import { api, client, deferred, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// A fresh source is observed only through the Linux-only quiescence / stopped-source proofs
// (src/runner.ts "non_linux_host" refusal, src/run-quiescence.ts), so it never qualifies on macOS (#1912).
// Linux runs every assertion.
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only run-quiescence / stopped-source proof (src/run-quiescence.ts, src/runner.ts refuse off Linux)"
  : false;

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
type Observation = {
  genesis: { runId: string; generation: number; clonePath: string; attemptId: string };
  terminal?: string;
  family?: { originals: Array<{ runId: string; generation: number; clonePath: string; attemptId: string; identity: string }>;
    observedExecution: { runId: string; generation: number }; metadata: string };
};
type Tails = { executionTails: ReadonlyMap<string, Promise<Observation | undefined>> };

for (const mode of ["normal", "unknown-sibling", "unknown-ledger", "malformed-ledger", "missing-family", "stopped-marker", "reclaimed-attempt", "unadmitted-restart", "adopted", "failed-stop", "late-fence"] as const) {
it(`#3031 actual pending fresh guarded park family: ${mode}`, { skip: mode === "adopted" ? false : LINUX_ONLY }, async (t) => {
  const claim = gitlabClaim(3031, { claim_generation: 1, inventory_guarded: true });
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) => name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  let parkedGeneration = 1;
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "fixture-hold", generation: parkedGeneration, inventory_guarded: true, has_available_capture: false,
  }] }));
  t.mock.method(client, "reserveRecoveryCapture", async () => ({
    capture_id: "00000000-0000-4000-8000-000000003031", state: "preparing" as const,
  }));
  t.mock.method(client, "uploadRecoveryBundle", async (...[_run, _capture, _manifest, stream]: Parameters<typeof client.uploadRecoveryBundle>) => {
    for await (const chunk of stream) void chunk;
    throw new Error("archive transport unavailable");
  });
  t.mock.method(client, "getRecoveryCaptureStatus", async () => { throw new Error("archive transport unavailable"); });
  t.mock.method(client, "publishCheckpoint", async (...[_run, _tip, stream]: Parameters<typeof client.publishCheckpoint>) => {
    for await (const chunk of stream) void chunk;
    return { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
  });
  const cleaning = deferred(), finishCleanup = deferred(), ackStarted = deferred(), releaseAck = deferred();
  let models = 0, source = "", branch = "", parked = false, discoveries = 0;
  const discover = git.discoverRetainedRecovery.bind(git);
  t.mock.method(git, "discoverRetainedRecovery", async (...args: Parameters<typeof discover>) => {
    discoveries++;
    if (discoveries === 2) {
      const bare = git.barePathFor(claim.repo.clone_url);
      const raw = command(bare, "config", "--get", "uzi-recovery." + branch + ".clone");
      const journal = JSON.parse(raw);
      if (mode === "unknown-sibling") await fs.mkdir(path.join(path.dirname(source), "untracked-source"));
      if (mode === "missing-family") await fs.rm(source, { recursive: true });
      if (mode === "stopped-marker") command(bare, "config", "--add", "uzi-stopped." + claim.run_id + ".entry", "malformed");
      if (mode === "malformed-ledger") command(bare, "config", "--add", "uzi-attempts." + branch + ".entry", "{");
      if (mode === "unknown-ledger") command(bare, "config", "--add", "uzi-attempts." + branch + ".entry", JSON.stringify({
        runId: journal.runId, attemptId: "20261018T120000Z-g1-1234567890abcdef",
        clonePath: path.join(path.dirname(source), "issue-3031.attempt-20261018T120000Z-g1-1234567890abcdef"), state: "live",
      }));
      if (mode === "reclaimed-attempt") {
        // Crash cut after the stopped ledger append, before the ordinary journal
        // clear. Keep the source discoverable so this tests refusal of that source,
        // rather than the unrelated fresh-attempt route after journal removal.
        command(bare, "config", "--add", "uzi-attempts." + branch + ".entry", JSON.stringify({
          attemptId: journal.attemptId, runId: journal.runId, clonePath: journal.clonePath, state: "reclaimed",
        }));
      }
    }
    return discover(...args);
  });
  const log = recordingLogger();
  const runner = runnerWith(() => ({
    homeDir: path.join(homeDir, claim.run_id),
    executor: {
      run: async ctx => {
        models++;
        source = ctx.worktreePath;
        branch = ctx.branch;
        command(source, "config", "user.name", "fixture");
        command(source, "config", "user.email", "fixture@example.com");
        await fs.writeFile(path.join(source, "unique-commit"), randomBytes(16 * 1024));
        command(source, "add", "unique-commit");
        command(source, "commit", "-m", "unique local work");
        await fs.writeFile(path.join(source, "README.md"), "tracked dirty work\n");
        await fs.writeFile(path.join(source, "unique-untracked"), "untracked dirty work\n");
        const pausedAt = { completedCount: 0 };
        assert.equal(await ctx.parkForPause?.(pausedAt), true);
        parked = true;
        return { branch: ctx.branch, pausedAt };
      },
      reapAttributedProcesses: async () => {
        if (parked) {
          cleaning.resolve();
          await finishCleanup.promise;
        }
        return { complete: true, killed: [], left: [] };
      },
    },
  }), fakeGitlab().gitlab, "fixture-journal-key", log.logger, {
    dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
    quiesceRun: async () => ({
      process: { state: "quiescent", processes: [], killed: [], detail: "" },
      docker: { state: "not_wired", removed: [], detail: "" },
    }),
  });
  if (mode === "adopted") {
    const seed = git.runnerCloneForBranch.bind(git);
    t.mock.method(git, "runnerCloneForBranch", (...args: Parameters<typeof seed>) => {
      if (args[7]) args[7] = { ...args[7], onSeeded: undefined };
      return seed(...args);
    });
  }
  const tails = (runner as unknown as Tails).executionTails;
  const first = runner.execute(claim);
  const firstTail = tails.get(claim.run_id)!;
  let second = Promise.resolve();
  try {
    await Promise.race([cleaning.promise, first.then(() => { throw new Error("park cleanup not reached: " + JSON.stringify(api.states)); })]);
    assert.ok((await fs.stat(source)).isDirectory(), "G1 source physically survives its legitimate guarded park");
    Object.defineProperty(git, "recoveryDecodedLimitBytes", { value: 4096 });
    parkedGeneration = 2;
    api.setOwnershipStatus(claim.run_id, "running", 2);
    if (mode === "failed-stop" || mode === "late-fence") {
      const stop = SteeringChannel.prototype.stop;
      if (mode === "late-fence") {
        const ack = client.ackInputs.bind(client);
        t.mock.method(client, "ackInputs", async (...args: Parameters<typeof ack>) => {
          ackStarted.resolve();
          await releaseAck.promise;
          return ack(...args);
        });
        api.setInputClaimGeneration(claim.run_id, 2);
        api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "fixture" }]);
      }
      t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
        if (mode === "late-fence") {
          await ackStarted.promise;
          api.setInputClaimGeneration(claim.run_id, 3);
          releaseAck.resolve();
        }
        await stop.call(this);
        if (mode === "late-fence") assert.equal(this.claimFence(), "stale");
        else throw new Error("steering join failed");
      });
    }
    if (mode === "unadmitted-restart") { finishCleanup.resolve(); await first; }
    second = runner.execute({ ...claim, claim_generation: 2, session_id: "prior-session" });
    const secondTail = tails.get(claim.run_id)!;
    const before = discoveries;
    await Promise.resolve();
    assert.equal(discoveries, before, "G2 cannot process sources before G1 final join");
    assert.equal(models, 1);
    finishCleanup.resolve();
    await first;
    const original = await firstTail;
    if (mode === "adopted") assert.equal(original, undefined);
    else {
      assert.ok(original?.family, "fresh surviving G1 family is positively observed");
      assert.equal(original.family.observedExecution.generation, 1);
      assert.equal(original.terminal, "normal_park");
    }
    await second;
    const result = await secondTail;
    if (mode === "normal") {
      const diagnostic = log.lines.slice(-8).map(line => {
        const { level, msg, blocker } = line as { level?: string; msg?: string; blocker?: string };
        return { level, msg: msg?.slice(0, 200), blocker };
      });
      assert.ok(result?.family, "normal blocked G2 returns a positive family: " + JSON.stringify(diagnostic));
      assert.equal(result.family.observedExecution.generation, 2);
      assert.equal(result.terminal, "normal_blocked", "current blocked execution does not inherit park tag");
      assert.equal(result.genesis.generation, 1);
      assert.deepEqual(result.family.originals, original!.family!.originals);
    } else assert.equal(result, undefined, "uncertain history cannot acquire source knowledge");
    assert.equal(models, 1, "retained source exhausts C without executing another model");
    const bare = git.barePathFor(claim.repo.clone_url);
    const journal = JSON.parse(command(bare, "config", "--get", "uzi-recovery." + branch + ".clone"));
    if (mode === "normal" || mode === "unadmitted-restart" || mode === "adopted" || mode === "failed-stop" || mode === "late-fence")
      assert.equal(journal.recovery.blocker, "decoded_history_limit");
    if (mode === "normal") {
      const key = path.basename(source).split(".attempt-")[0]!;
      const known = result!.family!.originals;
      const match = (k = key, r = claim.run_id, members = known) =>
        git.matchMaintenanceSourceFamily(bare, branch, k, r, members);
      assert.ok(await match(), "ordinary family qualifies before metadata probes");
      const movedFixture = await fs.mkdtemp(path.join(os.tmpdir(), "source-family-"));
      try {
        for (const ancestor of [path.dirname(source), path.dirname(path.dirname(source))]) {
          const moved = path.join(movedFixture, "moved");
          const before = await fs.lstat(source);
          await fs.rename(ancestor, moved);
          let linked = false;
          try {
            await fs.symlink(moved, ancestor, "dir");
            linked = true;
            const after = await fs.lstat(source);
            assert.equal(after.dev + ":" + after.ino, before.dev + ":" + before.ino,
              "ancestor substitution preserves known source inode");
            assert.equal(await match(), undefined,
              "source ancestor moved outside runner and replaced by symlink refuses: " + ancestor);
          } finally {
            if (linked) await fs.unlink(ancestor);
            await fs.rename(moved, ancestor);
          }
        }
      } finally { await fs.rmdir(movedFixture); }
      const activeKey = "uzi-recovery." + branch + ".clone";
      const protectedKey = "uzi-retained." + claim.run_id + ".journal";
      const active = command(bare, "config", "--get", activeKey);
      const parsed = JSON.parse(active);
      // Both variants parse as valid journals; only their recovery metadata differs.
      assert.ok(parsed.recovery);
      const differing = { ...parsed, recovery: { ...parsed.recovery, backoffMs: parsed.recovery.backoffMs === 1 ? 2 : 1 } };
      for (const protectedFirst of [false, true]) {
        for (const conflict of [false, true]) {
          command(bare, "config", "--unset-all", activeKey);
          const descriptor = JSON.stringify({ version: 1, branch, key, journal: conflict ? differing : parsed });
          if (protectedFirst) command(bare, "config", protectedKey, descriptor);
          command(bare, "config", activeKey, active);
          if (!protectedFirst) command(bare, "config", protectedKey, descriptor);
          const matched = await match();
          assert.equal(!!matched, !conflict, "descriptor consistency independent of order: " + protectedFirst);
          command(bare, "config", "--remove-section", "uzi-retained." + claim.run_id);
        }
      }
      for (const invalid of ["../" + key, key + "\n", " " + key, "x".repeat(129)]) {
        assert.equal(await match(invalid), undefined, "unsafe key refuses: " + JSON.stringify(invalid));
      }
      for (const invalid of [claim.run_id + "\n", " " + claim.run_id, "x".repeat(129)]) {
        assert.equal(await match(key, invalid), undefined, "unsafe run refuses");
      }
      for (const member of [
        { ...known[0]!, runId: claim.run_id + "\n" },
        { ...known[0]!, attemptId: known[0]!.attemptId + "\n" },
        { ...known[0]!, clonePath: path.dirname(source) + "/./" + path.basename(source) },
        { ...known[0]!, identity: "invalid" },
      ]) assert.equal(await match(key, claim.run_id, [member]), undefined, "unsafe known tuple refuses");
      const configPath = path.join(bare, "config");
      const beforeRows = await fs.readFile(configPath, "utf8");
      try {
        await fs.appendFile(configPath, "\n[benign]\n" + Array.from({ length: 2100 }, (_, i) => "row" + i + " = true\n").join(""));
        const rows = command(bare, "config", "--local", "--no-includes", "--name-only", "--list").split("\n");
        assert.ok(rows.length < 4096 && rows.length * 2 > 4096);
        assert.equal(await match(), undefined, "cumulative actual config visits exhaust discovery");
        await git.markRecoveryCapture(bare, source, branch, claim.run_id, parsed.attemptId);
        assert.deepEqual(JSON.parse(command(bare, "config", "--get", activeKey)), parsed,
          "unscoped recovery remains usable with many benign rows");
      } finally { await fs.writeFile(configPath, beforeRows); }
    }
    if (mode !== "missing-family") assert.ok((await fs.stat(source)).isDirectory());
    assert.equal(tails.has(claim.run_id), false);
  } finally {
    finishCleanup.resolve();
    releaseAck.resolve();
    runner.shutdown();
    await runner.shutdownRecoveries();
    await Promise.allSettled([first, second]);
  }
});
}
