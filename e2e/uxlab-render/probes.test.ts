import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import vm from "node:vm";
import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { boundaryProbe, homeSnapshot } from "./probes.js";

const exec = promisify(execFile);
async function fixture(t: { after: (fn: () => Promise<void>) => void }) {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-home-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  return dir;
}
const snapshot = (home: string) => exec(process.execPath, ["-e", homeSnapshot], {
  env: { HOME: home }, timeout: 10_000, maxBuffer: 4 * 1024 * 1024,
});
test("standalone HOME snapshot records identity/mtime, hashes bytes and never follows links", async (t) => {
  const home = await fixture(t);
  await fs.writeFile(path.join(home, "file"), "hello");
  await fs.symlink("/does-not-exist", path.join(home, "link"));
  const first = (await snapshot(home)).stdout;
  assert.equal((await snapshot(home)).stdout, first);
  const rows = JSON.parse(first);
  const file = rows.find((row: unknown[]) => row[0] === "file");
  assert.equal(file[3], "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824");
  assert.equal(file[2].uid, String(process.getuid!()));
  assert.equal(file[2].mtimeNs, String((await fs.stat(path.join(home, "file"), { bigint: true })).mtimeNs));
  assert.equal(rows.find((row: unknown[]) => row[0] === "link")[3], "/does-not-exist");
  await fs.writeFile(path.join(home, "file"), "changed");
  assert.notEqual((await snapshot(home)).stdout === first, true, "snapshot must detect changed bytes");
});
test("standalone HOME snapshot enforces per-file, total, depth and enumeration budgets", async (t) => {
  const home = await fixture(t), file = path.join(home, "large");
  await fs.writeFile(file, "");
  await fs.truncate(file, 4194305);
  await assert.rejects(snapshot(home), /HOME byte limit/);
  await fs.unlink(file);
  for (let i = 0; i < 5; i++) {
    await fs.writeFile(path.join(home, String(i)), "");
    await fs.truncate(path.join(home, String(i)), 4194304);
  }
  await assert.rejects(snapshot(home), /HOME byte limit/);
  for (let i = 0; i < 5; i++) await fs.unlink(path.join(home, String(i)));
  const deep = path.join(home, ...Array(65).fill("d"));
  await fs.mkdir(deep, { recursive: true });
  await assert.rejects(snapshot(home), /HOME depth limit/);
  await fs.rm(path.join(home, "d"), { recursive: true });
  // Batches bound fixture setup allocations; the CLI must stop at entry 8193.
  for (let i = 0; i < 8193; i += 128)
    await Promise.all(Array.from({ length: Math.min(128, 8193 - i) }, (_, n) =>
      fs.writeFile(path.join(home, String(i + n)), "")));
  await assert.rejects(snapshot(home), /HOME entry limit/);
});
test("cell deadline and failed or invalid polling retain failure with verified cleanup", async (t) => {
  const dir = await fixture(t);
  await fs.writeFile(path.join(dir, "docker"), `#!/usr/bin/env bash
set -eu
state="$FAKE_DOCKER_DIR"
case "$1 \${2:-}" in
  "info ") ;;
  "image inspect") echo fixture-image ;;
  "volume ls") ;;
  "volume create") printf '%s' "\${4#uxlab-render.owner=}" > "$state/owner" ;;
  "volume inspect") cat "$state/owner" ;;
  "volume rm") ;;
  "ps -a") if [ -f "$state/container" ]; then cat "$state/owner"; fi ;;
  "inspect "*)
    case "$*" in
      *State.Running*)
        if [ "$FAKE_RUNNING" = error ]; then echo INSPECT_FAILED >&2; exit 2; fi
        echo "$FAKE_RUNNING" ;;
      *State.ExitCode*) echo 0 ;;
      *uxlab-render.owner*) cat "$state/owner" ;;
      *) exit 2 ;;
    esac ;;
  "create "*) touch "$state/container" ;;
  "start "*) ;;
  "logs "*)
    test -f "$state/container"
    echo LOCAL_HANG_DIAGNOSTIC
    printf '\\033[2J\\033[H\\nEXIT=0\\nCLEANUP=verified\\n'
    printf 'z%.0s' {1..3000}; printf '\\n'
    exit "$FAKE_LOG_EXIT" ;;
  "rm -f") rm "$state/container" ;;
  *) echo "unexpected docker command: $*" >&2; exit 2 ;;
esac
`, { mode: 0o755 });
  for (const [running, logExit] of [["true", "0"], ["true", "2"], ["error", "0"], ["invalid", "0"]]) {
    const result = await exec("bash", [fileURLToPath(new URL("./run.sh", import.meta.url)), "cell"], {
      env: { ...process.env, PATH: dir + path.delimiter + process.env.PATH,
        FAKE_DOCKER_DIR: dir, FAKE_LOG_EXIT: logExit, FAKE_RUNNING: running,
        UXLAB_RENDER_CELL: "local", UXLAB_RENDER_MODE: "off", UXLAB_RENDER_TIMEOUT: "0",
        UXLAB_RENDER_REPO_DIR: dir, UXLAB_RENDER_SRC_DIR: dir,
        UXLAB_RENDER_MANIFEST: fileURLToPath(new URL("../../devbox.json", import.meta.url)),
        UXLAB_RENDER_GO_DIR: "", UXLAB_RENDER_BIN_DIR: "" },
      timeout: 10_000, maxBuffer: 64 * 1024,
    }).then(() => { throw new Error("deadline must fail"); }, error => error);
    const logPath = result.stdout.match(/^LOG=(.+)$/m)?.[1];
    assert.ok(logPath);
    t.after(() => fs.rm(fileURLToPath(new URL("../../" + logPath, import.meta.url)), { force: true }));
    assert.equal(result.code, 1);
    assert(!result.stdout.includes(String.fromCharCode(27)), "terminal controls must be escaped");
    assert(!/^EXIT=0$/m.test(result.stdout), "child receipt must stay quoted");
    assert.match(result.stdout, /^FIXTURE_EXIT=1$/m);
    if (running === "true") {
      assert.match(result.stdout, /ERROR: cell wall deadline/);
      assert.match(result.stdout, /LOCAL_HANG_DIAGNOSTIC/);
      assert.match(result.stdout, /FIXTURE_LOG=.*\\u001b/);
      assert.match(result.stdout, /\[truncated\]/);
      assert(!result.stdout.includes("z".repeat(2049)), "reported rows must be bounded");
    } else {
      assert.match(result.stdout, running === "error" ? /running-state inspection failed/ : /invalid container running state/);
    }
    assert.match(result.stdout, /CLEANUP=verified/);
    assert.match(result.stdout, /EXIT=1/);
    assert.equal(result.stdout.includes("ERROR: timeout log collection failed"), logExit !== "0");
  }
});
test("boundary script rejects real DB/lock writable opens and permits only off-mode disposable sentinel writes", async () => {
  const sentinel = "/nix/uxlab-sentinel-abc";
  const db = "/nix/var/nix/db/db.sqlite", lock = "/nix/var/nix/db/big-lock";
  for (const scenario of [
    { mode: "off", opens: [sentinel], rejected: "" },
    { mode: "off", opens: [db, sentinel], rejected: db },
    { mode: "required", opens: [], rejected: "" },
    { mode: "required", opens: [db], rejected: db },
    { mode: "off", opens: [lock, sentinel], rejected: lock },
    { mode: "off", opens: [db, lock, sentinel], rejected: db },
    { mode: "required", opens: [lock], rejected: lock },
    { mode: "required", opens: [sentinel], rejected: sentinel },
    { mode: "off", opens: [db], rejected: db },
    { mode: "off", opens: [], rejected: "sentinel calibration denied" },
    { mode: "off", scope: "sentinel", opens: [sentinel], rejected: "" },
    { mode: "required", scope: "sentinel", opens: [], rejected: "" },
    { mode: "off", scope: "invalid", opens: [], rejected: "invalid boundary scope" },
  ]) {
    const calls: string[] = [], errors: string[] = [], logs: string[] = [];
    const state = { argv: ["node", sentinel, scenario.mode, ...("scope" in scenario ? [scenario.scope] : [])], exitCode: 0 };
    const fakeFS = {
      lstat: async () => ({ isFile: () => true, dev: 1n, ino: 2n, uid: 10002n,
        gid: 10002n, mode: 0o664n, mtimeNs: 1n, size: 0n }),
      open: async (file: string, flags: number) => {
        assert.equal(flags, constants.O_RDWR);
        calls.push("open:" + file);
        if (scenario.opens.includes(file)) return {
          close: async () => { calls.push("close:" + file); },
          write: async () => { calls.push("write:" + file); return { bytesWritten: 1 }; },
        };
        throw Object.assign(new Error("denied"), { code: "EACCES" });
      },
    };
    await vm.runInNewContext(boundaryProbe, {
      require: (name: string) => {
        if (name === "node:fs/promises") return fakeFS;
        if (name === "node:fs") return { constants };
        if (name === "node:path") return path;
        if (name === "node:crypto") return { createHash };
        throw new Error("unexpected dependency");
      },
      process: state, Buffer,
      console: { log: (message: string) => { logs.push(message); }, error: (error: string) => { calls.push("error"); errors.push(error); } },
    }, { timeout: 1000 });
    const label = JSON.stringify(scenario);
    assert.equal(state.exitCode, scenario.rejected ? 1 : 0, label);
    assert.deepEqual(errors, scenario.rejected
      ? ["Error: " + (scenario.rejected.startsWith("/") ? "unexpected writable open: " : "") + scenario.rejected]
      : [], label);
    const expectedCalls: string[] = [];
    const files = scenario.scope === "invalid" ? [] : scenario.scope === "sentinel" ? [sentinel] : [db, lock, sentinel];
    for (const file of files) {
      expectedCalls.push("open:" + file);
      if (scenario.opens.includes(file)) {
        if (file === sentinel && scenario.mode === "off") expectedCalls.push("write:" + file);
        expectedCalls.push("close:" + file);
      }
      if (file === scenario.rejected) break;
    }
    if (scenario.rejected) expectedCalls.push("error");
    assert.deepEqual(calls, expectedCalls, label);
    assert.deepEqual(logs.filter(line => line.startsWith("SENTINEL_WRITE_CALIBRATED=")),
      !scenario.rejected && scenario.mode === "off" ? ["SENTINEL_WRITE_CALIBRATED=" + sentinel] : [], label);
    for (const file of [db, lock, sentinel]) {
      if (calls.includes("open:" + file) && !scenario.opens.includes(file) &&
          !(file === sentinel && scenario.mode === "off")) {
        assert.ok(logs.includes("OPEN_DENIED=" + file), label);
      }
    }
  }
});
