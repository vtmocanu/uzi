import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { cleanupP, PCleanupError, type PCleanupResources } from "./p-cleanup.js";

const scratch = path.resolve(__dirname, "../../.uzi/scratch");
const childPid = 4242;
type Adapter = (resources: PCleanupResources, work?: () => Promise<void>) => Promise<void>;

// Mirror the startup adapter: registration happens before resource acquisition;
// the hook receives the primary failure as well as the partially acquired resources.
const startup: Adapter = async (resources, work = async () => {}) => {
  let after!: () => Promise<void>;
  const registerAfter = (hook: () => Promise<void>): void => { after = hook; };
  let primaryFailure: { error: unknown } | undefined;
  registerAfter(async () => { await cleanupP(resources, primaryFailure); });
  try {
    await work();
  } catch (error) {
    primaryFailure = { error };
    throw error;
  } finally {
    await after();
  }
};
// Mirror the harness adapter: finally must preserve a primary error if teardown also fails.
const harness: Adapter = async (resources, work = async () => {}) => {
  let primaryFailure: { error: unknown } | undefined;
  try {
    await work();
  } catch (error) {
    primaryFailure = { error };
    throw error;
  } finally {
    await cleanupP(resources, primaryFailure);
  }
};

function fixture() {
  mkdirSync(scratch, { recursive: true });
  const base = mkdtempSync(path.join(scratch, "p-cleanup-test-"));
  writeFileSync(path.join(base, "owned"), "temporary data");
  const completed: string[] = [];
  const resources: PCleanupResources = {
    base,
    transport: { close: async () => { completed.push("transport"); } },
    handle: {
      started: {
        event: "started", supervisorPid: childPid, childPid, subreaper: true,
        nondumpable: true, uid: 10001, liveCapsZero: true,
        capBoundingSet: "0xc0", noNewPrivs: true,
      },
      dispose: async () => {
        completed.push("dispose");
        return { clean: true, event: {
          event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL",
          killed: [childPid], reaped: [childPid],
        } };
      },
    },
    provider: { close: async () => { completed.push("provider"); } },
    remove: async directory => {
      rmSync(directory, { recursive: true, force: true });
      assert.equal(existsSync(directory), false);
      completed.push("remove");
    },
  };
  return { base, resources, completed };
}

for (const [style, adapter] of [["startup after hook", startup], ["harness finally", harness]] as const) {
  test(`P cleanup ${style}: validates clean success and completes every operation`, async () => {
    const f = fixture();
    await adapter(f.resources);
    assert.deepEqual(f.completed, ["transport", "dispose", "provider", "remove"]);
    assert.equal(existsSync(f.base), false);
  });

  test(`P cleanup ${style}: clean:false fails after provider close and removal`, async () => {
    const f = fixture();
    f.resources.handle!.dispose = async () => {
      f.completed.push("dispose");
      return { clean: false, reason: "missing evidence EOF" };
    };
    await assert.rejects(adapter(f.resources), (error: unknown) => {
      assert.ok(error instanceof PCleanupError);
      assert.match(String(error.disposalError), /missing evidence EOF/);
      assert.deepEqual(error.results.map(result => [result.step, result.completed]), [
        ["transport", true], ["dispose", false], ["provider", true], ["remove", true],
      ]);
      assert.equal(error.base, f.base);
      assert.equal(error.baseAbsent, true);
      return true;
    });
    assert.deepEqual(f.completed, ["transport", "dispose", "provider", "remove"]);
    assert.equal(existsSync(f.base), false);
  });

  test(`P cleanup ${style}: thrown dispose identity survives other cleanup failures`, async () => {
    const f = fixture();
    const disposeError = new Error("dispose rejected");
    const transportError = new Error("transport rejected");
    const providerError = new Error("provider rejected");
    const removeError = new Error("removal callback rejected after removal");
    f.resources.transport!.close = async () => {
      f.completed.push("transport");
      throw transportError;
    };
    f.resources.handle!.dispose = async () => {
      f.completed.push("dispose");
      throw disposeError;
    };
    f.resources.provider!.close = async () => {
      f.completed.push("provider");
      throw providerError;
    };
    f.resources.remove = async directory => {
      rmSync(directory, { recursive: true, force: true });
      f.completed.push("remove");
      throw removeError;
    };
    await assert.rejects(adapter(f.resources), (error: unknown) => {
      assert.ok(error instanceof PCleanupError);
      assert.equal(error.disposalError, disposeError);
      assert.equal(error.cause, disposeError);
      assert.deepEqual(error.errors, [disposeError, transportError, providerError, removeError]);
      assert.equal(error.baseAbsent, true);
      return true;
    });
    assert.deepEqual(f.completed, ["transport", "dispose", "provider", "remove"]);
    assert.equal(existsSync(f.base), false);
  });

  test(`P cleanup ${style}: partial acquisition still closes provider and removes its tree`, async () => {
    const f = fixture();
    const partial: PCleanupResources = {};
    const acquire = async (): Promise<void> => {
      partial.provider = f.resources.provider;
      partial.base = f.base;
      throw new Error("setup failed before launch");
    };
    await assert.rejects(adapter(partial, acquire), /setup failed before launch/);
    assert.deepEqual(f.completed, ["provider"]);
    assert.equal(existsSync(f.base), false);
    await adapter({}); // failure before the first acquisition requires no cleanup
  });

  for (const field of ["killed", "reaped"] as const) {
    test(`P cleanup ${style}: rejects incorrect ${field} despite clean:true`, async () => {
      const f = fixture();
      const dispose = f.resources.handle!.dispose;
      f.resources.handle!.dispose = async () => {
        const outcome = await dispose();
        assert.ok(outcome.clean);
        return { ...outcome, event: { ...outcome.event, [field]: [childPid + 1] } };
      };
      await assert.rejects(adapter(f.resources), PCleanupError);
      assert.deepEqual(f.completed, ["transport", "dispose", "provider", "remove"]);
      assert.equal(existsSync(f.base), false);
    });
  }
}

for (const [style, adapter] of [["startup after hook", startup], ["harness finally", harness]] as const) {
  test(`P cleanup ${style}: preserves primary failure and disposal error together`, async () => {
    const f = fixture();
    const primary = new Error("turn failed");
    const disposal = new Error("dispose failed too");
    f.resources.handle!.dispose = async () => { throw disposal; };
    await assert.rejects(adapter(f.resources, async () => { throw primary; }), (error: unknown) => {
      assert.ok(error instanceof PCleanupError);
      assert.equal(error.primaryFailure?.error, primary);
      assert.equal(error.disposalError, disposal);
      assert.equal(error.cause, primary);
      assert.deepEqual(error.errors, [primary, disposal]);
      assert.equal(error.baseAbsent, true);
      return true;
    });
    assert.deepEqual(f.completed, ["transport", "provider", "remove"]);
    assert.equal(existsSync(f.base), false);
  });
}

test("P cleanup: removal that leaves a directory fails with final absence diagnostics", async () => {
  const f = fixture();
  try {
    f.resources.remove = async () => {};
    await assert.rejects(cleanupP(f.resources), (error: unknown) => {
      assert.ok(error instanceof PCleanupError);
      assert.equal(error.baseAbsent, false);
      assert.equal(error.results.at(-1)?.completed, false);
      return true;
    });
    assert.equal(existsSync(f.base), true);
  } finally {
    rmSync(f.base, { recursive: true, force: true });
  }
  assert.equal(existsSync(f.base), false);
});
