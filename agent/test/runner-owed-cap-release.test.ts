// Issue #4481 m2: a failed issue run whose owed heads reach the per-run cap releases custody through
// the held V2 publication and FINAL when the durable refusal gate proves completeness, and keeps custody
// (no publishHeld, no FINAL) whenever it cannot. Every case drives the real runner terminal hook.
import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { RunContext } from "../src/executor.js";
import { client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, PUBLICATION_ID, bare, command, commit, failedStates, failingWork,
  frozenRecords, heldServer, packObjects, redrive, skipCheckpoints, type HeldServer, type Work,
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

async function runFailed(claim: Claim, work: Partial<Work>, extra?: (ctx: RunContext) => Promise<void> | void) {
  const r = runner(failingWork(work, extra), fakeGitlab().gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
  await r.execute(claim);
  assert.equal(failedStates(claim.run_id).length, 1, "the honest failed report is sent once");
  return r;
}

/** A restarted worker over the same stores replays the journaled FINAL (the same GitCache keeps its process nonce). */
async function restartReplay(): Promise<void> {
  const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
  await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
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

it("evidence appearing after the archive upload and before the archive FINAL sends no FINAL", opts, async () => {
  const { claim, server, work } = setup(44820);
  server.heldAnswer = refusedHeld;
  const upload = client.uploadRecoveryBundle.bind(client);
  client.uploadRecoveryBundle = async (...a: Parameters<typeof upload>) => {
    const out = await upload(...a);
    injectUnledgeredStage(claim);
    return out;
  };
  const r = await runFailed(claim, work);
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
