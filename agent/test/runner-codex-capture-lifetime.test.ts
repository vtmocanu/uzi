import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { PassThrough } from "node:stream";
import { setImmediate as tick } from "node:timers/promises";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, GIT_ENV, manualDeadline, spyPublishLands } from "./runner-codex-sinks-fixture.js";

installHarness();

const bytes = "lifetime source must survive exactly\n";
const readGit = (tree: string, ...args: string[]) =>
  execFileSync("git", ["-C", tree, ...args], { env: GIT_ENV, encoding: "utf8" });

for (const site of ["wall", "hold", "recovery"] as const) {
  it(`${site}: exhausting the existing 180s episode before source denies all capture work`, async t => {
    let now = Date.now(), armed = false, expired = false, clone = "", originalHead = "";
    const reads: Array<{ name: string; expired: boolean }> = [];
    let fallbacks = 0, uploads = 0, acknowledgments = 0;
    const deadline = manualDeadline("shutdown");
    const lifecycle = new AbortController();
    const rig = codexRig({ now: () => now, armDeadline: deadline.armDeadline, lifecycleSignal: lifecycle.signal });
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) =>
      boundary(request, async permit => {
        if (armed && request.boundary === "shutdown" && !expired) {
          expired = true;
          // withBoundary is called only after the public callback created its episode.
          now = Date.now() + 180_001;
          t.mock.method(Date, "now", () => now);
          deadline.fire();
        }
        return action(permit);
      }));
    for (const name of ["worktreeStatus", "commitWipMarker", "worktreeHead", "fetchAgentBranch", "verifyRunnerTrackingCovers", "trackingTip"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (armed) reads.push({ name, expired });
        return Reflect.apply(original, git, args);
      });
    }
    const publication = spyPublishLands();
    t.after(publication);
    const publish = client.publishCheckpoint.bind(client);
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
      if (armed) uploads++;
      return publish(...args);
    });
    for (const name of ["requestCompletionHold", "reportWallPark", "reportState"] as const) {
      const original = client[name].bind(client);
      t.mock.method(client, name, async (...args: unknown[]) => {
        if (armed && (name !== "reportState" || (args[1] as { status: string }).status === "recovery_wait")) acknowledgments++;
        return Reflect.apply(original, client, args);
      });
    }
    api.setCompletionHoldResponse("paused");
    api.setWallParkResponse("paused", 200);
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(clone, "LIFETIME.txt"), bytes);
      originalHead = readGit(clone, "rev-parse", "HEAD");
      armed = true;
      if (site === "recovery") throw new TransientRecoveryError("lifetime fixture");
      if (site === "hold") {
        await ctx.enterCompletionHold!("lifetime hold");
        return { branch: ctx.branch, completionHeld: { reason: "lifetime hold" } };
      }
      await ctx.parkForWall!({ completedCount: 0 });
      return { branch: ctx.branch, walled: { reason: "lifetime wall" } };
    }, undefined, (ms, action, purpose, signal, attempt) => {
      fallbacks++;
      return rig.capture(ms, action, purpose, signal, attempt);
    });
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(gitlabClaim(285310 + ["wall", "hold", "recovery"].indexOf(site)));
    assert.equal(expired, true, "expiration was injected into an already-created episode");
    assert.equal(deadline.fired(), 1);
    console.log("EXHAUSTION_RESULT", site, JSON.stringify({ reads, fallbacks, uploads, acknowledgments, headUnchanged: readGit(clone, "rev-parse", "HEAD") === originalHead, status: readGit(clone, "status", "--porcelain"), bytesUnchanged: fs.readFileSync(path.join(clone, "LIFETIME.txt"), "utf8") === bytes }));
    assert.deepEqual(reads, [], "exhausted episode cannot begin protected source work");
    assert.equal(fallbacks, 0, "total budget exhaustion cannot buy a fresh fallback budget");
    assert.equal(uploads, 0);
    assert.equal(acknowledgments, 0, "no verified capture ACK without evidence");
    assert.equal(fs.readFileSync(path.join(clone, "LIFETIME.txt"), "utf8"), bytes);
    assert.equal(readGit(clone, "rev-parse", "HEAD"), originalHead, "no WIP commit changed the retained source");
    assert.equal(readGit(clone, "status", "--porcelain"), "?? LIFETIME.txt\n");
  });

  it(`${site}: a 31s fetch remains in the healthy 120s capture`, async t => {
    let now = Date.now(), armed = false, advanced = false, clone = "", fallbacks = 0;
    const rig = codexRig({ now: () => now });
    const publication = spyPublishLands();
    t.after(publication);
    const fetch = git.fetchAgentBranch.bind(git);
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof fetch>) => {
      if (armed && !advanced) {
        advanced = true;
        // Advance at the actual fetch seam, after episode creation and admission.
        now = Date.now() + 31_000;
        t.mock.method(Date, "now", () => now);
      }
      return fetch(...args);
    });
    api.setCompletionHoldResponse("paused");
    api.setWallParkResponse("paused", 200);
    const iid = 285320 + ["wall", "hold", "recovery"].indexOf(site);
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "LIFETIME.txt"), bytes);
      armed = true;
      if (site === "recovery") throw new TransientRecoveryError("slow fetch fixture");
      if (site === "hold") {
        assert.equal(await ctx.enterCompletionHold!("slow fetch"), true);
        return { branch: ctx.branch, completionHeld: { reason: "slow fetch" } };
      }
      assert.equal(await ctx.parkForWall!({ completedCount: 0 }), "parked");
      return { branch: ctx.branch, walled: { reason: "slow fetch" } };
    }, undefined, (ms, action, purpose, signal, attempt) => {
      fallbacks++;
      return rig.capture(ms, action, purpose, signal, attempt);
    });
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid);
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(advanced, true, "the real capture fetch ran");
    assert.equal(fallbacks, 0, "31 seconds must not trigger credential-free fallback");
    assert.equal(rig.refreshCalls(), 1, "one healthy admission");
    assert.equal(readGit(git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:LIFETIME.txt`), bytes);
    assert.equal(fs.readFileSync(path.join(clone, "LIFETIME.txt"), "utf8"), bytes);
    if (site === "recovery") assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "recovery_wait"));
  });
}

for (const refuseCleanup of [false, true]) {
  it(`primary Git completion waits for full-root cleanup (${refuseCleanup ? "refused" : "settled"})`, async t => {
    let armed = false, injected = false, sourceSteps = 0, acks = 0, fallbacks = 0;
    let rootSettled = false, done = false, clone = "";
    const phases: string[] = [];
    const events: Array<{ phase: string; release: () => void }> = [];
    const releases: Array<() => void> = [];
    let wake: (() => void) | undefined;
    let closed = false;
    const hold = async (phase: string) => {
      if (closed) return;
      await new Promise<void>(resolve => {
        releases.push(resolve);
        events.push({ phase, release: resolve });
        wake?.();
      });
    };
    const rig = codexRig({
      boundaryProcess: async request => {
        if (!armed || injected) return undefined;
        injected = true;
        const [bin, ...args] = request.argv;
        assert.ok(bin);
        // Execute only the runner's real Git request, never a screened candidate.
        const child = spawn(bin, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const stdout = new PassThrough(), stderr = new PassThrough();
        stdout.on("error", () => {});
        stderr.on("error", () => {});
        child.stdout.pipe(stdout);
        child.stderr.pipe(stderr);
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        void terminal.catch(() => undefined);
        await hold("launch");
        return {
          stdin: child.stdin, stdout, stderr,
          waitChild: async () => { const result = await terminal; await hold("primary"); return result; },
          root: {
            kind: "boundary_action",
            reap: async () => {
              await terminal;
              await hold("full-root");
              if (refuseCleanup) return { ok: false, error: { category: "tool", message: "lifetime root remains unsettled" } };
              rootSettled = true;
              return { ok: true };
            },
            dispose: async () => {
              if (child.exitCode === null) child.kill("SIGKILL");
              child.stdout.resume(); child.stderr.resume();
              await terminal.catch(() => undefined);
              stdout.destroy(); stderr.destroy();
            },
          },
        };
      },
    });
    for (const name of ["worktreeStatus", "commitWipMarker", "worktreeHead", "fetchAgentBranch"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (armed) sourceSteps++;
        return Reflect.apply(original, git, args);
      });
    }
    const report = client.requestCompletionHold.bind(client);
    t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof report>) => {
      acks++;
      assert.equal(rootSettled, true, "ACK requires full root settlement");
      return report(...args);
    });
    const publication = spyPublishLands();
    t.after(publication);
    api.setCompletionHoldResponse("paused");
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "LIFETIME.txt"), bytes);
      armed = true;
      await ctx.enterCompletionHold!("cleanup barrier");
      return { branch: ctx.branch, completionHeld: { reason: "cleanup barrier" } };
    }, undefined, (ms, action, purpose, signal, attempt) => {
      fallbacks++;
      return rig.capture(ms, action, purpose, signal, attempt);
    });
    const { gitlab } = fakeGitlab();
    const operation = runnerWith(() => ({ executor }), gitlab).execute(gitlabClaim(refuseCleanup ? 285331 : 285330))
      .finally(() => { done = true; wake?.(); });
    let firstSteps: number | undefined;
    try {
      while (!done) {
        if (!events.length) await Promise.race([operation, new Promise<void>(resolve => { wake = resolve; })]);
        wake = undefined;
        const event = events.shift();
        if (!event) continue;
        try {
          await tick();
          phases.push(event.phase);
          firstSteps ??= sourceSteps;
          assert.equal(done, false, "public callback remains pending at " + event.phase);
          assert.equal(sourceSteps, firstSteps, "no next source step before full-root settlement");
          assert.equal(acks, 0, "no ACK before full-root settlement");
          assert.equal(fallbacks, 0, "no fallback overlaps primary cleanup");
        } finally { event.release(); }
      }
      await operation;
    } finally {
      closed = true;
      releases.forEach(release => release());
      await operation.catch(() => undefined);
    }
    assert.deepEqual(phases.slice(0, 3), ["launch", "primary", "full-root"], "actual child completion and cleanup were separately held");
    if (refuseCleanup) {
      assert.equal(acks, 0, "unsettled root cannot authorize capture acknowledgment");
      assert.equal(fs.readFileSync(path.join(clone, "LIFETIME.txt"), "utf8"), bytes);
    } else {
      assert.equal(acks, 1);
      assert.ok(sourceSteps > firstSteps!, "capture continues only after cleanup");
      assert.equal(fallbacks, 0);
    }
  });
}
