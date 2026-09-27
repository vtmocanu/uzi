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

import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

type QuiescenceModule = typeof import("../../agent/src/run-quiescence.js");
type RunnerUidModule = typeof import("../../agent/src/runner-uid.js");
type MarkModule = typeof import("../../agent/src/worker-spawn-mark.js");

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
const children: ChildProcess[] = [];

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

  // ── EXTENSION POINT (issue #1783 M3) ────────────────────────────────────────────────────
  // M3 adds the root-owned residue cases here (e.g. a root-owned file tree or process left in a
  // clone by a Docker bind, and the seed/capture sweeps over `.attempt-*` paths). Keep them in
  // this describe block so they run under the same worker-uid / uid-split precondition above.
});
