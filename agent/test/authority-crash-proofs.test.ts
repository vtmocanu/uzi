import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext } from "../src/git.js";
import { agentRoot, makeTmp, testLockOptions, utilLinux } from "./authority-test-support.js";
import type { Barrier, ChildConfig } from "./fixtures/authority-crash-child.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed } from "./helpers.js";

// issue #3404 M4 - the named mandatory production-dispatch crash proofs. Each proof runs the PRODUCTION
// operation (GitCache over the real AuthorityRuntime and the real durable authority store) inside a
// separate OS process that holds W, and SIGKILLs that process at a deterministic barrier. The barriers are
// FIFOs, never sleeps; the injected seams only delay when the REAL rm / `git update-ref -d` child acts
// (see fixtures/authority-crash-child.ts). A successor is then built in this process over the same data
// directory and the observable end states (filesystem, refs, the durable slot) are asserted.
//
// Host note: the production flock trust check wants util-linux. The argument adapter of
// test/authority-test-support.ts (around the SAME kernel flock) is used only when the host flock is not
// util-linux (e.g. BusyBox); on CI, where util-linux is present, the production path runs unadapted. The
// trust check itself is asserted by test/authority-lock.test.ts. This file was also verified under util-linux
// 2.38.1 in a node:24.20.0-bookworm container.

const TOKEN = "m4-crash-proof-join-token";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const childScript = path.join(agentRoot, "test", "fixtures", "authority-crash-child.ts");

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}

// ---- process and barrier plumbing ----------------------------------------------------------

interface Child {
  proc: ChildProcess;
  /** Next stdout line; rejects if the process exits first. */
  line(): Promise<string>;
  exited: Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  /** SIGKILL the owning worker and wait for the kernel to report it killed by that signal. */
  kill(): Promise<void>;
  /** Every stdout line the worker has printed, in order (the queue `line()` consumes is a subset). */
  seen: string[];
}

function spawnChild(t: TestContext, config: ChildConfig): Child {
  const proc = spawn(process.execPath, ["--import", "tsx", childScript, JSON.stringify(config)], {
    cwd: agentRoot, env: ENV, stdio: ["pipe", "pipe", "inherit"],
  });
  const exited = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) =>
    proc.once("close", (code, signal) => resolve({ code, signal })));
  const queue: string[] = [];
  const waiters: Array<{ resolve(line: string): void; reject(error: Error): void }> = [];
  const seen: string[] = [];
  let buffer = "";
  proc.stdout!.on("data", (chunk: Buffer) => {
    buffer += chunk.toString("utf8");
    for (let i = buffer.indexOf("\n"); i >= 0; i = buffer.indexOf("\n")) {
      const next = buffer.slice(0, i);
      buffer = buffer.slice(i + 1);
      seen.push(next);
      const waiter = waiters.shift();
      if (waiter) waiter.resolve(next); else queue.push(next);
    }
  });
  let closed = false;
  void exited.then(({ code, signal }) => {
    closed = true;
    for (const waiter of waiters.splice(0)) waiter.reject(new Error(`child exited (${code ?? signal}) before reporting`));
  });
  t.after(() => { if (proc.exitCode === null && proc.signalCode === null) proc.kill("SIGKILL"); });
  return {
    proc, exited, seen,
    line: () => queue.length > 0 ? Promise.resolve(queue.shift()!)
      : closed ? Promise.reject(new Error("child exited before reporting"))
      : new Promise<string>((resolve, reject) => waiters.push({ resolve, reject })),
    kill: async () => {
      proc.kill("SIGKILL");
      const result = await exited;
      assert.equal(result.signal, "SIGKILL", "the owning worker died by SIGKILL, not a cooperative exit");
    },
  };
}

interface Rig extends Barrier { dir: string }

function makeBarrier(t: TestContext): Rig {
  const dir = makeTmp(t, "m4-barrier-");
  const rig = { dir, armed: path.join(dir, "armed"), go: path.join(dir, "go"), done: path.join(dir, "done"), pid: path.join(dir, "pid") };
  for (const fifo of [rig.armed, rig.go, rig.done]) execFileSync("mkfifo", [fifo]);
  return rig;
}

/** Block until the helper (the real rm / git child, wrapped in the barrier) reports it is waiting; returns its pid. */
async function armed(rig: Rig, worker: Child): Promise<number> {
  const said = await Promise.race([
    fsp.readFile(rig.armed, "utf8"),
    worker.exited.then(({ code, signal }) => { throw new Error(`worker exited (${code ?? signal}) before the barrier was armed`); }),
  ]);
  assert.equal(said.trim(), "armed");
  const pid = Number(fs.readFileSync(rig.pid, "utf8").trim());
  assert.ok(Number.isInteger(pid) && pid > 1);
  return pid;
}

/** Release the barrier and wait for the helper to report its exit status. */
async function release(rig: Rig): Promise<string> {
  await fsp.writeFile(rig.go, "go\n");
  return (await fsp.readFile(rig.done, "utf8")).trim();
}

function alive(pid: number): boolean {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3) !== "Z";
  } catch { return false; }
}

/** The helper has reported its status, so its `sh` is about to exit; wait for the kernel to say so. The helper
 * is an orphan of the SIGKILLed worker (reparented to init), so there is no exit event to subscribe to; this
 * polls the kernel's view against a generous wall-clock deadline (not an iteration count, not an ordering sleep).
 * The assertion is on the loop's own last observation, never a fresh /proc read: a later read (a reused pid, a
 * transient /proc result) must not contradict the observation that ended the loop (issue #3625). Accepted limit:
 * alive() reads any /proc error as gone, so one spurious "not alive" reading passes; pid identity binding is
 * deliberately out of scope unless that recurs. */
async function gone(pid: number, observe: (pid: number) => boolean = alive, budgetMs = 60_000): Promise<void> {
  const deadline = Date.now() + budgetMs;
  let live = observe(pid);
  while (live && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
    live = observe(pid);
  }
  assert.equal(live, false, "the helper process is gone");
}

/** Unblock a helper a failed test left parked on its FIFO, by the exact pid the helper wrote. */
function reapHelper(t: TestContext, rig: Rig): void {
  t.after(() => {
    try {
      const pid = Number(fs.readFileSync(rig.pid, "utf8").trim());
      if (Number.isInteger(pid) && pid > 1 && alive(pid)) process.kill(pid, "SIGKILL");
    } catch { /* never armed */ }
  });
}

async function slotOf(rt: AuthorityRuntime, target: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, target));
  });
}

/** A successor worker incarnation over the same data directory: takes W (the dead owner's lock was released by the kernel) and A. */
async function successor(dataDir: string): Promise<{ rt: AuthorityRuntime; git: GitCache }> {
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 20_000 });
  const git = new GitCache(dataDir, nullLogger(), undefined, { authority: rt.hooks, authorityRequired: true });
  return { rt, git };
}

function treeDigest(root: string): string {
  const hash = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const full = path.join(dir, name);
      const st = fs.lstatSync(full);
      hash.update(`${path.relative(root, full)}\0${st.mode}\0${st.size}\0${st.mtimeMs}\0`);
      if (st.isDirectory()) walk(full);
      else if (st.isFile()) hash.update(fs.readFileSync(full));
    }
  };
  walk(root);
  return hash.digest("hex");
}

// ---- proof 1: detached destructive helper after owner death --------------------------------

const RUN_A = "run-m4-a";
const RUN_B = "run-m4-b";
const BR_A = "agent/issue-5101";
const BR_B = "agent/issue-5102";

async function twoClones(t: TestContext): Promise<{ dataDir: string; origin: string; bare: string; a: string; b: string }> {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const a = (await plain.runnerCloneForBranch(bare, BR_A, "issue-5101", noProofReseed, RUN_A)).path;
  const b = (await plain.runnerCloneForBranch(bare, BR_B, "issue-5102", noProofReseed, RUN_B)).path;
  fs.writeFileSync(path.join(a, "owner-data-a.txt"), "A bytes\n");
  fs.writeFileSync(path.join(b, "owner-data-b.txt"), "B bytes\n");
  return { dataDir, origin: fx.originPath, bare, a, b };
}

const reseedA = (git: GitCache, bare: string) => git.runnerCloneForBranch(bare, BR_A, "issue-5101", noProofReseed, RUN_A);
const reseedB = (git: GitCache, bare: string) => git.runnerCloneForBranch(bare, BR_B, "issue-5102", noProofReseed, RUN_B);

test("gone() asserts its last observation, not a fresh /proc read", async () => {
  // The loop exits on "not alive"; every later observation says alive, as a reused pid would (issue #3625).
  let calls = 0;
  await gone(1, () => calls++ > 0);
  assert.equal(calls, 1, "the observation that ended the loop is the one asserted");
});

test("gone() still fails when the pid stays alive until its deadline", async () => {
  await assert.rejects(gone(1, () => true, 50), /the helper process is gone/);
});

test("proof 1: a detached runner-uid delete outlives its SIGKILLed owner; the successor refuses its footprint until an owner settlement", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a, b } = await twoClones(t);
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "delete", dataDir, token: TOKEN, bare, barrier: rig,
    branch: BR_A, key: "issue-5101", runId: RUN_A });
  reapHelper(t, rig);
  assert.equal(await worker.line(), "W", "the child holds the worker lifetime lock");
  const helperPid = await armed(rig, worker);
  assert.ok(alive(helperPid), "the real detached rm helper exists and waits at the barrier before walking");
  assert.ok(fs.existsSync(path.join(a, "owner-data-a.txt")), "nothing has been deleted yet");

  await worker.kill(); // only the owner; the helper is detached and keeps its FIFO
  assert.ok(alive(helperPid), "the detached helper survived its owner's SIGKILL");

  const { rt, git } = await successor(dataDir); // W and A are acquirable: the dead owner's flock went with it
  const before = await slotOf(rt, a);
  assert.equal(before?.pending?.status, "running", "the dead owner's destructive record is durable and unresolved");
  assert.equal(before?.pending?.kind, "destructive");
  assert.match(before?.pending?.footprint ?? "", /issue-5101/);

  // Overlapping reuse / removal of the footprint: the production reseed (free + seed) refuses before touching it.
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
  assert.equal(fs.readFileSync(path.join(a, "owner-data-a.txt"), "utf8"), "A bytes\n", "the refused successor deleted nothing");

  // An unaffected footprint progresses on the same bare.
  const reseededB = await rt.withTransaction(() => reseedB(git, bare));
  assert.equal(reseededB.path, b);
  assert.equal(fs.existsSync(path.join(b, "owner-data-b.txt")), false, "B was really freed and reseeded");
  assert.ok(fs.existsSync(path.join(b, ".git")));

  // The helper finishes its real walk. That alone is not an owner settlement.
  assert.equal(await release(rig), "0");
  await gone(helperPid);
  assert.equal(fs.existsSync(a), false, "the real rm deleted the footprint after the owner died");
  const after = await slotOf(rt, a);
  assert.equal(after?.pending?.status, "running", "helper exit does not by itself clear the durable record");
  assert.equal(after?.state, "active");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
  assert.equal(fs.existsSync(a), false, "and the refused reseed created nothing");
});

test("proof 1 control: a normally joined, fully accounted delete lets the successor proceed", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a } = await twoClones(t);
  const worker = spawnChild(t, { scenario: "delete", dataDir, token: TOKEN, bare, barrier: null,
    branch: BR_A, key: "issue-5101", runId: RUN_A });
  assert.equal(await worker.line(), "W");
  const finished = await worker.line();
  assert.match(finished, /^DONE:\{"ended":\["[^"]*issue-5101"\],"kept":\[\],"endedSlots":\[\{[^\]]*"path":"[^"]*issue-5101"\}\]\}$/);
  await worker.kill();
  const { rt, git } = await successor(dataDir);
  const slot = await slotOf(rt, a);
  assert.equal(slot?.pending, null, "the delete settled ok and left no pending record");
  assert.equal(slot?.state, "ineligible");
  const reseeded = await rt.withTransaction(() => reseedA(git, bare));
  assert.equal(reseeded.path, a);
  assert.equal(fs.existsSync(path.join(a, "owner-data-a.txt")), false);
  assert.ok(fs.existsSync(path.join(a, ".git")));
});


// ---- proof 2: same-SHA shared-ref ABA after owner death -------------------------------------

const OWED_RUN = "19241924-1111-2222-3333-444444444444";
const OWED_BR = "agent/issue-1924";

interface Generation {
  dataDir: string;
  bare: string;
  context: PositiveOwedCandidateContext;
  pinned: string;
  coverage: { sha: string; fingerprint: string };
  coverageRef: string;
  pinRef: string;
  config: Pick<ChildConfig, "context" | "frozen" | "coveragePins" | "recoverySources" | "coveringSha">;
}

/** A recovery generation with an owed candidate pin, a coverage ref and a recovery pin, all in the real bare. */
async function recoveryGeneration(t: TestContext): Promise<Generation> {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const dir = (await plain.runnerCloneForBranch(bare, OWED_BR, "issue-1924", noProofReseed, OWED_RUN)).path;
  const tree = gitIn(dir, ["rev-parse", `${base}^{tree}`]);
  const pinned = gitIn(dir, [...IDENT, "commit-tree", tree, "-m", "pinned", "-p", base]);
  gitIn(dir, ["update-ref", `refs/heads/${OWED_BR}`, pinned]);
  const opts: FetchAgentBranchOptions = { context: { runId: OWED_RUN, generation: 1, branch: OWED_BR, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
  assert.equal((await plain.fetchAgentBranch(bare, dir, OWED_BR, OWED_RUN, opts)).kind, "updated");
  const context = opts.context as PositiveOwedCandidateContext;
  const frozen = await plain.enumerateOwedCandidates(bare, OWED_RUN);
  const coverage = await plain.buildRecoveryCoverage(bare, context, [pinned], pinned);
  const pinRef = `refs/uzi-recovery-pin/${OWED_RUN}/1`;
  gitIn(bare, ["update-ref", pinRef, coverage.sha]);
  const coverageRef = `refs/uzi-coverage/${OWED_RUN}/1/${coverage.fingerprint}`;
  assert.equal(gitIn(bare, ["rev-parse", coverageRef]), coverage.sha);
  return { dataDir, bare, context, pinned, coverage, coverageRef, pinRef, config: {
    context, frozen: frozen as unknown as ChildConfig["frozen"],
    coveragePins: [{ fingerprint: coverage.fingerprint, sha: coverage.sha }],
    recoverySources: [pinned, coverage.sha], coveringSha: coverage.sha,
  } };
}

const refSha = (bare: string, ref: string): string => gitIn(bare, ["for-each-ref", "--format=%(objectname)", ref]);

// UZI_M4_ABA_MUTANT=1 is set only against the disposable mutation export that bypasses assertBareClear's
// pending-footprint check. The same scenario then witnesses the ABA the guard prevents; without it, the
// production behaviour is asserted. The exact one-line bypass (agent/src/authority-runtime.ts #assertBareClear):
//   -    if (slot?.pending?.status === "running") {
//   +    if (false && slot?.pending?.status === "running") {
const ABA_MUTANT = process.env.UZI_M4_ABA_MUTANT === "1";

test("proof 2: a delayed expected-SHA update-ref -d outlives its SIGKILLed owner; renewal of the same ref to the same SHA refuses", { timeout: 120_000 }, async (t) => {
  const g = await recoveryGeneration(t);
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "cleanup", dataDir: g.dataDir, token: TOKEN, bare: g.bare, barrier: rig,
    pauseAtDelete: 1, ...g.config });
  reapHelper(t, rig);
  assert.equal(await worker.line(), "W");
  const helperPid = await armed(rig, worker);
  assert.equal(refSha(g.bare, g.coverageRef), g.coverage.sha, "the real update-ref -d child has not acted yet");

  await worker.kill();
  assert.ok(alive(helperPid), "the delayed git child survived its owner's SIGKILL");

  const { rt, git } = await successor(g.dataDir);
  const slot = await slotOf(rt, g.bare);
  assert.equal(slot?.pending?.status, "running");
  assert.equal(slot?.pending?.kind, "bare_mutation");
  assert.match(slot?.pending?.footprint ?? "", new RegExp(`recovery-cleanup-delete-ref: ${g.coverageRef}`));

  // The production authorized renewal of that very ref to that very SHA.
  let renewed: { sha: string } | undefined;
  let refusal: unknown;
  try { renewed = await git.buildRecoveryCoverage(g.bare, g.context, [g.pinned], g.pinned); }
  catch (error) { refusal = error; }
  const anchored = await git.anchorRecoveryHead(g.bare, OWED_RUN, 7, g.pinned);

  assert.equal(await release(rig), "0");
  await gone(helperPid);
  const survivors = refSha(g.bare, g.coverageRef);

  if (ABA_MUTANT) {
    // Witness of the regression the guard prevents: renewal "succeeds" on top of the pending deletion, then
    // the delayed child really deletes the ref the successor believes it re-pinned.
    assert.equal(refusal, undefined, "mutant: renewal was admitted over the unresolved bare record");
    assert.equal(anchored, true, "mutant: the recovery-pin anchor renewal was admitted too");
    assert.equal(renewed?.sha, g.coverage.sha);
    assert.equal(survivors, "", "mutant: the released delayed child deleted the renewed ref");
    return;
  }
  assert.match(String((refusal as Error | undefined)?.message), /recovery-coverage-ref: an unresolved bare_mutation \(.*\) covers this bare/);
  assert.equal(anchored, false, "the recovery-pin anchor renewal refuses too");
  assert.equal(gitIn(g.bare, ["for-each-ref", `refs/uzi-recovery-pin/${OWED_RUN}/7`]), "", "no refused renewal created a ref");
  // The helper's real expected-SHA delete completed after the owner died; that is not an owner settlement.
  assert.equal(survivors, "", "the delayed child really deleted the ref");
  assert.equal(refSha(g.bare, g.pinRef), g.coverage.sha, "refs the dead owner never reached are untouched");
  const after = await slotOf(rt, g.bare);
  assert.equal(after?.pending?.status, "running", "helper exit and PID absence do not clear the durable bare record");
  await assert.rejects(git.buildRecoveryCoverage(g.bare, g.context, [g.pinned], g.pinned), /unresolved bare_mutation/);
  assert.equal(refSha(g.bare, g.coverageRef), "", "the second refused renewal recreated nothing");
});

test("proof 2 control: a normally joined delete lets a later renewal and cleanup proceed", { timeout: 120_000 }, async (t) => {
  const g = await recoveryGeneration(t);
  const worker = spawnChild(t, { scenario: "cleanup", dataDir: g.dataDir, token: TOKEN, bare: g.bare, barrier: null, ...g.config });
  assert.equal(await worker.line(), "W");
  assert.equal(await worker.line(), "DONE:removed");
  await worker.kill();
  assert.equal(refSha(g.bare, g.coverageRef), "");
  assert.equal(refSha(g.bare, g.pinRef), "");

  const { rt, git } = await successor(g.dataDir);
  const slot = await slotOf(rt, g.bare);
  assert.equal(slot?.pending, null, "every deletion was joined and settled ok");
  const renewed = await git.buildRecoveryCoverage(g.bare, g.context, [g.pinned], g.pinned);
  assert.equal(renewed.sha, g.coverage.sha);
  assert.equal(refSha(g.bare, g.coverageRef), g.coverage.sha, "the renewal re-created the ref");
  assert.equal(await git.anchorRecoveryHead(g.bare, OWED_RUN, 7, g.pinned), true);
  assert.equal(refSha(g.bare, `refs/uzi-recovery-pin/${OWED_RUN}/7`), g.pinned);
  // ... and the successor's own cleanup of the renewed refs goes through its own hooks.
  const frozen = await git.enumerateOwedCandidates(g.bare, OWED_RUN);
  const cleaned = await rt.withTransaction(() => git.cleanupRecoveryGeneration(g.bare, g.context, frozen,
    [{ fingerprint: g.coverage.fingerprint, sha: g.coverage.sha }], [g.pinned, g.coverage.sha], g.coverage.sha));
  assert.equal(cleaned, "removed");
  assert.equal(refSha(g.bare, g.coverageRef), "");
  assert.equal((await slotOf(rt, g.bare))?.pending, null);
});

// ---- proof 3: W two-process enforcement before any protected effect --------------------------

/** Run a second owner-candidate for `dataDir` as written (any spelling) and report its first stdout line and exit. */
async function contend(t: TestContext, dataDir: string, bare: string, rig: Rig): Promise<{ first: string; exit: Awaited<Child["exited"]> }> {
  const p2 = spawnChild(t, { scenario: "delete", dataDir, token: TOKEN, bare, barrier: rig, branch: BR_A, key: "issue-5101", runId: RUN_A });
  const first = await p2.line().catch((error: Error) => `EXIT:${error.message}`);
  return { first, exit: await p2.exited };
}

test("proof 3: while P1 holds W, P2 (same dir, symlinked parent, trailing slash) refuses before any protected effect; after SIGKILL P2 acquires W but P1's durable record still refuses the footprint", { timeout: 180_000 }, async (t) => {
  const { dataDir, bare, a } = await twoClones(t);
  const rig1 = makeBarrier(t);
  const rig2 = makeBarrier(t);
  reapHelper(t, rig1);
  reapHelper(t, rig2);
  const p1 = spawnChild(t, { scenario: "delete", dataDir, token: TOKEN, bare, barrier: rig1, branch: BR_A, key: "issue-5101", runId: RUN_A });
  assert.equal(await p1.line(), "W", "P1 holds W");
  const helperPid = await armed(rig1, p1);

  // Alternate spellings of the very same directory.
  const base = path.dirname(dataDir);
  const linkDir = makeTmp(t, "m4-alias-");
  const link = path.join(linkDir, "base-link");
  fs.symlinkSync(base, link);
  const spellings = {
    "the same spelling": dataDir,
    "a symlinked parent": path.join(link, path.basename(dataDir)),
    "a trailing slash": `${dataDir}/`,
    "a dot segment": `${dataDir}/./`,
  };
  const frozenDigest = treeDigest(base);
  const frozenFiles = treeDigest(path.join(dataDir, ".uzi-authority"));
  for (const [label, spelled] of Object.entries(spellings)) {
    const { first, exit } = await contend(t, spelled, bare, rig2);
    assert.match(first, /^FAILED:/, `${label}: P2 refused`);
    assert.equal(exit.code, 3, `${label}: P2 exited through its refusal path`);
    assert.equal(fs.existsSync(rig2.pid), false, `${label}: no destructive helper was spawned`);
    assert.equal(treeDigest(base), frozenDigest, `${label}: P2 changed nothing under the data root (files, modes, sizes, mtimes)`);
    assert.equal(treeDigest(path.join(dataDir, ".uzi-authority")), frozenFiles, `${label}: no authority file was written`);
    assert.ok(alive(helperPid) && fs.existsSync(path.join(a, "owner-data-a.txt")));
    t.diagnostic(`${label}: ${first}`);
  }
  // Only the genuinely identical-inode spellings can reach the kernel lock; the others are refused as spelling errors.
  const same = await contend(t, dataDir, bare, rig2);
  assert.match(same.first, /already holds the worker lifetime lock/);

  await p1.kill();
  assert.ok(alive(helperPid), "P1's helper outlives it");

  // P2 as its own process: it now acquires W (the kernel released P1's) and reaches the production delete,
  // which the durable record P1 left refuses before the helper is ever spawned.
  const p2 = spawnChild(t, { scenario: "delete", dataDir, token: TOKEN, bare, barrier: rig2, branch: BR_A, key: "issue-5101", runId: RUN_A });
  assert.equal(await p2.line(), "W", "P2 acquired W after P1's SIGKILL");
  assert.match(await p2.line(), /^FAILED:.*unresolved pending footprint/);
  assert.equal(fs.existsSync(rig2.pid), false, "no helper was spawned for the refused footprint");
  assert.equal(fs.readFileSync(path.join(a, "owner-data-a.txt"), "utf8"), "A bytes\n", "the footprint is intact");
  assert.equal(await release(rig1), "0");
  await gone(helperPid);
});

// ---- issue #4066: the worker shutdown barrier --------------------------------------------------
//
// The owning worker is the same real process as above, now with a SIGTERM path that mirrors main.ts (see
// fixtures/authority-crash-child.ts). Every pause is a FIFO; the shutdown budget and the idle-drain lock wait are small
// injected values, never real 20/30/35 second waits. Settlements come from the production runner-uid delete (the real
// rm held behind a FIFO until it times out into RunnerDeleteUnsettledError) and from the production
// cleanupArchivedRecoveryPin (its real `git update-ref --stdin` child held behind a FIFO), over the real AuthorityRuntime.
// A successor then reads the durable slot the exited worker left.

/** Read stdout lines until one satisfies `done`; returns that line. Rejects if the worker exits first. */
async function until(worker: Child, done: (line: string) => boolean): Promise<string> {
  for (;;) {
    const line = await worker.line();
    if (done(line)) return line;
  }
}

function makeFifo(t: TestContext): string {
  const fifo = path.join(makeTmp(t, "m4066-fifo-"), "fifo");
  execFileSync("mkfifo", [fifo]);
  return fifo;
}

/** Release a barrier without waiting for the helper's exit status (the git wrapper execs git, so it reports none). */
async function releaseGo(rig: Rig): Promise<void> {
  await fsp.writeFile(rig.go, "go\n");
}

/** Try to take A on `dataDir` from a separate process (W is held by the worker, so A is probed at the kernel lock). */
function probeAuthorityLock(dataDir: string): { acquired: boolean; status: number | null } {
  const file = path.join(dataDir, ".uzi-authority", "authority.lock");
  const result = spawnSync("/usr/bin/flock", utilLinux ? ["-x", "-w", "0.2", file, "true"] : ["-x", "-n", file, "true"], { encoding: "utf8" });
  return { acquired: result.status === 0, status: result.status };
}

const GATE_LATE = { rmTimeoutMs: 100, shutdownBudgetMs: 5_000 } as const;

/** A late-queue worker over a fresh clone pair; returns once the worker reports READY. */
async function lateWorker(t: TestContext, extra: Partial<ChildConfig>): Promise<{
  dataDir: string; bare: string; a: string; b: string; worker: Child; rig1: Rig; rig2: Rig;
}> {
  const { dataDir, bare, a, b } = await twoClones(t);
  const rig1 = makeBarrier(t);
  const rig2 = makeBarrier(t);
  reapHelper(t, rig1);
  reapHelper(t, rig2);
  const worker = spawnChild(t, { scenario: "late-queue", dataDir, token: TOKEN, bare, barrier: rig1, barrier2: rig2,
    branch: BR_A, key: "issue-5101", runId: RUN_A, branch2: BR_B, key2: "issue-5102", runId2: RUN_B, ...GATE_LATE, ...extra });
  await until(worker, (line) => line === "READY");
  assert.ok(worker.seen.some((line) => line.startsWith("UNSETTLED:")), "the production runner-uid delete timed out into an unsettled record");
  return { dataDir, bare, a, b, worker, rig1, rig2 };
}

/** What the successor reads of `target` after the worker exited: the durable slot. */
const persistedSuccessors = new Map<string, ReturnType<typeof successor>>();
/** One successor per data dir: its W handle lives as long as this test process, so a second acquisition would refuse. */
function persistedSuccessor(dataDir: string): ReturnType<typeof successor> {
  let opened = persistedSuccessors.get(dataDir);
  if (opened === undefined) { opened = successor(dataDir); persistedSuccessors.set(dataDir, opened); }
  return opened;
}
async function persistedSlot(dataDir: string, target: string): Promise<SlotSnapshot | null> {
  const { rt } = await persistedSuccessor(dataDir);
  return slotOf(rt, target);
}

const sigterm = async (worker: Child): Promise<void> => {
  worker.proc.kill("SIGTERM");
  await until(worker, (line) => line === "SIGTERM-HANDLER");
};

function resultOf(worker: Child): { result: string; exitCode: number } | undefined {
  const line = worker.seen.find((l) => l.startsWith("RESULT:"));
  return line === undefined ? undefined : JSON.parse(line.slice("RESULT:".length)) as { result: string; exitCode: number };
}

test("issue #4066 (a1): a settlement queued while the drain is paused after its splice is applied before a SIGTERM exit", { timeout: 120_000 }, async (t) => {
  const holdGo = makeFifo(t);
  const { dataDir, a, b, worker, rig1, rig2 } = await lateWorker(t, { lateMode: "a1", holdGo, lateWaitMs: 5_000 });
  await armed(rig1, worker);
  await armed(rig2, worker);
  assert.equal(await release(rig1), "0", "the first held rm exits");
  await until(worker, (line) => line === "SPLICED"); // the drain took the first late close out of the queue and is paused
  assert.equal(await release(rig2), "0", "the second held rm exits while the drain is paused");
  await until(worker, (line) => line === "CLOSED:2"); // its late close is queued behind the spliced one
  await sigterm(worker);
  await fsp.writeFile(holdGo, "go\n"); // the paused drain continues; its spliced entry is applied, the queued one is not
  const exit = await worker.exited;

  const slotB = await persistedSlot(dataDir, b);
  assert.notEqual(slotB?.pending?.status, "running", "the settlement queued during the paused drain is durable before the worker exits");
  const slotA = await persistedSlot(dataDir, a);
  assert.notEqual(slotA?.pending?.status, "running", "the spliced settlement is durable too");
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
});

test("issue #4066 (a2): a late settlement queued behind a raw A holder is applied before a SIGTERM exit", { timeout: 120_000 }, async (t) => {
  const holdGo = makeFifo(t);
  const { dataDir, a, worker, rig1 } = await lateWorker(t, { lateMode: "a2", holdGo, lateWaitMs: 200 });
  assert.ok(worker.seen.includes("HOLDING-A"), "a raw withAuthorityTransaction holder owns A");
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "QUEUED-WARN"); // the idle drain's bounded wait for A expired: the settlement stays queued
  await sigterm(worker);
  await fsp.writeFile(holdGo, "go\n"); // the raw holder releases
  const exit = await worker.exited;

  const slot = await persistedSlot(dataDir, a);
  assert.notEqual(slot?.pending?.status, "running", "the queued settlement is durable before the worker exits");
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
});

test("issue #4066 (a3): a settlement whose apply failed once and was re-queued is applied before a SIGTERM exit", { timeout: 120_000 }, async (t) => {
  const { dataDir, a, worker, rig1 } = await lateWorker(t, { lateMode: "a3" });
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "REQUEUED"); // the first apply failed (one injected rename failure) and was queued again
  await sigterm(worker);
  const exit = await worker.exited;

  const slot = await persistedSlot(dataDir, a);
  assert.notEqual(slot?.pending?.status, "running", "the re-queued settlement is durable before the worker exits");
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
});

test("issue #4066 (c): a late close that fires inside an open transaction keeps A held until its settlement write finishes", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a } = await twoClones(t);
  const rig = makeBarrier(t);
  reapHelper(t, rig);
  const writeGo = makeFifo(t);
  const worker = spawnChild(t, { scenario: "late-close-inline", dataDir, token: TOKEN, bare, barrier: rig, writeGo,
    branch: BR_A, key: "issue-5101", runId: RUN_A, rmTimeoutMs: 100, shutdownBudgetMs: 1_000 });
  await until(worker, (line) => line === "CALLBACK-RETURNED");
  assert.ok(worker.seen.includes("ARMED-WRITE"), "the late close fired in the open transaction and its slot write is paused before rename");
  assert.ok(!worker.seen.includes("INLINE-NOT-REACHED"));
  await sigterm(worker);

  // (ii) A is still held by the paused apply: the transaction's callback returned, but its lock lifetime is the apply's.
  const probe = probeAuthorityLock(dataDir);
  assert.equal(probe.acquired, false, `A is still held while the inline settlement write is paused (flock status ${probe.status})`);

  // (i) the observer reports the incomplete shutdown while the write is paused; nothing reports a clean shutdown.
  const logged = await until(worker, (line) => line.startsWith("LOG:") && line.includes("worker_shutdown_result"));
  const decision = JSON.parse(logged.slice("LOG:".length)) as { fields: { result: string; reasons: string[] } };
  assert.equal(decision.fields.result, "incomplete");
  assert.ok(decision.fields.reasons.some((r) => r === "inline_apply_in_flight" || r === "transaction_in_flight"), `reasons: ${decision.fields.reasons.join(",")}`);
  assert.equal(resultOf(worker), undefined, "no clean shutdown was reported while the write is paused");
  assert.equal(worker.proc.exitCode, null, "the worker is still alive");

  // (iii) releasing the write barrier lets the settlement become durable; the shutdown still exits 1.
  await fsp.writeFile(writeGo, "go\n");
  const exit = await worker.exited;
  const slot = await persistedSlot(dataDir, a);
  assert.equal(slot?.pending?.status, "settled_failed");
  assert.ok((slot?.pending?.residue ?? []).length > 0, "the failed delete's residue is recorded");
  assert.equal(exit.code, 1);
});

test("issue #4066 (c) SIGKILL variant: killed while the inline settlement write is paused before rename, the successor still refuses the footprint", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a } = await twoClones(t);
  const rig = makeBarrier(t);
  reapHelper(t, rig);
  const writeGo = makeFifo(t);
  const worker = spawnChild(t, { scenario: "late-close-inline", dataDir, token: TOKEN, bare, barrier: rig, writeGo,
    branch: BR_A, key: "issue-5101", runId: RUN_A, rmTimeoutMs: 100, shutdownBudgetMs: 1_000 });
  await until(worker, (line) => line === "CALLBACK-RETURNED");
  assert.ok(worker.seen.includes("ARMED-WRITE"));
  await worker.kill();
  const { rt, git } = await successor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running", "the rename never happened: the durable record is still running");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
});

test("issue #4066 (b): a held recovery-pin continuation is joined before the seal, and admission after the seal is refused", { timeout: 120_000 }, async (t) => {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const sourceSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const runId = "40664066-1111-2222-3333-444444444444";
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);
  const rig = makeBarrier(t);
  reapHelper(t, rig);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 10_000,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, deadlineMs: 3_000 } });
  await armed(rig, worker); // the real `git update-ref --stdin` child exists and is held before Git runs
  await until(worker, (line) => line === "DEADLINE-PASSED"); // the caller's deadline passed with the child in flight
  await sigterm(worker);
  assert.ok(!worker.seen.some((line) => line === "PHASE:sealed" || line === "PHASE:final_drain_started"),
    "nothing sealed or drained while the pin child is held");
  await releaseGo(rig); // within the abort grace: Git sees prepare then abort and exits by itself
  const exit = await worker.exited;

  const phases = worker.seen.filter((line) => line.startsWith("PHASE:"));
  assert.deepEqual(phases, ["PHASE:prerequisites_joined", "PHASE:sealed", "PHASE:final_drain_started"]);
  assert.ok(worker.seen.includes("ADMISSION:AuthorityShutdownRefusal"), "a new admission after the seal is refused");
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
  const slot = await persistedSlot(dataDir, bare);
  assert.notEqual(slot?.pending?.status, "running", "the pin continuation settled its record");
  assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-recovery-pin/${runId}/1`]), sourceSha, "the aborted delete left the recovery pin on its source");
});

test("issue #4066 (b): a held unsettled runner-uid delete is joined before the seal; its late close is applied by the final drain", { timeout: 120_000 }, async (t) => {
  const { dataDir, a, worker, rig1 } = await lateWorker(t, { lateMode: "b2" });
  await armed(rig1, worker);
  await sigterm(worker);
  assert.ok(!worker.seen.some((line) => line === "PHASE:sealed" || line === "PHASE:final_drain_started"),
    "nothing sealed or drained while the delete is unsettled");
  assert.equal(await release(rig1), "0");
  const exit = await worker.exited;

  const phases = worker.seen.filter((line) => line.startsWith("PHASE:"));
  assert.deepEqual(phases, ["PHASE:prerequisites_joined", "PHASE:sealed", "PHASE:final_drain_started"]);
  assert.ok(worker.seen.includes("ADMISSION:AuthorityShutdownRefusal"));
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
  assert.notEqual((await persistedSlot(dataDir, a))?.pending?.status, "running");
});

test("issue #4066 control: an uninterrupted late close is applied and a SIGTERM exits drained", { timeout: 120_000 }, async (t) => {
  const { dataDir, a, worker, rig1 } = await lateWorker(t, { lateMode: "plain" });
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "CLOSED:1");
  await sigterm(worker);
  const exit = await worker.exited;
  assert.notEqual((await persistedSlot(dataDir, a))?.pending?.status, "running", "the idle drain applied the late close");
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.equal(exit.code, 0);
});

test("issue #4066 control: SIGKILL before the late close leaves the running record and the successor refuses", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a, worker, rig1 } = await lateWorker(t, { lateMode: "plain" });
  await armed(rig1, worker);
  await worker.kill();
  const { rt, git } = await successor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
  assert.equal(await release(rig1), "0"); // the orphaned rm finishes; that alone settles nothing
  assert.equal((await slotOf(rt, a))?.pending?.status, "running");
});


// ---- issue #4066: the controls around the barrier ----------------------------------------------

/** The decisions the worker logged (`worker_shutdown_result`), parsed. */
function decisions(worker: Child): Array<{ level: string; fields: { result: string; reasons: string[]; counts: Record<string, number>; operations: string[] } }> {
  return worker.seen.filter((l) => l.startsWith("LOG:") && l.includes("worker_shutdown_result"))
    .map((l) => JSON.parse(l.slice("LOG:".length)) as ReturnType<typeof decisions>[number]);
}

async function firstDecision(worker: Child): Promise<ReturnType<typeof decisions>[number]> {
  await until(worker, (line) => line.startsWith("LOG:") && line.includes("worker_shutdown_result"));
  return decisions(worker)[0]!;
}

const SHORT_BUDGET = { rmTimeoutMs: 100, shutdownBudgetMs: 700, lateWaitMs: 200 } as const;

test("issue #4066 deadline, holder never released (A): the incomplete line comes first, then SIGKILL leaves the refusal in place", { timeout: 120_000 }, async (t) => {
  const holdGo = makeFifo(t);
  const { dataDir, bare, a, worker, rig1 } = await lateWorker(t, { lateMode: "a2", holdGo, ...SHORT_BUDGET });
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "QUEUED-WARN");
  await sigterm(worker);
  const decision = await firstDecision(worker);
  assert.equal(decision.level, "warn");
  assert.equal(decision.fields.result, "incomplete");
  for (const reason of ["deadline", "prerequisite_pending", "queued_settlement"]) {
    assert.ok(decision.fields.reasons.includes(reason), `reasons: ${decision.fields.reasons.join(",")}`);
  }
  assert.equal(decision.fields.operations.length, 1);
  assert.equal(resultOf(worker), undefined, "no clean result while the holder is unreleased");
  assert.ok(!worker.seen.some((line) => line.startsWith("PHASE:")), "no phase ran: the prerequisite join is still waiting");
  assert.equal(worker.proc.exitCode, null, "the worker is still alive");

  await worker.kill();
  const { rt, git } = await persistedSuccessor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running", "the settlement never became durable");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
});

test("issue #4066 deadline, holder released late (B): the join completes, the queued settlement stays queued, exit 1, the successor still refuses", { timeout: 120_000 }, async (t) => {
  const holdGo = makeFifo(t);
  const { dataDir, bare, a, worker, rig1 } = await lateWorker(t, { lateMode: "a2", holdGo, ...SHORT_BUDGET });
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "QUEUED-WARN");
  await sigterm(worker);
  await firstDecision(worker);
  await fsp.writeFile(holdGo, "go\n"); // the holder releases after the deadline
  const exit = await worker.exited;

  assert.equal(exit.code, 1);
  assert.equal(decisions(worker).length, 1, "decided once: the barrier added no second line");
  assert.deepEqual(resultOf(worker), { result: "incomplete", exitCode: 1 });
  assert.deepEqual(worker.seen.filter((l) => l.startsWith("PHASE:")), ["PHASE:prerequisites_joined", "PHASE:sealed", "PHASE:final_drain_started"]);
  const { rt, git } = await persistedSuccessor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running", "no drain starts past the deadline");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
});

test("issue #4066 deadline, pin continuation pending: the result is logged while it is pending, nothing is sealed, and after the join the late settle stays queued (exit 1)", { timeout: 120_000 }, async (t) => {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const sourceSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const runId = "40664066-5555-6666-7777-888888888888";
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);
  const rig = makeBarrier(t);
  reapHelper(t, rig);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 500,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, deadlineMs: 3_000 } });
  t.after(() => t.diagnostic(`worker output:\n${worker.seen.join("\n")}`));
  await armed(rig, worker);
  await until(worker, (line) => line === "DEADLINE-PASSED");
  await sigterm(worker);
  const decision = await firstDecision(worker);
  assert.equal(decision.fields.result, "incomplete");
  assert.ok(decision.fields.reasons.includes("prerequisite_pending") && decision.fields.reasons.includes("deadline"), decision.fields.reasons.join());
  assert.ok(!worker.seen.some((line) => line === "PHASE:sealed" || line === "PHASE:final_drain_started"), "nothing sealed while the pin child is held");
  assert.equal(worker.proc.exitCode, null, "the worker is alive");

  await releaseGo(rig);
  const exit = await worker.exited;
  assert.equal(exit.code, 1);
  assert.equal(decisions(worker).length, 1);
  assert.deepEqual(worker.seen.filter((l) => l.startsWith("PHASE:")), ["PHASE:prerequisites_joined", "PHASE:sealed", "PHASE:final_drain_started"]);
  assert.equal((await persistedSlot(dataDir, bare))?.pending?.status, "running", "the continuation's settle stayed queued past the deadline");
  assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-recovery-pin/${runId}/1`]), sourceSha, "the aborted delete left the recovery pin on its source");
});

test("issue #4066 unjoined git child: a pin-cleanup child killed by a signal never settles; incomplete with open_settlement, exit 1, the successor still refuses", { timeout: 120_000 }, async (t) => {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const sourceSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const runId = "40664066-9999-aaaa-bbbb-cccccccccccc";
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);
  const rig = makeBarrier(t);
  reapHelper(t, rig);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 2_000,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, deadlineMs: 3_000 } });
  t.after(() => t.diagnostic(`worker output:\n${worker.seen.join("\n")}`));
  const childPid = await armed(rig, worker); // the real `git update-ref --stdin` child, held before Git runs
  await until(worker, (line) => line === "DEADLINE-PASSED");
  await sigterm(worker);
  process.kill(childPid, "SIGKILL"); // killed by a signal: no real exit status, so the continuation cannot settle
  const exit = await worker.exited;

  assert.equal(exit.code, 1);
  const decision = decisions(worker);
  assert.equal(decision.length, 1, "decided once");
  assert.equal(decision[0]!.fields.result, "incomplete");
  assert.ok(decision[0]!.fields.reasons.includes("open_settlement"), `reasons: ${decision[0]!.fields.reasons.join(",")}`);
  assert.deepEqual(resultOf(worker), { result: "incomplete", exitCode: 1 });
  assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-recovery-pin/${runId}/1`]), sourceSha, "the pin still names the original source");
  const { rt, git } = await persistedSuccessor(dataDir);
  assert.equal((await slotOf(rt, bare))?.pending?.status, "running", "the unjoined child's record was never settled");
  assert.equal(await git.anchorRecoveryHead(bare, runId, 7, sourceSha), false, "a successor still refuses the bare");
  assert.equal(gitIn(bare, ["for-each-ref", `refs/uzi-recovery-pin/${runId}/7`]), "", "the refused anchor created no ref");
  assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-recovery-pin/${runId}/1`]), sourceSha);
});

test("issue #4066 permanent durable-write failure: every settlement rename fails; incomplete with apply_exhausted, never drained, exit 1, the successor refuses", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a, worker, rig1 } = await lateWorker(t, { lateMode: "a4" });
  await armed(rig1, worker);
  assert.equal(await release(rig1), "0");
  await until(worker, (line) => line === "REQUEUED"); // the idle drain's apply failed and was queued again
  await sigterm(worker);
  const exit = await worker.exited;

  assert.equal(exit.code, 1);
  const decision = decisions(worker);
  assert.equal(decision.length, 1, "decided once");
  assert.equal(decision[0]!.fields.result, "incomplete");
  assert.ok(decision[0]!.fields.reasons.includes("apply_exhausted"), `reasons: ${decision[0]!.fields.reasons.join(",")}`);
  assert.deepEqual(resultOf(worker), { result: "incomplete", exitCode: 1 });
  assert.ok(!worker.seen.some((line) => line.startsWith("LOG:") && line.includes('"result":"drained"')), "never reported drained");
  const { rt, git } = await persistedSuccessor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running", "no settlement ever became durable");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
});

test("issue #4066 repeated SIGTERM: the second signal changes nothing and the shutdown reports once", { timeout: 120_000 }, async (t) => {
  const { dataDir, a, worker, rig1 } = await lateWorker(t, { lateMode: "b2" });
  await armed(rig1, worker);
  await sigterm(worker);
  worker.proc.kill("SIGTERM");
  await until(worker, (line) => line === "SIGTERM-REPEAT");
  assert.ok(!worker.seen.some((line) => line === "PHASE:sealed"), "still joining the held delete");
  assert.equal(await release(rig1), "0");
  const exit = await worker.exited;
  assert.equal(exit.code, 0);
  assert.equal(worker.seen.filter((l) => l.startsWith("RESULT:")).length, 1);
  assert.equal(decisions(worker).length, 1);
  assert.deepEqual(resultOf(worker), { result: "drained", exitCode: 0 });
  assert.notEqual((await persistedSlot(dataDir, a))?.pending?.status, "running");
});

test("issue #4066 SIGTERM, then SIGKILL before the settlement: the record stays running and the successor refuses", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a, worker, rig1 } = await lateWorker(t, { lateMode: "b2" });
  await armed(rig1, worker);
  await sigterm(worker);
  await worker.kill();
  const { rt, git } = await persistedSuccessor(dataDir);
  assert.equal((await slotOf(rt, a))?.pending?.status, "running");
  await assert.rejects(rt.withTransaction(() => reseedA(git, bare)), /unresolved pending footprint/);
});
