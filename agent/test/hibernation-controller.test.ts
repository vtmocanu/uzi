import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { DindPruneGate } from "../src/dind-prune.js";
import {
  HibernationInventoryController, HibernationWriterRegistry,
  type CensusResult, type HibernationDeps, type HibernationInventoryBody, type HibernationInventoryMember,
  type ExclusionFinalRequest,
} from "../src/hibernation-inventory.js";
import { nullLogger } from "./helpers.js";

const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/hibernation-inventory/wire.json", import.meta.url), "utf8")) as {
  heartbeat_response_member: { awaiting: { hibernation_inventory: HibernationInventoryMember } };
  inventory_request: { entries: unknown[]; fingerprint: string; digest: string; docker: unknown };
};
const AWAITING = wire.heartbeat_response_member.awaiting.hibernation_inventory;
const ACCEPTED: HibernationInventoryMember = { ...AWAITING, phase: "accepted" };
const OTHER: HibernationInventoryMember = { ...AWAITING, op_id: "55555555-5555-5555-5555-5555555555ff" };

function census(over: Partial<CensusResult> = {}): CensusResult {
  return {
    complete: true, entries: wire.inventory_request.entries as never, fingerprint: wire.inventory_request.fingerprint,
    digest: wire.inventory_request.digest, docker: wire.inventory_request.docker as never, uncovered: [], ...over,
  };
}

function rig(over: Partial<HibernationDeps> = {}) {
  const events: string[] = [];
  const posts: HibernationInventoryBody[] = [];
  const finals: ExclusionFinalRequest[] = [];
  const registry = new HibernationWriterRegistry();
  const gate = new DindPruneGate();
  const exits: string[] = [];
  const state = { mode: "shared" as "shared" | "exclusive", closed: false, upgrade: true as boolean | Error, reacquire: true as boolean, atShared: undefined as undefined | { gateClosed: boolean; latched: boolean } };
  const ownership = {
    get mode() { if (state.closed) throw new Error("epoch ownership closed"); return state.mode; },
    async upgradeExclusive() {
      events.push("ex");
      if (state.upgrade instanceof Error) throw state.upgrade;
      if (state.upgrade) state.mode = "exclusive";
      return state.upgrade;
    },
    async reacquireShared() {
      events.push("sh");
      state.atShared = { gateClosed: gate.claimsClosed(), latched: registry.isLatched() };
      if (!state.reacquire) { state.closed = true; throw new Error("sh failed"); }
      state.mode = "shared";
    },
  };
  const deps: HibernationDeps = {
    registry, gate,
    client: {
      hasFeature: () => true,
      currentRegisterNonce: () => AWAITING.register_nonce,
      postHibernationInventory: async (b) => { events.push("post"); posts.push(b); },
      postHibernationExclusionFinal: async (b) => { events.push("final"); finals.push(b); },
    },
    ownership: ownership as never,
    laneSupported: true,
    podUid: () => AWAITING.pod_uid,
    isIdle: () => true,
    proof: async () => {
      events.push(`proof latched=${registry.isLatched()} gate=${gate.heldBy("hibernation")}`);
      return true;
    },
    census: async () => { events.push("census"); return census(); },
    recomputeFingerprint: async () => ({ digest: "d".repeat(64), fingerprint: "f".repeat(64) }),
    fatalExit: (r) => { exits.push(r); },
    log: nullLogger(),
    settleMs: 200, retryAfterMs: 0,
    ...over,
  };
  return { c: new HibernationInventoryController(deps), events, posts, finals, registry, gate, state, exits };
}

const feed = async (r: ReturnType<typeof rig>, m: HibernationInventoryMember | null) => { r.c.observe(m); await r.c.whenIdle(); };

describe("HibernationInventoryController", () => {
  it("runs latch, settle, proof, EX, census, post in order and holds", async () => {
    const r = rig();
    await feed(r, AWAITING);
    assert.deepEqual(r.events, ["proof latched=true gate=true", "ex", "census", "post"]);
    const body = r.posts[0] as unknown as Record<string, unknown>;
    assert.equal(body.op_id, AWAITING.op_id);
    assert.equal(body.register_nonce, AWAITING.register_nonce);
    assert.equal(body.pod_uid, AWAITING.pod_uid);
    assert.equal(body.deployment_resource_version, AWAITING.deployment_resource_version);
    assert.deepEqual(body.pvcs, AWAITING.pvcs);
    assert.deepEqual(body.exclusion, { lane: "dind", mechanism: "epoch_ex+latch+quiescence", uncovered: [] });
    assert.equal(body.complete, true);
    assert.equal(r.gate.claimsClosed(), true);
    assert.equal(r.registry.isLatched(), true);
    assert.deepEqual(r.c.exclusionReport(), { op_id: AWAITING.op_id, held: true });
    // Repeats and the accepted phase change nothing.
    await feed(r, AWAITING);
    await feed(r, ACCEPTED);
    assert.equal(r.posts.length, 1);
    assert.deepEqual(r.c.exclusionReport(), { op_id: AWAITING.op_id, held: true });
  });

  it("merges registry and census uncovered classes and posts complete=false", async () => {
    const r = rig({ census: async () => census({ uncovered: ["c1"] }) });
    r.registry.markUncovered("w1");
    await feed(r, AWAITING);
    const body = r.posts[0] as unknown as { complete: boolean; exclusion: { uncovered: string[] } };
    assert.deepEqual(body.exclusion.uncovered, ["w1", "c1"]);
    assert.equal(body.complete, false, "an otherwise complete census is incomplete with uncovered writers");
  });

  it("posts complete=false for a registry-only uncovered class", async () => {
    const r = rig();
    r.registry.markUncovered("w1");
    await feed(r, AWAITING);
    assert.equal((r.posts[0] as unknown as { complete: boolean }).complete, false);
  });

  it("does not release, post a failure or exit when stopped during the census", async () => {
    let started!: () => void;
    const inCensus = new Promise<void>((res) => { started = res; });
    const r = rig({
      census: ({ signal }) => new Promise((_, reject) => {
        started();
        signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
      }),
    });
    r.c.observe(AWAITING);
    await inCensus;
    r.c.stop();
    await r.c.whenIdle();
    assert.deepEqual(r.exits, []);
    assert.equal(r.events.includes("sh"), false, "no reacquireShared on shutdown");
    assert.equal(r.posts.length, 0);
    assert.equal(r.gate.claimsClosed(), true);
    assert.equal(r.registry.isLatched(), true);
    assert.equal(r.state.mode, "exclusive");
    // Later members (including an absent one) cannot release it either.
    await feed(r, null);
    assert.equal(r.events.includes("sh"), false);
    assert.equal(r.gate.claimsClosed(), true);
  });

  it("reports held:false once after a post-EX failure releases the exclusion", async () => {
    const r = rig({ census: async () => { throw new Error("census boom"); } });
    await feed(r, AWAITING);
    assert.equal((r.posts[0] as { status: string }).status, "busy");
    assert.equal(r.state.mode, "shared");
    assert.deepEqual(r.c.exclusionReport(), { op_id: AWAITING.op_id, held: false });
    assert.equal(r.c.exclusionReport(), undefined, "sent once");
  });

  it("posts exclusion_failed, re-takes shared and reopens when a non-fatal EX attempt throws", async () => {
    const r = rig();
    r.state.upgrade = new Error("flock failed: 1");
    await feed(r, AWAITING);
    assert.deepEqual(r.exits, []);
    const body = r.posts[0] as { status: string; reason: string };
    assert.equal(body.status, "exclusion_failed");
    assert.match(body.reason, /flock failed/);
    assert.equal(r.events.includes("sh"), false, "the ownership layer already re-took shared itself");
    assert.equal(r.state.mode, "shared");
    assert.equal(r.gate.claimsClosed(), false);
    assert.equal(r.registry.isLatched(), false);
    assert.equal(r.c.exclusionReport(), undefined);
  });

  it("does nothing for a wrong pod, a wrong nonce, or an unadvertised feature", async () => {
    for (const over of [
      { podUid: () => "66666666-6666-6666-6666-6666666666ff" },
      { client: { hasFeature: () => true, currentRegisterNonce: () => "other", postHibernationInventory: async () => {}, postHibernationExclusionFinal: async () => {} } },
      { client: { hasFeature: () => false, currentRegisterNonce: () => AWAITING.register_nonce, postHibernationInventory: async () => {}, postHibernationExclusionFinal: async () => {} } },
    ] as Partial<HibernationDeps>[]) {
      const r = rig(over);
      await feed(r, AWAITING);
      assert.deepEqual(r.events, []);
      assert.equal(r.gate.claimsClosed(), false);
      assert.equal(r.registry.isLatched(), false);
      assert.equal(r.c.exclusionReport(), undefined);
    }
  });

  it("posts exclusion_failed, re-takes shared and reopens when EX is refused", async () => {
    const r = rig();
    r.state.upgrade = false;
    await feed(r, AWAITING);
    assert.deepEqual(r.events.slice(-2), ["ex", "post"]);
    assert.equal((r.posts[0] as { status: string }).status, "exclusion_failed");
    assert.equal(r.state.mode, "shared");
    assert.equal(r.gate.claimsClosed(), false);
    assert.equal(r.registry.isLatched(), false);
    assert.equal(r.c.exclusionReport(), undefined);
  });

  it("exits fatally when the epoch handle was lost during the upgrade", async () => {
    const r = rig();
    r.state.upgrade = new Error("epoch shared ownership lost");
    r.state.closed = true;
    await feed(r, AWAITING);
    assert.deepEqual(r.exits, ["hibernation_shared_ownership_lost"]);
    assert.equal(r.gate.claimsClosed(), true, "the gate stays closed");
  });

  it("posts busy when the proof fails and never takes EX", async () => {
    const r = rig({ proof: async () => false });
    await feed(r, AWAITING);
    assert.equal((r.posts[0] as { status: string }).status, "busy");
    assert.equal(r.events.includes("ex"), false);
    assert.equal(r.gate.claimsClosed(), false);
    assert.equal(r.registry.isLatched(), false);
  });

  it("posts busy when writers do not settle in time", async () => {
    const r = rig();
    r.registry.enter("stuck");
    await feed(r, AWAITING);
    assert.equal((r.posts[0] as { status: string }).status, "busy");
    assert.match((r.posts[0] as { reason: string }).reason, /stuck/);
    assert.equal(r.events.includes("ex"), false);
  });

  it("posts busy with a bounded reason when the census throws", async () => {
    const r = rig({ census: async () => { throw new Error("x".repeat(5000)); } });
    await feed(r, AWAITING);
    const post = r.posts[0] as { status: string; reason: string };
    assert.equal(post.status, "busy");
    assert.ok(post.reason.length <= 200);
    assert.equal(r.state.mode, "shared", "SH re-taken after the failure");
  });

  it("bounds entries and the serialized body, marking the inventory incomplete", async () => {
    const entry = (i: number) => ({ kind: "unclassified", path: `/data/p${i}`, classification: "unknown" });
    const many = Array.from({ length: 10_050 }, (_, i) => entry(i));
    const r = rig({ census: async () => census({ entries: many }) });
    await feed(r, AWAITING);
    const body = r.posts[0] as { entries: unknown[]; complete: boolean };
    assert.equal(body.entries.length, 10_000);
    assert.equal(body.complete, false);
    const fat = Array.from({ length: 5000 }, (_, i) => ({ ...entry(i), detail: "d".repeat(400) }));
    const r2 = rig({ census: async () => census({ entries: fat }) });
    await feed(r2, AWAITING);
    const body2 = r2.posts[0] as { entries: unknown[]; complete: boolean };
    assert.ok(Buffer.byteLength(JSON.stringify(body2)) <= 1024 * 1024);
    assert.ok(body2.entries.length < 5000);
    assert.equal(body2.complete, false);
  });

  it("posts unsupported when the lane has no exclusion mechanism", async () => {
    const r = rig({ laneSupported: false });
    await feed(r, AWAITING);
    assert.equal((r.posts[0] as { status: string }).status, "unsupported");
    assert.equal(r.gate.claimsClosed(), false);
  });

  it("releases on an absent member: shared first, then the gate reopens", async () => {
    const r = rig();
    await feed(r, AWAITING);
    await feed(r, null);
    assert.deepEqual(r.events.slice(-1), ["sh"]);
    // Order: shared ownership is re-taken while the gate is still closed and the latch held.
    assert.deepEqual(r.state.atShared, { gateClosed: true, latched: true });
    assert.equal(r.state.mode, "shared");
    assert.equal(r.gate.claimsClosed(), false);
    assert.equal(r.registry.isLatched(), false);
    assert.equal(r.c.exclusionReport(), undefined);
  });

  it("releases when the member names another op, then starts the new one", async () => {
    const r = rig();
    await feed(r, AWAITING);
    await feed(r, OTHER);
    assert.deepEqual(r.events.filter((e) => e === "sh" || e === "ex" || e === "post"), ["ex", "post", "sh", "ex", "post"]);
    assert.equal((r.posts[1] as { op_id: string }).op_id, OTHER.op_id);
  });

  it("ignores an unknown (malformed) observation", async () => {
    const r = rig();
    await feed(r, AWAITING);
    r.c.observe(undefined);
    await r.c.whenIdle();
    assert.equal(r.gate.claimsClosed(), true);
  });

  it("exits fatally and keeps the gate closed when shared cannot be re-taken", async () => {
    const r = rig();
    await feed(r, AWAITING);
    r.state.reacquire = false;
    await feed(r, null);
    assert.deepEqual(r.exits, ["hibernation_reacquire_shared_failed"]);
    assert.equal(r.gate.claimsClosed(), true);
    assert.equal(r.gate.heldBy("hibernation"), true);
  });

  it("reports held:false after exclusion is lost or the latch is broken", async () => {
    const r = rig();
    await feed(r, AWAITING);
    r.state.mode = "shared";
    assert.deepEqual(r.c.exclusionReport(), { op_id: AWAITING.op_id, held: false });
    r.state.mode = "exclusive";
    assert.equal(r.c.exclusionReport()?.held, true);
    r.registry.unlatch();
    assert.equal(r.c.exclusionReport()?.held, false);
    r.registry.latch();
    r.gate.open("hibernation");
    assert.equal(r.c.exclusionReport()?.held, false);
  });

  it("posts exclusion-final with recomputed values at shutdown for an accepted op", async () => {
    const r = rig();
    await feed(r, AWAITING);
    await feed(r, ACCEPTED);
    await r.c.finalizeAtShutdown();
    assert.deepEqual(r.finals, [{
      op_id: AWAITING.op_id, pod_uid: AWAITING.pod_uid, register_nonce: AWAITING.register_nonce,
      held: true, digest: "d".repeat(64), fingerprint: "f".repeat(64),
    }]);
  });

  it("sends nothing at shutdown when the op is not accepted, exclusion was lost, or recompute fails", async () => {
    const pending = rig();
    await feed(pending, AWAITING);
    await pending.c.finalizeAtShutdown();
    assert.equal(pending.finals.length, 0);

    const lost = rig();
    await feed(lost, AWAITING);
    await feed(lost, ACCEPTED);
    lost.state.mode = "shared";
    await lost.c.finalizeAtShutdown();
    assert.equal(lost.finals.length, 0);

    const failing = rig({ recomputeFingerprint: async () => { throw new Error("timed out"); } });
    await feed(failing, AWAITING);
    await feed(failing, ACCEPTED);
    await failing.c.finalizeAtShutdown();
    assert.equal(failing.finals.length, 0);
  });

  it("sends nothing when the recompute never settles within its bound", async (t) => {
    const r = rig({ recomputeFingerprint: () => new Promise(() => {}) });
    // Build the held state with real timers, then fake them for the shutdown bound.
    await feed(r, AWAITING);
    await feed(r, ACCEPTED);
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const done = r.c.finalizeAtShutdown();
    t.mock.timers.tick(10_000);
    await done;
    assert.equal(r.finals.length, 0);
  });
});
