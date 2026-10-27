import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext } from "../src/git.js";
import { agentRoot, makeTmp, testLockOptions } from "./authority-test-support.js";
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
}

function spawnChild(t: TestContext, config: ChildConfig): Child {
  const proc = spawn(process.execPath, ["--import", "tsx", childScript, JSON.stringify(config)], {
    cwd: agentRoot, env: ENV, stdio: ["pipe", "pipe", "inherit"],
  });
  const exited = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) =>
    proc.once("close", (code, signal) => resolve({ code, signal })));
  const queue: string[] = [];
  const waiters: Array<{ resolve(line: string): void; reject(error: Error): void }> = [];
  let buffer = "";
  proc.stdout!.on("data", (chunk: Buffer) => {
    buffer += chunk.toString("utf8");
    for (let i = buffer.indexOf("\n"); i >= 0; i = buffer.indexOf("\n")) {
      const next = buffer.slice(0, i);
      buffer = buffer.slice(i + 1);
      const waiter = waiters.shift();
      if (waiter) waiter.resolve(next); else queue.push(next);
    }
  });
  void exited.then(({ code, signal }) => {
    for (const waiter of waiters.splice(0)) waiter.reject(new Error(`child exited (${code ?? signal}) before reporting`));
  });
  t.after(() => { if (proc.exitCode === null && proc.signalCode === null) proc.kill("SIGKILL"); });
  return {
    proc, exited,
    line: () => queue.length > 0 ? Promise.resolve(queue.shift()!)
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
 * polls the kernel's view against a generous wall-clock deadline (not an iteration count, not an ordering sleep). */
async function gone(pid: number): Promise<void> {
  const deadline = Date.now() + 60_000;
  while (alive(pid) && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(alive(pid), false, "the helper process is gone");
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
  assert.match(finished, /^DONE:\{"ended":\["[^"]*issue-5101"\],"kept":\[\]\}$/);
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
