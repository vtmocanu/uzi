// Issue #4481 m2: a failed issue run whose owed heads reach the per-run cap releases custody through
// the held V2 publication and FINAL when the durable refusal gate proves completeness, and keeps custody
// (no publishHeld, no FINAL) whenever it cannot. Every case drives the real runner terminal hook.
import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Executor, RunContext } from "../src/executor.js";
import { randomUUID } from "node:crypto";
import type { RefusalReleaseGate } from "../src/git.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, restartGitCache, runnerWith, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, PUBLICATION_ID, bare, command, commit, failedStates, failingWork,
  deferTerminalEvidence, frozenRecords, heldServer, packObjects, redrive, skipCheckpoints, type HeldServer, type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

const CAP = 2;
type Claim = ReturnType<typeof gitlabClaim>;
const GENERATION = 1;
const opts = { skip: HELD_LINUX_ONLY } as const;

const refusalDir = (): string => path.join(bare(), "info", "uzi-refusals");
const refusalFiles = (): string[] => fs.existsSync(refusalDir()) ? fs.readdirSync(refusalDir()).sort() : [];
const ledgerPath = (claim: Claim): string => path.join(refusalDir(), `refusals-${claim.run_id}.json`);
const originMain = (): string => command(bare(), ["rev-parse", "refs/remotes/origin/main"]);

function setup(iid: number, cap: number | undefined = CAP): { claim: Claim; server: HeldServer; work: Partial<Work> } {
  if (cap !== undefined) Reflect.set(git, "maxOwedCandidates", cap);
  const claim = gitlabClaim(iid, { claim_generation: GENERATION, inventory_guarded: true });
  const server = heldServer(claim);
  skipCheckpoints();
  return { claim, server, work: {} };
}

/** An import of the clone's current head straight through GitCache, as a refused cap import would run. */
function directFetch(claim: Claim, clone: string): ReturnType<typeof git.fetchAgentBranch> {
  const branch = command(clone, ["rev-parse", "--abbrev-ref", "HEAD"]);
  return git.fetchAgentBranch(bare(), clone, branch, claim.run_id, { context: {
    runId: claim.run_id, generation: GENERATION, branch, kind: "issue", barePath: bare(),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: originMain() },
  } });
}

/** An earlier head the cap refuses (the run is then at the cap). */
async function refuseEarlier(claim: Claim, ctx: RunContext, name: string): Promise<string> {
  const sha = commit(ctx.worktreePath, name);
  const result = await directFetch(claim, ctx.worktreePath);
  assert.deepEqual(result, { kind: "not_updated", reason: "owed_limit", refusedSha: sha }, "the earlier import is refused at the cap");
  return sha;
}

/** A refusal stage no ledger entry accounts for: evidence the gate cannot attribute. */
function injectUnledgeredStage(claim: Claim): string {
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@t", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@t" };
  const tree = command(bare(), ["rev-parse", `${originMain()}^{tree}`]);
  const sha = execFileSync("git", ["-C", bare(), "commit-tree", tree, "-m", "injected " + Date.now()], { encoding: "utf8", env }).trim();
  command(bare(), ["update-ref", `refs/uzi-incoming/${claim.run_id}/${GENERATION}/refused-${sha}`, sha]);
  return sha;
}

/** A stage on an already ledgered head: the gate stays eligible but the evidence digest changes. */
function injectCoveredStage(claim: Claim): void {
  const ledger = JSON.parse(fs.readFileSync(ledgerPath(claim), "utf8")) as { entries: { sha: string }[] };
  const sha = ledger.entries[ledger.entries.length - 1]!.sha;
  command(bare(), ["update-ref", `refs/uzi-incoming/${claim.run_id}/${GENERATION}/refused-${sha}-extra`, sha]);
}

function noHold(server: HeldServer, label: string): void {
  assert.equal(server.heldCalls.length, 0, `${label}: publishHeld was never called`);
  assert.equal(server.finals.length, 0, `${label}: no FINAL was sent`);
  assert.equal(server.acked(), 0, `${label}: nothing was released`);
  assert.equal(server.holdOpen(), true, `${label}: the guarded hold stays open`);
}

async function runFailed(claim: Claim, work: Partial<Work>, extra?: (ctx: RunContext) => Promise<void> | void,
  onRunner?: (r: ReturnType<typeof runner>) => void) {
  const r = runner(failingWork(work, extra), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  onRunner?.(r);
  await r.execute(claim);
  assert.equal(failedStates(claim.run_id).length, 1, "the honest failed report is sent once");
  return r;
}

/** A restarted worker over the same stores replays the journaled FINAL (the same GitCache keeps its process nonce). */
async function restartReplay(): Promise<void> {
  const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
  await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
}

/** Records what each terminal transfer pass returned for the run. */
function spyTransfer(t: TestContext, r: ReturnType<typeof runner>): (string | null)[] {
  const results: (string | null)[] = [];
  const original = Reflect.get(r, "transferRestorePointToTrustedBare") as (...a: unknown[]) => Promise<string | null>;
  t.mock.method(r as unknown as Record<string, (...a: unknown[]) => Promise<string | null>>, "transferRestorePointToTrustedBare",
    async function (this: unknown, ...a: unknown[]) { const out = await original.apply(this, a); results.push(out); return out; });
  return results;
}

const stackOf = (): string => new Error().stack ?? "";
/** Calls `inject` once, on the first hold read that follows a coverage proof's refusal gate. */
function injectAfterCoverageProof(armed: () => boolean, inject: () => void): () => boolean {
  let lastWasCoverage = false, fired = false;
  const gate = git.refusalReleaseGate.bind(git);
  git.refusalReleaseGate = async (...a: Parameters<typeof gate>) => {
    lastWasCoverage = /frozenCoverageProven/.test(stackOf());
    return gate(...a);
  };
  const list = client.listRecoveryHolds.bind(client);
  client.listRecoveryHolds = async (...a: Parameters<typeof list>) => {
    if (!fired && armed() && lastWasCoverage && /heldOperationState|openInventoryHold/.test(stackOf())) { fired = true; inject(); }
    return list(...a);
  };
  return () => fired;
}

const refusedHeld = (call: { run: string; generation: number; tip: string }) => ({ ok: true as const, body: {
  publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${call.run}/${call.generation}`, tip: call.tip,
  state: "refused" as const, reason: "create_refused" } });

it("a cap-stopped failed run releases through the held V2 publication and FINAL", opts, async () => {
  const { claim, server, work } = setup(44810);
  const r = await runFailed(claim, work);
  assert.ok(refusalFiles().some(n => n.startsWith("admission-")), "the flight admitted its generation before the first import");
  assert.ok(refusalFiles().some(n => n.startsWith("refusals-")), "the cap refusal was recorded durably");
  assert.equal(server.heldCalls.length, 1, "publishHeld ran at the cap");
  assert.equal(server.finals.length, 0, "no FINAL from inside the flight");
  const [record] = await frozenRecords(claim);
  assert.equal(record!.finalRequest?.disposition.kind, "held_publication");
  assert.equal(server.heldCalls[0]!.tip, record!.sourceSha);
  assert.ok(packObjects(server.heldCalls[0]!.pack).has(command(work.clone!, ["rev-parse", "HEAD"])), "the pack carries the clone head, WIP commit included");
  // The terminal transfer closed the generation: a later import refuses before it writes anything.
  const before = command(bare(), ["for-each-ref", "refs/"]);
  assert.equal((await directFetch(claim, work.clone!)).kind, "not_updated", "the fence refuses a later import");
  assert.equal(command(bare(), ["for-each-ref", "refs/"]), before, "the fence refused before any ref write");
  await redrive(r);
  assert.equal(server.finals.length, 1);
  assert.equal(server.finals[0]!.disposition?.kind, "held_publication");
  assert.equal(server.acked(), 1);
  assert.equal(server.holdOpen(), false);
  assert.equal(server.archiveReserves(), 0, "no archive was captured");
  assert.ok(refusalFiles().includes(`admission-${claim.run_id}-1.json`), "the acked generation's admission is kept");
});

it("a below-cap failed run still sends its FINAL and writes no seal", opts, async () => {
  const { claim, server, work } = setup(44811, 64);
  const r = await runFailed(claim, work);
  await redrive(r);
  assert.equal(server.heldCalls.length, 1);
  assert.equal(server.finals.length, 1);
  assert.equal(server.acked(), 1);
  assert.deepEqual(refusalFiles().filter(n => n.startsWith("final-") || n.startsWith("refusals-")), [], "no refusal ledger and no seal");
});

it("an earlier refused head that the final head covers still releases", opts, async () => {
  const { claim, server, work } = setup(44812);
  const r = await runFailed(claim, work, ctx => refuseEarlier(claim, ctx, "earlier.txt").then(() => undefined));
  await redrive(r);
  assert.equal(server.heldCalls.length, 1);
  assert.equal(server.acked(), 1);
});

it("a divergent earlier refused head keeps custody", opts, async () => {
  const { claim, server, work } = setup(44813);
  const r = await runFailed(claim, work, async ctx => {
    const base = command(ctx.worktreePath, ["rev-parse", "HEAD~"]);
    await refuseEarlier(claim, ctx, "earlier.txt");
    command(ctx.worktreePath, ["reset", "--hard", base]);
    commit(ctx.worktreePath, "divergent.txt");
  });
  await redrive(r);
  noHold(server, "divergent earlier head");
  assert.equal(server.archiveReserves(), 0);
});

it("a failed first ledger write then a new process then a divergent fetch keeps custody", opts, async t => {
  const { claim, server, work } = setup(44814);
  const r = await runFailed(claim, work, async ctx => {
    const write = Reflect.get(git, "writeOwedFile") as (...a: unknown[]) => Promise<unknown>;
    const failing = t.mock.method(git as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>, "writeOwedFile",
      function (this: unknown, ...a: unknown[]) {
        return typeof a[1] === "string" && a[1].startsWith("refusals-") ? Promise.reject(new Error("ledger write failed")) : write.apply(this, a);
      });
    const base = command(ctx.worktreePath, ["rev-parse", "HEAD~"]);
    await refuseEarlier(claim, ctx, "earlier.txt");
    assert.ok(failing.mock.callCount() >= 1, "the first ledger write failed");
    failing.mock.restore();
    Reflect.set(git, "processNonce", "restarted-worker-process");
    command(ctx.worktreePath, ["reset", "--hard", base]);
    commit(ctx.worktreePath, "divergent.txt");
  });
  await redrive(r);
  noHold(server, "failed first ledger write");
});

it("death before the ledger write (stage kept, ledger gone) keeps custody", opts, async () => {
  const { claim, server, work } = setup(44815);
  const r = await runFailed(claim, work, async ctx => {
    await refuseEarlier(claim, ctx, "earlier.txt");
    fs.rmSync(ledgerPath(claim));
    commit(ctx.worktreePath, "after-earlier.txt");
  });
  await redrive(r);
  noHold(server, "stage without ledger");
});

it("an old worker's refusal (admission never written) keeps custody", opts, async t => {
  const { claim, server, work } = setup(44816);
  t.mock.method(git, "admitRefusalProvenance", async () => "failed" as const);
  const r = await runFailed(claim, work);
  assert.deepEqual(refusalFiles().filter(n => n.startsWith("admission-")), []);
  await redrive(r);
  noHold(server, "no admission");
});

it("evidence appearing between the terminal transfer and the freeze keeps custody before any freeze", opts, async t => {
  const { claim, server, work } = setup(44817);
  const retain = git.retainCurrentOwedCandidate.bind(git);
  t.mock.method(git, "retainCurrentOwedCandidate", async (...a: Parameters<typeof retain>) => {
    injectUnledgeredStage(claim);
    return retain(...a);
  });
  const r = await runFailed(claim, work);
  assert.equal((await frozenRecords(claim)).length, 0, "the gate refused before the freeze");
  await redrive(r);
  noHold(server, "pre-freeze injection");
  assert.ok(fs.existsSync(work.clone!), "the clone is retained");
});

it("evidence appearing before the publish closure keeps custody without publishHeld", opts, async t => {
  const { claim, server, work } = setup(44818);
  const send = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...a: Parameters<typeof send>) => {
    const ack = await send(...a);
    if (a[1].status === "failed") injectUnledgeredStage(claim);
    return ack;
  });
  const r = await runFailed(claim, work);
  assert.equal((await frozenRecords(claim)).length, 1, "the freeze preceded the evidence");
  await redrive(r);
  noHold(server, "pre-publish injection");
});

it("evidence appearing after the held create and before the FINAL sends no FINAL", opts, async () => {
  const { claim, server, work } = setup(44819);
  server.onHeld = () => { injectUnledgeredStage(claim); };
  const r = await runFailed(claim, work);
  await redrive(r);
  assert.equal(server.heldCalls.length, 1, "the held create preceded the evidence");
  assert.equal(server.finals.length, 0, "the seal refused the FINAL");
  assert.equal(server.acked(), 0);
  assert.equal(server.holdOpen(), true);
  assert.ok(!refusalFiles().some(n => n.startsWith("final-")), "nothing was sealed");
});

it("evidence appearing after the archive upload and before the archive FINAL sends no FINAL", opts, async t => {
  const { claim, server, work } = setup(44820);
  server.heldAnswer = refusedHeld;
  const upload = client.uploadRecoveryBundle.bind(client);
  client.uploadRecoveryBundle = async (...a: Parameters<typeof upload>) => {
    const out = await upload(...a);
    injectUnledgeredStage(claim);
    return out;
  };
  const r = await runFailed(claim, work, undefined, runner => deferTerminalEvidence(runner, t));
  await redrive(r);
  assert.ok(server.archiveUploads() >= 1, "the archive fallback uploaded");
  assert.equal(server.finals.length, 0, "the seal refused the archive FINAL");
  assert.equal(server.holdOpen(), true);
});

it("a restart finds the gate ineligible before capture and upload", opts, async () => {
  const { claim, server, work } = setup(44821);
  server.heldAnswer = refusedHeld;
  const upload = client.uploadRecoveryBundle.bind(client);
  let failUpload = true;
  client.uploadRecoveryBundle = async (...a: Parameters<typeof upload>) => {
    if (failUpload) throw new Error("upload unavailable");
    return upload(...a);
  };
  await runFailed(claim, work);
  assert.equal(server.archiveUploads(), 0);
  assert.equal(server.finals.length, 0);
  failUpload = false;
  injectUnledgeredStage(claim);
  await restartReplay();
  assert.equal(server.archiveUploads(), 0, "no capture or upload while the gate is ineligible");
  assert.equal(server.finals.length, 0);
  assert.equal(server.holdOpen(), true);
});

it("a lost FINAL ACK replays under the same seal", opts, async () => {
  const { claim, server, work } = setup(44822);
  let first = true;
  server.finalAnswer = call => {
    if (first) { first = false; return new Error("ACK lost"); }
    return { run_id: claim.run_id, generation: call.generation!, released: true, holds_released: 1 };
  };
  const r = await runFailed(claim, work);
  await redrive(r, 1);
  assert.equal(server.finals.length, 1);
  assert.ok(refusalFiles().some(n => n.startsWith("final-")), "the first send wrote the seal");
  await restartReplay();
  assert.equal(server.finals.length, 2, "the unpersisted ACK replayed");
  assert.deepEqual(server.finals[1]!.disposition, server.finals[0]!.disposition);
  assert.equal(server.acked(), 1);
});

it("a replay after an unpersisted ACK with changed evidence sends no FINAL", opts, async () => {
  const { claim, server, work } = setup(44823);
  server.finalAnswer = () => new Error("ACK lost");
  const r = await runFailed(claim, work);
  await redrive(r, 1);
  assert.equal(server.finals.length, 1, "the first FINAL carried the seal");
  assert.ok(refusalFiles().some(n => n.startsWith("final-")));
  injectCoveredStage(claim);
  await restartReplay();
  await redrive(r, 2);
  assert.equal(server.finals.length, 1, "the changed evidence digest refused every replay");
  assert.equal(server.acked(), 0);
  assert.equal(server.holdOpen(), true);
});

it("a replay by another process keeps custody", opts, async () => {
  const { claim, server, work } = setup(44824);
  server.finalAnswer = () => new Error("ACK lost");
  const r = await runFailed(claim, work);
  await redrive(r, 1);
  assert.equal(server.finals.length, 1);
  Reflect.set(git, "processNonce", "another-worker-process");
  await restartReplay();
  await redrive(r, 2);
  assert.equal(server.finals.length, 1, "no FINAL from a process that did not admit the generation");
  assert.equal(server.acked(), 0);
  assert.equal(server.holdOpen(), true);
});

it("a cap run whose terminal hook runs twice releases through the first pass's head", opts, async t => {
  const { claim, server, work } = setup(44825);
  let results: (string | null)[] = [];
  const r = await runFailed(claim, work, undefined, runner => { results = spyTransfer(t, runner); });
  assert.ok(results.length >= 2, "the terminal hook ran more than once");
  assert.ok(results.every(h => h !== null && h === results[0]), "every pass returned the same released head");
  await redrive(r);
  assert.equal(server.acked(), 1);
});

it("a below-cap run's second terminal pass re-imports exactly as before", opts, async t => {
  const { claim, work } = setup(44826, 64);
  let results: (string | null)[] = [];
  await runFailed(claim, work, undefined, runner => { results = spyTransfer(t, runner); });
  assert.ok(results.length >= 2, "the terminal hook ran more than once");
  assert.ok(results.every(h => h !== null), "no pass was fenced out");
  assert.equal((await directFetch(claim, work.clone!)).kind, "updated", "the generation was never fenced");
});

it("a below-cap release still sends its FINAL with 4200 admission files present, and keeps its own", opts, async () => {
  const { claim, server, work } = setup(44827, 64);
  const r = await runFailed(claim, work, () => {
    fs.mkdirSync(refusalDir(), { recursive: true, mode: 0o700 });
    for (let i = 0; i < 4200; i++) {
      fs.writeFileSync(path.join(refusalDir(), `admission-${String(i).padStart(8, "0")}-aaaa-bbbb-cccc-444444444444-1.json`), "{}\n", { mode: 0o600 });
    }
  });
  await redrive(r);
  assert.equal(server.finals.length, 1);
  assert.equal(server.acked(), 1);
  assert.ok(refusalFiles().includes(`admission-${claim.run_id}-1.json`), "this run's admission is kept after the release");
  assert.ok(refusalFiles().filter(n => n.startsWith("admission-")).length >= 4200, "other runs' admissions stay");
});

it("an unguarded flight writes no admission", opts, async () => {
  Reflect.set(git, "maxOwedCandidates", 64);
  const claim = gitlabClaim(44828, { claim_generation: GENERATION });
  skipCheckpoints();
  const r = runner(failingWork({}), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  await r.execute(claim);
  assert.deepEqual(refusalFiles().filter(n => n.startsWith("admission-")), []);
});

it("evidence between the last coverage proof and the held create is caught by the publish gate", opts, async () => {
  const { claim, server, work } = setup(44830);
  const fired = injectAfterCoverageProof(() => server.heldCalls.length === 0, () => { injectUnledgeredStage(claim); });
  const r = await runFailed(claim, work);
  await redrive(r);
  assert.ok(fired(), "the evidence arrived inside the window");
  noHold(server, "publish window");
});

it("evidence between the archive coverage proof and the archive FINAL is caught by the seal", opts, async t => {
  const { claim, server, work } = setup(44831);
  server.heldAnswer = refusedHeld;
  const upload = client.uploadRecoveryBundle.bind(client);
  let uploaded = false;
  client.uploadRecoveryBundle = async (...a: Parameters<typeof upload>) => { const out = await upload(...a); uploaded = true; return out; };
  const fired = injectAfterCoverageProof(() => uploaded, () => { injectUnledgeredStage(claim); });
  const r = await runFailed(claim, work, undefined, runner => deferTerminalEvidence(runner, t));
  await redrive(r);
  assert.ok(fired(), "the evidence arrived inside the window");
  assert.equal(server.finals.length, 0, "the seal refused the archive FINAL");
  assert.equal(server.holdOpen(), true);
});

it("evidence after the upload mints no archive FINAL request", opts, async t => {
  const { claim, server, work } = setup(44832);
  server.heldAnswer = refusedHeld;
  const upload = client.uploadRecoveryBundle.bind(client);
  client.uploadRecoveryBundle = async (...a: Parameters<typeof upload>) => { const out = await upload(...a); injectUnledgeredStage(claim); return out; };
  const r = await runFailed(claim, work, undefined, runner => deferTerminalEvidence(runner, t));
  await redrive(r);
  const records = await frozenRecords(claim);
  assert.ok(records.some(r => r.state === "uploaded"), "the archive was uploaded");
  assert.ok(records.every(r => r.finalRequest === undefined), "the coverage proof refused before any FINAL was journaled");
  assert.equal(server.finals.length, 0);
});

it("a clone head that moved after the refusal is not released", opts, async () => {
  const { claim, server, work } = setup(44833);
  const head = git.worktreeHead.bind(git);
  let moved = false;
  git.worktreeHead = async (p: string) => {
    if (!moved && /transferAtOwedCap/.test(stackOf())) {
      moved = true;
      commit(p, "moved-after-refusal.txt");
      command(bare(), ["fetch", "-q", "--no-tags", `file://${p}`, "+HEAD:refs/uzi-test/moved"]);
    }
    return head(p);
  };
  await runFailed(claim, work);
  assert.ok(moved, "the clone moved inside the transfer");
  assert.equal(server.heldCalls.length, 0, "the terminal transfer released nothing for a head it did not refuse");
});

it("control: with no evidence the archive fallback sends its archive FINAL", opts, async t => {
  const { claim, server, work } = setup(44834);
  server.heldAnswer = refusedHeld;
  const r = await runFailed(claim, work, undefined, runner => deferTerminalEvidence(runner, t));
  await redrive(r);
  assert.equal(server.finals.length, 1);
  assert.equal(server.finals[0]!.disposition?.kind, "archive");
  assert.equal(server.acked(), 1);
});

it("an unguarded cap-stopped run uploads and releases nothing", opts, async () => {
  Reflect.set(git, "maxOwedCandidates", CAP);
  const claim = gitlabClaim(44840, { claim_generation: GENERATION });
  const server = heldServer(claim);
  skipCheckpoints();
  const r = runner(failingWork({}), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  await r.execute(claim);
  await redrive(r);
  assert.equal(server.archiveUploads(), 0, "no archive was uploaded");
  assert.equal(server.finals.length, 0, "nothing was released");
  assert.equal(server.heldCalls.length, 0);
  assert.ok(command(bare(), ["for-each-ref", `refs/uzi-incoming/${claim.run_id}/`]).length > 0, "the refused stage is kept");
  assert.ok(refusalFiles().some(n => n.startsWith("refusals-")), "the ledger is kept");
});

it("a second cap pass re-proves the clone head before returning the first pass's head", opts, async t => {
  const { claim, server, work } = setup(44841);
  let results: (string | null)[] = [];
  let pass = 0;
  const head = git.worktreeHead.bind(git);
  git.worktreeHead = async (p: string) => {
    if (/transferAtOwedCap/.test(stackOf()) && ++pass === 2) commit(p, "moved-between-passes.txt");
    return head(p);
  };
  await runFailed(claim, work, undefined, runner => { results = spyTransfer(t, runner); });
  assert.ok(results.length >= 2, "the terminal hook ran twice");
  assert.notEqual(results[0], null);
  assert.equal(results[1], null, "the second pass did not return a head the clone no longer has");
  void server;
});

it("a second cap pass re-proves the gate before returning the first pass's head", opts, async t => {
  const { claim, work } = setup(44842);
  let results: (string | null)[] = [];
  let pass = 0;
  const gate = git.refusalReleaseGate.bind(git);
  git.refusalReleaseGate = async (...a: Parameters<typeof gate>) => {
    if (/transferAtOwedCap/.test(stackOf()) && ++pass === 2) injectUnledgeredStage(claim);
    return gate(...a);
  };
  await runFailed(claim, work, undefined, runner => { results = spyTransfer(t, runner); });
  assert.ok(results.length >= 2, "the terminal hook ran twice");
  assert.notEqual(results[0], null);
  assert.equal(results[1], null, "the second pass saw the new evidence");
});

it("after the cap transfer a re-import of a new head is refused by the fence, not recorded", opts, async () => {
  const { claim, work, server } = setup(44843);
  await runFailed(claim, work);
  const ledgerBefore = fs.readFileSync(ledgerPath(claim), "utf8");
  const refsBefore = command(bare(), ["for-each-ref", "refs/"]);
  const fresh = commit(work.clone!, "after-the-cap.txt");
  const result = await directFetch(claim, work.clone!);
  assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" }, "fenced, not owed_limit");
  assert.equal(fs.readFileSync(ledgerPath(claim), "utf8"), ledgerBefore, "no new ledger entry");
  assert.equal(command(bare(), ["for-each-ref", "refs/"]), refsBefore, "no new stage or ref");
  assert.ok(!ledgerBefore.includes(fresh));
  void server;
});

it("F4: a successful terminal re-pin that lands exactly at the cap closes the fence and the second pass re-proves", opts, async t => {
  const { claim, server, work } = setup(44830, 3);
  let results: (string | null)[] = [];
  const r = await runFailed(claim, work, undefined, runner => { results = spyTransfer(t, runner); });
  assert.equal(command(bare(), ["for-each-ref", "refs/uzi-owed/"]).split("\n").length, 3, "the terminal import succeeded and filled the cap");
  assert.ok(results.length >= 2 && results.every(h => h !== null && h === results[0]), "the second pass returned the first pass's head");
  await redrive(r);
  assert.equal(server.acked(), 1, "the provenance-gated release still completes");
  const refsBefore = command(bare(), ["for-each-ref", "refs/"]);
  commit(work.clone!, "after-the-successful-pin.txt");
  assert.deepEqual(await directFetch(claim, work.clone!), { kind: "not_updated", reason: "ownership_unknown" }, "fenced");
  assert.equal(command(bare(), ["for-each-ref", "refs/"]), refsBefore, "no stage was created");
});

it("S4: a successful terminal fetch at the cap with an ineligible gate keeps custody", opts, async t => {
  const { claim, server, work } = setup(44832, 3);
  let results: (string | null)[] = [];
  const r = await runFailed(claim, work, () => {
    fs.mkdirSync(refusalDir(), { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(refusalDir(), `incomplete-${claim.run_id}.json`), "{}\n", { mode: 0o600 });
  }, runner => { results = spyTransfer(t, runner); });
  assert.equal(command(bare(), ["for-each-ref", "refs/uzi-owed/"]).split("\n").length, 3, "the terminal import itself succeeded at the cap");
  assert.ok(results.length >= 1 && results.every(h => h === null), "no pass released a head");
  await redrive(r);
  noHold(server, "incomplete history after a successful fetch");
});

it("N1: an unguarded run at the cap with a successful terminal fetch follows the base path (no fence, no gate)", opts, async t => {
  Reflect.set(git, "maxOwedCandidates", 3);
  const claim = gitlabClaim(44833, { claim_generation: GENERATION });
  skipCheckpoints();
  const work: Partial<Work> = {};
  let results: (string | null)[] = [];
  const r = runner(failingWork(work), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  results = spyTransfer(t, r);
  await r.execute(claim);
  assert.equal(command(bare(), ["for-each-ref", "refs/uzi-owed/"]).split("\n").length, 3, "the terminal import succeeded at the cap");
  assert.ok(results.length >= 1 && results.every(h => h !== null), "the base path released the head");
  assert.equal(git["refusalGenerationClosed"]({ runId: claim.run_id, generation: GENERATION }), false, "the generation was never fenced");
  assert.deepEqual(refusalFiles().filter(n => n.startsWith("admission-")), []);
});

/** One more commit and checkpoint after the roots: the cap refuses it inside the turn, and the runner stops the flight. */
async function midTurnStop(ctx: RunContext): Promise<void> {
  commit(ctx.worktreePath, "mid-turn.txt");
  await ctx.checkpoint!({ reap: false });
}

interface AuthorityProbe { transactions: number; waits: (AbortSignal | undefined)[]; transfers: (string | null)[]; stopped: boolean[] }

/** Installs a source authority that refuses an aborted wait signal, only for the duration of each terminal
 *  transfer (the rest of the flight runs without one), so the producing-source authority wait is really
 *  exercised. `atStart` runs as the transfer begins; `inWait` runs inside the authority wait, as a shutdown arriving mid-transfer would. */
function authorityDuringTransfer(t: TestContext, r: ReturnType<typeof runner>, when: { atStart?: (flight: unknown) => void; inWait?: () => void } = {}): AuthorityProbe {
  const probe: AuthorityProbe = { transactions: 0, waits: [], transfers: [], stopped: [] };
  const original = Reflect.get(r, "transferRestorePointToTrustedBare") as (...a: unknown[]) => Promise<string | null>;
  t.mock.method(r as unknown as Record<string, (...a: unknown[]) => Promise<string | null>>, "transferRestorePointToTrustedBare",
    async function (this: unknown, ...a: unknown[]) {
      const flight = a[0] as { cancel: AbortController; owedLimitStop?: unknown };
      when.atStart?.(flight);
      probe.stopped.push(flight.cancel.signal.aborted && flight.owedLimitStop !== undefined);
      Reflect.set(r, "authority", {
        withTransaction: async (fn: () => Promise<unknown>, wait: { signal?: AbortSignal } = {}) => {
          probe.transactions++;
          probe.waits.push(wait.signal);
          when.inWait?.();
          if (wait.signal?.aborted) throw new Error("source authority wait aborted");
          return fn();
        },
        recordAdmissions: (_sink: Set<string>, fn: () => Promise<unknown>) => fn(),
      });
      try { const out = await original.apply(this, a); probe.transfers.push(out); return out; }
      finally { Reflect.set(r, "authority", undefined); }
    });
  return probe;
}

it("an owed-cap stop does not abort the terminal refusal-release fetch's source authority wait", opts, async t => {
  const { claim, server, work } = setup(44830);
  let probe!: AuthorityProbe;
  const r = await runFailed(claim, work, midTurnStop, runner => { Reflect.set(runner, "attemptPaths", true); probe = authorityDuringTransfer(t, runner); });
  assert.ok(probe.stopped.length > 0 && probe.stopped.every(Boolean), "the owed-limit stop had aborted the flight before each terminal transfer");
  assert.ok(probe.transactions > 0, "the producing-source authority wait was exercised");
  assert.ok(probe.transfers.length > 0 && probe.transfers.every(h => h !== null && h === probe.transfers[0]), "the refused head was released");
  assert.equal(server.heldCalls.length, 1, "held publication proceeded");
  await redrive(r);
  assert.equal(server.acked(), 1);
});

it("a recorded cancel still aborts the terminal refusal-release fetch after an owed-cap stop", opts, async t => {
  const { claim, server, work } = setup(44831);
  let probe!: AuthorityProbe;
  await runFailed(claim, work, midTurnStop, runner => {
    Reflect.set(runner, "attemptPaths", true);
    probe = authorityDuringTransfer(t, runner, {
      atStart: flight => (flight as { steering: { markCancelled(): void } }).steering.markCancelled(),
    });
  });
  assert.ok(probe.transactions > 0, "the authority wait was exercised");
  assert.ok(probe.transfers.length > 0 && probe.transfers.every(h => h === null), "the cancelled transfer released nothing");
  assert.equal(server.heldCalls.length, 0, "no held publication");
});

it("a recorded claim fence still aborts the terminal refusal-release fetch after an owed-cap stop", opts, async t => {
  const { claim, server, work } = setup(44834);
  let probe!: AuthorityProbe;
  // A fenced claim ends the flight quietly, so runFailed's failed-report assertion does not apply.
  const r = runner(failingWork(work, midTurnStop), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  Reflect.set(r, "attemptPaths", true);
  probe = authorityDuringTransfer(t, r, {
    atStart: flight => Reflect.set((flight as { steering: object }).steering, "fence", "released"),
  });
  await r.execute(claim);
  assert.ok(probe.transactions > 0, "the authority wait was exercised");
  assert.ok(probe.transfers.length > 0 && probe.transfers.every(h => h === null), "the fenced transfer released nothing");
  assert.equal(server.heldCalls.length, 0, "no held publication");
});

it("a claim fence recorded after the first acquisition still aborts the later acquisitions of the release fetch", opts, async t => {
  const { claim, server, work } = setup(44835);
  let probe!: AuthorityProbe;
  const r = runner(failingWork(work, midTurnStop), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  Reflect.set(r, "attemptPaths", true);
  let flightRef: { steering: object } | undefined;
  probe = authorityDuringTransfer(t, r, {
    atStart: flight => { flightRef = flight as { steering: object }; },
    inWait: () => { if (probe.transactions === 1) Reflect.set(flightRef!.steering, "fence", "released"); },
  });
  await r.execute(claim);
  assert.ok(probe.transactions >= 2, "a later acquisition was exercised after the fence was recorded");
  assert.ok(probe.transfers.length > 0 && probe.transfers.every(h => h === null), "the transfer released nothing");
  assert.equal(server.heldCalls.length, 0, "no held publication");
});

it("shutdown during the terminal refusal-release fetch still aborts it after an owed-cap stop", opts, async t => {
  const { claim, server, work } = setup(44832);
  let probe!: AuthorityProbe;
  await runFailed(claim, work, midTurnStop, runner => {
    Reflect.set(runner, "attemptPaths", true);
    probe = authorityDuringTransfer(t, runner, { inWait: () => (Reflect.get(runner, "shutdownSignal") as AbortController).abort() });
  });
  assert.ok(probe.transactions > 0, "the authority wait was exercised");
  assert.ok(probe.transfers.length > 0 && probe.transfers.every(h => h === null), "the shut-down transfer released nothing");
  assert.equal(server.heldCalls.length, 0, "no held publication");
});

// --- Issue #4481 item E: real later claims of one run across worker restarts ------------------------------------
// Every generation is a real gitlabClaim of the SAME run executed through the real failing work and the real
// terminal hook. A restart is a brand new GitCache over the same data dir (restartGitCache) with its own process
// nonce: none of the GitCache's in-process refusal state survives (closed generations, recording failures, live or
// finished import stages), while on-disk records such as admission files do. The WorkerClient, the fake api and
// module-level state persist, so this is not a full process restart.
//
// A later claim of a run whose earlier generation still has local custody does not reach that generation's terminal
// gate: it adopts the retained source (see the lost-report case below). History 1 therefore models an OLD worker's
// released generation by what it leaves behind: the generation released, then its clone, journal key and (since an
// old worker never wrote one) its admission file are removed. History 2's old middle generation is this build with
// admitRefusalProvenance stubbed to "failed"; its end state is modelled by removing its clone and journal key.

interface GateSeen { generation: number; result: RefusalReleaseGate }
interface Gens { runId: string; iid: number; servers: HeldServer[]; gates: GateSeen[]; work: Partial<Work>; logs: unknown[]; last?: ReturnType<typeof runner> }

const newGens = (iid: number): Gens => ({ runId: randomUUID(), iid, servers: [], gates: [], work: {}, logs: [] });
const lastServer = (g: Gens): HeldServer => g.servers[g.servers.length - 1]!;

/** One real claim of the run; passing `process` first replaces the GitCache with a fresh one under that nonce. */
async function claimGeneration(g: Gens, generation: number, how: { process?: string; lostReport?: boolean; old?: boolean; cap?: number; executor?: Executor;
  onRunner?: (r: ReturnType<typeof runner>) => void } = {}): Promise<void> {
  if (how.process) restartGitCache({ maxOwedCandidates: how.cap ?? CAP, processNonce: how.process });
  if (how.old) git.admitRefusalProvenance = async () => "failed";
  const gate = git.refusalReleaseGate.bind(git);
  git.refusalReleaseGate = async (...a: Parameters<typeof gate>) => {
    const result = await gate(...a);
    g.gates.push({ generation: a[2], result });
    return result;
  };
  const claim = gitlabClaim(g.iid, { run_id: g.runId, claim_generation: generation, inventory_guarded: true });
  g.servers.push(heldServer(claim));
  skipCheckpoints();
  const stop = how.lostReport ? api.dropStatesWhen(g.runId, b => b.status === "failed") : undefined;
  const rec = recordingLogger();
  g.logs = rec.lines;
  const r = runnerWith(() => ({ executor: how.executor ?? failingWork(g.work) }), fakeGitlab().gitlab, JOIN_TOKEN, rec.logger, { checkpointIntervalMs: 1 });
  how.onRunner?.(r);
  // A restarted worker recovers its boot-time state before it takes the next claim.
  if (how.process && g.last) await r.resumePendingRecoveries(undefined, await r.snapshotBootRecoveries());
  try { await r.execute(claim); } finally { stop?.(); }
  g.last = r;
}

/** The generation released through the held V2 publication and FINAL, as a worker that has finished with it leaves things. */
async function releaseGeneration(g: Gens, generation: number, label: string, oldWorker: boolean): Promise<void> {
  await redrive(g.last!);
  const server = lastServer(g);
  assert.equal(server.heldCalls.length, 1, `${label}: the held publication was created`);
  assert.equal(server.finals.length, 1, `${label}: the FINAL was sent`);
  assert.equal(server.acked(), 1, `${label}: the release was ACKed`);
  fs.rmSync(g.work.clone!, { recursive: true, force: true });
  command(bare(), ["config", "--unset-all", `uzi-recovery.agent/issue-${g.iid}.clone`]);
  if (oldWorker) fs.rmSync(path.join(refusalDir(), `admission-${g.runId}-${generation}.json`));
}

const gateAt = (g: Gens, generation: number): RefusalReleaseGate[] => g.gates.filter(x => x.generation === generation).map(x => x.result);

/** The custody-retention assertions for the generation that must not release. */
function assertRetained(g: Gens, generation: number, reason: string, owed = CAP): void {
  const gates = gateAt(g, generation);
  assert.ok(gates.length >= 1, `the real terminal hook consulted the gate at generation ${generation}`);
  assert.deepEqual(gates.filter(x => x.kind === "eligible"), [], "no gate call was eligible");
  assert.deepEqual(gates[0], { kind: "ineligible", reason }, "the gate refused for the distinguishing reason");
  const server = lastServer(g);
  assert.equal(server.holdOpen(), true, "the guarded hold stays open");
  assert.equal(server.heldCalls.length, 0, "no held publication POST");
  assert.equal(server.finals.length, 0, "no FINAL, archive or held");
  assert.equal(server.archiveUploads(), 0, "no archive upload");
  assert.equal(server.archiveReserves(), 0, "no archive capture");
  assert.equal(server.acked(), 0, "nothing was released");
  assert.equal(command(bare(), ["for-each-ref", `refs/uzi-owed/${g.runId}/`]).split("\n").filter(Boolean).length, owed, "the owed evidence is still pinned");
  assert.ok(refusalFiles().includes(`owedpath-${g.runId}-${generation}.json`), "the at-cap marker is still present");
  assert.ok(fs.existsSync(g.work.clone!), "the runner clone is retained");
}

it("positive control: every generation updated in one process releases at the later generation", opts, async () => {
  const g = newGens(44850);
  await claimGeneration(g, 1, { process: "worker-process-p1" });
  await releaseGeneration(g, 1, "generation 1", false);
  await claimGeneration(g, 2);
  await releaseGeneration(g, 2, "generation 2", false);
  assert.deepEqual(gateAt(g, 2).filter(x => x.kind !== "eligible"), [], "the generation 2 gate was eligible every time");
  assert.ok(gateAt(g, 2).length >= 1);
  assert.equal(lastServer(g).holdOpen(), false);
});

it("an old worker's released generation, then a restarted updated worker's claim, keeps custody", opts, async () => {
  const g = newGens(44851);
  await claimGeneration(g, 1, { process: "worker-process-p1" });
  await releaseGeneration(g, 1, "generation 1", true);
  await claimGeneration(g, 2, { process: "worker-process-p2" });
  await redrive(g.last!);
  assertRetained(g, 2, "admission for generation 1 absent");
});

/** Two owed roots on fresh lines of history, then a divergent head that only a successful terminal import can take. */
function divergentWork(work: Partial<Work>): Executor {
  return { run: async (ctx: RunContext) => {
    const clone = ctx.worktreePath;
    work.clone = clone;
    for (const name of ["gen2-a.txt", "gen2-b.txt"]) {
      command(clone, ["reset", "--hard", originMain()]);
      commit(clone, name);
      await ctx.checkpoint!({ reap: false });
    }
    commit(clone, "gen2-final.txt");
    throw new Error("executor failed");
  } };
}

it("updated, then old (real displacement), then a restarted updated worker keeps custody", opts, async () => {
  const g = newGens(44852);
  await claimGeneration(g, 1, { process: "worker-process-p1" });
  const trackingRef = `refs/uzi-runner/agent/issue-${g.iid}`;
  const gen1Head = command(bare(), ["rev-parse", "--verify", trackingRef]);
  await releaseGeneration(g, 1, "generation 1", false);
  // Generation 2 is an old worker's claim (no admission) in its own cache with room for its terminal import, which
  // therefore succeeds and moves the tracking ref off generation 1's head.
  await claimGeneration(g, 2, { process: "worker-process-p2", cap: CAP + 2, old: true, executor: divergentWork(g.work) });
  const gen2Head = command(g.work.clone!, ["rev-parse", "HEAD"]);
  assert.notEqual(gen2Head, gen1Head, "generation 2 built its own head");
  assert.equal(command(bare(), ["rev-parse", "--verify", trackingRef]), gen2Head, "generation 2's import moved the tracking ref to its head");
  assert.equal(command(bare(), ["rev-parse", "--verify", `refs/uzi-owed/${g.runId}/${gen1Head}`]), gen1Head,
    "the displacement preserved generation 1's tracking head as an owed ref");
  assert.ok(!refusalFiles().includes(`admission-${g.runId}-2.json`), "the old worker wrote no admission");
  // This build's terminal hook ran for generation 2 and kept its hold; model the state a finished old worker leaves.
  fs.rmSync(g.work.clone!, { recursive: true, force: true });
  command(bare(), ["config", "--unset-all", `uzi-recovery.agent/issue-${g.iid}.clone`]);
  await claimGeneration(g, 3, { process: "worker-process-p3" });
  await redrive(g.last!);
  assertRetained(g, 3, "admission for generation 1 is from another process", CAP + 2);
});

for (const old of [true, false]) {
  it(`${old ? "an old" : "an updated"} worker's refusal with a lost terminal report: the next claim adopts the retained source and releases nothing`, opts, async () => {
    const g = newGens(old ? 44853 : 44854);
    Reflect.set(git, "maxOwedCandidates", CAP);
    if (old) git.admitRefusalProvenance = async () => "failed";
    await claimGeneration(g, 1, { lostReport: true });
    assert.equal(failedStates(g.runId).length, 0, "the terminal report was lost");
    assert.equal(refusalFiles().includes(`admission-${g.runId}-1.json`), !old,
      old ? "the old worker wrote no admission" : "the updated worker wrote its admission");
    const before = g.gates.length;
    await claimGeneration(g, 2, { process: "worker-process-p2" });
    await redrive(g.last!);
    const afterRestart = g.gates.slice(before).filter(x => x.generation === 1).map(x => x.result);
    if (old) assert.deepEqual(afterRestart, [], "the old worker left nothing for boot recovery to prove");
    else {
      assert.ok(afterRestart.length >= 1, "boot recovery consulted the generation 1 gate under the restarted cache");
      assert.deepEqual(afterRestart[0], { kind: "ineligible", reason: "admission for generation 1 is from another process" },
        "the restarted cache refuses generation 1 for the admission");
    }
    const logs = g.logs as { msg: string; blocker?: string }[];
    assert.ok(logs.some(l => l.msg === "Retained recovery blocked: budget exhausted; local work and custody retained" && l.blocker === "budget_exhausted"),
      "the retained-recovery adoption exhausted its budget");
    assert.deepEqual(gateAt(g, 2), [], "the terminal gate was never consulted at generation 2: the retention is not the admission check");
    for (const s of g.servers) {
      assert.equal(s.heldCalls.length, 0, "no held publication POST");
      assert.equal(s.finals.length, 0, "no FINAL");
      assert.equal(s.holdOpen(), true, "the guarded hold stays open");
    }
    // The admission does show at generation 1: the updated worker's gate is eligible and it captured and uploaded the
    // archive (its FINAL was never sent, the report being lost); the old worker's gate refuses before any capture.
    assert.equal(g.servers[0]!.archiveUploads(), old ? 0 : 1, "only the updated worker's generation 1 uploaded an archive");
    assert.equal(g.servers[1]!.archiveUploads(), 0, "generation 2 uploaded nothing");
    assert.ok(command(bare(), ["for-each-ref", `refs/uzi-owed/${g.runId}/`]).length > 0, "the owed evidence is still pinned");
  });
}

// The retained-capture gate precedes successor creation, so an ineligible history cannot reach that successor's
// terminal hook. Give adoption enough pin capacity to distinguish that refusal from the full-cap retry budget.
// The control keeps the complete history in one updated process and must adopt, execute and release instead.
for (const old of [true, false]) {
  it(`${old ? "an old worker's lost report is refused at retained capture before successor creation" : "control: an updated worker's lost report adopts and releases when retained capture is eligible"}`, opts, async t => {
    const g = newGens(old ? 44855 : 44856);
    await claimGeneration(g, 1, {
      process: "retained-capture-p1", lostReport: true, old,
      executor: { run: async ctx => {
        g.work.clone = ctx.worktreePath;
        // Linear roots are adoptable without independent remediation evidence. The shared failingWork fixture
        // intentionally diverges, which correctly stops wrapper adoption even when this gate permits capture.
        for (const name of ["retained-root-a.txt", "retained-root-b.txt"]) {
          commit(ctx.worktreePath, name);
          await ctx.checkpoint!({ reap: false });
        }
        commit(ctx.worktreePath, "retained-refused.txt");
        throw new Error("executor failed");
      } },
    });
    assert.equal(failedStates(g.runId).length, 0, "generation 1's terminal report was lost");
    const predecessor = g.work.clone!;
    assert.ok(fs.existsSync(predecessor), "the real retained source remains");
    assert.equal(refusalFiles().includes(`admission-${g.runId}-1.json`), !old);
    // This is a capacity control, not a bypass of provenance: the owed-path marker and the refused heads stay.
    // Both cases get the same room, so neither can pass just because a full cap prevented the retained capture.
    const cap = CAP + 16;
    if (!old) Reflect.set(git, "maxOwedCandidates", cap);
    let captures = 0, successors = 0, executions = 0, terminalTransfers = 0;
    const captureGates: RefusalReleaseGate[] = [];
    await claimGeneration(g, 2, {
      ...(old ? { process: "retained-capture-p2", cap } : {}),
      executor: { run: async ctx => {
        executions++;
        g.work.clone = ctx.worktreePath;
        // A read-only successful turn completes retained adoption; a later turn then fails normally.
        // Keep the already frozen retained inventory unchanged so its release remains a valid control.
        await ctx.onModelTurnSettled?.(1);
        throw new Error("executor failed");
      } },
      onRunner: r => {
        const coordinator = Reflect.get(r, "recovery") as RecoveryCoordinator;
        const capture = coordinator.captureAndUpload.bind(coordinator);
        t.mock.method(coordinator, "captureAndUpload", async (...args: Parameters<typeof capture>) => {
          captures++;
          const before = g.gates.length;
          try { return await capture(...args); }
          finally { captureGates.push(...g.gates.slice(before).filter(x => x.generation === 2).map(x => x.result)); }
        });
        const prepare = git.prepareRecoverySuccessor.bind(git);
        t.mock.method(git, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
          successors++;
          return prepare(...args);
        });
        const target = r as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;
        const transfer = target.transferRestorePointToTrustedBare!.bind(r);
        t.mock.method(target, "transferRestorePointToTrustedBare", async (...args: unknown[]) => {
          terminalTransfers++;
          return transfer(...args);
        });
      },
    });
    await redrive(g.last!);
    // The live flight deferred its source proof; the normal boot sweep can now observe it quiescent.
    // Reuse the same GitCache, so this does not invent a new process admission or bypass the gate.
    await restartReplay();
    assert.ok(captures >= 1, "the real retained-capture producer ran");
    assert.ok(captureGates.length >= 1, "captureAndUpload consulted generation 2's refusal gate");
    const logs = g.logs as { msg: string; blocker?: string }[];
    assert.ok(!logs.some(l => l.blocker === "budget_exhausted"), "pin capacity did not exhaust the recovery budget");
    if (old) {
      assert.deepEqual(captureGates[0], { kind: "ineligible", reason: "refusal import stage producer unproven" });
      assert.equal(lastServer(g).archiveUploads(), 0, "the refused retained capture uploaded no bundle");
      assert.ok(logs.some(l => l.blocker === "prerequisites_unavailable"), "the gate prevented the prerequisite bundle");
      assert.equal(successors, 0, "refusal precedes successor creation");
      assert.equal(executions, 0, "no successor executor ran");
      assert.equal(terminalTransfers, 0, "refusal precedes the successor terminal hook");
      for (const server of g.servers) noHold(server, "old-worker lost-report history");
      assert.ok(fs.existsSync(predecessor), "the predecessor clone stays in custody");
      assert.ok(refusalFiles().includes(`owedpath-${g.runId}-1.json`), "the cap history remains recorded");
    } else {
      assert.deepEqual(captureGates.filter(x => x.kind !== "eligible"), []);
      assert.ok(successors >= 1, "eligible capture permits real successor creation");
      assert.equal(executions, 1, "the successor executor ran");
      assert.ok(terminalTransfers >= 1, "the successor reached the real terminal hook");
      const server = lastServer(g);
      assert.equal(server.finals.length, 1, "the control sent its FINAL");
      assert.equal(server.finals[0]!.disposition?.kind, "archive", "the retained capture is delivered");
      assert.equal(server.acked(), 1, "the control released custody");
      assert.equal(server.holdOpen(), false);
    }
  });
}
