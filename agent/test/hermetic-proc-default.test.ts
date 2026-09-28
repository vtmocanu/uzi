import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawn, type ChildProcess } from "node:child_process";
import { LiveAttemptRegistry, newRunAttempt, procfsTable, quiesceRunAttempt, reapRunProcesses } from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV, workerSpawnNonce } from "../src/worker-spawn-mark.js";

// issue #1783 — the package.json `test` script preloads test/setup/hermetic-proc.ts into EVERY
// test file's process (node --test forwards the parent's --import flags to each child). This file
// deliberately does NOT import that module: it proves the preload reached it, by planting a REAL
// own-marked straggler in a clone that a real-procfs reap would kill, and showing the default
// reap (in-process and through the real helper) sees an empty table instead.

const HAS_PROCFS = process.platform === "linux" && fs.existsSync(path.join("/", "proc", "self", "status"));
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hermetic-default-"));
let straggler: ChildProcess | undefined;
after(() => {
  try {
    if (straggler?.pid) process.kill(straggler.pid, "SIGKILL");
  } catch {
    /* gone */
  }
  fs.rmSync(tmp, { recursive: true, force: true });
});

function alive(pid: number | undefined): boolean {
  if (!pid) return false;
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

describe("hermetic default quiescence view (preloaded)", { skip: !HAS_PROCFS }, () => {
  it("a reap with no injected table ignores the host's processes, even a real in-scope straggler", async () => {
    const clone = path.join(tmp, "runner", "github.com+o+r", "issue-17");
    fs.mkdirSync(clone, { recursive: true });
    const own = newRunAttempt("run-17", 1, clone, () => []);
    straggler = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000);"], {
      cwd: clone,
      env: { PATH: process.env.PATH, [RUN_ATTEMPT_ENV]: own.marker, [RUN_CLONE_KEY_ENV]: own.cloneKey },
      detached: true,
      stdio: "ignore",
    });
    const end = Date.now() + 3000;
    while (!alive(straggler.pid) && Date.now() < end) await new Promise((r) => setTimeout(r, 25));
    assert.equal(alive(straggler.pid), true);

    const registry = new LiveAttemptRegistry();
    registry.add(own);
    const out = await quiesceRunAttempt({
      mode: "own",
      attempt: own,
      cloneKey: own.cloneKey,
      targetPaths: [own.clonePath],
      processes: true,
      dockerHost: undefined,
      registry,
    });
    assert.equal(out.process?.state, "quiescent", out.process?.detail);
    assert.deepEqual(out.process?.killed, [], "the preloaded empty view lists no process at all");

    // The helper path gets the same view forwarded in its request.
    const viaHelper = await reapRunProcesses(
      {
        mode: "own",
        targetUid: process.getuid?.() ?? 0,
        targetKey: own.cloneKey,
        targetPaths: [own.clonePath],
        ownMarker: own.marker,
        liveMarkers: [],
        liveRoots: [],
        workerNonce: workerSpawnNonce(),
      },
      { viaHelper: true },
    );
    assert.equal(viaHelper.state, "quiescent", viaHelper.detail);
    assert.deepEqual(viaHelper.killed, []);
    assert.equal(alive(straggler.pid), true, "no real process was signalled");
  });
});
