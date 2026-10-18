import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { setImmediate as tick } from "node:timers/promises";
import { codexRig, drain } from "./runner-codex-sinks-fixture.js";
import { git, client } from "./runner-harness.js";

export function observe(opts: { vault?: boolean; refusal?: boolean } = {}) {
  let armed = false, start = 0, active = false, entries = 0, returns = 0;
  let fullReap = false, done = false, injected = false;
  const authDeltas: unknown[] = [];
  const source: unknown[] = [], holds: unknown[] = [], pubs: unknown[] = [], acks: unknown[] = [];
  const rig = codexRig({
    vaultLocked: () => !!opts.vault,
    boundaryProcess: async request => {
      if (!armed || injected) return undefined;
      injected = true;
      // Run the actual requested Git child; primary completion and root cleanup are independent.
      const [bin, ...args] = request.argv;
      if (!bin) throw new Error("test fixture empty Git argv");
      const child = spawn(bin, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{code:number}>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", code => resolve({code: code ?? 1}));
      });
      let releasePrimary!: () => void;
      const primaryGate = new Promise<void>(resolve => { releasePrimary = resolve; });
      let releaseRoot!: () => void;
      const rootGate = new Promise<void>(resolve => { releaseRoot = resolve; });
      // The controller releases each independent latch only after observing the awaiting caller.
      void terminal.then(async () => {
        await tick();
        holds.push({ phase: "primary-held", active, pending: !done, captureReturned: returns > 0 });
        releasePrimary();
      });
      return {
        stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
        waitChild: async () => {
          const result = await terminal;
          await primaryGate;
          return result;
        },
        root: {
          kind: "boundary_action",
          reap: async () => {
            await terminal;
            void tick().then(() => {
              holds.push({ phase: "full-root-held", active, pending: !done, captureReturned: returns > 0 });
              releaseRoot();
            });
            await rootGate;
            return {ok:true};
          },
          dispose: async () => {
            releasePrimary(); releaseRoot();
            if (child.exitCode === null) child.kill("SIGKILL");
            await terminal.catch(() => undefined);
          }
        }
      };
    }
  });
  // An owned provider root models completed full-root proof, independently of a reap call count.
  const reserve = rig.registry.reserveLaunch("provider");
  assert.equal(reserve.kind, "reserved");
  if (reserve.kind === "reserved") rig.registry.registerRoot(reserve.reservation, {
    kind: "provider",
    reap: async () => {
      await tick();
      if (opts.refusal) return {ok:false, error:{category:"tool" as const, message:"test root proof refused"}};
      fullReap = true;
      return {ok:true};
    },
    dispose: async () => {}
  });
  const arm = () => { if (!armed) { armed = true; start = rig.processSpawns(); } };
  const boundary = rig.safety.withBoundary.bind(rig.safety);
  rig.safety.withBoundary = async (req, action) => {
    try { return await boundary(req, action); }
    catch (error) { if (opts.vault) arm(); throw error; }
  };
  const wip = git.commitWipMarker.bind(git);
  git.commitWipMarker = async (...args) => {
    if (armed) source.push({
      state: rig.registry.state(), pending: rig.registry.pendingLaunchCount(),
      callbacks: rig.registry.inFlightCallbackCount(), fullReap, active
    });
    return wip(...args);
  };
  const status = git.worktreeStatus.bind(git);
  git.worktreeStatus = async (...args) => {
    if (armed) source.push({
      state: rig.registry.state(), pending: rig.registry.pendingLaunchCount(),
      callbacks: rig.registry.inFlightCallbackCount(), fullReap, active
    });
    return status(...args);
  };
  const publish = client.publishCheckpoint.bind(client);
  client.publishCheckpoint = async (_id, _oid, pack) => {
    await drain(pack);
    if (armed) { await tick(); pubs.push({active, pending:!done}); }
    return {ok:true, body:{published:true, ref:"refs/uzi-checkpoints/agent/issue-x"}};
  };
  const report = client.reportState.bind(client);
  client.reportState = async (id, body, signal) => {
    if (armed && ["paused", "recovery_wait"].includes(body.status)) {
      await tick(); acks.push({active, pending:!done});
    }
    return report(id, body, signal);
  };
  const capture: typeof rig.capture = async (ms, action, purpose, signal) => {
    try {
      return await rig.capture(ms, async held => {
        entries++; active = true;
        const refresh = rig.refreshCalls(), release = rig.releaseCalls();
        try { return await action(held); }
        finally { authDeltas.push({refresh:rig.refreshCalls()-refresh,release:rig.releaseCalls()-release}); }
      }, purpose, signal);
    } finally { active = false; returns++; }
  };
  const observations = () => ({entries, returns, source, holds, pubs, acks, authDeltas, spawns:rig.processSpawns()-start});
  return {
    rig, capture, arm, observations,
    execute: async (operation: Promise<void>) => { await operation.then(() => { done = true; }, error => { done = true; throw error; }); },
    restore: () => {git.commitWipMarker=wip; git.worktreeStatus=status; client.publishCheckpoint=publish; client.reportState=report;},
    assertProof: (publication: boolean, ack = false) => {
      const o = observations();
      console.log("PUBLIC_PROOF", JSON.stringify(o));
      assert.ok(source.length > 0, "protected source was actually read");
      assert.deepEqual(source[0], {state:"poisoned",pending:0,callbacks:0,fullReap:true,active:true},
        "first protected source requires closed admission, drained launches/callbacks and completed root proof inside capture");
      assert.ok(authDeltas.length > 0, "actual capture auth activity observed");
      assert.ok(authDeltas.every((d:any)=>d.refresh===0 && d.release===0), "zero refresh/release inside actual capture");
      assert.ok(entries > 0 && returns >= entries, "actual capture callback entered and awaited return");
      assert.ok(o.spawns > 0, "capture region launches Git through supervised process seam");
      assert.deepEqual(holds, [
        {phase:"primary-held",active:true,pending:true,captureReturned:false},
        {phase:"full-root-held",active:true,pending:true,captureReturned:false}
      ], "caller remains pending through primary completion and full-root cleanup");
      if (publication) {
        assert.ok(pubs.length > 0, "checkpoint pack actually uploaded");
        assert.ok(pubs.every((x:any)=>x.active && x.pending), "capture held through awaited upload");
      } else assert.equal(pubs.length, 0, "terminal capture publishes nothing");
      if (ack) {
        assert.ok(acks.length > 0, "final park ACK observed");
        assert.ok(acks.every((x:any)=>x.active && x.pending), "capture held through final park ACK");
      }
    },
    assertRefusal: () => {
      console.log("PUBLIC_REFUSAL", JSON.stringify(observations()));
      assert.equal(source.length,0,"failed initial proof forbids protected source reads");
      assert.equal(entries,0,"failed initial proof never enters capture");
      assert.equal(observations().spawns,0,"failed initial proof launches no capture Git");
      assert.equal(pubs.length,0,"failed initial proof publishes nothing");
    }
  };
}
