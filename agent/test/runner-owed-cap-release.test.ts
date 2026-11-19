// Issue #4481 m2: a failed issue run whose owed heads reach the per-run cap releases custody through
// the held V2 publication and FINAL when the durable refusal gate proves completeness, and keeps custody
// (no publishHeld, no FINAL) whenever it cannot. Every case drives the real runner terminal hook.
import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { RunContext } from "../src/executor.js";
import { client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
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
