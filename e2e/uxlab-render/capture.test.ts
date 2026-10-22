import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { captureOutput } from "./capture.js";

test("diagnostics flush a prefix before EOF and stop at exactly 1 MiB", { timeout: 30000 }, async () => {
  const dir = fs.mkdtempSync(fileURLToPath(new URL("../../.uzi/scratch/capture-test-", import.meta.url)));
  const file = dir + "/output", fd = fs.openSync(file, "wx");
  const child = spawn("/bin/sh", ["-c", "exec " + captureOutput],
    { stdio: ["pipe", fd, "ignore"] });
  fs.closeSync(fd);
  const input = child.stdin;
  assert(input);
  input.on("error", () => {});
  const done = new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", code => code === 0 ? resolve() : reject(new Error("capture exit " + code)));
  });
  // Handle rejection during failure teardown even if prefix verification fails first.
  void done.catch(() => {});
  let watcher: fs.FSWatcher | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const flushed = new Promise<void>((resolve, reject) => {
      watcher = fs.watch(file, () => { if (fs.statSync(file).size >= 6) resolve(); });
      timer = setTimeout(() => reject(new Error("prefix buffered until EOF")), 2000);
    });
    input.write("prefix");
    await flushed;
    clearTimeout(timer);
    watcher?.close();
    watcher = undefined;
    assert.equal(fs.readFileSync(file, "utf8"), "prefix");
    input.end(Buffer.alloc(1048576, 120));
    await done;
    assert.equal(fs.statSync(file).size, 1048576);
    assert.equal(fs.readFileSync(file).subarray(0, 6).toString(), "prefix");
  } finally {
    clearTimeout(timer);
    watcher?.close();
    input.destroy();
    child.kill("SIGKILL"); // This exact child handle only; no PID/group lookup.
    await done.catch(() => {});
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
