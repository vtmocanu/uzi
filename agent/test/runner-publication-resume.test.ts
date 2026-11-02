import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import { RunRunner } from "../src/runner.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { SummaryRunner } from "../src/summary-runner.js";
import type { Logger } from "../src/log.js";
import type { PublicationRecord } from "../src/publication-intent.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, worktreeDirFor } from "./runner-harness.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { FakeCodexExecutor, codexRig } from "./runner-codex-sinks-fixture.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// Issue #1324 M5: the publication-only resume. A claim that carries an active `publication_resume` finishes the
// recorded publication with ZERO model calls, through RunRunner with a real Outbox, a real git origin and the
// fake api (POST /publication-intent is scripted). The first flight enrols and parks on an MR-create outage;
// the second is the resumed episode under test.

installHarness();

const REPO_ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const GEN = 4;
const FEATURES = ["publication_resume_v1", "recovery_park_cause"];
const CODEX = { auth_mode: "api_key", access_token: "fixture-access", capability: "fixture-cap" };
const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

const roots: string[] = [];
afterEach(async () => {
  for (const r of roots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-publication-resume-"));
  roots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

/** A second worker: its own data dir and bare, so it holds nothing the first one did. */
function mkPeerGit(): GitCache {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-publication-peer-"));
  roots.push(dir);
  return new GitCache(dir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
}

const claimFor = (iid: number, over: Record<string, unknown> = {}) =>
  gitlabClaim(iid, { claim_generation: GEN, repo: { id: REPO_ID, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath }, ...over });
const githubClaim = (iid: number, over: Record<string, unknown> = {}) =>
  claimFor(iid, { repo: { id: REPO_ID, url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" }, ...over });

type Counts = { run: number; settle: number; summary: number; resume: number; handoff: number };
const counts = (): Counts => ({ run: 0, settle: 0, summary: 0, resume: 0, handoff: 0 });

/** An executor any model work trips: run() and closeAndSettlePlanSummary() throw and are counted. */
function tripwireExecutor(c: Counts): Executor {
  return {
    run: async () => { c.run++; throw new Error("tripwire: executor.run was called"); },
    closeAndSettlePlanSummary: async () => { c.settle++; throw new Error("tripwire: closeAndSettlePlanSummary was entered"); },
    killAgentTree() {},
  };
}

/** A SummaryRunner any use trips. */
function tripwireSummary(c: Counts): SummaryRunner {
  const trip = (): never => { c.summary++; throw new Error("tripwire: the SummaryRunner was used"); };
  return { deliverySummaryDeadline: trip, generateDeliverySummary: trip } as unknown as SummaryRunner;
}

type BuildOptions = { c?: Counts; phases?: boolean; extra?: Record<string, unknown>; log?: Logger; gitCache?: GitCache };
function build(executor: Executor, forge: { gitlab?: unknown; github?: unknown }, outbox: Outbox, opts: BuildOptions = {}): RunRunner {
  const r = new RunRunner(client, opts.gitCache ?? git, () => ({ executor }), opts.log ?? nullLogger(), 20, undefined, {
    pollMs: 5, planApprovalTimeoutMs: 0, prDescriptionHeadLagMs: 0, recoveryRetryMs: 5,
    checkpointTestHooks: { retryBackoffMs: [0, 0] },
    ...(forge.gitlab ? { gitlab: forge.gitlab as never } : {}), ...(forge.github ? { github: forge.github as never } : {}),
    outbox, ...(opts.c ? { summaryRunner: tripwireSummary(opts.c) } : {}), ...opts.extra,
  });
  const seams = r as unknown as Record<string, unknown>;
  seams["forgeRetrySleep"] = async () => {};
  seams["publicationRetry"] = { retryDelaysMs: [0, 0, 0], sleep: async () => {} };
  if (opts.c && opts.phases !== false) {
    const c = opts.c;
    // phaseResume and phasePreflightHandoff are where a resumed run would start a session and the model.
    seams["phaseResume"] = async () => { c.resume++; throw new Error("tripwire: phaseResume was entered"); };
    seams["phasePreflightHandoff"] = async () => { c.handoff++; throw new Error("tripwire: phasePreflightHandoff was entered"); };
  }
  return r;
}

const states = (runId: string) => api.states.filter(s => s.runId === runId).map(s => s.body);
const parks = (runId: string) => states(runId).filter(b => b.status === "recovery_wait");
const failed = (runId: string) => states(runId).filter(b => b.status === "failed");
const completed = (runId: string) => states(runId).filter(b => b.status === "completed");

/** Ack every intent request: an enrollment is (1,1); an advance echoes the sequence numbers it carries. */
function ackIntents(): void {
  api.publicationIntentWire = (req) => ({ status: 200, body: {
    enrollment_seq: (req.body.enrollment_seq as number | undefined) ?? 1,
    candidate_seq: (req.body.candidate_seq as number | undefined) ?? 1,
    state: "active",
  } });
}

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
const originBranch = (iid: number) => {
  try { return gitIn(fx.originPath, ["rev-parse", "--verify", "--quiet", `refs/heads/agent/issue-${iid}`]); } catch { return undefined; }
};
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

/** An executor that commits, then advances origin's workflows so the finalize overlay yields a new candidate. */
function advancingExecutor(): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "impl.ts"), "task content\n");
      execFileSync("git", ["-C", ctx.worktreePath, "add", "."], { env: ENV });
      execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "agent work"], { env: ENV });
      commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances");
      return { branch: ctx.branch };
    },
  };
}

type WireIntent = Record<string, unknown> & { candidate: string; original_head: string; floor: { kind: string; oid?: string }; mr: Record<string, unknown>; guard: Record<string, unknown> };
type Enrolled = { claim: ClaimResponse; intents: WireIntent[]; seqs: Array<{ enrollment_seq: number; candidate_seq: number }>; last: WireIntent };

/** Make every `git push` fail as a dropped connection until the returned switch is turned off. */
function pushOutage(): { restore: () => void } {
  const seam = git as unknown as { execScoped(cmd: string, args: string[], ...rest: unknown[]): Promise<unknown> };
  const real = seam.execScoped.bind(git);
  let down = true;
  seam.execScoped = async (cmd, args, ...rest) => {
    if (down && cmd === "git" && args.includes("push")) {
      throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr: "fatal: unable to access: connection reset by peer" });
    }
    return real(cmd, args, ...rest);
  };
  return { restore: () => { down = false; } };
}

/**
 * The first flight: enrol (1,1), optionally advance to a transformed candidate, and park on a forge outage at
 * the push (default: nothing is published, so the resumed episode has to do all of it) or at the MR create.
 */
async function enrolAndPark(iid: number, outbox: Outbox, opts: { github?: boolean; transformed?: boolean; handoff?: boolean; at?: "push" | "mr_create"; result?: Partial<ExecutorResult>; claim?: Record<string, unknown> } = {}): Promise<Enrolled> {
  client.protocolFeatures = FEATURES;
  ackIntents();
  if (opts.transformed) commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  if (opts.handoff) {
    // The broker accepts the pack and confirms exactly the tip the worker declared.
    client.publishCheckpoint = async (_runId, tipOid, pack) => {
      for await (const chunk of pack) void chunk;
      return { ok: true, body: { published: true, checkpoint_tip: tipOid } } as never;
    };
  }
  const at = opts.at ?? "push";
  const claim = opts.github ? githubClaim(iid, opts.claim) : claimFor(iid, opts.claim);
  const mrOutage = { intercept: (req: { method: string }) => req.method === "POST" ? { status: 503, text: async () => "down" } : undefined };
  const forgeOpts = at === "mr_create" ? mrOutage : {};
  const forge = opts.github ? { github: fakeGitHub(forgeOpts).github } : { gitlab: fakeGitlab(forgeOpts).gitlab };
  const stub = new StubExecutor(nullLogger());
  const executor: Executor = opts.transformed ? advancingExecutor()
    : opts.result ? { run: async (ctx: RunContext) => ({ ...(await stub.run(ctx)), ...opts.result }) } : stub;
  const outage = at === "push" ? pushOutage() : undefined;
  await build(executor, forge, outbox).execute(claim);
  outage?.restore();
  assert.equal(parks(claim.run_id).length, 1, `the first flight parked in the publication phase: ${JSON.stringify(states(claim.run_id).map(s => s.status))}`);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(originBranch(iid) === undefined, at === "push", "a push outage leaves nothing published; an MR outage leaves the branch");
  const requests = api.publicationIntentRequests.filter(q => q.runId === claim.run_id);
  const intents = requests.map(q => (q.body as { intent: WireIntent }).intent);
  const seqs = requests.map(q => ({
    enrollment_seq: (q.body.enrollment_seq as number | undefined) ?? 1, candidate_seq: (q.body.candidate_seq as number | undefined) ?? 1,
  }));
  return { claim, intents, seqs, last: intents.at(-1)! };
}

/** The resumed claim: a later generation carrying the active enrollment. */
const resumeClaim = (claim: ClaimResponse, intent: WireIntent, over: Record<string, unknown> = {}, seqs = { enrollment_seq: 1, candidate_seq: 1 }): ClaimResponse => {
  // The retained-source path asks the api who owns the run: this claim's generation, running.
  api.setOwnershipStatus(claim.run_id, "running", GEN + 1);
  return { ...claim, claim_generation: GEN + 1, publication_resume: { ...seqs, intent }, ...over } as ClaimResponse;
};

const reset = (): void => { api.publicationIntentRequests.length = 0; };

// ── the zero-model tripwire ───────────────────────────────────────────────────────────────────────────

it("the tripwire seam is live: an ordinary run trips the SummaryRunner and the executor", async () => {
  client.protocolFeatures = FEATURES;
  ackIntents();
  const c = counts();
  const claim = claimFor(5300);
  const stub = new StubExecutor(nullLogger());
  const executor: Executor = { run: async (ctx) => { c.run++; return stub.run(ctx); } };
  await build(executor, { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c, phases: false }).execute(claim);
  assert.equal(c.run, 1, "an ordinary claim runs the executor");
  assert.ok(c.summary > 0, "an ordinary claim reaches the description pass");
});

it("restart with the local record present: the Claude-identified resume publishes with zero model calls and no new enrollment", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5301, outbox);
  reset();
  const c = counts();
  const forge = fakeGitlab();
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, outbox, { c }).execute(resumeClaim(claim, last));

  assert.deepEqual(c, counts(), "no executor, session, plan summary or description pass ran");
  assert.equal(api.publicationIntentRequests.length, 0, "the adopted enrollment is not re-enrolled at the new generation (the M4 N1 regression)");
  assert.equal(completed(claim.run_id).length, 1, `the resumed episode completed: ${JSON.stringify(states(claim.run_id).map(s => s.status))}`);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(forge.calls.filter(x => x.method === "POST").length, 1, "one merge request was created");
  assert.equal(originBranch(5301), last.candidate, "the recorded candidate is what landed");
  assert.equal(outbox.hasPublicationRecords(claim.run_id), false, "the applied completion discards the record");
});

it("cold handoff: a peer with no local record and the checkpoint mirror publishes with zero model calls, whatever the harness", async () => {
  for (const harness of ["claude", "codex"] as const) {
    const iid = harness === "claude" ? 5302 : 5303;
    const first = await enrolAndPark(iid, await mkOutbox(), { handoff: true });
    // The checkpoint ref the broker would have written: the candidate, mirrored by the peer's clone fetch.
    const workerBare = git.barePathFor(fx.originPath);
    gitIn(fx.originPath, ["fetch", "-q", workerBare, `${first.last.candidate}:refs/uzi-checkpoints/agent/issue-${iid}`]);
    reset();
    const peer = mkPeerGit();
    const c = counts();
    const forge = fakeGitlab();
    const resumed = resumeClaim(first.claim, first.last, {
      checkpoint_tip: first.last.candidate, session_id: "sess-before-the-park",
      ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat-000000", codex: CODEX } } : {}),
    });
    // The Codex-identified peer runs the Codex safety facade: the finalize boundary and the capture lane are
    // the real ones, around an executor whose run() trips.
    const rig = codexRig({ authMode: "api_key" });
    const executor = harness === "codex"
      ? Object.assign(new FakeCodexExecutor(rig.safety, async () => { c.run++; throw new Error("tripwire: executor.run was called"); }, rig.settle, rig.capture), { sandboxesCommands: true })
      : tripwireExecutor(c);
    await build(executor, { gitlab: forge.gitlab }, await mkOutbox(), { c, gitCache: peer }).execute(resumed);

    assert.deepEqual(c, counts(), `${harness}: no model work`);
    assert.equal(api.publicationIntentRequests.length, 0, `${harness}: the server's intent is adopted, never re-enrolled`);
    assert.equal(completed(first.claim.run_id).length, 1, `${harness}: ${JSON.stringify(states(first.claim.run_id).map(s => s.status))}`);
    assert.equal(failed(first.claim.run_id).length, 0);
    assert.equal(originBranch(iid), first.last.candidate, `${harness}: the mirrored candidate is what landed`);
  }
});

// ── the handoff source proof ─────────────────────────────────────────────────────────────────────────

it("handoff (a): H a proper ancestor of C, roots present: the transformed candidate publishes", async () => {
  const outbox = await mkOutbox();
  const first = await enrolAndPark(5304, outbox, { github: true, transformed: true });
  assert.notEqual(first.last.candidate, first.last.original_head, "the parked candidate is a transformed one");
  assert.deepEqual(first.seqs.at(-1), { enrollment_seq: 1, candidate_seq: 2 });
  reset();
  const c = counts();
  const forge = fakeGitHub();
  await build(tripwireExecutor(c), { github: forge.github }, outbox, { c }).execute(resumeClaim(first.claim, first.last, {}, { enrollment_seq: 1, candidate_seq: 2 }));
  assert.deepEqual(c, counts());
  assert.equal(completed(first.claim.run_id).length, 1, JSON.stringify(states(first.claim.run_id).map(s => s.status)));
  assert.equal(originBranch(5304), first.last.candidate, "the recorded candidate C landed, not H");
  assert.equal(api.publicationIntentRequests.filter(q => q.body.enrollment_seq === undefined).length, 0, "no fresh enrollment");
});

it("handoff (a): the finalization pin names the recorded original head H, not the candidate the clone sits at", async () => {
  const outbox = await mkOutbox();
  const first = await enrolAndPark(5306, outbox, { github: true, transformed: true });
  reset();
  const c = counts();
  const r = build(tripwireExecutor(c), { github: fakeGitHub().github }, outbox, {
    c, extra: { recovery: new RecoveryCoordinator({ client: new FakeRecoveryClient(), git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: "publication-resume-worker-fixture" }) },
  });
  const rec = (r as unknown as { recovery: { pin: (input: { sourceSha: string; finalizationPin?: boolean; attemptedHeadSha?: string }) => Promise<unknown> } }).recovery;
  const real = rec.pin.bind(rec);
  const pins: Array<{ sourceSha: string; finalizationPin?: boolean }> = [];
  rec.pin = async (input) => { pins.push({ sourceSha: input.sourceSha, ...(input.finalizationPin ? { finalizationPin: true } : {}) }); return real(input); };
  const pushed: Array<{ originalHead: string; candidate: string }> = [];
  const realPush = git.pushBranch.bind(git);
  git.pushBranch = (async (...args: Parameters<typeof realPush>) => {
    const publication = args[5];
    if (publication) pushed.push({ originalHead: publication.originalHead, candidate: publication.candidate });
    return realPush(...args);
  }) as typeof git.pushBranch;
  await r.execute(resumeClaim(first.claim, first.last, {}, { enrollment_seq: 1, candidate_seq: 2 }));
  assert.deepEqual(c, counts());
  assert.notEqual(first.last.candidate, first.last.original_head);
  assert.deepEqual(pushed, [{ originalHead: first.last.original_head, candidate: first.last.candidate }],
    "the push group is (recorded H, recorded C), which is what the floor refresh proves history against");
  assert.ok(pins.some(p => p.finalizationPin && p.sourceSha === first.last.original_head), `the finalization pin is H: ${JSON.stringify(pins)}`);
  assert.ok(!pins.some(p => p.finalizationPin && p.sourceSha === first.last.candidate), "never the candidate");
});

type HoldCase ={ name: string; reason: string; mutate: (intent: WireIntent, ctx: { parent: string }) => WireIntent };
const holdCases: HoldCase[] = [
  { name: "(b) a rewritten candidate that excludes H", reason: "publication_source_unavailable",
    mutate: (i, { parent }) => ({ ...i, candidate: parent }) },
  { name: "(c) a missing original head (root)", reason: "publication_source_unavailable",
    mutate: (i) => ({ ...i, original_head: "e".repeat(40) }) },
  { name: "(c) a missing pinned floor", reason: "publication_source_unavailable",
    mutate: (i) => ({ ...i, floor: { kind: "pinned", oid: "d".repeat(40) } }) },
  { name: "an intent this worker cannot parse", reason: "publication_intent_mismatch",
    mutate: (i) => ({ ...i, v: 2 }) },
];
for (const hc of holdCases) {
  it(`holds ${hc.reason} for ${hc.name}: nothing is pushed, no failure, no model call, exact wire body`, async () => {
    const outbox = await mkOutbox();
    const { claim, last } = await enrolAndPark(5310 + holdCases.indexOf(hc), outbox);
    reset();
    const parent = gitIn(git.barePathFor(fx.originPath), ["rev-parse", `${last.original_head}^`]);
    const c = counts();
    const forge = fakeGitlab();
    const tampered = hc.mutate(last, { parent });
    // The local record is the enrolled one; the claim's intent is the tampered one, so use a fresh outbox for
    // the cases that must not be answered by the local record.
    await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, tampered));

    assert.deepEqual(c, counts());
    assert.equal(originBranch(5310 + holdCases.indexOf(hc)), undefined, "nothing was pushed");
    assert.equal(forge.calls.length, 0, "no merge request");
    assert.equal(failed(claim.run_id).length, 0, "a hold is a state report, never a failure");
    assert.equal(completed(claim.run_id).length, 0);
    const holds = parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined);
    assert.equal(holds.length, 1);
    assert.deepEqual(holds[0], {
      status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_phase: "publication",
      publication_hold_reason: hc.reason, claim_generation: GEN + 1,
    });
    assert.equal(api.publicationIntentRequests.length, 0, "a hold sends no intent request");
  });
}

it("holds publication_model_required when the restored workspace holds work beyond the recorded candidate", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5313, outbox);
  reset();
  // Work committed in the parked clone after the candidate: the resume would have to publish something the
  // record does not describe, which only the agent could reconcile.
  const clone = worktreeDirFor(5313);
  fs.writeFileSync(path.join(clone, "later.ts"), "later work\n");
  gitIn(clone, ["add", "."]);
  gitIn(clone, [...IDENT, "commit", "-m", "work after the candidate"]);
  const c = counts();
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, outbox, { c }).execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5313), undefined, "nothing was pushed");
  const holds = parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined);
  assert.deepEqual(holds.map(h => h.publication_hold_reason), ["publication_model_required"]);
  assert.equal(failed(claim.run_id).length, 0);
});

it("holds publication_source_unavailable when the restored workspace is not the candidate (a peer with no checkpoint evidence)", async () => {
  const first = await enrolAndPark(5314, await mkOutbox(), { handoff: true });
  assert.equal(parks(first.claim.run_id)[0]!.handoff_checkpoint_tip, first.last.candidate, "the park offered the candidate as the handoff tip");
  const workerBare = git.barePathFor(fx.originPath);
  gitIn(fx.originPath, ["fetch", "-q", workerBare, `${first.last.candidate}:refs/uzi-checkpoints/agent/issue-5314`]);
  reset();
  const c = counts();
  // No checkpoint_tip and no session: the peer seeds from the default branch, so its clone is not at C.
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c, gitCache: mkPeerGit() })
    .execute(resumeClaim(first.claim, first.last));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5314), undefined);
  assert.deepEqual(parks(first.claim.run_id).filter(b => b.publication_hold_reason !== undefined).map(h => h.publication_hold_reason), ["publication_source_unavailable"]);
});

it("holds publication_model_required for a scope-capped delivery the live claim cannot restate", async () => {
  const { claim, last } = await enrolAndPark(5315, await mkOutbox());
  reset();
  const c = counts();
  const partial = { ...last, mr: { ...last.mr, title: `[partial] ${claim.issue_title}` } };
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, partial));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5315), undefined);
  assert.deepEqual(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).map(h => h.publication_hold_reason), ["publication_model_required"]);
});

// ── candidate ordering between the local record and the api ──────────────────────────────────────────

/** Write a local record for `intent` at the given sequence numbers into `outbox`. */
async function seedLocal(outbox: Outbox, intent: WireIntent, seqs: { enrollment_seq: number; candidate_seq: number }): Promise<void> {
  const saved = await outbox.savePublicationRecord({ ...(intent as unknown as PublicationRecord), owner_kind: "issue" });
  assert.ok(saved.ok, "the seed was saved");
  assert.equal((await outbox.replacePublicationRecord({ ...saved.record, ...seqs }, {})).ok, true);
}

it("server ahead: the api's newer candidate is adopted, never the older local one", async () => {
  const first = await enrolAndPark(5320, await mkOutbox(), { github: true, transformed: true });
  assert.equal(first.intents.length, 2);
  reset();
  // This worker only ever saw candidate_seq 1 (H); the api is at candidate_seq 2 (the transformed C).
  const stale = await mkOutbox();
  await seedLocal(stale, first.intents[0]!, { enrollment_seq: 1, candidate_seq: 1 });
  const c = counts();
  await build(tripwireExecutor(c), { github: fakeGitHub().github }, stale, { c })
    .execute(resumeClaim(first.claim, first.last, {}, { enrollment_seq: 1, candidate_seq: 2 }));
  assert.deepEqual(c, counts());
  assert.equal(completed(first.claim.run_id).length, 1, JSON.stringify(states(first.claim.run_id).map(s => s.status)));
  assert.equal(originBranch(5320), first.last.candidate, "the server's candidate landed");
  assert.equal(api.publicationIntentRequests.length, 0, "nothing was sent: the api's state was adopted as is");
});

it("local ahead: a candidate advance whose ACK was lost is sent again and used once ACKed", async () => {
  const outbox = await mkOutbox();
  const first = await enrolAndPark(5321, outbox, { github: true, transformed: true });
  reset();
  const resent: Array<{ enrollment_seq?: unknown; candidate_seq?: unknown; claim_generation?: unknown }> = [];
  api.publicationIntentWire = (req) => {
    resent.push({ enrollment_seq: req.body.enrollment_seq, candidate_seq: req.body.candidate_seq, claim_generation: req.body.claim_generation });
    return { status: 200, body: { enrollment_seq: 1, candidate_seq: 2, state: "active" } };
  };
  // The api only knows the original enrollment (candidate_seq 1, H); the local record is at candidate_seq 2.
  const c = counts();
  await build(tripwireExecutor(c), { github: fakeGitHub().github }, outbox, { c })
    .execute(resumeClaim(first.claim, first.intents[0]!, {}, { enrollment_seq: 1, candidate_seq: 1 }));
  assert.deepEqual(c, counts());
  assert.deepEqual(resent, [{ enrollment_seq: 1, candidate_seq: 2, claim_generation: GEN + 1 }], "the local advance was re-sent once, fenced by the live generation");
  assert.equal(completed(first.claim.run_id).length, 1, JSON.stringify(states(first.claim.run_id).map(s => s.status)));
  assert.equal(originBranch(5321), first.last.candidate, "the local (newer) candidate landed once the api acknowledged it");
});

it("local ahead with the re-send unacknowledged: retained, nothing pushed, no failure and no hold", async () => {
  const outbox = await mkOutbox();
  const first = await enrolAndPark(5322, outbox, { github: true, transformed: true });
  reset();
  api.publicationIntentWire = () => "drop";
  const c = counts();
  const forge = fakeGitHub();
  await build(tripwireExecutor(c), { github: forge.github }, outbox, { c })
    .execute(resumeClaim(first.claim, first.intents[0]!, {}, { enrollment_seq: 1, candidate_seq: 1 }));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5322), undefined);
  assert.equal(forge.all.length, 0);
  assert.equal(failed(first.claim.run_id).length, 0);
  assert.equal(parks(first.claim.run_id).filter(b => b.publication_hold_reason !== undefined).length, 0, "an unknown outcome is not a hold");
  assert.equal((await outbox.loadPublicationRecord(first.claim.run_id, GEN)).kind, "ok", "the record is kept");
});

it("content mismatch: same sequence numbers, other content: held as an intent mismatch", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5323, outbox);
  reset();
  const c = counts();
  const forge = fakeGitlab();
  const weaker = { ...last, mr: { ...last.mr, title: "a different title" } };
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, outbox, { c }).execute(resumeClaim(claim, weaker));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5323), undefined);
  assert.equal(parks(claim.run_id).filter(b => b.publication_hold_reason === "publication_intent_mismatch").length, 1);
  assert.equal(failed(claim.run_id).length, 0);
});

it("a stale generation: an enrollment newer than the claim holds as an intent mismatch", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5324, outbox);
  reset();
  const c = counts();
  // A fresh outbox: the local record cannot be what refuses it, the enrollment's own generation must.
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c })
    .execute(resumeClaim(claim, { ...last, claim_generation: GEN + 9 }));
  assert.deepEqual(c, counts());
  assert.equal(parks(claim.run_id).filter(b => b.publication_hold_reason === "publication_intent_mismatch").length, 1);
  assert.equal(originBranch(5324), undefined);
});

// ── D6: a landed push and an existing merge request are adopted ─────────────────────────────────────

it("a push that already landed is a no-op and an existing merge request is adopted, its head and body re-verified", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5316, outbox, { at: "mr_create" });
  const landed = originBranch(5316);
  assert.equal(landed, last.candidate, "the first flight's push landed at C");
  reset();
  const c = counts();
  const forge = fakeGitlab({ existing: "Reviewer-written description", head: last.candidate });
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, outbox, { c }).execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5316), landed, "the non-forced push left the remote exactly at C");
  assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(forge.calls.filter(x => x.method === "POST").length, 1, "one create call, answered with the existing merge request");
  assert.ok(forge.reads.length > 0, "the adopted merge request was read back");
});

it("without a usable outbox the resume retains the work: nothing is pushed, nothing fails, nothing is held", async () => {
  const { claim, last } = await enrolAndPark(5317, await mkOutbox());
  reset();
  const c = counts();
  const forge = fakeGitlab();
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c, extra: { outbox: undefined } }).execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5317), undefined);
  assert.equal(forge.calls.length, 0);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).length, 0);
});

// ── the hold report and its ACK ──────────────────────────────────────────────────────────────────────

it("the client refuses a malformed hold: a surface or step, an unknown reason or no generation never leave the worker", async () => {
  const hold = { status: "recovery_wait" as const, recovery_cause: "forge_unreachable", recovery_phase: "publication" as const, publication_hold_reason: "publication_model_required" as const, claim_generation: 3 };
  const runId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
  await assert.rejects(client.reportState(runId, { ...hold, publication_surface: "git" }), /publication park requires/);
  await assert.rejects(client.reportState(runId, { ...hold, publication_step: "push" }), /publication park requires/);
  await assert.rejects(client.reportState(runId, { ...hold, publication_hold_reason: "something_else" as never }), /publication park requires/);
  await assert.rejects(client.reportState(runId, { ...hold, claim_generation: 0 }), /publication park requires/);
  await assert.rejects(client.reportState(runId, { ...hold, status: "failed" as never }), /publication park requires/);
  // A hold with no recovery_phase is as malformed: it must not slip past the validation as an untyped park.
  const { recovery_phase: _phase, ...noPhase } = hold;
  await assert.rejects(client.reportState(runId, noPhase), /publication park requires/);
  assert.equal(api.states.filter(s => s.runId === runId).length, 0, "nothing reached the api");
});

it("a well-formed hold reaches the api exactly as shaped", async () => {
  const runId = "dddddddd-dddd-4ddd-8ddd-ddddddddddde";
  await client.reportState(runId, { status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_phase: "publication", publication_hold_reason: "publication_source_unavailable", claim_generation: 3 });
  assert.deepEqual(states(runId).at(-1), {
    status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_phase: "publication",
    publication_hold_reason: "publication_source_unavailable", claim_generation: 3,
  });
});

async function heldFlight(iid: number, answer?: { httpStatus: number; reason: string }): Promise<{ claim: ClaimResponse; outbox: Outbox }> {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(iid, outbox);
  reset();
  if (answer) {
    api.answerStateOnce(claim.run_id, (b) => b.publication_hold_reason !== undefined, { httpStatus: answer.httpStatus, runStatus: "running", gateRevision: 0, reason: answer.reason });
  }
  const c = counts();
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, outbox, { c })
    .execute(resumeClaim(claim, { ...last, original_head: "e".repeat(40) }, {}));
  assert.deepEqual(c, counts());
  return { claim, outbox };
}

it("an applied hold keeps the record and the source and sends nothing else", async () => {
  const { claim, outbox } = await heldFlight(5330);
  assert.equal(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).length, 1);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(completed(claim.run_id).length, 0);
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "the record survives the hold");
});

it("a stale-claim ACK to the hold stops the flight: no further report, no failure", async () => {
  const { claim } = await heldFlight(5331, { httpStatus: 409, reason: "stale_claim" });
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(completed(claim.run_id).length, 0);
});

for (const reason of ["publication_not_enrolled", "publication_enrolled"]) {
  it(`a ${reason} refusal of the hold retains the work: no failure, no release, record kept`, async () => {
    const released: unknown[] = [];
    const realRelease = client.releaseRecoveryCustody.bind(client);
    client.releaseRecoveryCustody = async (...a: Parameters<typeof client.releaseRecoveryCustody>) => { released.push(a); return realRelease(...a); };
    const { claim, outbox } = await heldFlight(5332 + (reason === "publication_enrolled" ? 1 : 0), { httpStatus: 409, reason });
    assert.equal(failed(claim.run_id).length, 0);
    assert.equal(completed(claim.run_id).length, 0);
    assert.equal(released.length, 0, "no source release");
    assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok");
  });
}

// ── the re-park of a resumed publication ─────────────────────────────────────────────────────────────

it("a forge outage during the resumed publication re-parks through the publication park, still with zero model calls", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5340, outbox, { at: "mr_create" });
  reset();
  const c = counts();
  const down = fakeGitlab({ intercept: (req) => req.method === "POST" ? { status: 503, text: async () => "down" } : undefined });
  await build(tripwireExecutor(c), { gitlab: down.gitlab }, outbox, { c }).execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  const second = parks(claim.run_id).filter(b => b.claim_generation === GEN + 1);
  assert.equal(second.length, 1, "parked again by the resumed flight");
  assert.deepEqual([second[0]!.recovery_phase, second[0]!.publication_surface, second[0]!.publication_step, second[0]!.publication_hold_reason],
    ["publication", "api", "mr_create", undefined]);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "the record survives the second park");
});

// ── the secret remediation state survives the resume ────────────────────────────────────────────────

/** The enrolled intent with a hand-set secret remediation, as a claim from a worker that knew a finding. */
const withRemediation = (intent: WireIntent, over: Record<string, unknown>): WireIntent => ({
  ...intent, guard: { ...intent.guard, secret_remediation: { ...(intent.guard.secret_remediation as Record<string, unknown>), ...over } },
});

it("a secret finding flagged before the park still blocks the resumed publication (push_secret_blocked, no push)", async () => {
  const { claim, last } = await enrolAndPark(5350, await mkOutbox());
  reset();
  const c = counts();
  const forge = fakeGitlab();
  const flagged = withRemediation(last, {
    ever_known: true, flagged_commits: [last.candidate],
    flagged_findings: [{ start_line: 3, commit: last.candidate, rule_id: "generic-api-key" }],
  });
  // A fresh outbox: the worker lost its record, so the api's intent is what the resume continues from.
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, flagged));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5350), undefined, "the flagged candidate was not pushed");
  assert.equal(forge.calls.length, 0);
  const terminal = failed(claim.run_id);
  assert.equal(terminal.length, 1);
  assert.equal(terminal[0]!.fail_origin, "push_secret_blocked");
});

it("a recorded clean_tip never skips the scan: an ever-known run is re-scanned at the resume", async () => {
  const { claim, last } = await enrolAndPark(5351, await mkOutbox());
  reset();
  const c = counts();
  const claimed = withRemediation(last, { ever_known: true, clean_tip: last.candidate });
  const r = build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c });
  const rescans: string[] = [];
  const seam = r as unknown as { scanRemediationRange: (...a: unknown[]) => Promise<unknown> };
  const real = seam.scanRemediationRange.bind(r);
  seam.scanRemediationRange = async (...a) => { rescans.push("scan"); return real(...a); };
  await r.execute(resumeClaim(claim, claimed));
  assert.deepEqual(c, counts());
  assert.deepEqual(rescans, ["scan"], "the branch was re-scanned although the record said it was clean");
  assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
});

// ── the guards the live claim and the record both set ───────────────────────────────────────────────

it("a recorded CI-config guard stays on when the live claim no longer asks for it", async () => {
  const { claim, last } = await enrolAndPark(5352, await mkOutbox(), { github: true, transformed: true });
  reset();
  // The recorded guard protects workflow files; this branch's commits include the workflow overlay-free change set.
  const guarded = { ...last, guard: { ...last.guard, ci_config_guard_globs: ["impl.ts"] } };
  const c = counts();
  const forge = fakeGitHub();
  await build(tripwireExecutor(c), { github: forge.github }, await mkOutbox(), { c })
    .execute(resumeClaim(claim, guarded, {}, { enrollment_seq: 1, candidate_seq: 2 }));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5352), undefined, "the recorded guard refused the candidate that changes impl.ts");
  assert.equal(forge.calls.filter(x => x.method === "POST").length, 0);
  assert.equal(failed(claim.run_id).length, 1);
});

// ── the recorded closing set only tightens ──────────────────────────────────────────────────────────

/** The merge request description the resumed flight wrote: the create body, then every later write. */
const writtenBodies = (forge: { calls: Array<{ body?: string }>; pr: { description: string } }): string =>
  [...forge.calls.map(x => x.body ?? ""), forge.pr.description].join("\n");

it("a lead's non-closing declaration (no interlock) survives the resume: the merge request never closes the issue", async () => {
  const { claim, last } = await enrolAndPark(5399, await mkOutbox(), { result: { nonClosing: { reason: "partial by design" } } });
  assert.deepEqual(last.mr.closing, [], "the original flight recorded no closing issue");
  reset();
  const c = counts();
  const forge = fakeGitlab({ head: last.candidate });
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
  assert.ok(!/Closes #5399/.test(writtenBodies(forge)), `no closing directive: ${writtenBodies(forge)}`);
});

it("control: an ordinary full delivery still closes its issue on the resume", async () => {
  const { claim, last } = await enrolAndPark(5398, await mkOutbox());
  assert.deepEqual(last.mr.closing, ["#5398"]);
  reset();
  const c = counts();
  const forge = fakeGitlab({ head: last.candidate });
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last));
  assert.match(writtenBodies(forge), /Closes #5398/);
});

it("a scope-capped park is still recognised when the issue was retitled before the resume", async () => {
  const { claim, last } = await enrolAndPark(5397, await mkOutbox(), { result: { scopeCapped: { completedCount: 1, total: 3 } } });
  assert.ok(String(last.mr.title).startsWith("[partial] "));
  reset();
  const c = counts();
  const forge = fakeGitlab({ head: last.candidate });
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last, { issue_title: "A completely different title" }));
  assert.deepEqual(c, counts());
  assert.deepEqual(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).map(h => h.publication_hold_reason), ["publication_model_required"]);
  assert.equal(forge.calls.length, 0);
});

for (const [name, enrolTitle, resumeTitle] of [
  ["a capped run on an issue already titled with the partial prefix", "[partial] foo", "[partial] foo"],
  ["a capped run whose issue was retitled to start with the partial prefix", "Fix thing", "[partial] other"],
] as const) {
  it(`${name} still holds as scope-capped`, async () => {
    const { claim, last } = await enrolAndPark(5395, await mkOutbox(), {
      claim: { issue_title: enrolTitle }, result: { scopeCapped: { completedCount: 1, total: 3 } },
    });
    reset();
    const c = counts();
    const forge = fakeGitlab({ head: last.candidate });
    await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last, { issue_title: resumeTitle }));
    assert.deepEqual(c, counts());
    assert.deepEqual(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).map(h => h.publication_hold_reason), ["publication_model_required"]);
    assert.equal(forge.calls.length, 0);
  });
}

it("an issue whose own title starts with the partial prefix is not mistaken for a scope cap", async () => {
  const { claim, last } = await enrolAndPark(5396, await mkOutbox(), { claim: { issue_title: "[partial] a real title" } });
  reset();
  const c = counts();
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last));
  assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
});

for (const [name, config] of [
  ["a live claim.config.non_closing", { non_closing: { reason: "owner declared" } }],
  ["a live owner deferral", { completion_scope: { deferred: [{ milestone_id: "m2", title: "later", reason: "owner" }] } }],
] as const) {
  it(`the live claim wins over a recorded closing set: ${name} delivers non-closing`, async () => {
    const iid = name.includes("deferral") ? 5395 : 5394;
    const { claim, last } = await enrolAndPark(iid, await mkOutbox());
    assert.deepEqual(last.mr.closing, [`#${iid}`], "the record lists the issue as closing");
    reset();
    const c = counts();
    const forge = fakeGitlab({ head: last.candidate });
    await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, last, { config }));
    assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
    assert.ok(!new RegExp(`Closes #${iid}`).test(writtenBodies(forge)), writtenBodies(forge));
  });
}

// ── plan test-list gaps ─────────────────────────────────────────────────────────────────────────────

it("a superseded local record: the server's intent is adopted, never the old local identity, and nothing is re-enrolled", async () => {
  const first = await enrolAndPark(5393, await mkOutbox());
  reset();
  const stale = await mkOutbox();
  // An older enrollment of this run, with a candidate this worker never had: adopting it would be unpublishable.
  await seedLocal(stale, { ...first.last, candidate: "9".repeat(40) }, { enrollment_seq: 7, candidate_seq: 1 });
  const c = counts();
  await build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, stale, { c }).execute(resumeClaim(first.claim, first.last));
  assert.deepEqual(c, counts());
  assert.equal(api.publicationIntentRequests.length, 0, "no enrollment and no resend of the old identity");
  assert.equal(completed(first.claim.run_id).length, 1, JSON.stringify(states(first.claim.run_id).map(s => s.status)));
  assert.equal(originBranch(5393), first.last.candidate);
});

it("a failed capture on the re-park is retried until it is verified; the park is sent once, after it, and nothing fails", async () => {
  const outbox = await mkOutbox();
  const { claim, last } = await enrolAndPark(5392, outbox, { at: "mr_create" });
  reset();
  const c = counts();
  const down = fakeGitlab({ intercept: (req) => req.method === "POST" ? { status: 503, text: async () => "down" } : undefined });
  const r = build(tripwireExecutor(c), { gitlab: down.gitlab }, outbox, { c });
  const seam = r as unknown as { captureRecoveryRestorePoint: (...a: unknown[]) => Promise<unknown> };
  const real = seam.captureRecoveryRestorePoint.bind(r);
  let attempts = 0;
  seam.captureRecoveryRestorePoint = async (...a) => {
    attempts++;
    if (attempts <= 2) return { verified: false, published: false };
    return real(...a);
  };
  const before = parks(claim.run_id).length;
  await r.execute(resumeClaim(claim, last));
  assert.deepEqual(c, counts());
  assert.ok(attempts >= 3, `the capture was retried: ${attempts}`);
  assert.equal(parks(claim.run_id).length - before, 1, "exactly one park, after the capture verified");
  assert.equal(failed(claim.run_id).length, 0);
});

it("a secret finding known at the recorded floor is treated the same after the resume (blocked, not pushed)", async () => {
  const { claim, last } = await enrolAndPark(5391, await mkOutbox());
  reset();
  const c = counts();
  const forge = fakeGitlab();
  // The finding sits AT the recorded floor, as an at-floor finding the original gate could not remediate.
  const floor = last.original_head;
  const atFloor = withRemediation({ ...last, floor: { kind: "pinned", oid: floor } }, {
    ever_known: true, known: [{ start_line: 1, commit: floor, rule_id: "generic-api-key" }],
    flagged_commits: [floor], flagged_findings: [{ start_line: 1, commit: floor, rule_id: "generic-api-key" }],
  });
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c }).execute(resumeClaim(claim, atFloor));
  assert.deepEqual(c, counts());
  assert.equal(originBranch(5391), undefined);
  assert.equal(forge.calls.length, 0);
  assert.equal(failed(claim.run_id)[0]?.fail_origin, "push_secret_blocked");
});

it("a required code cross-check is disclosed as incomplete instead of silently dropped", async () => {
  const { claim, last } = await enrolAndPark(5390, await mkOutbox());
  reset();
  const c = counts();
  const forge = fakeGitlab();
  await build(tripwireExecutor(c), { gitlab: forge.gitlab }, await mkOutbox(), { c })
    .execute(resumeClaim(claim, last, { code_cross_check_required: true }));
  assert.equal(completed(claim.run_id).length, 1, JSON.stringify(states(claim.run_id).map(s => s.status === "failed" ? `failed:${s.failure_reason}` : s.status)));
  assert.match(writtenBodies(forge), /status unavailable|incomplete/i);
});

it("a flight with no branch is refused as an intent mismatch instead of skipping the branch check", async () => {
  const { claim, last } = await enrolAndPark(5389, await mkOutbox());
  reset();
  const c = counts();
  const r = build(tripwireExecutor(c), { gitlab: fakeGitlab().gitlab }, await mkOutbox(), { c });
  const seam = r as unknown as { phasePublicationOnly: (claim: ClaimResponse, flight: { branch?: string }) => Promise<unknown> };
  const real = seam.phasePublicationOnly.bind(r);
  seam.phasePublicationOnly = async (cl, flight) => { flight.branch = undefined; return real(cl, flight); };
  await r.execute(resumeClaim(claim, last));
  assert.deepEqual(parks(claim.run_id).filter(b => b.publication_hold_reason !== undefined).map(h => h.publication_hold_reason), ["publication_intent_mismatch"]);
});

for (const mode of ["clean", "empty commit", "committed work", "dirty", "untracked", "wrong capture run", "wrong capture generation", "wrong capture inventory", "missing capture", "wrong enrollment", "no coverage pin", "hold refused", "hold unavailable"] as const) {
  it(`M3 guarded retained publication: ${mode}`, async t => {
    const iid = 5388, outbox = await mkOutbox();
    const { claim, last } = await enrolAndPark(iid, outbox);
    const bare = git.barePathFor(fx.originPath), branch = `agent/issue-${iid}`;
    const retained = JSON.parse(gitIn(bare, ["config", "--get", `uzi-recovery.${branch}.clone`])) as { clonePath: string };
    assert.ok(retained);
    assert.equal(gitIn(retained.clonePath, ["rev-parse", "HEAD"]), last.candidate);
    assert.equal(gitIn(retained.clonePath, ["status", "--porcelain"]), "");
    if (mode === "empty commit") gitIn(retained.clonePath, [...IDENT, "commit", "--allow-empty", "-m", "genuine empty work"]);
    if (mode === "committed work" || mode === "dirty" || mode === "untracked") {
      fs.writeFileSync(path.join(retained.clonePath, "extra.txt"), "genuine extra work\n");
      if (mode !== "untracked") gitIn(retained.clonePath, ["add", "extra.txt"]);
      if (mode === "committed work") gitIn(retained.clonePath, [...IDENT, "commit", "-m", "extra work"]);
    }
    const laterHead = gitIn(retained.clonePath, ["rev-parse", "HEAD"]);
    const archive = Object.assign(new FakeRecoveryClient(), { hasFeature: (name: string) => name === "recovery_inventory_v1", getRunOwnership: (runId: string) => client.getRunOwnership(runId) });
    archive.holds = [{ generation: GEN + 1, inventory_guarded: true, hold_id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd" }] as never;
    const recovery = new RecoveryCoordinator({ client: archive, git, log: nullLogger(),
      recoveryRoot: git.recoveryRoot, workerToken: "publication-resume-worker-fixture" });
    const verify = recovery.verifiedLocalCapture.bind(recovery);
    recovery.verifiedLocalCapture = async (...args) => {
      const local = await verify(...args);
      if (!local || mode === "missing capture") return undefined;
      return { ...local, ...(["wrong capture run", "hold refused", "hold unavailable"].includes(mode) ? { runId: "other-run" } : {}),
        ...(mode === "wrong capture generation" ? { generation: 999 } : {}),
        ...(mode === "wrong capture inventory" ? { inventoryCurrentSha: "e".repeat(40) } : {}) };
    };
    let wrapper: string | undefined;
    const record = git.recordRecoveryCapture.bind(git);
    git.recordRecoveryCapture = async (...args) => {
      wrapper = args[5];
      if (mode === "clean" || mode === "wrong enrollment") assert.equal(args[6], last.candidate, "authenticated inventory maps wrapper to exact candidate");
      assert.notEqual(wrapper, last.candidate);
      if (mode === "clean") {
        assert.equal(gitIn(bare, ["rev-parse", `${wrapper}^{tree}`]), gitIn(bare, ["rev-parse", `${last.candidate}^{tree}`]));
        assert.equal(gitIn(bare, ["rev-list", `${last.candidate}..${wrapper}`]).split("\n").length, 1);
      }
      assert.match(gitIn(bare, ["log", "-1", "--format=%s", wrapper!]), /^uzi recovery coverage /);
      t.diagnostic(JSON.stringify({ candidate: last.candidate, wrapper, source: args[6] }));
      await record(...args);
      if (mode === "no coverage pin") {
        for (const ref of gitIn(bare, ["for-each-ref", "--format=%(refname)", `refs/uzi-coverage/${claim.run_id}/`]).split("\n")) {
          gitIn(bare, ["update-ref", "-d", ref]);
        }
      }
    };
    let preparations = 0;
    const prepare = git.prepareRecoverySuccessor.bind(git);
    git.prepareRecoverySuccessor = async (...args) => { preparations++; return prepare(...args); };
    let holdAttempts = 0;
    if (mode === "hold refused" || mode === "hold unavailable") {
      const report = client.reportState.bind(client);
      t.mock.method(client, "reportState", async (...args: Parameters<typeof client.reportState>) => {
        if (args[0] === claim.run_id && args[1].publication_hold_reason !== undefined) {
          holdAttempts++;
          if (mode === "hold unavailable") throw new Error("fixture hold report unavailable");
          return { status: "running", reason: "publication_not_enrolled" };
        }
        return report(...args);
      });
    }
    reset();
    const c = counts(), forge = fakeGitlab();
    await build(tripwireExecutor(c), { gitlab: forge.gitlab }, outbox, { c, extra: { recovery }, log: { ...nullLogger(), warn: (msg, fields) => t.diagnostic(JSON.stringify({ msg, fields })) } })
      .execute(resumeClaim(claim, mode === "wrong enrollment" ? { ...last, v: 999 } : last, { inventory_guarded: true }));
    assert.deepEqual(c, counts());
    t.diagnostic(JSON.stringify(states(claim.run_id)));
    if (mode !== "clean") {
      assert.equal(completed(claim.run_id).length, 0);
      assert.equal(failed(claim.run_id).length, 0, "publication refusals never become terminal failures");
      const holds = parks(claim.run_id).filter(s => s.claim_generation === GEN + 1);
      const reason = mode === "wrong enrollment" ? "publication_intent_mismatch"
        : ["empty commit", "committed work", "dirty", "untracked"].includes(mode) ? "publication_model_required"
        : "publication_source_unavailable";
      if (mode === "hold refused" || mode === "hold unavailable") {
        assert.equal(holdAttempts, 1);
        assert.equal(holds.length, 0, "unconfirmed holds retain without claiming a park");
      } else {
        assert.equal(holds.length, 1, "this resume, not its earlier flight, holds for its owner");
        assert.equal(holds[0]!.recovery_phase, "publication");
        assert.equal(holds[0]!.publication_hold_reason, reason);
      }
      assert.equal(preparations, mode === "no coverage pin" ? 1 : 0, "eligibility refusals precede successor preparation");
      assert.equal(originBranch(iid), undefined);
      assert.equal(forge.calls.length, 0);
      assert.ok(fs.existsSync(retained.clonePath), "source preserved");
      if (["committed work", "dirty", "untracked"].includes(mode)) {
        assert.equal(fs.readFileSync(path.join(retained.clonePath, "extra.txt"), "utf8"), "genuine extra work\n");
      }
      assert.equal(archive.releaseCalls.length, 0, "custody preserved");
      assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "publication record preserved");
      assert.equal(gitIn(retained.clonePath, ["merge-base", "--is-ancestor", laterHead, "HEAD"]), "", "later commits remain reachable");
      if (mode === "no coverage pin") {
        const journal = JSON.parse(gitIn(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]));
        assert.equal(journal.recovery.stage, "captured", "verifier refusal never adopts a successor");
      }
      return;
    }
    assert.ok(wrapper, "production retained capture ran");
    assert.equal(completed(claim.run_id).length, 1, "exact candidate must complete publication");
    assert.equal(originBranch(iid), last.candidate);
    assert.equal(gitIn(fx.originPath, ["rev-list", branch]).split("\n").includes(wrapper!), false);
    assert.equal(forge.calls.filter(q => q.method === "POST").length, 1, "one MR");
  });
}
