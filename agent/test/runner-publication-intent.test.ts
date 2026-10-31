import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { FORGE_RETRY_SCHEDULE } from "../src/forge-retry.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
import { PUBLICATION_RESUME_UNAVAILABLE } from "../src/publication-intent.js";
import { WorkerClient } from "../src/client.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { api, baseUrl, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, TOKEN } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

// Issue #1324 M4b: the first-run finalize write-ahead and the publication-phase park, end to end through
// RunRunner with a real Outbox, a real git origin and the fake api (POST /publication-intent is scripted).

installHarness();

const REPO_ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const GEN = 4;
const ATTEMPTS = FORGE_RETRY_SCHEDULE.length + 1;
const FEATURES = ["publication_resume_v1", "recovery_park_cause"];

const roots: string[] = [];
afterEach(async () => {
  for (const r of roots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(rawWrite?: RawWriteSeam): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-publication-"));
  roots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000, ...(rawWrite ? { rawWrite } : {}),
  });
  await outbox.init();
  return outbox;
}

const claimFor = (iid: number, over: Record<string, unknown> = {}) =>
  gitlabClaim(iid, { claim_generation: GEN, repo: { id: REPO_ID, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath }, ...over });

/** Wrap the remote-capable git entry points so the call order is observable. */
function observeRemote(events: string[]): void {
  const g = git as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  for (const name of ["pushBranch", "fetchDefaultTip", "fetchWorkflowTargetTip"]) {
    const real = g[name]!.bind(git);
    g[name] = async (...a) => { events.push(name); return real(...a); };
  }
}

/** Ack every intent request: an enrollment is (1,1); an advance echoes the sequence numbers it carries. */
function ackIntents(events: string[], outbox?: Outbox, localAtAck?: boolean[]): void {
  api.publicationIntentWire = (req) => {
    const advance = req.body.enrollment_seq !== undefined;
    events.push(advance ? "intent:advance" : "intent:enroll");
    if (outbox && localAtAck) localAtAck.push(outbox.hasPublicationRecords(req.runId));
    return { status: 200, body: {
      enrollment_seq: (req.body.enrollment_seq as number | undefined) ?? 1,
      candidate_seq: (req.body.candidate_seq as number | undefined) ?? 1,
      state: "active",
    } };
  };
}

function build(executor: Executor, forge: { gitlab?: unknown; github?: unknown }, outbox: Outbox | undefined, log = nullLogger(), extra: Record<string, unknown> = {}): RunRunner {
  const r = new RunRunner(client, git, () => ({ executor }), log, 20, undefined, {
    pollMs: 5, planApprovalTimeoutMs: 0, prDescriptionHeadLagMs: 0, recoveryRetryMs: 5,
    checkpointTestHooks: { retryBackoffMs: [0, 0] },
    ...(forge.gitlab ? { gitlab: forge.gitlab as never } : {}), ...(forge.github ? { github: forge.github as never } : {}),
    ...(outbox ? { outbox } : {}), ...extra,
  });
  const seams = r as unknown as { forgeRetrySleep: (ms: number) => Promise<void>; publicationRetry: unknown };
  seams.forgeRetrySleep = async () => {};
  seams.publicationRetry = { retryDelaysMs: [0, 0, 0], sleep: async () => {} };
  return r;
}

const states = (runId: string) => api.states.filter(s => s.runId === runId).map(s => s.body);
const parks = (runId: string) => states(runId).filter(b => b.status === "recovery_wait");
const failed = (runId: string) => states(runId).filter(b => b.status === "failed");

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

type ExecSeam = { execScoped(cmd: string, args: string[], ...rest: unknown[]): Promise<unknown> };
function failGit(sub: string, stderr: string, onHit: () => void): void {
  const seam = git as unknown as ExecSeam;
  const real = seam.execScoped.bind(git);
  seam.execScoped = async (cmd, args, ...rest) => {
    if (cmd === "git" && args.includes(sub)) {
      onHit();
      throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr });
    }
    return real(cmd, args, ...rest);
  };
}

/** A logger that records the messages it receives. */
function recordingLogger(): { log: Logger; lines: Array<{ level: string; msg: string; fields?: Record<string, unknown> }> } {
  const lines: Array<{ level: string; msg: string; fields?: Record<string, unknown> }> = [];
  const base = nullLogger();
  const log: Logger = {
    ...base,
    info(msg: string, fields?: Record<string, unknown>) { lines.push({ level: "info", msg, fields }); },
    warn(msg: string, fields?: Record<string, unknown>) { lines.push({ level: "warn", msg, fields }); },
    child() { return log; },
  };
  return { log, lines };
}

// ── D2 ordering and the fallback matrix ──────────────────────────────────────────────────────────────

it("enrolls (local record, then api ack) before the first remote git or forge step", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  const localAtAck: boolean[] = [];
  ackIntents(events, outbox, localAtAck);
  observeRemote(events);
  const forge = fakeGitHub({ intercept: (req) => { events.push(`forge:${req.method}`); return undefined; } });
  const claim = claimFor(5101, { repo: { id: REPO_ID, url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" } });
  const r = build(new StubExecutor(nullLogger()), { github: forge.github }, outbox);
  await r.execute(claim);

  assert.equal(events[0], "intent:enroll", `the acknowledged enrollment precedes every remote step: ${events.join(",")}`);
  assert.deepEqual(localAtAck, [true], "the local record was already durable when the api was asked");
  assert.ok(events.includes("fetchDefaultTip") && events.includes("pushBranch"), "the ordinary finalize ran afterwards");
  const req = api.publicationIntentRequests[0]!.body as { intent: { original_head: string; branch: string; floor: unknown }; claim_generation: number };
  assert.equal(req.claim_generation, GEN);
  assert.match(req.intent.original_head, /^[0-9a-f]{40}$/);
  assert.equal(req.intent.branch, "agent/issue-5101");
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
  // The completion was acknowledged, so the local record is discarded.
  assert.equal(outbox.hasPublicationRecords(claim.run_id), false, "completion discards the record");
});

it("without the advertised feature the run finalizes ordinarily and logs the unavailable diagnostic", async () => {
  client.protocolFeatures = ["recovery_park_cause"];
  const outbox = await mkOutbox();
  const rec = recordingLogger();
  const claim = claimFor(5102);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox, rec.log);
  await r.execute(claim);
  assert.equal(api.publicationIntentRequests.length, 0);
  assert.equal(outbox.hasPublicationRecords(claim.run_id), false);
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
  const diag = rec.lines.find(l => l.msg === PUBLICATION_RESUME_UNAVAILABLE);
  assert.equal(diag?.fields?.reason, "server_feature_unavailable");
});

it("a known not-enrolled outcome (local ENOSPC) still publishes ordinarily and says no guarantee is claimed", async () => {
  client.protocolFeatures = FEATURES;
  const events: string[] = [];
  ackIntents(events);
  const outbox = await mkOutbox(async (write, ctx) => {
    if (ctx.kind === "publication") throw Object.assign(new Error("no space left"), { code: "ENOSPC" });
    return write();
  });
  const claim = claimFor(5103);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  await r.execute(claim);
  assert.equal(api.publicationIntentRequests.length, 0, "nothing reached the api: the write failed first");
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
  assert.equal(failed(claim.run_id).length, 0);
  const text = api.messages(claim.run_id).filter(m => m.kind === "status").map(m => String(m.payload.text));
  assert.ok(text.some(t => t.startsWith(`${PUBLICATION_RESUME_UNAVAILABLE}: enospc`) && t.includes("without the resume guarantee")), text.join(" | "));
});

it("an unknown enrollment outcome (lost ACK) makes no fetch, push or MR, keeps the record and does not fail the run", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  api.publicationIntentWire = () => "drop";
  observeRemote(events);
  const forge = fakeGitlab({ intercept: (req) => { events.push(`forge:${req.method}`); return undefined; } });
  const claim = claimFor(5104);
  const r = build(new StubExecutor(nullLogger()), { gitlab: forge.gitlab }, outbox);
  await r.execute(claim);

  assert.equal(api.publicationIntentRequests.length, 3, "bounded idempotent retries only");
  assert.deepEqual(events, [], "no remote step ran on an unknown outcome");
  assert.equal(forge.all.length, 0);
  const loaded = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(loaded.kind, "ok", "the local record is retained");
  assert.equal(failed(claim.run_id).length, 0, "no false failure");
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 0);
  assert.equal(parks(claim.run_id).length, 0, "no guessed park either");
});

// ── D2 transformed candidates ────────────────────────────────────────────────────────────────────────

const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

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
const githubClaim = (iid: number) =>
  claimFor(iid, { repo: { id: REPO_ID, url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" } });

it("persists and acknowledges a transformed candidate before the push that uses it", async () => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  const seqAtAck: Array<number | undefined> = [];
  ackIntents(events);
  const wire = api.publicationIntentWire!;
  api.publicationIntentWire = async (req) => {
    const reply = await wire(req);
    if (req.body.enrollment_seq !== undefined) {
      const loaded = await outbox.loadPublicationRecord(req.runId, GEN);
      seqAtAck.push(loaded.kind === "ok" ? loaded.record.candidate_seq : undefined);
    }
    return reply;
  };
  observeRemote(events);
  const claim = githubClaim(5105);
  const r = build(advancingExecutor(), { github: fakeGitHub().github }, outbox);
  await r.execute(claim);

  const advance = events.indexOf("intent:advance");
  assert.ok(advance > 0, `a candidate advance happened: ${events.join(",")}`);
  assert.ok(advance < events.indexOf("pushBranch"), "C is acknowledged before the push that uses it");
  assert.deepEqual(seqAtAck, [2], "C was already saved locally (candidate_seq 2) when the api was asked");
  const sent = api.publicationIntentRequests[1]!.body as { candidate_seq: number; intent: { candidate: string; original_head: string } };
  assert.equal(sent.candidate_seq, 2);
  assert.notEqual(sent.intent.candidate, sent.intent.original_head, "the transformed candidate differs from H");
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
});

it("an unconfirmed transformed candidate is never pushed and does not fail the run", async () => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  ackIntents(events);
  const wire = api.publicationIntentWire!;
  api.publicationIntentWire = (req) => req.body.enrollment_seq !== undefined ? "drop" : wire(req);
  observeRemote(events);
  const claim = githubClaim(5106);
  const r = build(advancingExecutor(), { github: fakeGitHub().github }, outbox);
  await r.execute(claim);

  assert.ok(!events.includes("pushBranch"), `no push with an unconfirmed candidate: ${events.join(",")}`);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 0);
  const loaded = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(loaded.kind, "ok", "the record survives");
});

it("a failed local candidate save sends nothing to the api and pushes nothing", async () => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  client.protocolFeatures = FEATURES;
  // Local publication writes work through the enrollment, then every later one fails.
  let enrolled = false;
  const outbox = await mkOutbox(async (write, ctx) => {
    if (ctx.kind === "publication" && enrolled) throw Object.assign(new Error("io"), { code: "EIO" });
    return write();
  });
  const events: string[] = [];
  ackIntents(events);
  // The push proof computes the transformed candidate; from here on the local disk refuses writes.
  const preflight = git.scratchPublicationPreflight.bind(git);
  git.scratchPublicationPreflight = async (...args) => { const c = await preflight(...args); enrolled = true; return c; };
  observeRemote(events);
  const claim = githubClaim(5107);
  const r = build(advancingExecutor(), { github: fakeGitHub().github }, outbox);
  await r.execute(claim);
  assert.deepEqual(events.filter(e => e.startsWith("intent:")), ["intent:enroll"], "the api is asked only after the local save");
  assert.ok(!events.includes("pushBranch"), events.join(","));
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "the previous record is untouched");
});

// ── D3 / D5 the publication-phase park ───────────────────────────────────────────────────────────────

it("an enrolled MR-create outage captures first, then parks in the publication phase without a release", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  ackIntents(events);
  const claim = claimFor(5108);
  api.onState(claim.run_id, (b) => { if (b.status === "recovery_wait") events.push("park"); });
  let creates = 0;
  const forge = fakeGitlab({ intercept: (req) => { if (req.method !== "POST") return undefined; creates++; return { status: 503, text: async () => "down" }; } });
  const released: unknown[] = [];
  const realRelease = client.releaseRecoveryCustody.bind(client);
  client.releaseRecoveryCustody = async (...a: Parameters<WorkerClient["releaseRecoveryCustody"]>) => { released.push(a); return realRelease(...a); };
  const r = build(new StubExecutor(nullLogger()), { gitlab: forge.gitlab }, outbox);
  const proto = r as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  const capture = proto["captureRecoveryRestorePoint"]!.bind(r);
  proto["captureRecoveryRestorePoint"] = async (...a) => { events.push("capture"); return capture(...a); };
  await r.execute(claim);

  assert.equal(creates, ATTEMPTS);
  assert.ok(events.indexOf("capture") >= 0 && events.indexOf("capture") < events.indexOf("park"), `capture before the park report: ${events.join(",")}`);
  const park = parks(claim.run_id);
  assert.equal(park.length, 1);
  assert.deepEqual(
    [park[0]!.recovery_cause, park[0]!.recovery_phase, park[0]!.publication_surface, park[0]!.publication_step, park[0]!.claim_generation],
    ["forge_unreachable", "publication", "api", "mr_create", GEN]);
  assert.equal(park[0]!.handoff_checkpoint_tip, undefined, "no broker-confirmed checkpoint, so no handoff (custodian-only)");
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(released.length, 0, "no no_adopted_source style release");
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "the record survives the park");
});

it("the park carries handoff_checkpoint_tip when the broker confirmed the candidate", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  // The broker accepts the pack and confirms exactly the tip the worker declared.
  client.publishCheckpoint = async (_runId, tipOid, pack) => {
    for await (const chunk of pack) void chunk;
    return { ok: true, body: { published: true, checkpoint_tip: tipOid } } as never;
  };
  const claim = claimFor(5116);
  const forge = fakeGitlab({ intercept: (req) => req.method === "POST" ? { status: 503, text: async () => "down" } : undefined });
  const r = build(new StubExecutor(nullLogger()), { gitlab: forge.gitlab }, outbox);
  await r.execute(claim);
  const park = parks(claim.run_id);
  assert.equal(park.length, 1);
  const record = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(record.kind, "ok");
  const candidate = record.kind === "ok" ? record.record.candidate : undefined;
  assert.match(String(park[0]!.handoff_checkpoint_tip), /^[0-9a-f]{40}$/);
  assert.equal(park[0]!.handoff_checkpoint_tip, candidate, "the handoff tip is exactly the candidate");
  assert.equal(failed(claim.run_id).length, 0);
});

it("an enrolled push outage parks on the git surface", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  let pushes = 0;
  failGit("push", "fatal: unable to access: connection reset by peer", () => { pushes++; });
  const claim = claimFor(5109);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS);
  const park = parks(claim.run_id);
  assert.equal(park.length, 1);
  assert.deepEqual([park[0]!.recovery_phase, park[0]!.publication_surface, park[0]!.publication_step], ["publication", "git", "push"]);
  assert.equal(failed(claim.run_id).length, 0);
});

it("a non-enrolled run keeps the existing typed failure for the same outage", async () => {
  client.protocolFeatures = ["recovery_park_cause"];
  failGit("push", "fatal: unable to access: connection reset by peer", () => {});
  const claim = claimFor(5110);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, await mkOutbox());
  await r.execute(claim);
  assert.equal(parks(claim.run_id).length, 0);
  assert.match(String(failed(claim.run_id)[0]?.failure_reason), /publication_forge_unreachable: push/);
});

it("a lost park ACK is reconciled through the ownership probe: parked once, record kept, no failure", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  failGit("push", "fatal: unable to access: connection reset by peer", () => {});
  const claim = claimFor(5111);
  // The park is persisted, then its ACK is lost; the idempotent resend lands on a run that is already parked.
  api.afterPersistState(claim.run_id, (b) => b.status === "recovery_wait", "drop");
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  await r.execute(claim);
  assert.ok(parks(claim.run_id).length >= 1);
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok");
});

for (const reason of ["publication_enrolled", "publication_not_enrolled"]) {
  it(`a ${reason} refusal retains the work: no failure, no release, record kept`, async () => {
    client.protocolFeatures = FEATURES;
    const outbox = await mkOutbox();
    ackIntents([]);
    failGit("push", "fatal: unable to access: connection reset by peer", () => {});
    const claim = claimFor(5112);
    api.answerStateOnce(claim.run_id, (b) => b.status === "recovery_wait", { httpStatus: 409, runStatus: "running", gateRevision: 0, reason });
    let released = 0;
    const realRelease = client.releaseRecoveryCustody.bind(client);
    client.releaseRecoveryCustody = async (...a: Parameters<WorkerClient["releaseRecoveryCustody"]>) => { released++; return realRelease(...a); };
    const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
    await r.execute(claim);
    assert.equal(parks(claim.run_id).length, 0, "the refused park was not recorded");
    assert.equal(failed(claim.run_id).length, 0);
    assert.equal(released, 0);
    assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok");
  });
}

it("handoff_checkpoint_tip is sent only when the confirmed checkpoint is the candidate and H and the floor are its ancestors", async () => {
  type Handoff = (flight: unknown) => Promise<string | undefined>;
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, undefined);
  const handoff = (r as unknown as { publicationHandoffTip: Handoff }).publicationHandoffTip.bind(r);
  // A tiny repo: A <- B <- C, and an unrelated D.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "handoff-"));
  roots.push(dir);
  gitIn(dir, ["init", "-q", "--bare"]);
  const work = path.join(dir, "w");
  execFileSync("git", ["init", "-q", work], { env: ENV });
  const commit = (m: string, parent?: string) => {
    fs.writeFileSync(path.join(work, "f"), m);
    execFileSync("git", ["-C", work, "add", "."], { env: ENV });
    if (parent === undefined) execFileSync("git", ["-C", work, "checkout", "-q", "--orphan", `o${m}`], { env: ENV });
    execFileSync("git", ["-C", work, ...IDENT, "commit", "-q", "-m", m], { env: ENV });
    return gitIn(work, ["rev-parse", "HEAD"]);
  };
  const A = commit("a");
  const B = commit("b", A);
  const C = commit("c", B);
  gitIn(work, ["checkout", "-q", "--orphan", "other"]);
  const D = commit("d");
  gitIn(work, ["push", "-q", dir, "--all"]);
  const flight = (over: Record<string, unknown>) => ({
    barePath: dir, lastCheckpointRefTip: C,
    publicationRecord: { candidate: C, original_head: B, floor: { kind: "pinned", oid: A } }, ...over,
  });
  assert.equal(await handoff(flight({})), C, "all conditions hold");
  assert.equal(await handoff(flight({ publicationRecord: { candidate: C, original_head: B, floor: { kind: "absent" } } })), C, "an absent floor is allowed");
  assert.equal(await handoff(flight({ lastCheckpointRefTip: B })), undefined, "the confirmed checkpoint is not the candidate");
  assert.equal(await handoff(flight({ lastCheckpointRefTip: undefined })), undefined, "no confirmed checkpoint");
  assert.equal(await handoff(flight({ publicationRecord: { candidate: C, original_head: D, floor: { kind: "pinned", oid: A } } })), undefined, "H is not an ancestor");
  assert.equal(await handoff(flight({ publicationRecord: { candidate: C, original_head: B, floor: { kind: "pinned", oid: D } } })), undefined, "the floor is not an ancestor");
});

// ── D9 ───────────────────────────────────────────────────────────────────────────────────────────────

it("reports the forge API success after the MR is created", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  const seen: string[] = [];
  const forge = fakeGitlab({ intercept: (req) => { seen.push(`forge:${req.method}`); return undefined; } });
  client.reportForgeApiSuccess = (c) => { seen.push(`success:${c.run_id}`); };
  const claim = claimFor(5113);
  const r = build(new StubExecutor(nullLogger()), { gitlab: forge.gitlab }, outbox);
  await r.execute(claim);
  assert.ok(seen.includes(`success:${claim.run_id}`), seen.join(","));
  assert.ok(seen.indexOf("forge:POST") < seen.indexOf(`success:${claim.run_id}`), "after the create call returned");
});

// ── the enrolled pre-clone failure ───────────────────────────────────────────────────────────────────

class RecordingClient extends WorkerClient {
  readonly reported: StateRequest[] = [];
  constructor(features: string[]) {
    super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
    this.protocolFeatures = features;
  }
  override async reportState(_runId: string, body: StateRequest): Promise<StateAck> {
    this.reported.push({ ...body });
    return { applied: body.status !== undefined, status: body.status };
  }
}

for (const enrolled of [true, false]) {
  it(`a transient clone failure on a claim ${enrolled ? "with" : "without"} an active enrollment sends the ${enrolled ? "publication" : "legacy"} park`, async () => {
    const rc = new RecordingClient(FEATURES);
    git.ensureClone = async () => { throw new Error("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com"); };
    const claim = claimFor(5114, enrolled ? { publication_resume: { enrollment_seq: 1, candidate_seq: 1, intent: {} } } : {});
    const r = new RunRunner(rc, git, () => ({ executor: new StubExecutor(nullLogger()) }), nullLogger(), 20, undefined,
      { pollMs: 5, planApprovalTimeoutMs: 0, recoveryRetryMs: 5, gitlab: fakeGitlab().gitlab });
    await r.execute(claim);
    const park = rc.reported.filter(b => b.status === "recovery_wait");
    assert.equal(park.length, 1);
    if (enrolled) {
      assert.deepEqual([park[0]!.recovery_cause, park[0]!.recovery_phase, park[0]!.publication_surface, park[0]!.claim_generation],
        ["forge_unreachable", "publication", "git", GEN]);
    } else {
      assert.equal(park[0]!.recovery_phase, undefined);
      assert.equal(park[0]!.recovery_cause, "forge_unreachable");
    }
  });
}

// ── lifecycle ────────────────────────────────────────────────────────────────────────────────────────

it("a surviving record whose enrollment the api retired neither resurrects it nor overrides the new enrollment", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const claim = claimFor(5115);
  const events: string[] = [];
  api.publicationIntentWire = (req) => {
    const seq = req.body.enrollment_seq as number | undefined;
    events.push(seq === undefined ? "enroll" : `resend:${seq}`);
    if (seq === 7) return { status: 409, body: { reason: "publication_enrollment_retired" } };
    return { status: 200, body: { enrollment_seq: 8, candidate_seq: 1, state: "active" } };
  };
  // The executor seeds the OLD acknowledged record (enrollment_seq 7, candidate_seq 3) for this very finalize
  // identity, as a completion-hold continue leaves it: same run, generation, branch, head and floor.
  const stub = new StubExecutor(nullLogger());
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      const result = await stub.run(ctx);
      const head = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      const saved = await outbox.savePublicationRecord({
        owner_kind: "issue",
        v: 1, run_id: claim.run_id, repo_id: REPO_ID, branch: ctx.branch, claim_generation: GEN,
        original_head: head, floor: { kind: "absent" }, candidate: head,
        source: { pin_generation: GEN, checkpoint_key: null },
        mr: { title: "old", summary: "", closing: [], contract_revision: 0 },
        guard: {
          frozen_held: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] },
          secret_remediation: { attempts: 0, known: [], flagged_commits: [], flagged_findings: [], blocked: [], ever_known: false, withhold_paths: false, overflow: false },
          published_real_tips: [], landed_checkpoint: false, ci_fix_human_approved: false, ci_config_guard_globs: null,
        },
      });
      assert.ok(saved.ok);
      const acked = await outbox.replacePublicationRecord({ ...saved.record, enrollment_seq: 7, candidate_seq: 3 }, {});
      assert.equal(acked.ok, true);
      return result;
    },
  };
  const r = build(executor, { gitlab: fakeGitlab().gitlab }, outbox);
  await r.execute(claim);
  assert.deepEqual(events, ["resend:7", "enroll"], "the old enrollment is asked about once, then a fresh enrollment replaces it");
  const resend = api.publicationIntentRequests[0]!.body as { candidate_seq: number };
  assert.equal(resend.candidate_seq, 3, "the resend carried the old record's own state, not a new candidate");
  const fresh = api.publicationIntentRequests[1]!.body as { enrollment_seq?: number; candidate_seq?: number };
  assert.equal(fresh.enrollment_seq, undefined, "the fresh enrollment replays nothing of the old sequence");
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
});

// ── review rework: what the intent records ───────────────────────────────────────────────────────────

type IntentBodyOnWire = {
  intent: {
    mr: { title: string; summary: string; closing: string[] };
    guard: { secret_remediation: { known: unknown[]; flagged_findings: unknown[]; blocked: unknown[] } };
  };
};
const PAT = "fixture-forge-pat-000000";

it("records the title and summary redacted, the summary as parseable JSON, and no secret-finding path", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  const kindAtAck: string[] = [];
  const ackWire = api.publicationIntentWire!;
  api.publicationIntentWire = async (req) => {
    const loaded = await outbox.loadPublicationRecord(req.runId, GEN);
    kindAtAck.push(loaded.kind === "ok" ? loaded.record.owner_kind : loaded.kind);
    return ackWire(req);
  };
  const claim = claimFor(5120, { issue_title: `Rotate ${PAT} now` });
  const stub = new StubExecutor(nullLogger());
  const executor: Executor = {
    run: async (ctx) => ({
      ...(await stub.run(ctx)),
      prSummary: { what: `rotates ${PAT}`, why: "because", changes: ["a", `b ${PAT}`] },
    }),
  };
  const r = build(executor, { gitlab: fakeGitlab().gitlab }, outbox);
  // A flagged finding exists when the intent is written; its path must never reach the record or the wire.
  const rt = r as unknown as { fetchTracking: (flight: { secretRemediation?: unknown }, ...rest: unknown[]) => Promise<unknown> };
  const realFetch = rt.fetchTracking.bind(r);
  rt.fetchTracking = async (flight, ...rest) => {
    flight.secretRemediation = {
      attempts: 1, withholdPaths: true,
      flaggedFindings: [{ file: "secret/leaked-name.txt", startLine: 7, commit: "a".repeat(40), ruleId: "generic-api-key" }],
    };
    return realFetch(flight, ...rest);
  };
  await r.execute(claim);

  assert.ok(api.publicationIntentRequests.length >= 1);
  const wire = JSON.stringify(api.publicationIntentRequests[0]!.body);
  assert.ok(!wire.includes(PAT), "no credential on the wire");
  assert.ok(!wire.includes("leaked-name"), "no finding path on the wire");
  const body = api.publicationIntentRequests[0]!.body as IntentBodyOnWire;
  assert.ok(body.intent.mr.title.startsWith("Rotate ") && body.intent.mr.title.endsWith(" now") && !body.intent.mr.title.includes(PAT), body.intent.mr.title);
  const summary = JSON.parse(body.intent.mr.summary) as { what: string; why: string; changes: string[] };
  assert.ok(!summary.what.includes(PAT) && !summary.changes[1]!.includes(PAT));
  assert.equal(summary.why, "because");
  assert.deepEqual(body.intent.guard.secret_remediation.flagged_findings, [{ start_line: 7, commit: "a".repeat(40), rule_id: "generic-api-key" }]);
  assert.deepEqual(kindAtAck, ["issue"], "the local envelope carries the run's kind (the caller's, not the body's)");
  assert.deepEqual(body.intent.mr.closing, [`#5120`], "a plain full delivery is known closing");
});

it("records no closing reference for an owner-partial delivery or a scope-capped one", async () => {
  client.protocolFeatures = FEATURES;
  ackIntents([]);
  const partial = claimFor(5121, { config: { completion_scope: { deferred: [{ milestone_id: "m2", title: "later", reason: "owner" }] } } });
  const r1 = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, await mkOutbox());
  await r1.execute(partial);
  assert.deepEqual((api.publicationIntentRequests.find(q => q.runId === partial.run_id)!.body as IntentBodyOnWire).intent.mr.closing, []);

  const capped = claimFor(5122);
  const stub = new StubExecutor(nullLogger());
  const executor: Executor = { run: async (ctx) => ({ ...(await stub.run(ctx)), scopeCapped: { completedCount: 1, total: 3 } }) };
  const r2 = build(executor, { gitlab: fakeGitlab().gitlab }, await mkOutbox());
  await r2.execute(capped);
  assert.deepEqual((api.publicationIntentRequests.find(q => q.runId === capped.run_id)!.body as IntentBodyOnWire).intent.mr.closing, []);
});

// ── review rework: transformed candidates are pinned, and an unpinned one is never pushed ────────────

/** A runner whose recovery coordinator is ENABLED (a worker token), as on a real worker. */
const recoveryRunner = (executor: Executor, outbox: Outbox): RunRunner =>
  build(executor, { github: fakeGitHub().github }, outbox, nullLogger(), {
    recovery: new RecoveryCoordinator({ client: new FakeRecoveryClient(), git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: "publication-intent-worker-fixture" }),
  });

type PinSpy = { calls: Array<{ attemptedHeadSha?: string }>; results: Array<{ attemptedHeadShas?: string[]; attemptedHeadSha?: string } | undefined> };
function spyOnPin(r: RunRunner, failCandidates: boolean): PinSpy {
  const rec = (r as unknown as { recovery: { pin: (input: { attemptedHeadSha?: string }) => Promise<unknown> } }).recovery;
  const real = rec.pin.bind(rec);
  const spy: PinSpy = { calls: [], results: [] };
  rec.pin = async (input) => {
    spy.calls.push(input.attemptedHeadSha ? { attemptedHeadSha: input.attemptedHeadSha } : {});
    const out = failCandidates && input.attemptedHeadSha ? undefined : await real(input);
    spy.results.push(out as PinSpy["results"][number]);
    return out;
  };
  return spy;
}

it("pins the transformed candidate in the recovery record before the push that uses it", async () => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  ackIntents(events);
  observeRemote(events);
  const claim = githubClaim(5123);
  const r = recoveryRunner(advancingExecutor(), outbox);
  const spy = spyOnPin(r, false);
  await r.execute(claim);
  const candidate = (api.publicationIntentRequests[1]!.body as { intent: { candidate: string } }).intent.candidate;
  const pinned = spy.calls.findIndex(c => c.attemptedHeadSha === candidate);
  assert.ok(pinned >= 0, "the candidate was pinned");
  assert.ok(spy.results[pinned]?.attemptedHeadShas?.includes(candidate) ?? spy.results[pinned]?.attemptedHeadSha === candidate);
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 1);
});

it("a transformed candidate whose pin fails is not pushed, is retained, and reports no failure", async () => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const events: string[] = [];
  ackIntents(events);
  observeRemote(events);
  const claim = githubClaim(5124);
  const r = recoveryRunner(advancingExecutor(), outbox);
  spyOnPin(r, true);
  await r.execute(claim);
  assert.ok(!events.includes("pushBranch"), `no push with an unpinned candidate: ${events.join(",")}`);
  assert.ok(!events.includes("intent:advance"), "the api was not asked to move to a candidate that could not be pinned");
  assert.equal(failed(claim.run_id).length, 0);
  assert.equal(states(claim.run_id).filter(b => b.status === "completed").length, 0);
  assert.equal((await outbox.loadPublicationRecord(claim.run_id, GEN)).kind, "ok", "the record is kept for the reconciliation");
});

// ── review rework: the record is discarded on any applied terminal ACK ───────────────────────────────

const seedRecord = async (outbox: Outbox, runId: string, generation: number, enrollmentSeq: number): Promise<void> => {
  const head = "1".repeat(40);
  const saved = await outbox.savePublicationRecord({
    owner_kind: "issue", v: 1, run_id: runId, repo_id: REPO_ID, branch: "agent/issue-9", claim_generation: generation,
    original_head: head, floor: { kind: "absent" }, candidate: head,
    source: { pin_generation: generation, checkpoint_key: null },
    mr: { title: "t", summary: "", closing: [], contract_revision: 0 },
    guard: {
      frozen_held: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] },
      secret_remediation: { attempts: 0, known: [], flagged_commits: [], flagged_findings: [], blocked: [], ever_known: false, withhold_paths: false, overflow: false },
      published_real_tips: [], landed_checkpoint: false, ci_fix_human_approved: false, ci_config_guard_globs: null,
    },
  });
  assert.ok(saved.ok);
  assert.equal((await outbox.replacePublicationRecord({ ...saved.record, enrollment_seq: enrollmentSeq, candidate_seq: 1 }, {})).ok, true);
};

it("a failed run's applied terminal ACK discards its publication record", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  failGit("push", "remote: Permission to org/repo denied to bot.", () => {});
  const claim = claimFor(5125);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  await r.execute(claim);
  assert.equal(failed(claim.run_id).length, 1, "the run failed on the permanent push rejection");
  assert.equal(outbox.hasPublicationRecords(claim.run_id), false, "the failed ACK closed the enrollment, so the record is gone");
});

it("every applied terminal ACK (completed, failed, cancelled) discards the records of ALL generations, including a replayed one", async () => {
  const outbox = await mkOutbox();
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const observe = (r as unknown as { observeSettlementTerminalAck: (runId: string, gen: number, body: StateRequest, ack: StateAck) => Promise<void> })
    .observeSettlementTerminalAck.bind(r);
  const cases: Array<[string, StateRequest, StateAck]> = [
    ["completed", { status: "completed" }, { applied: true, status: "completed" }],
    ["failed", { status: "failed" }, { applied: true, status: "failed" }],
    ["cancelled", { status: "failed" }, { applied: true, status: "cancelled" }],
  ];
  for (const [name, body, ack] of cases) {
    const runId = crypto.randomUUID();
    await seedRecord(outbox, runId, 3, 11);
    await seedRecord(outbox, runId, 5, 12);
    await observe(runId, 5, body, ack); // delivered later, e.g. by a terminal-journal replay at the newer generation
    assert.equal(outbox.hasPublicationRecords(runId), false, `${name}: both generations discarded`);
  }
  // Nothing is discarded on an answer that did not apply a terminal write.
  const kept = crypto.randomUUID();
  await seedRecord(outbox, kept, 3, 11);
  await observe(kept, 3, { status: "failed" }, { applied: false, status: "running" });
  await observe(kept, 3, { status: "failed" }, { applied: true, status: "recovery_wait" });
  await observe(kept, 3, { status: "running" } as StateRequest, { applied: true, status: "failed" });
  assert.equal(outbox.hasPublicationRecords(kept), true, "an unapplied or non-terminal ack keeps the record");
});

// ── review rework: the request carries the LIVE claim generation ─────────────────────────────────────

it("a record enrolled at generation G is advanced from a flight at G+1 with a request that carries G+1", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const runId = crypto.randomUUID();
  await seedRecord(outbox, runId, 4, 7);
  const loaded = await outbox.loadPublicationRecord(runId, 4);
  assert.equal(loaded.kind, "ok");
  const events: string[] = [];
  ackIntents(events);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const flight = {
    runId, claimGeneration: 5, publicationRecord: loaded.kind === "ok" ? loaded.record : undefined,
    batcher: { emit() {} }, runLog: nullLogger(), completionHoldOutcome: undefined,
  };
  const confirm = (r as unknown as { confirmPublicationCandidate: (f: unknown, candidate: string) => Promise<void> }).confirmPublicationCandidate.bind(r);
  await confirm(flight, "9".repeat(40));
  const sent = api.publicationIntentRequests.find(q => q.runId === runId)!.body as { claim_generation: number; enrollment_seq: number; candidate_seq: number; intent: { claim_generation: number } };
  assert.equal(sent.claim_generation, 5, "the fence is the live claim generation");
  assert.deepEqual([sent.enrollment_seq, sent.candidate_seq, sent.intent.claim_generation], [7, 2, 4], "the enrollment identity stays at G");
});

// ── review rework: a refused publication park at pre-clone ───────────────────────────────────────────

class ScriptedClient extends WorkerClient {
  readonly reported: StateRequest[] = [];
  constructor(private readonly answer: (body: StateRequest, n: number) => StateAck) {
    super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
    this.protocolFeatures = FEATURES;
  }
  override async reportState(_runId: string, body: StateRequest): Promise<StateAck> {
    this.reported.push({ ...body });
    return this.answer(body, this.reported.length);
  }
}

async function preClonePark(answer: (body: StateRequest, n: number) => StateAck): Promise<{ rc: ScriptedClient; settles: number }> {
  const rc = new ScriptedClient(answer);
  git.ensureClone = async () => { throw new Error("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com"); };
  const claim = claimFor(5130, { publication_resume: { enrollment_seq: 1, candidate_seq: 1, intent: {} } });
  const r = new RunRunner(rc, git, () => ({ executor: new StubExecutor(nullLogger()) }), nullLogger(), 20, undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, recoveryRetryMs: 5, gitlab: fakeGitlab().gitlab });
  let settles = 0;
  (r as unknown as { settleUnadoptedGuardedClaim: () => Promise<void> }).settleUnadoptedGuardedClaim = async () => { settles++; };
  await r.execute(claim);
  return { rc, settles };
}

it("a pre-clone publication park refused as not enrolled falls back to the legacy typed forge park, then settles the unadopted hold", async () => {
  const { rc, settles } = await preClonePark((body) =>
    body.recovery_phase === "publication" ? { applied: false, status: "running", reason: "publication_not_enrolled" } : { applied: true, status: body.status });
  const parks = rc.reported.filter(b => b.status === "recovery_wait");
  assert.equal(parks.length, 2, "the publication park, then the legacy park");
  assert.equal(parks[0]!.recovery_phase, "publication");
  assert.equal(parks[1]!.recovery_phase, undefined);
  assert.equal(parks[1]!.recovery_cause, "forge_unreachable");
  assert.equal(parks[1]!.claim_generation, GEN);
  assert.equal(rc.reported.filter(b => b.status === "failed").length, 0);
  assert.equal(settles, 1, "the legacy park settles the empty guarded hold");
});

it("a pre-clone publication park refused as ENROLLED is retained: no second park, no failure", async () => {
  const { rc } = await preClonePark((body) =>
    body.recovery_phase === "publication" ? { applied: false, status: "running", reason: "publication_enrolled" } : { applied: true, status: body.status });
  assert.equal(rc.reported.filter(b => b.status === "recovery_wait").length, 1);
  assert.equal(rc.reported.filter(b => b.status === "failed").length, 0);
});

it("a successful publication-phase park does not try to settle an unadopted guarded hold", async () => {
  const { rc, settles } = await preClonePark((body) => ({ applied: true, status: body.status }));
  assert.equal(rc.reported.filter(b => b.status === "recovery_wait").length, 1);
  assert.equal(settles, 0, "the api refuses that settle for a publication phase");
});

// ── review rework: a surviving record never outranks this flight's fresh intent; an abort is not a hold ───

type EnrollSeam = {
  enrollPublication(claim: unknown, flight: unknown, input: unknown, signal?: AbortSignal): Promise<"proceed" | "retain">;
};
const HEAD = "1".repeat(40);
const frozenHeld = { version: 1 as const, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] };
const secretRemediation = { attempts: 0, known: [], flagged_commits: [], flagged_findings: [], blocked: [], ever_known: false, withhold_paths: false, overflow: false };
const intentInput = (strict: boolean, branch: string) => ({
  branch, original_head: HEAD, source: { pin_generation: GEN, checkpoint_key: null },
  mr: { title: strict ? "strict title" : "lax title", summary: "", closing: strict ? [] : ["#9"], contract_revision: 0 },
  guard: {
    frozen_held: frozenHeld, secret_remediation: secretRemediation, published_real_tips: [], landed_checkpoint: false,
    ci_fix_human_approved: false, ci_config_guard_globs: strict ? [".github/**"] : null,
  },
});
const enrollFlight = (runId: string) => ({
  runId, claimGeneration: GEN, runKind: "issue", publicationFloor: { kind: "absent" }, publicationRecord: undefined as
    undefined | { mr: { title: string; closing: string[] }; guard: { ci_config_guard_globs: string[] | null }; candidate_seq?: number },
  batcher: { emit() {} }, runLog: nullLogger(), completionHoldOutcome: "none",
});
const seedLax = async (outbox: Outbox, claim: { run_id: string }, branch: string, seqs?: [number, number]): Promise<void> => {
  const saved = await outbox.savePublicationRecord({
    owner_kind: "issue", v: 1, run_id: claim.run_id, repo_id: REPO_ID, claim_generation: GEN, floor: { kind: "absent" }, candidate: HEAD,
    ...intentInput(false, branch),
  });
  assert.ok(saved.ok);
  if (seqs) assert.equal((await outbox.replacePublicationRecord({ ...saved.record, enrollment_seq: seqs[0], candidate_seq: seqs[1] }, {})).ok, true);
};
type WireIntent = { candidate_seq?: number; intent: { mr: { title: string; closing: string[] }; guard: { ci_config_guard_globs: string[] | null } } };

/** A stateful api mirroring applyPublicationIntent: a seq-less request returns the stored row on an identity match
 *  WITHOUT storing the body; an update at stored candidate_seq + 1 stores the whole intent. */
function statefulIntentApi(initial?: WireIntent["intent"]): { stored: () => WireIntent["intent"] | undefined } {
  let intent = initial;
  let es = 1;
  let cs = 1;
  api.publicationIntentWire = (req) => {
    const body = req.body as unknown as WireIntent & { enrollment_seq?: number };
    if (body.enrollment_seq === undefined) {
      if (!intent) intent = body.intent;
      return { status: 200, body: { enrollment_seq: es, candidate_seq: cs, state: "active" } };
    }
    if (body.enrollment_seq !== es) return { status: 409, body: { error: "publication_enrollment_mismatch" } };
    if (body.candidate_seq === cs + 1) { intent = body.intent; cs += 1; }
    else if (body.candidate_seq !== cs) return { status: 409, body: { error: "publication_candidate_seq_mismatch" } };
    return { status: 200, body: { enrollment_seq: es, candidate_seq: cs, state: "active" } };
  };
  return { stored: () => intent };
}

it("an adopted never-ACKed record (its ACK was lost) is posted as held, then advanced to this flight's stricter intent", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  const claim = claimFor(5140);
  await seedLax(outbox, claim, "agent/issue-5140");
  // Flight A's POST landed (the api stored the lax intent) and only its ACK was lost.
  const lax = (await outbox.loadPublicationRecord(claim.run_id, GEN));
  assert.equal(lax.kind, "ok");
  const { stored } = statefulIntentApi(lax.kind === "ok" ? ({ ...lax.record, enrollment_seq: undefined, candidate_seq: undefined, owner_kind: undefined } as unknown as WireIntent["intent"]) : undefined);
  const flight = enrollFlight(claim.run_id);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const result = await (r as unknown as EnrollSeam).enrollPublication(claim, flight, intentInput(true, "agent/issue-5140"));
  assert.equal(result, "proceed");
  const sent = api.publicationIntentRequests.map(q => q.body as unknown as WireIntent);
  assert.equal(sent.length, 2, "the held enrollment, then one advance");
  assert.deepEqual([sent[0]!.intent.mr.title, sent[0]!.candidate_seq], ["lax title", undefined], "the held content is posted unchanged");
  assert.deepEqual([sent[1]!.intent.mr.title, sent[1]!.candidate_seq], ["strict title", 2]);
  const apiStored = stored()!;
  assert.deepEqual([apiStored.mr.title, apiStored.mr.closing, apiStored.guard.ci_config_guard_globs], ["strict title", [], [".github/**"]], "the api ends on the strict intent");
  assert.deepEqual([flight.publicationRecord?.mr.title, flight.publicationRecord?.guard.ci_config_guard_globs, flight.publicationRecord?.candidate_seq], ["strict title", [".github/**"], 2]);
  const loaded = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(loaded.kind === "ok" ? loaded.record.mr.title : loaded.kind, "strict title", "the local record equals the api's");
  assert.equal(loaded.kind === "ok" ? loaded.record.candidate_seq : undefined, 2);
});

it("a surviving ACKed record with a laxer snapshot is advanced with this flight's stricter guard and title, never adopted as is", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  const claim = claimFor(5141);
  await seedLax(outbox, claim, "agent/issue-5141", [7, 1]);
  const flight = enrollFlight(claim.run_id);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const result = await (r as unknown as EnrollSeam).enrollPublication(claim, flight, intentInput(true, "agent/issue-5141"));
  assert.equal(result, "proceed");
  const last = api.publicationIntentRequests.at(-1)!.body as unknown as WireIntent;
  assert.equal(last.candidate_seq, 2, "advanced with the CAS sequence");
  assert.deepEqual([last.intent.mr.title, last.intent.guard.ci_config_guard_globs], ["strict title", [".github/**"]]);
  assert.deepEqual([flight.publicationRecord?.mr.title, flight.publicationRecord?.guard.ci_config_guard_globs, flight.publicationRecord?.candidate_seq], ["strict title", [".github/**"], 2]);
});

it("an abort during enrollment is rethrown like every other publish step: no unresolved hold, the record is kept", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  const claim = claimFor(5142);
  const flight = enrollFlight(claim.run_id);
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects((r as unknown as EnrollSeam).enrollPublication(claim, flight, intentInput(false, "agent/issue-5142"), controller.signal));
  assert.equal(flight.completionHoldOutcome, "none", "the abort must not latch the unresolved hold that skips every terminal and park");
  assert.equal(outbox.hasPublicationRecords(claim.run_id), true, "the local record is kept");
});

it("an abort during a transformed-candidate advance is rethrown: no unresolved hold, no unconfirmed-candidate retain, the record is kept", async () => {
  client.protocolFeatures = FEATURES;
  const outbox = await mkOutbox();
  ackIntents([]);
  const claim = claimFor(5143);
  await seedLax(outbox, claim, "agent/issue-5143", [7, 1]);
  const loaded = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(loaded.kind, "ok");
  const flight = { ...enrollFlight(claim.run_id), publicationRecord: loaded.kind === "ok" ? loaded.record : undefined };
  const r = build(new StubExecutor(nullLogger()), { gitlab: fakeGitlab().gitlab }, outbox);
  const controller = new AbortController();
  controller.abort();
  const confirm = (r as unknown as { confirmPublicationCandidate: (f: unknown, candidate: string, signal?: AbortSignal) => Promise<void> }).confirmPublicationCandidate.bind(r);
  await assert.rejects(confirm(flight, "9".repeat(40), controller.signal), (err: unknown) => (err as Error).name !== "PublicationUnconfirmedSignal");
  assert.equal(flight.completionHoldOutcome, "none", "the abort must not latch the unresolved hold");
  assert.equal(api.publicationIntentRequests.length, 0, "nothing was sent after the abort");
  const after = await outbox.loadPublicationRecord(claim.run_id, GEN);
  assert.equal(after.kind, "ok", "the local record is kept");
});
