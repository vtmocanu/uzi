import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { RecoveryReleaseResponse, StateAck, StateRequest } from "../src/protocol.js";
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
  run: (iid: number, ensureClone: () => Promise<never>) => Promise<string>;
}

function rig(opts: { releaseFails?: boolean } = {}): Rig {
  const events: string[] = [];
  const releases: Rig["releases"] = [];
  client.protocolFeatures = ["recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1"];
  client.hasFeature = (name: string) => name === "recovery_inventory_v1";
  client.reportState = async (_runId: string, body: StateRequest): Promise<StateAck> => {
    events.push(`report:${body.status}`);
    return body.status === "recovery_wait" ? { applied: true, status: "recovery_wait" } : { applied: true, status: body.status };
  };
  client.getRunOwnership = async () => ({ status: "failed", claim_generation: GEN, inventory_guarded: true });
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
    run: async (iid, ensureClone) => {
      const claim = gitlabClaim(iid, { claim_generation: GEN, inventory_guarded: true });
      runId = claim.run_id;
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
