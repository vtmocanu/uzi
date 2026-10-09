import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  launchCodexEffectRoot, type LauncherDeps, type SupervisorProcess,
} from "../src/codex/launcher.js";
import { COMMAND_UID } from "../src/runner-uid.js";
import { RejectedStartupTransport } from "./codex-rejected-startup-fixture.js";

const spec = {
  identity: "command" as const, command: "/bin/true", args: [], cwd: "/work/repo", env: {},
  supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
};

function deps(child: RejectedStartupTransport, extra: Partial<LauncherDeps> = {}): LauncherDeps {
  return {
    env: { UZI_UID_SPLIT: "1" }, resolveCommandUid: () => COMMAND_UID,
    spawnSupervisor: () => child as unknown as SupervisorProcess,
    ...extra,
  };
}

function started(child: RejectedStartupTransport, nondumpable = true): void {
  child.evidence.write(JSON.stringify({
    event: "started", supervisorPid: child.pid, childPid: child.pid + 1,
    subreaper: true, nondumpable, uid: COMMAND_UID, liveCapsZero: true,
    capBoundingSet: "0xc0", noNewPrivs: true,
  }) + "\n");
}

describe("#2562 launcher startup deadline arm", () => {
  it("keeps the rejected launch pending until its actual cleanup settles", async () => {
    const child = new RejectedStartupTransport("command");
    let fire!: () => void;
    let settled = false;
    const authorization = Object.freeze({ hardDeadlineAt: Date.now() + 60000, attempt: Object.freeze({}) });
    const pending = launchCodexEffectRoot(spec, deps(child, {
      startupCleanup: authorization,
      armStartedDeadlineForTest: (callback, ms) => {
        assert.equal(ms, 10000);
        fire = callback;
        return () => {};
      },
    }));
    const result = pending.then(value => { settled = true; return { value, error: undefined }; },
      (error: unknown) => { settled = true; return { value: undefined, error }; });
    try {
      fire();
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.ok(child.events.includes("dispose"), "startup expiry requests real disposal");
      assert.equal(settled, false, "launcher returned before owned cleanup");
      child.releaseVerifiedCleanup();
      const outcome = await result;
      assert.ok(outcome.error instanceof Error);
      child.assertClosed();
    } finally { child.destroy(); }
  });

  it("default construction arms a real 10000ms timer, unrefs and clears that handle", async (t) => {
    const child = new RejectedStartupTransport("command");
    const nativeSet = globalThis.setTimeout;
    const nativeClear = globalThis.clearTimeout;
    let handle: ReturnType<typeof setTimeout> | undefined;
    let unrefObserved = false;
    let cleared = false;
    let restoreUnref: (() => void) | undefined;
    let restoreSet: (() => void) | undefined;
    let restoreClear: (() => void) | undefined;
    try {
      const setSpy = t.mock.method(globalThis, "setTimeout", (...args: Parameters<typeof setTimeout>) => {
        const timer = nativeSet(...args);
        if (args[1] === 10000) {
          assert.equal(handle, undefined, "identify the exact startup budget, not the first timer");
          handle = timer;
          const nativeUnref = timer.unref.bind(timer);
          const unrefSpy = t.mock.method(timer, "unref", () => {
            unrefObserved = true;
            return nativeUnref();
          });
          restoreUnref = () => unrefSpy.mock.restore();
        }
        return timer;
      });
      restoreSet = () => setSpy.mock.restore();
      const clearSpy = t.mock.method(globalThis, "clearTimeout", (...args: Parameters<typeof clearTimeout>) => {
        if (args[0] === handle && handle !== undefined) cleared = true;
        return nativeClear(...args);
      });
      restoreClear = () => clearSpy.mock.restore();
      const pending = launchCodexEffectRoot(spec, deps(child));
      queueMicrotask(() => started(child));
      const launched = await pending;
      assert.equal(launched.started.uid, COMMAND_UID);
      assert.ok(handle, "new field absent still selects the real default startup timer");
      assert.equal(unrefObserved, true);
      assert.equal(cleared, true);
    } finally {
      restoreUnref?.();
      restoreSet?.();
      restoreClear?.();
      child.destroy();
    }
  });

  for (const mode of ["success", "posture", "expiry", "synchronous expiry"] as const) {
    it(`cancels the injected arm on ${mode}`, async () => {
      const child = new RejectedStartupTransport("command");
      let fire: (() => void) | undefined;
      let cancelled = 0;
      const pending = launchCodexEffectRoot(spec, deps(child, {
        armStartedDeadlineForTest: (callback, ms) => {
          assert.equal(ms, 10000);
          fire = callback;
          if (mode === "synchronous expiry") callback();
          return () => { cancelled += 1; fire = undefined; };
        },
      }));
      // Every rejection is observed before driving the fake supervisor.
      const result = pending.then(value => ({ value, error: undefined }),
        (error: unknown) => ({ value: undefined, error }));
      try {
        assert.ok(fire || mode === "synchronous expiry", "startup arm installed");
        if (mode === "success" || mode === "posture") started(child, mode === "success");
        if (mode === "expiry") fire!();
        const settled = await result;
        if (mode === "success") assert.ok(settled.value);
        else assert.ok(settled.error instanceof Error);
        assert.equal(cancelled, 1, "injected resource released exactly once");
        assert.equal(fire, undefined);
      } finally { child.destroy(); }
    });
  }
});
