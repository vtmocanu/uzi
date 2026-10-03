import { after, afterEach, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync, type ChildProcess } from "node:child_process";
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
import { failOriginForReason, RunRunner, type ExecutorFactory } from "../src/runner.js";
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
import { scopedRealView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { realProcfsSkip } from "./real-procfs.js";
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
    return { calls, reseed: { beforeFree: async (p) => void calls.push(p) } };
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
    const again = await git.createOrAttachRunnerClone(b, iid, { beforeFree: async () => {} }, randomUUID());
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
    await assert.rejects(git.createOrAttachRunnerClone(b, iid, { beforeFree: async (p) => void proofs.push(p) }, randomUUID()), (err: unknown) => {
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
    const counting: CanonicalReseedOptions = { beforeFree: async () => void proofs++ };
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
