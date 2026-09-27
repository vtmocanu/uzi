// Issue #1783: the run-quiescence reaper under the REAL worker/runner/runner-cmd uid split.
//
// Runs INSIDE the worker image, started through its root entrypoint (and `docker run --init`, so
// pid 1 is a root-owned docker-init), so this process is the `worker` uid (10001) holding only the
// controller's SETUID/SETGID, exactly as in production. The reaper's scan and kills therefore run
// in the runner-uid helper, started through the production setpriv wrapper (runnerCommand).
//
// A host or single-uid run cannot manufacture that split, and a run executed wholly as root
// bypasses it, so the first test REFUSES to pass anywhere else instead of skipping.
//
// Covered here: A-healthy through the real setpriv helper (a clean clone is quiescent; an
// attributed runner-uid straggler is reaped) and A-foreign-uids with real worker, root and
// runner-cmd processes in scope (never signalled, never `unverified`).
//
// M3 adds the ROOT-OWNED RESIDUE cases: run.sh's root step (plant-residue.sh) plants a
// `root:root drwxr-sr-x` directory holding `agent/src` and `gate-log.loop` at a canonical clone
// path before the drop. The worker can neither delete it nor move it to another parent (EACCES:
// a cross-parent directory rename rewrites `..`, which needs write on the moved directory), but it
// can rename it within its own worker-owned 2775 repo dir, which is how the canonical reseed frees
// the path. Those cases SKIP, naming why, wherever the residue is not root-owned.

import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

type QuiescenceModule = typeof import("../../agent/src/run-quiescence.js");
type RunnerUidModule = typeof import("../../agent/src/runner-uid.js");
type MarkModule = typeof import("../../agent/src/worker-spawn-mark.js");
type GitModule = typeof import("../../agent/src/git.js");
type AttemptPathModule = typeof import("../../agent/src/attempt-path.js");

// The code under test is loaded from the image's /app/src by default, so a built image proves
// what it ships. CQ_SRC points it at a mounted tree (run.sh mounts it UNDER /app, so the helper
// still resolves the image's tsx loader from /app/node_modules).
const SRC = process.env.CQ_SRC ?? "/app/src";
const load = async <T>(name: string): Promise<T> => (await import(pathToFileURL(path.join(SRC, name)).href)) as T;

// A world-traversable fixture root, so worker, runner and runner-cmd processes can all sit in it.
const ROOT = "/tmp/cq-fixture";
const CLONE = path.join(ROOT, "runner", "github.com+o+cq", "issue-17");
const SIBLING = path.join(ROOT, "runner", "github.com+o+cq", "issue-1769");

let q: QuiescenceModule;
let runnerUid: RunnerUidModule;
let mark: MarkModule;
let gitMod: GitModule;
let attemptPath: AttemptPathModule;
const children: ChildProcess[] = [];

// issue #1783 M3: planted by plant-residue.sh (root, before the drop). The data dir and origin are
// what GitCache derives the canonical path from: `<data>/runner/<bareDirName(origin)>/issue-<iid>`.
const RESIDUE_DATA = "/tmp/cq-residue";
const RESIDUE_ORIGIN = "/tmp/cq-origin";
const RESIDUE_IID = 42;
const RESIDUE_CANON = path.join(RESIDUE_DATA, "runner", "tmp+cq-origin", `issue-${RESIDUE_IID}`);
/** Evaluated at load, before any test moves it: the planted residue must be root-owned. */
const RESIDUE_SKIP: string | false = (() => {
  try {
    const st = fs.lstatSync(RESIDUE_CANON);
    if (st.isDirectory() && !st.isSymbolicLink() && st.uid === 0) return false;
  } catch {
    // absent: not planted by run.sh
  }
  return "requires a root-planted residue; run task test:clone-quiescence";
})();

/** Start a detached `sleep` as `runner` (via the production wrapper) with `env`, in `cwd`, and
 *  return its pid. The shell backgrounds it through `setsid`, so it outlives the wrapper. */
function runnerSleeper(cwd: string, env: Record<string, string>): number {
  const assignments = Object.entries(env).map(([k, v]) => `${k}=${v}`);
  const wrapped = runnerUid.runnerCommand("/bin/sh", [
    "-c",
    'cd "$1"; shift; env "$@" setsid sleep 300 >/dev/null 2>&1 & echo $!',
    "sh",
    cwd,
    ...assignments,
  ]);
  const out = execFileSync(wrapped.command, wrapped.args, { encoding: "utf8", env: { PATH: "/usr/bin:/bin" } });
  return Number(out.trim());
}

/** The same, as the Codex command-root uid `runner-cmd` (10003). */
function commandSleeper(cwd: string, env: Record<string, string>): number {
  const assignments = Object.entries(env).map(([k, v]) => `${k}=${v}`);
  const wrapped = runnerUid.commandRootCommand("/bin/sh", [
    "-c",
    'cd "$1"; shift; env "$@" setsid sleep 300 >/dev/null 2>&1 & echo $!',
    "sh",
    cwd,
    ...assignments,
  ]);
  const out = execFileSync(wrapped.command, wrapped.args, { encoding: "utf8", env: { PATH: "/usr/bin:/bin" } });
  return Number(out.trim());
}

/** Alive (and not a zombie), read from the world-readable status file. */
function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(q.procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

function uidOf(pid: number): number {
  return Number(/^Uid:\s+(\d+)/m.exec(q.procfsTable.readStatus(pid))?.[1]);
}

async function until(pred: () => boolean, ms = 5000): Promise<boolean> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return pred();
}

function quiesce(attempt: ReturnType<QuiescenceModule["newRunAttempt"]>, registry: InstanceType<QuiescenceModule["LiveAttemptRegistry"]>) {
  return q.quiesceRunAttempt({
    mode: "own",
    attempt,
    cloneKey: attempt.cloneKey,
    targetPaths: [attempt.clonePath],
    processes: true,
    dockerHost: undefined,
    registry,
  });
}

before(async () => {
  q = await load<QuiescenceModule>("run-quiescence.ts");
  runnerUid = await load<RunnerUidModule>("runner-uid.ts");
  mark = await load<MarkModule>("worker-spawn-mark.ts");
  gitMod = await load<GitModule>("git.ts");
  attemptPath = await load<AttemptPathModule>("attempt-path.ts");
  for (const d of [CLONE, SIBLING]) fs.mkdirSync(d, { recursive: true });
  for (const d of [ROOT, path.join(ROOT, "runner"), path.dirname(CLONE), CLONE, SIBLING]) fs.chmodSync(d, 0o777);
});

after(() => {
  for (const c of children) c.kill("SIGKILL");
});

describe("clone quiescence under the real uid split (issue #1783)", () => {
  it("runs as the worker uid under the split, not as root or single-uid", () => {
    assert.equal(process.getuid?.(), runnerUid.WORKER_UID, "the fixture must run as the worker uid");
    assert.equal(runnerUid.uidSplitActive(), true, "the entrypoint must have established UZI_UID_SPLIT=1");
    assert.equal(uidOf(1), 0, "docker-init (pid 1) is root-owned");
  });

  it("A-healthy: a clean clone is quiescent through the real setpriv helper", async () => {
    const registry = new q.LiveAttemptRegistry();
    const attempt = q.newRunAttempt("run-healthy", 1, CLONE, () => []);
    registry.add(attempt);
    const out = await quiesce(attempt, registry);
    assert.equal(out.process?.state, "quiescent", out.process?.detail);
    assert.deepEqual(out.process?.killed, []);
    assert.equal(out.docker.state, "not_wired");
  });

  it("A-healthy: an attributed runner-uid straggler is reaped by the helper; a sibling key's is not", async () => {
    const registry = new q.LiveAttemptRegistry();
    const attempt = q.newRunAttempt("run-straggler", 1, CLONE, () => []);
    const sibling = q.newRunAttempt("run-sibling", 1, SIBLING, () => []);
    registry.add(attempt);
    registry.add(sibling);
    const markers = (a: typeof attempt) => ({
      [mark.RUN_ATTEMPT_ENV]: a.marker,
      [mark.RUN_CLONE_ENV]: a.clonePath,
      [mark.RUN_CLONE_KEY_ENV]: a.cloneKey,
    });
    const straggler = runnerSleeper(CLONE, markers(attempt));
    const next = runnerSleeper(SIBLING, markers(sibling));
    assert.ok(await until(() => alive(straggler) && alive(next)));
    assert.equal(uidOf(straggler), runnerUid.RUNNER_UID);
    const out = await quiesce(attempt, registry);
    assert.equal(out.process?.state, "quiescent", out.process?.detail);
    assert.deepEqual(out.process?.killed, [straggler]);
    assert.equal(await until(() => !alive(straggler)), true, "the straggler is gone");
    assert.equal(alive(next), true, "the issue-1769 process is untouched");
    // Clean up the sibling through its own attempt's reap.
    const sib = await quiesce(sibling, registry);
    assert.deepEqual(sib.process?.killed, [next]);
  });

  it("A-foreign-uids: worker, root and runner-cmd processes in scope are ignored, never unverified", async () => {
    const registry = new q.LiveAttemptRegistry();
    const attempt = q.newRunAttempt("run-foreign", 1, CLONE, () => []);
    registry.add(attempt);
    const own = {
      [mark.RUN_ATTEMPT_ENV]: attempt.marker,
      [mark.RUN_CLONE_KEY_ENV]: attempt.cloneKey,
    };
    // A worker-uid process sitting in the clone and carrying the run's marker (its environ is
    // unreadable to the runner-uid helper, which must not matter: R0 reads the uid FIRST).
    const worker = spawn("/bin/sleep", ["300"], { cwd: CLONE, env: { PATH: "/usr/bin:/bin", ...own }, stdio: "ignore" });
    children.push(worker);
    // A runner-cmd process with the run's own marker in the clone.
    const cmd = commandSleeper(CLONE, own);
    assert.ok(await until(() => alive(worker.pid!) && alive(cmd)));
    assert.equal(uidOf(worker.pid!), runnerUid.WORKER_UID);
    assert.equal(uidOf(cmd), runnerUid.COMMAND_UID);
    const out = await quiesce(attempt, registry);
    assert.equal(out.process?.state, "quiescent", out.process?.detail);
    assert.deepEqual(out.process?.killed, []);
    assert.equal(alive(worker.pid!), true);
    assert.equal(alive(cmd), true);
    assert.equal(alive(1), true, "the root pid 1 is untouched");
    worker.kill("SIGKILL");
    const k = runnerUid.commandRootCommand("kill", ["-KILL", String(cmd)]);
    execFileSync(k.command, k.args, { env: { PATH: "/usr/bin:/bin" } });
  });

  // ── issue #1783 M3: a root-owned residue at the canonical clone path ─────────────────────
  // Kept in this describe block so they run under the same worker-uid / uid-split precondition.

  it("M3: the planted residue is root:root drwxr-sr-x with agent/src and gate-log.loop, and the worker cannot delete it", { skip: RESIDUE_SKIP }, async () => {
    const st = fs.lstatSync(RESIDUE_CANON);
    assert.equal(st.uid, 0);
    assert.equal(st.gid, 0);
    assert.equal(st.mode & 0o7777, 0o2755, "drwxr-sr-x");
    assert.equal(fs.statSync(path.join(RESIDUE_CANON, "agent", "src")).isDirectory(), true);
    assert.equal(fs.statSync(path.join(RESIDUE_CANON, "gate-log.loop")).isFile(), true);
    await assert.rejects(fs.promises.rm(RESIDUE_CANON, { recursive: true, force: true }), { code: "EACCES" });
    assert.equal(fs.existsSync(path.join(RESIDUE_CANON, "gate-log.loop")), true, "nothing was deleted");
  });

  it("M3: a CROSS-parent rename of the residue fails EACCES, which is why the quarantine stays in the same parent", { skip: RESIDUE_SKIP }, () => {
    // The shape of createRetireScratchParent: a fresh worker-owned 0700 dir under the runner root.
    const scratch = path.join(RESIDUE_DATA, "runner", `.retire-cq-${process.pid}`);
    fs.mkdirSync(scratch, { mode: 0o700 });
    try {
      assert.throws(() => fs.renameSync(RESIDUE_CANON, path.join(scratch, "clone")), { code: "EACCES" });
      assert.equal(fs.lstatSync(RESIDUE_CANON).uid, 0, "the residue did not move");
    } finally {
      fs.rmdirSync(scratch);
    }
  });

  it("M3: the canonical reseed renames it within the same parent, keeps it, and completes", { skip: RESIDUE_SKIP }, async () => {
    // main.ts runs the worker with umask 002 under the split, so the runner can write what the
    // worker creates for it (the seed's scratch posture check depends on it).
    process.umask(0o002);
    const gitEnv = { PATH: "/usr/bin:/bin", GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    fs.mkdirSync(RESIDUE_ORIGIN, { recursive: true });
    execFileSync("git", ["init", "-q", "-b", "main", RESIDUE_ORIGIN], { env: gitEnv });
    fs.writeFileSync(path.join(RESIDUE_ORIGIN, "README.md"), "cq\n");
    execFileSync("git", ["-C", RESIDUE_ORIGIN, "add", "README.md"], { env: gitEnv });
    execFileSync("git", ["-C", RESIDUE_ORIGIN, "-c", "user.email=cq@cq", "-c", "user.name=cq", "commit", "-q", "-m", "init"], { env: gitEnv });

    const lines: Array<Record<string, unknown>> = [];
    const logger = {
      debug: () => {},
      info: () => {},
      warn: (msg: string, fields?: Record<string, unknown>) => void lines.push({ msg, ...fields }),
      error: () => {},
      addSecret: () => {},
      removeSecret: () => {},
      child: () => logger,
    };
    const git = new gitMod.GitCache(RESIDUE_DATA, logger);
    const bare = await git.ensureClone(RESIDUE_ORIGIN);
    assert.equal(git.runnerClonePath(bare, `issue-${RESIDUE_IID}`), RESIDUE_CANON, "plant-residue.sh planted at GitCache's canonical path");

    const proofs: string[] = [];
    const clone = await git.createOrAttachRunnerClone(bare, RESIDUE_IID, "run-cq-residue", false, undefined, undefined, {
      // The runner's canonical-reseed proof: a seed-mode scan scoped to the canonical path,
      // through the real runner-uid helper. Anything but quiescent blocks with nothing moved.
      beforeFree: async (p) => {
        proofs.push(p);
        const out = await q.quiesceRunAttempt({
          mode: "seed",
          attempt: undefined,
          cloneKey: attemptPath.cloneKeyOf(p).cloneKey,
          targetPaths: [p],
          processes: true,
          dockerHost: undefined,
          registry: new q.LiveAttemptRegistry(),
          site: "canonical_reseed",
        });
        if (out.process?.state !== "quiescent") throw new gitMod.CloneResidueBlockedError(out.process?.detail ?? "not quiescent");
      },
    });
    assert.deepEqual(proofs, [RESIDUE_CANON], "the proof ran over the canonical path");
    assert.equal(clone.path, RESIDUE_CANON, "the reseed completed at the canonical path");
    assert.equal(fs.existsSync(path.join(RESIDUE_CANON, ".git")), true);
    assert.equal(fs.lstatSync(RESIDUE_CANON).uid, runnerUid.RUNNER_UID, "a fresh runner-owned clone");
    assert.equal(fs.existsSync(path.join(RESIDUE_CANON, "README.md")), true);

    const parent = path.dirname(RESIDUE_CANON);
    const names = fs.readdirSync(parent).filter((n) => n.startsWith(".uzi-residue-"));
    assert.equal(names.length, 1, `one residue: ${names.join(",")}`);
    const art = attemptPath.parseRetainedArtifactName(names[0]!);
    assert.ok(art?.kind === "residue" && art.key === `issue-${RESIDUE_IID}`, "the pinned residue grammar");
    const residue = path.join(parent, names[0]!);
    const st = fs.lstatSync(residue);
    assert.equal(st.uid, 0, "the residue is the root-owned directory itself, moved within the same parent");
    assert.equal(st.mode & 0o7777, 0o2755);
    assert.equal(fs.existsSync(path.join(residue, "agent", "src")), true);
    assert.equal(fs.existsSync(path.join(residue, "gate-log.loop")), true);
    assert.equal(lines.find((l) => String(l.msg).includes("quarantined it as residue"))?.residue, residue, "the log names the residue");

    // Kept: a runner-uid delete (what the retention sweep runs) cannot remove a root-owned residue.
    const rm = runnerUid.runnerCommand("/bin/rm", ["-rf", "--", residue]);
    assert.throws(() => execFileSync(rm.command, rm.args, { env: { PATH: "/usr/bin:/bin" }, stdio: "pipe" }));
    assert.equal(fs.existsSync(path.join(residue, "gate-log.loop")), true, "the residue is kept");
  });
});
