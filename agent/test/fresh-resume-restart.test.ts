import { after, before, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { AuthorityRuntime } from "../src/authority-runtime.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { attemptClonePath } from "../src/attempt-path.js";
import { parseAttemptLedgerEntry } from "../src/git.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { ExecutorFactory } from "../src/runner.js";
import { quiesceRunAttempt, setQuiescenceViewForTests, mintAttemptId, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { freshJournalJson, freshJournalKey, parseFreshJournal } from "../src/fresh-journal.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { HERMETIC_VIEW, restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// issue #4628 m3-B: the destination worker after a restart. An ordinary claim of a run whose branch carries a fresh journal asks the
// server what became of the transfer (outside every lock), and the answer, not the local files, decides: adopt A (positive), set A
// aside (negative), or stop nonterminally and keep custody (anything else). Each case is driven through RunRunner.execute() with
// real git, the real client and the fake api's scripted state route. The crash rig builds the bare exactly as a process kill at
// each step of the seed would have left it.

installHarness();
before(() => setQuiescenceViewForTests(HERMETIC_VIEW));
after(() => restoreHermeticView());

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const SEED_GENERATION = 3;
const RESTART_GENERATION = 4;
const CHECKPOINT_REF = "refs/uzi-checkpoints/agent/issue-fresh";
const PARK_FEATURE = "recovery_cause_retained_recovery_adoption_failed";

const sh = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, stdio: "pipe" }).trim();
const quiesce = (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => quiesceRunAttempt(req, { docker: { intervalMs: 5, deadlineMs: 3_000, requestTimeoutMs: 2_000 } });

function publishCheckpoint(): string {
  sh(fx.originPath, ["checkout", "-q", "-B", "tmp-cp", "main"]);
  fs.writeFileSync(path.join(fx.originPath, "saved.txt"), "saved by the previous worker\n");
  sh(fx.originPath, ["add", "-A"]);
  sh(fx.originPath, ["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "work saved"]);
  const sha = sh(fx.originPath, ["rev-parse", "HEAD"]);
  sh(fx.originPath, ["update-ref", CHECKPOINT_REF, sha]);
  sh(fx.originPath, ["checkout", "-q", "main"]);
  sh(fx.originPath, ["branch", "-D", "tmp-cp"]);
  return sha;
}

interface Scene {
  claim: ClaimResponse;
  branch: string;
  runId: string;
  bare: string;
  sha: string;
  frid: string;
  attemptId: string;
  clonePath: string;
}

type Step = "L0" | "L1" | "L2";

/** The bare as a kill leaves it after `upTo`: L0 the journal, L1 also the pending ledger entry, L2 also the seeded clone. */
async function scene(iid: number, upTo: Step): Promise<Scene> {
  simulateCommittedWork();
  const sha = publishCheckpoint();
  const claim = gitlabClaim(iid, { claim_generation: RESTART_GENERATION });
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const bare = await git.ensureClone(fx.originPath);
  sh(bare, ["fetch", "--no-tags", "--quiet", fx.originPath, `+${CHECKPOINT_REF}:${CHECKPOINT_REF}`]);
  const frid = randomUUID();
  const attemptId = mintAttemptId(SEED_GENERATION);
  const clonePath = attemptClonePath(git.runnerClonePath(bare, key), attemptId);
  const s: Scene = { claim, branch, runId: claim.run_id, bare, sha, frid, attemptId, clonePath };
  if (upTo === "L2") {
    await git.seedFreshAttempt({ barePath: bare, branch, key, runId: claim.run_id, attemptId, frid, generation: SEED_GENERATION,
      artifact: { kind: "checkpoint_ref", artifactRef: CHECKPOINT_REF, artifactSha: sha }, isLive: () => false, beforeSeed: async () => {} });
    return s;
  }
  sh(bare, ["config", "--local", freshJournalKey(branch), freshJournalJson({ frid, attemptId, generation: SEED_GENERATION, sha, stage: "seeding" })]);
  if (upTo === "L1") {
    sh(bare, ["config", "--local", "--add", `uzi-attempts.${branch}.entry`,
      JSON.stringify({ attemptId, runId: claim.run_id, clonePath, state: "fresh_pending" })]);
  }
  return s;
}

const cfg = (s: Scene, key: string): string[] => { try { return sh(s.bare, ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; } };
const ledgerState = (s: Scene): string | undefined => parseAttemptLedgerEntry(cfg(s, `uzi-attempts.${s.branch}.entry`).at(-1) ?? "")?.state;
const freshStage = (s: Scene): string | undefined => { const v = cfg(s, freshJournalKey(s.branch)).at(-1); return v ? parseFreshJournal(v).stage : undefined; };
const recoveryJournal = (s: Scene): string[] => cfg(s, `uzi-recovery.${s.branch}.clone`);
const bareConfigBytes = (s: Scene): string => fs.readFileSync(path.join(s.bare, "config"), "utf8");

const seededReply = (s: Scene, over: Record<string, unknown> = {}) => ({ status: 200, body: {
  state: "seeded", seeded_at: "2026-01-01T00:00:00Z", destination_attempt_id: s.attemptId, seeded_generation: SEED_GENERATION, current_authority: true, ...over } });
const terminalReply = (state: "refused" | "cancelled", seededAt: string | null = null) => ({ status: 200, body: {
  state, seeded_at: seededAt, destination_attempt_id: null, seeded_generation: null, current_authority: false } });
const script = (answer: { status: number; body: unknown } | undefined): void => {
  api.freshResume = { state: () => answer as { status: number; body: unknown } };
};

function factoryFor(): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: { run: async (_ctx: RunContext): Promise<ExecutorResult> => { started++; throw new Error("stop after the model starts"); } },
  });
  return { factory, started: () => started };
}

const run = (factory: ExecutorFactory, extra: Record<string, unknown> = {}, log = nullLogger()) =>
  runnerWith(factory, fakeGitlab().gitlab, undefined, log, { quiesceRun: quiesce, recoveryRetryMs: 5, ...extra });
const reported = (runId: string): string[] => api.states.filter(s => s.runId === runId).map(s => String(s.body.status));
const stateCalls = () => api.freshResumeCalls.filter(c => c.route === "state");
const parks = (runId: string) => api.states.filter(s => s.runId === runId && s.body.status === "recovery_wait");

/** A stand-in for the runner's AuthorityRuntime: only the three reads the fresh discovery makes are real; everything else is inert. */
function stubAuthority(slot: SlotSnapshot | null, o: { self?: string; declined?: boolean } = {}): AuthorityRuntime {
  const real: Record<string | symbol, unknown> = {
    peekSlot: async () => slot, authorityIncarnation: () => o.self ?? "self-incarnation", sourceEndDeclined: async () => o.declined === true,
  };
  return new Proxy(real, { get: (t, p) => (p in t ? t[p] : () => undefined) }) as unknown as AuthorityRuntime;
}
const slotOf = (owner: string, state: string, pending: unknown = null): SlotSnapshot =>
  ({ version: "1", key: "k", state, epoch: "e", seq: 1, owner: { worker: owner, executor: null }, pending, retiring: null, publication: null, codexMarked: false }) as unknown as SlotSnapshot;

/** The api acknowledges a typed park as the real one does: with the cause in the run it returns. */
function ackParks(confirm: boolean): void {
  api.stateAckWire = (body, run) => body.status === "recovery_wait"
    ? { status: 200, body: { run: confirm ? { ...run, status: "recovery_wait", recovery_wait_cause: body.recovery_cause } : { id: run.id } } } : undefined;
}

describe("the crash rig: a process kill at each step of a fresh seed, then a restart", () => {
  it("a kill after L0 (journal only) and the state route says nothing decisive (404): the restart stops nonterminally and writes nothing", async () => {
    const s = await scene(9101, "L0");
    const before = bareConfigBytes(s);
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(stateCalls().length, 1, "the journal's own transfer is asked about; there is no attempt to discard");
    assert.equal(stateCalls()[0]!.frid, s.frid);
    assert.equal(bareConfigBytes(s), before, "the bare is byte-identical");
    assert.equal(reported(s.runId).includes("failed"), false);
  });

  for (const state of ["refused", "cancelled"] as const) {
    it(`N-2: a kill after L0 and the server says ${state} with no seed: the journal and its artifact ref are cleared and the claim goes on`, async () => {
      const s = await scene(state === "refused" ? 9170 : 9171, "L0");
      const ref = `refs/uzi-fresh/${s.frid}`;
      sh(s.bare, ["update-ref", ref, s.sha]);
      script(terminalReply(state));
      const { factory, started } = factoryFor();
      await run(factory).execute(s.claim);
      assert.equal(freshStage(s), undefined, "the journal is cleared");
      assert.throws(() => sh(s.bare, ["rev-parse", "--verify", "-q", ref]), "the artifact ref is gone");
      assert.equal(stateCalls().length, 1);
      assert.equal(started(), 1, "the ordinary path went on to start the model");
    });
  }

  it("N-2: a kill after L0 and the server's row records a seed: the journal stays and the claim stops", async () => {
    const s = await scene(9172, "L0");
    script(terminalReply("cancelled", "2026-01-01T00:00:00Z"));
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(freshStage(s), "seeding");
    assert.equal(reported(s.runId).includes("failed"), false);
  });

  it("a kill after L1 and the server refused: A is set aside as fresh_unbound and never adopted", async () => {
    const s = await scene(9102, "L1");
    script(terminalReply("refused"));
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.ok(cfg(s, `uzi-attempts.${s.branch}.entry`).some(v => parseAttemptLedgerEntry(v)?.attemptId === s.attemptId && parseAttemptLedgerEntry(v)?.state === "fresh_unbound"),
      "A's ledger history ends fresh_unbound");
    assert.ok(!recoveryJournal(s).some(v => v.includes(s.attemptId)), "the recovery journal never names A");
    assert.equal(stateCalls().length, 1);
    assert.equal(stateCalls()[0]!.frid, s.frid);
    assert.equal(stateCalls()[0]!.query.live_generation, String(RESTART_GENERATION), "asked with this claim's generation");
    assert.equal(started(), 1, "the unbound attempt is set aside and the ordinary claim goes on to start the model");
    const entries = cfg(s, `uzi-attempts.${s.branch}.entry`).map(v => parseAttemptLedgerEntry(v)!);
    assert.equal(entries.filter(e => e.attemptId === s.attemptId).at(-1)?.state, "fresh_unbound", "A's last entry stays fresh_unbound");
  });

  it("a kill during L2 (a partial clone, server still claimed): not decisive, so a stop; A stays pending and the clone is untouched", async () => {
    const s = await scene(9103, "L1");
    fs.mkdirSync(s.clonePath, { recursive: true });
    fs.writeFileSync(path.join(s.clonePath, "half-written"), "x");
    script({ status: 200, body: { state: "claimed", seeded_at: null, destination_attempt_id: null, seeded_generation: null, current_authority: true } });
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(ledgerState(s), "fresh_pending", "never unbound on an indecisive answer");
    assert.equal(fs.readFileSync(path.join(s.clonePath, "half-written"), "utf8"), "x");
    assert.equal(reported(s.runId).includes("failed"), false);
  });

  it("a kill after the server committed the seed (reply lost): the restart reads seeded and commits L4 once", async () => {
    const s = await scene(9104, "L2");
    assert.equal(freshStage(s), "seeding");
    script(seededReply(s));
    const { factory } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(freshStage(s), "ready");
    assert.ok(cfg(s, `uzi-attempts.${s.branch}.entry`).some(v => parseAttemptLedgerEntry(v)?.state === "live"));
    const journal = JSON.parse(recoveryJournal(s).at(-1)!) as { runId: string; clonePath: string; attemptId: string };
    assert.deepEqual({ runId: journal.runId, clonePath: journal.clonePath, attemptId: journal.attemptId }, { runId: s.runId, clonePath: s.clonePath, attemptId: s.attemptId });
    assert.match(s.attemptId, /-g3-/, "A keeps the generation it was minted at; no gx id exists");
    assert.equal(cfg(s, `uzi-attempts.${s.branch}.entry`).some(v => /-gx-/.test(v)), false);
  });

  it("a kill after L4: the restart is positive and rewrites nothing", async () => {
    const s = await scene(9105, "L2");
    script(seededReply(s));
    const first = factoryFor();
    await run(first.factory).execute(s.claim);
    assert.equal(freshStage(s), "ready");
    const journalsBefore = [recoveryJournal(s), cfg(s, `uzi-attempts.${s.branch}.entry`), cfg(s, freshJournalKey(s.branch))];
    const again = factoryFor();
    const { logger, lines } = recordingLogger();
    await run(again.factory, {}, logger).execute(gitlabClaim(9105, { claim_generation: RESTART_GENERATION + 1, run_id: s.runId } as never));
    const msgs = lines.map(l => (l as { msg: string }).msg);
    // The ready journal is decided locally: the claim goes on to the ordinary retained-recovery path of the live attempt (the first
    // flight's failed model left its clone retained), which is that path's own stop, not a fresh one.
    assert.ok(msgs.includes("retained recovery stopped; source and accounting kept"), "the ordinary retained-recovery path decided the second claim");
    assert.equal(msgs.some(m => m.startsWith("fresh resume")), false, "no fresh resume stop, hold or discovery line was logged");
    assert.equal(reported(s.runId).includes("failed"), false);
    assert.deepEqual([recoveryJournal(s), cfg(s, `uzi-attempts.${s.branch}.entry`).slice(0, journalsBefore[1]!.length), cfg(s, freshJournalKey(s.branch))],
      journalsBefore, "L0 to L4 are not rewritten by a later claim");
    assert.equal(stateCalls().length, 1, "only the first claim (journal seeding) asked the server; a ready journal is decided locally");
  });

  it("B-1: after L4, A retired and its path gone and compacted away, a later claim still starts and asks the server nothing", async () => {
    const s = await scene(9107, "L2");
    // L4 exactly as the receipt commit writes it, without a flight that would go on to fail and retain the clone for recovery.
    await git.commitFreshReceipt({ barePath: s.bare, branch: s.branch, runId: s.runId, clonePath: s.clonePath, attemptId: s.attemptId,
      frid: s.frid, generation: SEED_GENERATION, sha: s.sha });
    assert.equal(freshStage(s), "ready");
    const ref = `refs/uzi-fresh/${s.frid}`;
    sh(s.bare, ["update-ref", ref, s.sha]);
    const asked = stateCalls().length;
    // The attempt's run leaves it: retired, path removed. Leaving `live` ends the transfer, so the journal and its ref go with it.
    await git.retireRunnerClone(s.bare, s.clonePath, s.branch, s.runId, { discard: true, attemptId: s.attemptId });
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(ledgerState(s), "retired");
    assert.equal(freshStage(s), undefined, "the retire cleared the ready journal");
    assert.throws(() => sh(s.bare, ["rev-parse", "--verify", "-q", ref]), "and its artifact ref");
    // Two later claims (the first compacts A out of the ledger through its ordinary seed): neither stops or asks.
    for (const generation of [RESTART_GENERATION + 1, RESTART_GENERATION + 2]) {
      const next = factoryFor();
      await run(next.factory).execute(gitlabClaim(9107, { claim_generation: generation, run_id: s.runId } as never));
      assert.equal(next.started(), 1, `claim at generation ${generation} starts the model`);
    }
    assert.equal(stateCalls().length, asked, "no state-route call for a ready or cleared journal");
  });

  it("B-1: a ready journal whose attempt left the ledger (retired by a path that never cleared it) is cleared by compaction", async () => {
    const s = await scene(9108, "L2");
    script(seededReply(s));
    await run(factoryFor().factory).execute(s.claim);
    assert.equal(freshStage(s), "ready");
    const ref = `refs/uzi-fresh/${s.frid}`;
    sh(s.bare, ["update-ref", ref, s.sha]);
    // Retired behind the journal's back, then the path goes: the next ordinary seed's compaction drops A's entry.
    sh(s.bare, ["config", "--local", "--add", `uzi-attempts.${s.branch}.entry`,
      JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "retired" })]);
    sh(s.bare, ["config", "--local", `uzi-recovery.${s.branch}.clone`, ""]);
    fs.rmSync(s.clonePath, { recursive: true, force: true });
    const asked = stateCalls().length;
    const next = factoryFor();
    await run(next.factory).execute(gitlabClaim(9108, { claim_generation: RESTART_GENERATION + 1, run_id: s.runId } as never));
    assert.equal(next.started(), 1);
    assert.equal(freshStage(s), undefined, "the journal of a gone attempt is cleared");
    assert.throws(() => sh(s.bare, ["rev-parse", "--verify", "-q", ref]));
    assert.equal(stateCalls().length, asked);
  });

  it("a kill between link and rename inside the L4 rewrite: the stale config.lock is never removed and never retried; the stop takes the hold", async () => {
    const s = await scene(9106, "L2");
    client.protocolFeatures = [PARK_FEATURE];
    ackParks(true);
    script(seededReply(s));
    const lock = path.join(s.bare, "config.lock");
    fs.copyFileSync(path.join(s.bare, "config"), lock);
    const lockBytes = fs.readFileSync(lock);
    const before = bareConfigBytes(s);
    const { factory, started } = factoryFor();
    const { logger, lines } = recordingLogger();
    await run(factory, {}, logger).execute(s.claim);
    assert.equal(started(), 0);
    assert.ok(fs.readFileSync(lock).equals(lockBytes), "the lock is untouched");
    assert.equal(bareConfigBytes(s), before, "config is unchanged: L4 never landed");
    assert.equal(freshStage(s), "seeding");
    assert.equal(stateCalls().length, 1, "never retried");
    const park = parks(s.runId);
    assert.equal(park.length, 1, "the hold was requested through the owner park");
    assert.equal(park[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
    assert.ok(lines.some(l => (l as { msg: string }).msg === "fresh resume owner hold confirmed"));
    assert.equal(reported(s.runId).includes("failed"), false);
  });
});

describe("the restart matrix", () => {
  it("positive without current authority: no L4, no model start, not failed", async () => {
    const s = await scene(9111, "L2");
    script(seededReply(s, { current_authority: false }));
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(freshStage(s), "seeding");
    assert.deepEqual(recoveryJournal(s), []);
    assert.equal(ledgerState(s), "fresh_pending");
    assert.equal(reported(s.runId).includes("failed"), false);
  });

  it("a cancelled transfer that was never seeded is negative; one that records a seed is not", async () => {
    const neg = await scene(9112, "L1");
    script(terminalReply("cancelled"));
    await run(factoryFor().factory).execute(neg.claim);
    assert.equal(ledgerState(neg), "fresh_unbound");
  });

  it("a terminal row that carries a seed stops and never unbinds", async () => {
    const s = await scene(9113, "L1");
    script(terminalReply("cancelled", "2026-01-01T00:00:00Z"));
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(ledgerState(s), "fresh_pending");
  });

  const stops: Array<[string, () => { status: number; body: unknown } | undefined, (s: Scene) => { status: number; body: unknown } | undefined]> = [
    ["a seeded row naming a different attempt", () => undefined, s => seededReply(s, { destination_attempt_id: mintAttemptId(SEED_GENERATION) })],
    ["a seeded row at a different generation", () => undefined, s => seededReply(s, { seeded_generation: SEED_GENERATION + 1 })],
    ["a missing record (404)", () => undefined, () => undefined],
    ["an unavailable reply (500)", () => undefined, () => ({ status: 500, body: { error: "boom" } })],
    ["an ambiguous reply (no current_authority)", () => undefined, () => ({ status: 200, body: { state: "seeded" } })],
    ["a state this worker does not know", () => undefined, () => ({ status: 200, body: { state: "pending_review", seeded_at: null, destination_attempt_id: null,
      seeded_generation: null, current_authority: true } })],
  ];
  stops.forEach(([name, , answer], i) => {
    it(`${name}: stops nonterminally, keeps A pending, never fresh_unbound`, async () => {
      const s = await scene(9120 + i, "L2");
      script(answer(s));
      const { factory, started } = factoryFor();
      await run(factory).execute(s.claim);
      assert.equal(started(), 0);
      assert.equal(ledgerState(s), "fresh_pending");
      assert.equal(freshStage(s), "seeding");
      assert.deepEqual(recoveryJournal(s), []);
      assert.equal(reported(s.runId).includes("failed"), false);
      assert.equal(api.freshResumeCalls.some(c => c.route === "abandon"), false, "an ordinary claim never abandons");
    });
  });

  it("the journal changing between the question and the action stops without a write", async () => {
    const s = await scene(9130, "L2");
    api.freshResume = { state: () => {
      sh(s.bare, ["config", "--local", freshJournalKey(s.branch), freshJournalJson({ frid: randomUUID(), attemptId: s.attemptId, generation: SEED_GENERATION, sha: s.sha, stage: "seeding" })]);
      return seededReply(s);
    } };
    const { factory, started } = factoryFor();
    await run(factory).execute(s.claim);
    assert.equal(started(), 0);
    assert.equal(freshStage(s), "seeding");
    assert.deepEqual(recoveryJournal(s), []);
  });

  it("the journal changing under the lock of the unbind (after the runner's own recheck) stops, never unbinds or continues", async () => {
    const s = await scene(9131, "L1");
    script(terminalReply("refused"));
    const real = git.markFreshUnbound.bind(git);
    const patched = mock.method(git, "markFreshUnbound", async (...args: Parameters<typeof real>) => {
      // After the runner's `again` read and before the locked recheck: another journal for the same attempt appears.
      sh(s.bare, ["config", "--local", freshJournalKey(s.branch), freshJournalJson({ frid: randomUUID(), attemptId: s.attemptId, generation: SEED_GENERATION, sha: s.sha, stage: "seeding" })]);
      return real(...args);
    });
    try {
      const { factory, started } = factoryFor();
      await run(factory).execute(s.claim);
      assert.equal(patched.mock.callCount(), 1);
      assert.equal(started(), 0, "the claim stopped instead of going on to start the model");
      assert.equal(ledgerState(s), "fresh_pending", "A was not unbound");
      assert.equal(reported(s.runId).includes("failed"), false);
    } finally { patched.mock.restore(); }
  });

  describe("the source's slot after a restart", () => {
    it("a ready journal with a foreign-active slot takes the ordinary restore route, not the fresh hold", async () => {
      const s = await scene(9144, "L2");
      await git.commitFreshReceipt({ barePath: s.bare, branch: s.branch, runId: s.runId, clonePath: s.clonePath, attemptId: s.attemptId,
        frid: s.frid, generation: SEED_GENERATION, sha: s.sha });
      assert.equal(freshStage(s), "ready");
      client.protocolFeatures = [PARK_FEATURE, "active_run_snapshot"];
      ackParks(true);
      const { logger, lines } = recordingLogger();
      const { factory, started } = factoryFor();
      await run(factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) }, logger).execute(s.claim);
      const msgs = lines.map(l => (l as { msg: string }).msg);
      assert.ok(msgs.includes("retained predecessor is foreign-owned; taking the independent restore route"), "the #3644 restore route was taken");
      assert.equal(msgs.some(m => /^fresh resume (source retained|owner hold)/.test(m)), false, "no fresh hold");
      assert.equal(stateCalls().length, 0, "a ready journal asks the server nothing");
      assert.equal(started(), 0);
      assert.equal(freshStage(s), "ready");
    });

    it("ended (the slot is ineligible): no hold, L4 commits", async () => {
      const s = await scene(9140, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      script(seededReply(s));
      await run(factoryFor().factory, { authority: stubAuthority(slotOf("self-incarnation", "ineligible")) }).execute(s.claim);
      assert.equal(parks(s.runId).length, 0);
      assert.equal(freshStage(s), "ready");
    });

    it("kept (the end of source use was declined): the hold is requested, L4 does not run", async () => {
      const s = await scene(9141, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      ackParks(true);
      script(seededReply(s));
      const { factory, started } = factoryFor();
      await run(factory, { authority: stubAuthority(slotOf("self-incarnation", "active"), { declined: true }) }).execute(s.claim);
      assert.equal(started(), 0);
      assert.equal(parks(s.runId).length, 1);
      assert.equal(parks(s.runId)[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
      assert.equal(freshStage(s), "seeding");
      assert.equal(reported(s.runId).includes("failed"), false);
    });

    it("a hard restart (the slot is active under another incarnation): the hold, never the restore route", async () => {
      const s = await scene(9142, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      ackParks(true);
      script(seededReply(s));
      const { factory, started } = factoryFor();
      await run(factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) }).execute(s.claim);
      assert.equal(started(), 0);
      assert.equal(parks(s.runId).length, 1);
      assert.equal(freshStage(s), "seeding");
    });

    it("a pending footprint of another incarnation counts as foreign-active", async () => {
      const s = await scene(9143, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      ackParks(true);
      script(seededReply(s));
      await run(factoryFor().factory, { authority: stubAuthority(slotOf("old-incarnation", "ineligible", { status: "running" })) }).execute(s.claim);
      assert.equal(parks(s.runId).length, 1);
    });
  });

  describe("the owner hold counts only when its park is applied", () => {
    const held = (lines: unknown[]) => lines.some(l => (l as { msg: string }).msg === "fresh resume owner hold confirmed");
    const unheld = (lines: unknown[]) => lines.some(l => (l as { msg: string }).msg === "fresh resume owner hold unconfirmed; stopped with custody kept");

    it("an applied park is a confirmed hold", async () => {
      const s = await scene(9150, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      ackParks(true);
      script(seededReply(s));
      const { logger, lines } = recordingLogger();
      await run(factoryFor().factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) }, logger).execute(s.claim);
      assert.equal(held(lines), true);
      assert.equal(unheld(lines), false);
    });

    it("a park the api does not confirm is a nonterminal stop with custody retained, not a hold", async () => {
      const s = await scene(9151, "L2");
      client.protocolFeatures = [PARK_FEATURE];
      ackParks(false);
      script(seededReply(s));
      const { logger, lines } = recordingLogger();
      await run(factoryFor().factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) }, logger).execute(s.claim);
      assert.equal(held(lines), false);
      assert.equal(unheld(lines), true);
      assert.equal(reported(s.runId).includes("failed"), false);
      assert.equal(fs.existsSync(s.clonePath), true, "the seeded clone is kept");
    });

    it("an api that does not advertise the cause is asked for no hold at all", async () => {
      const s = await scene(9152, "L2");
      client.protocolFeatures = [];
      script(seededReply(s));
      const { logger, lines } = recordingLogger();
      await run(factoryFor().factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) }, logger).execute(s.claim);
      assert.equal(parks(s.runId).length, 0);
      assert.equal(held(lines), false);
      assert.equal(unheld(lines), true);
    });
  });
});

describe("F1: the proofs the destination must never skip", () => {
  it("a differing A and a missing receipt both stop; neither starts the model or mints an id", async () => {
    const differing = await scene(9161, "L2");
    script(seededReply(differing, { destination_attempt_id: mintAttemptId(SEED_GENERATION) }));
    const a = factoryFor();
    await run(a.factory).execute(differing.claim);
    assert.equal(a.started(), 0);
    const ids = (s: Scene): string[] => cfg(s, `uzi-attempts.${s.branch}.entry`).map(v => parseAttemptLedgerEntry(v)!.attemptId);
    assert.deepEqual(ids(differing), [differing.attemptId], "no other attempt id was minted");

    const missing = await scene(9162, "L2");
    script({ status: 404, body: { error: "fresh resume transfer not found", reason: "fresh_resume_not_found" } });
    const b = factoryFor();
    await run(b.factory).execute(missing.claim);
    assert.equal(b.started(), 0);
    assert.deepEqual(ids(missing), [missing.attemptId]);
  });

  it("a pre-seed refusal marks A fresh_unbound, and without authority neither L4 nor the model happens", async () => {
    const refused = await scene(9163, "L1");
    script(terminalReply("refused"));
    await run(factoryFor().factory).execute(refused.claim);
    assert.equal(ledgerState(refused), "fresh_unbound");

    const noAuthority = await scene(9164, "L2");
    script(seededReply(noAuthority, { current_authority: false }));
    const f = factoryFor();
    await run(f.factory).execute(noAuthority.claim);
    assert.equal(f.started(), 0);
    assert.deepEqual(recoveryJournal(noAuthority), []);
    assert.equal(freshStage(noAuthority), "seeding");
  });
});

describe("a park refused before the receipt", () => {
  it("answered 409 fresh_resume_pre_seed: the transfer is abandoned, the stop is nonterminal and nothing is reported failed", async () => {
    simulateCommittedWork();
    const sha = publishCheckpoint();
    const frid = randomUUID();
    const claim = gitlabClaim(9171, { claim_generation: SEED_GENERATION,
      fresh_resume: { id: frid, kind: "checkpoint_ref", checkpoint_ref: CHECKPOINT_REF, sha, evidence_generation: 1 } });
    // The clone fails transiently before any seed, so the flight parks, and the server (which holds the transfer claimed) refuses it.
    mock.method(git, "ensureClone", async () => { throw new Error("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com"); });
    api.stateAckWire = body => body.status === "recovery_wait" || body.status === "failed"
      ? { status: 409, body: { error: "refused", reason: "fresh_resume_pre_seed", applied: false } } : undefined;
    const { factory, started } = factoryFor();
    await run(factory).execute(claim);
    assert.equal(started(), 0);
    const abandons = api.freshResumeCalls.filter(c => c.route === "abandon");
    assert.equal(abandons.length, 1, "the refused park abandoned the transfer");
    assert.equal(abandons[0]!.frid, frid);
    assert.equal(abandons[0]!.body.claim_generation, SEED_GENERATION);
    assert.equal(api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed").length, 0, "no failed report reached the api");
  });
});

describe("a park answered as not applied before the receipt", () => {
  it("a 409 that only says the run moved on (applied false, no pre-seed reason) abandons the transfer through the report path itself", async () => {
    simulateCommittedWork();
    const sha = publishCheckpoint();
    const frid = randomUUID();
    const claim = gitlabClaim(9172, { claim_generation: SEED_GENERATION,
      fresh_resume: { id: frid, kind: "checkpoint_ref", checkpoint_ref: CHECKPOINT_REF, sha, evidence_generation: 1 } });
    api.stateAckWire = body => body.status === "recovery_wait"
      ? { status: 409, body: { applied: false, run: { id: claim.run_id, status: "running", hold_reason: null } } } : undefined;
    const { logger, lines } = recordingLogger();
    const runner = run(factoryFor().factory, {}, logger);
    const priv = runner as unknown as Record<string, (...a: unknown[]) => unknown>;
    // A park is sent inside the pre-receipt window, where the server holds the transfer claimed.
    mock.method(priv, "seedFreshResume", async (...args: unknown[]) => {
      const flight = args[1] as { reportState(body: { status: string }): Promise<unknown> };
      await flight.reportState({ status: "recovery_wait" });
    });
    await runner.execute(claim);
    const abandons = api.freshResumeCalls.filter(c => c.route === "abandon");
    assert.equal(abandons.length, 1, "abandoned once, by the unapplied answer");
    assert.equal(abandons[0]!.frid, frid);
    const logged = lines.find(l => (l as { msg?: string }).msg === "fresh resume transfer abandoned") as { reason?: string } | undefined;
    assert.equal(logged?.reason, "report_not_applied");
    assert.equal(api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed").length, 0, "no failed report reached the api");
  });
});

describe("a batcher breaker trip never reports failed from the fresh windows", () => {
  /** Arms a permanent 403 for the next message send, queues a feed line and flushes it: the breaker must trip (positive witness). */
  async function trip(flight: { batcher: { emit(msg: unknown): void; flushOwned(signal: AbortSignal): Promise<boolean>; awaitPermanentFailureSettled(): Promise<void> } }): Promise<void> {
    api.failMessagesNext(1, 403);
    flight.batcher.emit({ kind: "status", agent: "worker", payload: { text: "transport trip witness" } });
    assert.equal(await flight.batcher.flushOwned(new AbortController().signal), false, "the permanent failure refused the flush");
    await flight.batcher.awaitPermanentFailureSettled();
  }
  type FlightLike = Parameters<typeof trip>[0] & { freshResumeTransportStopped?: boolean; freshResumeHold?: boolean };

  it("in the pre-seed window: no failed report, the sticky flag is set and the flight stops without a model", async () => {
    simulateCommittedWork();
    const sha = publishCheckpoint();
    const frid = randomUUID();
    const claim = gitlabClaim(9181, { claim_generation: SEED_GENERATION,
      fresh_resume: { id: frid, kind: "checkpoint_ref", checkpoint_ref: CHECKPOINT_REF, sha, evidence_generation: 1 } });
    const runner = run(factoryFor().factory);
    const entries: Array<{ stage: string | undefined; hold: boolean | undefined }> = [];
    const priv = runner as unknown as Record<string, (...a: unknown[]) => unknown>;
    const handler = priv.handlePermanentFailure!.bind(runner);
    mock.method(priv, "handlePermanentFailure", (...args: unknown[]) => {
      const f = args[1] as { freshResume?: { stage: string }; freshResumeHold?: boolean };
      entries.push({ stage: f.freshResume?.stage, hold: f.freshResumeHold });
      return handler(...args);
    });
    let flight: FlightLike | undefined;
    const seedInWindow = priv.seedFreshResume!.bind(runner);
    mock.method(priv, "seedFreshResume", async (...args: unknown[]) => {
      flight = args[1] as FlightLike;
      await trip(flight);
      return seedInWindow(...args);
    });
    const { factory, started } = factoryFor();
    void factory;
    await runner.execute(claim);
    assert.equal(started(), 0);
    assert.deepEqual(entries.map(e => e.stage), ["pre_receipt"], "the breaker entered handlePermanentFailure inside the window");
    assert.equal(flight?.freshResumeTransportStopped, true);
    assert.equal(api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed").length, 0);
  });

  it("during the confirmed fresh hold: no failed report, the flag is recorded and the hold's own park still goes out", async () => {
    const s = await scene(9182, "L2");
    client.protocolFeatures = [PARK_FEATURE];
    ackParks(true);
    script(seededReply(s));
    const runner = run(factoryFor().factory, { authority: stubAuthority(slotOf("old-incarnation", "active")) });
    const priv = runner as unknown as Record<string, (...a: unknown[]) => unknown>;
    let flight: FlightLike | undefined;
    let inHold = false;
    const park = priv.parkRetainedRecoveryExhausted!.bind(runner);
    mock.method(priv, "parkRetainedRecoveryExhausted", async (...args: unknown[]) => {
      flight = args[0] as FlightLike;
      inHold = flight.freshResumeHold === true;
      await trip(flight);
      return park(...args);
    });
    await runner.execute(s.claim);
    assert.equal(inHold, true, "the park ran under the fresh-hold flag");
    assert.equal(flight?.freshResumeTransportStopped, true);
    assert.equal(parks(s.runId).length, 1, "the hold's park was not aborted by the trip");
    assert.equal(api.states.filter(st => st.runId === s.runId && st.body.status === "failed").length, 0);
  });
});
