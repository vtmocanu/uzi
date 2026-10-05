import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import net from "node:net";
import { execFileSync } from "node:child_process";
import { ForeignCaptureBlockedError, CapturePathMismatchError } from "../src/git.js";
import type { Logger } from "../src/log.js";
import { RequestError } from "../src/client.js";
import { type ExecutorFactory } from "../src/runner.js";
import { Hash, randomUUID } from "node:crypto";
import { formatAttemptId } from "../src/attempt-path.js";
import type { AttemptSeedOptions } from "../src/git.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { nullLogger, noProofReseed } from "./helpers.js";
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
  worktreeDirFor,
} from "./runner-harness.js";
import { listenUnix, shortUnixSocket } from "./unix-socket.js";

// issue #1315 — atomic runner-clone RELEASE (retireRunnerClone). The terminal cleanup
// bug was non-atomic: a recursive `fs.rm` racing a `git maintenance`/`fsmonitor` daemon
// threw ENOTEMPTY, the journal-clear never ran, and a later foreign run wedged forever.
// The fix renames the clone to a worker-only holding location (rename(2) beats the race)
// and only THEN clears the journal. These tests pin the guard classification, the
// runner's owner-probe-and-dispose, and every fail-closed edge of retireRunnerClone.

installHarness();

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

/** Read the worker-owned recovery journal directly (bare git config). Returns undefined
 *  when the key is absent or was cleared to an empty value, mirroring readRecoveryCapture. */
function readJournal(bare: string, branch: string): { runId: string; clonePath: string } | undefined {
  let raw = "";
  try {
    raw = execFileSync("git", ["-C", bare, "config", "--local", "--get", `uzi-recovery.${branch}.clone`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return undefined; // git config exits 1 when the key is absent
  }
  if (!raw) return undefined;
  return JSON.parse(raw) as { runId: string; clonePath: string };
}

const holdingRoot = (): string => path.join(fx.dataDir, "runner-quarantine");
const runnerRoot = (): string => path.join(fx.dataDir, "runner");

/** Seed a clone at the branch's canonical path, drop an owner-only marker byte, and
 *  journal (ownerRunId, canonicalPath): a run whose terminal cleanup crashed mid-release
 *  and left the journal pointing at retained residue. */
async function seedResidue(
  iid: number,
  ownerRunId: string,
  marker: string,
): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const branch = `agent/issue-${iid}`;
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, ownerRunId);
  fs.writeFileSync(path.join(clone.path, marker), "owner-only bytes\n");
  await git.markRecoveryCapture(bare, clone.path, branch, ownerRunId);
  return { bare, branch, clonePath: clone.path };
}

/** Like seedResidue, but for a NON-issue clone keyed on an arbitrary branch+slug (the
 *  ci_fix / mr_rework shapes): seed the canonical clone for `branch` at `<runnerRoot>/…/<key>`,
 *  drop an owner-only marker, and journal (ownerRunId, canonicalPath). */
async function seedResidueForBranch(
  branch: string,
  key: string,
  ownerRunId: string,
  marker: string,
): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const clone = await git.runnerCloneForBranch(bare, branch, key, noProofReseed, ownerRunId);
  fs.writeFileSync(path.join(clone.path, marker), "owner-only bytes\n");
  await git.markRecoveryCapture(bare, clone.path, branch, ownerRunId);
  return { bare, branch, clonePath: clone.path };
}

function diagnosticLogger() {
  const events: Record<string, unknown>[] = [];
  const capture = (level: "info" | "warn"): Logger["info"] => (event, fields) => {
    if (!event.startsWith("orphan_")) return;
    assert.equal(level, event === "orphan_reclaim_succeeded" ? "info" : "warn");
    events.push({ ...fields });
  };
  const logger: Logger = { ...nullLogger(), info: capture("info"), warn: capture("warn") };
  return { logger, events };
}

function assertSafeEvents(events: Record<string, unknown>[], forbidden: string[] = []): void {
  assert.ok(events.length > 0);
  for (const event of events) {
    assert.deepEqual(Object.keys(event).sort(), ["claimant_id", "errno", "event", "owner_id", "path_fingerprint", "path_shape", "reason", "repo_id", "stage", ...(event.http_status === undefined ? [] : ["http_status"])].sort());
    assert.match(String(event.path_fingerprint), /^[a-f0-9]{64}$/);
    assert.ok(JSON.stringify(event).length < 700);
    for (const value of forbidden) assert.ok(!JSON.stringify(event).includes(value));
  }
}

describe("1848 M1: moved terminal mr_rework custody", () => {
  const branch = "agent/issue-1810";
  const slug = "agent-issue-1810";
  const tracked = "1848-committed.txt";
  const marker = "1848-untracked.txt";
  const committed = "already published work\n";
  const dirty = "DIRTY tracked owner bytes\n";
  const untracked = "UNTRACKED owner bytes\n";

  function localGit(repo: string, ...args: string[]): string {
    return execFileSync("git", ["-C", repo, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" });
  }

  function configValues(bare: string, key: string): string[] {
    try {
      return localGit(bare, "config", "--local", "--get-all", key).trim().split("\n");
    } catch (error) {
      assert.equal((error as { status?: number }).status, 1, "only an absent config key is empty");
      return [];
    }
  }

  function attemptOptions(attemptId: string): AttemptSeedOptions {
    return { attemptId, isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true };
  }

  function ledger(bare: string): { attemptId: string; clonePath: string; runId: string; state: string }[] {
    return configValues(bare, `uzi-attempts.${branch}.entry`).map((raw) => JSON.parse(raw));
  }

  async function seed(attempt: boolean, status: "failed" | "completed", cloneSlug: string = slug) {
    const owner = randomUUID();
    const claimant = randomUUID();
    localGit(fx.originPath, "checkout", "-b", branch);
    fs.writeFileSync(path.join(fx.originPath, tracked), committed);
    localGit(fx.originPath, "add", tracked);
    localGit(fx.originPath, "commit", "-m", "fixture published MR work");
    const publishedHead = localGit(fx.originPath, "rev-parse", "HEAD").trim();
    localGit(fx.originPath, "checkout", "main");
    const bare = await git.ensureClone(fx.originPath);
    const attemptId = attempt ? formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef") : undefined;
    const clone = await git.runnerCloneForBranch(bare, branch, cloneSlug, noProofReseed, owner, false, undefined,
      attemptId ? attemptOptions(attemptId) : undefined);
    assert.equal(localGit(clone.path, "rev-parse", "HEAD").trim(), publishedHead);
    fs.writeFileSync(path.join(clone.path, tracked), dirty);
    fs.writeFileSync(path.join(clone.path, marker), untracked);
    await git.markRecoveryCapture(bare, clone.path, branch, owner, attemptId);
    // Worker A's ownership probe no longer sees the run moved to B. The owner-scoped
    // response supplies persisted MR identity; the real DB test covers the worker transitions.
    api.setOwnershipNotOwned(owner);
    api.setOrphanClassification(owner, {
      status, repo_id: "r1", kind: "mr_rework", issue_iid: null,
      branch: null, pipeline_ref: "agent/issue-1810", pipeline_id: null,
    });
    return { bare, owner, claimant, clonePath: clone.path, attemptId, publishedHead };
  }

  type Seed = Awaited<ReturnType<typeof seed>>;
  function assertBytes(dir: string, s: Seed): void {
    assert.equal(fs.readFileSync(path.join(dir, tracked), "utf8"), dirty);
    assert.equal(fs.readFileSync(path.join(dir, marker), "utf8"), untracked);
    assert.equal(localGit(dir, "rev-parse", "HEAD").trim(), s.publishedHead);
    assert.equal(localGit(dir, "show", `HEAD:${tracked}`), committed);
  }

  function heldPath(s: Seed): string {
    if (s.attemptId) return s.clonePath;
    const matches = fs.readdirSync(holdingRoot()).map((name) => path.join(holdingRoot(), name))
      .filter((dir) => fs.existsSync(path.join(dir, marker)));
    assert.equal(matches.length, 1, "one complete predecessor quarantine");
    return matches[0]!;
  }

  function execution(s: Seed, enabled: boolean, blocked?: "survivors" | "unverified", loggerOverride?: Logger) {
    const { gitlab } = fakeGitlab();
    const calls: QuiesceRunRequest[] = [];
    let entry: { clonePath: string; journal: ReturnType<typeof readJournal>; clean: string; contents: string; hasMarker: boolean; retainedPath: string; ledgerState: string | undefined } | undefined;
    let entryError: unknown;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, s.claimant),
      executor: { run: async (ctx) => {
        // Check predecessor custody at model entry, before ordinary terminal cleanup.
        let retainedPath: string;
        try {
          retainedPath = heldPath(s);
          assertBytes(retainedPath, s);
        } catch (error) {
          entryError = error;
          throw error;
        }
        entry = {
          retainedPath, ledgerState: ledger(s.bare).filter((item) => item.attemptId === s.attemptId).at(-1)?.state,
          clonePath: ctx.worktreePath, journal: readJournal(s.bare, branch),
          clean: localGit(ctx.worktreePath, "status", "--porcelain"),
          contents: fs.readFileSync(path.join(ctx.worktreePath, tracked), "utf8"),
          hasMarker: fs.existsSync(path.join(ctx.worktreePath, marker)),
        };
        throw new Error("1848 stop at model entry");
      } },
    });
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      calls.push(req);
      return {
        process: { state: req.site === "orphan_reclaim" && blocked ? blocked : "quiescent", processes: [], killed: [], detail: "/private/owner-path\n1848 scripted proof" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    };
    const logs = diagnosticLogger();
    const runner = runnerWith(factory, gitlab, undefined, loggerOverride ?? logs.logger, {
      recoveryRetryMs: 5, ...(enabled ? { dockerHost: "unix:///1848-scripted-only.sock", quiesceRun } : {}),
    });
    return { run: () => runner.execute(gitlabClaim(1810, { run_id: s.claimant, kind: "mr_rework", branch })),
      entry: () => entry, entryError: () => entryError, calls, events: logs.events, runner };
  }

  function assertEntry(s: Seed, e: ReturnType<typeof execution>, enabled: boolean, expectReclaimProof = true): void {
    if (expectReclaimProof) assertSafeEvents(e.events, [s.clonePath]);
    if (expectReclaimProof) assert.equal(e.events.filter((event) => event.event === "orphan_reclaim_succeeded").at(-1)?.reason,
      s.attemptId ? "retained-in-place" : "quarantined");
    assert.equal(e.entryError(), undefined, "predecessor bytes survived through model entry");
    const entry = e.entry();
    assert.ok(entry, "successor model entered");
    if (s.attemptId) {
      assert.equal(entry.retainedPath, s.clonePath, "attempt retained in place at model entry");
      assert.equal(entry.ledgerState, "reclaimed");
    } else assert.ok(entry.retainedPath.startsWith(holdingRoot() + path.sep));
    assert.equal(entry.clean, "", "successor clone is clean before model work");
    assert.equal(entry.contents, committed);
    assert.equal(entry.hasMarker, false);
    assert.deepEqual(entry.journal, {
      runId: s.claimant, clonePath: entry.clonePath,
      ...(enabled ? { attemptId: path.basename(entry.clonePath).split(".attempt-")[1] } : {}),
    });
    const canonical = git.runnerClonePath(s.bare, slug);
    if (enabled) {
      assert.ok(entry.clonePath.startsWith(`${canonical}.attempt-`));
      assert.notEqual(entry.clonePath, s.clonePath);
      const proof = e.calls.filter((req) => req.site === "orphan_reclaim");
      assert.equal(proof.length, expectReclaimProof ? 1 : 0);
      if (expectReclaimProof) {
        assert.equal(proof[0]!.mode, "capture");
        assert.deepEqual(proof[0]!.targetPaths, [s.clonePath]);
      }
    } else assert.equal(entry.clonePath, canonical);
  }

  for (const row of [
    { status: "failed", attempt: false, enabled: false },
    { status: "completed", attempt: false, enabled: false },
    { status: "completed", attempt: false, enabled: true },
    { status: "failed", attempt: true, enabled: true },
  ] as const) {
    it(`1848 ${row.status} MR owner moved A→B: ${row.attempt ? "attempt" : "canonical"}, attempts=${row.enabled}`, async (t) => {
      const s = await seed(row.attempt, row.status);
      const rpcCalls: { method: string; runId: string; beforeEntry: boolean }[] = [];
      const e = execution(s, row.enabled);
      for (const method of ["releaseRecoveryCustody", "settleRecoveryHold", "settleRecoveryHoldLive"] as const) {
        const real = client[method].bind(client);
        t.mock.method(client, method, async (...args: Parameters<typeof real>) => {
          rpcCalls.push({ method, runId: args[0], beforeEntry: !e.entry() });
          return Reflect.apply(real, client, args);
        });
      }
      try {
        await e.run();
        assertEntry(s, e, row.enabled);
        assertBytes(heldPath(s), s);
        assert.deepEqual(rpcCalls.filter((call) => call.beforeEntry || call.runId === s.owner), [],
          "phaseClone never releases custody, including already-published predecessor work");
        if (row.attempt) {
          assert.equal(ledger(s.bare).filter((item) => item.attemptId === s.attemptId).at(-1)?.state, "reclaimed");
          const canonical = git.runnerClonePath(s.bare, slug);
          // Four newer abandoned entries force a real retention deletion on the next seed.
          const abandoned: string[] = [];
          for (let i = 1; i <= 4; i++) {
            const id = formatAttemptId(new Date(`2026-02-0${i}T00:00:00Z`), 1, `000000000000000${i}`);
            const clonePath = `${canonical}.attempt-${id}`;
            abandoned.push(clonePath);
            fs.mkdirSync(clonePath);
            localGit(s.bare, "config", "--local", "--add", `uzi-attempts.${branch}.entry`,
              JSON.stringify({ attemptId: id, runId: randomUUID(), clonePath, state: "abandoned" }));
          }
          await git.runnerCloneForBranch(s.bare, branch, slug, noProofReseed, randomUUID(), false, undefined,
            attemptOptions(formatAttemptId(new Date("2026-03-01T00:00:00Z"), 1, "fedcba9876543210")));
          assertBytes(s.clonePath, s);
          assert.equal(ledger(s.bare).find((item) => item.attemptId === s.attemptId)?.state, "reclaimed");
          assert.equal(fs.existsSync(abandoned[0]!), false, "retention deleted the oldest disposable attempt");
          assert.equal(fs.existsSync(abandoned.at(-1)!), true, "retention kept a newer disposable attempt");
        }
      } finally { t.mock.restoreAll(); }
    });
  }

  for (const attempt of [false, true]) {
    for (const blocked of ["survivors", "unverified"] as const) {
      it(`1848 ${attempt ? "attempt" : "canonical"} orphan_reclaim ${blocked} preserves all custody`, async () => {
        const s = await seed(attempt, "completed");
        const journal = configValues(s.bare, `uzi-recovery.${branch}.clone`);
        const beforeLedger = configValues(s.bare, `uzi-attempts.${branch}.entry`);
        const e = execution(s, true, blocked);
        await e.run();
        assertSafeEvents(e.events, [s.clonePath, "/private/owner-path"]);
        assert.equal(e.events.at(-1)?.reason, "quiescence_blocked");
        assert.ok(!JSON.stringify(api.states).includes("/private/owner-path"));
        assert.equal(e.entry(), undefined);
        assert.deepEqual(configValues(s.bare, `uzi-recovery.${branch}.clone`), journal);
        assert.deepEqual(configValues(s.bare, `uzi-attempts.${branch}.entry`), beforeLedger);
        assertBytes(s.clonePath, s);
        const proof = e.calls.filter((req) => req.site === "orphan_reclaim");
        assert.equal(proof.length, 1);
        assert.equal(proof[0]!.mode, "capture");
        assert.deepEqual(proof[0]!.targetPaths, [s.clonePath]);
        assert.equal(api.states.filter((state) => state.runId === s.claimant && state.body.status === "failed")
          .at(-1)?.body.fail_origin, "worker_residue_blocked");
      });
    }
  }

  for (const stage of ["attempt-ledger", "attempt-journal", "canonical-journal"] as const) {
    it(`1848 ${stage} failure preserves protecting metadata and retries safely`, async () => {
      const s = await seed(stage !== "canonical-journal", "failed");
      const journal = configValues(s.bare, `uzi-recovery.${branch}.clone`);
      const beforeLedger = configValues(s.bare, `uzi-attempts.${branch}.entry`);
      type RunGit = (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string>;
      const seam = git as unknown as { runGit: RunGit };
      const real = seam.runGit;
      let injected = 0;
      seam.runGit = async (cwd, args, ...rest) => {
        const ledgerWrite = args[0] === "config" && args.includes("--add")
          && args.includes(`uzi-attempts.${branch}.entry`) && args.at(-1)?.includes('"state":"reclaimed"');
        const journalClear = args[0] === "config" && args.at(-2) === `uzi-recovery.${branch}.clone` && args.at(-1) === "";
        if (stage === "attempt-ledger" ? ledgerWrite : journalClear) {
          injected++;
          throw new Error(`1848 injected ${stage}`);
        }
        return Reflect.apply(real, git, [cwd, args, ...rest]);
      };
      const failed = execution(s, true);
      try { await failed.run(); } finally { seam.runGit = real; }
      assert.ok(injected > 0, "requested write failure was exercised");
      assertSafeEvents(failed.events, [s.clonePath]);
      assert.equal(failed.events.at(-1)?.stage, stage === "attempt-ledger" ? "attempt_ledger" : stage === "attempt-journal" ? "attempt_journal" : "retirement");
      if (stage === "canonical-journal") assert.equal(failed.events[0]?.stage, "journal_clear");
      assert.equal(failed.entry(), undefined);
      assert.deepEqual(configValues(s.bare, `uzi-recovery.${branch}.clone`), journal);
      if (stage === "attempt-ledger") {
        assert.deepEqual(configValues(s.bare, `uzi-attempts.${branch}.entry`), beforeLedger);
      } else if (stage === "attempt-journal") {
        assert.equal(ledger(s.bare).filter((item) => item.attemptId === s.attemptId).at(-1)?.state, "reclaimed");
      } else {
        assert.equal(fs.existsSync(s.clonePath), false, "post-rename failure already freed canonical layout");
      }
      const retained = heldPath(s);
      assertBytes(retained, s);
      const retry = execution(s, true);
      const clearedDuringClone: ReturnType<typeof readJournal>[] = [];
      seam.runGit = async (cwd, args, ...rest) => {
        if (!retry.entry() && args[0] === "config" && args.at(-2) === `uzi-recovery.${branch}.clone` && args.at(-1) === "") {
          clearedDuringClone.push(readJournal(s.bare, branch));
        }
        return Reflect.apply(real, git, [cwd, args, ...rest]);
      };
      try { await retry.run(); } finally { seam.runGit = real; }
      assert.ok(clearedDuringClone.every((capture) => capture?.runId !== s.claimant), "retry never clears successor custody during clone preparation");
      assertEntry(s, retry, true, stage !== "canonical-journal");
      assertBytes(retained, s);
    });
  }
  for (const malformed of ["null", "missing-fields", "status-accessor", "repo-accessor", "object-kind", "unknown-kind", "missing-kind", "null-kind"] as const) {
    it(`1848 M2 malformed classification ${malformed} preserves fixed refusal`, async (t) => {
      // missing-kind / null-kind seed the canonical clone an ISSUE owner of this branch would have
      // (`issue-1810`), so a kind defaulted to "issue" passes every later predicate and reclaims.
      const kindless = malformed === "missing-kind" || malformed === "null-kind";
      const s = await seed(false, "completed", kindless ? "issue-1810" : slug);
      const leaked = s.clonePath + "\nprivate classification accessor";
      const value = malformed === "null" ? null : malformed === "missing-fields" ? {} :
        malformed === "missing-kind" ? { status: "completed", repo_id: "r1", issue_iid: 1810 } :
        malformed === "null-kind" ? { status: "completed", repo_id: "r1", issue_iid: 1810, kind: null } :
        malformed === "object-kind" || malformed === "unknown-kind" ? {
          status: "completed", repo_id: "r1",
          kind: malformed === "object-kind" ? { branch, slug } : "/private/unknown-kind",
        } : Object.defineProperty({ status: "completed", repo_id: "r1" },
          malformed === "status-accessor" ? "status" : "repo_id", { get: () => { throw new Error(leaked); } });
      t.mock.method(client, "getRunOrphanClassification", async () => value);
      const e = execution(s, false);
      await e.run();
      assert.equal(e.entry(), undefined);
      assertSafeEvents(e.events, [s.clonePath, leaked]);
      assert.equal(e.events.at(-1)?.stage, "identity");
      assert.equal(e.events.at(-1)?.reason, "malformed_identity");
      // The kindless fixture's journal names the issue-keyed path, so the claimant reaches the
      // reclaim through the path-mismatch arm; both original refusals are content-free.
      assert.equal(api.states.filter((state) => state.runId === s.claimant && state.body.status === "failed").at(-1)?.body.failure_reason,
        kindless ? "recovery journal points at a different clone path than this branch's computed clone"
          : "refusing to replace a retained clone owned by another run");
      assertBytes(s.clonePath, s);
      assert.deepEqual(readJournal(s.bare, branch), { runId: s.owner, clonePath: s.clonePath });
    });
  }

  // This unwired reclaim path never reads flight; keep the private seam's fixture minimal.
  function reclaim(e: ReturnType<typeof execution>, s: Seed, original: Error): Promise<void> {
    const seam = e.runner as unknown as {
      reclaimTerminalOrphan: (bare: string, claim: ReturnType<typeof gitlabClaim>, flight: Record<string, never>,
        clonePath: string, branch: string, owner: string, original: Error) => Promise<void>;
    };
    return seam.reclaimTerminalOrphan(s.bare,
      gitlabClaim(1810, { run_id: s.claimant, kind: "mr_rework", branch }), {}, s.clonePath, branch, s.owner, original);
  }

  for (const refusal of ["foreign", "path-mismatch"] as const) {
    for (const diagnosticFault of ["logger", "fingerprint"] as const) {
      it(`1848 M2 404 ${refusal} keeps exact original with ${diagnosticFault} failure`, async (t) => {
        const s = await seed(false, "completed");
        const original = refusal === "foreign"
          ? new ForeignCaptureBlockedError(s.clonePath, branch, s.owner)
          : new CapturePathMismatchError(s.clonePath, "/private/claimant", branch, s.owner);
        const error = new RequestError("GET", "/private/classification", 404, "private body");
        t.mock.method(client, "getRunOrphanClassification", async () => { throw error; });
        const logs = diagnosticLogger();
        const logger: Logger = { ...logs.logger, warn: (event, fields) => {
          logs.logger.warn(event, fields);
          if (diagnosticFault === "logger") throw new Error(s.clonePath);
        } };
        if (diagnosticFault === "fingerprint") {
          const update = Hash.prototype.update;
          t.mock.method(Hash.prototype, "update", function(this: Hash, ...args: Parameters<Hash["update"]>) {
            if (args[0] === s.clonePath) throw new Error(s.clonePath);
            return Reflect.apply(update, this, args) as Hash;
          });
        }
        await assert.rejects(reclaim(execution(s, false, undefined, logger), s, original), (error) => error === original);
        assertSafeEvents(logs.events, [s.clonePath, error.path, error.body]);
        assert.equal(logs.events.at(-1)?.http_status, 404);
        if (diagnosticFault === "fingerprint") assert.equal(logs.events.at(-1)?.path_fingerprint, "0".repeat(64));
        assertBytes(s.clonePath, s);
        assert.deepEqual(readJournal(s.bare, branch), { runId: s.owner, clonePath: s.clonePath });
      });
    }
  }

  it("1848 M2 runner reports source-already-absent success", async () => {
    const s = await seed(false, "completed");
    fs.rmSync(s.clonePath, { recursive: true });
    const e = execution(s, false);
    await reclaim(e, s, new ForeignCaptureBlockedError(s.clonePath, branch, s.owner));
    assertSafeEvents(e.events, [s.clonePath]);
    assert.equal(e.events.at(-1)?.event, "orphan_reclaim_succeeded");
    assert.equal(e.events.at(-1)?.reason, "source-already-absent");
    assert.equal(readJournal(s.bare, branch), undefined);
  });

  for (const fault of ["transport", "derive", "path", "quiescence"] as const) {
    it(`1848 M2 ${fault} exception is a safe original refusal`, async (t) => {
      const s = await seed(false, "completed");
      const secret = "glpat-" + "0123456789abcdefghij";
      const error = new Error(`/private/owner-path\n${secret}`);
      if (fault === "transport") t.mock.method(client, "getRunOrphanClassification", async () => { throw error; });
      if (fault === "derive") t.mock.method(client, "getRunOrphanClassification", async () => ({
        status: "completed", repo_id: "r1", kind: "mr_rework", pipeline_ref: 42,
      }));
      if (fault === "path") t.mock.method(git, "classifyOwnerClonePath", async () => { throw error; });
      const e = execution(s, true);
      if (fault === "quiescence") {
        // Stub at the approved quiesceRun seam, without depending on process attribution.
        const original = e.runner;
        t.mock.method(original as unknown as { quiesceRun: () => Promise<unknown> }, "quiesceRun", async () => { throw error; });
      }
      await e.run();
      assert.equal(e.entry(), undefined);
      assertSafeEvents(e.events, [s.clonePath, secret, "/private/owner-path"]);
      assert.equal(e.events.at(-1)?.reason, { transport: "transport_unknown", derive: "malformed_identity", path: "path_error", quiescence: "quiescence_error" }[fault]);
      assert.equal(api.states.filter((state) => state.runId === s.claimant && state.body.status === "failed").at(-1)?.body.failure_reason,
        "refusing to replace a retained clone owned by another run");
      assertBytes(s.clonePath, s);
      assert.deepEqual(readJournal(s.bare, branch), { runId: s.owner, clonePath: s.clonePath });
    });
  }

  it("1848 M2 logger and fingerprint failures cannot undo completed reclaim", async (t) => {
    const s = await seed(false, "completed");
    const realHashUpdate = Hash.prototype.update;
    t.mock.method(Hash.prototype, "update", function(this: Hash, ...args: Parameters<Hash["update"]>) {
      if (args[0] === s.clonePath) throw new Error("hash failed");
      return Reflect.apply(realHashUpdate, this, args) as Hash;
    });
    const logs = diagnosticLogger();
    const e = execution(s, true, undefined, { ...logs.logger, info: (event, fields) => {
      logs.logger.info(event, fields);
      if (event.startsWith("orphan_")) throw new Error("logger failed");
    } });
    await e.run();
    assert.ok(e.entry());
    assertBytes(heldPath(s), s);
    assertSafeEvents(logs.events, [s.clonePath]);
    assert.equal(logs.events.at(-1)?.reason, "quarantined");
    assert.equal(logs.events.at(-1)?.path_fingerprint, "0".repeat(64));
  });

  it("1848 M2 HTTP refusal contains only numeric status; throwing logger preserves refusal", async (t) => {
    const s = await seed(false, "completed");
    const error = new RequestError("GET", "/private/path", 503, "glpat-" + "0123456789abcdefghij");
    t.mock.method(client, "getRunOrphanClassification", async () => { throw error; });
    const e = execution(s, true);
    await e.run();
    assertSafeEvents(e.events, [error.body, error.path]);
    assert.equal(e.events.at(-1)?.http_status, 503);
    const logger = { ...nullLogger(), warn: () => { throw error; } };
    const failedStates = () => api.states.filter((state) => state.runId === s.claimant && state.body.status === "failed");
    const before = failedStates().length;
    const retry = execution(s, true, undefined, logger);
    await retry.run();
    assert.equal(failedStates().length, before + 1, "the retry reported its own refusal");
    assert.equal(failedStates().at(-1)?.body.failure_reason,
      "refusing to replace a retained clone owned by another run");
  });

});

describe("atomic runner-clone release (#1315) + owner-derived reclaim (#1319)", () => {
  it("Test 1 (Gap 1, Case A cross-kind): an mr_rework reclaims a TERMINAL issue owner's residue and reseeds at its own slug", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1401;
    const ownerRunId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const claimantRunId = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
    // An ISSUE owner seeded at `.../issue-N`, journaled under branch `agent/issue-N`.
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
    // The claimant is an mr_rework on the SAME branch → slug `agent-issue-N`, so the journal
    // path (`.../issue-N`) diverges from this claimant's computed clone → Case A.
    const freshPath = git.runnerClonePath(bare, `agent-issue-${iid}`);
    // branch NULL exercises the issue_iid-only derivation (the realistic pre-completion case).
    api.setOrphanClassification(ownerRunId, {
      status: "completed",
      repo_id: "r1",
      kind: "issue",
      issue_iid: iid,
      branch: null,
      pipeline_ref: null,
      pipeline_id: null,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean; oldGone: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
            oldGone: !fs.existsSync(clonePath),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(gitlabClaim(iid, { run_id: claimantRunId, kind: "mr_rework", branch: `agent/issue-${iid}` }));

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded");
    assert.equal(observed!.worktree, freshPath, "the reseed lands at the mr_rework slug `agent-issue-N`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.oldGone, true, "the old `.../issue-N` was RENAMED into the quarantine");
    assert.equal(observed!.quarantineHasResidue, true, "the owner residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, freshPath, "the journal points at the fresh `agent-issue-N` clone");
  });

  it("Test 2 (Gap 2, Case B same-slug worker-move): reclaim succeeds via the NEW owner-scoped read despite an ownership 404", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1402;
    const ownerRunId = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
    const claimantRunId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
    // Simulate the worker move: the OLD worker-scoped ownership probe 404s (owner not on this
    // worker), but the NEW owner-scoped orphan-classification read is authoritative.
    api.setOwnershipNotOwned(ownerRunId);
    api.setOrphanClassification(ownerRunId, {
      status: "completed",
      repo_id: "r1",
      kind: "issue",
      issue_iid: iid,
      branch: null,
      pipeline_ref: null,
      pipeline_id: null,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(gitlabClaim(iid, { run_id: claimantRunId }));

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded via the owner-scoped read");
    assert.equal(observed!.worktree, clonePath, "the reseed lands at the canonical `.../issue-N`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.quarantineHasResidue, true, "the residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, clonePath);
  });

  it("Test 2b (ci_fix pipeline_id arm, moved-worker default-branch reclaim end-to-end)", async () => {
    const { gitlab } = fakeGitlab();
    const pid = 5150;
    const ownerRunId = "e1111111-1111-4111-8111-111111111111";
    const claimantRunId = "e2222222-2222-4222-8222-222222222222";
    // A ci_fix owner on the default branch: branch `ci-fix/pipeline-5150`, slug `ci-fix-pipeline-5150`.
    const { bare, branch, clonePath } = await seedResidueForBranch(
      `ci-fix/pipeline-${pid}`,
      `ci-fix-pipeline-${pid}`,
      ownerRunId,
      "FOREIGN.txt",
    );
    // The owner-side deriveCloneKey reconstructs `ci-fix/pipeline-5150` from pipeline_id +
    // pipeline_ref + the claimant's default branch "main" → predicates (c)/(d) hold.
    api.setOrphanClassification(ownerRunId, {
      status: "failed",
      repo_id: "r1",
      kind: "ci_fix",
      issue_iid: null,
      branch: null,
      pipeline_ref: "main",
      pipeline_id: pid,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(
      gitlabClaim(0, {
        run_id: claimantRunId,
        kind: "ci_fix",
        pipeline: { id: pid, ref: "main", sha: "0".repeat(40), web_url: "https://x/p", failed_jobs: [] },
        repo: { id: "r1", url: "https://x/r", clone_url: fx.originPath, default_branch: "main" },
      }),
    );

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded");
    assert.equal(observed!.worktree, clonePath, "the reseed lands at the ci-fix slug `ci-fix-pipeline-5150`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.quarantineHasResidue, true, "the residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, clonePath);
  });

  // Test 3 — fail closed, one row per predicate. Each row asserts: the model never starts,
  // retireRunnerClone is NEVER called, the run ends failed, the journal + seeded clone are
  // untouched, and no quarantine subtree is created.
  interface FailClosedRow {
    name: string;
    iid: number;
    /** Optional claim-field overrides merged into the claimant's `gitlabClaim`. Absent ⇒ a
     *  plain ISSUE claim on `iid` (the four original rows). A row sets this to make the
     *  claimant a different kind (e.g. a `task` run, whose slug is DECOUPLED from its branch)
     *  so a single predicate can be isolated. */
    claimOverrides?: Record<string, unknown>;
    /** Build the (bare, branch, residue, expected journal) fixture and set the fake's read.
     *  The claimant is an issue run on `iid`, so each row is Case A or Case B as noted. */
    setup: (ctx: {
      iid: number;
      ownerRunId: string;
    }) => Promise<{ bare: string; branch: string; residuePath: string; expectJournal: { runId: string; clonePath: string } }>;
  }

  const failClosedRows: FailClosedRow[] = [
    {
      name: "(b) wrong repo",
      iid: 1411,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r2",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      name: "(c) wrong owner-derived branch",
      iid: 1412,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "issue",
          issue_iid: 9999, // → owner-derived branch `agent/issue-9999` ≠ journal branch
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      name: "(d) wrong in-tree journal path",
      iid: 1413,
      setup: async ({ iid, ownerRunId }) => {
        const bare = await git.ensureClone(fx.originPath);
        const branch = `agent/issue-${iid}`;
        // A DIFFERENT in-tree clone; journal THIS branch to point at it → Case A. (c) passes
        // (owner branch `agent/issue-N`), (d) fails (owner slug `issue-N` ≠ `.../issue-9470`).
        const other = await git.createOrAttachRunnerClone(bare, 9470, noProofReseed, ownerRunId);
        fs.writeFileSync(path.join(other.path, "FOREIGN.txt"), "owner-only bytes\n");
        await git.markRecoveryCapture(bare, other.path, branch, ownerRunId);
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: other.path, expectJournal: { runId: ownerRunId, clonePath: other.path } };
      },
    },
    {
      name: "(malformed) insufficient identity",
      iid: 1414,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        // mr_rework with no branch (pipeline_ref null) → deriveCloneKey returns undefined.
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "mr_rework",
          issue_iid: null,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      // Isolates predicate (c) — it is the ONLY guard a `task` owner can trip here, because a
      // task's slug is `task-<runId>` (DECOUPLED from its branch), so a mismatched owner-derived
      // BRANCH still reproduces the journaled PATH: (d) passes, only (c) fires. (The issue-kind
      // "(c) wrong owner-derived branch" row above ALSO trips (d), since issue derives both from
      // issue_iid — deleting the (c) line leaves it green, but reddens THIS row.)
      name: "(c-isolated) task owner: branch mismatch but slug/path match",
      iid: 1415,
      claimOverrides: { kind: "task", branch: "uzi/task/foo" },
      setup: async ({ ownerRunId }) => {
        // Seed the OWNER's task clone at branch `uzi/task/foo`, slug `task-<ownerRunId>`: the
        // journal is keyed under `uzi/task/foo` and points at `.../task-<ownerRunId>`. The
        // claimant (a task on `uzi/task/foo`, slug `task-<claimantRunId>`) reads that journal
        // and computes a DIFFERENT path → Case A / CapturePathMismatchError.
        const { bare, branch, clonePath } = await seedResidueForBranch(
          "uzi/task/foo",
          `task-${ownerRunId}`,
          ownerRunId,
          "FOREIGN.txt",
        );
        // Owner-derived branch `uzi/task/DIFFERENT` ≠ journal branch `uzi/task/foo` → (c) FIRES,
        // but owner-derived slug `task-<ownerRunId>` reproduces the journaled path exactly → (d)
        // PASSES. This is the scenario predicate (c) exclusively guards.
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "task",
          issue_iid: null,
          branch: "uzi/task/DIFFERENT",
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
  ];

  for (const row of failClosedRows) {
    it(`Test 3: fail closed ${row.name} — no probe-driven reclaim, journal & clone untouched`, async () => {
      const { gitlab } = fakeGitlab();
      const ownerRunId = `f0000000-0000-4000-8000-00000000${row.iid}`;
      const claimantRunId = `f1000000-0000-4000-8000-00000000${row.iid}`;
      const { bare, branch, residuePath, expectJournal } = await row.setup({ iid: row.iid, ownerRunId });

      let retireCalls = 0;
      const origRetire = git.retireRunnerClone.bind(git);
      git.retireRunnerClone = async (b, c, br, r, o) => {
        retireCalls++;
        return origRetire(b, c, br, r, o);
      };
      let ran = false;
      const factory: ExecutorFactory = () => ({
        homeDir: path.join(homeDir, claimantRunId),
        executor: {
          run: async () => {
            ran = true;
            throw new Error("the model must never start on a fail-closed clone");
          },
        },
      });
      const logs = diagnosticLogger();
      const runner = runnerWith(factory, gitlab, undefined, logs.logger, { recoveryRetryMs: 5 });
      try {
        await runner.execute(gitlabClaim(row.iid, { run_id: claimantRunId, ...row.claimOverrides }));
      } finally {
        git.retireRunnerClone = origRetire;
      }

      assertSafeEvents(logs.events);
      assert.equal(logs.events.at(-1)?.event, "orphan_reclaim_refused");
      assert.equal(ran, false, "the model never starts");
      assert.equal(logs.events.at(-1)?.reason, row.iid === 1411 ? "repo_mismatch" : row.iid === 1413 ? "path_mismatch" : row.iid === 1414 ? "malformed_identity" : "branch_mismatch");
      assert.equal(retireCalls, 0, "retireRunnerClone is NEVER called on an unmet predicate");
      const failure = api.states.filter((s) => s.runId === claimantRunId && s.body.status === "failed").at(-1)?.body.failure_reason;
      assert.equal(failure, row.iid === 1413 || row.iid === 1415
        ? "recovery journal points at a different clone path than this branch's computed clone"
        : "refusing to replace a retained clone owned by another run");
      assert.ok(!String(failure).includes(residuePath), "failure_reason excludes the journaled worker path");
      assert.deepEqual(readJournal(bare, branch), expectJournal, "the journal is untouched");
      assert.equal(
        fs.readFileSync(path.join(residuePath, "FOREIGN.txt"), "utf8"),
        "owner-only bytes\n",
        "the seeded clone is untouched",
      );
      assert.equal(fs.existsSync(holdingRoot()), false, "no quarantine subtree is created");
    });
  }

  // Test 4 — fail closed on the probe itself. issue owner + claimant issue run (Case B).
  for (const scenario of ["non-terminal", "404", "transient"] as const) {
    it(`Test 4: fail closed on probe (${scenario}) — no reclaim, journal & clone untouched`, async () => {
      const { gitlab } = fakeGitlab();
      const n = scenario === "non-terminal" ? 1 : scenario === "404" ? 2 : 3;
      const iid = 1420 + n;
      const ownerRunId = `a3000000-0000-4000-8000-00000000000${n}`;
      const claimantRunId = `a4000000-0000-4000-8000-00000000000${n}`;
      const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
      if (scenario === "non-terminal") {
        api.setOrphanClassification(ownerRunId, {
          status: "running",
          repo_id: "r1",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
      } else if (scenario === "404") {
        api.setOrphanNotFound(ownerRunId);
      } else {
        api.failOrphanClassification(ownerRunId, 503);
      }

      let retireCalls = 0;
      const origRetire = git.retireRunnerClone.bind(git);
      git.retireRunnerClone = async (b, c, br, r, o) => {
        retireCalls++;
        return origRetire(b, c, br, r, o);
      };
      let ran = false;
      const factory: ExecutorFactory = () => ({
        homeDir: path.join(homeDir, claimantRunId),
        executor: {
          run: async () => {
            ran = true;
            throw new Error("the model must never start on a fail-closed clone");
          },
        },
      });
      const logs = diagnosticLogger();
      const runner = runnerWith(factory, gitlab, undefined, logs.logger, { recoveryRetryMs: 5 });
      try {
        await runner.execute(gitlabClaim(iid, { run_id: claimantRunId }));
      } finally {
        git.retireRunnerClone = origRetire;
      }

      assertSafeEvents(logs.events);
      assert.equal(logs.events.at(-1)?.event, "orphan_reclaim_refused");
      assert.equal(ran, false, "the model never starts");
      assert.equal(logs.events.at(-1)?.reason, scenario === "non-terminal" ? "nonterminal_owner" : "http");
      assert.equal(logs.events.at(-1)?.http_status, scenario === "non-terminal" ? undefined : scenario === "404" ? 404 : 503);
      assert.equal(retireCalls, 0, "no reclaim on a non-terminal / 404 / transient probe");
      assert.ok(api.states.some((s) => s.body.status === "failed"), "the run fails closed");
      assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is untouched");
      assert.equal(
        fs.readFileSync(path.join(clonePath, "FOREIGN.txt"), "utf8"),
        "owner-only bytes\n",
        "the canonical clone is untouched",
      );
      assert.equal(fs.existsSync(holdingRoot()), false, "no quarantine subtree is created");
    });
  }

  it("T4: the owner's terminal retire releases the canonical even when the trash delete fails", async () => {
    const iid = 1420;
    const ownerRunId = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "TRASH_ME.txt");
    const hRoot = holdingRoot();
    // Make ONLY the post-rename trash delete fail (targeted by path so no other fs.rm is
    // affected); this file's tests run in an isolated process, and we restore in finally.
    const origRm = fsp.rm.bind(fsp);
    (fsp as { rm: typeof fsp.rm }).rm = (async (p: fs.PathLike, opts?: Parameters<typeof origRm>[1]) => {
      if (String(p).startsWith(hRoot)) throw Object.assign(new Error("injected trash delete failure"), { code: "EPERM" });
      return origRm(p, opts);
    }) as typeof fsp.rm;
    try {
      await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
    } finally {
      (fsp as { rm: typeof fsp.rm }).rm = origRm;
    }

    // The RELEASE (atomic rename + journal clear) is durable regardless of disposal.
    assert.equal(fs.existsSync(clonePath), false, "the canonical is free (renamed away)");
    assert.equal(readJournal(bare, branch), undefined, "the (runId, clonePath) journal is cleared");
    // The isolated trash residue remains (the delete failed), and NOT at canonical.
    const dirs = fs.readdirSync(hRoot);
    assert.equal(dirs.length, 1, "the failed delete leaves exactly the one isolated trash dir");
    assert.equal(fs.readFileSync(path.join(hRoot, dirs[0]!, "TRASH_ME.txt"), "utf8"), "owner-only bytes\n");
    // A subsequent run seeds cleanly on the now-free branch.
    const reseeded = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, "ffffffff-ffff-4fff-8fff-ffffffffffff");
    assert.equal(reseeded.path, clonePath);
    assert.equal(fs.existsSync(clonePath), true);
  });

  it("T5: a journal rewritten between the guard and retire makes retire move NOTHING", async () => {
    const iid = 1430;
    const ownerRunId = "11111111-1111-4111-8111-111111111111";
    const successorRunId = "22222222-2222-4222-8222-222222222222";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "SUCCESSOR.txt");
    // The lock gap: after the guard threw ForeignCaptureBlockedError(ownerRunId), a
    // successor re-journals the SAME canonical path under a DIFFERENT runId.
    await git.markRecoveryCapture(bare, clonePath, branch, successorRunId);

    await assert.rejects(
      git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false }),
      CapturePathMismatchError,
      "retire fails closed on a pre-rename pair mismatch",
    );

    assert.equal(fs.existsSync(clonePath), true, "the successor's clone is untouched");
    assert.equal(fs.readFileSync(path.join(clonePath, "SUCCESSOR.txt"), "utf8"), "owner-only bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: successorRunId, clonePath }, "the successor journal is untouched");
    assert.equal(fs.existsSync(holdingRoot()), false, "nothing was moved to a quarantine");
  });

  it("T6: the quarantine is OUTSIDE runnerRoot and the holding subtree is mode 0700", async () => {
    const iid = 1440;
    const ownerRunId = "33333333-3333-4333-8333-333333333333";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "Q.txt");
    fs.writeFileSync(path.join(clonePath, ".uzi", "scratch", "gate-log.test"), "done");
    await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false });
    assert.equal(fs.existsSync(path.join(clonePath, ".uzi", "scratch")), false, "quarantine removes scratch from the canonical clone path");

    const hRoot = holdingRoot();
    assert.equal(fs.existsSync(hRoot), true);
    assert.ok(
      !hRoot.startsWith(runnerRoot() + path.sep) && hRoot !== runnerRoot(),
      "the quarantine root is a SIBLING of runnerRoot, never under the runner-writable tree",
    );
    const mode = fs.statSync(hRoot).mode & 0o777;
    assert.equal(mode & 0o077, 0, "the holding subtree grants NOTHING to group/other");
    assert.equal(mode & 0o700, 0o700, "the owner holds rwx on the holding subtree (0700)");
    const dirs = fs.readdirSync(hRoot);
    assert.equal(dirs.length, 1, "the residue is retained (discard:false)");
    assert.equal(fs.existsSync(path.join(hRoot, dirs[0]!, "Q.txt")), true);
    assert.equal(fs.readFileSync(path.join(hRoot, dirs[0]!, ".uzi", "scratch", "gate-log.test"), "utf8"), "done");
  });

  it("settled retirement removes scratch with the discarded runner clone", async () => {
    const ownerRunId = "34343434-3434-4434-8434-343434343434";
    const { bare, branch, clonePath } = await seedResidue(1441, ownerRunId, "Q.txt");
    fs.writeFileSync(path.join(clonePath, ".uzi", "scratch", "gate-log.test"), "done");
    await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
    assert.equal(fs.existsSync(clonePath), false);
    assert.equal(readJournal(bare, branch), undefined);
    assert.deepEqual(fs.readdirSync(holdingRoot()), []);
  });

  it("issue #1783: a failed `retired` ledger append neither fails the retire nor leaks the holding copy", async () => {
    const ownerRunId = "35353535-3535-4535-8535-353535353535";
    const { bare, branch, clonePath } = await seedResidue(1442, ownerRunId, "Q.txt");
    type Append = (b: string, br: string, e: { state: string }) => Promise<void>;
    const target = git as unknown as { appendAttemptLedger: Append };
    const orig = target.appendAttemptLedger;
    let failed = 0;
    target.appendAttemptLedger = async function (this: unknown, b, br, e) {
      if (e.state === "retired") {
        failed++;
        throw new Error("injected ledger append failure");
      }
      return orig.call(this, b, br, e);
    };
    try {
      await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true, attemptId: "20260927T000000Z-g1-0123456789abcdef" });
    } finally {
      target.appendAttemptLedger = orig;
    }
    assert.equal(failed, 1, "the retired append was attempted and failed");
    assert.equal(fs.existsSync(clonePath), false, "the canonical is free");
    assert.equal(readJournal(bare, branch), undefined, "the journal is cleared");
    assert.deepEqual(fs.readdirSync(holdingRoot()), [], "step 6 still discarded the holding copy");
  });

  it("T7a: a missing SOURCE is treated as already-free (journal cleared, no throw)", async () => {
    const iid = 1450;
    const ownerRunId = "44444444-4444-4444-8444-444444444444";
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    const clonePath = worktreeDirFor(iid); // under runnerRoot, but NEVER created on disk
    await git.markRecoveryCapture(bare, clonePath, branch, ownerRunId);

    // rename ENOENT -> lstat(source) ENOENT -> canonical already free -> clear the journal.
    await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
    assert.equal(readJournal(bare, branch), undefined, "a confirmed-free canonical clears the journal");
  });

  it("T7b: a rename ENOENT with the source present surfaces as a real error (journal intact)", async () => {
    const iid = 1451;
    const ownerRunId = "55555555-5555-4555-8555-555555555555";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    // Simulate a rename ENOENT (a DESTINATION-parent problem) while the SOURCE is present.
    const origRename = fsp.rename.bind(fsp);
    (fsp as { rename: typeof fsp.rename }).rename = (async () => {
      throw Object.assign(new Error("simulated missing destination parent"), { code: "ENOENT" });
    }) as typeof fsp.rename;
    try {
      await assert.rejects(
        git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true }),
        /simulated missing destination parent/,
        "a rename ENOENT while the source is present is a REAL error, never treated as free",
      );
    } finally {
      (fsp as { rename: typeof fsp.rename }).rename = origRename;
    }
    assert.equal(fs.readFileSync(path.join(clonePath, "KEEP.txt"), "utf8"), "owner-only bytes\n", "the source is untouched");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is NOT cleared");
  });

  it("T8: a journaled clonePath OUTSIDE runnerRoot fails closed and moves nothing", async () => {
    const iid = 1460;
    const ownerRunId = "66666666-6666-4666-8666-666666666666";
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    // A journaled path OUTSIDE the runner-writable tree (a sibling under dataDir).
    const outside = path.join(fx.dataDir, "not-runner", "evil-clone");
    fs.mkdirSync(outside, { recursive: true });
    fs.writeFileSync(path.join(outside, "OUTSIDE.txt"), "outside bytes\n");
    await git.markRecoveryCapture(bare, outside, branch, ownerRunId);

    await assert.rejects(
      git.retireRunnerClone(bare, outside, branch, ownerRunId, { discard: true }),
      CapturePathMismatchError,
      "a path outside runnerRoot is never retired, whatever the journal claims",
    );
    assert.equal(fs.existsSync(outside), true, "the out-of-tree path is not moved");
    assert.equal(fs.readFileSync(path.join(outside, "OUTSIDE.txt"), "utf8"), "outside bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath: outside }, "the journal is untouched");
  });

  it("T1(git): the guard classifies same-run / same-path-foreign / different-path", async () => {
    const iid = 1480;
    const ownerRunId = "99999999-9999-4999-8999-999999999999";
    const foreignRunId = "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "GUARD.txt");

    // Case C — same run: PendingRecoveryCaptureError.
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, ownerRunId),
      (err: unknown) => {
        assert.equal((err as Error).name, "PendingRecoveryCaptureError");
        return true;
      },
      "same-run reseed must be captured first",
    );
    // Case B — same canonical path, foreign owner: ForeignCaptureBlockedError with fields.
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, noProofReseed, foreignRunId),
      (err: unknown) => {
        assert.ok(err instanceof ForeignCaptureBlockedError);
        assert.equal(err.ownerRunId, ownerRunId);
        assert.equal(err.clonePath, clonePath);
        assert.equal(err.branch, branch);
        return true;
      },
      "same-path foreign owner is the reclaimable case",
    );
    // Case A — a different clone key computes a different path: CapturePathMismatchError.
    await assert.rejects(
      git.runnerCloneForBranch(bare, branch, "different-kind-clone", noProofReseed, foreignRunId),
      (err: unknown) => {
        assert.ok(err instanceof CapturePathMismatchError);
        assert.equal(err.journaledPath, clonePath);
        assert.equal(err.branch, branch);
        // issue #1319: ownerRunId is now load-bearing — the runner's Case A reclaim probes it.
        assert.equal(err.ownerRunId, ownerRunId);
        return true;
      },
      "a different clone key is the runner's owner-derived reclaim case (#1319)",
    );
    // The journal is untouched by all three fail-closed classifications.
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath });
  });
});

// issue #1354 — retireRunnerClone's EXDEV fallback. On a docker-lane (dind) worker
// `/data/runner` (runnerRoot) is a separate emptyDir while `/data/runner-quarantine`
// (the retire destination) is on the `/data` PVC — DIFFERENT devices — so the step-4
// `fs.rename(clonePath, holdingDest)` returns EXDEV and the old catch (ENOENT-only)
// rethrew it, wedging the run in a re-park loop. The fix frees the canonical with an
// intra-device atomic rename into a scratch parent under runnerRoot (same device, never
// EXDEV, survives a daemon file-hold), preserving the #1315 invariant that the journaled
// canonical is freed ONLY by an atomic rename — never a direct recursive rm of residue.
//
// These tests faithfully model docker-lane by monkeypatching `fsp.rename` PATH-SELECTIVELY:
// a rename whose destination is under holdingRoot() (the PVC) throws EXDEV, while the
// intra-device scratch rename (destination under runnerRoot()) delegates to the real
// rename. `fsp.cp`/`fsp.rm` stay real unless a specific test needs otherwise. The existing
// #1315 tests (T4/T5/T6/T7a/T7b/T8) never patch rename, so their same-filesystem renames
// still succeed and never enter the EXDEV branch.
describe("retireRunnerClone EXDEV fallback (#1354)", () => {
  /** Model docker-lane: a rename whose DEST is under the PVC quarantine throws EXDEV, so the
   *  intra-device scratch rename (dest under runnerRoot) is the only one that can succeed.
   *  `onScratch`, when given, overrides the scratch rename (dest under `runnerRoot/.retire-`)
   *  so a test can make ONLY that rename fail. Returns a restore fn to call in a finally. */
  function stubDockerLaneRename(
    onScratch?: (from: fs.PathLike, to: fs.PathLike) => Promise<never>,
  ): () => void {
    const orig = fsp.rename.bind(fsp);
    (fsp as { rename: typeof fsp.rename }).rename = (async (from: fs.PathLike, to: fs.PathLike) => {
      const dest = String(to);
      if (dest.startsWith(holdingRoot())) {
        throw Object.assign(new Error("EXDEV: cross-device link not permitted"), { code: "EXDEV" });
      }
      if (onScratch && dest.startsWith(path.join(runnerRoot(), ".retire-"))) {
        return onScratch(from, to);
      }
      return orig(from, to);
    }) as typeof fsp.rename;
    return () => {
      (fsp as { rename: typeof fsp.rename }).rename = orig;
    };
  }

  const scratchDirs = (): string[] => fs.readdirSync(runnerRoot()).filter((d) => d.startsWith(".retire-"));

  async function bindAndMoveSocket(
    server: net.Server,
    clonePath: string,
    move: (from: string, to: string) => void,
    retire: () => Promise<void>,
  ): Promise<void> {
    const short = shortUnixSocket();
    let bound = false;
    try {
      await listenUnix(server, short.socket);
      bound = true;
      move(short.socket, path.join(clonePath, ".sock"));
      await retire();
    } finally {
      try {
        if (bound) {
          await new Promise<void>((resolve) => server.close(() => resolve()));
        }
      } finally {
        short.dispose();
      }
    }
  }

  it("T1354-a (discard EXDEV): the atomic rename frees the canonical past a FIFO + socket; no quarantine retained", async () => {
    const iid = 1470;
    const ownerRunId = "d1354001-0000-4000-8000-000000000001";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "DISCARD.txt");
    // A FIFO and a bound UNIX socket inside the clone — fs.cp throws on these, so the fact
    // the discard path frees the clone anyway proves it never copies (pure atomic rename,
    // which is exactly what tolerates the git fsmonitor socket on a real docker-lane clone).
    // Bind at a short path first, then move the live socket node into the clone: macOS caps UNIX
    // socket addresses at 104 bytes, while this fixture's intentionally nested clone path is longer.
    // Issue #2044: under a Codex run's TMPDIR even fx.dataDir is too long, so the short path comes
    // from shortUnixSocket.
    execFileSync("mkfifo", [path.join(clonePath, "worktree.fifo")]);
    const server = net.createServer();
    await bindAndMoveSocket(server, clonePath, fs.renameSync, async () => {
      const restore = stubDockerLaneRename();
      try {
        await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
      } finally {
        restore();
      }
    });

    assert.equal(fs.existsSync(clonePath), false, "the canonical is freed by the intra-device atomic rename");
    assert.equal(readJournal(bare, branch), undefined, "the journal is cleared once the rename frees the canonical");
    const q = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
    assert.equal(q.length, 0, "the discard path retains NO quarantine subtree");
    assert.equal(scratchDirs().length, 0, "the intra-device scratch parent is disposed out of the lock");
  });

  it("T1354-a socket move failure preserves the error and closes the bound server", { timeout: 5000 }, async () => {
    const { clonePath } = await seedResidue(1477, "d1354007-0000-4000-8000-000000000007", "MOVE.txt");
    const server = net.createServer();
    const moveError = new Error("T1354-a socket move failed");
    let movedAfterBind = false;
    try {
      await assert.rejects(
        bindAndMoveSocket(server, clonePath, (from) => {
          movedAfterBind = server.listening && fs.existsSync(from);
          throw moveError;
        }, async () => {}),
        (error: unknown) => error === moveError,
      );
      assert.equal(movedAfterBind, true, "the move fails after the socket is bound");
      assert.equal(server.listening, false, "a failed socket move closes the bound server");
    } finally {
      if (server.listening) {
        await new Promise<void>((resolve) => server.close(() => resolve()));
      }
    }
  });

  it("T1354-a bind failure preserves the original listen error", { timeout: 5000 }, async () => {
    const server = net.createServer();
    const listenError = new Error("T1354-a socket bind failed");
    server.listen = (() => {
      queueMicrotask(() => server.emit("error", listenError));
      return server;
    }) as typeof server.listen;
    let moveCalled = false;
    await assert.rejects(
      bindAndMoveSocket(server, runnerRoot(), () => { moveCalled = true; }, async () => {}),
      (error: unknown) => error === listenError,
    );
    assert.equal(moveCalled, false, "a failed bind never attempts the move");
    assert.equal(server.listening, false, "a failed bind leaves the server unbound");
  });

  it("T1354-b (!discard EXDEV): copy-before-free keeps a symlink-faithful quarantine and skips the FIFO", async () => {
    const iid = 1471;
    const ownerRunId = "d1354002-0000-4000-8000-000000000002";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    // A symlink pointing OUTSIDE the clone tree, and a FIFO (fs.cp throws on a FIFO, so the
    // filter must skip it).
    const outsideTarget = path.join(fx.dataDir, "outside-target.txt");
    fs.writeFileSync(outsideTarget, "outside\n");
    fs.symlinkSync(outsideTarget, path.join(clonePath, "outlink"));
    execFileSync("mkfifo", [path.join(clonePath, "worktree.fifo")]);

    const restore = stubDockerLaneRename();
    try {
      await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false });
    } finally {
      restore();
    }

    assert.equal(fs.existsSync(clonePath), false, "the canonical is freed after the copy completes");
    assert.equal(readJournal(bare, branch), undefined, "the journal is cleared");
    const q = fs.readdirSync(holdingRoot());
    assert.equal(q.length, 1, "the quarantine subtree is RETAINED (discard:false)");
    const held = path.join(holdingRoot(), q[0]!);
    assert.equal(fs.readFileSync(path.join(held, "KEEP.txt"), "utf8"), "owner-only bytes\n", "the tree was copied");
    const copiedLink = path.join(held, "outlink");
    assert.equal(fs.lstatSync(copiedLink).isSymbolicLink(), true, "the symlink is copied AS a link, not dereferenced");
    assert.equal(fs.readlinkSync(copiedLink), outsideTarget, "the symlink target is preserved verbatim");
    assert.equal(fs.existsSync(path.join(held, "worktree.fifo")), false, "the FIFO is skipped by the filter");
    assert.equal(scratchDirs().length, 0, "the intra-device scratch parent is disposed out of the lock");
  });

  // T1354-c — the discriminating partial-rm-then-throw test. The scratch-disposal recursive
  // rm deletes an inner entry then THROWS, constructed so the top-level scratch dir stays
  // present. The forbidden mutant (a direct recursive rm of the journaled canonical BEFORE
  // the journal clear) would, under the same partial failure, leave the canonical
  // partial/present with a live journal → a permanent capture-guard wedge. Here we assert
  // that at the moment the rm throws the canonical is ALREADY gone (via the rename) and the
  // journal ALREADY cleared, that the fault targeted the SCRATCH (never clonePath), and that
  // a subsequent claim reseeds without any capture-guard error. Each claimant runs on its
  // OWN seeded fixture: a same-runId claim (Case C, would-be PendingRecoveryCaptureError) and
  // a different-runId claim (would-be ForeignCaptureBlockedError), for both discard values.
  async function partialDisposalNoWedge(o: {
    iid: number;
    ownerRunId: string;
    discard: boolean;
    claimantRunId: string;
  }): Promise<void> {
    const { bare, branch, clonePath } = await seedResidue(o.iid, o.ownerRunId, "MARK.txt");
    let rmFaultTarget: string | undefined;
    let cloneGoneAtFault: boolean | undefined;
    let journalClearedAtFault: boolean | undefined;

    const restoreRename = stubDockerLaneRename();
    const origRm = fsp.rm.bind(fsp);
    let injected = false;
    // Intercept the FIRST recursive rm of EITHER the scratch parent OR the canonical: the
    // correct code only ever recursively rm's the SCRATCH (the canonical was atomically
    // renamed away), whereas the forbidden mutant (a direct recursive rm of the journaled
    // canonical before the journal clear) would recursively rm clonePath here. Whatever the
    // target, delete ONE inner entry then THROW, leaving the top-level dir PRESENT — so the
    // mutant would leave the canonical partial/present with a live journal.
    (fsp as { rm: typeof fsp.rm }).rm = (async (p: fs.PathLike, opts?: Parameters<typeof origRm>[1]) => {
      const target = String(p);
      const matched = !injected && (target === clonePath || target.startsWith(path.join(runnerRoot(), ".retire-")));
      if (matched) {
        injected = true;
        rmFaultTarget = target;
        cloneGoneAtFault = !fs.existsSync(clonePath);
        journalClearedAtFault = readJournal(bare, branch) === undefined;
        const inner = fs.existsSync(target) ? fs.readdirSync(target) : [];
        if (inner.length > 0) await origRm(path.join(target, inner[0]!), { recursive: true, force: true });
        throw Object.assign(new Error("injected recursive-rm partial failure"), { code: "EIO" });
      }
      return origRm(p, opts);
    }) as typeof fsp.rm;

    try {
      // Disposal is out-of-lock and best-effort (.catch), so retire itself RESOLVES.
      await git.retireRunnerClone(bare, clonePath, branch, o.ownerRunId, { discard: o.discard });
    } finally {
      restoreRename();
      (fsp as { rm: typeof fsp.rm }).rm = origRm;
    }

    assert.ok(rmFaultTarget, "the scratch disposal rm was attempted");
    assert.ok(
      rmFaultTarget!.startsWith(path.join(runnerRoot(), ".retire-")),
      "the disposal fault target is the SCRATCH parent",
    );
    assert.notEqual(rmFaultTarget, clonePath, "the disposal NEVER recursively rm's the journaled canonical directly");
    assert.equal(cloneGoneAtFault, true, "at the moment the rm throws, the canonical is ALREADY freed (via the rename)");
    assert.equal(journalClearedAtFault, true, "at the moment the rm throws, the journal is ALREADY cleared");

    assert.equal(scratchDirs().length, 1, "the partial disposal left the top-level scratch dir present (harmless)");
    assert.equal(fs.existsSync(clonePath), false, "the canonical remains free after the failed disposal");
    assert.equal(readJournal(bare, branch), undefined, "the journal remains cleared after the failed disposal");
    if (!o.discard) {
      const q = fs.readdirSync(holdingRoot());
      assert.equal(q.length, 1, "!discard retains the COMPLETE quarantine copy despite the disposal failure");
      assert.equal(fs.readFileSync(path.join(holdingRoot(), q[0]!, "MARK.txt"), "utf8"), "owner-only bytes\n");
    }

    // The negative, broadened: the subsequent claim throws NONE of the capture-guard errors.
    const reseeded = await git
      .createOrAttachRunnerClone(bare, o.iid, noProofReseed, o.claimantRunId)
      .catch((err: unknown) => assert.fail(`the subsequent claim wedged: ${(err as Error).name}: ${(err as Error).message}`));
    assert.equal(reseeded.path, clonePath, "the reseed lands cleanly at the now-free canonical");
    assert.equal(fs.existsSync(clonePath), true, "the reseed recreated the canonical clone");
  }

  let caseN = 0;
  for (const discard of [true, false]) {
    for (const claimant of ["same-run", "different-run"] as const) {
      caseN++;
      const n = caseN;
      it(`T1354-c (discard=${discard}, ${claimant}): a partial scratch-disposal is survivable — no capture-guard wedge`, async () => {
        const iid = 1471 + n; // 1472..1475
        const ownerRunId = `d1354c${n}0-0000-4000-8000-000000000003`;
        const claimantRunId = claimant === "same-run" ? ownerRunId : `d1354c${n}1-0000-4000-8000-000000000004`;
        await partialDisposalNoWedge({ iid, ownerRunId, discard, claimantRunId });
      });
    }
  }

  for (const discard of [true, false]) {
    it(`T1354-d (discard=${discard}): a journal-clear failure AFTER the rename leaves no capture-guard wedge`, async () => {
      const iid = discard ? 1476 : 1477;
      const ownerRunId = `d1354d${discard ? "1" : "0"}0-0000-4000-8000-000000000006`;
      const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "MARK.txt");

      const restoreRename = stubDockerLaneRename();
      type RunGit = (cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string) => Promise<string>;
      const gitAny = git as unknown as { runGit: RunGit };
      const origRunGit = gitAny.runGit.bind(git);
      gitAny.runGit = (async (cwd, args, pat, scope, username) => {
        // The journal CLEAR is the only config write with an empty value; the --list reads
        // and markRecoveryCapture (a JSON value) pass through untouched.
        if (args[0] === "config" && args[3] === "") throw new Error("injected journal-clear failure");
        return origRunGit(cwd, args, pat, scope, username);
      }) as RunGit;

      try {
        await assert.rejects(
          git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard }),
          /injected journal-clear failure/,
          "the clear failure surfaces (the rename already freed the canonical)",
        );
      } finally {
        restoreRename();
        gitAny.runGit = origRunGit;
      }

      assert.equal(fs.existsSync(clonePath), false, "the canonical is gone: the rename freed it BEFORE the clear failed");
      assert.deepEqual(
        readJournal(bare, branch),
        { runId: ownerRunId, clonePath },
        "the journal is left STALE by the failed clear, still naming the now-gone canonical",
      );
      if (!discard) {
        const q = fs.readdirSync(holdingRoot());
        assert.equal(q.length, 1, "the already-complete PVC copy survives the clear failure (copied before the rename)");
        assert.equal(fs.readFileSync(path.join(holdingRoot(), q[0]!, "MARK.txt"), "utf8"), "owner-only bytes\n");
      }
      // The stale journal does NOT wedge the next claim: the guard's lstat→ENOENT (canonical
      // gone via the rename) lets it reseed despite the uncleared journal.
      const reseeded = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, ownerRunId);
      assert.equal(reseeded.path, clonePath);
      assert.equal(fs.existsSync(clonePath), true);
    });
  }

  it("T1354-e (!discard EXDEV): a copy failure removes the incomplete quarantine; canonical + journal intact", async () => {
    const iid = 1478;
    const ownerRunId = "d1354005-0000-4000-8000-000000000005";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    const restoreRename = stubDockerLaneRename();
    const origCp = fsp.cp.bind(fsp);
    (fsp as { cp: typeof fsp.cp }).cp = (async () => {
      throw Object.assign(new Error("injected copy failure"), { code: "EIO" });
    }) as typeof fsp.cp;
    try {
      await assert.rejects(
        git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false }),
        /injected copy failure/,
      );
    } finally {
      restoreRename();
      (fsp as { cp: typeof fsp.cp }).cp = origCp;
    }
    assert.equal(fs.existsSync(clonePath), true, "the canonical is intact (copy-before-free never freed it)");
    assert.equal(fs.readFileSync(path.join(clonePath, "KEEP.txt"), "utf8"), "owner-only bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is intact");
    const q = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
    assert.equal(q.length, 0, "the incomplete quarantine copy was removed on failure");
    assert.equal(scratchDirs().length, 0, "no scratch parent is created when the copy fails first");
  });

  it("T1354-f (!discard EXDEV): a post-copy scratch-rename failure RETAINS the completed quarantine; canonical + journal intact", async () => {
    const iid = 1479;
    const ownerRunId = "d1354006-0000-4000-8000-000000000006";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    // ONLY the intra-device scratch rename throws a real (non-ENOENT, source-present) error;
    // the copy into holdingDest completes first via the real fs.cp.
    const restoreRename = stubDockerLaneRename(async () => {
      throw Object.assign(new Error("injected scratch rename failure"), { code: "EACCES" });
    });
    try {
      await assert.rejects(
        git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false }),
        /injected scratch rename failure/,
      );
    } finally {
      restoreRename();
    }
    assert.equal(fs.existsSync(clonePath), true, "the canonical is intact (the scratch rename failed before freeing it)");
    assert.equal(fs.readFileSync(path.join(clonePath, "KEEP.txt"), "utf8"), "owner-only bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is intact");
    const q = fs.readdirSync(holdingRoot());
    assert.equal(q.length, 1, "the completed PVC copy is RETAINED, never deleted on a rename failure");
    assert.equal(fs.readFileSync(path.join(holdingRoot(), q[0]!, "KEEP.txt"), "utf8"), "owner-only bytes\n");
  });

  it("T1354-g: the scratch parent is created EXCLUSIVELY (non-recursive, mode 0700); a pre-planted path fails closed", async () => {
    const iid = 1480;
    const ownerRunId = "d1354007-0000-4000-8000-000000000007";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    const restoreRename = stubDockerLaneRename();
    const origMkdir = fsp.mkdir.bind(fsp);
    let scratchMode: number | undefined;
    let scratchRecursive: boolean | undefined;
    (fsp as { mkdir: typeof fsp.mkdir }).mkdir = (async (p: fs.PathLike, opts?: Parameters<typeof origMkdir>[1]) => {
      if (String(p).startsWith(path.join(runnerRoot(), ".retire-"))) {
        const o = (typeof opts === "object" && opts !== null ? opts : {}) as { mode?: number; recursive?: boolean };
        scratchMode = o.mode;
        scratchRecursive = o.recursive;
        // Model a pre-planted path / type-surprise: an exclusive (non-recursive) mkdir
        // throws EEXIST, so the retire fails closed rather than reusing a runner-planted dir.
        throw Object.assign(new Error("EEXIST: scratch parent already exists"), { code: "EEXIST" });
      }
      return origMkdir(p, opts);
    }) as typeof fsp.mkdir;
    try {
      await assert.rejects(
        git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true }),
        /EEXIST/,
        "a pre-planted scratch parent makes the retire fail closed",
      );
    } finally {
      restoreRename();
      (fsp as { mkdir: typeof fsp.mkdir }).mkdir = origMkdir;
    }
    assert.equal(scratchMode, 0o700, "the scratch parent is created with mode 0700");
    assert.notEqual(scratchRecursive, true, "the scratch mkdir is NON-recursive (exclusive create — fails closed on EEXIST)");
    assert.equal(fs.existsSync(clonePath), true, "the canonical is intact after the fail-closed scratch mkdir");
    assert.equal(fs.readFileSync(path.join(clonePath, "KEEP.txt"), "utf8"), "owner-only bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is intact");
  });
});

describe("1848 M2 local retirement disposition and stage diagnostics", () => {
  const owner = "12345678-1234-4234-8234-123456789abc";
  function diagnostics(logger: Logger) {
    return { logger, claimantId: "hostile\n" + "x".repeat(2000), ownerId: owner,
      repoId: "glpat-" + "0123456789abcdefghij", pathShape: "canonical" as const };
  }

  for (const stage of ["journal_read", "journal_validation", "containment", "holding_parent", "rename", "retained_copy", "intra_device_rename", "journal_clear"] as const) {
    it(`1848 M2 retirement failure at ${stage} retains original exception and custody`, async (t) => {
      const seeded = await seedResidue(1890, owner, "M2.txt");
      let clonePath = seeded.clonePath;
      if (stage === "containment") {
        clonePath = path.join(fx.dataDir, "outside-runner");
        fs.mkdirSync(clonePath);
        fs.writeFileSync(path.join(clonePath, "M2.txt"), "owner-only bytes\n");
        await git.markRecoveryCapture(seeded.bare, clonePath, seeded.branch, owner);
      }
      const logs = diagnosticLogger();
      const fault = Object.assign(new Error("/private/path\n" + "glpat-" + "0123456789abcdefghij"), { code: stage === "rename" ? "ENOENT" : "EIO" });
      const readSeam = git as unknown as { readRecoveryCapture: (bare: string, branch: string) => Promise<{ runId: string; clonePath: string } | undefined> };
      const realRead = readSeam.readRecoveryCapture.bind(git);
      let reads = 0;
      if (stage === "journal_read" || stage === "journal_validation") {
        t.mock.method(readSeam, "readRecoveryCapture", async (...args: Parameters<typeof realRead>) => {
          if (++reads === 1) {
            if (stage === "journal_read") throw fault;
            return undefined;
          }
          return realRead(...args);
        });
      }
      const realMkdir = fsp.mkdir.bind(fsp);
      if (stage === "holding_parent") t.mock.method(fsp, "mkdir", async (...args: Parameters<typeof realMkdir>) => {
        if (String(args[0]) === holdingRoot()) throw fault;
        return realMkdir(...args);
      });
      const realRename = fsp.rename.bind(fsp);
      if (["rename", "retained_copy", "intra_device_rename"].includes(stage)) {
        t.mock.method(fsp, "rename", async (...args: Parameters<typeof realRename>) => {
          if (String(args[0]) === clonePath) {
            if (stage === "rename" || String(args[1]).startsWith(runnerRoot() + path.sep)) throw fault;
            throw Object.assign(new Error("cross-device"), { code: "EXDEV" });
          }
          return realRename(...args);
        });
      }
      if (stage === "retained_copy") t.mock.method(fsp, "cp", async () => { throw fault; });
      type RunGit = (cwd: string | undefined, args: string[]) => Promise<string>;
      const seam = git as unknown as { runGit: RunGit };
      const realRunGit = seam.runGit.bind(git);
      if (stage === "journal_clear") t.mock.method(seam, "runGit", async (cwd: string | undefined, args: string[]) => {
        if (args[0] === "config" && args.at(-1) === "") throw fault;
        return realRunGit(cwd, args);
      });
      let caught: unknown;
      try { await git.retireRunnerClone(seeded.bare, clonePath, seeded.branch, owner, { discard: false, orphanDiagnostics: diagnostics(logs.logger) }); }
      catch (error) { caught = error; }
      assert.ok(caught);
      if (stage === "journal_validation" || stage === "containment") assert.ok(caught instanceof CapturePathMismatchError);
      else assert.equal(caught, fault);
      assertSafeEvents(logs.events, [clonePath, fault.message]);
      assert.equal(logs.events.length, 1);
      assert.equal(logs.events[0]?.stage, stage);
      assert.equal(logs.events[0]?.claimant_id, "invalid");
      assert.equal(logs.events[0]?.repo_id, "invalid");
      assert.equal(logs.events[0]?.owner_id, owner);
      assert.deepEqual(readJournal(seeded.bare, seeded.branch), { runId: owner, clonePath });
      if (stage === "journal_clear") assert.equal(fs.existsSync(clonePath), false);
      else assert.equal(fs.readFileSync(path.join(clonePath, "M2.txt"), "utf8"), "owner-only bytes\n");
      const priorHeld = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()).map((name) => path.join(holdingRoot(), name)) : [];
      if (stage === "intra_device_rename" || stage === "journal_clear") {
        assert.equal(priorHeld.length, 1);
        assert.equal(fs.readFileSync(path.join(priorHeld[0]!, "M2.txt"), "utf8"), "owner-only bytes\n");
      }
      t.mock.restoreAll();
      if (stage === "containment") {
        // The refusal above left both fixtures untouched; restore canonical custody
        // before retrying a path that retirement is permitted to move.
        assert.equal(fs.readFileSync(path.join(seeded.clonePath, "M2.txt"), "utf8"), "owner-only bytes\n");
        await git.markRecoveryCapture(seeded.bare, seeded.clonePath, seeded.branch, owner);
        clonePath = seeded.clonePath;
      }
      assert.equal(await git.retireRunnerClone(seeded.bare, clonePath, seeded.branch, owner, {
        discard: false, orphanDiagnostics: diagnostics(logs.logger),
      }), stage === "journal_clear" ? "source-already-absent" : "quarantined");
      assert.equal(readJournal(seeded.bare, seeded.branch), undefined);
      assert.equal(fs.existsSync(clonePath), false);
      const heldAfter = fs.readdirSync(holdingRoot()).map((name) => path.join(holdingRoot(), name));
      assert.ok(heldAfter.length > 0);
      for (const held of heldAfter) {
        assert.equal(fs.readFileSync(path.join(held, "M2.txt"), "utf8"), "owner-only bytes\n");
      }
      for (const held of priorHeld) assert.ok(heldAfter.includes(held), "retry retains prior complete holding bytes");
      if (stage === "journal_clear") assert.deepEqual(heldAfter, priorHeld);
      if (stage === "containment") {
        assert.equal(fs.readFileSync(path.join(fx.dataDir, "outside-runner", "M2.txt"), "utf8"), "owner-only bytes\n");
      }
    });
  }

  for (const crossDevice of [false, true]) {
    it(`1848 M2 completed retirement returns quarantined, cross-device=${crossDevice}`, async (t) => {
      const { bare, branch, clonePath } = await seedResidue(1891, owner, "M2.txt");
      const realRename = fsp.rename.bind(fsp);
      if (crossDevice) t.mock.method(fsp, "rename", async (...args: Parameters<typeof realRename>) => {
        if (String(args[1]).startsWith(holdingRoot())) throw Object.assign(new Error("cross-device"), { code: "EXDEV" });
        return realRename(...args);
      });
      assert.equal(await git.retireRunnerClone(bare, clonePath, branch, owner, { discard: false,
        orphanDiagnostics: diagnostics({ ...nullLogger(), info: () => { throw new Error("logger failed"); } }) }), "quarantined");
      assert.equal(fs.existsSync(clonePath), false);
      assert.equal(readJournal(bare, branch), undefined);
      const held = fs.readdirSync(holdingRoot());
      assert.equal(fs.readFileSync(path.join(holdingRoot(), held[0]!, "M2.txt"), "utf8"), "owner-only bytes\n");
    });
  }

  it("1848 M2 retirement never clears a successor journal written after the rename", async (t) => {
    const { bare, branch, clonePath } = await seedResidue(1894, owner, "M2.txt");
    const successor = randomUUID();
    const realRename = fsp.rename.bind(fsp);
    t.mock.method(fsp, "rename", async (...args: Parameters<typeof realRename>) => {
      await realRename(...args);
      if (String(args[0]) === clonePath) execFileSync("git", ["-C", bare, "config", "--local", `uzi-recovery.${branch}.clone`,
        JSON.stringify({ runId: successor, clonePath })], { env: GIT_ENV, stdio: "pipe" });
    });
    assert.equal(await git.retireRunnerClone(bare, clonePath, branch, owner, { discard: false }), "quarantined");
    assert.deepEqual(readJournal(bare, branch), { runId: successor, clonePath });
    const held = fs.readdirSync(holdingRoot());
    assert.equal(fs.readFileSync(path.join(holdingRoot(), held[0]!, "M2.txt"), "utf8"), "owner-only bytes\n");
  });

  it("1848 M2 confirmed source absence returns source-already-absent", async () => {
    const { bare, branch, clonePath } = await seedResidue(1892, owner, "M2.txt");
    fs.rmSync(clonePath, { recursive: true });
    assert.equal(await git.retireRunnerClone(bare, clonePath, branch, owner, { discard: false }), "source-already-absent");
    assert.equal(readJournal(bare, branch), undefined);
  });

  it("1848 M2 fingerprint and logger failures cannot replace the retirement exception", async (t) => {
    const { bare, branch, clonePath } = await seedResidue(1893, owner, "M2.txt");
    const fault = Object.assign(new Error("/private/path"), { code: "/private/secret-errno" });
    t.mock.method(fsp, "rename", async () => { throw fault; });
    const realHashUpdate = Hash.prototype.update;
    t.mock.method(Hash.prototype, "update", function(this: Hash, ...args: Parameters<Hash["update"]>) {
      if (args[0] === clonePath) throw new Error("hash failed");
      return Reflect.apply(realHashUpdate, this, args) as Hash;
    });
    const logs = diagnosticLogger();
    await assert.rejects(git.retireRunnerClone(bare, clonePath, branch, owner, { discard: false,
      orphanDiagnostics: { ...diagnostics(logs.logger), ownerId: "hostile\n" + "x".repeat(2000) } }), (error) => error === fault);
    assert.equal(logs.events[0]?.errno, "unknown");
    assert.equal(logs.events[0]?.owner_id, "invalid");
    assertSafeEvents(logs.events, [clonePath, "/private/secret-errno"]);
    assert.equal(logs.events[0]?.path_fingerprint, "0".repeat(64));
    await assert.rejects(git.retireRunnerClone(bare, clonePath, branch, owner, { discard: false,
      orphanDiagnostics: diagnostics({ ...nullLogger(), warn: () => { throw new Error("logger failed"); } }) }), (error) => error === fault);
  });
});
