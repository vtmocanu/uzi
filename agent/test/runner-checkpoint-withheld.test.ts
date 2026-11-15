// Issue #4257 m2: the worker's content-free "checkpoint publication withheld" diagnostic.
// Real git and the real runner against the fake api; the client seam records what the runner
// asked to send and answers like a server applying the report_seq order rule.
import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { RunContext } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import { CHECKPOINT_PUBLICATION_WITHHELD_FEATURE } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runnerWith, homeDir } from "./runner-harness.js";

installHarness();

type Ack = "recorded" | "cleared" | "refused" | "unrecorded";
type Pub = { state: "withheld" | "cleared"; since?: string; report_seq: number };
type Flight = Record<string, unknown> & {
  runLog: Logger; runId: string;
  secretRemediation?: { attempts: number; flaggedCommits?: string[]; flaggedFindings?: unknown[]; known?: unknown };
  checkpointWithheld?: { episode: number; since: string; phase: "withheld" | "clear_pending" };
  withheldLastSent?: { episode: number; state: "withheld" | "cleared"; report_seq: number };
  trustedFindingSeq?: number;
  trustedFindingEvidence?: { runId: string; barePath: string; branch: string };
  trustedFindingRetired?: boolean;
  reportState: (body: { status: string }, signal?: AbortSignal) => Promise<unknown>;
};
type Subject = {
  publishCheckpointOutcome: (f: Flight, bare: string, branch: string, ...rest: unknown[]) => Promise<{ published: boolean; reason?: string }>;
  persistRemediationScan: (...a: unknown[]) => Promise<void>;
  noteMidRunTrigger: (...a: unknown[]) => Promise<void>;
  checkpointRemediationHold: (...a: unknown[]) => Promise<boolean>;
  ciConfigGuardBlocksPublication: (...a: unknown[]) => Promise<boolean>;
  runSecretRemediationGate: (f: Flight, ...rest: unknown[]) => Promise<unknown>;
  noteTrustedFinding: (f: Flight, barePath: string | undefined, branch: string | undefined,
    source?: "scan" | "record") => void;
  settleCheckpointPublication: (f: Flight, sent: { episode: number; state: "withheld" | "cleared"; report_seq: number },
    answer: Ack) => void;
  noteCheckpointWithheld: (f: Flight, gated: boolean) => void;
  bootstrapRemediationRestrictions: (f: Flight, barePath?: string, branch?: string) => Promise<{ kind: string; enrolled?: boolean }>;
  restoreRemediationRestrictions: (f: Flight) => Promise<unknown>;
  scanRemediationRange: (...a: unknown[]) => Promise<unknown>;
  persistRemediationRestrictions: (...a: unknown[]) => Promise<void>;
};

const PATH_MARK = "src/zz-distinct-path-7f3a.txt";
const RULE_MARK = "zz-distinct-rule-91c";

function commit(tree: string, text = "fixture\n", file = "remembered.txt"): string {
  fs.writeFileSync(path.join(tree, file), text);
  execFileSync("git", ["-C", tree, "add", file]);
  execFileSync("git", ["-C", tree, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"]);
  return execFileSync("git", ["-C", tree, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
}

/** A recording seam in front of the real client, answering like the D3 server. */
function seam(opts: { delay?: (pub: Pub) => Promise<void> | undefined; lose?: (pub: Pub) => boolean; answer?: (pub: Pub, natural: Ack) => Ack } = {}) {
  client.protocolFeatures = [CHECKPOINT_PUBLICATION_WITHHELD_FEATURE];
  const sent: Pub[] = [];
  const server = { lastSeq: 0, fact: undefined as Pub["state"] | undefined, outcomes: new Map<number, Ack>() };
  const real = client.reportState.bind(client);
  client.reportState = async (runId, body, signal, options) => {
    const pub = options?.checkpointPublication as Pub | undefined;
    const ack = await real(runId, body, signal, options);
    if (!pub) return ack;
    sent.push({ ...pub });
    let natural: Ack;
    const prior = server.outcomes.get(pub.report_seq);
    if (prior) natural = prior;
    else if (pub.report_seq > server.lastSeq) {
      server.lastSeq = pub.report_seq;
      server.fact = pub.state;
      natural = pub.state === "withheld" ? "recorded" : "cleared";
    } else natural = "refused";
    server.outcomes.set(pub.report_seq, natural);
    await opts.delay?.(pub);
    if (opts.lose?.(pub)) throw new Error("lost ACK");
    ack.checkpointPublication = opts.answer ? opts.answer(pub, natural) : natural;
    return ack;
  };
  return { sent, server };
}

async function until(cond: () => boolean, what: string): Promise<void> {
  const deadline = Date.now() + 8000;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise(r => setTimeout(r, 5));
  }
}
const settle = () => new Promise(r => setTimeout(r, 60));

async function withFlight(
  body: (ctx: RunContext, f: Flight, r: Subject, runner: RunRunner) => Promise<void>,
  opts: { bypassBody?: true } = {},
): Promise<void> {
  let flight: Flight | undefined;
  let failure: unknown;
  let entered = false;
  let completed = false;
  const { gitlab } = fakeGitlab();
  const runner = runnerWith(() => ({
    homeDir,
    executor: { run: async (ctx: RunContext) => {
      if (opts.bypassBody) return { branch: ctx.branch }; // Test seam: the helper must notice the body never ran.
      try {
        entered = true;
        await ctx.secretRemediationGate!();
        assert.ok(flight);
        await body(ctx, flight, subject, runner);
        completed = true;
      } catch (e) { failure = e; throw e; }
      return { branch: ctx.branch };
    } },
  }), gitlab, undefined, nullLogger(), { checkpointIntervalMs: 60_000 });
  const subject = runner as unknown as Subject;
  const gate = subject.runSecretRemediationGate.bind(runner);
  subject.runSecretRemediationGate = async (f, ...rest) => { flight = f; return gate(f, ...rest); };
  await runner.execute(gitlabClaim(4257, { wait_on_limit: true }));
  if (failure) throw failure;
  assert.ok(entered, "executor body was entered");
  assert.ok(completed, "scenario body completed");
}

it("withFlight rejects when the scenario body never runs", async () => {
  await assert.rejects(withFlight(async () => {}, { bypassBody: true }), /executor body was entered/);
});

function uploads(): string[] {
  const tips: string[] = [];
  client.publishCheckpoint = async (_id, tip, pack) => {
    tips.push(tip);
    for await (const _chunk of pack) { /* drain real producer */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
  };
  return tips;
}

function trustedFinding(sha: string, file = "remembered.txt", ruleId = "github-pat") {
  git.secretScanCheckpointRange = async () => ({ trusted: true, findings: [{ file, startLine: 1, commit: sha, ruleId }] });
}

it("a first trusted finding at the milestone scan reports withheld on the first attempt, with no second checkpoint attempt", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    let attempts = 0;
    const publish = r.publishCheckpointOutcome.bind(r);
    r.publishCheckpointOutcome = async (...a) => { attempts++; return publish(...a); };
    trustedFinding(commit(ctx.worktreePath));
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
    assert.match(sent[0]!.since!, /^\d{4}-\d\d-\d\dT[\d:.]+Z$/);
    assert.equal(sent[0]!.report_seq, 1);
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    assert.equal(attempts, 0, "the scan blocked before any publish attempt");
    assert.equal(tips.length, 0);
  });
});

it("a real trusted pre-pack finding records the flight's evidence, so a later gated hold opens its own episode", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    trustedFinding(commit(ctx.worktreePath));
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
    assert.deepEqual(f.trustedFindingEvidence, { runId: f.runId, barePath: f.barePath, branch: f.branch });
    // The episode is retired (as a cleared ACK would), then a gated hold alone starts a new one
    // because the pre-pack site's evidence persists.
    f.checkpointWithheld = undefined;
    r.noteCheckpointWithheld(f, true);
    const current = (): Flight["checkpointWithheld"] => f.checkpointWithheld;
    assert.deepEqual(current() && { e: current()!.episode, p: current()!.phase }, { e: 2, p: "withheld" });
  });
});

it("a trusted finding inside the candidate admission scan records evidence even when persistence rejects", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    // The admission selects its source from the flight's runner clone, which this harness leaves unset.
    const clone = { path: String(f.worktreePath), attemptId: "a1" };
    f.runnerClone = clone;
    // Enrolled restrictions with confirmed facts, so the hold goes through the real admission.
    f.remediationRestrictions = { roots: [], flaggedOIDs: [] };
    r.bootstrapRemediationRestrictions = async () => ({ kind: "confirmed", enrolled: true });
    r.restoreRemediationRestrictions = async () =>
      recoveryRecord(f, { clonePath: clone.path, attemptId: clone.attemptId, ownedTip: sha, flaggedOIDs: [] });
    git.readRemediationRestrictionFacts = async () => FACTS({ enrolled: true }) as never;
    // The admission scan (not the pre-pack scan, which this direct publish never runs) is trusted
    // and finds a secret; persistence of the restriction then fails.
    r.scanRemediationRange = async () => ({
      trusted: true, range: { tipSha: sha }, floorCandidates: [],
      findings: [{ file: "remembered.txt", startLine: 1, commit: sha, ruleId: "github-pat" }],
    });
    r.persistRemediationRestrictions = async () => { throw new Error("persist failed"); };
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
    assert.equal(tips.length, 0);
  });
});

it("a rejecting persistRemediationScan cannot suppress the report", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, _f, r) => {
    r.persistRemediationScan = async () => { throw new Error("persist failed"); };
    trustedFinding(commit(ctx.worktreePath));
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
  });
});

it("a blocking persistRemediationScan: the report is sent while persistence is still pending", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, _f, r) => {
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    r.persistRemediationScan = () => gate;
    trustedFinding(commit(ctx.worktreePath));
    const checkpoint = ctx.checkpoint!({ reap: true });
    try {
      await until(() => sent.length >= 1, "the withheld report while persistence is pending");
      assert.equal(sent[0]!.state, "withheld");
    } finally { release(); }
    await checkpoint;
  });
});

it("a blocking noteMidRunTrigger: the report is sent while the trigger is still pending", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, _f, r) => {
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    r.noteMidRunTrigger = () => gate;
    trustedFinding(commit(ctx.worktreePath));
    const checkpoint = ctx.checkpoint!({ reap: true });
    try {
      await until(() => sent.length >= 1, "the withheld report while the trigger is pending");
      assert.equal(sent[0]!.state, "withheld");
    } finally { release(); }
    await checkpoint;
  });
});

it("an untrusted scan with findings, and a clean scan, set nothing", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f) => {
    const sha = commit(ctx.worktreePath);
    git.secretScanCheckpointRange = async () => ({ trusted: false, reason: "range_invalid", findings: [{ file: "remembered.txt", startLine: 1, commit: sha, ruleId: "github-pat" }] });
    await ctx.checkpoint!({ reap: true });
    await settle();
    assert.equal(f.checkpointWithheld, undefined);
    commit(ctx.worktreePath, "more\n");
    git.secretScanCheckpointRange = async () => ({ trusted: true, findings: [] });
    await ctx.checkpoint!({ reap: true });
    await settle();
    assert.equal(f.checkpointWithheld, undefined);
    assert.equal(sent.length, 0, "no episode, no diagnostic");
  });
});

it("the post-bridge hold (milestone) sets it", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    f.secretRemediation = { attempts: 0, flaggedCommits: [commit(ctx.worktreePath)] };
    r.noteTrustedFinding(f, f.barePath as string, ctx.branch); // the hold this models came from a trusted finding
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
    assert.equal(tips.length, 0);
  });
});

it("a best-effort sink (pause park) sets it", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    f.secretRemediation = { attempts: 0, flaggedCommits: [commit(ctx.worktreePath)] };
    r.noteTrustedFinding(f, f.barePath as string, ctx.branch);
    assert.equal(await ctx.parkForPause!({ completedCount: 1, total: 2 }), false);
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
  });
});

it("the hold sites need a restriction or remediation record on the flight", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    commit(ctx.worktreePath);
    f.secretRemediation = undefined;
    f.remediationRestrictions = undefined;
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).published, true);
    await settle();
    assert.equal(sent.length, 0);
    // Force the hold outcome with no record on the flight: the gate in noteCheckpointWithheld,
    // not the absence of a hold, must keep the diagnostic silent.
    r.checkpointRemediationHold = async () => true;
    commit(ctx.worktreePath, "again\n");
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    await settle();
    assert.equal(sent.length, 0);
    assert.equal(f.checkpointWithheld, undefined);
  });
});

/** Start an episode through the best-effort publish hold, then lift the hold so a publish is admitted. */
async function startEpisode(ctx: RunContext, f: Flight, r: Subject): Promise<string> {
  const sha = commit(ctx.worktreePath);
  f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
  r.noteTrustedFinding(f, f.barePath as string, ctx.branch);
  assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
  return sha;
}

const FACTS = (over: Record<string, unknown>) => ({ enrolled: false, blocked: false, flaggedOIDs: [] as string[], roots: [] as string[], fingerprint: "fp", ...over });

it("a failed restriction-facts read latches an empty restriction but reports no withheld diagnostic", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    git.readRemediationRestrictionFacts = async () => { throw new Error("facts read failed"); };
    commit(ctx.worktreePath);
    // The hold comes from the real failed read (the empty latch), not from a forced hold stub.
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    assert.ok(f.remediationRestrictions, "the failed read latched an empty restriction");
    assert.equal(tips.length, 0, "publication stayed held: nothing was uploaded");
    await settle();
    assert.equal(sent.length, 0);
    assert.equal(f.checkpointWithheld, undefined);
    assert.equal(f.trustedFindingEvidence, undefined);
  });
});

it("confirmed enrolled facts with flagged OIDs for this bare/branch/run set the evidence and report withheld", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    git.readRemediationRestrictionFacts = async () => FACTS({ enrolled: true, flaggedOIDs: [sha], roots: [sha] }) as never;
    assert.deepEqual(await r.bootstrapRemediationRestrictions(f, String(f.barePath), ctx.branch), { kind: "confirmed", enrolled: true });
    assert.deepEqual(f.trustedFindingEvidence, { runId: f.runId, barePath: f.barePath, branch: ctx.branch });
    r.noteCheckpointWithheld(f, true);
    await until(() => sent.length >= 1, "the withheld report");
    assert.equal(sent[0]!.state, "withheld");
  });
});

it("blocked facts with flagged OIDs hold but leave no evidence and no diagnostic", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    git.readRemediationRestrictionFacts = async () => FACTS({ enrolled: true, blocked: true, flaggedOIDs: [sha], roots: [sha] }) as never;
    assert.deepEqual(await r.bootstrapRemediationRestrictions(f, String(f.barePath), ctx.branch), { kind: "unavailable" });
    assert.ok(f.remediationRestrictions, "custody-only facts still latch the hold");
    assert.equal(f.trustedFindingEvidence, undefined);
    commit(ctx.worktreePath, "again\n");
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    await settle();
    assert.equal(sent.length, 0);
    assert.equal(f.checkpointWithheld, undefined);
  });
});

it("evidence for a different branch, bare or run never enables the diagnostic", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    r.noteTrustedFinding(f, String(f.barePath), "some/other-branch");
    r.noteTrustedFinding(f, "/elsewhere/bare.git", ctx.branch);
    r.noteTrustedFinding(f, undefined, ctx.branch);
    r.noteTrustedFinding(f, String(f.barePath), undefined);
    assert.equal(f.trustedFindingEvidence, undefined);
    // Evidence recorded for this identity stops counting once the flight's identity differs.
    r.noteTrustedFinding(f, String(f.barePath), ctx.branch);
    assert.ok(f.trustedFindingEvidence);
    const original = f.runId;
    f.runId = "another-run";
    r.noteCheckpointWithheld(f, true);
    f.runId = original;
    f.branch = "moved/branch";
    r.noteCheckpointWithheld(f, true);
    await settle();
    assert.equal(sent.length, 0);
    assert.equal(f.checkpointWithheld, undefined);
  });
});

function recoveryRecord(f: Flight, over: Record<string, unknown>) {
  return { version: 1, blocked: false, runId: f.runId, branch: f.branch, clonePath: f.worktreePath, attemptId: "a1", sequence: 1,
    ownedTip: "0".repeat(40), flaggedOIDs: ["1".repeat(40)], edges: [], ...over };
}

it("restoring a matching recovery record with flagged OIDs sets the evidence", async () => {
  await withFlight(async (_ctx, f, r) => {
    git.readRuntimeRemediationRecovery = async () => recoveryRecord(f, {}) as never;
    await r.restoreRemediationRestrictions(f);
    assert.deepEqual(f.trustedFindingEvidence, { runId: f.runId, barePath: f.barePath, branch: f.branch });
  });
});

it("restoring a mismatched, empty or unreadable recovery record sets no evidence", async () => {
  await withFlight(async (_ctx, f, r) => {
    git.readRuntimeRemediationRecovery = async () => recoveryRecord(f, { runId: "someone-else" }) as never;
    await r.restoreRemediationRestrictions(f);
    assert.equal(f.trustedFindingEvidence, undefined);
    git.readRuntimeRemediationRecovery = async () => recoveryRecord(f, { branch: "some/other-branch" }) as never;
    await r.restoreRemediationRestrictions(f);
    assert.equal(f.trustedFindingEvidence, undefined);
    git.readRuntimeRemediationRecovery = async () => recoveryRecord(f, { flaggedOIDs: [] }) as never;
    await r.restoreRemediationRestrictions(f);
    assert.equal(f.trustedFindingEvidence, undefined);
    git.readRuntimeRemediationRecovery = async () => { throw new Error("journal unreadable"); };
    await assert.rejects(r.restoreRemediationRestrictions(f), /journal unreadable/);
    assert.equal(f.trustedFindingEvidence, undefined);
  });
});

it("an admitted and accepted publication clears; the cleared ACK retires the episode", async () => {
  const { sent, server } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => sent.length >= 2, "cleared");
    assert.equal(sent[1]!.state, "cleared");
    assert.equal(sent[1]!.since, undefined);
    assert.equal(tips.length, 1);
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(server.fact, "cleared");
    await f.reportState({ status: "running" });
    assert.equal(sent.length, 2, "no episode, nothing more sent");
  });
});

it("a cleared episode's evidence never re-enables the flag: a later failed facts read holds silently", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => sent.length >= 2, "cleared");
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    git.readRemediationRestrictionFacts = async () => { throw new Error("facts read failed"); };
    commit(ctx.worktreePath, "later\n");
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    assert.equal(tips.length, 1, "the held publish uploaded nothing");
    await settle();
    assert.equal(sent.length, 2, JSON.stringify(sent));
    assert.equal(f.checkpointWithheld, undefined);
    assert.equal(f.trustedFindingEvidence, undefined);
  });
});

it("a scan finding recorded after the episode opened, before admission, is resolved by the accepted publication", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    r.noteTrustedFinding(f, String(f.barePath), ctx.branch, "scan"); // arrives after the episode opened, before admission
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => sent.length >= 2 && sent[1]!.state === "cleared", "cleared");
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(f.trustedFindingEvidence, undefined, "the admitted publication resolved this evidence");
    git.readRemediationRestrictionFacts = async () => { throw new Error("facts read failed"); };
    commit(ctx.worktreePath, "later\n");
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    await settle();
    assert.equal(sent.length, 2, JSON.stringify(sent));
    assert.equal(tips.length, 1, "the held publish uploaded nothing");
  });
});

it("a trusted finding that arrives after admission keeps the episode withheld, and an older cleared answer cannot retire it", async () => {
  const { sent } = seam();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    client.publishCheckpoint = async (_id, _tip, pack) => {
      for await (const _chunk of pack) { /* drain real producer */ }
      r.noteTrustedFinding(f, String(f.barePath), ctx.branch, "scan"); // after the admission guard ran
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
    };
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await settle();
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    assert.ok(f.trustedFindingEvidence, "evidence newer than the admission is kept");
    assert.ok(!sent.some(s => s.state === "cleared"), JSON.stringify(sent));
    await f.reportState({ status: "running" });
    assert.equal(sent.at(-1)!.state, "withheld", JSON.stringify(sent));
    const episode = f.checkpointWithheld!.episode;
    r.settleCheckpointPublication(f, { episode, state: "cleared", report_seq: 1 }, "cleared");
    r.settleCheckpointPublication(f, f.withheldLastSent!, "cleared");
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    assert.equal(f.checkpointWithheld?.episode, episode);
  });
});

it("a fresh trusted scan re-finding a previously resolved commit raises the diagnostic again", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    trustedFinding(sha);
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "withheld");
    assert.equal(f.checkpointWithheld?.episode, 1);
    // Resolve it: the hold is released and an accepted publication clears and retires the episode.
    f.secretRemediation = undefined;
    f.remediationRestrictions = undefined;
    git.secretScanCheckpointRange = async () => ({ trusted: true, findings: [] });
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(f.trustedFindingEvidence, undefined);
    // New work, and a fresh trusted scan finds the SAME commit again.
    commit(ctx.worktreePath, "new work\n", "other.txt");
    trustedFinding(sha);
    await ctx.checkpoint!({ reap: true });
    await until(() => f.checkpointWithheld?.episode === 2, "episode 2");
    assert.ok(f.trustedFindingEvidence, "a fresh scan is new evidence even for a known commit");
    assert.equal(f.trustedFindingRetired, false);
    await until(() => sent.length >= 3 && sent.at(-1)!.state === "withheld", "a withheld send for episode 2");
  });
});

it("a record observation never advances the evidence version", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    const before = f.trustedFindingSeq;
    client.publishCheckpoint = async (_id, _tip, pack) => {
      for await (const _chunk of pack) { /* drain real producer */ }
      r.noteTrustedFinding(f, String(f.barePath), ctx.branch, "record"); // after the admission guard ran
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
    };
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    assert.equal(f.trustedFindingSeq, before, "a record call does not advance the version");
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(f.trustedFindingEvidence, undefined);
  });
});

it("the done gate's replay of remembered flagged commits is a record: it neither revives retired evidence nor advances the version", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(f.trustedFindingEvidence, undefined);
    const seq = f.trustedFindingSeq;
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha],
      flaggedFindings: [{ file: "remembered.txt", startLine: 1, commit: sha, ruleId: "github-pat" }] };
    git.secretScanCheckpointRange = async () => ({ trusted: true, findings: [] });
    const ancestryCalls: unknown[][] = [];
    const ancestry = git.ancestry.bind(git);
    git.ancestry = (async (...a: Parameters<typeof ancestry>) => { ancestryCalls.push(a); return ancestry(...a); }) as typeof git.ancestry;
    await ctx.secretRemediationGate!();
    assert.ok(ancestryCalls.some(c => c.includes(sha)), "the remembered commit was checked for reachability");
    assert.ok(f.secretRemediation?.known, "the replay path ran, not the clean proceed");
    assert.equal(f.trustedFindingEvidence, undefined);
    assert.equal(f.trustedFindingSeq, seq);
  });
});

it("historical provenance (confirmed enrolled facts, a journal record) never revives retired evidence; only a scan finding does", async () => {
  const { sent } = seam();
  const tips = uploads();
  await withFlight(async (ctx, f, r) => {
    const sha = await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    await until(() => sent.length >= 2, "cleared");
    await until(() => f.checkpointWithheld === undefined, "episode retired by the cleared ACK");
    assert.equal(f.trustedFindingEvidence, undefined);
    // An accepted publication through enrolled admission cannot be driven here: checkpointPack reads
    // the restriction facts itself from a real recovery journal, which this harness does not write.
    // The record sites that such a later admission (or a restart) would hit are driven directly.
    const bare = String(f.barePath);
    git.readRemediationRestrictionFacts = async () => FACTS({ enrolled: true, flaggedOIDs: [sha], roots: [sha] }) as never;
    assert.deepEqual(await r.bootstrapRemediationRestrictions(f, bare, ctx.branch), { kind: "confirmed", enrolled: true });
    assert.equal(f.trustedFindingEvidence, undefined, "confirmed enrolled facts did not revive the evidence");
    f.runnerClone = { path: String(f.worktreePath), attemptId: "a1" };
    git.readRuntimeRemediationRecovery = async () => recoveryRecord(f, { flaggedOIDs: [sha] }) as never;
    await r.restoreRemediationRestrictions(f);
    assert.equal(f.trustedFindingEvidence, undefined, "a journal record did not revive the evidence");
    f.remediationRestrictions = undefined;
    git.readRemediationRestrictionFacts = async () => { throw new Error("facts read failed"); };
    commit(ctx.worktreePath, "later\n");
    assert.equal((await r.publishCheckpointOutcome(f, bare, ctx.branch)).reason, "secret_remediation_pending");
    await settle();
    assert.equal(tips.length, 1, "the held publish uploaded nothing");
    assert.equal(sent.length, 2, JSON.stringify(sent));
    assert.equal(f.checkpointWithheld, undefined);
    // A newer trusted scan finding does re-enable it.
    r.noteTrustedFinding(f, bare, ctx.branch, "scan");
    assert.ok(f.trustedFindingEvidence);
    assert.equal(f.trustedFindingRetired, false);
  });
});

it("a rejected publish, a skipped publish and a ci-guard skip do not clear", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    f.secretRemediation = undefined;
    client.publishCheckpoint = async (_id, _tip, pack) => {
      for await (const _chunk of pack) { /* drain */ }
      return { ok: false, httpStatus: 400, body: {} } as never;
    };
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "rejected");
    client.publishCheckpoint = async (_id, _tip, pack) => {
      for await (const _chunk of pack) { /* drain */ }
      return { ok: true, body: { published: false, skipped: "no_enrollment" } } as never;
    };
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "skipped");
    r.ciConfigGuardBlocksPublication = async () => true;
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "skipped");
    await settle();
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    assert.deepEqual(sent.map(s => s.state), ["withheld"]);
  });
});

it("a later commit that removes the flagged content does not clear: the hold still holds", async () => {
  const { sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "withheld");
    fs.rmSync(path.join(ctx.worktreePath, "remembered.txt"));
    execFileSync("git", ["-C", ctx.worktreePath, "add", "-A"]);
    execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "remove"]);
    await ctx.checkpoint!({ reap: true });
    await settle();
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    // The checkpoint's own running report re-carries the current fact; it is never a clear.
    assert.ok(sent.length >= 1 && sent.every(s => s.state === "withheld"), JSON.stringify(sent));
  });
});

it("report_seq strictly increases across every diagnostic send, retries included", async () => {
  const { sent } = seam({ lose: pub => pub.report_seq === 2 });
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    f.secretRemediation = undefined;
    await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch); // clear, its ACK is lost
    await until(() => sent.length >= 2, "the lost clear");
    for (let i = 0; i < 3; i++) await f.reportState({ status: "running" }).catch(() => undefined);
    const seqs = sent.map(s => s.report_seq);
    assert.ok(seqs.length >= 3, JSON.stringify(sent));
    for (let i = 1; i < seqs.length; i++) assert.ok(seqs[i]! > seqs[i - 1]!, JSON.stringify(seqs));
  });
});

it("a lost clear ACK leaves clear_pending, and the next running report re-sends the clear with a higher seq", async () => {
  const { sent } = seam({ lose: pub => pub.report_seq === 2 });
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    f.secretRemediation = undefined;
    await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch);
    await until(() => sent.length >= 2, "the lost clear");
    assert.equal(f.checkpointWithheld?.phase, "clear_pending");
    await f.reportState({ status: "running" });
    assert.deepEqual(sent.map(s => [s.state, s.report_seq]), [["withheld", 1], ["cleared", 2], ["cleared", 3]]);
    await until(() => f.checkpointWithheld === undefined, "retired by the retried clear's ACK");
  });
});

it("a lost clear ACK followed by a new trusted finding: the retry is rebuilt as withheld with a higher seq", async () => {
  const { sent, server } = seam({ lose: pub => pub.report_seq === 2 });
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    f.secretRemediation = undefined;
    await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch);
    await until(() => sent.length >= 2, "the lost clear");
    trustedFinding(commit(ctx.worktreePath, "second\n"));
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 3, "the rebuilt withheld");
    assert.equal(sent[2]!.state, "withheld");
    assert.ok(sent[2]!.report_seq > sent[1]!.report_seq);
    assert.equal(f.checkpointWithheld?.episode, 2);
    assert.equal(server.fact, "withheld");
  });
});

for (const stale of ["cleared", "refused"] as const) {
  it(`delayed clear ACK (${stale}): a newer finding's episode survives the old ACK`, async () => {
    let release!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    const { sent, server } = seam({
      delay: pub => (pub.state === "cleared" ? held : undefined),
      answer: (pub, natural) => (pub.state === "cleared" ? stale : natural),
    });
    uploads();
    await withFlight(async (ctx, f, r) => {
      await startEpisode(ctx, f, r);
      f.secretRemediation = undefined;
      await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch); // clear send, ACK held
      await until(() => sent.length >= 2, "the held clear");
      trustedFinding(commit(ctx.worktreePath, "second\n"));
      // The new episode starts while the old clear's ACK is outstanding; the checkpoint's own
      // report queues behind the held send, so release only once the episode exists.
      const checkpoint = ctx.checkpoint!({ reap: true });
      await until(() => f.checkpointWithheld?.episode === 2, "the new episode");
      release();
      await checkpoint;
      await until(() => sent.length >= 3, "the new episode's report");
      await settle();
      assert.deepEqual(f.checkpointWithheld && { e: f.checkpointWithheld.episode, p: f.checkpointWithheld.phase }, { e: 2, p: "withheld" });
      assert.equal(server.fact, "withheld");
    });
  });
}

it("a refused ACK for an older send leaves the current episode intact; a refused for the latest drops it", async () => {
  let releaseA!: () => void;
  const heldA = new Promise<void>(resolve => { releaseA = resolve; });
  const { sent } = seam({
    delay: pub => (pub.report_seq === 1 ? heldA : undefined),
    answer: (pub, natural) => (pub.report_seq === 1 ? "refused" : pub.report_seq === 3 ? "refused" : natural),
  });
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => sent.length >= 1, "send A built and held");
    await f.reportState({ status: "running" }); // send B (seq 2), unchained, overtakes A
    await until(() => sent.length >= 2, "send B");
    releaseA(); // A's refused ACK arrives late: not the latest send
    await settle();
    assert.equal(f.checkpointWithheld?.phase, "withheld");
    await f.reportState({ status: "running" }); // seq 3, answered refused and it IS the latest
    await until(() => f.checkpointWithheld === undefined, "dropped by the latest send's refusal");
  });
});

it("the recording server's last applied fact stays withheld until an admitted and accepted publication", async () => {
  const { server, sent } = seam();
  uploads();
  await withFlight(async (ctx, f, r) => {
    await startEpisode(ctx, f, r);
    await until(() => server.fact === "withheld", "withheld applied");
    await ctx.checkpoint!({ reap: true });
    await f.reportState({ status: "running" });
    await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch);
    await settle();
    assert.equal(server.fact, "withheld", JSON.stringify(sent));
    f.secretRemediation = undefined;
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).published, true);
    await until(() => server.fact === "cleared", "cleared applied");
  });
});

it("content-free: no path, rule id or SHA reaches any state report body or feed line", async () => {
  const { sent } = seam();
  uploads();
  let sha = "";
  let feed = "";
  await withFlight(async (ctx, f) => {
    sha = commit(ctx.worktreePath, "x\n", "planted.txt");
    trustedFinding(sha, PATH_MARK, RULE_MARK);
    await ctx.checkpoint!({ reap: true });
    await until(() => sent.length >= 1, "the withheld report");
    await (f.batcher as { flush: () => Promise<void> }).flush();
    // Snapshot the feed while the run is live (teardown lines come later). The pre-existing
    // "Retained heads <sha>" notice is emitted by checkpoint retention whether or not this
    // feature exists and is not part of the diagnostic, so it is excluded.
    feed = api.messages(f.runId).filter(m => !String(m.payload.text).startsWith("Retained heads "))
      .map(m => JSON.stringify(m.payload)).join("\n");
  });
  const surfaces: [string, string][] = [
    // Running reports only: the run's own finalize failure legitimately names its pre-push scan.
    ["running state report bodies", api.states.filter(s => s.body.status === "running").map(s => JSON.stringify(s.body)).join("\n")],
    ["diagnostics handed to the client", JSON.stringify(sent)],
    ["feed lines", feed],
  ];
  assert.ok(sent.length >= 1);
  for (const [name, text] of surfaces)
    for (const marker of [PATH_MARK, RULE_MARK, sha, sha.slice(0, 12)])
      assert.equal(text.includes(marker), false, `${name} leaked ${marker}`);
});
