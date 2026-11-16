import { after, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, rmSync } from "node:fs";
import {
  createDindPrune, DindPruneGate, DIND_PRUNE_ARGV, TICK_MS, dockerExecEnvironment, type DockerExec,
} from "../src/dind-prune.js";
import {
  HibernationInventoryController, HibernationWriterRegistry, hibernationWriters,
  type HibernationDeps, type HibernationInventoryBody, type HibernationInventoryMember, type InventoryRequest,
} from "../src/hibernation-inventory.js";
import { nullLogger } from "./helpers.js";

// #4369 (PRD #4208 D16): at Docker storage scope the inventory always names `dind_prune` uncovered.
// A client-side close of a prune step is not daemon settlement, so no prune outcome (failure, kill,
// abort, later success), restart, empty registry or unwired leg may yield a complete inventory.
// These cases drive the real DindPruneController through its public loop on the shared
// `hibernationWriters`, then run the real inventory controller. The literal name is used on purpose.

// dockerExecEnvironment creates a shared empty DOCKER_CONFIG dir in tmpdir; remove it so
// tmpdir-leak-guard stays clean.
after(() => { rmSync(dockerExecEnvironment("unix:///unused").DOCKER_CONFIG as string, { recursive: true, force: true }); });

const wire = JSON.parse(readFileSync(new URL("../../fixtures/hibernation-inventory/wire.json", import.meta.url), "utf8"));
const member = wire.heartbeat_response_member.awaiting.hibernation_inventory as HibernationInventoryMember;
const START = 1_700_000_000_000;
const key = (argv: readonly string[]) => argv.join(" ");

const killedError = () => Object.assign(new Error("Command failed: docker builder prune -f"), { killed: true, signal: "SIGKILL", code: null });

interface Drive {
  calls: string[];
  /** Resolves once the loop has entered a pending exec for `docker builder prune`. */
  builderEntered: Promise<void>;
  controller: AbortController;
  done: Promise<void>;
}

/** Real DindPruneController over the public loop(): two over-watermark ticks reach the prune. */
function drive(exec: (argv: readonly string[], signal: AbortSignal | undefined) => Promise<string>, maxSleeps: number): Drive {
  let now = START;
  let sleeps = 0;
  const calls: string[] = [];
  const controller = new AbortController();
  let markEntered!: () => void;
  const builderEntered = new Promise<void>((resolve) => { markEntered = resolve; });
  const ctl = createDindPrune(
    { dindPruneEnabled: true, dockerWiring: { dockerHost: "unix:///test.sock" }, heartbeatIntervalMs: 15_000, workerName: "dind-prune-coverage" },
    {
      gate: new DindPruneGate(), isIdle: () => true, log: nullLogger(),
      now: () => now,
      readSample: () => ({ epochS: now / 1000 + 1, bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 }),
      exec: (async (argv, opts) => {
        calls.push(key(argv));
        if (key(argv) === key(DIND_PRUNE_ARGV.listRunning)) return "";
        if (key(argv) === key(DIND_PRUNE_ARGV.pruneBuildCache)) markEntered();
        return exec(argv, opts.signal);
      }) satisfies DockerExec,
      sleep: async () => {
        now += TICK_MS;
        ctl?.recordCustody(false, now);
        if (++sleeps >= maxSleeps) controller.abort();
      },
    },
  );
  assert.ok(ctl, "the prune controller must be built for an enabled, docker-wired config");
  ctl.recordCustody(false, now);
  return { calls, builderEntered, controller, done: ctl.loop(controller.signal) };
}

const ok = async (): Promise<string> => "";

interface Rig {
  posts: HibernationInventoryBody[];
  inventory(): Promise<InventoryRequest>;
  controller: HibernationInventoryController;
  gate: DindPruneGate;
  registry: HibernationWriterRegistry;
  seen: { proof: number; upgrade: number; census: number };
}

function inventoryRig(t: TestContext, over: Partial<HibernationDeps> & { pvcs?: HibernationInventoryMember["pvcs"]; docker?: null } = {}): Rig {
  const registry = over.registry ?? hibernationWriters;
  const gate = new DindPruneGate();
  const posts: HibernationInventoryBody[] = [];
  const seen = { proof: 0, upgrade: 0, census: 0 };
  let mode: "shared" | "exclusive" = "shared";
  const { pvcs, docker, ...deps } = over;
  const m: HibernationInventoryMember = pvcs ? { ...member, pvcs } : member;
  const controller = new HibernationInventoryController({
    registry, gate, laneSupported: true, dockerStorage: true,
    client: {
      hasFeature: () => true,
      currentRegisterNonce: () => m.register_nonce,
      postHibernationInventory: async (body) => { posts.push(body); },
      postHibernationExclusionFinal: async () => {},
    },
    ownership: {
      get mode() { return mode; },
      async upgradeExclusive() { seen.upgrade++; mode = "exclusive"; return true; },
      async reacquireShared() { mode = "shared"; },
    } as never,
    podUid: () => m.pod_uid, isIdle: () => true,
    proof: async () => { seen.proof++; return true; },
    census: async () => {
      seen.census++;
      return {
        complete: true, entries: [], fingerprint: "f".repeat(64), digest: "d".repeat(64),
        docker: docker === null ? null : { containers: [], volumes: [], images_count: 0, build_cache_bytes: 0 }, uncovered: [],
      };
    },
    recomputeFingerprint: async () => ({ fingerprint: "f".repeat(64), digest: "d".repeat(64) }),
    fatalExit: (reason) => assert.fail(reason), log: nullLogger(),
    ...deps,
  });
  t.after(() => {
    controller.observe(null);
    registry.unlatch();
    controller.stop();
  });
  return {
    posts, controller, gate, registry, seen,
    async inventory() {
      controller.observe(m);
      await controller.whenIdle();
      const body = posts[posts.length - 1];
      assert.ok(body && "complete" in body, `the real controller must post a full inventory, posted: ${JSON.stringify(posts)}`);
      return body;
    },
  };
}

function assertGap(inv: InventoryRequest, why: string): void {
  assert.ok(inv.exclusion.uncovered.includes("dind_prune"), `${why}: uncovered must name dind_prune, got ${JSON.stringify(inv.exclusion.uncovered)}`);
  assert.equal(inv.complete, false, `${why}: a Docker-scoped inventory must be complete=false`);
}

describe("dind_prune is an uncovered writer at Docker storage scope (#4369)", () => {
  it("(a) a builder prune killed by timeout: client closed, daemon work unsettled", async (t) => {
    let daemonSettled = false;
    const daemonWork = new Promise<void>(() => { /* BuildKit is still working; never settles here */ });
    void daemonWork.then(() => { daemonSettled = true; });
    const d = drive(async (argv) => {
      if (key(argv) === key(DIND_PRUNE_ARGV.pruneBuildCache)) throw killedError();
      return "";
    }, 2);
    await d.done;
    assert.ok(d.calls.includes(key(DIND_PRUNE_ARGV.pruneBuildCache)), `the builder prune step must have run, calls: ${d.calls.join(" | ")}`);
    assert.equal(daemonSettled, false);
    assertGap(await inventoryRig(t).inventory(), "after a killed builder prune");
  });

  it("(b) a builder prune cancelled through the loop abort signal", async (t) => {
    // Steps other than the builder prune resolve; only the builder step stays pending until abort.
    const d = drive((argv, signal) => new Promise<string>((resolve, reject) => {
      if (key(argv) !== key(DIND_PRUNE_ARGV.pruneBuildCache)) return resolve("");
      signal?.addEventListener("abort", () => reject(Object.assign(new Error("The operation was aborted"), { name: "AbortError" })), { once: true });
    }), Number.POSITIVE_INFINITY);
    await d.builderEntered;
    d.controller.abort();
    await d.done;
    assert.ok(d.calls.includes(key(DIND_PRUNE_ARGV.pruneBuildCache)), `the builder prune step must have run, calls: ${d.calls.join(" | ")}`);
    assertGap(await inventoryRig(t).inventory(), "after an aborted builder prune");
  });

  it("(c) a later successful prune step clears nothing", async (t) => {
    const first = drive(async (argv) => {
      if (key(argv) === key(DIND_PRUNE_ARGV.pruneBuildCache)) throw killedError();
      return "";
    }, 2);
    await first.done;
    const second = drive(ok, 2);
    await second.done;
    assert.ok(second.calls.includes(key(DIND_PRUNE_ARGV.pruneBuildCache)), `the successful prune must have run, calls: ${second.calls.join(" | ")}`);
    assertGap(await inventoryRig(t).inventory(), "after a later successful prune");
  });

  it("(d) a restart: a fresh registry with no prune controller ever built", async (t) => {
    assertGap(await inventoryRig(t, { registry: new HibernationWriterRegistry() }).inventory(), "after a restart");
  });

  it("(e) a disabled or unwired prune leg", async (t) => {
    const deps = { gate: new DindPruneGate(), isIdle: () => true, log: nullLogger() };
    const base = { heartbeatIntervalMs: 15_000, workerName: "w" };
    assert.equal(createDindPrune({ ...base, dindPruneEnabled: false, dockerWiring: { dockerHost: "unix:///test.sock" } }, deps), undefined);
    assert.equal(createDindPrune({ ...base, dindPruneEnabled: true, dockerWiring: {} }, deps), undefined);
    assertGap(await inventoryRig(t, { registry: new HibernationWriterRegistry() }).inventory(), "with the prune leg disabled or unwired");
  });

  for (const message of [
    "Error response from daemon: error pruning build cache: context deadline exceeded",
    "Cannot connect to the Docker daemon at unix:///test.sock. Is the docker daemon running?",
  ]) {
    it(`(f) a degraded leg: the docker step fails with "${message.slice(0, 40)}..."`, async (t) => {
      const d = drive(async (argv) => {
        if (key(argv) === key(DIND_PRUNE_ARGV.pruneDangling)) throw Object.assign(new Error(message), { code: 1 });
        return "";
      }, 2);
      await d.done;
      assert.ok(d.calls.includes(key(DIND_PRUNE_ARGV.pruneDangling)), `the first prune step must have run, calls: ${d.calls.join(" | ")}`);
      assertGap(await inventoryRig(t).inventory(), "after a daemon error or refusal");
    });
  }

  for (const [label, desc] of [
    ["(u1) degraded socket wiring (sidecar expected, no daemon reachable)", "no daemon wired"],
    ["(u2) epoch ownership unsupported on a Docker worker", "epoch unsupported"],
  ] as const) {
    it(`${label}: answers unsupported naming dind_prune and takes no exclusion`, async (t) => {
      const r = inventoryRig(t, { laneSupported: false });
      r.controller.observe(member);
      await r.controller.whenIdle();
      const post = r.posts[0];
      assert.ok(post && "status" in post, `${desc}: an unsupported status must be posted, posted: ${JSON.stringify(r.posts)}`);
      assert.equal(post.status, "unsupported");
      assert.ok(post.reason.includes("dind_prune"), `${desc}: the unsupported reason must name dind_prune, got ${JSON.stringify(post.reason)}`);
      assert.equal(r.posts.some((p) => "complete" in p), false, "no full inventory is posted");
      assert.equal(r.registry.isLatched(), false, "the registry is never latched");
      assert.equal(r.gate.claimsClosed(), false, "the claim gate is never closed");
      assert.deepEqual(r.seen, { proof: 0, upgrade: 0, census: 0 }, "no proof, EX upgrade or census");
    });
  }

  it("positive control: a worker without Docker storage scope stays complete", async (t) => {
    const r = inventoryRig(t, {
      registry: new HibernationWriterRegistry(), dockerStorage: false, docker: null,
      pvcs: member.pvcs.filter((p) => p.role === "data" || p.role === "nix"),
    });
    const inv = await r.inventory();
    assert.equal(inv.complete, true);
    assert.deepEqual(inv.exclusion.uncovered, []);
  });
});
