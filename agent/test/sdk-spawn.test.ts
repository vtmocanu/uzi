import { spawn } from "node:child_process";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached, killProcessGroup, killProcessGroupOnly, processGroupPresent } from "../src/sdk-spawn.js";

// Proves the process-group kill actually reaps a grandchild (the whole point of
// spawning detached): a bash the agent backgrounds must die when the watchdog
// group-kills. No Anthropic session — benign `sleep` subprocesses only.

const alive = (pid: number): boolean => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

const waitUntil = async (fn: () => boolean, timeoutMs = 4000): Promise<boolean> => {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (fn()) return true;
    await new Promise((r) => setTimeout(r, 20));
  }
  return fn();
};

function spawnOpts(command: string, args: string[]): SpawnOptions {
  return { command, args, env: { ...process.env }, signal: new AbortController().signal };
}

describe("killProcessGroup", () => {
  it("no-ops on an undefined or non-positive pid", () => {
    assert.strictEqual(killProcessGroup(undefined), false);
    assert.strictEqual(killProcessGroup(0), false);
    assert.strictEqual(killProcessGroup(-1), false);
  });
});

describe("spawnDetached + killProcessGroup", () => {
  it("kills the detached child AND a backgrounded grandchild via the group", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-spawn-"));
    const gpidFile = path.join(dir, "grandchild.pid");
    // Child backgrounds a grandchild sleep, records its pid, then sleeps.
    const child = spawnDetached(spawnOpts("sh", ["-c", `sleep 30 & echo $! > ${gpidFile}; sleep 30`]));

    try {
      assert.ok(typeof child.pid === "number" && child.pid > 0, "child should have a pid");
      // Wait for the grandchild pid to be recorded.
      await waitUntil(() => fs.existsSync(gpidFile) && fs.readFileSync(gpidFile, "utf8").trim().length > 0);
      const grandchildPid = Number(fs.readFileSync(gpidFile, "utf8").trim());
      assert.ok(grandchildPid > 0, "grandchild pid should be recorded");
      assert.ok(alive(grandchildPid), "grandchild should be alive before the kill");

      assert.strictEqual(killProcessGroup(child.pid), true);

      // Both the child and the grandchild die because they share the group the
      // detached spawn created (kill(-pid) reaches the whole tree).
      assert.strictEqual(await waitUntil(() => !alive(child.pid!)), true, "child should be dead");
      assert.strictEqual(await waitUntil(() => !alive(grandchildPid)), true, "grandchild should be dead");
    } finally {
      killProcessGroup(child.pid);
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

// issue #1656: before resuming a turn whose CLI died from a foreign signal, the executor
// SIGKILLs the dead CLI's group ONLY (never the bare pid, which may be recycled) and resumes
// only once processGroupPresent confirms the group empty.

describe("killProcessGroupOnly (issue #1656)", () => {
  it("signals the group only: a pid that leads no group is left alone (no bare-pid fallback)", async () => {
    // Our own NON-detached child: its pid is no process-group id, so the group signal fails
    // and, unlike killProcessGroup, nothing falls back to signalling the pid itself.
    const child = spawn("sleep", ["30"], { stdio: "ignore" });
    try {
      assert.ok(typeof child.pid === "number" && child.pid > 0);
      assert.strictEqual(killProcessGroupOnly(child.pid), false);
      await new Promise((r) => setTimeout(r, 100));
      assert.ok(alive(child.pid), "the bare pid must survive a group-only kill");
    } finally {
      child.kill("SIGKILL");
    }
  });

  it("returns false for a non-positive pgid", () => {
    assert.strictEqual(killProcessGroupOnly(0), false);
    assert.strictEqual(killProcessGroupOnly(-1), false);
  });
});

describe("processGroupPresent (issue #1656)", () => {
  // The kernel is the authority: `kill(-pgid, 0)` is an atomic existence check on the group,
  // unlike any listing of processes. ESRCH ⇒ absent; success or EPERM (a live runner group seen
  // from the worker uid under the split) ⇒ present; anything else ⇒ unknowable.
  const errno = (code: string): Error => Object.assign(new Error(code), { code });
  const cases: Array<[string, () => true, boolean | undefined]> = [
    ["a delivered probe signal means present", () => true, true],
    ["EPERM means present (a live group of another uid)", () => { throw errno("EPERM"); }, true],
    ["ESRCH means absent", () => { throw errno("ESRCH"); }, false],
    ["any other error is unknowable", () => { throw errno("EINVAL"); }, undefined],
    ["an error with no code is unknowable", () => { throw new Error("boom"); }, undefined],
  ];
  for (const [label, kill, want] of cases) {
    it(label, () => {
      const calls: Array<[number, string | number | undefined]> = [];
      const got = processGroupPresent(4242, (pid, signal) => {
        calls.push([pid, signal]);
        return kill();
      });
      assert.strictEqual(got, want);
      assert.deepStrictEqual(calls, [[-4242, 0]], "probes the GROUP with signal 0, nothing else");
    });
  }

  it("a non-positive pgid is unknowable and probes nothing", () => {
    let called = false;
    const probe = (): true => { called = true; return true; };
    assert.strictEqual(processGroupPresent(0, probe), undefined);
    assert.strictEqual(processGroupPresent(-5, probe), undefined);
    assert.strictEqual(called, false);
  });

  it("tracks a real detached group through its group kill", async () => {
    const child = spawnDetached(spawnOpts("sleep", ["30"]));
    try {
      assert.ok(typeof child.pid === "number" && child.pid > 0);
      assert.strictEqual(processGroupPresent(child.pid), true);
      assert.strictEqual(killProcessGroupOnly(child.pid), true);
      assert.strictEqual(await waitUntil(() => processGroupPresent(child.pid!) === false), true);
    } finally {
      killProcessGroup(child.pid);
    }
  });
});
