import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { RecoveryReleaseResponse, RunOwnershipResponse, StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// issue #1924: a guarded claim that parks or fails BEFORE its clone adopted nothing, yet the api
// keeps its guarded hold open. The worker must send a settled forge_no_output release for exactly
// that generation, after the park/failure report, and only on a positive "nothing adopted" proof.

const GEN = 4;
const EMPTY_DIGEST = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
const TRANSIENT = () => new Error("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com");
const PERMANENT = () => new Error("fatal: Authentication failed for 'https://github.com/x/y.git/'");

interface Rig {
  events: string[];
  releases: Array<{ runId: string; generation?: number; evidence?: string; disposition?: unknown }>;
  coordinator: RecoveryCoordinator;
  run: (iid: number, ensureClone: () => Promise<never>, prepare?: (runId: string) => Promise<void>) => Promise<string>;
}

function rig(opts: { releaseFails?: boolean; parkAck?: StateAck;
  parkTransportFailure?: "once" | "always"; ownership?: RunOwnershipResponse } = {}): Rig {
  const events: string[] = [];
  const releases: Rig["releases"] = [];
  client.protocolFeatures = ["recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1"];
  client.hasFeature = (name: string) => name === "recovery_inventory_v1";
  let parkReports = 0;
  client.reportState = async (_runId: string, body: StateRequest): Promise<StateAck> => {
    events.push(`report:${body.status}`);
    if (body.status === "recovery_wait" && opts.parkTransportFailure &&
        (opts.parkTransportFailure === "always" || parkReports++ === 0)) throw new Error("lost park acknowledgement");
    return body.status === "recovery_wait" ? (opts.parkAck ?? { applied: true, status: "recovery_wait" }) : { applied: true, status: body.status };
  };
  client.getRunOwnership = async () => opts.ownership ?? ({ status: "failed", claim_generation: GEN, inventory_guarded: true });
  let runId = "";
  client.listRecoveryHolds = async () => ({ run_id: runId, holds: [
    { hold_id: "hold", generation: GEN, inventory_guarded: true, has_available_capture: false },
  ] });
  client.releaseRecoveryCustody = async (id, generation, evidence, disposition): Promise<RecoveryReleaseResponse> => {
    events.push("release");
    releases.push({ runId: id, generation, evidence, disposition });
    if (opts.releaseFails) throw new Error("not ready");
    return { run_id: id, generation, released: true, holds_released: 1 };
  };
  const coordinator = new RecoveryCoordinator({
    client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: "journal-key",
    unadoptedSettleRetryDelaysMs: [0, 0, 0],
  });
  return {
    events, releases, coordinator,
    run: async (iid, ensureClone, prepare) => {
      const claim = gitlabClaim(iid, { claim_generation: GEN, inventory_guarded: true });
      runId = claim.run_id;
      await prepare?.(runId);
      git.ensureClone = ensureClone;
      const { gitlab } = fakeGitlab();
      await runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key", { recovery: coordinator }).execute(claim);
      return runId;
    },
  };
}

function trackSettled(): Array<[string, number]> {
  const marked: Array<[string, number]> = [];
  (client as unknown as { markInventoryGuardedClaimSettled: (r: string, g: number) => void })
    .markInventoryGuardedClaimSettled = (r, g) => { marked.push([r, g]); };
  return marked;
}

for (const status of ["failed", "cancelled"] as const) {
  it(`pre-clone server ${status} ack settles exactly the empty guarded generation without a terminal re-report`, async () => {
    const r = rig({ parkAck: { applied: true, status } });
    const runId = await r.run(9120, async () => { throw TRANSIENT(); });
    assert.deepEqual(r.releases, [{ runId, generation: GEN, evidence: "forge_no_output",
      disposition: { kind: "settled", coverage_digest: EMPTY_DIGEST } }]);
    assert.ok(r.events.indexOf("release") > r.events.indexOf("report:recovery_wait"));
    assert.equal(r.events.includes(`report:${status}`), false, "the authoritative terminal needs no second report");
  });
}

it("a stale pre-clone terminal ack never releases guarded custody", async () => {
  const r = rig({ parkAck: { applied: false, status: "failed", reason: "stale_claim" } });
  await r.run(9121, async () => { throw TRANSIENT(); });
  assert.deepEqual(r.releases, []);
});

it("a server terminal ack keeps a guarded generation with journaled source", async () => {
  const r = rig({ parkAck: { applied: true, status: "failed" } });
  await r.run(9122, async () => { throw TRANSIENT(); }, async (runId) => {
    await r.coordinator.pin({ runId, generation: GEN, kind: "issue", branch: "task",
      sourceSha: "a".repeat(40), inventoryGuarded: true });
  });
  assert.deepEqual(r.releases, []);
});

it("a same-generation terminal ownership probe settles after a lost park acknowledgement", async () => {
  const r = rig({ parkTransportFailure: "always" });
  const runId = await r.run(9123, async () => { throw TRANSIENT(); });
  assert.deepEqual(r.releases, [{ runId, generation: GEN, evidence: "forge_no_output",
    disposition: { kind: "settled", coverage_digest: EMPTY_DIGEST } }]);
  assert.equal(r.events.includes("report:failed"), false);
});

it("a resend after a running ownership probe settles the terminal acknowledgement", async () => {
  const r = rig({ parkTransportFailure: "once", parkAck: { applied: true, status: "failed" },
    ownership: { status: "running", claim_generation: GEN } });
  await r.run(9124, async () => { throw TRANSIENT(); });
  assert.equal(r.releases.length, 1);
  assert.equal(r.events.filter(e => e === "report:recovery_wait").length, 2);
  assert.ok(r.events.indexOf("release") > r.events.lastIndexOf("report:recovery_wait"));
});

for (const ownership of [{ status: "failed" }, { status: "failed", claim_generation: GEN + 1 }] as const) {
  it(`a terminal ownership probe with ${"claim_generation" in ownership ? "mismatched" : "absent"} generation keeps custody`, async () => {
    const r = rig({ parkTransportFailure: "always", ownership });
    await r.run(9125, async () => { throw TRANSIENT(); });
    assert.deepEqual(r.releases, []);
  });
}

for (const source of ["owed pin", "retained clone"] as const) {
  it(`a server terminal acknowledgement retains an existing ${source}`, async () => {
    const r = rig({ parkAck: { applied: true, status: "failed" } });
    await r.run(9126, async () => { throw TRANSIENT(); }, async (runId) => {
      const bare = git.barePathFor(gitlabClaim(9126).repo.clone_url);
      fs.mkdirSync(bare, { recursive: true });
      git.resolveRecoveryBareDir = async () => bare;
      git.enumerateOwedCandidates = async () => source === "owed pin"
        ? [{ sha: "b".repeat(40), pinRef: "refs/x", contexts: [{ runId }] }] as never : [];
      git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [],
        clones: source === "retained clone" ? [{ path: "retained" }] : [], foreignOwners: [] }) as never;
    });
    assert.deepEqual(r.releases, []);
  });
}

it("a confirmed empty settle forgets the remembered guarded claim for exactly that generation", async () => {
  const r = rig();
  const marked = trackSettled();
  const runId = await r.run(9111, async () => { throw PERMANENT(); });
  assert.deepEqual(marked, [[runId, GEN]]);
});

it("an unconfirmed empty settle keeps the remembered guarded claim", async () => {
  const r = rig({ releaseFails: true });
  const marked = trackSettled();
  await r.run(9112, async () => { throw PERMANENT(); });
  assert.deepEqual(marked, []);
});

for (const [name, fail] of [["forge park", TRANSIENT], ["pre-clone failure", PERMANENT]] as const) {
  it(`${name}: a guarded generation that adopted nothing sends one settled forge_no_output release after the report`, async () => {
    const r = rig();
    const runId = await r.run(9101, async () => { throw fail(); });
    assert.deepEqual(r.releases, [{
      runId, generation: GEN, evidence: "forge_no_output",
      disposition: { kind: "settled", coverage_digest: EMPTY_DIGEST },
    }]);
    const report = name === "forge park" ? "report:recovery_wait" : "report:failed";
    assert.ok(r.events.indexOf(report) >= 0 && r.events.indexOf("release") > r.events.lastIndexOf(report),
      `release must follow the ${report} report: ${r.events.join(",")}`);
    assert.equal(r.events.filter(e => e === "release").length, 1);
  });

  it(`${name}: a journal record for the generation keeps the hold (fail closed)`, async () => {
    const r = rig();
    const claim = gitlabClaim(9102, { claim_generation: GEN, inventory_guarded: true });
    await r.coordinator.pin({
      runId: claim.run_id, generation: GEN, kind: "issue", branch: "task", sourceSha: "a".repeat(40), inventoryGuarded: true,
    });
    git.ensureClone = async () => { throw fail(); };
    const { gitlab } = fakeGitlab();
    await runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key", { recovery: r.coordinator }).execute(claim);
    assert.deepEqual(r.releases, [], "adopted source: no settled release");
  });
}

it("an owed pin of the run in the claim repo's bare keeps the hold", async () => {
  const r = rig();
  const bare = git.barePathFor("https://example.invalid/x/y.git");
  const claim = gitlabClaim(9103, { claim_generation: GEN, inventory_guarded: true });
  claim.repo.clone_url = "https://example.invalid/x/y.git";
  fs.mkdirSync(bare, { recursive: true });
  git.resolveRecoveryBareDir = async () => bare;
  git.enumerateOwedCandidates = async () => [{ sha: "b".repeat(40), pinRef: "refs/x", contexts: [{ runId: claim.run_id }] }] as never;
  git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] });
  git.ensureClone = async () => { throw PERMANENT(); };
  const { gitlab } = fakeGitlab();
  await runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key", { recovery: r.coordinator }).execute(claim);
  assert.deepEqual(r.releases, []);
});

it("an unverifiable bare keeps the hold", async () => {
  const r = rig();
  const claim = gitlabClaim(9104, { claim_generation: GEN, inventory_guarded: true });
  claim.repo.clone_url = "https://example.invalid/x/z.git";
  fs.mkdirSync(git.barePathFor(claim.repo.clone_url), { recursive: true });
  git.resolveRecoveryBareDir = async () => undefined;
  git.ensureClone = async () => { throw PERMANENT(); };
  const { gitlab } = fakeGitlab();
  await runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key", { recovery: r.coordinator }).execute(claim);
  assert.deepEqual(r.releases, []);
});

it("an api that is not ready is retried a bounded number of times, then the hold is kept", async () => {
  const r = rig({ releaseFails: true });
  await r.run(9105, async () => { throw PERMANENT(); });
  assert.equal(r.releases.length, 4, "one attempt plus the three configured retries, no more");
});

it("a malformed hold flag is not read as a closed hold: no settle, no forgotten claim", async () => {
  const r = rig();
  const marked = trackSettled();
  client.listRecoveryHolds = async (id: string) => ({ run_id: id, holds: [
    { hold_id: "hold", generation: GEN, inventory_guarded: "true", has_available_capture: false },
  ] } as never);
  await r.run(9113, async () => { throw PERMANENT(); });
  assert.deepEqual(r.releases, []);
  assert.deepEqual(marked, []);
});
