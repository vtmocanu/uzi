import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached, killProcessGroup } from "../src/sdk-spawn.js";

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
