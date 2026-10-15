import { after, afterEach, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import {
  CapturePathMismatchError,
  CloneResidueBlockedError,
  ForeignCaptureBlockedError,
  GitCache,
  PendingRecoveryCaptureError,
  type AttemptSeedOptions,
  type CanonicalReseedOptions,
  type GitCacheOptions,
} from "../src/git.js";
import { failOriginForReason, RunRunner, StaleClaimError, type ExecutorFactory } from "../src/runner.js";
import { PauseNowSignal } from "../src/steering.js";
import { formatResidueName, parseRetainedArtifactName } from "../src/attempt-path.js";
import {
  LiveAttemptRegistry,
  mintAttemptId,
  newRunAttempt,
  quiesceRunAttempt,
  setQuiescenceViewForTests,
  type QuiesceRunOutcome,
  type QuiesceRunRequest,
} from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { makeFakeProcRoot, plantInScope, plantUnreadableUnattributed, scopedRealView, withQuiescenceView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { realProcfsSkip } from "./real-procfs.js";
import { latchResidueQuarantine, residueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// issue #1783 M3 — the CANONICAL reseed (an unwired worker's `<runnerRoot>/<repoDir>/<key>`) frees
// the canonical path only after a seed-mode process proof scoped to it, and quarantines what the
// delete cannot remove (a root-owned or read-only tree, e.g. `EACCES … rmdir '…/agent/src'`) by a
// SAME-PARENT rename to `.uzi-residue-<key>.residue-<uuid>`, which is kept. Anything that blocks
// fails the run typed `worker_residue_blocked`, with nothing moved. These tests run non-root.

const HAS_PROCFS = process.platform === "linux";
/** The skip for a case whose proof scans the REAL process table: none, "Linux only", or (a sandbox
 *  that denies enumerating the proc root, issue #1863) the shared detector's recorded reason. */
const realTableSkip = (label: string): string | false =>
  !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip(`clone-residue: ${label}`);

/** Directories made read-only by a test, restored (u+w) before the harness removes the fixture. */
const readOnly: string[] = [];
const orphans: number[] = [];

// The real reaper sees this file's own descendants plus its orphans (reparented away, so recorded
// in a pidfile), never the host's other processes.
const orphanPidDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-orphan-pids-"));
const orphanPidFile = path.join(orphanPidDir, "orphans.pids");
before(() => setQuiescenceViewForTests(scopedRealView({ pidFiles: [orphanPidFile] })));
after(() => {
  restoreHermeticView();
  fs.rmSync(orphanPidDir, { recursive: true, force: true });
});

// Registered BEFORE installHarness so it runs before the harness removes the fixture dir.
afterEach(() => {
  for (const d of readOnly.splice(0)) {
    try {
      fs.chmodSync(d, 0o755);
    } catch {
      /* gone */
    }
  }
  for (const pid of orphans.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      /* gone */
    }
  }
});

installHarness();

function chmodReadOnly(dir: string): void {
  fs.chmodSync(dir, 0o555);
  readOnly.push(dir);
}

function bare(): string {
  return git.barePathFor(fx.originPath);
}

function runnerRepoDir(): string {
  return path.join(fx.dataDir, "runner", path.basename(bare()).replace(/\.git$/, ""));
}

function canonicalFor(iid: number): string {
  return path.join(runnerRepoDir(), `issue-${iid}`);
}

/** The incident's shape, self-owned: a canonical clone path holding `agent/` at 0555 with
 *  `agent/src` (and a file) inside, so a recursive delete fails `EACCES … rmdir '…/agent/src'`. */
function plantReadOnlyTree(canonical: string): void {
  fs.mkdirSync(path.join(canonical, "agent", "src"), { recursive: true });
  fs.writeFileSync(path.join(canonical, "agent", "src", "gate-log.loop"), "residue\n");
  fs.writeFileSync(path.join(canonical, "STALE.txt"), "a stale clone file\n");
  chmodReadOnly(path.join(canonical, "agent"));
}

/** Every `.uzi-residue-*` entry of the runner repo dir. */
function residueNames(): string[] {
  try {
    return fs.readdirSync(runnerRepoDir()).filter((n) => n.startsWith(".uzi-residue-"));
  } catch {
    return [];
  }
}

/** A content hash of a whole tree (paths + bytes + types + modes), for an unchanged-tree assertion. */
function treeHash(root: string): string {
  const h = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const p = path.join(dir, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(root, p)}\0${st.isDirectory() ? "d" : st.isSymbolicLink() ? "l" : "f"}\0${st.mode}\0`);
      if (st.isDirectory()) walk(p);
      else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(root);
  return h.digest("hex");
}

/** An executor that records the clone it was handed, then stops the run. */
function transientFactory(onRun: (ctx: RunContext) => void = () => {}): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        started++;
        onRun(ctx);
        throw new Error("stop after the seed");
      },
    },
  });
  return { factory, started: () => started };
}

const quiescentOutcome = (): QuiesceRunOutcome => ({
  process: { state: "quiescent", processes: [], killed: [], detail: "scripted quiescent" },
  docker: { state: "not_wired", removed: [], detail: "" },
});

/** A recorder that answers every proof quiescent (the behaviour under test is the delete/quarantine,
 *  not the host's process table). */
function recordedQuiescent() {
  const calls: QuiesceRunRequest[] = [];
  return {
    calls,
    quiesceRun: async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      calls.push(req);
      return quiescentOutcome();
    },
  };
}

/** An unwired runner (no dockerHost) over the harness GitCache, or over `gitCache` when given. */
function unwired(factory: ExecutorFactory, extra: Parameters<typeof runnerWith>[4] = {}, gitCache?: GitCache) {
  if (!gitCache) return runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 5, ...extra });
  return new RunRunner(client, gitCache, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab: fakeGitlab().gitlab,
    recoveryRetryMs: 5,
    ...extra,
  });
}

function lastFailed(runId: string) {
  return api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
}

/** An env-scrubbed process whose cwd is `cwd`, orphaned (double fork) so it is not a descendant of
 *  the scanning process. */
function orphanIn(cwd: string, extraEnv: Record<string, string> = {}): number {
  const out = execFileSync("/bin/sh", ["-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!"], {
    cwd,
    env: { PATH: process.env.PATH ?? "/usr/bin:/bin", ...extraEnv },
    encoding: "utf8",
  });
  const pid = Number(out.trim());
  orphans.push(pid);
  fs.appendFileSync(orphanPidFile, `${pid}\n`);
  return pid;
}

/** The fixture is alive while it can execute work. A killed orphan may remain a zombie until
 *  its adopting parent collects it; PID removal is not part of the residue proof. */
function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(fs.readFileSync(`/proc/${pid}/status`, "utf8"));
  } catch {
    return false;
  }
}

it("issue #2104: an unreaped zombie is dead even while signal-0 succeeds", { skip: realTableSkip("held zombie") }, async () => {
  const pidFile = path.join(orphanPidDir, "held-zombie.pid");
  // First let the shell exec a parent that never waits for its child. Only then kill
  // the child, so the shell cannot reap it before exec and erase the regression seam.
  const parent = spawn("/bin/sh", ["-c", 'sleep 30 & echo $! > "$1"; exec sleep 30', "held-zombie", pidFile], { stdio: "ignore" });
  let spawnError: Error | undefined;
  parent.once("error", (err) => { spawnError = err; });
  const closed = new Promise<void>((resolve) => parent.once("close", () => resolve()));
  let pid: number | undefined;
  async function until(check: () => boolean, message: string): Promise<void> {
    const deadline = Date.now() + 5000;
    while (!check()) {
      if (spawnError) throw spawnError;
      assert.ok(Date.now() < deadline, message);
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  try {
    await until(() => {
      if (!fs.existsSync(pidFile) || !parent.pid) return false;
      const candidate = Number(fs.readFileSync(pidFile, "utf8").trim());
      if (!Number.isInteger(candidate) || candidate <= 0) return false;
      pid = candidate;
      return /^Name:\s*sleep$/m.test(fs.readFileSync(`/proc/${parent.pid}/status`, "utf8"));
    }, "the holding parent execs sleep and publishes its child PID");
    assert.equal(alive(parent.pid!), true, "the holding parent is still live");
    process.kill(pid!, "SIGKILL");
    await until(() => /^State:\s*Z/m.test(fs.readFileSync(`/proc/${pid}/status`, "utf8")), "the killed child remains an unreaped zombie");
    assert.doesNotThrow(() => process.kill(pid!, 0), "signal-0 still finds the zombie PID");
    assert.equal(alive(pid!), false, "the zombie cannot execute work");
  } finally {
    if (pid !== undefined) { try { process.kill(pid, "SIGKILL"); } catch { /* already gone */ } }
    if (parent.pid) { try { process.kill(parent.pid, "SIGKILL"); } catch { /* already gone */ } }
    await closed;
    fs.rmSync(pidFile, { force: true });
  }
});

// ─── the typed failure ─────────────────────────────────────────────────────────────────────

describe("issue #1783 M3: CloneResidueBlockedError is typed worker_residue_blocked", () => {
  it("maps through failOriginForReason, never the generic agent_failure", () => {
    const err = new CloneResidueBlockedError("quarantine rename failed: EACCES");
    assert.equal(failOriginForReason(err.message), "worker_residue_blocked");
    assert.equal(err.name, "CloneResidueBlockedError");
  });
});

// ─── the self-owned residue (the incident, non-root) ───────────────────────────────────────

describe("issue #1783 M3: a canonical reseed quarantines what it cannot delete", () => {
  it("a 0555 agent/ with agent/src: the reseed succeeds, the residue is kept in the same parent under the pinned name, and the log names it", async () => {
    const iid = 3001;
    const canonical = canonicalFor(iid);
    plantReadOnlyTree(canonical);
    const { logger, lines } = recordingLogger();
    const recordingGit = new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    let worktree = "";
    let seededHasGit = false;
    const { factory, started } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
      seededHasGit = fs.existsSync(path.join(ctx.worktreePath, ".git"));
    });
    const { calls, quiesceRun } = recordedQuiescent();
    const claim = gitlabClaim(iid);
    await unwired(factory, { quiesceRun }, recordingGit).execute(claim);

    assert.equal(started(), 1, "the model started: the reseed succeeded");
    assert.equal(worktree, canonical, "an unwired worker reseeds the canonical path");
    assert.equal(seededHasGit, true, "a fresh clone was seeded at the canonical path");
    const names = residueNames();
    assert.equal(names.length, 1, `one residue entry: ${names.join(",")}`);
    const residue = path.join(runnerRepoDir(), names[0]!);
    const art = parseRetainedArtifactName(names[0]!);
    assert.ok(art?.kind === "residue", "the pinned residue grammar");
    assert.equal(art.key, `issue-${iid}`);
    assert.equal(names[0], formatResidueName(`issue-${iid}`, art.uuid), "the pinned grammar round-trips");
    assert.equal(path.dirname(residue), path.dirname(canonical), "quarantined in the SAME parent");
    assert.equal(fs.readFileSync(path.join(residue, "agent", "src", "gate-log.loop"), "utf8"), "residue\n", "what the delete could not remove is kept");
    readOnly.push(path.join(residue, "agent"));
    // The proof ran first, in seed mode, scoped to the canonical path alone.
    const reseedProof = calls.find((c) => c.site === "canonical_reseed");
    assert.ok(reseedProof, "a canonical_reseed proof ran");
    assert.equal(reseedProof.mode, "seed");
    assert.deepEqual(reseedProof.targetPaths, [canonical]);
    const warn = lines.find((l) => (l as { msg?: string }).msg?.includes("quarantined it as residue"));
    assert.ok(warn, "the quarantine is logged");
    assert.equal((warn as { residue?: string }).residue, residue, "the log names the residue path");
    assert.equal((warn as { canonical?: string }).canonical, canonical);
    // The failed run's terminal retire removed its own clone; the residue is untouched.
    assert.equal(lastFailed(claim.run_id)?.fail_origin, undefined, "the stop is an ordinary agent failure, not a residue block");
    assert.equal(fs.existsSync(residue), true, "the residue is never deleted right after quarantine");
  });

  it("a canonical path the delete CAN remove is removed, no residue is made", async () => {
    const iid = 3002;
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    fs.writeFileSync(path.join(canonical, "old", "f.txt"), "x");
    let worktree = "";
    const { factory } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
    });
    const { calls, quiesceRun } = recordedQuiescent();
    await unwired(factory, { quiesceRun }).execute(gitlabClaim(iid));
    assert.equal(worktree, canonical);
    assert.deepEqual(residueNames(), []);
    assert.equal(calls.filter((c) => c.site === "canonical_reseed").length, 1);
  });

  it("no canonical path on disk: nothing to free, no proof", async () => {
    const iid = 3003;
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recordedQuiescent();
    await unwired(factory, { quiesceRun }).execute(gitlabClaim(iid));
    assert.equal(started(), 1);
    assert.equal(calls.filter((c) => c.site === "canonical_reseed").length, 0);
  });
});

// ─── the journal cases run first ───────────────────────────────────────────────────────────

describe("issue #1783 M3: journal classification runs before any free; a journal case leaves the residue untouched", () => {
  const branch = (iid: number) => `agent/issue-${iid}`;

  function recordingReseed(): { reseed: CanonicalReseedOptions; calls: string[] } {
    const calls: string[] = [];
    return { calls, reseed: { ...noProofReseed, beforeFree: async (p) => void calls.push(p) } };
  }

  async function primedCanonical(iid: number): Promise<{ b: string; canonical: string; hash: string }> {
    const b = await git.ensureClone(fx.originPath);
    const canonical = canonicalFor(iid);
    plantReadOnlyTree(canonical);
    return { b, canonical, hash: treeHash(canonical) };
  }

  function assertUntouched(canonical: string, hash: string, calls: string[]): void {
    assert.deepEqual(calls, [], "the free's proof never ran");
    assert.equal(treeHash(canonical), hash, "the canonical tree is unchanged");
    assert.deepEqual(residueNames(), [], "nothing was quarantined");
  }

  for (const attemptMode of [false, true]) {
    for (const shape of ["matching foreign", "path mismatch", "attempt-id mismatch"] as const) {
      it(`missing-source ${attemptMode ? "attempt" : "canonical"} ${shape} classification`, async () => {
        const iid = 3190;
        const owner = randomUUID();
        const claimant = randomUUID();
        const { b, canonical } = await primedCanonical(iid);
        const id = mintAttemptId(1);
        const journaledPath = shape === "matching foreign" ? canonical
          : shape === "path mismatch" ? path.join(runnerRepoDir(), `agent-issue-${iid}`)
          : `${canonical}.attempt-${id}`;
        if (journaledPath !== canonical) {
          fs.mkdirSync(journaledPath);
          fs.writeFileSync(path.join(journaledPath, "EVIDENCE.txt"), "retained owner bytes\n");
        }
        const journalId = shape === "attempt-id mismatch" ? mintAttemptId(2) : undefined;
        const command = (...args: string[]) => execFileSync("git", ["-C", b, ...args], { encoding: "utf8" }).trim();
        await git.markRecoveryCapture(b, journaledPath, branch(iid), owner, shape === "attempt-id mismatch" ? id : undefined);
        if (journalId) {
          // Legacy journals can carry inconsistent ids; the guarded writer rejects them.
          command("config", `uzi-recovery.${branch(iid)}.clone`, JSON.stringify({ runId: owner, clonePath: journaledPath, attemptId: journalId }));
        }
        const pending = command("config", `uzi-recovery.${branch(iid)}.clone`);
        command("config", `uzi-retained.${owner}.journal`,
          JSON.stringify({ version: 1, branch: branch(iid), key: `issue-${iid}`, journal: { ...JSON.parse(pending), ...(journalId ? { attemptId: id } : {}) } }));
        const tip = command("rev-parse", "refs/remotes/origin/main");
        command("update-ref", `refs/uzi-recovery-episode/${owner}/${tip}`, tip);
        fs.renameSync(journaledPath, journaledPath + ".saved");
        if (journaledPath === canonical) readOnly.push(path.join(journaledPath + ".saved", "agent"));
        const savedHash = treeHash(journaledPath + ".saved");
        const snapshot = () => [
          command("config", "--get-regexp", "^uzi-"),
          command("for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi"),
        ];
        const before = snapshot();
        const pathsBefore = fs.readdirSync(runnerRepoDir()).sort();
        const { reseed, calls } = recordingReseed();
        const seeds: string[][] = [];
        const opts: AttemptSeedOptions = {
          attemptId: mintAttemptId(3), isLive: () => false,
          beforeSeed: async p => void seeds.push(p), quiescent: async () => { calls.push("sweep"); return true; },
        };
        await assert.rejects(
          git.createOrAttachRunnerClone(b, iid, reseed, claimant, false, undefined, attemptMode ? opts : undefined),
          (error: unknown) => {
            if (shape === "matching foreign") {
              assert.ok(error instanceof ForeignCaptureBlockedError);
              assert.equal(error.clonePath, journaledPath);
            } else {
              assert.ok(error instanceof CapturePathMismatchError);
              assert.equal(error.journaledPath, journaledPath);
              assert.equal(error.computedPath, canonical);
            }
            assert.equal(error.branch, branch(iid));
            assert.equal(error.ownerRunId, owner);
            return true;
          },
        );
        assert.deepEqual(calls, []);
        assert.deepEqual(seeds, []);
        assert.deepEqual(snapshot(), before, "journal, protected descriptor and pins unchanged");
        assert.deepEqual(fs.readdirSync(runnerRepoDir()).sort(), pathsBefore, "no destination created");
        assert.equal(treeHash(journaledPath + ".saved"), savedHash);
        assert.equal(fs.existsSync(journaledPath), false);
      });
    }
  }

  it("Case C (this run's own journal): PendingRecoveryCaptureError, nothing freed", async () => {
    const iid = 3101;
    const runId = randomUUID();
    const { b, canonical, hash } = await primedCanonical(iid);
    await git.markRecoveryCapture(b, canonical, branch(iid), runId);
    const { reseed, calls } = recordingReseed();
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, runId), PendingRecoveryCaptureError);
    assertUntouched(canonical, hash, calls);
  });

  it("Case B (another run's journal on the canonical path): ForeignCaptureBlockedError, nothing freed", async () => {
    const iid = 3102;
    const { b, canonical, hash } = await primedCanonical(iid);
    await git.markRecoveryCapture(b, canonical, branch(iid), randomUUID());
    const { reseed, calls } = recordingReseed();
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, randomUUID()), ForeignCaptureBlockedError);
    assertUntouched(canonical, hash, calls);
  });

  it("Case A (the journal names another path that exists): CapturePathMismatchError, nothing freed", async () => {
    const iid = 3103;
    const { b, canonical, hash } = await primedCanonical(iid);
    const other = path.join(runnerRepoDir(), `agent-issue-${iid}`);
    fs.mkdirSync(other, { recursive: true });
    await git.markRecoveryCapture(b, other, branch(iid), randomUUID());
    const { reseed, calls } = recordingReseed();
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, randomUUID()), CapturePathMismatchError);
    assertUntouched(canonical, hash, calls);
  });

  describe("the primed variants on a wired worker (A′/B′/C′) never free the canonical path", () => {
    function attemptOpts(): { opts: AttemptSeedOptions; seeds: string[][] } {
      const seeds: string[][] = [];
      return {
        seeds,
        opts: { attemptId: mintAttemptId(1), isLive: () => false, beforeSeed: async (p) => void seeds.push(p), quiescent: async () => true },
      };
    }

    it("C′ (own journal on the legacy canonical path): PendingRecoveryCaptureError, canonical untouched", async () => {
      const iid = 3111;
      const runId = randomUUID();
      const { b, canonical, hash } = await primedCanonical(iid);
      await git.markRecoveryCapture(b, canonical, branch(iid), runId);
      const { reseed, calls } = recordingReseed();
      const { opts, seeds } = attemptOpts();
      await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, runId, false, undefined, opts), PendingRecoveryCaptureError);
      assert.deepEqual(seeds, [], "the attempt sweep never ran");
      assertUntouched(canonical, hash, calls);
    });

    it("B′ (a foreign journal on the legacy canonical path): ForeignCaptureBlockedError, canonical untouched", async () => {
      const iid = 3112;
      const { b, canonical, hash } = await primedCanonical(iid);
      await git.markRecoveryCapture(b, canonical, branch(iid), randomUUID());
      const { reseed, calls } = recordingReseed();
      const { opts, seeds } = attemptOpts();
      await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, randomUUID(), false, undefined, opts), ForeignCaptureBlockedError);
      assert.deepEqual(seeds, []);
      assertUntouched(canonical, hash, calls);
    });

    it("A′ (a journal naming another key): CapturePathMismatchError, canonical untouched", async () => {
      const iid = 3113;
      const { b, canonical, hash } = await primedCanonical(iid);
      const other = path.join(runnerRepoDir(), `agent-issue-${iid}`);
      fs.mkdirSync(other, { recursive: true });
      await git.markRecoveryCapture(b, other, branch(iid), randomUUID());
      const { reseed, calls } = recordingReseed();
      const { opts, seeds } = attemptOpts();
      await assert.rejects(git.createOrAttachRunnerClone(b, iid, reseed, randomUUID(), false, undefined, opts), CapturePathMismatchError);
      assert.deepEqual(seeds, []);
      assertUntouched(canonical, hash, calls);
    });

    it("no journal: the wired seed goes to a fresh attempt path and never frees or quarantines the canonical one", async () => {
      const iid = 3114;
      const { b, canonical, hash } = await primedCanonical(iid);
      const { reseed, calls } = recordingReseed();
      const { opts } = attemptOpts();
      const clone = await git.createOrAttachRunnerClone(b, iid, reseed, randomUUID(), false, undefined, opts);
      assert.notEqual(clone.path, canonical);
      assertUntouched(canonical, hash, calls);
    });
  });
});

// ─── refusals: nothing moves ───────────────────────────────────────────────────────────────

describe("issue #1783 M3: a blocked canonical free moves nothing and fails worker_residue_blocked", () => {
  it(
    "active owner: a live same-key attempt's marker process in scope blocks; it is never signalled and nothing moves",
    { skip: realTableSkip("blocked canonical free: active owner") },
    async () => {
      const iid = 3201;
      const canonical = canonicalFor(iid);
      plantReadOnlyTree(canonical);
      const hash = treeHash(canonical);
      const liveAttempts = new LiveAttemptRegistry();
      const other = newRunAttempt(randomUUID(), 1, canonical, () => []);
      liveAttempts.add(other);
      const pid = orphanIn(canonical, { [RUN_ATTEMPT_ENV]: other.marker, [RUN_CLONE_KEY_ENV]: other.cloneKey });
      // The REAL scan (in-process on a non-split host), recorded.
      const verdicts: QuiesceRunOutcome[] = [];
      const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
        const out = await quiesceRunAttempt(req);
        if (req.site === "canonical_reseed") verdicts.push(out);
        return out;
      };
      const { factory, started } = transientFactory();
      const claim = gitlabClaim(iid);
      await unwired(factory, { quiesceRun, liveAttempts }).execute(claim);
      assert.equal(started(), 0, "no model started");
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked");
      assert.match(String(failed?.failure_reason), /canonical runner clone path could not be freed/);
      assert.ok(
        verdicts[0]?.process?.processes.some((p) => p.pid === pid && p.reason === "live_attempt_conflict"),
        "the live owner is reported as a conflict",
      );
      assert.deepEqual(verdicts[0]?.process?.killed, [], "no signal is sent");
      assert.equal(alive(pid), true, "the live attempt's process is alive");
      assert.equal(treeHash(canonical), hash, "nothing was deleted");
      assert.deepEqual(residueNames(), [], "nothing was moved");
    },
  );

  it("unverified at reseed blocks too: the attempt seed's unreadable-unattributed relaxation does not apply; nothing moves", async () => {
    const iid = 3202;
    const canonical = canonicalFor(iid);
    plantReadOnlyTree(canonical);
    const hash = treeHash(canonical);
    // Exactly the verdict the wired attempt seed relaxes (unverifiedOnlyByUnattributedUnreadable).
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> =>
      req.mode === "seed"
        ? {
            process: {
              state: "unverified",
              processes: [{ pid: 424242, uid: 10002, comm: "node", cwd: "unreadable", reason: "unreadable_unattributed" }],
              killed: [],
              detail: "1 runner-uid process(es) could not be attributed",
            },
            docker: { state: "not_wired", removed: [], detail: "" },
          }
        : quiescentOutcome();
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid);
    await unwired(factory, { quiesceRun }).execute(claim);
    assert.equal(started(), 0);
    assert.equal(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
    assert.equal(treeHash(canonical), hash, "nothing was deleted");
    assert.deepEqual(residueNames(), [], "nothing was moved");
  });

  it("a symlinked canonical path is quarantined, never followed: the link is renamed to residue, its target untouched, and the reseed succeeds", async () => {
    const iid = 3203;
    const canonical = canonicalFor(iid);
    const target = path.join(fx.dataDir, "elsewhere");
    fs.mkdirSync(target, { recursive: true });
    fs.writeFileSync(path.join(target, "KEEP.txt"), "not the runner's\n");
    const targetHash = treeHash(target);
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    fs.symlinkSync(target, canonical);
    const { logger, lines } = recordingLogger();
    const recordingGit = new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const { calls, quiesceRun } = recordedQuiescent();
    let worktree = "";
    const { factory, started } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
    });
    const claim = gitlabClaim(iid);
    await unwired(factory, { quiesceRun }, recordingGit).execute(claim);
    assert.equal(started(), 1, "the reseed succeeded: a planted link no longer wedges the key");
    assert.equal(worktree, canonical);
    assert.equal(lastFailed(claim.run_id)?.fail_origin, undefined, "not a residue block");
    const names = residueNames();
    assert.equal(names.length, 1, `one residue entry: ${names.join(",")}`);
    const residue = path.join(runnerRepoDir(), names[0]!);
    assert.equal(fs.lstatSync(residue).isSymbolicLink(), true, "the LINK itself was moved, not what it points at");
    assert.equal(fs.readlinkSync(residue), target);
    assert.equal(treeHash(target), targetHash, "its target is untouched");
    assert.equal(calls.filter((c) => c.site === "canonical_reseed").length, 0, "no proof over a path that is not a real directory");
    const warn = lines.find((l) => (l as { msg?: string }).msg?.includes("not a real directory"));
    assert.equal((warn as { residue?: string } | undefined)?.residue, residue, "the quarantine is logged with the residue path");
  });

  it("a plain file at the canonical path is quarantined too, and the reseed succeeds", async () => {
    const iid = 3205;
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    fs.writeFileSync(canonical, "planted by another run\n");
    const { quiesceRun } = recordedQuiescent();
    let worktree = "";
    const { factory, started } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
    });
    await unwired(factory, { quiesceRun }).execute(gitlabClaim(iid));
    assert.equal(started(), 1);
    assert.equal(worktree, canonical);
    const names = residueNames();
    assert.equal(names.length, 1);
    assert.equal(fs.readFileSync(path.join(runnerRepoDir(), names[0]!), "utf8"), "planted by another run\n", "the file is kept as residue");
  });

  it(
    "a non-writable parent makes the quarantine rename fail: the run fails worker_residue_blocked, what is left stays",
    { skip: process.getuid?.() === 0 ? "root bypasses the parent's mode" : false },
    async () => {
      const iid = 3204;
      const canonical = canonicalFor(iid);
      plantReadOnlyTree(canonical);
      // Warm the bare first so the only thing the read-only parent can refuse is the free.
      await git.ensureClone(fx.originPath);
      chmodReadOnly(path.dirname(canonical));
      const { factory, started } = transientFactory();
      const claim = gitlabClaim(iid);
      await unwired(factory, { quiesceRun: recordedQuiescent().quiesceRun }).execute(claim);
      assert.equal(started(), 0);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked", `failure_reason: ${String(failed?.failure_reason)}`);
      assert.match(String(failed?.failure_reason), /quarantine rename failed/);
      assert.equal(fs.existsSync(path.join(canonical, "agent", "src", "gate-log.loop")), true, "the undeletable part is still at the canonical path");
      assert.deepEqual(residueNames(), []);
    },
  );
});

// ─── listing consumers ─────────────────────────────────────────────────────────────────────

describe("issue #1783 M3: a quarantined residue is never mistaken for a clone", () => {
  it("a wired seed's sweep never lists residue as a non-live path of the key, and leaves it in place", async () => {
    const iid = 3301;
    const b = await git.ensureClone(fx.originPath);
    const key = `issue-${iid}`;
    const residue = path.join(runnerRepoDir(), formatResidueName(key, randomUUID()));
    fs.mkdirSync(path.join(residue, "agent", "src"), { recursive: true });
    const seeds: string[][] = [];
    const opts: AttemptSeedOptions = {
      attemptId: mintAttemptId(1),
      isLive: () => false,
      beforeSeed: async (p) => void seeds.push(p),
      quiescent: async () => true,
    };
    await git.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID(), false, undefined, opts);
    assert.equal(seeds.length, 1);
    assert.deepEqual(
      seeds[0]!.filter((p) => path.basename(p).startsWith(".uzi-")),
      [],
      "no retained artifact is ever a path of the key",
    );
    assert.equal(fs.existsSync(path.join(residue, "agent", "src")), true, "one residue (under the cap) is kept");
    // A later canonical reseed of the same key leaves it alone too.
    const again = await git.createOrAttachRunnerClone(b, iid, { ...noProofReseed, beforeFree: async () => {} }, randomUUID());
    assert.equal(again.path, canonicalFor(iid));
    assert.equal(fs.existsSync(residue), true);
    assert.equal(fs.readdirSync(runnerRepoDir()).filter((n) => n.startsWith(".uzi-residue-")).length, 1);
  });
});

// ─── the delete runs as the runner uid; the defensive branches ─────────────────────────────

describe("issue #1783 M3 rework: the canonical delete runs as the runner uid, and every defensive branch holds", () => {
  type Seams = Pick<GitCacheOptions, "retentionDelete" | "canonicalFree">;
  const recorded: Array<{ command: string; args: string[] }> = [];

  function seamed(seams: Seams): { g: GitCache; lines: unknown[] } {
    const { logger, lines } = recordingLogger();
    return { g: new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim(), ...seams })), lines };
  }

  /** A forced uid split whose runner-uid rm is recorded, then `effect` stands in for it. */
  function splitRm(effect: (target: string) => void): Seams["retentionDelete"] {
    recorded.length = 0;
    return {
      split: true,
      run: async (command, args) => {
        recorded.push({ command, args });
        effect(args.at(-1)!);
      },
    };
  }

  const failingRm = (): Seams["retentionDelete"] =>
    splitRm(() => {
      throw new Error("runner-uid delete exited 1: rm: can't remove 'agent': Permission denied");
    });

  /** A removable canonical tree (no read-only part). */
  function plantPlainTree(canonical: string): void {
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    fs.writeFileSync(path.join(canonical, "old", "f.txt"), "x");
  }

  it("under a forced uid split the canonical delete is the setpriv-wrapped /bin/rm -rf -- <canonical>, never an in-process rm", async () => {
    const iid = 3401;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const { g } = seamed({ retentionDelete: splitRm((t) => fs.rmSync(t, { recursive: true, force: true })) });
    const b = await g.ensureClone(fx.originPath);
    const clone = await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(clone.path, canonical);
    assert.deepEqual(recorded.map((c) => c.args.at(-1)), [canonical], "exactly one runner-uid delete, of the canonical path");
    const c = recorded[0]!;
    assert.equal(c.command, "/bin/setpriv", "wrapped by runnerCommand");
    assert.deepEqual(c.args.slice(-4), ["/bin/rm", "-rf", "--", canonical], "the fixed rm argv, target last");
    assert.ok(c.args.includes("--reuid"), "as the runner uid");
    assert.deepEqual(residueNames(), []);
  });

  it("a failed runner-uid delete that leaves the path present (a root-owned tree, a timeout) falls through to the same-parent quarantine", async () => {
    const iid = 3402;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const { g, lines } = seamed({ retentionDelete: failingRm() });
    const b = await g.ensureClone(fx.originPath);
    const clone = await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(clone.path, canonical);
    assert.equal(recorded.length, 1);
    const names = residueNames();
    assert.equal(names.length, 1);
    assert.equal(fs.readFileSync(path.join(runnerRepoDir(), names[0]!, "old", "f.txt"), "utf8"), "x", "the tree was moved, whole");
    const warn = lines.find((l) => (l as { msg?: string }).msg?.includes("quarantined it as residue"));
    assert.match(String((warn as { detail?: string }).detail), /Permission denied/);
  });

  it("a failed runner-uid delete that leaves NOTHING to quarantine has an unknown outcome: typed CloneResidueBlockedError", async () => {
    const iid = 3403;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const { g } = seamed({
      retentionDelete: splitRm((t) => {
        fs.rmSync(t, { recursive: true, force: true });
        throw new Error("runner-uid delete timed out after 120000ms");
      }),
    });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError);
      assert.match(err.detail, /left nothing to quarantine: runner-uid delete timed out/);
      return true;
    });
    assert.equal(fs.existsSync(canonical), false, "nothing was seeded");
  });

  it("the re-validation after a failed delete blocks when the repo dir no longer reads as a real directory; nothing is renamed", async () => {
    const iid = 3404;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const hash = treeHash(canonical);
    const repoDir = runnerRepoDir();
    const { g } = seamed({
      retentionDelete: failingRm(),
      canonicalFree: {
        // After the delete ran, the repo dir reads as a symlink.
        lstat: async (p) => (recorded.length > 0 && p === repoDir ? ({ isSymbolicLink: () => true, isDirectory: () => false } as fs.Stats) : fs.promises.lstat(p)),
      },
    });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError);
      assert.match(err.detail, /runner repo dir is not a real directory/);
      return true;
    });
    assert.equal(recorded.length, 1, "the delete ran first");
    assert.equal(treeHash(canonical), hash, "nothing was renamed");
    assert.deepEqual(residueNames(), []);
  });

  it("an existing residue path blocks the rename (rename(2) would silently replace an empty directory)", async () => {
    const iid = 3405;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const hash = treeHash(canonical);
    const uuid = randomUUID();
    const taken = path.join(runnerRepoDir(), formatResidueName(`issue-${iid}`, uuid));
    fs.mkdirSync(taken);
    const { g } = seamed({ retentionDelete: failingRm(), canonicalFree: { residueUuid: () => uuid } });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError);
      assert.match(err.detail, /the residue path already exists/);
      return true;
    });
    assert.equal(treeHash(canonical), hash, "the canonical tree was not moved");
    assert.deepEqual(fs.readdirSync(taken), [], "the existing entry was not replaced");
  });

  it("a canonical path still present after the rename blocks, and the detail names the residue it moved", async () => {
    const iid = 3406;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const uuid = randomUUID();
    const residueName = formatResidueName(`issue-${iid}`, uuid);
    const { g } = seamed({
      retentionDelete: failingRm(),
      canonicalFree: {
        residueUuid: () => uuid,
        // The rename lands, then something re-creates the canonical path.
        rename: async (from, to) => {
          await fs.promises.rename(from, to);
          fs.mkdirSync(from);
        },
      },
    });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError);
      assert.match(err.detail, /still present after the quarantine moved it to /);
      assert.ok(err.detail.includes(residueName), `the detail names the residue: ${err.detail}`);
      return true;
    });
    assert.equal(fs.readFileSync(path.join(runnerRepoDir(), residueName, "old", "f.txt"), "utf8"), "x", "the moved tree is kept");
    assert.equal(fs.existsSync(path.join(canonical, ".git")), false, "nothing was seeded");
  });

  it("a symlinked repo dir is refused before anything runs: nothing behind it is deleted or moved", async () => {
    const iid = 3407;
    const b = await git.ensureClone(fx.originPath);
    const repoDir = runnerRepoDir();
    const elsewhere = path.join(fx.dataDir, "elsewhere-repo");
    fs.mkdirSync(path.join(elsewhere, `issue-${iid}`, "old"), { recursive: true });
    fs.writeFileSync(path.join(elsewhere, `issue-${iid}`, "old", "f.txt"), "not the runner's\n");
    const hash = treeHash(elsewhere);
    fs.mkdirSync(path.dirname(repoDir), { recursive: true });
    fs.rmSync(repoDir, { recursive: true, force: true });
    fs.symlinkSync(elsewhere, repoDir);
    const proofs: string[] = [];
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, { ...noProofReseed, beforeFree: async (p) => void proofs.push(p) }, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError);
      assert.match(err.detail, /runner repo dir is not a real directory/);
      return true;
    });
    assert.deepEqual(proofs, [], "no proof, no delete");
    assert.equal(treeHash(elsewhere), hash, "nothing behind the link was touched");
    fs.unlinkSync(repoDir);
  });
});

describe("issue #1783 M3 rework (N5): there is no unproven plain-rm fallback", () => {
  it("a caller that omits the reseed options (only possible past the type) fails before anything is deleted", async () => {
    const iid = 3501;
    const b = await git.ensureClone(fx.originPath);
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    fs.writeFileSync(path.join(canonical, "old", "f.txt"), "x");
    const hash = treeHash(canonical);
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, undefined as unknown as CanonicalReseedOptions, randomUUID()));
    assert.equal(treeHash(canonical), hash, "the canonical tree was not deleted");
    assert.deepEqual(residueNames(), []);
  });
});

describe("issue #1783 final: a timed-out canonical rm is waited out before any quarantine or reseed", () => {
  type Seams = Pick<GitCacheOptions, "retentionDelete" | "canonicalFree">;

  function seamed(seams: Seams): GitCache {
    return new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim(), ...seams }));
  }

  /** A stand-in rm child that never exits on its own (the busybox rm still walking). */
  function hangingChild(pid: number): ChildProcess {
    const c = new EventEmitter() as unknown as ChildProcess;
    Object.assign(c, { pid, stderr: null });
    return c;
  }

  function plantPlainTree(canonical: string): void {
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    fs.writeFileSync(path.join(canonical, "old", "f.txt"), "x");
  }

  it("the group kill fails and 'close' never arrives: typed CloneResidueBlockedError, nothing renamed, nothing reseeded", async () => {
    const iid = 3601;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const hash = treeHash(canonical);
    const kills: Array<number | undefined> = [];
    const g = seamed({
      retentionDelete: { split: true, spawn: () => hangingChild(910001), kill: (pid) => (kills.push(pid), false), timeoutMs: 20, closeWaitMs: 50 },
    });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError, String(err));
      assert.match(err.detail, /may still be running: group kill of pid 910001 failed after 20ms; no close within 50ms/);
      assert.match(err.detail, /nothing quarantined or reseeded/);
      return true;
    });
    assert.deepEqual(kills, [910001]);
    assert.equal(treeHash(canonical), hash, "the canonical tree is untouched: no rename, no seed");
    assert.deepEqual(residueNames(), []);
  });

  it("the kill lands but 'close' never arrives: typed CloneResidueBlockedError, nothing renamed, nothing reseeded", async () => {
    const iid = 3602;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const hash = treeHash(canonical);
    const g = seamed({ retentionDelete: { split: true, spawn: () => hangingChild(910002), kill: () => true, timeoutMs: 20, closeWaitMs: 50 } });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError, String(err));
      assert.match(err.detail, /may still be running: no exit within 50ms of the runner group kill/);
      assert.doesNotMatch(err.detail, /group kill of pid .* failed/);
      return true;
    });
    assert.equal(treeHash(canonical), hash, "the canonical tree is untouched: no rename, no seed");
    assert.deepEqual(residueNames(), []);
  });

  it("'close' arrives after the kill: the quarantine runs only after it, and the reseed proceeds", async () => {
    const iid = 3603;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const events: string[] = [];
    let child: ChildProcess | undefined;
    const g = seamed({
      retentionDelete: {
        split: true,
        spawn: () => (child = hangingChild(910003)),
        kill: () => {
          events.push("kill");
          setTimeout(() => {
            events.push("close");
            child!.emit("close", null, "SIGKILL");
          }, 300);
          return true;
        },
        timeoutMs: 20,
        closeWaitMs: 5_000,
      },
      canonicalFree: {
        rename: async (from, to) => {
          events.push("rename");
          await fs.promises.rename(from, to);
        },
      },
    });
    const b = await g.ensureClone(fx.originPath);
    const clone = await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(clone.path, canonical);
    assert.deepEqual(events, ["kill", "close", "rename"], "the rename waited for the killed rm's close");
    const names = residueNames();
    assert.equal(names.length, 1);
    assert.equal(fs.readFileSync(path.join(runnerRepoDir(), names[0]!, "old", "f.txt"), "utf8"), "x", "the tree was moved, whole");
  });

  it("N2: the group kill fails but 'close' arrives within the wait (the group had already exited): the reseed proceeds", async () => {
    const iid = 3604;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const events: string[] = [];
    let child: ChildProcess | undefined;
    const g = seamed({
      retentionDelete: {
        split: true,
        spawn: () => (child = hangingChild(910004)),
        kill: () => {
          events.push("kill failed");
          setImmediate(() => {
            events.push("close");
            child!.emit("close", 0, null);
          });
          return false;
        },
        timeoutMs: 20,
        closeWaitMs: 5_000,
      },
      canonicalFree: {
        rename: async (from, to) => {
          events.push("rename");
          await fs.promises.rename(from, to);
        },
      },
    });
    const b = await g.ensureClone(fx.originPath);
    const clone = await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(clone.path, canonical);
    assert.deepEqual(events, ["kill failed", "close", "rename"], "an ordinary timeout: the leftover tree is quarantined, then reseeded");
    assert.equal(residueNames().length, 1);
  });

  it("N1: an unsettled rm blocks every later claim of the same path until its 'close' arrives", async () => {
    const iid = 3605;
    const canonical = canonicalFor(iid);
    plantPlainTree(canonical);
    const hash = treeHash(canonical);
    const spawned: string[] = [];
    let first: ChildProcess | undefined;
    const g = seamed({
      retentionDelete: {
        split: true,
        spawn: (_command, args) => {
          const target = args.at(-1)!;
          spawned.push(target);
          if (first === undefined) return (first = hangingChild(910005));
          // A later rm deletes its target and exits 0.
          const c = hangingChild(910006);
          setImmediate(() => {
            fs.rmSync(target, { recursive: true, force: true });
            c.emit("close", 0, null);
          });
          return c;
        },
        kill: () => false,
        timeoutMs: 20,
        closeWaitMs: 50,
      },
    });
    const b = await g.ensureClone(fx.originPath);
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError, String(err));
      assert.match(err.detail, /group kill of pid 910005 failed/);
      return true;
    });
    let proofs = 0;
    const counting: CanonicalReseedOptions = { ...noProofReseed, beforeFree: async () => void proofs++ };
    await assert.rejects(g.createOrAttachRunnerClone(b, iid, counting, randomUUID()), (err: unknown) => {
      assert.ok(err instanceof CloneResidueBlockedError, String(err));
      assert.match(err.detail, /an earlier runner-uid delete of this path may still be running/);
      return true;
    });
    assert.equal(spawned.length, 1, "the second claim started no rm beside the first");
    assert.equal(proofs, 0, "and touched nothing");
    assert.equal(treeHash(canonical), hash, "the canonical tree is untouched: no rename, no seed");
    assert.deepEqual(residueNames(), []);

    first!.emit("close", null, "SIGKILL");
    const clone = await g.createOrAttachRunnerClone(b, iid, counting, randomUUID());
    assert.equal(clone.path, canonical);
    assert.equal(proofs, 1);
    assert.deepEqual(spawned, [canonical, canonical], "the third claim ran its own rm, then reseeded");
    assert.deepEqual(residueNames(), []);
  });
});

describe("issue #1783 final (R3): the clone key alone never puts a process in a seed's scope", { skip: realTableSkip("final (R3): the clone key alone") }, () => {
  /** A canonical reseed of `iid` over the REAL in-process scan, its canonical_reseed verdicts recorded. */
  async function reseedWith(iid: number, liveAttempts: LiveAttemptRegistry) {
    const verdicts: QuiesceRunOutcome[] = [];
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      const out = await quiesceRunAttempt(req);
      if (req.site === "canonical_reseed") verdicts.push(out);
      return out;
    };
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid);
    await unwired(factory, { quiesceRun, liveAttempts }).execute(claim);
    return { verdicts, started: started(), failed: lastFailed(claim.run_id) };
  }

  const keyOf = (canonical: string): string => newRunAttempt(randomUUID(), 1, canonical, () => []).cloneKey;

  it("a key-only foreign process in /tmp (no attempt marker) neither blocks the seed nor is killed by it", async () => {
    const iid = 3701;
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    // Run B: from /tmp, with the attempt marker unset and this key set, a detached sleep.
    const pid = orphanIn("/tmp", { [RUN_CLONE_KEY_ENV]: keyOf(canonical) });
    const { verdicts, started, failed } = await reseedWith(iid, new LiveAttemptRegistry());
    assert.notEqual(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
    assert.equal(started, 1, "the seed went through and the model started");
    assert.equal(verdicts[0]?.process?.state, "quiescent", verdicts[0]?.process?.detail);
    assert.ok(!verdicts[0]?.process?.processes.some((p) => p.pid === pid), "never reported");
    assert.deepEqual(verdicts[0]?.process?.killed, [], "never signalled");
    assert.equal(alive(pid), true, "the foreign process is alive");
  });

  it("a MARKED terminal same-key process in /tmp is still in scope by key, and killed", async () => {
    const iid = 3702;
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    const gone = newRunAttempt(randomUUID(), 1, canonical, () => []);
    const pid = orphanIn("/tmp", { [RUN_ATTEMPT_ENV]: gone.marker, [RUN_CLONE_KEY_ENV]: gone.cloneKey });
    const { verdicts, started } = await reseedWith(iid, new LiveAttemptRegistry());
    assert.equal(started, 1, "the seed went through");
    assert.deepEqual(verdicts[0]?.process?.killed, [pid], "the terminal attempt's process was reaped");
    assert.equal(alive(pid), false);
  });

  it("a MARKED live same-key process in /tmp is still a conflict: never signalled, the seed blocks", async () => {
    const iid = 3703;
    const canonical = canonicalFor(iid);
    fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
    const hash = treeHash(canonical);
    const liveAttempts = new LiveAttemptRegistry();
    const other = newRunAttempt(randomUUID(), 1, canonical, () => []);
    liveAttempts.add(other);
    const pid = orphanIn("/tmp", { [RUN_ATTEMPT_ENV]: other.marker, [RUN_CLONE_KEY_ENV]: other.cloneKey });
    const { verdicts, started, failed } = await reseedWith(iid, liveAttempts);
    assert.equal(started, 0, "no model started");
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.ok(verdicts[0]?.process?.processes.some((p) => p.pid === pid && p.reason === "live_attempt_conflict"), "reported as a conflict");
    assert.deepEqual(verdicts[0]?.process?.killed, []);
    assert.equal(alive(pid), true);
    assert.equal(treeHash(canonical), hash, "nothing moved");
  });
});


describe("M1 canonical residue retention through the public seed path", () => {
  const iid = 3901;
  async function planted(count = 7): Promise<{ b: string; paths: string[] }> {
    const b = await git.ensureClone(fx.originPath);
    fs.mkdirSync(runnerRepoDir(), { recursive: true });
    const paths = Array.from({ length: count }, (_, i) => path.join(runnerRepoDir(),
      formatResidueName(`issue-${iid}`, `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`)));
    for (const p of paths) {
      fs.mkdirSync(p);
      fs.writeFileSync(path.join(p, "kept.txt"), "residue");
      fs.utimesSync(p, 1000, 1000);
    }
    return { b, paths };
  }
  afterEach(() => resetResidueQuarantineForTests());

  it("residue work cap attempts at most 16 candidates per canonical sweep", async () => {
    const { b, paths } = await planted(45);
    const attempts: string[] = [];
    const opts = { ...noProofReseed, quiescent: async ([p]: string[]) => { attempts.push(p!); return false; } };
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.deepEqual(attempts, paths.slice(0, 16), "one sweep must not prove the entire backlog");
    attempts.length = 0;
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.deepEqual(attempts, paths.slice(16, 32), "the next sweep starts beyond the refused prefix");
    assert.ok(paths.every(p => fs.existsSync(p)));
  });

  it("residue work cap progresses beyond 20 persistently refused entries and keeps the newest five", async () => {
    const { b, paths } = await planted(45);
    const refused = new Set(paths.slice(0, 20));
    const attempts: string[] = [];
    const opts = { ...noProofReseed, quiescent: async ([p]: string[]) => {
      attempts.push(p!); return !refused.has(p!);
    } };
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.deepEqual(attempts, paths.slice(0, 16));
    assert.ok(paths.every(p => fs.existsSync(p)), "the refused first batch cannot consume all backlog work");
    for (let round = 0; round < 3; round++) {
      attempts.length = 0;
      await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
      assert.ok(attempts.length <= 16);
      assert.ok(paths.slice(40).every(p => fs.existsSync(p)), "the original newest five are never substitutes");
    }
    assert.ok(paths.slice(0, 20).every(p => fs.existsSync(p)));
    assert.ok(paths.slice(20, 40).every(p => !fs.existsSync(p)), "later safe excess is reached despite the refused prefix");
  });

  it("residue work cap revisits a refusal that later clears", async () => {
    const { b, paths } = await planted(23);
    let cleared = false;
    const attempts: string[] = [];
    const opts = { ...noProofReseed, quiescent: async ([p]: string[]) => {
      attempts.push(p!); return cleared && p === paths[0];
    } };
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.ok(paths.every(p => fs.existsSync(p)));
    cleared = true;
    attempts.length = 0;
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.ok(attempts.includes(paths[0]!), "a refusal is not cached as a permanent veto");
    assert.ok(attempts.length <= 16);
    assert.equal(fs.existsSync(paths[0]!), false);
    assert.ok(paths.slice(-5).every(p => fs.existsSync(p)));
  });

  it("residue work cap clears progress when no excess remains", async () => {
    const { b, paths } = await planted(45);
    const attempts: string[] = [];
    const opts = { ...noProofReseed, quiescent: async ([p]: string[]) => { attempts.push(p!); return false; } };
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    for (const p of paths.slice(0, 40)) fs.rmSync(p, { recursive: true });
    attempts.length = 0;
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.deepEqual(attempts, []);
    for (const p of paths.slice(0, 40)) {
      fs.mkdirSync(p);
      fs.utimesSync(p, 1000, 1000);
    }
    await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
    assert.deepEqual(attempts, paths.slice(0, 16), "new excess starts from oldest after an empty sweep");
  });

  for (const kind of ["recognized", "unrecognized", "read failure"] as const) {
    it(`bounded enumeration refuses ${kind} overflow or failure before candidate work and closes the directory`, async (t) => {
      const { b, paths } = await planted();
      if (kind !== "read failure") {
        for (let i = 7; i < 4097; i++) {
          const name = kind === "recognized"
            ? formatResidueName(`issue-${iid}`, `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`)
            : `unrecognized-${i}`;
          fs.writeFileSync(path.join(runnerRepoDir(), name), "keep");
        }
      }
      const before = fs.readdirSync(runnerRepoDir()).sort();
      const rec = recordingLogger();
      let proofs = 0;
      const deletes: string[] = [];
      const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
        gitleaksBin: defaultGitleaksShim(), retentionDelete: {
          split: true, run: async (_command, args) => { deletes.push(args.at(-1)!); },
        },
      }));
      const originalOpen = fs.promises.opendir.bind(fs.promises);
      let opened: fs.Dir | undefined;
      let enumerated = 0;
      t.mock.method(fs.promises, "opendir", async (...args: Parameters<typeof fs.promises.opendir>) => {
        const dir = await originalOpen(...args);
        if (args[0] !== runnerRepoDir()) return dir;
        opened = dir;
        return {
          async *[Symbol.asyncIterator]() {
            for await (const entry of dir) {
              enumerated++;
              if (kind === "read failure" && enumerated === 3) throw new Error("EACCES directory read");
              yield entry;
            }
          },
        } as unknown as fs.Dir;
      });
      const originalStat = fs.promises.lstat.bind(fs.promises);
      let candidateStats = 0;
      t.mock.method(fs.promises, "lstat", async (...args: Parameters<typeof fs.promises.lstat>) => {
        if (typeof args[0] === "string" && before.includes(path.basename(args[0]))
          && path.dirname(args[0]) === runnerRepoDir()) candidateStats++;
        return originalStat(...args);
      });
      const clone = await g.createOrAttachRunnerClone(b, iid, {
        ...noProofReseed, quiescent: async () => { proofs++; return true; },
      }, randomUUID());
      assert.equal(clone.path, canonicalFor(iid), "ordinary seed still succeeds");
      assert.equal(enumerated, kind === "read failure" ? 3 : 4097, "stop at the first entry over the fixed budget");
      assert.ok(opened);
      await assert.rejects(opened.read(), { code: "ERR_DIR_CLOSED" });
      assert.equal(candidateStats, 0);
      assert.equal(proofs, 0);
      assert.deepEqual(deletes, []);
      assert.deepEqual(fs.readdirSync(runnerRepoDir()).filter(n => n !== `issue-${iid}`).sort(), before);
      assert.ok(paths.every(p => fs.readFileSync(path.join(p, "kept.txt"), "utf8") === "residue"));
      const warn = rec.lines.find(l => /retention sweep failed/.test(String((l as { msg?: string }).msg))) as { error: string };
      assert.match(warn.error, kind === "read failure" ? /EACCES directory read/ : /4096-entry work budget/);
      assert.ok(warn.error.length <= 160);
    });
  }

  it("exactly 4096 siblings still sweep recognized excess down to five", async () => {
    const { b, paths } = await planted();
    for (let i = 7; i < 4096; i++) fs.writeFileSync(path.join(runnerRepoDir(), `unrecognized-${i}`), "keep");
    const proofs: string[][] = [];
    await git.createOrAttachRunnerClone(b, iid, {
      ...noProofReseed, quiescent: async ps => { proofs.push(ps); return true; },
    }, randomUUID());
    assert.deepEqual(proofs, paths.slice(0, 2).map(p => [p]));
    assert.deepEqual(paths.map(p => fs.existsSync(p)), [false, false, true, true, true, true, true]);
    assert.equal(fs.readFileSync(path.join(runnerRepoDir(), "unrecognized-4095"), "utf8"), "keep");
  });

  it("ordinary canonical reseeds cap tied-mtime siblings at five, including repeated replenishment", async () => {
    const { b, paths } = await planted();
    const protectedNames = [formatResidueName("issue-3902", randomUUID()),
      ".uzi-residue-issue-3901.residue-malformed", `issue-${iid}.attempt-${mintAttemptId(1)}`,
      `.uzi-skills-issue-${iid}`, `issue-3902`];
    for (const name of protectedNames) fs.mkdirSync(path.join(runnerRepoDir(), name));
    const proofs: string[][] = [];
    const opts = { ...noProofReseed, quiescent: async (ps: string[]) => { proofs.push(ps); return true; } };
    for (let round = 0; round < 3; round++) {
      const clone = await git.createOrAttachRunnerClone(b, iid, opts, randomUUID());
      assert.equal(clone.path, canonicalFor(iid));
      assert.deepEqual(paths.map(p => fs.existsSync(p)), [false, false, true, true, true, true, true]);
      if (round < 2) for (const p of paths.slice(0, 2)) { fs.mkdirSync(p); fs.utimesSync(p, 1000, 1000); }
    }
    assert.deepEqual(proofs, Array.from({ length: 3 }, () => paths.slice(0, 2).map(p => [p])).flat());
    for (const name of protectedNames) assert.ok(fs.existsSync(path.join(runnerRepoDir(), name)));
  });

  for (const kind of ["false", "throw", "absent", "live", "mid-live", "quarantine", "mid-quarantine", "journal", "mid-journal"] as const) {
    it(`keeps the original excess on ${kind}, without deleting newer replacements`, async () => {
      const { b, paths } = await planted(6);
      const rec = recordingLogger();
      const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
      let live = kind === "live";
      const journal = async () => {
        const child = path.join(paths[0]!, "child");
        fs.mkdirSync(child, { recursive: true });
        execFileSync("git", ["-C", b, "config", "--local", "uzi-recovery.agent/other-retained.clone",
          JSON.stringify({ clonePath: child, runId: randomUUID() })]);
        fs.utimesSync(paths[0]!, 1000, 1000);
      };
      if (kind === "journal") await journal();
      if (kind === "quarantine") latchResidueQuarantine({ cause: "test", site: "canonical_retention" }, nullLogger());
      const opts: CanonicalReseedOptions = {
        ...noProofReseed, isLive: () => live,
        quiescent: async () => {
          if (kind === "throw") throw new Error("unverified\n" + "x".repeat(300));
          if (kind === "mid-live") live = true;
          if (kind === "mid-quarantine") latchResidueQuarantine({ cause: "test", site: "canonical_retention" }, nullLogger());
          if (kind === "mid-journal") await journal();
          return kind === "absent" ? undefined as unknown as boolean : kind !== "false";
        },
      };
      await g.createOrAttachRunnerClone(b, iid, opts, randomUUID());
      assert.ok(paths.every(p => fs.existsSync(p)));
      assert.ok(rec.lines.some(l => String((l as { msg?: string }).msg).includes("keeping it")));
      for (const entry of rec.lines) {
        const line = entry as { reason?: string };
        if (typeof line.reason === "string") {
        assert.ok(line.reason.length <= 160);
        assert.ok(!line.reason.includes("\n"));
        }
      }
    });
  }

  it("unlinks residue symlinks and files without following an external target", async () => {
    const { b, paths } = await planted();
    const external = path.join(fx.dataDir, "external-retained");
    fs.mkdirSync(external);
    fs.writeFileSync(path.join(external, "keep"), "keep");
    fs.rmSync(paths[0]!, { recursive: true }); fs.symlinkSync(external, paths[0]!);
    fs.rmSync(paths[1]!, { recursive: true }); fs.writeFileSync(paths[1]!, "file");
    fs.lutimesSync(paths[0]!, 1, 1); fs.utimesSync(paths[1]!, 1, 1);
    await git.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(fs.existsSync(paths[0]!), false); assert.equal(fs.existsSync(paths[1]!), false);
    assert.equal(fs.readFileSync(path.join(external, "keep"), "utf8"), "keep");
  });

  it("recovery interruption during proof propagates and leaves every residue", async () => {
    const { b, paths } = await planted();
    const controller = new AbortController();
    await assert.rejects(git.withRecoveryOperation(controller.signal, Date.now() + 30000, () =>
      git.createOrAttachRunnerClone(b, iid, { ...noProofReseed, quiescent: async () => {
        controller.abort(new Error("retention interrupted")); throw controller.signal.reason;
      } }, randomUUID())), /retention interrupted/);
    assert.ok(paths.every(p => fs.existsSync(p)));
    assert.equal(fs.existsSync(canonicalFor(iid)), false);
  });

  for (const failure of ["journal enumeration", "parent enumeration", "path validation"] as const) {
    it(`unreadable ${failure} after the canonical guard aborts all residue reclamation`, async (t) => {
      const { b, paths } = await planted();
      fs.mkdirSync(canonicalFor(iid));
      const rec = recordingLogger();
      const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
      let guarded = false;
      let injected = false;
      const originalRead = fs.promises.opendir.bind(fs.promises);
      const originalStat = fs.promises.lstat.bind(fs.promises);
      const gitSeam = g as unknown as { runGit: (cwd: string, args: string[]) => Promise<string> };
      const originalGit = gitSeam.runGit.bind(g);
      t.mock.method(gitSeam, "runGit", async (cwd: string, args: string[]) => {
        if (guarded && !injected && failure === "journal enumeration" && args.includes("--list")) {
          injected = true;
          throw new Error("EACCES journal enumeration");
        }
        return originalGit(cwd, args);
      });
      t.mock.method(fs.promises, "opendir", async (...args: Parameters<typeof fs.promises.opendir>) => {
        if (guarded && !injected && failure === "parent enumeration" && args[0] === runnerRepoDir()) {
          injected = true;
          throw new Error("EACCES parent enumeration");
        }
        return originalRead(...args);
      });
      t.mock.method(fs.promises, "lstat", async (...args: Parameters<typeof fs.promises.lstat>) => {
        if (guarded && !injected && failure === "path validation" && args[0] === paths[1]) {
          injected = true;
          throw new Error("EACCES path validation");
        }
        return originalStat(...args);
      });
      let proofs = 0;
      await g.createOrAttachRunnerClone(b, iid, {
        ...noProofReseed, beforeFree: async () => { guarded = true; },
        quiescent: async () => { proofs++; return true; },
      }, randomUUID());
      assert.ok(injected);
      assert.equal(proofs, 0);
      assert.ok(paths.every(p => fs.existsSync(p)));
      assert.ok(rec.lines.some(l => /retention sweep failed/.test(String((l as { msg?: string }).msg))
        && /EACCES/.test(String((l as { error?: string }).error))));
    });
  }

  it("a canonical-free proof failure preserves its original typed failure and never sweeps", async () => {
    const { b, paths } = await planted();
    fs.mkdirSync(canonicalFor(iid));
    const original = new CloneResidueBlockedError("canonical proof refused");
    let proofs = 0;
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, {
      ...noProofReseed, beforeFree: async () => { throw original; },
      quiescent: async () => { proofs++; return true; },
    }, randomUUID()), err => err === original);
    assert.equal(proofs, 0);
    assert.ok(paths.every(p => fs.existsSync(p)));
    assert.ok(fs.existsSync(canonicalFor(iid)));
  });

  it("a failed runner-uid residue delete stops the canonical sweep and keeps the newer excess", async () => {
    const { b, paths } = await planted();
    const rec = recordingLogger();
    const deletes: string[] = [];
    const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
      gitleaksBin: defaultGitleaksShim(), retentionDelete: {
        split: true, run: async (_command, args) => {
          deletes.push(args.at(-1)!);
          throw new Error("EACCES residue delete\n" + "x".repeat(300));
        },
      },
    }));
    await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.deepEqual(deletes, [paths[0]]);
    assert.ok(paths.every(p => fs.existsSync(p)));
    const warn = rec.lines.find(l => /retention sweep failed/.test(String((l as { msg?: string }).msg))) as { error: string };
    assert.match(warn.error, /EACCES/);
    assert.ok(warn.error.length <= 160);
    assert.ok(!warn.error.includes("\n"));
  });

  it("an unsettled residue delete stops the canonical sweep; later sweeps refuse until close", async () => {
    const { b, paths } = await planted();
    const rec = recordingLogger();
    const deletes: string[] = [];
    let held: ChildProcess | undefined;
    const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
      gitleaksBin: defaultGitleaksShim(), retentionDelete: {
        split: true, timeoutMs: 5, closeWaitMs: 5, kill: () => false,
        spawn: (_command, args) => {
          const target = args.at(-1)!;
          deletes.push(target);
          const child = new EventEmitter() as unknown as ChildProcess;
          Object.assign(child, { pid: 920001 + deletes.length, stderr: null });
          if (target === paths[0] && !held) held = child;
          else setImmediate(() => {
            fs.rmSync(target, { recursive: true, force: true });
            child.emit("close", 0, null);
          });
          return child;
        },
      },
    }));
    await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.deepEqual(deletes, [paths[0]]);
    assert.ok(paths.every(p => fs.existsSync(p)));
    assert.ok(rec.lines.some(l => /retention sweep failed/.test(String((l as { msg?: string }).msg))
      && /may still be running/.test(String((l as { error?: string }).error))));
    await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(deletes.filter(p => p === paths[0]).length, 1);
    assert.ok(fs.existsSync(paths[0]!));
    assert.ok(rec.lines.some(l => (l as { path?: string }).path === paths[0]
      && /may still be running/.test(String((l as { reason?: string }).reason))));
    held!.emit("close", null, "SIGKILL");
    await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.equal(deletes.filter(p => p === paths[0]).length, 2);
    assert.equal(fs.existsSync(paths[0]!), false);
  });

  for (const entry of ["directory", "file", "symlink"] as const) {
    for (const veto of ["quarantine", "live", "path", "parent"] as const) {
      it(`${veto} changing inside awaited delete lstat vetoes ${entry} destruction`, async (t) => {
        const { b, paths } = await planted();
        const target = paths[0]!;
        const external = path.join(fx.dataDir, "external-kept");
        fs.mkdirSync(external);
        fs.writeFileSync(path.join(external, "keep"), "keep");
        if (entry !== "directory") {
          fs.rmSync(target, { recursive: true });
          if (entry === "file") fs.writeFileSync(target, "keep");
          else fs.symlinkSync(external, target);
          fs.lutimesSync(target, 1000, 1000);
        }
        const rec = recordingLogger();
        let live = false;
        let checked = false;
        const deletes: string[] = [];
        const originalStat = fs.promises.lstat.bind(fs.promises);
        let parentVetoed = false;
        t.mock.method(fs.promises, "lstat", async (...args: Parameters<typeof fs.promises.lstat>) => {
          const st = await originalStat(...args);
          if (veto === "parent" && checked && !parentVetoed && args[0] === runnerRepoDir()) {
            parentVetoed = true;
            return Object.assign(Object.create(st) as fs.Stats, { isSymbolicLink: () => true, isDirectory: () => false });
          }
          return st;
        });
        const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
          gitleaksBin: defaultGitleaksShim(), retentionDelete: {
            split: true, run: async (_command, args) => { deletes.push(args.at(-1)!); },
            lstat: async p => {
              const st = await fs.promises.lstat(p);
              if (p === target) {
                await Promise.resolve();
                checked = true;
                if (veto === "quarantine") latchResidueQuarantine({ cause: "test", site: "delete_lstat" }, nullLogger());
                if (veto === "live") live = true;
                if (veto === "path") throw new Error("EACCES delete path validation");
              }
              return st;
            },
          },
        }));
        await g.createOrAttachRunnerClone(b, iid, { ...noProofReseed, isLive: () => live }, randomUUID());
        assert.ok(checked);
        if (veto === "parent") assert.ok(parentVetoed, "parent is revalidated after the awaited target lstat");
        assert.deepEqual(deletes, []);
        assert.ok(paths.every(p => fs.existsSync(p)));
        assert.equal(fs.readFileSync(path.join(external, "keep"), "utf8"), "keep");
        assert.ok(rec.lines.some(l => /keeping it|retention sweep failed/.test(String((l as { msg?: string }).msg))));
      });
    }
  }

  // Exercise the actual private options creators without executing the slow recovery lifecycle.
  function retentionOptions(extra: Parameters<typeof runnerWith>[4] = {}) {
    const { factory } = transientFactory();
    const runner = unwired(factory, extra);
    const claim = gitlabClaim(iid);
    const flight = {
      runId: claim.run_id, attemptId: mintAttemptId(1), effectiveAttemptPaths: true,
      executor: {}, runLog: nullLogger(), sinkGate: { run: async <T>(fn: () => Promise<T>) => fn() },
    };
    const creators = runner as unknown as {
      canonicalReseedOptions: (flight: unknown) => CanonicalReseedOptions;
      attemptSeedOptions: (claim: unknown, flight: unknown) => AttemptSeedOptions;
    };
    return {
      canonical: creators.canonicalReseedOptions(flight),
      attempt: creators.attemptSeedOptions(claim, flight),
    };
  }

  for (const kind of ["canonical", "attempt"] as const) {
    for (const Control of [StaleClaimError, PauseNowSignal]) {
      it(`real ${kind} retention propagates the same ${Control.name} through the public seed path`, async () => {
        const { b, paths } = await planted();
        const stop = new Control();
        let proofs = 0;
        const opts = retentionOptions({ quiesceRun: async req => {
          if (req.site === "attempt_seed") return quiescentOutcome();
          proofs++;
          throw stop;
        } });
        await assert.rejects(opts[kind].quiescent([paths[0]!], canonicalFor(iid)), err => err === stop);
        await assert.rejects(git.createOrAttachRunnerClone(b, iid, opts.canonical, randomUUID(),
          false, undefined, kind === "attempt" ? opts.attempt : undefined), err => err === stop);
        assert.equal(proofs, 2, "public seed stops after the first interrupted retention proof");
        assert.equal(fs.existsSync(canonicalFor(iid)), false);
        assert.ok(paths.every(p => fs.existsSync(p)), "no residues removed");
      });
    }

    it(`real ${kind} retention treats generic errors as best-effort refusal`, async () => {
      const { b, paths } = await planted();
      let proofs = 0;
      const opts = retentionOptions({ quiesceRun: async req => {
        if (req.site === "attempt_seed") return quiescentOutcome();
        proofs++;
        throw new Error("ordinary proof failure");
      } });
      const clone = await git.createOrAttachRunnerClone(b, iid, opts.canonical, randomUUID(),
        false, undefined, kind === "attempt" ? opts.attempt : undefined);
      assert.ok(fs.existsSync(clone.path));
      assert.equal(proofs, 2, "both original excess entries were refused");
      assert.ok(paths.every(p => fs.existsSync(p)));
    });
  }

  for (const kind of ["empty", "live", "unattributed", "unreadable"] as const) {
    it(`runner canonical retention uses the real proof with configured Docker: ${kind}`, { skip: process.platform !== "linux" }, async () => {
      const { paths } = await planted(6);
      const procRoot = makeFakeProcRoot();
      const registry = new LiveAttemptRegistry();
      const live = newRunAttempt(randomUUID(), 1, canonicalFor(iid), () => []);
      if (kind === "live") registry.add(live);
      if (kind === "live") plantInScope(procRoot, 424242, paths[0]!, { marker: live.marker, cloneKey: live.cloneKey });
      if (kind === "unattributed") plantInScope(procRoot, 424242, paths[0]!);
      if (kind === "unreadable") plantUnreadableUnattributed(procRoot, 424242);
      const requests: QuiesceRunRequest[] = [];
      const outcomes: QuiesceRunOutcome[] = [];
      const dockerHost = `unix://${path.join(fx.dataDir, "absent-docker.sock")}`;
      const opts = retentionOptions({
        dockerHost, liveAttempts: registry, quiesceRun: async req => {
          requests.push(req);
          const outcome = await quiesceRunAttempt(req);
          outcomes.push(outcome);
          return outcome;
        },
      });
      try {
        await withQuiescenceView({ procRoot }, async () => {
          assert.equal(await opts.canonical.quiescent([paths[0]!], canonicalFor(iid)), kind === "empty");
          assert.equal(requests[0]?.dockerHost, dockerHost);
          assert.equal(requests[0]?.mode, "seed");
          assert.equal(requests[0]?.site, "canonical_retention");
          assert.deepEqual(requests[0]?.targetPaths, [paths[0]]);
          assert.equal(outcomes[0]?.docker.state, "docker_error");
          assert.deepEqual(outcomes[0]?.process?.killed, []);
          if (kind !== "empty") assert.ok(fs.existsSync(path.join(procRoot, "424242")), "no signal to a live or unattributed process");
          if (kind === "unreadable") {
            assert.equal(residueQuarantine()?.site, "canonical_retention");
            fs.rmSync(path.join(procRoot, "424242"), { recursive: true });
            assert.equal(await opts.canonical.quiescent([paths[0]!], canonicalFor(iid)), false);
            assert.equal(requests.length, 1, "latched quarantine vetoes even a now-quiescent proof");
          } else {
            latchResidueQuarantine({ cause: "test", site: "before_proof" }, nullLogger());
            assert.equal(await opts.canonical.quiescent([paths[0]!], canonicalFor(iid)), false);
            assert.equal(requests.length, 1, "quarantine is checked before proof");
          }
        });
      } finally {
        fs.rmSync(procRoot, { recursive: true, force: true });
      }
    });
  }

  for (const kind of ["canonical", "attempt"] as const) {
    it(`runner ${kind} retention checks quarantine before and after an affirmative proof`, async () => {
      let proofs = 0;
      const opts = retentionOptions({ quiesceRun: async () => {
        proofs++;
        latchResidueQuarantine({ cause: "test", site: "during_proof" }, nullLogger());
        return quiescentOutcome();
      } });
      assert.equal(await opts[kind].quiescent(["retained"], canonicalFor(iid)), false);
      assert.equal(proofs, 1, "quarantine set during an affirmative proof vetoes deletion");
      assert.equal(await opts[kind].quiescent(["retained"], canonicalFor(iid)), false);
      assert.equal(proofs, 1, "already quarantined skips the next proof");
    });
  }

  it("a failed residue unlink stops the canonical sweep and preserves newer excess", async () => {
    const { b, paths } = await planted();
    const rec = recordingLogger();
    const g = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
      gitleaksBin: defaultGitleaksShim(), retentionDelete: { lstat: async p => {
        const st = await fs.promises.lstat(p);
        return p === paths[0] ? Object.assign(Object.create(st) as fs.Stats,
          { isSymbolicLink: () => true, isDirectory: () => false }) : st;
      } },
    }));
    await g.createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID());
    assert.ok(paths.every(p => fs.existsSync(p)));
    assert.ok(rec.lines.some(l => /retention sweep failed/.test(String((l as { msg?: string }).msg))
      && /EISDIR|EPERM/.test(String((l as { error?: string }).error))));
  });

  it("runner attempt retention rejects an ABSENT process verdict through its real callback and stays process-only", async () => {
    const requests: QuiesceRunRequest[] = [];
    const opts = retentionOptions({
      dockerHost: "unix:///absent-retention.sock", quiesceRun: async req => {
        requests.push(req);
        return { ...quiescentOutcome(), process: undefined };
      },
    });
    assert.equal(await opts.attempt.quiescent(["retained"], canonicalFor(iid)), false);
    assert.equal(requests[0]?.site, "attempt_retention");
    assert.equal(requests[0]?.dockerHost, undefined);
    assert.equal(requests[0]?.processes, process.platform === "linux");
  });

  it("fresh attempt unreadable relaxation preserves retentionVetoed after the quarantine latch resets", async () => {
    const requests: QuiesceRunRequest[] = [];
    const opts = retentionOptions({ quiesceRun: async req => {
      requests.push(req);
      return { ...quiescentOutcome(), process: {
        state: "unverified", killed: [], detail: "unreadable",
        processes: [{ pid: 424242, uid: process.getuid?.() ?? 0, comm: "node", cwd: "unreadable", reason: "unreadable_unattributed" }],
      } };
    } });
    await opts.attempt.beforeSeed([], canonicalFor(iid));
    resetResidueQuarantineForTests();
    assert.equal(await opts.attempt.quiescent(["retained"], canonicalFor(iid)), false);
    assert.equal(requests.length, 1, "this seed's veto survives a later affirmative proof");
  });

  for (const verdict of ["quiescent", "unverified", "absent"] as const) {
    it(`runner canonical retention requests seed-mode proof and requires affirmative verdict: ${verdict}`, async () => {
      const { paths } = await planted(6);
      const calls: QuiesceRunRequest[] = [];
      const { factory } = transientFactory();
      await unwired(factory, { quiesceRun: async req => {
        calls.push(req);
        if (req.site !== "canonical_retention" || verdict === "quiescent") return quiescentOutcome();
        return { ...quiescentOutcome(), process: verdict === "absent" ? undefined : { state: "unverified", processes: [], killed: [], detail: "unverified" } };
      } }).execute(gitlabClaim(iid));
      const req = calls.find(c => c.site === "canonical_retention");
      assert.ok(req); assert.equal(req.mode, "seed"); assert.deepEqual(req.targetPaths, [paths[0]]);
      assert.equal(req.dockerHost, undefined); assert.equal(req.cloneKey, newRunAttempt("r", 1, canonicalFor(iid), () => []).cloneKey);
      assert.equal(fs.existsSync(paths[0]!), verdict !== "quiescent");
      assert.ok(paths.slice(1).every(p => fs.existsSync(p)));
    });
  }
});
