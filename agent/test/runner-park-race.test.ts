import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import type { RunContext, ExecutorResult } from "../src/executor.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { procfsTable } from "../src/run-quiescence.js";
import { recordingLogger } from "./helpers.js";
import { scopedRealView, withQuiescenceView } from "./fake-proc.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, VAULT_FEATURE, codexRig, installRecoveryRootCleanup } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

const LATE = "late writer bytes after the byte check\n";
function gitIn(cwd: string, ...args: string[]): string {
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" },
    encoding: "utf8",
  }).trim();
}

// The child inherits an already-open writable FD, but no run markers and no clone cwd.
// It writes only on the IPC handshake. Each test waits for exit before permitting the final scan.
async function writer(clone: string) {
  fs.writeFileSync(path.join(clone, "RACE.txt"), "captured bytes\n");
  gitIn(clone, "add", "RACE.txt");
  gitIn(clone, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false",
    "commit", "-m", "race fixture");
  const fd = fs.openSync(path.join(clone, "RACE.txt"), "r+");
  let child: ChildProcess;
  try {
    child = spawn(process.execPath, ["-e", `const fs = require("node:fs");
process.on("message", () => { fs.ftruncateSync(3, 0); fs.writeSync(3, ${JSON.stringify(LATE)}); process.exit(0); });
process.send("ready");`], { cwd: fx.dataDir, env: { PATH: process.env.PATH }, stdio: ["ignore", "ignore", "pipe", fd, "ipc"] });
  } finally { fs.closeSync(fd); }
  const exited = once(child, "exit");
  let didExit = false;
  const close = async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await exited;
  };
  try {
    assert.equal((await once(child, "message"))[0], "ready");
    assert.ok(procfsTable.listPids().includes(child.pid!), "calibration: test-owned live child is in real procfs");
    assert.equal(procfsTable.readCwd(child.pid!), fs.realpathSync(fx.dataDir), "writer cwd is outside clone");
    assert.ok(!procfsTable.readEnviron(child.pid!).includes("UZI_RUN"), "writer has no inherited run markers");
    const statFields = procfsTable.readStat(child.pid!).split(") ")[1];
    assert.ok(statFields, "writer process stat fields are available");
    assert.equal(Number(statFields.split(" ")[1]), process.pid,
      "writer is a direct descendant in the installed scoped view");
    return {
      pid: child.pid!,
      exited: () => didExit,
      write: async () => {
        child.send("write");
        const [code, signal] = await exited;
        assert.equal(code, 0, "writer exits successfully before scan");
        assert.equal(signal, null);
        didExit = true;
      },
      close,
    };
  } catch (error) {
    await close();
    throw error;
  }
}

for (const [index, kind] of ["recovery", "credential deferral", "disk", "provider direct", "provider reconciled", "live nonterminal", "switch outer", "switch in-place"].entries()) {
  it(`M1 late-writer race: ${kind} keeps exact clone and plain journal despite positive coverage`,
    { skip: process.platform !== "linux" }, async () => {
    const claim = gitlabClaim(28520 + index, { claim_generation: 1 });
    api.setOwnershipStatus(claim.run_id, "running", 1);
    const { gitlab } = fakeGitlab();
    const { logger, lines } = recordingLogger();
    let clone = "";
    let branch = "";
    let w: Awaited<ReturnType<typeof writer>> | undefined;
    let boundary = false;
    let covered = false;
    let verified = false;
    let calibrated = false;
    let fixtureError: unknown;
    const provider = kind.startsWith("provider");
    const switching = kind.startsWith("switch");
    client.protocolFeatures = kind === "credential deferral" ? [VAULT_FEATURE] :
      provider ? ["claim_generation_fence", "recovery_cause_provider_outage"] :
      ["claim_generation_fence", "recovery_cause_data_volume_full"];
    const rig = codexRig({ vaultLocked: () => true });
    const behavior = async (ctx: RunContext): Promise<ExecutorResult> => {
      clone = ctx.worktreePath;
      branch = ctx.branch;
      try { w = await writer(clone); } catch (error) { fixtureError = error; throw error; }
      // The real view is installed before any capture or cleanup proof.
      const execute = async (): Promise<ExecutorResult> => {
        if (kind === "credential deferral") return { branch: ctx.branch };
        if (kind === "switch in-place") {
          assert.equal(await ctx.attemptCredentialSwitch!(), "released");
          return { branch: ctx.branch, switchReleased: true };
        }
        if (switching) throw new CredentialSwitchSignal();
        if (kind === "disk") throw new DiskParkSignal(false);
        throw new TransientRecoveryError("test recovery", { providerOrigin: provider });
      };
      return execute();
    };
    // Keep the view active across the runner's catch/finally, not only the executor.
    const view = scopedRealView();
    const runner = runnerWith(() => ({
      executor: kind === "credential deferral" ? new FakeCodexExecutor(rig.safety, behavior, rig.settle) : { run: behavior },
    }), gitlab, undefined, logger, { recoveryRetryMs: 1 });
    const readEnviron = procfsTable.readEnviron;
    procfsTable.readEnviron = (pid) => {
      const result = readEnviron(pid);
      // Only runner scans after writer() returned count as installed-view calibration.
      if (w && pid === w.pid && !w.exited()) calibrated = true;
      return result;
    };
    const originalCovered = git.verifyStoppedSourceCovered.bind(git);
    const originalVerify = git.verifyRunnerTrackingCovers.bind(git);
    const report = client.reportState.bind(client);
    const ownership = client.getRunOwnership.bind(client);
    let lost = false;
    git.verifyStoppedSourceCovered = async (...args) => {
      const quiesce = args[6];
      args[6] = async (paths, phase) => {
        if (phase === "after" && !boundary) {
          assert.ok(calibrated, "installed real view has read the live writer");
          boundary = true;
          await w!.write();
          assert.ok(w!.exited(), "child has exited BEFORE final coverage scan");
        }
        return quiesce(paths, phase);
      };
      const result = await originalCovered(...args);
      covered ||= result === "covered";
      return result;
    };
    git.verifyRunnerTrackingCovers = async (...args) => {
      const result = await originalVerify(...args);
      verified ||= result;
      return result;
    };
    const internals = runner as unknown as { quiesceRun: (...args: any[]) => Promise<any> };
    const quiesce = internals.quiesceRun.bind(runner);
    internals.quiesceRun = async (...args) => {
      if (switching && args[2].site === "credential_switch:after_runner_git" && !boundary) {
        assert.ok(calibrated, "installed real view has read the live writer");
        boundary = true;
        await w!.write();
        assert.ok(w!.exited(), "child exited BEFORE capture reproof scan");
      }
      return quiesce(...args);
    };
    client.reportState = async (runId, body, signal) => {
      if (body.status === "credential_switch") return { applied: true, status: "queued", credentialSwitchReleased: true };
      if (body.status === "recovery_wait" && kind === "live nonterminal") return { applied: true, status: "paused" };
      if (body.status === "recovery_wait" && kind === "provider reconciled") {
        lost = true;
        throw new Error("lost park acknowledgement");
      }
      return report(runId, body, signal);
    };
    client.getRunOwnership = async (runId) => lost ?
      { status: "recovery_wait", claim_generation: 1 } : ownership(runId);
    try {
      await withQuiescenceView(view, () => runner.execute(claim));
      if (fixtureError) throw fixtureError;
      if (!boundary) console.error("M1 boundary missing", JSON.stringify(lines));
      assert.ok(boundary, "byte-check/final-scan boundary reached");
      assert.ok(w!.exited(), "late writer exited before scan");
      assert.ok(verified, "capture verification positively observed");
      if (!switching) assert.ok(covered, "cleanup verdict positively observed as covered");
      console.log(`M1 calibrated=${calibrated} boundary=${boundary} child_exit=${w!.exited()} verified=${verified} covered=${covered} kind=${kind}`);
      assert.ok(fs.existsSync(clone), "exact clone retained despite the positive snapshot verdict");
      assert.equal(fs.readFileSync(path.join(clone, "RACE.txt"), "utf8"), LATE);
      const journal = gitIn(git.barePathFor(fx.originPath), "config", "--get-all", `uzi-recovery.${branch}.clone`);
      assert.ok(journal.includes(clone), "plain journal retains exact clone");
      assert.ok(!lines.some((l) => (l as { msg?: string }).msg?.includes("teardown proceeds")), "park snapshot is diagnostic only");
    } finally {
      procfsTable.readEnviron = readEnviron;
      git.verifyStoppedSourceCovered = originalCovered;
      git.verifyRunnerTrackingCovers = originalVerify;
      client.reportState = report;
      client.getRunOwnership = ownership;
      await w?.close();
    }
  });
}
