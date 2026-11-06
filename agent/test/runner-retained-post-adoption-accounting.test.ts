// issue #3994 - accounting across claims. A transient failure of the post-adoption ownership read
// must not turn into an adoption_failed hold, a recovery_wait park or a requeue. Every scenario
// states which of three different things happened: a CONFIRMED recovery_wait park (a report whose
// ACK carries status recovery_wait and the same cause, which parks the flight), an UNCONFIRMED park
// (a report sent but never confirmed, a nonterminal stop) or an owner RESUME (a later claim that
// carries a renewed owner grant). The server's requeue of a run whose flight is missing is a model
// (FakeApi.requeueRun), not the server's detection.
import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { ClaimResponse } from "../src/protocol.js";
import type { RunRunner } from "../src/runner.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

let transportHook: typeof fetch | undefined;
installHarness({ fetch: (input, init) => (transportHook ?? globalThis.fetch)(input, init) });
afterEach(() => { transportHook = undefined; });

const CAUSE = "retained_recovery_adoption_failed";
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

type Fault = "ok" | 503 | "enotfound";
interface Rig {
  claim: ClaimResponse;
  bare: string;
  clone: { path: string; branch: string };
  runner: RunRunner;
  /** One entry per runner.execute(), in claim order. */
  flights: Array<{ parked: boolean; claimGeneration: number }>;
  counters: { models: number; prepares: number; captures: number; confirmedParks: number; unconfirmedParks: number; windowGets: number };
  script: Fault[];
  /** Fired before the real retry wait of the post-adoption window. */
  onWait?: (flight: any) => void;
  /** How the fake server answers an adoption_failed recovery_wait report. */
  ack: "confirm" | "status-only";
  log: ReturnType<typeof recordingLogger>;
}

async function rig(options: { script: Fault[]; ack?: "confirm" | "status-only"; onWait?: (flight: any) => void }): Promise<Rig> {
  const claim = gitlabClaim(2513, { claim_generation: 2, worker_recovery_episode: 7, worker_recovery_owner_episode: 0 });
  const bare = await git.ensureClone(fx.originPath);
  const clone = await git.createOrAttachRunnerClone(bare, 2513, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "local work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  client.protocolFeatures = [`recovery_cause_${CAUSE}`];
  const log = recordingLogger();
  const r = {
    claim, bare, clone, flights: [] as Rig["flights"],
    counters: { models: 0, prepares: 0, captures: 0, confirmedParks: 0, unconfirmedParks: 0, windowGets: 0 },
    script: options.script, ack: options.ack ?? "confirm", onWait: options.onWait, log,
  } as unknown as Rig;
  const runId = claim.run_id;
  api.stateAckWire = (body, run) => {
    if (body.status !== "recovery_wait" || body.recovery_cause !== CAUSE) return undefined;
    if (r.ack === "status-only") { r.counters.unconfirmedParks++; return { status: 200, body: { run: { ...run, status: "recovery_wait" } } }; }
    r.counters.confirmedParks++;
    return { status: 200, body: { run: { ...run, status: "recovery_wait", recovery_wait_cause: body.recovery_cause } } };
  };
  const capture = git.recordRecoveryCapture.bind(git);
  git.recordRecoveryCapture = async (...args) => { r.counters.captures++; return capture(...args); };
  let armed = false, dirty = false, generation = 2;
  const prepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => {
    r.counters.prepares++;
    const out = await prepare(...args);
    armed = true;
    return out;
  };
  const realFetch = globalThis.fetch;
  transportHook = async (input, init) => {
    const url = String(input);
    if ((init?.method ?? "GET") !== "GET" || !url.endsWith(`/runs/${runId}/ownership`)) return realFetch(input, init);
    if (dirty) { api.setOwnershipStatus(runId, "running", generation); dirty = false; }
    if (!armed || r.counters.models > 0) return realFetch(input, init);
    const fault = r.script[r.counters.windowGets++] ?? "ok";
    if (fault === 503) { api.failOwnership(runId, 503); dirty = true; }
    else if (fault === "enotfound") throw new TypeError("fetch failed", { cause: Object.assign(new Error("ENOTFOUND"), { code: "ENOTFOUND" }) });
    return realFetch(input, init);
  };
  r.runner = runnerWith(() => ({ executor: {
    sandboxesCommands: false,
    run: async () => { r.counters.models++; throw new Error("model reached"); },
  } as never }), fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
  const phaseClone = (r.runner as any).phaseClone.bind(r.runner);
  (r.runner as any).phaseClone = async (...args: any[]) => {
    r.flights.push(args[1]);
    return phaseClone(...args);
  };
  const wait = (r.runner as any).waitRecoveryRetry.bind(r.runner);
  (r.runner as any).waitRecoveryRetry = async (flight: any, ...rest: unknown[]) => {
    if (armed && r.counters.models === 0) r.onWait?.(flight);
    return wait(flight, ...rest);
  };
  // Serve claims through the fake server's claim endpoint (counted), one execute per claim.
  (r as any).serve = async (between: (claim: ClaimResponse, n: number) => void): Promise<void> => {
    api.enqueueClaim(claim);
    let n = 0;
    for (;;) {
      const served = await client.claimRun();
      if (!served) return;
      generation = served.claim_generation ?? generation;
      armed = false;
      await r.runner.execute(served);
      armed = false;
      between(served, ++n);
    }
  };
  return r;
}
const serve = (r: Rig, between: (claim: ClaimResponse, n: number) => void = () => {}) =>
  (r as any).serve(between) as Promise<void>;
const journal = (r: Rig) => JSON.parse(command(r.bare, "config", `uzi-recovery.${r.clone.branch}.clone`)).recovery;
const parkReports = () => api.states.filter(s => s.body.status === "recovery_wait" && s.body.recovery_cause === CAUSE);
const failedReports = () => api.states.filter(s => s.body.status === "failed");

it("accounting (a): a transient failure at gen 2 is one claim with no requeue, hold or park and the model is entered", async () => {
  const r = await rig({ script: [503] });
  await serve(r);
  assert.equal(api.claimsServed, 1);
  assert.equal(api.requeues, 0);
  assert.equal(parkReports().length, 0, "no recovery_wait report");
  assert.equal(r.counters.confirmedParks, 0, "no confirmed park");
  assert.equal(r.counters.unconfirmedParks, 0, "no unconfirmed park");
  assert.equal(r.flights.length, 1);
  assert.equal(r.flights[0]!.parked, false);
  assert.equal(r.counters.windowGets, 2, "the failed read and the retried read");
  assert.equal(r.counters.models, 1);
  assert.equal(r.counters.prepares, 1);
  assert.equal(r.counters.captures, 1, "the one capture of the retained work at gen 2");
  assert.notEqual(journal(r).stage, "blocked", "no hold was added");
  assert.equal(journal(r).blocker, undefined);
  assert.equal(journal(r).attempts, 1);
});

it("accounting (b): an unknown failure at gen 2 is a CONFIRMED adoption_failed park; the gen-3 requeue reholds the blocked journal", async () => {
  const r = await rig({ script: ["enotfound"] });
  const afterFirst: Array<{ parked: boolean; confirmed: number; reports: number; captures: number; prepares: number }> = [];
  await serve(r, (claim, n) => {
    afterFirst.push({ parked: r.flights[n - 1]!.parked, confirmed: r.counters.confirmedParks,
      reports: parkReports().length, captures: r.counters.captures, prepares: r.counters.prepares });
    if (n === 1) api.requeueRun(claim, 3);
  });
  assert.deepEqual(afterFirst[0], { parked: true, confirmed: 1, reports: 1, captures: 1, prepares: 1 },
    "gen 2: the report was ACKed with status and cause, which parks the flight");
  assert.equal(parkReports()[0]!.body.claim_generation, 2);
  assert.equal(api.claimsServed, 2);
  assert.equal(api.requeues, 1);
  assert.equal(r.flights.length, 2);
  assert.equal(r.flights[1]!.parked, true, "gen 3: the blocked journal is reheld with a confirmed park");
  assert.equal(parkReports().length, 2);
  assert.equal(parkReports()[1]!.body.claim_generation, 3);
  assert.equal(r.counters.confirmedParks, 2);
  assert.equal(r.counters.unconfirmedParks, 0);
  assert.equal(r.counters.models, 0);
  assert.equal(r.counters.prepares, 1, "the rehold adopts no second successor");
  assert.equal(r.counters.captures, afterFirst[0]!.captures, "the rehold captures nothing");
  assert.equal(journal(r).stage, "blocked");
  assert.equal(journal(r).blocker, "adoption_failed");
  assert.equal(journal(r).attempts, 1);
  assert.equal(failedReports().length, 0);
});

it("accounting (c): pause now during the transient retry wait departs without any park; the modeled gen-3 requeue runs a fresh iteration", async () => {
  const r = await rig({ script: [503], onWait: flight => { flight.steering.route("pause", "now", 1, false, undefined); } });
  const afterFirst: Array<{ parked: boolean; captures: number; prepares: number }> = [];
  await serve(r, (claim, n) => {
    afterFirst.push({ parked: r.flights[n - 1]!.parked, captures: r.counters.captures, prepares: r.counters.prepares });
    if (n === 1) api.requeueRun(claim, 3);
  });
  assert.deepEqual(afterFirst[0], { parked: false, captures: 1, prepares: 1 }, "gen 2 departed: no confirmed park");
  assert.equal(r.counters.confirmedParks, 0);
  assert.equal(r.counters.unconfirmedParks, 0, "no unconfirmed park either: no recovery_wait report was sent");
  assert.equal(parkReports().length, 0);
  assert.equal(api.claimsServed, 2);
  assert.equal(api.requeues, 1);
  // PIN (observed, not designed): the departed gen-2 successor is not resumed. The gen-3 claim
  // runs a fresh retained recovery iteration: a second capture and successor, one more attempt,
  // and the model. The departure added no hold and no park.
  assert.deepEqual({ gen3Parked: r.flights[1]!.parked, models: r.counters.models, prepares: r.counters.prepares,
    captures: r.counters.captures, blocker: journal(r).blocker, stage: journal(r).stage, attempts: journal(r).attempts },
  { gen3Parked: false, models: 1, prepares: 2, captures: 2, blocker: undefined, stage: "ready-for-model", attempts: 2 });
  assert.equal(parkReports().length, 0, "no adoption_failed hold was added");
  assert.deepEqual(failedReports().map(state => state.body.failure_reason), ["model reached"], "only the fixture's model failure");
});

it("accounting (d): after the confirmed park an owner RESUME (a renewed grant, no requeue) adopts and enters the model", async () => {
  const r = await rig({ script: ["enotfound"] });
  let resumes = 0;
  await serve(r, (claim, n) => {
    if (n !== 1) return;
    assert.equal(r.flights[0]!.parked, true, "gen 2 was a confirmed park");
    resumes++;
    api.setOwnershipStatus(claim.run_id, "running", 3);
    api.enqueueClaim({ ...claim, claim_generation: 3, worker_recovery_episode: 8, worker_recovery_owner_episode: 1 });
  });
  assert.equal(resumes, 1);
  assert.equal(api.requeues, 0, "a Resume is not a requeue");
  assert.equal(api.claimsServed, 2);
  assert.equal(parkReports().length, 1, "only the gen-2 park; the Resume parked nothing");
  assert.equal(r.counters.confirmedParks, 1);
  assert.equal(r.flights[1]!.parked, false);
  assert.equal(r.counters.models, 1);
  assert.equal(r.counters.prepares, 2, "the renewed grant adopts a fresh successor");
  assert.equal(failedReports().length, 1, "only the fixture's model failure");
  assert.equal(failedReports()[0]!.body.failure_reason, "model reached");
});

it("accounting (e): an unknown failure whose park report is never confirmed is an UNCONFIRMED park, a nonterminal stop", async () => {
  const r = await rig({ script: ["enotfound"], ack: "status-only" });
  await serve(r);
  assert.equal(api.claimsServed, 1);
  assert.equal(api.requeues, 0);
  assert.equal(r.counters.confirmedParks, 0, "no ACK carried the cause");
  assert.equal(r.counters.unconfirmedParks, 2, "the report and its one duplicate were sent");
  assert.equal(parkReports().length, 2);
  assert.equal(r.flights[0]!.parked, false);
  assert.equal(r.counters.models, 0);
  assert.equal(failedReports().length, 0, "nonterminal: no failed report");
  assert.equal(journal(r).blocker, "adoption_failed");
});
