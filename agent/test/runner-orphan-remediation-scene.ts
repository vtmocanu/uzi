import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { TestContext } from "node:test";
import { formatAttemptId } from "../src/attempt-path.js";
import type { CompletionSourceFence } from "../src/completion-source.js";
import type { GitAuthorityHooks, AuthoritySettlement } from "../src/git-authority-hooks.js";
import type { FetchAgentBranchOptions } from "../src/git.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, runnerWith } from "./runner-harness.js";

// issue #4058 M2 - the runner-level scene of a successor reclaiming a cancelled predecessor's orphaned attempt whose
// recovery journal carries an active remediation recovery record. Real Git, real bare config, the FakeApi as the server.

export const FEATURE = "recovery_cause_retained_recovery_adoption_failed";
export const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"];
export const OWNER_FILE = "OWNER-ONLY.txt";
export const FENCE: CompletionSourceFence = {
  version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64),
};

export function gitIn(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}

export interface SceneOptions {
  /** A: the successor is an mr_rework (cross-kind slug, CapturePathMismatchError). B: an issue run (same slug). */
  entry: "A" | "B";
  /** Attempt paths enabled on the claimant: the release branch (releaseAttemptInPlace). Off: the retire branch. */
  attemptPaths: boolean;
  /** An authority-required cache (the production wiring). */
  authority?: boolean;
  /** Stamp a completion-source fence on the journal (visible as `fenced` only with `authority`). */
  fenced?: boolean;
  /** Reserve a retained recovery iteration so the journal reads as retained. */
  retained?: boolean;
  /** Enroll the remediation record (default true). */
  enroll?: boolean;
  /** Also write the custody-only block through the public writer. */
  blocked?: boolean;
  /** protocol features advertised to the worker client (default: the adoption-failed cause). */
  features?: string[];
  iid?: number;
}

let counter = 4200;

export interface Scene {
  iid: number;
  branch: string;
  key: string;
  bare: string;
  ownerRun: string;
  clone: string;
  attemptId: string;
  tip: string;
  claim: ReturnType<typeof gitlabClaim>;
  runner: RunRunner;
  lines: Array<Record<string, unknown>>;
  /** Set when the executor was entered (a model turn would start). */
  entered: { value: boolean };
  /** Every error the claimant's reclaim was entered with. */
  entries: Error[];
  /** The live flight of the latest reclaim entry. */
  flight: { steering: { markCancelled(): void }; cancel: AbortController; batcher: { emit(m: unknown): void; flush(): Promise<void> } } | undefined;
  /** Runs synchronously when a reclaim is entered, with the live flight (before the owner is classified). */
  beforeReclaim: ((flight: NonNullable<Scene["flight"]>) => void) | undefined;
  /** Call counts of the effects and proofs a hold must never reach. */
  calls: { release: () => number; retire: () => number; detach: () => number; handoff: () => number; quiesce: string[] };
  /** Make the orphan_reclaim proof (if ever reached) report survivors. */
  quiesceBlocked: { value: boolean };
  /** All uzi-* bare config keys with their values: the journal, ledger, attempt-mode markers and descriptors. */
  durable(): string;
  journalRaw(): string;
  source(): { runId: string; clonePath: string; attemptId: string };
  enroll(): Promise<void>;
  /** A later claim of the SAME run at a higher generation (an owner Resume). */
  nextClaim(generation: number): ReturnType<typeof gitlabClaim>;
  /** Commit a clean forward fix on top of the flagged tip and record the scan through the public writers. */
  forwardFix(): Promise<void>;
  /** A runner over a brand new GitCache on the same data dir: a cold reader after a worker restart. */
  coldRunner(): RunRunner;
  /** Drive the claim to its end through the real runner. */
  run(claim?: ReturnType<typeof gitlabClaim>, runner?: RunRunner): Promise<void>;
  /** The reports the claimant made, as `status` or `status:cause`. */
  reported(claim?: ReturnType<typeof gitlabClaim>): string[];
  parks(claim?: ReturnType<typeof gitlabClaim>): number;
  guidance(claim?: ReturnType<typeof gitlabClaim>): string[];
}

function noopAuthority(): GitAuthorityHooks {
  const settlement: AuthoritySettlement = { settle: async () => {} };
  return {
    admitSourceUse: async () => {},
    beginBareMutation: async () => settlement,
    assertBareClear: async () => {},
    beginDestructive: async () => settlement,
  };
}

export async function scene(t: TestContext, o: SceneOptions): Promise<Scene> {
  const iid = o.iid ?? counter++;
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const ownerRun = randomUUID();
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
  client.protocolFeatures = o.features ?? ["claim_generation_fence", FEATURE];
  const bare = await git.ensureClone(fx.originPath);
  const base = gitIn(bare, "rev-parse", "refs/remotes/origin/main");
  const seeded = await git.runnerCloneForBranch(bare, branch, key, noProofReseed, ownerRun, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  const clone = seeded.path;
  await git.markRecoveryCapture(bare, clone, branch, ownerRun, attemptId);
  fs.writeFileSync(path.join(clone, OWNER_FILE), "owner-only bytes\n");
  const source = () => ({ runId: ownerRun, clonePath: clone, attemptId });
  const fetchOpts: FetchAgentBranchOptions = {
    context: { runId: ownerRun, generation: 1, kind: "issue", branch, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } },
    producingSource: source(),
  };
  const flagged = gitIn(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "flagged");
  gitIn(clone, "update-ref", "refs/heads/" + branch, flagged);
  const fetched = await git.fetchAgentBranch(bare, clone, branch, ownerRun, fetchOpts);
  if (fetched.kind !== "updated") throw new Error("fixture fetch: " + JSON.stringify(fetched));
  const rawJournal = () => gitIn(bare, "config", "--get", `uzi-recovery.${branch}.clone`);
  const enroll = async () => {
    await git.recordRemediationScan(bare, branch, source(), { trusted: true, tip: flagged, flaggedOIDs: [flagged], origin: "pre-exit" });
  };
  if (o.enroll !== false) {
    await enroll();
    if (o.blocked) await git.blockRemediationRecovery(bare, branch, source());
  }
  if (o.retained) await git.reserveRecoveryIteration(bare, branch, key, source());
  if (o.fenced) {
    gitIn(bare, "config", "--local", `uzi-recovery.${branch}.clone`, JSON.stringify({ ...JSON.parse(rawJournal()), completionSourceFence: FENCE }));
  }
  // The production wiring applies from here on: the owner was seeded and enrolled by a worker without authority hooks.
  if (o.authority) {
    const previous = { a: (git as unknown as { authority?: unknown }).authority, r: (git as unknown as { authorityRequired?: boolean }).authorityRequired };
    Object.assign(git, { authority: noopAuthority(), authorityRequired: true });
    t.after(() => { Object.assign(git, { authority: previous.a, authorityRequired: previous.r }); });
  }
  api.setOrphanClassification(ownerRun, {
    status: "cancelled", repo_id: "r1", kind: "issue", issue_iid: iid, branch: null, pipeline_ref: null, pipeline_id: null,
  });
  const recorded = recordingLogger();
  const entered = { value: false };
  const factory: ExecutorFactory = runId => ({
    homeDir: path.join(homeDir, runId),
    executor: { run: async () => { entered.value = true; throw new Error("the model must not start"); } },
  });
  const quiesceCalls: string[] = [];
  const quiesceBlocked = { value: false };
  const quiesceRun = async (req: { site: string }) => {
    quiesceCalls.push(req.site);
    return quiesceBlocked.value
      ? { blocked: true, process: { state: "survivors", processes: [], killed: [], detail: "fixture survivor" },
        docker: { state: "not_wired", removed: [], detail: "" } }
      : { process: { state: "quiescent", processes: [], killed: [], detail: "fixture proof" },
        docker: { state: "not_wired", removed: [], detail: "" } };
  };
  const mk = (): RunRunner => {
    const r = runnerWith(factory, fakeGitlab().gitlab, undefined, recorded.logger, {
      recoveryRetryMs: 5, quiesceRun: quiesceRun as never,
    });
    Object.assign(r, { attemptPaths: o.attemptPaths });
    return r;
  };
  const runner = mk();
  const entries: Error[] = [];
  const release = t.mock.method(git, "releaseAttemptInPlace");
  const retire = t.mock.method(git, "retireRunnerClone");
  const detach = t.mock.method(git, "detachTerminalRetained");
  const handoff = t.mock.method((runner as unknown as { recovery: RecoveryCoordinator }).recovery, "retireCompletedSourceForHandoff");
  const watch = (r: RunRunner): void => {
    const real = (r as unknown as { reclaimTerminalOrphan: (...a: unknown[]) => Promise<void> }).reclaimTerminalOrphan.bind(r);
    (r as unknown as { reclaimTerminalOrphan: unknown }).reclaimTerminalOrphan = (...a: unknown[]) => {
      entries.push(a[6] as Error);
      s.flight = a[2] as Scene["flight"];
      s.beforeReclaim?.(s.flight!);
      return real(...a);
    };
  };
  watch(runner);
  const runId = randomUUID();
  const mkClaim = (generation: number) => gitlabClaim(iid, {
    run_id: runId, claim_generation: generation,
    ...(o.entry === "A" ? { kind: "mr_rework", branch } : {}),
  });
  const claim = mkClaim(5);
  const states = (c: ReturnType<typeof gitlabClaim>) => api.states.filter(s => s.runId === c.run_id);
  const forwardFix = async () => {
    const fix = gitIn(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", flagged, "-m", "forward fix");
    gitIn(clone, "update-ref", "refs/heads/" + branch, fix);
    const r = await git.fetchAgentBranch(bare, clone, branch, ownerRun, fetchOpts);
    if (r.kind !== "updated") throw new Error("fixture fetch: " + JSON.stringify(r));
    await git.recordRemediationScan(bare, branch, source(), { trusted: true, tip: fix, flaggedOIDs: [] });
  };
  const coldRunner = (): RunRunner => {
    const cold = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    if (o.authority) Object.assign(cold, { authority: noopAuthority(), authorityRequired: true });
    const r = new RunRunner(client, cold, factory, recorded.logger, 20, undefined, {
      pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0, gitlab: fakeGitlab().gitlab,
      teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 5, quiesceRun: quiesceRun as never,
      checkpointTestHooks: { retryBackoffMs: [0, 0] },
    });
    Object.assign(r, { attemptPaths: o.attemptPaths });
    watch(r);
    return r;
  };
  const s: Scene = {
    iid, branch, key, bare, ownerRun, clone, attemptId, tip: flagged, claim, runner, lines: recorded.lines as Array<Record<string, unknown>>,
    entered, entries, flight: undefined, beforeReclaim: undefined, forwardFix, coldRunner, quiesceBlocked,
    calls: { release: () => release.mock.callCount(), retire: () => retire.mock.callCount(), detach: () => detach.mock.callCount(),
      handoff: () => handoff.mock.callCount(), quiesce: quiesceCalls },
    durable: () => execFileSync("git", ["-C", bare, "config", "--local", "--get-regexp", "^uzi-"], { env: ENV, encoding: "utf8" }),
    journalRaw: rawJournal,
    source,
    enroll,
    nextClaim: generation => mkClaim(generation),
    run: async (c = claim, r = runner) => { await r.execute(c); },
    reported: (c = claim) => states(c).map(x => String(x.body.status) +
      ((x.body as { recovery_cause?: string }).recovery_cause ? ":" + (x.body as { recovery_cause?: string }).recovery_cause : "")),
    parks: (c = claim) => states(c).filter(x => x.body.status === "recovery_wait" &&
      (x.body as { recovery_cause?: string }).recovery_cause === "retained_recovery_adoption_failed").length,
    guidance: (c = claim) => api.messages(c.run_id).map(m => JSON.stringify(m)).filter(m => m.includes("secret-scan remediation")),
  };
  return s;
}

/** Model the server's confirmation of a typed park: the FakeApi ack echoes the status but not the typed cause. */
export function confirmParks(t: TestContext, hook?: (body: Record<string, unknown>, original: () => Promise<unknown>) => Promise<unknown>): void {
  const real = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
  t.mock.method(client, "reportState", (async (...args: unknown[]) => {
    const body = args[1] as Record<string, unknown>;
    const original = () => real(...args);
    const ack = (hook ? await hook(body, original) : await original()) as Record<string, unknown>;
    return body.status === "recovery_wait" && typeof body.recovery_cause === "string"
      ? { ...ack, recoveryCause: body.recovery_cause } : ack;
  }) as never);
}

export function terminal(s: Scene, claim = s.claim): string[] {
  return s.reported(claim).filter(r => r === "failed" || r === "completed" || r === "cancelled");
}
