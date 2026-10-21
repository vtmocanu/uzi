import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { RequestError } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import type { StateRequest } from "../src/protocol.js";
import type { RunRunner } from "../src/runner.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  runnerWith,
} from "./runner-harness.js";
import {
  FakeCodexExecutor,
  VAULT_FEATURE,
  codexRig,
  installRecoveryRootCleanup,
  parkReports,
  statuses,
} from "./runner-codex-sinks-fixture.js";
import { LINUX_CAPTURE, QUIESCENT, fixture } from "./runner-terminal-disk-deferral-fixture.js";

installHarness();
installRecoveryRootCleanup();

// issue #1786 M2 — the recovery handler's exits (handleRecoveryExhausted and
// confirmRunningForCredentialPark) used to clear the clone-keep flag on a cached verdict: a verified
// capture memo, a stale/other-generation read, a terminal status. A cached verdict says nothing
// about the clone at the moment of cleanup, so every such exit now revalidates coverage for the exact
// source and keeps (and records) anything unpreserved. Real Git throughout: each fixture holds an
// unfetched commit, a modified tracked binary and an untracked file, and the assertions read bytes
// back from a copy of the retained clone.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BIN_V1 = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x10, 0x80]);
const BIN_V2 = Buffer.from([0xde, 0xad, 0x00, 0xbe, 0xef, 0x00, 0xff, 0x7f, 0x00]);
const BIN_V3 = Buffer.from([0x7f, 0x45, 0x4c, 0x46, 0x00, 0x00, 0x99]);
const UNTRACKED = "only on this worker\n";
const LATE_UNTRACKED = "written after the capture verified\n";
const DOCKER = "unix:///nonexistent/uzi-test-docker.sock";

function gitIn(dir: string, ...args: string[]): string {
  // PATH is read per call so withSameSecondCommits' git reaches the fixture's commits too.
  return execFileSync("git", ["-C", dir, ...args], { env: { ...GIT_ENV, PATH: process.env.PATH }, encoding: "utf8", stdio: "pipe" }).trim();
}

/** Runs `fn` with a `git` first on PATH that pins every commit to one timestamp, the fixture's and
 *  the runner's alike (the runner's git env carries PATH but no GIT_*_DATE), so two rigs' commits
 *  land in the "same second" deterministically: the #3131 collision, without depending on timing. */
async function withSameSecondCommits<T>(fn: () => Promise<T>): Promise<T> {
  const realGit = execFileSync("sh", ["-c", "command -v git"], { encoding: "utf8" }).trim();
  const quoted = `'${realGit.replaceAll("'", `'\\''`)}'`; // POSIX single quotes: no $ or ` expansion
  const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "uzi-same-second-git-"));
  fs.writeFileSync(path.join(dir, "git"),
    `#!/bin/sh\nGIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" exec ${quoted} "$@"\n`,
    { mode: 0o755 });
  const savedPath = process.env.PATH;
  process.env.PATH = `${dir}${path.delimiter}${savedPath ?? ""}`;
  try {
    return await fn();
  } finally {
    if (savedPath === undefined) delete process.env.PATH; else process.env.PATH = savedPath;
    fs.rmSync(dir, { recursive: true, force: true });
  }
}
function cfgAll(bare: string, key: string): string[] {
  try {
    return gitIn(bare, "config", "--local", "--get-all", key).split("\n").filter((l) => l !== "");
  } catch {
    return [];
  }
}
const bareOf = (): string => git.barePathFor(fx.originPath);
const branchOf = (iid: number): string => `agent/issue-${iid}`;

/** What a recovering operator must find in a retained clone. */
interface Expected {
  head: string;
  blob: Buffer;
  blobMode: number;
  untracked: Record<string, string>;
  /** Commits that must remain reachable objects of the copy. */
  objects?: string[];
}

/** Every rig shares one bare and commits the same files with the same identity, so two rigs whose
 *  commits land in the same second would otherwise mint identical SHAs: the runner's park commit on
 *  top would then be owed by both runs, and retention reports shared_owed_pin (#3131). The sequence
 *  number keeps every rig's history distinct. */
let workerStateSeq = 0;

/** An unfetched commit, a tracked binary modified (bytes and mode) after it, and an untracked file. */
function writeWorkerState(wt: string): Expected {
  fs.mkdirSync(path.join(wt, "assets"), { recursive: true });
  fs.writeFileSync(path.join(wt, "assets", "blob.bin"), BIN_V1);
  gitIn(wt, "add", "assets/blob.bin");
  gitIn(wt, ...IDENT, "commit", "-q", "-m", `unfetched work ${++workerStateSeq}`);
  const head = gitIn(wt, "rev-parse", "HEAD");
  fs.writeFileSync(path.join(wt, "assets", "blob.bin"), BIN_V2);
  fs.chmodSync(path.join(wt, "assets", "blob.bin"), 0o755);
  fs.writeFileSync(path.join(wt, "untracked.txt"), UNTRACKED);
  return { head, blob: BIN_V2, blobMode: 0o755, untracked: { "untracked.txt": UNTRACKED } };
}

type Late = "all" | "commit" | "tracked" | "untracked";

/** Source changes after a verified capture: a new commit, a modified tracked file, a new untracked file. */
function writeLateChange(wt: string, kind: Late = "all"): Expected {
  const objects = [gitIn(wt, "rev-parse", "HEAD")];
  let blob = fs.readFileSync(path.join(wt, "assets", "blob.bin"));
  let blobMode = fs.statSync(path.join(wt, "assets", "blob.bin")).mode & 0o777;
  const untracked: Record<string, string> = {};
  if (kind === "all" || kind === "commit") {
    fs.writeFileSync(path.join(wt, "late.txt"), "late committed work\n");
    gitIn(wt, "add", "late.txt");
    gitIn(wt, ...IDENT, "commit", "-q", "-m", "late commit");
  }
  if (kind === "all" || kind === "tracked") {
    fs.writeFileSync(path.join(wt, "assets", "blob.bin"), BIN_V3);
    fs.chmodSync(path.join(wt, "assets", "blob.bin"), 0o700);
    blob = BIN_V3;
    blobMode = 0o700;
  }
  if (kind === "all" || kind === "untracked") {
    fs.writeFileSync(path.join(wt, "late-untracked.txt"), LATE_UNTRACKED);
    untracked["late-untracked.txt"] = LATE_UNTRACKED;
  }
  return { head: gitIn(wt, "rev-parse", "HEAD"), blob, blobMode, untracked, objects };
}

/** The clone is still there and a byte copy of it yields every expected file and object. */
function assertRecoverable(clone: string, want: Expected, label: string): void {
  assert.ok(fs.existsSync(clone), `${label}: the clone is retained`);
  const exportDir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "uzi-m2-export-"));
  try {
    const copy = path.join(exportDir, "copy");
    fs.cpSync(clone, copy, { recursive: true, verbatimSymlinks: true });
    assert.equal(gitIn(copy, "rev-parse", "HEAD"), want.head, `${label}: the exported HEAD`);
    for (const object of want.objects ?? []) gitIn(copy, "cat-file", "-e", `${object}^{commit}`);
    assert.deepEqual(fs.readFileSync(path.join(copy, "assets", "blob.bin")), want.blob, `${label}: the tracked binary's bytes`);
    assert.equal(fs.statSync(path.join(copy, "assets", "blob.bin")).mode & 0o777, want.blobMode, `${label}: the binary's mode`);
    for (const [name, content] of Object.entries(want.untracked)) {
      assert.equal(fs.readFileSync(path.join(copy, name), "utf8"), content, `${label}: untracked ${name}`);
    }
  } finally {
    fs.rmSync(exportDir, { recursive: true, force: true });
  }
}

/** Run `fn` once, right after the next verified capture's tracking verification returns true. */
function afterVerifiedCapture(fn: (clone: string) => void | Promise<void>): { restore: () => void; fired: () => number; flag: () => boolean } {
  const original = git.verifyRunnerTrackingCovers.bind(git);
  let fired = 0;
  git.verifyRunnerTrackingCovers = (async (...args: Parameters<typeof original>) => {
    const ok = await original(...args);
    if (ok && fired === 0) {
      fired++;
      await fn(args[1]);
    }
    return ok;
  }) as typeof git.verifyRunnerTrackingCovers;
  return { restore: () => { git.verifyRunnerTrackingCovers = original; }, fired: () => fired, flag: () => fired > 0 };
}

/** Every `retainStoppedSource` outcome the runner logged, by exit site. */
function retainedReasons(lines: unknown[]): string[] {
  return (lines as Array<{ msg?: string; reason?: string }>)
    .filter((l) => l.msg === "stopped source retained on this worker; inspect it with the operator channel before any disposal")
    .map((l) => String(l.reason));
}
/** Exits that kept the source WITHOUT a stopped-source record (park and terminal exits keep the plain
 *  journal, the ADR-1197 posture: a resume recaptures the bytes). */
function keptReasons(lines: unknown[]): string[] {
  return (lines as Array<{ msg?: string; reason?: string }>)
    .filter((l) => l.msg === "source kept without a stopped-source record (park or terminal exit); a resume recaptures it")
    .map((l) => String(l.reason));
}
/** What `handleRecoveryExhausted` returned (true = parked), per call. */
function spyHandlerResults(runner: RunRunner): boolean[] {
  const results: boolean[] = [];
  const impl = (runner as unknown as { handleRecoveryExhausted: (...a: unknown[]) => Promise<boolean> }).handleRecoveryExhausted.bind(runner);
  (runner as unknown as { handleRecoveryExhausted: unknown }).handleRecoveryExhausted = async (...a: unknown[]) => {
    const result = await impl(...a);
    results.push(result);
    return result;
  };
  return results;
}
function ledgerStates(iid: number): string[] {
  return cfgAll(bareOf(), `uzi-attempts.${branchOf(iid)}.entry`).map((raw) => (JSON.parse(raw) as { state: string }).state);
}
function journalCount(iid: number): number {
  return cfgAll(bareOf(), `uzi-recovery.${branchOf(iid)}.clone`).length;
}
function coveredReasons(lines: unknown[]): string[] {
  return (lines as Array<{ msg?: string; reason?: string }>)
    .filter((l) => typeof l.msg === "string" && l.msg.startsWith("stopped source: clean and already held"))
    .map((l) => String(l.reason));
}

/** Another run's or generation's fetch-back moves the shared per-branch tracking ref off the captured
 *  head, and this run holds no owed pin for it (a settled pin): nothing in the bare anchors the head. */
function moveSharedTrackingAway(runId: string, iid: number, elsewhere: string): void {
  gitIn(bareOf(), "update-ref", `refs/uzi-runner/${branchOf(iid)}`, elsewhere);
  for (const line of gitIn(bareOf(), "for-each-ref", "--format=%(refname)", `refs/uzi-owed/${runId}/`).split("\n").filter(Boolean)) {
    gitIn(bareOf(), "update-ref", "-d", line);
  }
}

/** The exit sites that called `retainStoppedSource`, in order (the fixture's logger is silent). */
function spyExitSites(runner: RunRunner): string[] {
  const sites: string[] = [];
  const impl = (runner as unknown as { retainStoppedSource: (...a: unknown[]) => Promise<unknown> }).retainStoppedSource.bind(runner);
  (runner as unknown as { retainStoppedSource: unknown }).retainStoppedSource = (...a: unknown[]) => { sites.push(String(a[2])); return impl(...a); };
  return sites;
}

function stopMarkers(runId: string): string[] {
  return cfgAll(bareOf(), `uzi-stopped.${runId}.entry`);
}

// --- the vault-locked (credential-deferred) rig --------------------------------------------------

interface CredRig {
  claim: ReturnType<typeof gitlabClaim>;
  runner: RunRunner;
  lines: unknown[];
  worked: () => Expected;
  clone: () => string;
  /** True once the executor has written the worker-only state. */
  ran: () => boolean;
  seedHead: () => string;
}

function credRig(iid: number, o: { generation?: number; extra?: Record<string, unknown>; claim?: Record<string, unknown> } = {}): CredRig {
  const { gitlab } = fakeGitlab();
  client.protocolFeatures = [VAULT_FEATURE];
  const rig = codexRig({ vaultLocked: () => true });
  const claim = gitlabClaim(iid, { claim_generation: o.generation ?? 5, ...o.claim });
  let facts: Expected | undefined;
  let clone = "";
  let seedHead = "";
  const exec = new FakeCodexExecutor(rig.safety, async (ctx: RunContext): Promise<ExecutorResult> => {
    clone = ctx.worktreePath;
    seedHead = gitIn(clone, "rev-parse", "HEAD");
    facts = writeWorkerState(ctx.worktreePath);
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation as number);
    return { branch: ctx.branch };
  }, rig.settle);
  const { logger, lines } = recordingLogger();
  const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, { recoveryRetryMs: 5, ...o.extra });
  return { claim, runner, lines, worked: () => facts!, clone: () => clone, ran: () => facts !== undefined, seedHead: () => seedHead };
}

const noTrackingWrite = (iid: number): void => {
  assert.equal(gitTracking(iid), null, "no refs/uzi-runner write on a stale exit");
};
function gitTracking(iid: number): string | null {
  try {
    return gitIn(bareOf(), "rev-parse", "--verify", `refs/uzi-runner/${branchOf(iid)}`);
  } catch {
    return null;
  }
}

describe("RunRunner #1786 M2 — the recovery handler's exits keep an unpreserved source (credential-deferred)", () => {
  it("row 1: confirmRunningForCredentialPark `stop` (stale_claim ack on the confirming running report) keeps the source and writes nothing through the stale claim", async () => {
    const iid = 17901;
    const published: string[] = [];
    const origPublish = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (...a: Parameters<typeof origPublish>) => {
      published.push("publish");
      return origPublish(...a);
    };
    try {
      const r = credRig(iid);
      api.failStateWhen(r.claim.run_id, (b) => b.status === "running" && r.ran(), {
        runStatus: "running", disposition: "stale_claim",
      });
      await r.runner.execute(r.claim);
      assertRecoverable(r.clone(), r.worked(), "row 1");
      assert.equal(parkReports(r.claim.run_id).length, 0, "never parked");
      assert.ok(!statuses(r.claim.run_id).some((s) => s === "failed" || s === "completed"), "no terminal report");
      assert.deepEqual(published, [], "no publish");
      noTrackingWrite(iid);
      assert.deepEqual(retainedReasons(r.lines), ["recovery_confirm_stop"]);
      assert.equal(stopMarkers(r.claim.run_id).length, 1, "recorded for the operator channel");
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = origPublish;
    }
  });

  it("row 1 on an attempt clone: the stop records the attempt as reclaimed in the ledger", async () => {
    const iid = 17902;
    const r = credRig(iid, { extra: { dockerHost: DOCKER } });
    api.failStateWhen(r.claim.run_id, (b) => b.status === "running" && r.ran(), {
      runStatus: "running", disposition: "stale_claim",
    });
    await r.runner.execute(r.claim);
    assert.match(path.basename(r.clone()), /\.attempt-/);
    assertRecoverable(r.clone(), r.worked(), "row 1 attempt");
    const states = cfgAll(bareOf(), `uzi-attempts.${branchOf(iid)}.entry`).map((raw) => (JSON.parse(raw) as { state: string }).state);
    assert.equal(states.at(-1), "reclaimed");
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("row 2: an ownership probe showing ANOTHER generation keeps the source; no report", async () => {
    const iid = 17903;
    const r = credRig(iid);
    // The confirming running report lands; the loop's own ownership read then shows a successor generation.
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => (r.ran() ? { status: "running", claim_generation: 9 } as never : own(runId));
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.getRunOwnership = own;
    }
    assertRecoverable(r.clone(), r.worked(), "row 2");
    assert.equal(parkReports(r.claim.run_id).length, 0);
    assert.ok(!statuses(r.claim.run_id).includes("failed"));
    noTrackingWrite(iid);
    assert.deepEqual(retainedReasons(r.lines), ["recovery_other_generation"]);
  });

  it("row 7: the credential-deferred park ACK keeps a source that changed after its verified capture (commit, tracked edit and untracked file)", async () => {
    const iid = 17904;
    const r = credRig(iid);
    let late: Expected | undefined;
    const parked = spyHandlerResults(r.runner);
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assert.equal(hook.fired(), 1, "the hook ran after the verified capture");
    assert.equal(parkReports(r.claim.run_id).length, 1, "the park is reported exactly once, as before");
    assertRecoverable(r.clone(), late!, "row 7");
    assert.deepEqual(parked, [true], "the handler still reports the run parked");
    // A park exit keeps the plain journal: no stopped-source marker, so the resume recaptures the bytes.
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(retainedReasons(r.lines), []);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("row 7 control: a capture still current and anchored at the exit keeps the clone; the park is unchanged", async () => {
    const iid = 17905;
    const r = credRig(iid);
    let waits = 0;
    const waitRetry = (r.runner as unknown as { waitRecoveryRetry: (...a: unknown[]) => Promise<void> }).waitRecoveryRetry.bind(r.runner);
    (r.runner as unknown as { waitRecoveryRetry: unknown }).waitRecoveryRetry = (...a: unknown[]) => { waits++; return waitRetry(...a); };
    const parked = spyHandlerResults(r.runner);
    await r.runner.execute(r.claim);
    assert.equal(fs.existsSync(r.clone()), true, "covered park: diagnostic only");
    assert.deepEqual(parked, [true]);
    assert.deepEqual(api.states.filter((s) => s.runId === r.claim.run_id).map((s) => s.body.status),
      ["running", "running", "running", "recovery_wait"], "the same park reports in the same order");
    assert.equal(waits, 0, "no new waits");
    assert.deepEqual(coveredReasons(r.lines), []);
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(retainedReasons(r.lines), []);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("shared tracking moves after the verified capture (another run's fetch-back, no owed pin for the captured head): the source is kept", async () => {
    const iid = 17906;
    const r = credRig(iid);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        // Another run's fetch-back moves the shared per-branch tracking ref off the captured head.
        moveSharedTrackingAway(r.claim.run_id, iid, r.seedHead());
      }
      return report(runId, body, signal);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assertRecoverable(r.clone(), writeExpectedAfterWip(r.worked(), r.clone()), "shared tracking move");
    assert.equal(parkReports(r.claim.run_id).length, 1);
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("row 8: a park ACK with a terminal status keeps a source that changed after the capture (no new report)", async () => {
    const iid = 17907;
    const r = credRig(iid);
    // Only the park report is answered with a terminal status; the earlier running confirmation is not.
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => (body.status === "recovery_wait"
      ? ({ applied: true, status: "failed" } as never) : report(runId, body, signal));
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
      client.reportState = report;
    }
    assertRecoverable(r.clone(), late!, "row 8");
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_ack_terminal"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), [], "a terminal exit keeps the plain journal");
  });

  it("clean terminal park ACK releases a covered clone (recovery_park_ack_terminal)", { skip: LINUX_CAPTURE }, async () => {
    const r = credRig(17910);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => (body.status === "recovery_wait"
      ? ({ applied: true, status: "failed" } as never) : report(runId, body, signal));
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assert.equal(fs.existsSync(r.clone()), false, "terminal coverage still releases the clone");
    assert.deepEqual(coveredReasons(r.lines), ["recovery_park_ack_terminal"]);
    assert.deepEqual(keptReasons(r.lines), []);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("row 9: a StaleClaimError on the park report keeps the source and reports nothing further", async () => {
    const iid = 17908;
    const r = credRig(iid);
    api.failStateWhen(r.claim.run_id, (b) => b.status === "recovery_wait", { runStatus: "running", disposition: "stale_claim" });
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assertRecoverable(r.clone(), late!, "row 9");
    assert.deepEqual(retainedReasons(r.lines), ["recovery_park_report_stale"]);
    assert.ok(!statuses(r.claim.run_id).some((s) => s === "failed" || s === "completed"), "no terminal report");
  });

  it("row 9 without a later change: the stale park report finds the capture still covered, but a stopped exit keeps and records the clone (coverage is diagnostic only)", async () => {
    const iid = 17909;
    const r = credRig(iid);
    api.failStateWhen(r.claim.run_id, (b) => b.status === "recovery_wait", { runStatus: "running", disposition: "stale_claim" });
    await r.runner.execute(r.claim);
    assert.ok(fs.existsSync(r.clone()), "a stopped exit keeps the clone even when covered at the exit");
    assert.deepEqual(coveredReasons(r.lines), [], "no release on a covered verdict");
    assert.deepEqual(retainedReasons(r.lines), ["recovery_park_report_stale"]);
  });

  it("keepUnlessCaptured: an ownership read of a terminal status keeps a source that changed after its capture", async () => {
    const iid = 17910;
    const r = credRig(iid);
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => (r.ran() ? { status: "completed", claim_generation: 5 } as never : own(runId));
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt, "untracked"); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.getRunOwnership = own;
      hook.restore();
    }
    assert.equal(hook.fired(), 1);
    const want = writeExpectedAfterWip(late!, r.clone());
    assertRecoverable(r.clone(), want, "keepUnlessCaptured");
    assert.equal(parkReports(r.claim.run_id).length, 0);
    assert.deepEqual(keptReasons(r.lines), ["recovery_terminal_status"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });
});

/** After the WIP marker the earlier dirty state is committed: expect the clone's real tracked bytes. */
function writeExpectedAfterWip(late: Expected, clone: string): Expected {
  assert.ok(fs.existsSync(clone), "the clone is retained");
  return { ...late, head: gitIn(clone, "rev-parse", "HEAD"), blob: fs.readFileSync(path.join(clone, "assets", "blob.bin")),
    blobMode: fs.statSync(path.join(clone, "assets", "blob.bin")).mode & 0o777 };
}

// --- the transient recovery park -------------------------------------------------------------------

interface TransientRig {
  claim: ReturnType<typeof gitlabClaim>;
  runner: RunRunner;
  lines: unknown[];
  clone: () => string;
  worked: () => Expected;
  seedHead: () => string;
}

function transientRig(iid: number, o: { dockerHost?: boolean; claim?: Record<string, unknown> } = {}): TransientRig {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid, { claim_generation: 1, ...o.claim });
  let clone = "";
  let facts: Expected | undefined;
  let seedHead = "";
  const { logger, lines } = recordingLogger();
  const runner = runnerWith((runId) => {
    const runHome = path.join(homeDir, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          clone = ctx.worktreePath;
          seedHead = gitIn(clone, "rev-parse", "HEAD");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          facts = writeWorkerState(clone);
          throw new TransientRecoveryError();
        },
      },
    };
  }, gitlab, undefined, logger, { recoveryRetryMs: 5, ...(o.dockerHost ? { dockerHost: DOCKER } : {}) });
  return { claim, runner, lines, clone: () => clone, worked: () => facts!, seedHead: () => seedHead };
}

describe("RunRunner #1786 M2 — the transient recovery park revalidates coverage at its exits", () => {
  for (const kind of ["all", "commit", "tracked", "untracked"] as const) {
    it(`a ${kind === "all" ? "new commit, tracked edit and untracked file" : kind === "commit" ? "new commit" : kind === "tracked" ? "modified tracked file" : "new untracked file"} after the verified capture keeps the clone at the park exit`, async () => {
      const iid = 17920 + ["all", "commit", "tracked", "untracked"].indexOf(kind);
      const r = transientRig(iid);
      const parked = spyHandlerResults(r.runner);
      let late: Expected | undefined;
      const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt, kind); });
      try {
        await r.runner.execute(r.claim);
      } finally {
        hook.restore();
      }
      assert.equal(api.states.filter((s) => s.runId === r.claim.run_id && s.body.status === "recovery_wait").length, 1, "one park report");
      assertRecoverable(r.clone(), writeExpectedAfterWip(late!, r.clone()), `transient ${kind}`);
      assert.deepEqual(parked, [true]);
      // The one revalidation runs at the handler's cleanup, after the park ACK; a park keeps the plain journal.
      assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
      assert.deepEqual(stopMarkers(r.claim.run_id), []);
    });
  }

  it("shared tracking moves after the verified capture: the clone is kept at the park exit", async () => {
    const iid = 17924;
    const r = transientRig(iid);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") moveSharedTrackingAway(r.claim.run_id, iid, r.seedHead());
      return report(runId, body, signal);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assertRecoverable(r.clone(), writeExpectedAfterWip(r.worked(), r.clone()), "transient shared tracking");
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("control: a capture still current and anchored keeps the clone; one park report, no wait", async () => {
    const iid = 17925;
    const r = transientRig(iid);
    let waits = 0;
    const waitRetry = (r.runner as unknown as { waitRecoveryRetry: (...a: unknown[]) => Promise<void> }).waitRecoveryRetry.bind(r.runner);
    (r.runner as unknown as { waitRecoveryRetry: unknown }).waitRecoveryRetry = (...a: unknown[]) => { waits++; return waitRetry(...a); };
    await r.runner.execute(r.claim);
    assert.equal(fs.existsSync(r.clone()), true, "covered park: the clone stays for recapture");
    assert.equal(fs.existsSync(path.join(homeDir, r.claim.run_id)), true, "the session survives the park");
    assert.deepEqual(api.states.filter((s) => s.runId === r.claim.run_id).map((s) => s.body.status).filter((s) => s !== "running"),
      ["recovery_wait"]);
    assert.equal(waits, 0);
    assert.deepEqual(retainedReasons(r.lines), []);
  });

  it("row 8 (transient): a park ACK with a terminal status keeps a source that changed after the capture", async () => {
    const iid = 17926;
    const r = transientRig(iid);
    api.overrideStateStatus(r.claim.run_id, "failed");
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assertRecoverable(r.clone(), late!, "row 8 transient");
  });

  it("operator inspect pins what a park keeps: an attempt clone is listed through its ledger entry, a canonical clone kept under its plain journal is listed too and is discardable only on a terminal server lifecycle", async () => {
    // Both parks commit in the "same second": their pins must still be their own (#3131).
    const { attempt, canonical } = await withSameSecondCommits(async () => {
      const attempt = transientRig(17928, { dockerHost: true });
      const canonical = transientRig(17929);
      const hook = afterVerifiedCapture((wt) => { writeLateChange(wt); });
      try {
        await attempt.runner.execute(attempt.claim);
      } finally {
        hook.restore();
      }
      const hook2 = afterVerifiedCapture((wt) => { writeLateChange(wt); });
      try {
        await canonical.runner.execute(canonical.claim);
      } finally {
        hook2.restore();
      }
      return { attempt, canonical };
    });
    assert.match(path.basename(attempt.clone()), /\.attempt-/);
    assert.ok(fs.existsSync(attempt.clone()) && fs.existsSync(canonical.clone()), "both parks kept their clone");
    const result = await git.inspectRetainedSources({ barePath: bareOf(), isLivePath: () => false });
    const ours = result.sources.filter((s) => s.runId === attempt.claim.run_id);
    assert.equal(ours.length, 1, "the attempt park clone is listed via its ledger entry");
    assert.equal(ours[0]!.presence, "present");
    const kept = result.sources.filter((s) => s.runId === canonical.claim.run_id);
    assert.equal(kept.length, 1, "a canonical clone kept under its plain journal is listed");
    assert.deepEqual([kept[0]!.kind, kept[0]!.markerState, kept[0]!.journalState], ["canonical", "none", "plain"]);
    assert.equal(journalCount(17929), 1, "its plain journal stays for the same run's resume");
    const sel = { runId: canonical.claim.run_id, generation: "none" as const, attempt: "canonical" };
    const open = { state: "read" as const, holds: [{ id: "hold-1", generation: 1 }] };
    const view = await git.inspectRetainedSources({ barePath: bareOf(), isLivePath: () => false, selector: sel, holds: open });
    assert.deepEqual(view.sources[0]!.disposition, { kind: "owner_hold_discard", holdIds: ["hold-1"] }, "an open hold is the owner's to discard");
    const none = { state: "read" as const, holds: [] };
    const terminal = { state: "terminal" as const, status: "completed" as const };
    const unread = await git.inspectRetainedSources({ barePath: bareOf(), isLivePath: () => false, selector: sel, holds: none });
    assert.deepEqual(unread.sources[0]!.disposition, { kind: "refused", consumers: ["run_state_unknown"] },
      "no hold and no terminal evidence keeps a parked canonical clone");
    const idle = await git.inspectRetainedSources({ barePath: bareOf(), isLivePath: () => false, selector: sel, holds: none, lifecycle: terminal });
    assert.deepEqual(idle.sources[0]!.disposition, { kind: "local_discard" });
    const refused = await git.discardStoppedSource({ selector: sel, confirm: idle.sources[0]!.descriptor, holds: open, lifecycle: terminal, isLivePath: () => false,
      quiesce: async () => true, barePath: bareOf() });
    assert.equal(refused.kind, "refused");
    assert.ok(fs.existsSync(canonical.clone()), "an open hold deletes nothing");
    const done = await git.discardStoppedSource({ selector: sel, confirm: idle.sources[0]!.descriptor, holds: none, lifecycle: terminal, isLivePath: () => false,
      quiesce: async () => true, barePath: bareOf() });
    assert.equal(done.kind, "done");
    assert.equal(fs.existsSync(canonical.clone()), false);
    assert.equal(journalCount(17929), 0, "the journal that named it is cleared with it");
  });

  it("a retained attempt clone whose emptyDir path is later deleted is reported missing by inspect, never empty", async () => {
    const iid = 17927;
    const r = transientRig(iid, { dockerHost: true });
    const hook = afterVerifiedCapture((wt) => { writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assert.match(path.basename(r.clone()), /\.attempt-/);
    assert.ok(fs.existsSync(r.clone()), "retained");
    fs.rmSync(r.clone(), { recursive: true });
    const result = await git.inspectRetainedSources({ barePath: bareOf(), isLivePath: () => false });
    const ours = result.sources.filter((s) => s.runId === r.claim.run_id);
    assert.equal(ours.length, 1, "the retained attempt is enumerated from the ledger");
    assert.equal(ours[0]!.presence, "missing");
    assert.equal(ours[0]!.allocatedBytes, 0);
    // A park keeps the plain journal and the live attempt: no ledger `reclaimed` (that is for a stopped generation).
    assert.equal(ours[0]!.ledgerState, "live");
    assert.ok(!ledgerStates(iid).includes("reclaimed"));
    assert.equal(journalCount(iid), 1, "the journal survives for the resume");
  });
});

// --- terminal-disk deferral rows ---------------------------------------------------------------------

describe("RunRunner #1786 M2 — terminal-disk deferral exits keep an unpreserved source", { skip: LINUX_CAPTURE }, () => {
  function diskRig(o: { status?: string; generation?: number; setup?: (f: ReturnType<typeof fixture>) => void } = {}) {
    let worked: Expected | undefined;
    const f: ReturnType<typeof fixture> = fixture({
      ...(o.status ? { status: o.status } : {}),
      ...(o.generation !== undefined ? { generation: o.generation } : {}),
      afterWork: async (ctx) => {
        worked = writeWorkerState(ctx.worktreePath);
        o.setup?.(f);
      },
    });
    return { f, worked: () => worked! };
  }

  /** The terminal-disk entry reads ownership once before the handler; the handler's loop reads it again
   *  before any capture. `script` answers the Nth read (1-based) of the pre-capture phase. */
  function scriptOwnership(script: (n: number) => unknown): () => void {
    const own = client.getRunOwnership.bind(client);
    let n = 0;
    client.getRunOwnership = async (runId: string) => {
      const answer = script(++n);
      if (answer === undefined) return own(runId);
      if (answer instanceof Error) throw answer;
      return answer as never;
    };
    return () => { client.getRunOwnership = own; };
  }

  it("row 3: an ownership 404 in the handler's loop (missing authority) keeps the seeded source", async () => {
    const { f, worked } = diskRig();
    const sites = spyExitSites(f.runner);
    const restore = scriptOwnership((n) => (n === 2 ? new RequestError("GET", "/ownership", 404, "not found") : undefined));
    try {
      await f.runner.execute(f.claim);
    } finally {
      restore();
    }
    assertRecoverable(f.clone(), worked(), "row 3");
    // issue #1786: a 404 is an explicit keep; no coverage check runs at all (a covered verdict could
    // not release it either way, see the covered-clone suite at the end of this file).
    assert.deepEqual(sites, []);
    assert.deepEqual(stopMarkers(f.claim.run_id), [], "missing authority keeps the plain journal");
    assert.equal(api.states.filter((s) => s.body.status === "failed" || s.body.status === "recovery_wait").length, 0);
  });

  it("row 4: the handler's ownership loop seeing a terminal status keeps the source (a terminal status is not a source proof)", async () => {
    const { f, worked } = diskRig();
    const sites = spyExitSites(f.runner);
    const restore = scriptOwnership((n) => (n === 2 ? { status: "completed", claim_generation: 4 } : undefined));
    try {
      await f.runner.execute(f.claim);
    } finally {
      restore();
    }
    assertRecoverable(f.clone(), worked(), "row 4");
    assert.deepEqual(sites, ["recovery_terminal_status"]);
    assert.deepEqual(stopMarkers(f.claim.run_id), [], "a terminal status keeps the plain journal");
    assert.equal(api.states.some((s) => s.body.status === "failed"), false);
  });

  it("row 2 (terminal disk): the handler's ownership read of another generation keeps the source", async () => {
    const { f, worked } = diskRig();
    const sites = spyExitSites(f.runner);
    const restore = scriptOwnership((n) => (n === 2 ? { status: "running", claim_generation: 9 } : undefined));
    try {
      await f.runner.execute(f.claim);
    } finally {
      restore();
    }
    assertRecoverable(f.clone(), worked(), "row 2 terminal disk");
    assert.deepEqual(sites, ["recovery_other_generation"]);
    assert.equal(stopMarkers(f.claim.run_id).length, 1, "a superseded generation is recorded as stopped");
    assert.equal(api.states.some((s) => s.body.status === "failed"), false);
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 0);
  });

  for (const [name, options] of [["another generation", { generation: 5 }], ["a terminal status", { status: "completed" }]] as const) {
    it(`the pre-handler ownership read of ${name} keeps the source and stays silent`, async () => {
      const { f, worked } = diskRig(options);
      const sites = spyExitSites(f.runner);
      await f.runner.execute(f.claim);
      assertRecoverable(f.clone(), worked(), `precheck ${name}`);
      assert.deepEqual(sites, ["terminal_disk_precheck"]);
      // Another generation is a stopped source (marker); a terminal status keeps the plain journal.
      assert.equal(stopMarkers(f.claim.run_id).length, name === "another generation" ? 1 : 0);
      assert.equal(api.states.filter((s) => s.body.status === "failed" || s.body.status === "recovery_wait").length, 0);
    });
  }

  for (const recheck of ["generation", "terminal"] as const) {
    it(`row 5: the post-capture recheck seeing ${recheck === "generation" ? "another generation" : "a terminal status"} keeps a source that changed after the capture`, async () => {
      const { f } = diskRig();
      const sites = spyExitSites(f.runner);
      let captured = false;
      let late: Expected | undefined;
      const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); captured = true; });
      const own = client.getRunOwnership.bind(client);
      client.getRunOwnership = async (runId: string) => (captured
        ? ({ status: recheck === "terminal" ? "completed" : "running", claim_generation: recheck === "terminal" ? 4 : 9 } as never)
        : own(runId));
      try {
        await f.runner.execute(f.claim);
      } finally {
        client.getRunOwnership = own;
        hook.restore();
      }
      assert.equal(hook.fired(), 1);
      assertRecoverable(f.clone(), writeExpectedAfterWip(late!, f.clone()), `row 5 ${recheck}`);
      assert.deepEqual(sites.slice(-1), ["terminal_disk_recheck"]);
      assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 0);
    });
  }

  it("row 6: a pending outbox terminal keeps reporting precedence but cannot authorize deleting a source that changed after the capture", async () => {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "m2-terminal-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const g: ReturnType<typeof fixture> = fixture({
      afterWork: async (ctx) => { writeWorkerState(ctx.worktreePath); },
      runner: { outbox, quiesceRun: async () => QUIESCENT },
    });
    const sites = spyExitSites(g.runner);
    let late: Expected | undefined;
    const hook = afterVerifiedCapture(async (wt) => {
      late = writeLateChange(wt);
      const journaled = await outbox.journalTerminal(g.claim.run_id, 4, "running", 0,
        { status: "failed", failure_reason: "durable original failure", claim_generation: 4 });
      assert.equal(journaled.journaled, true);
    });
    try {
      await g.runner.execute(g.claim);
    } finally {
      hook.restore();
    }
    assert.equal(hook.fired(), 1);
    assertRecoverable(g.clone(), writeExpectedAfterWip(late!, g.clone()), "row 6");
    assert.deepEqual(sites.slice(-1), ["terminal_disk_pending_terminal"]);
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 0, "no park");
  });

  it("an interruption propagating out of the handler after a verified capture runs no settle: no stopped-source proof, the clone is kept", async () => {
    const { f } = diskRig();
    const sites = spyExitSites(f.runner);
    let proofs = 0;
    const quiesce = (f.runner as unknown as { quiesceRun: (...a: unknown[]) => Promise<unknown> }).quiesceRun.bind(f.runner);
    (f.runner as unknown as { quiesceRun: unknown }).quiesceRun = (flight: unknown, executor: unknown, opts: { site?: string }) => {
      if (opts?.site === "stopped_source") proofs++;
      return quiesce(flight, executor, opts);
    };
    let late: Expected | undefined;
    // The capture verifies and the exit is marked release-eligible; a shutdown then arrives with the
    // post-capture ownership read, so the handler's next terminal-disk check throws.
    let captured = false;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); captured = true; });
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => {
      const answer = await own(runId);
      if (captured) (f.runner as unknown as { shuttingDownGlobal: boolean }).shuttingDownGlobal = true;
      return answer;
    };
    try {
      await f.runner.execute(f.claim);
    } finally {
      client.getRunOwnership = own;
      hook.restore();
      (f.runner as unknown as { shuttingDownGlobal: boolean }).shuttingDownGlobal = false;
    }
    assert.equal(hook.fired(), 1);
    assert.deepEqual(sites, [], "no settle revalidation ran for the throwing exit");
    assert.equal(proofs, 0, "no stopped_source proof ran while the interruption propagated");
    assertRecoverable(f.clone(), writeExpectedAfterWip(late!, f.clone()), "interrupted exit");
    assert.deepEqual(stopMarkers(f.claim.run_id), []);
  });

  it("the degraded {verified:false} stub is never sufficient: a terminal park ACK keeps the unverified clone's bytes", async () => {
    let worked: Expected | undefined;
    const wip = git.commitWipMarker.bind(git);
    git.commitWipMarker = async () => false;
    const f = fixture({
      afterWork: async (ctx) => { worked = writeWorkerState(ctx.worktreePath); },
      runner: { quiesceRun: async () => QUIESCENT },
    });
    const report = client.reportState.bind(client);
    let sends = 0;
    const sites = spyExitSites(f.runner);
    client.reportState = async (id, body: StateRequest, signal) => {
      if (body.status === "recovery_wait") {
        sends++;
        if (sends === 1) throw new Error("lost reply");
        if (sends === 2) return { applied: false };
        api.overrideStateStatus(id, "failed");
      }
      return report(id, body, signal);
    };
    try {
      await f.runner.execute(f.claim);
    } finally {
      client.reportState = report;
      git.commitWipMarker = wip;
    }
    assert.equal(sends, 3, "the same three park reports as before");
    assert.deepEqual(sites, ["recovery_park_ack_terminal"]);
    // The fixture's own sentinel files sit beside the writeWorkerState bytes; the binary and untracked file are the claim.
    assertRecoverable(f.clone(), { ...worked!, head: gitIn(f.clone(), "rev-parse", "HEAD") }, "degraded stub");
  });
});

// --- review rework: B2 (revalidate at EVERY return), B3 (cancel semantics), N1 (representation) --------

/** After the first recovery_wait park report is sent, bytes land in the clone (a writer the capture-time
 *  check never saw), then `answer` is the server's reply. */
function lateWriteAtPark(r: TransientRig, answer: (body: StateRequest) => unknown): { late: () => Expected | undefined; restore: () => void } {
  const report = client.reportState.bind(client);
  let late: Expected | undefined;
  let sent = 0;
  client.reportState = async (runId, body, signal) => {
    if (body.status === "recovery_wait" && ++sent === 1) {
      late = writeLateChange(r.clone());
      return answer(body) as never;
    }
    return report(runId, body, signal);
  };
  return { late: () => late, restore: () => { client.reportState = report; } };
}

describe("RunRunner #1786 M2 rework — no return path of the handler releases a source written after the capture", () => {
  const exits = [
    { name: "PROBE-B: a live non-terminal park ACK (paused)", iid: 17930 },
    { name: "PROBE-C: a statusless ACK, then an ownership 404", iid: 17931 },
    { name: "a statusless ACK, then a live non-running ownership status", iid: 17932 },
    { name: "a statusless ACK, then a worker shutdown at the loop top", iid: 17933 },
  ];
  for (const exit of exits) {
    it(`${exit.name}: the bytes written after the capture-time check are kept; no marker`, async () => {
      const r = transientRig(exit.iid);
      const parked = spyHandlerResults(r.runner);
      const own = client.getRunOwnership.bind(client);
      let written = false;
      const hook = lateWriteAtPark(r, () => {
        written = true;
        if (exit.iid === 17933) r.runner.shutdown();
        return exit.iid === 17930 ? { applied: true, status: "paused" } : { applied: true };
      });
      client.getRunOwnership = async (runId: string) => {
        if (written && exit.iid === 17931) throw new RequestError("GET", "/ownership", 404, "not found");
        if (written && exit.iid === 17932) return { status: "paused", claim_generation: 1 } as never;
        return own(runId);
      };
      try {
        await r.runner.execute(r.claim);
      } finally {
        hook.restore();
        client.getRunOwnership = own;
      }
      assert.ok(hook.late(), "bytes were written after the capture-time point");
      assert.deepEqual(parked, [false], "the handler did not park");
      assertRecoverable(r.clone(), hook.late()!, exit.name);
      assert.deepEqual(stopMarkers(r.claim.run_id), []);
      // A shutdown exit runs no cleanup-time check at all (the clone is kept by the latched flag).
      assert.deepEqual(keptReasons(r.lines).length, exit.iid === 17933 ? 0 : 1,
        exit.iid === 17933 ? "shutdown: no cleanup-time check" : "one cleanup-time check, which kept the source");
    });
  }

  it("control: the same exits with nothing written after the capture keep the covered clone", async () => {
    const r = transientRig(17934);
    const parked = spyHandlerResults(r.runner);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) =>
      (body.status === "recovery_wait" ? ({ applied: true, status: "paused" } as never) : report(runId, body, signal));
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assert.deepEqual(parked, [false]);
    assert.equal(fs.existsSync(r.clone()), true, "covered nonterminal exit: diagnostic only");
    assert.deepEqual(coveredReasons(r.lines).length, 0);
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
  });

  it("PROBE-A: a transient park retained at cleanup on a canonical worker does not block the same run's next claim; the late bytes reach it", async () => {
    const iid = 17935;
    const r = transientRig(iid);
    // The real park ACK is wanted: write the late bytes after the capture-time point, then reach the server.
    const report = client.reportState.bind(client);
    let late: Expected | undefined;
    let sent = 0;
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait" && ++sent === 1) late = writeLateChange(r.clone());
      return report(runId, body, signal);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assert.ok(late);
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), [], "no uzi-stopped marker on a park");
    assertRecoverable(r.clone(), late!, "the parked clone");

    // The same run's next claim (generation 2) resumes on this worker: it recaptures, never worker_residue_blocked.
    let seen: { head: string; blob: Buffer; late: string; committed: string } | undefined;
    const { gitlab } = fakeGitlab();
    const resumed = runnerWith(() => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen = {
            head: gitIn(ctx.worktreePath, "rev-parse", "HEAD"),
            blob: fs.readFileSync(path.join(ctx.worktreePath, "assets", "blob.bin")),
            late: fs.readFileSync(path.join(ctx.worktreePath, "late-untracked.txt"), "utf8"),
            committed: fs.readFileSync(path.join(ctx.worktreePath, "late.txt"), "utf8"),
          };
          return { branch: ctx.branch };
        },
      },
    }), gitlab, undefined, undefined, { recoveryRetryMs: 5 });
    api.setOwnershipStatus(r.claim.run_id, "running", 2);
    await resumed.execute({ ...r.claim, claim_generation: 2 });
    assert.ok(!statuses(r.claim.run_id).includes("failed"), `the resumed claim did not fail: ${JSON.stringify(api.states.filter((s) => s.runId === r.claim.run_id).map((s) => [s.body.status, s.body.failure_reason]))}`);
    assert.ok(seen, "the second claim reached its executor");
    assert.deepEqual(seen!.blob, late!.blob, "the tracked edit written after the capture");
    assert.equal(seen!.late, LATE_UNTRACKED, "the untracked file written after the capture");
    assert.equal(seen!.committed, "late committed work\n", "the commit written after the capture");
  });

  it("an attempt-clone park keeps its journal and live ledger entry (no `reclaimed`)", async () => {
    const iid = 17936;
    const r = transientRig(iid, { dockerHost: true });
    const hook = afterVerifiedCapture((wt) => { writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assert.match(path.basename(r.clone()), /\.attempt-/);
    assert.ok(fs.existsSync(r.clone()));
    assert.ok(!ledgerStates(iid).includes("reclaimed"));
    assert.equal(journalCount(iid), 1);
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
  });

  it("the captured head is bound: shared tracking moves to the seed head and the clone is reset to it after the capture, so the captured head is no longer anchored and the clone is kept", async () => {
    const iid = 17937;
    const r = transientRig(iid);
    const parked = spyHandlerResults(r.runner);
    const report = client.reportState.bind(client);
    let moved = false;
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait" && !moved) {
        moved = true;
        // After the capture verified: another run's fetch-back moves the shared tracking ref to the seed head
        // (this run's owed pins are gone), and the clone is reset to that head with nothing untracked left.
        // The clone is then clean at a head the bare holds, but not at the head the capture covered.
        moveSharedTrackingAway(r.claim.run_id, iid, r.seedHead());
        gitIn(r.clone(), "reset", "--hard", r.seedHead());
        gitIn(r.clone(), "clean", "-fdx");
      }
      return report(runId, body, signal);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assert.ok(moved);
    assert.deepEqual(parked, [true]);
    assert.ok(fs.existsSync(r.clone()), "kept: the captured head is no longer anchored");
    assert.deepEqual(keptReasons(r.lines), ["recovery_park_keep"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("guarded (inventory_guarded) park row: the late bytes are kept and no marker is written", async () => {
    const iid = 17938;
    const r = transientRig(iid, { claim: { inventory_guarded: true } });
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
    }
    assertRecoverable(r.clone(), writeExpectedAfterWip(late!, r.clone()), "guarded park");
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });
});

describe("RunRunner #1786 M2 rework — credential-deferred terminal exits and the stale row keep the representation they own", () => {
  it("recovery_confirm_terminal: a terminal ACK on the confirming report keeps a source that changed after its capture; no marker, no report", async () => {
    const r = credRig(17940);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => (body.status === "running" && r.ran()
      ? ({ applied: true, status: "completed" } as never) : report(runId, body, signal));
    const parked = spyHandlerResults(r.runner);
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt, "untracked"); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
      client.reportState = report;
    }
    assert.equal(hook.fired(), 1);
    assertRecoverable(r.clone(), writeExpectedAfterWip(late!, r.clone()), "confirm_terminal");
    assert.deepEqual(parked, [false]);
    assert.deepEqual(keptReasons(r.lines), ["recovery_confirm_terminal"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
    assert.equal(parkReports(r.claim.run_id).length, 0);
  });

  it("recovery_receipt_failed: a given-up input receipt keeps a source that changed after its capture and still reports the failure", async () => {
    const r = credRig(17941);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "running" && r.ran()) throw Object.assign(new Error("input receipt given up"), { name: "InputReceiptError" });
      return report(runId, body, signal);
    };
    let late: Expected | undefined;
    const hook = afterVerifiedCapture((wt) => { late = writeLateChange(wt, "untracked"); });
    try {
      await r.runner.execute(r.claim);
    } finally {
      hook.restore();
      client.reportState = report;
    }
    assert.equal(hook.fired(), 1);
    assertRecoverable(r.clone(), writeExpectedAfterWip(late!, r.clone()), "receipt_failed");
    assert.deepEqual(keptReasons(r.lines), ["recovery_receipt_failed"]);
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
  });

  it("recovery_confirm_gone: a 404 on the confirmation keeps the uncaptured source; no marker", async () => {
    const r = credRig(17942);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "running" && r.ran()) throw new Error("transport down");
      return report(runId, body, signal);
    };
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => {
      if (r.ran()) throw new RequestError("GET", "/ownership", 404, "not found");
      return own(runId);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
      client.getRunOwnership = own;
    }
    assertRecoverable(r.clone(), writeExpectedAfterWip(r.worked(), r.clone()), "confirm_gone");
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
    assert.equal(parkReports(r.claim.run_id).length, 0);
  });

  it("guarded (inventory_guarded) stale row: the stop is recorded as a stopped source, nothing is written through the stale claim", async () => {
    const iid = 17943;
    const r = credRig(iid, { claim: { inventory_guarded: true } });
    api.failStateWhen(r.claim.run_id, (b) => b.status === "running" && r.ran(), { runStatus: "running", disposition: "stale_claim" });
    await r.runner.execute(r.claim);
    assertRecoverable(r.clone(), r.worked(), "guarded stale");
    assert.deepEqual(retainedReasons(r.lines), ["recovery_confirm_stop"]);
    assert.equal(stopMarkers(r.claim.run_id).length, 1);
    noTrackingWrite(iid);
  });
});

describe("RunRunner #1786 M2 rework — the cancel ACK keeps owner-authorized discard semantics (PRD Module 1)", () => {
  function cancelMirror(runId: string): { stop: () => void } {
    // Mirror Service.SetState: the consumed cancel input stamped stop_kind, and the worker's failed report cancels.
    let stopRequested = false;
    api.onState(runId, (body) => {
      const status = body.status === "failed" && stopRequested ? "cancelled" : body.status;
      api.setOwnershipStatus(runId, status);
      api.overrideStateStatus(runId, status);
    });
    return { stop: () => { stopRequested = true; api.setInputs(runId, [{ id: 1, kind: "cancel" }]); } };
  }

  it("non-credential owner cancel during failed capture retries: normal cleanup even though the clone is not covered (claim_generation 1)", async () => {
    const r = transientRig(17950);
    const mirror = cancelMirror(r.claim.run_id);
    let reached!: () => void;
    const failed = new Promise<void>((resolve) => { reached = resolve; });
    git.commitWipMarker = async () => { reached(); return false; };
    const execution = r.runner.execute(r.claim);
    await Promise.race([failed, execution.then(() => { throw new Error("capture path not reached"); })]);
    mirror.stop();
    await execution;
    assert.ok(api.states.some((s) => s.runId === r.claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"));
    assert.equal(fs.existsSync(r.clone()), false, "explicit cancellation keeps normal cleanup");
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
    assert.deepEqual(retainedReasons(r.lines), []);
  });

  for (const changed of [true, false]) {
    it(`credential-deferred cancel after a verified capture: revalidated at cleanup (${changed ? "bytes written after the capture are kept" : "a covered clone goes"})`, { skip: changed ? false : LINUX_CAPTURE }, async () => {
      const iid = changed ? 17951 : 17952;
      const r = credRig(iid);
      const mirror = cancelMirror(r.claim.run_id);
      const report = client.reportState.bind(client);
      let late: Expected | undefined;
      let sent = 0;
      client.reportState = async (runId, body, signal) => {
        if (body.status === "recovery_wait") {
          // Statusless until the consumed cancel is seen by the handler's loop (the poller is asynchronous).
          if (++sent === 1) {
            if (changed) late = writeLateChange(r.clone());
            mirror.stop();
          }
          return { applied: true } as never;
        }
        return report(runId, body, signal);
      };
      try {
        await r.runner.execute(r.claim);
      } finally {
        client.reportState = report;
      }
      assert.ok(api.states.some((s) => s.runId === r.claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        `cancel was reported: ${JSON.stringify(api.states.filter((s) => s.runId === r.claim.run_id).map((s) => [s.body.status, s.body.failure_reason]))}`);
      if (changed) {
        assertRecoverable(r.clone(), late!, "cancel ack kept");
        assert.deepEqual(keptReasons(r.lines), ["recovery_cancel_ack"]);
      } else {
        assert.equal(fs.existsSync(r.clone()), false);
        assert.deepEqual(coveredReasons(r.lines), ["recovery_cancel_ack"]);
      }
      assert.deepEqual(stopMarkers(r.claim.run_id), []);
    });
  }
});

// issue #1786: an ownership 404 in the handler's loop is a quiet stop with unknown authority. It is
// neither a park nor a terminal status, so a covered verdict must never release the clone. The
// coverage proof only runs on Linux (and the hardened clean check refuses a case-folding clone), so
// the suite is skipped elsewhere, like the terminal-disk suite above.
describe("RunRunner #1786 — an ownership 404 in the handler's loop keeps a covered clone", { skip: LINUX_CAPTURE }, () => {
  it("credential-deferred: a 404 after a verified, still-covered capture keeps the clone; no stopped record, no further report", async () => {
    const iid = 17950;
    const r = credRig(iid);
    const parked = spyHandlerResults(r.runner);
    const report = client.reportState.bind(client);
    let parkSent = false;
    let parkReportsSent = 0;
    // A statusless park ACK sends the loop back to the ownership read, which now answers 404.
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") { parkSent = true; parkReportsSent++; return { applied: true } as never; }
      return report(runId, body, signal);
    };
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => {
      if (parkSent) throw new RequestError("GET", "/ownership", 404, "not found");
      return own(runId);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
      client.getRunOwnership = own;
    }
    assert.deepEqual(parked, [false], "the handler stopped without parking");
    assertRecoverable(r.clone(), writeExpectedAfterWip(r.worked(), r.clone()), "credential-deferred 404");
    assert.deepEqual(coveredReasons(r.lines), [], "no cleanup-time check released the clone");
    assert.deepEqual(stopMarkers(r.claim.run_id), []);
    assert.equal(parkReportsSent, 1, "one park report, none after the 404");
    assert.ok(!statuses(r.claim.run_id).includes("failed"));
  });

  it("terminal-disk: a 404 after a verified, still-covered capture keeps the clone; no stopped record, no failure report", async () => {
    const f = fixture();
    const sites = spyExitSites(f.runner);
    let captured = false;
    const hook = afterVerifiedCapture(() => { captured = true; });
    const own = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId: string) => {
      if (captured) throw new RequestError("GET", "/ownership", 404, "not found");
      return own(runId);
    };
    try {
      await f.runner.execute(f.claim);
    } finally {
      client.getRunOwnership = own;
      hook.restore();
    }
    assert.equal(hook.fired(), 1, "the capture verified before the 404");
    assert.ok(fs.existsSync(f.clone()), "the covered clone survives the 404");
    assert.equal(fs.readFileSync(path.join(f.clone(), "DIRTY.txt"), "utf8"), "dirty sentinel\n");
    assert.deepEqual(sites, [], "no coverage-based release was attempted");
    assert.deepEqual(stopMarkers(f.claim.run_id), []);
    assert.equal(api.states.filter((s) => s.body.status === "failed" || s.body.status === "recovery_wait").length, 0);
  });
});

describe("RunRunner #1786 — a shutdown exit runs no cleanup-time coverage check", { skip: LINUX_CAPTURE }, () => {
  it("a verified, still-covered capture marks release eligible, then shutdown during the loop: no coverage check runs and the clone survives", async () => {
    const r = transientRig(17960);
    const sites = spyExitSites(r.runner);
    const parked = spyHandlerResults(r.runner);
    const report = client.reportState.bind(client);
    // A statusless park ACK sends the loop back to its top, where the shutdown is observed.
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") { r.runner.shutdown(); return { applied: true } as never; }
      return report(runId, body, signal);
    };
    try {
      await r.runner.execute(r.claim);
    } finally {
      client.reportState = report;
    }
    assert.deepEqual(parked, [false], "the handler stopped without parking");
    assertRecoverable(r.clone(), writeExpectedAfterWip(r.worked(), r.clone()), "shutdown exit");
    assert.deepEqual(sites, [], "no retainStoppedSource call inside the termination grace");
    assert.deepEqual(coveredReasons(r.lines), []);
  });
});
