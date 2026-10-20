import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { spawn } from "node:child_process";
import { setImmediate as tick } from "node:timers/promises";
import { RecoveryCoordinator } from "../src/recovery.js";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, codexRig, manualDeadline, trackedFile, statuses, spyCustodySettle, spyPublishLands } from "./runner-codex-sinks-fixture.js";
import { JOIN_TOKEN } from "./runner-held-publication-fixture.js";

installHarness();
const bytes = "R1 exact owner source\n";
type Window = "healthy" | "ACK" | "head" | "pin" | "cleanup";
type Ack = "paused" | "missing" | "declined" | "uncertain" | "previous-park";

for (const [window, ackMode, origin] of [
  ["healthy", "paused", "owner"], ["ACK", "paused", "owner"], ["head", "paused", "owner"],
  ["pin", "paused", "owner"], ["cleanup", "paused", "owner"],
  ["ACK", "missing", "owner"], ["ACK", "declined", "owner"], ["ACK", "uncertain", "owner"],
  ["ACK", "previous-park", "owner"],
  ["healthy", "paused", "lead_park"], ["ACK", "paused", "lead_park"], ["ACK", "declined", "lead_park"],
] as const satisfies readonly (readonly [Window, Ack, "owner" | "lead_park"])[]) {
  it(`R1 public pause ${ackMode} ACK / ${window} window / ${origin}`, async t => {
    const deadline = manualDeadline("checkpoint");
    type Event = { phase: Window; release: () => void };
    const events: Event[] = [];
    const releases = new Set<() => void>();
    let wake: (() => void) | undefined;
    let closed = false, done = false, returned = false, expired = false;
    let acked = false, requests = 0, injected = false, cleanupJoined = false;
    let clone = "", inode = 0, head = "", result: boolean | undefined, rejection: unknown;
    let snapshot: unknown, auth = { refresh: 0, release: 0 };
    const protectedAfterExpiry: string[] = [];
    const phases: string[] = [];
    const hold = async (phase: Window) => {
      if (closed) return;
      let release!: () => void;
      const pending = new Promise<void>(resolve => { release = resolve; });
      releases.add(release); events.push({ phase, release }); wake?.();
      await pending; releases.delete(release);
    };
    const rig = codexRig({
      armDeadline: deadline.armDeadline,
      boundaryProcess: async request => {
        if (window !== "cleanup" || !acked || injected) return undefined;
        injected = true;
        // Execute only the real trusted Git request, with its original cwd/env.
        // A successful primary exit is distinct from full-root cleanup.
        const [bin, ...args] = request.argv;
        assert.ok(bin);
        const child = spawn(bin, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        void terminal.catch(() => undefined);
        return {
          stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
          waitChild: () => terminal,
          root: {
            kind: "boundary_action",
            reap: async () => {
              await terminal;
              await hold("cleanup");
              cleanupJoined = true;
              // Never certify a root clean when the modeled supervisor refuses.
              return { ok: false, error: { category: "tool", message: "R1 full-root cleanup refused" } };
            },
            dispose: async () => {
              child.stdout.resume(); child.stderr.resume();
              if (child.exitCode === null) child.kill("SIGKILL");
              await terminal.catch(() => undefined);
            },
          },
        };
      },
    });
    const claim = { ...gitlabClaim(2852), claim_generation: 1, park_run_supported: true };
    const at = { completedCount: 1, total: 2 };
    const exec = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "PUBLIC.txt"), bytes);
      fs.writeFileSync(path.join(homeDir, "owner-session"), "R1 exact session");
      inode = fs.statSync(clone).ino;
      auth = { refresh: rig.refreshCalls(), release: rig.releaseCalls() };
      if (ackMode === "previous-park") {
        assert.equal(await ctx.parkForPause!(at), true, "public first pause establishes parked");
        acked = false;
      }
      try {
        result = origin === "owner" ? await ctx.parkForPause!(at) : (await ctx.requestParkRun!(at)).kind === "paused";
      }
      catch (error) { rejection = error; if (ackMode === "paused") throw error; }
      finally { returned = true; }
      assert.equal(rig.registry.state(), "poisoned", "accepted/refused capture keeps safety poisoned before terminal dispose");
      if (window === "healthy") {
        assert.equal(fs.statSync(clone).ino, inode);
        assert.equal(fs.readFileSync(path.join(homeDir, "owner-session"), "utf8"), "R1 exact session");
      }
      return { branch: ctx.branch, pausedAt: at };
    }, rig.settle, rig.capture);
    const runner = runnerWith(() => ({ executor: exec, homeDir }), fakeGitlab().gitlab, JOIN_TOKEN,
      undefined, { checkpointIntervalMs: 0 });
    const internal = runner as any;
    let captureFlight: any;
    const capture = internal.runFreeCapture.bind(runner);
    t.mock.method(internal, "runFreeCapture", async (...args: any[]) => {
      captureFlight = args[1];
      return capture(...args);
    });
    const custody = spyCustodySettle(runner);
    const restorePublish = spyPublishLands();
    const recovery = (runner as unknown as { recovery: RecoveryCoordinator }).recovery;
    const remember = async () => {
      snapshot = await recovery.inspect(claim.run_id);
      assert.ok(Array.isArray(snapshot) && snapshot.length > 0, "real authenticated recovery journal exists before expiry");
    };
    if (origin === "lead_park")
      t.mock.method(client, "requestWorkerPause", async () => ({ applied: true, status: "running" as const }));
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      if (args[1].status !== "paused") return report(...args);
      requests++;
      const first = ackMode === "previous-park" && requests === 1;
      const ack = await report(...args);
      if (first) { acked = true; return ack; }
      if (ackMode === "paused") {
        head = await originalTip(clone, "agent/issue-2852") ?? "";
        await remember();
      }
      acked = ackMode === "paused";
      if (window === "ACK") await hold("ACK");
      if (ackMode === "uncertain") throw new Error("R1 accepted reply lost in transport");
      if (ackMode === "missing") return { ...ack, status: undefined } as unknown as typeof ack;
      if (ackMode === "declined" || ackMode === "previous-park") return { ...ack, status: "running", applied: false };
      return ack;
    });
    const originalTip = git.branchTip.bind(git);
    t.mock.method(git, "branchTip", async (...args: Parameters<typeof originalTip>) => {
      if (expired) protectedAfterExpiry.push("branchTip");
      const value = await originalTip(...args);
      if (acked && window === "head") { await remember(); await hold("head"); }
      return value;
    });
    const pin = RecoveryCoordinator.prototype.pin;
    t.mock.method(RecoveryCoordinator.prototype, "pin", async function(this: RecoveryCoordinator, ...args: Parameters<typeof pin>) {
      if (expired) protectedAfterExpiry.push("pin");
      const value = await pin.apply(this, args);
      if (acked && window === "pin") { await remember(); await hold("pin"); }
      return value;
    });
    for (const method of ["worktreeStatus", "commitWipMarker", "worktreeHead"] as const) {
      const original = git[method].bind(git);
      t.mock.method(git, method, async (...args: unknown[]) => {
        if (expired) protectedAfterExpiry.push(method);
        return Reflect.apply(original, git, args);
      });
    }
    const operation = runner.execute(claim).finally(() => { done = true; wake?.(); });
    try {
      while (!done) {
        if (!events.length) await Promise.race([operation, new Promise<void>(resolve => { wake = resolve; })]);
        wake = undefined;
        const event = events.shift();
        if (!event) continue;
        try {
          phases.push(event.phase);
          if (ackMode !== "previous-park")
            assert.equal(api.messages(claim.run_id).some(message => message.kind === "paused"), false,
              "no paused display before ACK and capture custody handling complete");
          assert.equal(returned, false, "public pause stays pending behind external event");
          assert.equal(done, false, "execute tail stays pending behind external event");
          assert.ok(deadline.armed() > 0, "actual checkpoint capture deadline armed");
          expired = true; deadline.fire();
          await tick();
          assert.equal(returned, false, "expiry does not abandon held action or cleanup");
          assert.equal(done, false, "execute cannot release its tail before cleanup joins");
        } finally { event.release(); }
      }
      await operation;
      console.log("R1_PAUSE", JSON.stringify({ window, ackMode, result, rejection: rejection instanceof Error ? rejection.name : null,
        phases, cleanupJoined, states: statuses(claim.run_id), protectedAfterExpiry, fired: deadline.fired() }));
      if (ackMode === "paused") {
        assert.equal(result, true, "exact positive paused ACK survives capture expiry/cleanup refusal");
        assert.equal(rejection, undefined, "accepted pause never escapes into generic failure");
        assert.deepEqual(statuses(claim.run_id).filter(s => ["paused", "failed", "pause_failed"].includes(s)), ["paused"],
          "one accepted pause, no generic failure or second pause");
        assert.equal(requests, 1);
        if (window !== "healthy") {
          assert.equal(captureFlight.parked, true, "accepted ACK keeps the run parked");
          assert.equal(captureFlight.sourceJoinFailed, true, "incomplete capture cannot qualify as a normal source");
          assert.equal(fs.statSync(clone).ino, inode, "exact clone retained");
          assert.equal(fs.readFileSync(path.join(clone, "PUBLIC.txt"), "utf8"), bytes);
          assert.equal(fs.readFileSync(path.join(homeDir, "owner-session"), "utf8"), "R1 exact session");
        }
        assert.equal(trackedFile(2852, "PUBLIC.txt"), bytes, "published checkpoint retains exact work");
        assert.equal(custody(), 0, "no custody settlement");
        assert.equal(rig.refreshCalls(), auth.refresh, "no credential refresh");
        assert.equal(rig.releaseCalls(), auth.release, "no credential release");
        if (window !== "healthy") {
          assert.deepEqual(await recovery.inspect(claim.run_id), snapshot, "exact journal at expiry retained");
          assert.deepEqual(protectedAfterExpiry, [], "no new protected read or pin after authority expires");
          assert.deepEqual(phases, [window]);
        } else assert.ok(head.length === 40, "healthy published source head observed");
        if (window === "cleanup") {
          assert.ok(injected, "actual post-ACK Git child reached cleanup seam");
          assert.ok(cleanupJoined, "refused full-root cleanup joined");
        }
      } else {
        assert.notEqual(result, true, "missing/declined/uncertain ACK and pre-existing parked never qualify");
        assert.equal(requests, ackMode === "previous-park" ? 2 : 1);
        assert.equal(deadline.fired(), 1);
      }
      assert.equal(rig.registry.state(), "disposed", "execute disposes safety without reopening it");
      let called = false;
      await assert.rejects(rig.safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async () => { called = true; }));
      assert.equal(called, false, "successor boundary remains refused");
    } finally {
      closed = true;
      for (const release of releases) release();
      await operation.catch(() => undefined);
      restorePublish();
    }
  });
}
