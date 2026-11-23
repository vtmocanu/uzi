import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext } from "../src/git.js";
import { agentRoot, fifoDir, makeTmp, readFifoBounded, testLockOptions, writeFifoBounded } from "./authority-test-support.js";
import type { Barrier, ChildConfig, RefusalHold, RefusalOp } from "./fixtures/authority-crash-child.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

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
  /** Subscribe to every later stdout line without consuming the `line()` queue; returns the unsubscribe. */
  onLine(listener: (line: string) => void): () => void;
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
  const listeners = new Set<(line: string) => void>();
  proc.stdin!.on("error", () => { /* EPIPE: the worker is gone or its stdin is closed */ });
  let buffer = "";
  proc.stdout!.on("data", (chunk: Buffer) => {
    buffer += chunk.toString("utf8");
    for (let i = buffer.indexOf("\n"); i >= 0; i = buffer.indexOf("\n")) {
      const next = buffer.slice(0, i);
      buffer = buffer.slice(i + 1);
      seen.push(next);
      for (const listener of listeners) listener(next);
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
    onLine: (listener) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
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
  const fifos = fifoDir(t, "m4-barrier-", ["armed", "go", "done"], { pidFile: true });
  return { dir: fifos.dir, armed: fifos.path("armed"), go: fifos.path("go"), done: fifos.path("done"), pid: fifos.pid! };
}

/** The wall-clock bound on every wait for a held helper. It only turns a hang into a named failure; nothing waits on it. */
const BARRIER_BOUND_MS = 60_000;

/** Non-consuming wait for the first stdout line (already seen or later) satisfying `pred`. */
function watch(worker: Child, pred: (line: string) => boolean): Promise<string> {
  const earlier = worker.seen.find(pred);
  if (earlier !== undefined) return Promise.resolve(earlier);
  return new Promise<string>((resolve) => {
    const stop = worker.onLine((line) => { if (pred(line)) { stop(); resolve(line); } });
  });
}

/** Block until the helper (the real rm / git child, wrapped in the barrier) reports it is waiting; returns its pid. */
async function armed(rig: Rig, worker: Child, ...raceWith: Promise<never>[]): Promise<number> {
  const said = await readFifoBounded(rig.armed, BARRIER_BOUND_MS, "the armed barrier",
    worker.exited.then(({ code, signal }) => { throw new Error(`worker exited (${code ?? signal}) before the barrier was armed`); }),
    ...raceWith);
  assert.equal(said.trim(), "armed");
  const pid = Number(fs.readFileSync(rig.pid, "utf8").trim());
  assert.ok(Number.isInteger(pid) && pid > 1);
  return pid;
}

/** Release the barrier and wait for the helper to report its exit status. */
async function release(rig: Rig): Promise<string> {
  await writeFifoBounded(rig.go, "go\n", BARRIER_BOUND_MS, "the go barrier");
  return (await readFifoBounded(rig.done, BARRIER_BOUND_MS, "the done barrier")).trim();
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
  assert.ok(finished.startsWith("DONE:"));
  const result: unknown = JSON.parse(finished.slice("DONE:".length));
  await worker.kill();
  const { rt, git } = await successor(dataDir);
  const slot = await slotOf(rt, a);
  assert.ok(slot);
  assert.deepEqual(result, {
    ended: [a], kept: [],
    endedSlots: [{ key: slot.key, epoch: slot.epoch, seq: slot.seq, worker: slot.owner.worker, path: a }],
    reasons: [],
  });
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
  return fifoDir(t, "m4066-fifo-", ["fifo"]).path("fifo");
}

/** Release a barrier without waiting for the helper's exit status (the git wrapper execs git, so it reports none). */
async function releaseGo(rig: Rig): Promise<void> {
  await writeFifoBounded(rig.go, "go\n", BARRIER_BOUND_MS, "the go barrier");
}

/** Try to take A on `dataDir` from a separate process (W is held by the worker, so A is probed at the kernel lock). */
function probeAuthorityLock(dataDir: string): { status: number } {
  const file = path.join(dataDir, ".uzi-authority", "authority.lock");
  // Resolve via PATH. Both util-linux and BusyBox return 1 for a conflict with this common -n form.
  const result = spawnSync("flock", ["-x", "-n", file, "true"], { encoding: "utf8" });
  assert.ifError(result.error);
  assert.equal(result.signal, null, "flock probe must not be killed");
  assert.ok(result.status !== null, "flock probe must produce an exit status");
  assert.ok(result.status === 0 || result.status === 1, `flock probe failed with status ${result.status}: ${result.stderr}`);
  assert.equal(result.stderr, "", "flock probe must not report an execution or usage error");
  return { status: result.status };
}

const GATE_LATE = { rmTimeoutMs: 100, shutdownBudgetMs: 5_000 } as const;

/** A late-queue worker over a fresh clone pair; returns once the worker reports READY. */
async function lateWorker(t: TestContext, extra: Partial<ChildConfig>): Promise<{
  dataDir: string; bare: string; a: string; b: string; worker: Child; rig1: Rig; rig2: Rig;
}> {
  const { dataDir, bare, a, b } = await twoClones(t);
  const rig1 = makeBarrier(t);
  const rig2 = makeBarrier(t);
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
  await writeFifoBounded(holdGo, "go\n", BARRIER_BOUND_MS, "the holdGo FIFO"); // the paused drain continues; its spliced entry is applied, the queued one is not
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
  await writeFifoBounded(holdGo, "go\n", BARRIER_BOUND_MS, "the holdGo FIFO"); // the raw holder releases
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
  const writeGo = makeFifo(t);
  const worker = spawnChild(t, { scenario: "late-close-inline", dataDir, token: TOKEN, bare, barrier: rig, writeGo,
    branch: BR_A, key: "issue-5101", runId: RUN_A, rmTimeoutMs: 100, shutdownBudgetMs: 1_000 });
  await until(worker, (line) => line === "CALLBACK-RETURNED");
  assert.ok(worker.seen.includes("ARMED-WRITE"), "the late close fired in the open transaction and its slot write is paused before rename");
  assert.ok(!worker.seen.includes("INLINE-NOT-REACHED"));
  await sigterm(worker);

  // (ii) A is still held by the paused apply: the transaction's callback returned, but its lock lifetime is the apply's.
  const probe = probeAuthorityLock(dataDir);
  assert.equal(probe.status, 1, "confirmed flock conflict: A is still held while the inline settlement write is paused");

  // (i) the observer reports the incomplete shutdown while the write is paused; nothing reports a clean shutdown.
  const logged = await until(worker, (line) => line.startsWith("LOG:") && line.includes("worker_shutdown_result"));
  const decision = JSON.parse(logged.slice("LOG:".length)) as { fields: { result: string; reasons: string[] } };
  assert.equal(decision.fields.result, "incomplete");
  assert.ok(decision.fields.reasons.some((r) => r === "inline_apply_in_flight" || r === "transaction_in_flight"), `reasons: ${decision.fields.reasons.join(",")}`);
  assert.equal(resultOf(worker), undefined, "no clean shutdown was reported while the write is paused");
  assert.equal(worker.proc.exitCode, null, "the worker is still alive");

  // (iii) releasing the write barrier lets the settlement become durable; the shutdown still exits 1.
  await writeFifoBounded(writeGo, "go\n", BARRIER_BOUND_MS, "the writeGo FIFO");
  const exit = await worker.exited;
  const slot = await persistedSlot(dataDir, a);
  assert.equal(slot?.pending?.status, "settled_failed");
  assert.ok((slot?.pending?.residue ?? []).length > 0, "the failed delete's residue is recorded");
  assert.equal(exit.code, 1);
});

test("issue #4066 (c) SIGKILL variant: killed while the inline settlement write is paused before rename, the successor still refuses the footprint", { timeout: 120_000 }, async (t) => {
  const { dataDir, bare, a } = await twoClones(t);
  const rig = makeBarrier(t);
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

/** A fresh bare with the archived recovery pin of `runId` generation 1 anchored on origin/main. */
async function pinnedBare(t: TestContext, runId: string): Promise<{ dataDir: string; bare: string; sourceSha: string }> {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const sourceSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);
  return { dataDir, bare, sourceSha };
}

/** Like armed(), but a cleanup that settles first fails by name: its update-ref child will then never exist. */
async function armedPin(rig: Rig, worker: Child): Promise<number> {
  const settledFirst = watch(worker, (line) => line.startsWith("CLEANUP:")).then((line): never => {
    throw new assert.AssertionError({ message: `cleanup settled before its update-ref child was armed: ${line}` });
  });
  settledFirst.catch(() => undefined); // the cleanup settles at the end of every run; only the race above may observe it
  return armed(rig, worker, settledFirst);
}

/** The pin cleanup's deadline passes on the worker's manual clock, once its child is held; the worker reports it. */
async function passDeadline(worker: Child): Promise<void> {
  worker.proc.stdin!.write("advance\n");
  await until(worker, (line) => line === "DEADLINE-PASSED");
}

async function heldPinContinuationIsJoined(t: TestContext, runId: string, pin: { deadlineMs: number; eligibleStallMs?: number }): Promise<void> {
  const { dataDir, bare, sourceSha } = await pinnedBare(t, runId);
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 10_000,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, ...pin } });
  await armedPin(rig, worker); // the real `git update-ref --stdin` child exists and is held before Git runs
  await passDeadline(worker); // the caller's deadline passed with the child in flight
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
}

test("issue #4066 (b): a held recovery-pin continuation is joined before the seal, and admission after the seal is refused", { timeout: 120_000 }, async (t) => {
  await heldPinContinuationIsJoined(t, "40664066-1111-2222-3333-444444444444", { deadlineMs: 20_000 });
});

test("issue #4066 (b) controlled variant: a cleanup whose pre-spawn phase outlasts the wall-clock budget still reaches its held child", { timeout: 120_000 }, async (t) => {
  // The first isEligible call (before the child exists) stalls the worker 2.5 s of real time, past the 2 s budget the
  // cleanup would have had on the real clock. The manual clock keeps the deadline in front of it.
  await heldPinContinuationIsJoined(t, "40664066-1111-2222-3333-555555555555", { deadlineMs: 2_000, eligibleStallMs: 2_500 });
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
  await writeFifoBounded(holdGo, "go\n", BARRIER_BOUND_MS, "the holdGo FIFO"); // the holder releases after the deadline
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
  const runId = "40664066-5555-6666-7777-888888888888";
  const { dataDir, bare, sourceSha } = await pinnedBare(t, runId);
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 500,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, deadlineMs: 20_000 } });
  t.after(() => t.diagnostic(`worker output:\n${worker.seen.join("\n")}`));
  await armedPin(rig, worker);
  await passDeadline(worker);
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
  const runId = "40664066-9999-aaaa-bbbb-cccccccccccc";
  const { dataDir, bare, sourceSha } = await pinnedBare(t, runId);
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "pin-cleanup", dataDir, token: TOKEN, bare, barrier: rig, shutdownBudgetMs: 2_000,
    wrapDir: makeTmp(t, "m4066-wrap-"), pin: { runId, generation: 1, sourceSha, deadlineMs: 20_000 } });
  t.after(() => t.diagnostic(`worker output:\n${worker.seen.join("\n")}`));
  const childPid = await armedPin(rig, worker); // the real `git update-ref --stdin` child, held before Git runs
  await passDeadline(worker);
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

// ---- issue #4481: the refusal store under SIGKILL ----------------------------------------------------------------------
//
// The owning worker (scenario "refusal" of fixtures/authority-crash-child.ts) runs the real refusal operations and parks
// itself in-process before or after one real effect (an fs rename of the write-once record, an fs unlink, or a git child);
// the test SIGKILLs it there. Two successors then read the same bare:
//  - the real successor: another process nonce, so every admission or seal the dead worker wrote is foreign to it;
//  - a forensic reader with the dead worker's nonce, which isolates what the on-disk state alone says (the verdict the
//    dead worker itself would reach), so each ordering point is asserted for its own reason and not only for the nonce.
// No refused head may be lost: after every kill each one is still reachable through a uzi ref, or (only once a FINAL
// covering it was sealed) is an ancestor of that covering commit.

const RF_RUN = "44814481-aaaa-bbbb-cccc-444444444444";
const RF_BR = "agent/issue-4481";
const RF_CAP = 6;
const RF_NONCE = "crash-proof-dead-worker-nonce-0001";
const RF_LEDGER = `refusals-${RF_RUN}.json`;

interface RefusalWorld {
  dataDir: string;
  bare: string;
  clone: string;
  base: string;
  ctx: PositiveOwedCandidateContext;
  kept: string[];
  /** Refused-to-be heads: commits in the clone only, never on a ref in the bare until the dead worker imports them. */
  heads: string[];
  /** A commit in the bare whose parents are every kept and refused head: the confirmed FINAL a seal would cover. */
  cover: string;
  /** A child of `cover`: a different source that still covers every head. */
  cover2: string;
  later: string;
  /** The frozen roots the cleanup caller holds: enumerated once, before any crash (production passes its journaled list, never a re-read of refs the dead worker may already have deleted). */
  frozen: Array<{ sha: string; contexts: PositiveOwedCandidateContext[] }>;
}

/** The bare at the owed cap (six kept pins), `headCount` refusable heads, a covering commit, and an optional later-generation ledger entry. */
async function refusalWorld(t: TestContext, headCount: number, laterEntry = false): Promise<RefusalWorld> {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: RF_CAP }));
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const clone = (await plain.runnerCloneForBranch(bare, RF_BR, "issue-4481", noProofReseed, RF_RUN)).path;
  const ctx: PositiveOwedCandidateContext = { runId: RF_RUN, generation: 1, branch: RF_BR, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } };
  let n = 0;
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const commit = (label: string, parents: string[], moveBranch: boolean): string => {
    const sha = gitIn(clone, [...IDENT, "commit-tree", tree, ...parents.flatMap((p) => ["-p", p]), "-m", `${label} ${n++}`]);
    if (moveBranch) gitIn(clone, ["update-ref", `refs/heads/${RF_BR}`, sha]);
    return sha;
  };
  const kept: string[] = [];
  for (let i = 0; i < RF_CAP; i++) {
    kept.push(commit("kept", [base], true));
    assert.equal((await plain.fetchAgentBranch(bare, clone, RF_BR, RF_RUN, { context: ctx })).kind, "updated");
  }
  const heads = Array.from({ length: headCount }, () => commit("refused", [base], false));
  const later = commit("later-generation", [base], false);
  const cover = commit("cover", [...kept, ...heads], false);
  gitIn(clone, ["update-ref", "refs/heads/cover", cover]);
  gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, "+refs/heads/cover:refs/uzi-test/cover"]);
  const cover2 = commit("cover2", [cover], false);
  gitIn(clone, ["update-ref", "refs/heads/cover2", cover2]);
  gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, "+refs/heads/cover2:refs/uzi-test/cover2"]);
  if (laterEntry) {
    const dir = path.join(bare, "info", "uzi-refusals");
    fs.mkdirSync(dir, { mode: 0o700, recursive: true });
    fs.writeFileSync(path.join(dir, RF_LEDGER), JSON.stringify({ version: 1, runId: RF_RUN, branch: RF_BR, barePath: bare,
      entries: [{ generation: 2, sha: later, path: "P2" }] }) + "\n", { mode: 0o600 });
  }
  const frozen = (await plain.enumerateOwedCandidates(bare, RF_RUN)).map((c) => ({ sha: c.sha, contexts: c.contexts as PositiveOwedCandidateContext[] }));
  return { dataDir, bare, clone, base, ctx, kept, heads, cover, cover2, later, frozen };
}

const rfDir = (w: RefusalWorld): string => path.join(w.bare, "info", "uzi-refusals");
const rfFiles = (w: RefusalWorld): string[] => fs.existsSync(rfDir(w)) ? fs.readdirSync(rfDir(w)).sort() : [];
const rfJson = (w: RefusalWorld, name: string): any => JSON.parse(fs.readFileSync(path.join(rfDir(w), name), "utf8"));
const rfRaw = (w: RefusalWorld, name: string): string => fs.readFileSync(path.join(rfDir(w), name), "utf8");
const rfLedgerShas = (w: RefusalWorld): string[] => rfFiles(w).includes(RF_LEDGER) ? rfJson(w, RF_LEDGER).entries.map((e: { sha: string }) => e.sha) : [];
const incomingRefs = (w: RefusalWorld): string[] =>
  gitIn(w.bare, ["for-each-ref", "--format=%(refname) %(objectname)", `refs/uzi-incoming/${RF_RUN}/`]).split("\n").filter(Boolean).sort();
const stagesAt = (w: RefusalWorld, sha: string): string[] => incomingRefs(w).filter((l) => l.endsWith(` ${sha}`) && !l.includes("/refused-"));
const refusedRefAt = (w: RefusalWorld, sha: string): string[] => incomingRefs(w).filter((l) => l.includes(`/refused-${sha} `));
const ownedPins = (w: RefusalWorld): string[] =>
  gitIn(w.bare, ["for-each-ref", "--format=%(refname)", `refs/uzi-owed/${RF_RUN}/`]).split("\n").filter(Boolean);
/** Every uzi ref (outside the test's own covering ref) whose tip contains `sha`. */
const protectedBy = (w: RefusalWorld, sha: string): string[] =>
  gitIn(w.bare, ["for-each-ref", "--contains", sha, "--format=%(refname)",
    "refs/uzi-incoming", "refs/uzi-owed", "refs/uzi-runner", "refs/uzi-coverage"]).split("\n").filter(Boolean);
const isAncestor = (w: RefusalWorld, sha: string, of: string): boolean =>
  spawnSync("git", ["-C", w.bare, "merge-base", "--is-ancestor", sha, of], { env: ENV }).status === 0;

/** No refused head is lost: each is reachable through a uzi ref, or (covered = a sealed FINAL exists) an ancestor of it. */
function assertNoLoss(w: RefusalWorld, label: string, covered = false): void {
  for (const head of w.heads) {
    if (protectedBy(w, head).length > 0) continue;
    assert.ok(covered && isAncestor(w, head, w.cover), `${label}: refused head ${head} lost (no uzi ref reaches it and no sealed cover holds it)`);
  }
}

async function crashRefusal(t: TestContext, w: RefusalWorld, ops: RefusalOp[], hold: RefusalHold): Promise<void> {
  const rig = makeBarrier(t);
  const worker = spawnChild(t, { scenario: "refusal", dataDir: w.dataDir, token: TOKEN, bare: w.bare, barrier: rig, context: w.ctx,
    refusal: { cap: RF_CAP, nonce: RF_NONCE, clone: w.clone, ops, hold } });
  assert.equal(await worker.line(), "W");
  const finishedFirst = watch(worker, (line) => line.startsWith("DONE") || line.startsWith("FAILED")).then((line): never => {
    throw new assert.AssertionError({ message: `the worker finished before its hold was reached: ${line}` });
  });
  await armed(rig, worker, finishedFirst);
  await worker.kill();
}

async function refusalSuccessor(w: RefusalWorld): Promise<{ rt: AuthorityRuntime; git: GitCache }> {
  const lifetime = await acquireWorkerLifetime(w.dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 20_000 });
  return { rt, git: new GitCache(w.dataDir, nullLogger(), undefined, { authority: rt.hooks, authorityRequired: true, maxOwedCandidates: RF_CAP }) };
}

/** A reader carrying the dead worker's process nonce: what the on-disk evidence alone says. */
const deadWorkerReader = (w: RefusalWorld): GitCache =>
  new GitCache(w.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: RF_CAP, processNonce: RF_NONCE }));

/** The dead worker's reader that also treats the import stages now on disk as finished imports of its process: a
 *  crashed worker's imports are over, and the process-local finished record is what proves it. */
const provenReader = (w: RefusalWorld): GitCache => {
  const reader = deadWorkerReader(w);
  const finished = (reader as unknown as { finishedImportStages: Set<string> }).finishedImportStages;
  for (const line of incomingRefs(w)) if (/\/import-/.test(line.split(" ")[0]!)) finished.add(line.split(" ")[0]!);
  return reader;
};

const gateOf = (w: RefusalWorld, git: GitCache, head: string) => git.refusalReleaseGate(w.bare, RF_RUN, 1, head);

async function resume(w: RefusalWorld, s: { rt: AuthorityRuntime; git: GitCache }, covering: string): Promise<string> {
  return s.rt.withTransaction(() => s.git.cleanupRecoveryGeneration(w.bare, w.ctx, w.frozen, [], [], covering, undefined, "archive"));
}

const refusedRefHold = (sha: string): string => `update-ref --no-deref refs/uzi-incoming/${RF_RUN}/1/refused-${sha} ${sha} 0{40}`;
const stageDeleteHold = (sha: string): string => `update-ref -d refs/uzi-incoming/${RF_RUN}/1/import-[A-Za-z0-9-]{8,64}-[0-9a-f-]{36} ${sha}`;
const refuse = (head: string): RefusalOp => ({ op: "fetch", head });

test("refusal proof 1a: SIGKILL before the admission rename leaves only temp residue; the successor gate is ineligible and a fresh admission does not clear it", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  await crashRefusal(t, w, [{ op: "admit" }], { op: 0, kind: "rename-before", match: "^admission-" });
  const files = rfFiles(w);
  assert.ok(files.some((f) => f.startsWith(".tmp-")), "the interrupted write left its temp file");
  assert.ok(!files.some((f) => f.startsWith("admission-")), "no admission, partial or whole, was published");

  const s = await refusalSuccessor(w);
  for (const [who, git] of [["successor", s.git], ["dead-worker nonce", deadWorkerReader(w)]] as const) {
    const verdict = await gateOf(w, git, w.cover);
    assert.equal(verdict.kind, "ineligible", who);
    assert.match((verdict as { reason: string }).reason, /temporary refusal metadata present/, who);
  }
  assert.equal(await s.git.admitRefusalProvenance(w.ctx), "written", "the successor may write its own admission");
  const after = await gateOf(w, s.git, w.cover);
  assert.equal(after.kind, "ineligible", "a complete admission does not make the leftover temp file harmless");
  assert.match((after as { reason: string }).reason, /temporary refusal metadata present/);
});

test("refusal proof 1b: SIGKILL after the admission rename leaves a complete record; it is foreign to the successor and never overwritten", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  await crashRefusal(t, w, [{ op: "admit" }], { op: 0, kind: "rename-after", match: "^admission-" });
  const name = `admission-${RF_RUN}-1.json`;
  assert.deepEqual(rfJson(w, name), { version: 1, runId: RF_RUN, generation: 1, branch: RF_BR, barePath: w.bare, protocol: 1, process: RF_NONCE });
  assert.ok(!rfFiles(w).some((f) => f.startsWith(".tmp-")), "the rename consumed the temp file");
  const before = rfRaw(w, name);

  const s = await refusalSuccessor(w);
  const verdict = await gateOf(w, s.git, w.cover);
  assert.equal(verdict.kind, "ineligible");
  assert.match((verdict as { reason: string }).reason, /admission for generation 1 is from another process/);
  assert.equal((await gateOf(w, deadWorkerReader(w), w.cover)).kind, "eligible", "the complete record is a valid admission for its own process");
  assert.equal(await s.git.admitRefusalProvenance(w.ctx), "existing");
  assert.equal(rfRaw(w, name), before, "the admission is write-once: the successor's nonce never replaced the dead worker's");
});

test("refusal proof 2: SIGKILL with the stage present and no ledger keeps the stage; the gate is ineligible", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h)], { op: 1, kind: "git-before", match: refusedRefHold(h) });
  assert.equal(stagesAt(w, h).length, 1, "the attributed stage is kept as the refused head's evidence");
  assert.deepEqual(refusedRefAt(w, h), []);
  assert.ok(!rfFiles(w).includes(RF_LEDGER), "the ledger was not written yet");
  assertNoLoss(w, "stage only");

  const s = await refusalSuccessor(w);
  assert.equal((await gateOf(w, s.git, w.cover)).kind, "ineligible");
  // Without a finished record of its own, a dead worker's leftover import stage is unproven (fail closed).
  assert.deepEqual(await gateOf(w, deadWorkerReader(w), w.cover), { kind: "ineligible", reason: "refusal import stage producer unproven" });
  assert.deepEqual(await gateOf(w, provenReader(w), w.cover), { kind: "ineligible", reason: "refusal stage is not in the ledger" });
  assert.equal(stagesAt(w, h).length, 1, "reading the gate deleted nothing");
});

test("refusal proof 3: SIGKILL with the refused ref written and no ledger keeps the object reachable; the gate is ineligible and a retry completes the ledger", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h)], { op: 1, kind: "git-after", match: refusedRefHold(h) });
  assert.equal(refusedRefAt(w, h).length, 1, "the refused ref exists");
  assert.equal(stagesAt(w, h).length, 1);
  assert.ok(!rfFiles(w).includes(RF_LEDGER));
  assertNoLoss(w, "refused ref, no ledger");

  const s = await refusalSuccessor(w);
  assert.equal((await gateOf(w, s.git, w.cover)).kind, "ineligible");
  // Without a finished record of its own, a dead worker's leftover import stage is unproven (fail closed).
  assert.deepEqual(await gateOf(w, deadWorkerReader(w), w.cover), { kind: "ineligible", reason: "refusal import stage producer unproven" });
  assert.deepEqual(await gateOf(w, provenReader(w), w.cover), { kind: "ineligible", reason: "refusal stage is not in the ledger" });

  // The successor's retry of the same refusal records the head (the refused ref write is idempotent).
  gitIn(w.clone, ["update-ref", `refs/heads/${RF_BR}`, h]);
  const retry = new GitCache(w.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: RF_CAP }));
  assert.deepEqual(await retry.fetchAgentBranch(w.bare, w.clone, RF_BR, RF_RUN, { context: w.ctx }), { kind: "not_updated", reason: "owed_limit", refusedSha: h });
  assert.deepEqual(rfLedgerShas(w), [h]);
  assertNoLoss(w, "after retry");
});

test("refusal proof 4: SIGKILL with the ledger temp file written and not renamed keeps the old ledger; temp residue makes the gate ineligible", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 2);
  const [h0, h1] = w.heads as [string, string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h0), refuse(h1)], { op: 2, kind: "rename-before", match: "^refusals-" });
  assert.deepEqual(rfLedgerShas(w), [h0], "the previous ledger is intact");
  assert.ok(rfFiles(w).some((f) => f.startsWith(".tmp-")), "the interrupted rewrite left its temp file");
  assert.equal(refusedRefAt(w, h1).length, 1, "the head being recorded already has its refused ref");
  assert.equal(stagesAt(w, h1).length, 1);
  assertNoLoss(w, "ledger temp");

  const s = await refusalSuccessor(w);
  for (const [who, git] of [["successor", s.git], ["dead-worker nonce", deadWorkerReader(w)]] as const) {
    const verdict = await gateOf(w, git, w.cover);
    assert.equal(verdict.kind, "ineligible", who);
    assert.match((verdict as { reason: string }).reason, /temporary refusal metadata present/, who);
  }
});

test("refusal proof 5: SIGKILL after the ledger rename leaves the complete ledger, which the successor reads", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 2);
  const [h0, h1] = w.heads as [string, string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h0), refuse(h1)], { op: 2, kind: "rename-after", match: "^refusals-" });
  assert.deepEqual(rfLedgerShas(w), [h0, h1], "the renamed ledger is complete");
  assert.ok(!rfFiles(w).some((f) => f.startsWith(".tmp-")));
  assertNoLoss(w, "ledger renamed");

  const s = await refusalSuccessor(w);
  const verdict = await gateOf(w, s.git, w.cover);
  assert.equal(verdict.kind, "ineligible");
  // The dead worker's leftover import stage is unproven to the successor and holds the gate before the admission
  // check (proof 1b covers the foreign admission with no stage on disk).
  assert.deepEqual(verdict, { kind: "ineligible", reason: "refusal import stage producer unproven" });
  const own = await gateOf(w, provenReader(w), w.cover);
  assert.equal(own.kind, "eligible", "the evidence on disk (both heads ledgered, every stage ledgered) is complete");
  assert.deepEqual(rfLedgerShas(w), [h0, h1], "the successor's reads changed nothing");
});

test("refusal proof 6a: SIGKILL before the duplicate stage delete keeps both stages and the ledgered head", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), refuse(h)], { op: 2, kind: "git-before", match: stageDeleteHold(h) });
  assert.equal(stagesAt(w, h).length, 2, "the duplicate has not been deleted yet");
  assert.equal(refusedRefAt(w, h).length, 1);
  assert.deepEqual(rfLedgerShas(w), [h]);
  assertNoLoss(w, "duplicate pending");
  assert.deepEqual(await gateOf(w, deadWorkerReader(w), w.cover), { kind: "ineligible", reason: "refusal import stage producer unproven" });
  const own = await gateOf(w, provenReader(w), w.cover);
  assert.equal(own.kind, "eligible", "a surplus ledgered stage never makes the evidence incomplete");
  assert.equal((await gateOf(w, (await refusalSuccessor(w)).git, w.cover)).kind, "ineligible");
});

test("refusal proof 6b: SIGKILL after the duplicate stage delete leaves one stage and the ledgered head", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), refuse(h)], { op: 2, kind: "git-after", match: stageDeleteHold(h) });
  assert.equal(stagesAt(w, h).length, 1, "exactly the duplicate was deleted");
  assert.equal(refusedRefAt(w, h).length, 1);
  assert.deepEqual(rfLedgerShas(w), [h], "the head is ledgered, so the deletion was never an unledgered loss");
  assertNoLoss(w, "duplicate deleted");
  assert.deepEqual(await gateOf(w, deadWorkerReader(w), w.cover), { kind: "ineligible", reason: "refusal import stage producer unproven" });
  assert.equal((await gateOf(w, provenReader(w), w.cover)).kind, "eligible");
});

test("refusal proof 7: a FINAL written with no POST, then restart: the successor's seal is ineligible and never sends; only the dead worker's own nonce matches", { timeout: 120_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }], { op: 2, kind: "rename-after", match: "^final-" });
  const name = `final-${RF_RUN}-1.json`;
  const final = rfJson(w, name);
  assert.equal(final.sourceSha, w.cover);
  assert.equal(final.process, RF_NONCE);
  const before = rfRaw(w, name);
  assertNoLoss(w, "final written");

  const s = await refusalSuccessor(w);
  const result = await s.git.sealRefusalRelease(w.bare, RF_RUN, 1, w.cover);
  assert.equal(result, "ineligible", "another process may never replay or re-seal the release (it would be a send)");
  assert.equal(rfRaw(w, name), before, "the successor did not rewrite the FINAL");
  assert.deepEqual(await gateOf(w, deadWorkerReader(w), w.cover2), { kind: "ineligible", reason: "refusal import stage producer unproven" });
  assert.equal(await deadWorkerReader(w).sealRefusalRelease(w.bare, RF_RUN, 1, w.cover), "ineligible", "the dead worker's own nonce without a finished record cannot seal either");
  assert.equal(await provenReader(w).sealRefusalRelease(w.bare, RF_RUN, 1, w.cover), "matches");
  assert.equal((await gateOf(w, provenReader(w), w.cover2)).kind, "eligible", "the other source covers every head, so only the existing FINAL can refuse it");
  assert.equal(await provenReader(w).sealRefusalRelease(w.bare, RF_RUN, 1, w.cover2), "mismatch", "a different source is never sealed over the existing FINAL");
  assert.equal(rfRaw(w, name), before);
});

async function allGone(w: RefusalWorld, label: string): Promise<void> {
  assert.deepEqual(incomingRefs(w), [], `${label}: no refusal stage or refused ref remains`);
  assert.deepEqual(rfFiles(w).filter((f) => !f.startsWith("refusals-")), [`admission-${RF_RUN}-1.json`], `${label}: only the kept admission remains, no final, cleanup or temp`);
  assert.deepEqual(ownedPins(w), [], `${label}: the anchors are released`);
}

test("refusal proof 8: SIGKILL with the cleanup intent written never deletes under an uncovered source, and resuming against the sealed source completes", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "rename-after", match: "^cleanup-" });
  const files = rfFiles(w);
  assert.ok(files.includes(`cleanup-${RF_RUN}-1.json`) && files.includes(`final-${RF_RUN}-1.json`));
  assert.deepEqual(rfJson(w, `cleanup-${RF_RUN}-1.json`).ledgerEntries, [{ generation: 1, sha: h }]);
  assert.equal(stagesAt(w, h).length, 1, "the intent precedes every deletion");
  const evidence = (): string => JSON.stringify([rfFiles(w).map((f) => [f, rfRaw(w, f)]), incomingRefs(w), ownedPins(w)]);
  const before = evidence();

  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.base), "retained", "a source that is not the sealed one deletes nothing");
  assert.equal(evidence(), before);
  assertNoLoss(w, "uncovered resume");

  assert.equal(await resume(w, s, w.cover), "removed");
  await allGone(w, "resumed");
  assert.deepEqual(rfLedgerShas(w), []);
  assertNoLoss(w, "resumed", true);
});

for (const nth of [1, 2, 3, 4]) {
  test(`refusal proof 9: SIGKILL after refusal-stage delete ${nth} of 4 keeps the ledger and every head covered; the resumed cleanup is fenced by the unresolved bare record`, { timeout: 180_000 }, async (t) => {
    const w = await refusalWorld(t, 2);
    const [h0, h1] = w.heads as [string, string];
    await crashRefusal(t, w, [{ op: "admit" }, refuse(h0), refuse(h1), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
      { op: 4, kind: "git-after", match: `update-ref -d refs/uzi-incoming/${RF_RUN}/1/`, nth });
    assert.equal(incomingRefs(w).length, 4 - nth, "exactly the deletions up to the hold happened");
    assert.deepEqual(rfLedgerShas(w), [h0, h1], "the ledger is rewritten only after every stage");
    assert.ok(rfFiles(w).includes(`cleanup-${RF_RUN}-1.json`));
    assertNoLoss(w, `after delete ${nth}`, true);

    // KNOWN RESIDUAL of ADR-2545 (#4481 amendment), under ADR-1783's accepted contract (#4066; recovery-design
    // follow-up #4550): the refusal-stage delete is a registered bare mutation (git-authority-hooks.ts "refusal-cleanup-delete-ref"). Its
    // owner died before settling it, and no production path resolves a dead owner's running bare record by read-back
    // (the same contract as proofs 2 and the recovery-cleanup-delete-ref records above): it stays running and fences every
    // later writer on the bare (no in-product path resolves it; operator intervention is needed). So the resumed cleanup
    // is REFUSED, never completed on top of the unconfirmed delete, and it deletes no head's coverage.
    const refsBeforeResume = incomingRefs(w);
    const s = await refusalSuccessor(w);
    assert.equal((await slotOf(s.rt, w.bare))?.pending?.status, "running", "the dead owner's delete is durable and unresolved");
    await assert.rejects(resume(w, s, w.cover), /unresolved pending footprint/);
    assert.equal((await slotOf(s.rt, w.bare))?.pending?.status, "running", "the refused resume settled nothing");
    assert.ok(rfFiles(w).includes(`cleanup-${RF_RUN}-1.json`) && rfFiles(w).includes(`final-${RF_RUN}-1.json`), "the sealed intent is kept");
    assert.deepEqual(incomingRefs(w), refsBeforeResume, "the refused resume deleted no further stage");
    assertNoLoss(w, "refused resume", true);
  });
}

test("refusal proof 10a: SIGKILL after the ledger rewrite keeps the later generation's entry; resuming finishes without re-creating anything", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1, true);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "rename-after", match: "^refusals-" });
  assert.deepEqual(incomingRefs(w), [], "every listed stage is gone before the ledger is rewritten");
  assert.deepEqual(rfLedgerShas(w), [w.later], "exactly the listed entry was removed by the rewrite");
  assert.ok(rfFiles(w).includes(`cleanup-${RF_RUN}-1.json`) && rfFiles(w).includes(`final-${RF_RUN}-1.json`));
  assertNoLoss(w, "rewritten", true);

  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.cover), "removed");
  assert.deepEqual(rfLedgerShas(w), [w.later], "a later generation's evidence is never cleaned by this generation");
  await allGone(w, "resumed");
});

test("refusal proof 10b: SIGKILL after the emptied ledger is removed leaves nothing refused to protect; resuming finishes", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "unlink-after", match: "^refusals-" });
  assert.ok(!rfFiles(w).includes(RF_LEDGER));
  assert.deepEqual(incomingRefs(w), []);
  assertNoLoss(w, "ledger removed", true);
  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.cover), "removed");
  await allGone(w, "resumed");
});

test("refusal proof 11: SIGKILL after the anchor deletes (before the first candidate record is removed) leaves heads covered; resuming does not delete uncovered", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "unlink-before", match: "^candidate-" });
  assert.deepEqual(ownedPins(w), [], "every anchor was deleted");
  assert.deepEqual(incomingRefs(w), []);
  assert.ok(rfFiles(w).includes(`cleanup-${RF_RUN}-1.json`) && rfFiles(w).includes(`final-${RF_RUN}-1.json`));
  assertNoLoss(w, "anchors deleted", true);

  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.base), "retained", "an uncovered source never completes the cleanup");
  assert.ok(rfFiles(w).includes(`final-${RF_RUN}-1.json`), "nothing was deleted by the uncovered resume");
  assertNoLoss(w, "uncovered resume", true);
  assert.equal(await resume(w, s, w.cover), "removed");
  await allGone(w, "resumed");
});

test("refusal proof 12a: SIGKILL after the FINAL is deleted; resuming deletes the intent, keeps the admission and re-creates nothing", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "unlink-after", match: "^final-" });
  const files = rfFiles(w);
  assert.ok(!files.includes(`final-${RF_RUN}-1.json`));
  assert.ok(files.includes(`cleanup-${RF_RUN}-1.json`) && files.includes(`admission-${RF_RUN}-1.json`));
  assertNoLoss(w, "final deleted", true);

  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.cover), "removed");
  await allGone(w, "resumed");
  // Nothing is re-created: with the evidence gone the seal has nothing to seal and writes no FINAL.
  assert.equal(await deadWorkerReader(w).sealRefusalRelease(w.bare, RF_RUN, 1, w.cover), "none");
  await allGone(w, "after a later seal attempt");
});

test("refusal proof 12b: SIGKILL after the cleanup intent is deleted; resuming keeps the admission", { timeout: 180_000 }, async (t) => {
  const w = await refusalWorld(t, 1);
  const [h] = w.heads as [string];
  await crashRefusal(t, w, [{ op: "admit" }, refuse(h), { op: "seal", sha: w.cover }, { op: "cleanup", covering: w.cover }],
    { op: 3, kind: "unlink-after", match: "^cleanup-" });
  // The owed-path marker is removed last (after the final and the intent), so it is still present at this crash point.
  assert.deepEqual(rfFiles(w), [`admission-${RF_RUN}-1.json`, `owedpath-${RF_RUN}-1.json`]);
  assertNoLoss(w, "intent deleted", true);
  const s = await refusalSuccessor(w);
  assert.equal(await resume(w, s, w.cover), "removed");
  await allGone(w, "resumed");
});
