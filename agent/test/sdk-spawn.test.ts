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
  // A fake procfs: `<root>/self/mountinfo` plus one `<root>/<pid>/stat` per entry.
  function fakeProc(opts: { superOpts?: string; mountinfo?: boolean; stats?: Record<string, string> }): string {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-fakeproc-"));
    if (opts.mountinfo !== false) {
      fs.mkdirSync(path.join(root, "self"));
      fs.writeFileSync(
        path.join(root, "self", "mountinfo"),
        `22 1 0:5 / / rw,relatime - overlay overlay rw\n` +
          `23 22 0:21 / ${root} rw,nosuid,nodev,noexec,relatime - proc proc ${opts.superOpts ?? "rw"}\n`,
      );
    }
    for (const [pid, stat] of Object.entries(opts.stats ?? {})) {
      fs.mkdirSync(path.join(root, pid));
      fs.writeFileSync(path.join(root, pid, "stat"), stat);
    }
    return root;
  }
  const stat = (pid: number, comm: string, pgrp: number, state = "S"): string =>
    `${pid} (${comm}) ${state} 1 ${pgrp} ${pgrp} 0 -1 4194560 0 0 0 0\n`;

  const cases: Array<[string, Parameters<typeof fakeProc>[0], boolean | undefined]> = [
    ["a member (comm with spaces and parens) is present", { stats: { "7": stat(7, "a) (b", 4242) } }, true],
    ["a zombie member still counts as present", { stats: { "7": stat(7, "sh", 4242, "Z") } }, true],
    ["no member is absent", { stats: { "7": stat(7, "sh", 7), "8": stat(8, "node", 8) } }, false],
    ["hidepid=2 is unknowable", { superOpts: "rw,hidepid=2", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["hidepid=invisible is unknowable", { superOpts: "rw,hidepid=invisible", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["hidepid=ptraceable is unknowable", { superOpts: "rw,hidepid=ptraceable", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["numeric hidepid=4 is unknowable", { superOpts: "rw,hidepid=4", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["an unrecognised future hidepid value is unknowable", { superOpts: "rw,hidepid=someday", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["any subset= value is unknowable", { superOpts: "rw,subset=sysfs", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["hidepid=0 counts as complete", { superOpts: "rw,hidepid=0", stats: { "7": stat(7, "sh", 4242) } }, true],
    ["hidepid=off counts as complete", { superOpts: "rw,hidepid=off", stats: { "7": stat(7, "sh", 7) } }, false],
    ["subset=pid is unknowable", { superOpts: "rw,subset=pid", stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["no mountinfo is unknowable", { mountinfo: false, stats: { "7": stat(7, "sh", 7) } }, undefined],
    ["a scan that sees no process is unknowable", { stats: {} }, undefined],
    ["a malformed stat is unknowable", { stats: { "7": "garbage" } }, undefined],
  ];
  for (const [label, spec, want] of cases) {
    it(label, () => {
      const root = fakeProc(spec);
      try {
        assert.strictEqual(processGroupPresent(4242, root), want);
      } finally {
        fs.rmSync(root, { recursive: true, force: true });
      }
    });
  }

  it("a procfs root that does not exist is unknowable", () => {
    assert.strictEqual(processGroupPresent(4242, path.join(os.tmpdir(), `uzi-no-proc-${process.pid}`)), undefined);
  });

  it("on the real procfs, tracks a detached group through its group kill (Linux) or is unknowable", async () => {
    const child = spawnDetached(spawnOpts("sleep", ["30"]));
    try {
      assert.ok(typeof child.pid === "number" && child.pid > 0);
      if (!fs.existsSync("/proc/self/stat")) {
        assert.strictEqual(processGroupPresent(child.pid), undefined, "no procfs: never reported absent");
        return;
      }
      assert.strictEqual(processGroupPresent(child.pid), true);
      assert.strictEqual(killProcessGroupOnly(child.pid), true);
      assert.strictEqual(await waitUntil(() => processGroupPresent(child.pid!) === false), true);
    } finally {
      killProcessGroup(child.pid);
    }
  });
});
