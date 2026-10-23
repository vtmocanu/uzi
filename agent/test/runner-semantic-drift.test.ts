// PRD #1230 D5: source drift observation. A detector only: the runner posts one observation per accepted
// semantic presentation and one at publication, and no failure of it may block, fail or change the run.
import { beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { RequestError, WorkerClient } from "../src/client.js";
import { observePrdSources, hasDriftContradiction, semanticObservationRecordSchema, type SemanticObservationRecord } from "../src/semantic-drift.js";
import { semanticCoverageStatusSchema } from "../src/semantic-coverage-wire.js";
import { renderCompletionBlock } from "../src/pr-description.js";
import { api, client, fakeGitlab, fx, git, installHarness, runner } from "./runner-harness.js";
import { freshClaim, gates, newApi, row, send } from "./gate-revision-harness.js";
import { boundedTransport, enrollment, proposal } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

const PRD = "prds/9-drift.md";
const PRD_V1 = "# Drift PRD\n\nOriginal contract.\n";
const PRD_V2 = "# Drift PRD\n\nEdited after approval.\n";
const sha256 = (s: string) => createHash("sha256").update(s).digest("hex");
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
const milestones = [{ id: "m1", title: "A" }];
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], {
  encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
}).trim();
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"];

beforeEach(() => {
  newApi(); api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
  git.capturePlanningDiff = async (clone, base, bare) => {
    assert.ok(bare, "runner forwards explicit worker bare authority");
    assert.match(base, /^[a-f0-9]{40}$/);
    assert.ok(clone);
    return Buffer.from("");
  };
  fs.mkdirSync(path.join(fx.originPath, "prds"));
  fs.writeFileSync(path.join(fx.originPath, PRD), PRD_V1);
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", "add prd"]);
});

const description = `Implement A, see ${PRD}`;

function serverWithAck(events: string[]) {
  const server = semanticServer(client, events);
  const presentations: ReturnType<typeof server.gateAck>[] = [];
  api.stateAckWire = (body, run) => {
    if (body.status !== "awaiting_approval") return undefined;
    const ack = server.gateAck(body.claim_generation!, api.gateResumeFields(String(run.id)).resume_gate_revision!, body.semantic_park);
    presentations.push(ack);
    return { status: 200, body: { run, ...ack } };
  };
  return { ...server, presentations };
}

function submitted(issueIid: number | null) {
  const p = proposal(description);
  p.criteria[0]!.citations[0]!.source_id = "issue:" + issueIid;
  return p;
}

/** Plans (through the real gate), then commits real work: `editPrd` rewrites the PRD in the pushed commit. */
class DriftExecutor extends StubExecutor {
  head = "";
  constructor(private readonly iid: number | null, private readonly rounds: number, private readonly editPrd: boolean) { super(nullLogger()); }
  override async run(ctx: RunContext): Promise<ExecutorResult> {
    for (let n = 1; n <= this.rounds; n++) {
      const verdict = await ctx.gatePlan!("Implement A " + n, milestones, undefined, undefined, submitted(this.iid));
      assert.equal(verdict.kind, n < this.rounds ? "revise" : "approve");
    }
    const result = await super.run(ctx);
    if (this.editPrd) {
      fs.writeFileSync(path.join(ctx.worktreePath, PRD), PRD_V2);
      gitIn(ctx.worktreePath, ["add", PRD]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "edit prd"]);
    }
    this.head = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    return result;
  }
}

async function drive(opts: { rounds?: number; editPrd?: boolean; arrange?: (s: ReturnType<typeof serverWithAck>) => void } = {}) {
  const c = freshClaim(1, { kind: "issue", auto_approve: false, issue_description: description, semantic_planning: enrollment });
  const events: string[] = [];
  const server = serverWithAck(events);
  opts.arrange?.(server);
  const transport = await boundedTransport("claude", server.report, events);
  const rounds = opts.rounds ?? 1;
  let seen = 0;
  api.onState(c.run_id, body => {
    if (body.status === "awaiting_approval") send(c.run_id, row(++seen < rounds ? "revise_plan" : "approve_plan", "owner decision"));
  });
  const exec = new DriftExecutor(c.issue_iid, rounds, opts.editPrd ?? false);
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab, undefined, { semanticCoverageTestTransport: transport, planApprovalTimeoutMs: 4000 }).execute(c);
  const completed = api.states.filter(s => s.runId === c.run_id && s.body.status === "completed");
  return { c, events, server, exec, forge, completed };
}

it("semantic-drift.changed-tip-prd", { skip: LINUX_CAPTURE }, async () => {
  const { c, server, exec, forge, completed } = await drive({ editPrd: true });
  assert.equal(completed.length, 1, JSON.stringify(api.states.map(s => s.body.status)));
  const publication = server.observations.find(o => o.phase === "publication");
  assert.ok(publication && publication.phase === "publication");
  assert.equal(publication.pushed_sha, exec.head, "read at the exact pushed SHA, not a branch tip");
  assert.deepEqual(publication.prd_sources, [{ source_id: "prd:" + PRD, observed_hash: sha256(PRD_V2), ref: exec.head, outcome: "changed" }]);
  // The presentation read the capture commit, where the PRD still matches the frozen hash.
  const presentation = server.observations.find(o => o.phase === "presentation");
  assert.equal(presentation?.prd_sources[0]?.outcome, "unchanged");
  assert.equal(presentation?.prd_sources[0]?.observed_hash, sha256(PRD_V1));
  assert.match(forge.pr.description, /Source drift detected/);
  assert.match(forge.pr.description, /prds\/9-drift\.md/);
  assert.ok(api.messages(c.run_id).some(m => m.payload.event === "semantic_source_drift" && m.payload.contradiction === true));
});

it("semantic-drift.changed-current-issue", { skip: LINUX_CAPTURE }, async () => {
  const { c, completed, forge } = await drive({ arrange: s => s.driftIssue({ outcome: "changed", checkbox_only: true }, ["prds/10-new.md"]) });
  assert.equal(completed.length, 1, "a changed issue never blocks the run");
  assert.match(forge.pr.description, /Source drift detected/);
  assert.match(forge.pr.description, new RegExp(`issue:${c.issue_iid}`));
  assert.match(forge.pr.description, /changed/);
  assert.match(forge.pr.description, /checkbox state only/);
  assert.match(forge.pr.description, /PRD link added to the issue.*prds\/10-new\.md/);
  assert.ok(!api.states.some(s => s.runId === c.run_id && s.body.status === "failed"));
});

it("semantic-drift.unreadable-observation", { skip: LINUX_CAPTURE }, async () => {
  const base = gitIn(fx.originPath, ["rev-parse", "main"]);
  const real = git.readBare.bind(git);
  // Capture at the base commit works; every later read of the PRD fails like an unreadable object.
  git.readBare = (async (bare: string, args: readonly string[], opts: Parameters<typeof real>[2]) => {
    if (String(args[1]).startsWith(base)) return real(bare, args, opts);
    throw new Error("fatal: unable to read blob object");
  }) as typeof git.readBare;
  const { c, server, completed, forge } = await drive({
    arrange: s => s.failObservation(new RequestError("POST", "/semantic/observation", 503, "down")),
  });
  assert.equal(completed.length, 1, "a failed observation never blocks the run");
  const publication = server.observations.find(o => o.phase === "publication");
  assert.ok(publication);
  assert.deepEqual(publication.prd_sources.map(({ source_id, outcome, reason, observed_hash }) => ({ source_id, outcome, reason, observed_hash })),
    [{ source_id: "prd:" + PRD, outcome: "unknown", reason: "unreadable", observed_hash: undefined }]);
  assert.equal(server.stored.length, 0, "the server refused to record");
  assert.ok(!/Source drift detected/.test(forge.pr.description), "no record, so nothing is asserted in the PR");
  const notes = api.messages(c.run_id).map(m => String((m.payload as { text?: unknown }).text ?? ""));
  assert.equal(notes.filter(t => /source drift observation \(presentation\) not recorded/.test(t)).length, 1);
  assert.equal(notes.filter(t => /source drift observation \(publication\) not recorded/.test(t)).length, 1);
  assert.ok(!api.states.some(s => s.runId === c.run_id && s.body.status === "failed"));
});

it("semantic-drift.no-silent-rewrite-or-satisfaction", { skip: LINUX_CAPTURE }, async () => {
  const control = await drive();
  const frozenBefore = JSON.stringify(control.server.compiled().candidate);
  const drifted = await drive({ editPrd: true, arrange: s => s.driftIssue({ outcome: "changed" }, ["prds/10-new.md"]) });
  assert.equal(control.completed.length, 1);
  assert.equal(drifted.completed.length, 1);
  assert.ok(drifted.server.stored.some(r => hasDriftContradiction(r)));
  // The server fixture compiles exactly once per run; drift never recompiles or rewrites the frozen contract.
  assert.equal(drifted.events.filter(e => e === "compile").length, 1);
  const candidate = drifted.server.compiled().candidate;
  assert.equal(JSON.stringify(candidate).includes("satisf"), false);
  assert.deepEqual(candidate.criteria.map(x => x.kind), ["code"]);
  assert.equal(JSON.stringify(control.server.compiled().candidate), frozenBefore);
  // Completion is the same shape as the control run, and no gate was added or removed by drift.
  const shape = (r: typeof control) => Object.keys(r.completed[0]!.body).sort();
  assert.deepEqual(shape(drifted), shape(control));
  assert.equal(gates(drifted.c.run_id).length, gates(control.c.run_id).length);
  // The PR body marks nothing met and the closing line is exactly what the control run rendered.
  const body = drifted.forge.pr.description;
  assert.ok(!/\[[xX]\]/.test(body), "no box is checked");
  assert.equal(/Closes #\d+/.test(body), /Closes #\d+/.test(control.forge.pr.description));
  assert.ok(!/satisf/i.test(body.slice(body.indexOf("Source drift detected"))), "the note never calls anything satisfied");
});

it("semantic-drift.presentation-and-publication-observations", { skip: LINUX_CAPTURE }, async () => {
  const { server, completed } = await drive({ rounds: 2 });
  assert.equal(completed.length, 1);
  assert.deepEqual(server.observations.map(o => o.phase), ["presentation", "presentation", "publication"]);
  assert.equal(server.presentations.length, 2);
  const [first, revised, publication] = server.observations;
  for (const [obs, ack] of [[first, server.presentations[0]!], [revised, server.presentations[1]!]] as const) {
    assert.ok(obs && obs.phase === "presentation");
    assert.equal(obs.candidate_digest, ack.semantic_approval.candidate_digest);
    assert.equal(obs.gate_revision, ack.gate_revision);
    assert.equal("pushed_sha" in obs, false);
  }
  assert.notEqual((first as { gate_revision: number }).gate_revision, (revised as { gate_revision: number }).gate_revision);
  assert.ok(publication && publication.phase === "publication");
  assert.match(publication.pushed_sha, /^[0-9a-f]{40}$/);
  assert.equal("candidate_digest" in publication || "gate_revision" in publication, false);
  assert.ok(publication.prd_sources.every(s => s.ref === publication.pushed_sha));
});

it("semantic-drift.structural-runs-make-no-observation-call", { skip: LINUX_CAPTURE }, async () => {
  let calls = 0;
  client.postSemanticObservation = async () => { calls++; throw new Error("structural runs must not observe"); };
  const c = freshClaim(1, { kind: "issue", auto_approve: true });
  const forge = fakeGitlab();
  await runner(new StubExecutor(nullLogger()), forge.gitlab).execute(c);
  assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "completed"));
  assert.equal(calls, 0);
  assert.ok(!/Source drift/.test(forge.pr.description));
});

/** A resumed, already-approved run: the executor never plans (no gatePlan in this process), the server holds the
 * frozen candidate, and the status endpoint is the only place the PRD refs can come from. */
async function resumeDrive(opts: { prdRefs?: boolean; statusFailure?: unknown; editPrd?: boolean; noPrd?: boolean } = {}) {
  const c = freshClaim(1, { kind: "issue", auto_approve: false, issue_description: description, semantic_planning: enrollment });
  const events: string[] = [];
  const server = serverWithAck(events);
  const b64 = (t: string) => Buffer.from(t).toString("base64");
  await client.submitSemanticCandidate(c.run_id, 1, { plan_md: "Implement A", milestones } as never, submitted(c.issue_iid), [
    { id: "issue:" + c.issue_iid, kind: "issue", provenance: "worker:test", completeness: "complete", bytes: b64(description) },
    ...(opts.noPrd ? [] : [{ id: "prd:" + PRD, kind: "prd", provenance: "repo:test", completeness: "complete", bytes: b64(PRD_V1) }]),
  ] as never);
  server.prdRefs(opts.prdRefs ?? true);
  if (opts.statusFailure !== undefined) server.failStatus(opts.statusFailure);
  events.length = 0;
  const exec = new DriftExecutor(c.issue_iid, 0, opts.editPrd ?? true);
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab, undefined, { semanticCoverageTestTransport: await boundedTransport("claude", server.report, events) }).execute(c);
  const completed = api.states.filter(s => s.runId === c.run_id && s.body.status === "completed");
  return { c, events, server, exec, forge, completed };
}

it("semantic-drift.resume-observes-publication-from-status", { skip: LINUX_CAPTURE }, async () => {
  const { c, server, exec, forge, completed, events } = await resumeDrive();
  assert.equal(completed.length, 1);
  assert.equal(server.observations.length, 1, "exactly one observation: the publication");
  const publication = server.observations[0]!;
  assert.ok(publication.phase === "publication");
  assert.equal(publication.pushed_sha, exec.head, "read at the exact pushed SHA from the bare");
  assert.deepEqual(publication.prd_sources, [{ source_id: "prd:" + PRD, observed_hash: sha256(PRD_V2), ref: exec.head, outcome: "changed" }]);
  assert.equal(events.filter(e => e === "status").length, 1, "one status read supplies the frozen refs");
  assert.match(forge.pr.description, /Source drift detected/);
  assert.ok(api.messages(c.run_id).some(m => m.payload.event === "semantic_source_drift" && m.payload.contradiction === true));
});

it("semantic-drift.resume-without-prd-sources-still-observes-the-issue", { skip: LINUX_CAPTURE }, async () => {
  const { server, exec, completed } = await resumeDrive({ noPrd: true, editPrd: false });
  assert.equal(completed.length, 1);
  assert.equal(server.observations.length, 1, "an explicit empty PRD list still records the issue observation");
  const publication = server.observations[0]!;
  assert.ok(publication.phase === "publication");
  assert.equal(publication.pushed_sha, exec.head);
  assert.deepEqual(publication.prd_sources, []);
});

for (const [name, opts, note] of [
  ["status read fails", { statusFailure: new RequestError("GET", "/semantic/coverage", 503, "down") }, /not recorded/],
  ["status lacks prd_sources", { prdRefs: false }, /no frozen PRD sources/],
] as const) {
  it(`semantic-drift.resume-${name.replaceAll(" ", "-")}-records-nothing-and-delivers`, { skip: LINUX_CAPTURE }, async () => {
    const { c, server, completed, forge } = await resumeDrive(opts);
    assert.equal(completed.length, 1, "an unobservable resume still delivers");
    assert.equal(server.observations.length, 0, "nothing posted");
    assert.ok(!api.states.some(s => s.runId === c.run_id && s.body.status === "failed"));
    assert.ok(!/Source drift/.test(forge.pr.description));
    const notes = api.messages(c.run_id).map(m => String((m.payload as { text?: unknown }).text ?? ""));
    assert.equal(notes.filter(t => /source drift observation \(publication\) not recorded/.test(t) && note.test(t)).length, 1, JSON.stringify(notes));
  });
}

it("semantic-drift.in-process-publication-makes-no-status-read", { skip: LINUX_CAPTURE }, async () => {
  const { events, completed, server } = await drive({});
  assert.equal(completed.length, 1);
  const tail = events.slice(events.indexOf("observe:publication") - 1);
  assert.equal(tail[1], "observe:publication");
  assert.notEqual(tail[0], "status", "the in-process path reuses the frozen candidate, not a status read");
  assert.equal(server.observations.filter(o => o.phase === "publication").length, 1);
});

it("semantic-drift.presentation-observation-does-not-delay-the-verdict-wait", { skip: LINUX_CAPTURE }, async () => {
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  // Released once the run has completed; the 60 s fallback (well past a sibling drive under load) only bounds a regression where the verdict wait is held.
  const poll = setInterval(() => { if (api.states.some(x => x.body.status === "completed")) release(); }, 5);
  const fallback = setTimeout(release, 60000);
  let completedWhenReleased: boolean | undefined;
  const result = await drive({
    arrange: s => {
      const inner = client.postSemanticObservation.bind(client);
      client.postSemanticObservation = (async (...args: Parameters<typeof inner>) => {
        if (args[2].phase === "presentation") {
          await held;
          completedWhenReleased ??= api.states.some(x => x.body.status === "completed");
        }
        return inner(...args);
      }) as typeof client.postSemanticObservation;
      void s;
    },
  });
  clearInterval(poll); clearTimeout(fallback);
  assert.equal(result.completed.length, 1);
  release();
  await new Promise(r => setTimeout(r, 20));
  assert.equal(completedWhenReleased, true, "the run reached completion while the presentation observation was still held");
  assert.ok(result.server.observations.some(o => o.phase === "presentation"), "the held observation still posts, bounded and advisory");
});

// ── redaction and the contradiction predicate, through a real publication ───────────────────────

const REGISTERED_SECRET = "fixture-forge-pat-000000";

it("semantic-drift.pr-line-redacts-registered-secret-in-source-ids", { skip: LINUX_CAPTURE }, async () => {
  const { forge, completed } = await drive({
    arrange: s => s.editRecord(r => {
      r.issue = { ...r.issue, outcome: "changed", source_id: "issue:" + REGISTERED_SECRET };
      r.prds = r.prds.map(x => ({ ...x, outcome: "changed", source_id: "prd:prds/" + REGISTERED_SECRET + ".md" }));
    }),
  });
  assert.equal(completed.length, 1);
  assert.match(forge.pr.description, /Source drift detected/);
  assert.ok(!forge.pr.description.includes(REGISTERED_SECRET), "a registered secret in a source id never reaches the PR body");
});

it("semantic-drift.pr-line-redacts-registered-secret-in-link-paths", { skip: LINUX_CAPTURE }, async () => {
  const { forge, completed } = await drive({
    arrange: s => s.editRecord(r => {
      r.links_added = ["prds/" + REGISTERED_SECRET + "-added.md"];
      r.links_removed = ["prds/" + REGISTERED_SECRET + "-removed.md"];
    }),
  });
  assert.equal(completed.length, 1);
  assert.match(forge.pr.description, /PRD link added to the issue/);
  assert.match(forge.pr.description, /PRD link removed from the issue/);
  assert.ok(!forge.pr.description.includes(REGISTERED_SECRET), "a registered secret in a link path never reaches the PR body");
});

it("semantic-drift.unknown-issue-source-alone-discloses-in-the-pr", { skip: LINUX_CAPTURE }, async () => {
  const { forge, completed, server } = await drive({ arrange: s => s.driftIssue({ outcome: "unknown", reason: "timeout" }) });
  assert.equal(completed.length, 1);
  const publication = server.stored.find(r => r.phase === "publication")!;
  assert.deepEqual([publication.links_added, publication.links_removed], [[], []], "no link change in this record");
  assert.ok(publication.prds.every(p => p.outcome === "unchanged"), "the PRDs are unchanged too");
  assert.match(forge.pr.description, /Source drift detected/, "an unknown source alone is a contradiction");
});

it("semantic-drift hasDriftContradiction: unknown alone, and links alone, are contradictions", () => {
  const base: SemanticObservationRecord = {
    phase: "publication", pushed_sha: SHA, observed_at: "2030-01-01T00:00:00Z",
    issue: { source_id: "issue:7", frozen_hash: sha256("a"), observed_hash: sha256("a"), outcome: "unchanged" },
    prds: [], links_added: [], links_removed: [],
  };
  assert.equal(hasDriftContradiction(base), false);
  assert.equal(hasDriftContradiction({ ...base, issue: { ...base.issue, outcome: "unknown", reason: "timeout", observed_hash: undefined } }), true);
  assert.equal(hasDriftContradiction({ ...base, links_added: ["prds/1-a.md"] }), true);
  assert.equal(hasDriftContradiction({ ...base, links_removed: ["prds/1-a.md"] }), true);
});

// ── a SHA-256 object format is accepted wherever a SHA-1 one is ─────────────────────────────────

it("semantic-drift accepts 40 and 64 hex pushed shas and nothing else, in the record and the runner", async () => {
  const record = (pushed_sha: string) => ({
    phase: "publication", pushed_sha, observed_at: "2030-01-01T00:00:00Z",
    issue: { source_id: "issue:1", frozen_hash: sha256("a"), outcome: "unchanged" }, prds: [], links_added: [], links_removed: [],
  });
  for (const ok of ["a".repeat(40), "0123456789abcdef".repeat(4)]) assert.ok(semanticObservationRecordSchema.safeParse(record(ok)).success, ok);
  for (const bad of ["a".repeat(39), "a".repeat(41), "a".repeat(63), "a".repeat(65), "A".repeat(40), "-" + "a".repeat(39), "-" + "a".repeat(63)]) {
    assert.ok(!semanticObservationRecordSchema.safeParse(record(bad)).success, bad);
  }
  // The runner side: a 64-hex pushed sha is read at that exact ref and posted.
  const sha64 = "b".repeat(64);
  const reads: string[] = [];
  const posted: unknown[] = [];
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  const prior = git.readBare;
  git.readBare = (async (_b: string, args: readonly string[]) => { reads.push(String(args[1])); return { text: PRD_V1, truncated: false, bytes: Buffer.from(PRD_V1) }; }) as typeof git.readBare;
  client.postSemanticObservation = (async (_id: string, _gen: number, request: unknown) => { posted.push(request); throw new Error("stop"); }) as typeof client.postSemanticObservation;
  const flight = { runId: "run-1", claimGeneration: 1, runLog: { warn() {} }, batcher: { emit() {} }, redactText: (t: string) => t,
    semanticDrift: { barePath: "/bare", headSha: "a".repeat(40), sources: [{ id: "prd:" + PRD, kind: "prd", hash: sha256(PRD_V1), completeness: "complete" }] } };
  try {
    await (r as unknown as { observeSemanticDrift(f: unknown, t: unknown): Promise<unknown> }).observeSemanticDrift(flight, { phase: "publication", pushed_sha: sha64 });
  } finally { git.readBare = prior; }
  assert.deepEqual(reads, [sha64 + ":" + PRD]);
  assert.equal((posted[0] as { pushed_sha: string }).pushed_sha, sha64);
});

// ── the status wire carries the frozen PRD refs, strictly ───────────────────────────────────────

it("semantic-drift status wire: prd_sources is optional, bounded, prd-only and strict", () => {
  const base = { outcome: "unavailable", candidate_digest: "", source_digest: "", criteria_digest: "", reviews: 0, invocations: 0,
    charged_tokens: 0, remaining_tokens: 0, deadline: "2030-01-01T00:00:00Z" };
  const ref = (i = 0) => ({ id: "prd:prds/" + i + "-x.md", kind: "prd", provenance: "repo:test", hash: sha256("x" + i), completeness: "complete" });
  assert.ok(semanticCoverageStatusSchema.safeParse(base).success, "absent is accepted");
  assert.ok(semanticCoverageStatusSchema.safeParse({ ...base, prd_sources: [] }).success);
  assert.ok(semanticCoverageStatusSchema.safeParse({ ...base, prd_sources: Array.from({ length: 8 }, (_, i) => ref(i)) }).success);
  assert.ok(!semanticCoverageStatusSchema.safeParse({ ...base, prd_sources: Array.from({ length: 9 }, (_, i) => ref(i)) }).success, "more than 8");
  assert.ok(!semanticCoverageStatusSchema.safeParse({ ...base, prd_sources: [{ ...ref(), kind: "issue" }] }).success, "kind issue");
  assert.ok(!semanticCoverageStatusSchema.safeParse({ ...base, prd_sources: [{ ...ref(), extra: 1 }] }).success, "extra key");
});

// ── the reader and the client, without a run ────────────────────────────────────────────────────

const SHA = "a".repeat(40);
const frozen = (id: string, completeness = "complete") => ({ id, kind: "prd", hash: sha256(PRD_V1), completeness });
const reader = (impl: (args: readonly string[]) => Promise<{ text: string; truncated: boolean; bytes?: Uint8Array }>) =>
  ({ readBare: (_b: string, args: readonly string[]) => impl(args) });
const ok = (s: string) => ({ text: s, truncated: false, bytes: Buffer.from(s) });
const observe = (r: ReturnType<typeof reader>, sources: ReturnType<typeof frozen>[], timeoutMs = 1000) =>
  observePrdSources(r, { barePath: "/bare", sha: SHA, sources, timeoutMs });

it("semantic-drift reader maps each read outcome to one explicit result and never reads claim text", async () => {
  const reads: string[] = [];
  const out = await observe(reader(async args => {
    reads.push(String(args[1]));
    const file = String(args[1]).slice(SHA.length + 1);
    if (file === "prds/same.md") return ok(PRD_V1);
    if (file === "prds/diff.md") return ok(PRD_V2);
    if (file === "prds/gone.md") throw new Error(`fatal: path '${file}' does not exist in '${SHA}'`);
    if (file === "prds/big.md") return { text: "", truncated: true, bytes: Buffer.alloc(0) };
    if (file === "prds/slow.md") throw new Error("git show exceeded 10ms");
    if (file === "prds/text-only.md") return { text: "x", truncated: false };
    if (file === "prds/stale-baseline.md") return ok(PRD_V1);
    throw new Error("fatal: bad object");
  }), ["same", "diff", "gone", "big", "slow", "text-only", "stale-baseline", "boom"].map(n => frozen("prd:prds/" + n + ".md", n === "stale-baseline" ? "unknown" : "complete")));
  const by = Object.fromEntries(out.map(x => [x.source_id.slice(9, -3), x]));
  assert.deepEqual(by["same"], { source_id: "prd:prds/same.md", observed_hash: sha256(PRD_V1), ref: SHA, outcome: "unchanged" });
  assert.deepEqual(by["diff"], { source_id: "prd:prds/diff.md", observed_hash: sha256(PRD_V2), ref: SHA, outcome: "changed" });
  for (const [name, reason] of [["gone", "missing"], ["big", "truncated"], ["slow", "timeout"], ["text-only", "unreadable"], ["stale-baseline", "invalid"], ["boom", "unreadable"]] as const) {
    assert.deepEqual(by[name], { source_id: `prd:prds/${name}.md`, ref: SHA, outcome: "unknown", reason }, name);
  }
  assert.ok(reads.every(r => r.startsWith(SHA + ":prds/")), "every read names the exact SHA");
});

it("semantic-drift reader reports an unsafe source id as invalid without reading, and exhausts the shared deadline as timeout", async () => {
  let reads = 0;
  const slow = reader(async () => { reads++; await new Promise(r => setTimeout(r, 40)); return ok(PRD_V1); });
  const out = await observe(slow, [frozen("prd:unknown:1"), frozen("prd:../x.md"), frozen("prd:prds/a.md"), frozen("prd:prds/b.md")], 30);
  assert.deepEqual(out.map(x => x.reason), ["invalid", "invalid", undefined, "timeout"]);
  assert.equal(reads, 1);
});

it("semantic-drift client posts the fenced request and decodes a strict bounded record", async () => {
  const record: SemanticObservationRecord = {
    phase: "publication", pushed_sha: SHA, observed_at: "2030-01-01T00:00:00Z",
    issue: { source_id: "issue:1", frozen_hash: sha256("a"), observed_hash: sha256("b"), outcome: "changed" },
    prds: [], links_added: ["prds/1-a.md"], links_removed: [],
  };
  let seen: { url: string; body: Record<string, unknown> } | undefined;
  let answer: unknown = record;
  const c = new WorkerClient("http://example.test", "tkn", "test", nullLogger(), {
    fetch: async (input, init) => {
      seen = { url: String(input), body: JSON.parse(String(init?.body)) };
      return new Response(JSON.stringify(answer), { status: 200, headers: { "content-type": "application/json" } });
    },
  });
  const request = { phase: "publication" as const, pushed_sha: SHA, prd_sources: [] };
  assert.deepEqual(await c.postSemanticObservation("run-1", 3, request), record);
  assert.match(seen!.url, /\/runs\/run-1\/semantic\/observation$/);
  assert.deepEqual(seen!.body, { claim_generation: 3, ...request });
  await assert.rejects(c.postSemanticObservation("run-1", 0, request), /invalid semantic claim generation/);
  answer = { ...record, surprise: true };
  await assert.rejects(c.postSemanticObservation("run-1", 3, request));
  answer = { ...record, links_added: Array.from({ length: 9 }, (_, i) => `prds/${i}-a.md`) };
  await assert.rejects(c.postSemanticObservation("run-1", 3, request));
});

// ── the PR line ─────────────────────────────────────────────────────────────────────────────────

function renderWith(record: SemanticObservationRecord): string {
  return renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: false, semanticDrift: record });
}
const clean: SemanticObservationRecord = {
  phase: "publication", pushed_sha: SHA, observed_at: "2030-01-01T00:00:00Z",
  issue: { source_id: "issue:7", frozen_hash: sha256("a"), observed_hash: sha256("a"), outcome: "unchanged" },
  prds: [], links_added: [], links_removed: [],
};

it("semantic-drift PR line: absent for a clean record, escaped and bounded for a contradiction, never closing", () => {
  assert.ok(!/Source drift/.test(renderWith(clean)));
  const long = "prds/" + "a".repeat(400) + ".md";
  const hostile = "prds/@everyone-`x`-Closes-#7.md";
  const worst: SemanticObservationRecord = {
    ...clean,
    issue: { ...clean.issue, outcome: "unknown", reason: "timeout", observed_hash: undefined },
    prds: Array.from({ length: 8 }, (_, i) => ({ source_id: `prd:${long}${i}`, frozen_hash: sha256("a"), ref: SHA, outcome: "changed" as const })),
    links_added: Array.from({ length: 8 }, () => hostile), links_removed: Array.from({ length: 8 }, () => long),
    links_truncated: true,
  };
  const body = renderWith(worst);
  assert.match(body, /Source drift detected/);
  assert.match(body, /the approved contract is unchanged and needs owner review/i);
  assert.ok(!/Closes #7/.test(body), "no closing keyword is added");
  assert.ok(!body.includes("@everyone"), "a mention is broken");
  assert.ok(body.length < 12_000, `bounded: ${body.length}`);
  assert.match(renderWith({ ...clean, links_removed: ["prds/2-b.md"] }), /PRD link removed from the issue/);
});
