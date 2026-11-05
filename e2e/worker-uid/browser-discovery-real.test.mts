// Real-browser acceptance for the protected browser discovery paths (issue #3907, M3).
//
// Runs in the dedicated browser-proof container of run.sh: root-started through the real worker
// entrypoint (worker uid 10001, UZI_UID_SPLIT=1, runner uid 10002, command uid 10003) with the
// image's /nix UNMASKED (a labelled named volume populated from the image), because the baked
// Chromium lives under /opt/uzi-toolchain -> /nix/store. Every other worker-uid container masks
// /nix, so only this one can launch a browser.
//
// Both cases launch the baked Chromium through nothing but the variables the production env
// builders export (CHROME_PATH and AGENT_BROWSER_EXECUTABLE_PATH, both the uzi-chromium wrapper)
// and prove the result by decoding the screenshot: a solid red block on a white page must come
// back red. The commands pass no browser flags of their own and no --user-data-dir.
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import zlib from "node:zlib";
import { guardedCommandEnv } from "../../agent/src/claude-command/env.js";
import { GUARDED_COMMAND_TOOL } from "../../agent/src/claude-command/grants.js";
import { ClaudeCommandLane, type ClaudeCommandLaneOptions } from "../../agent/src/claude-command/lane.js";
import type { CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { buildCommandEnv, makeDefaultSpawnCommand } from "../../agent/src/codex/codex-executor.js";
import { launchCodexEffectRoot } from "../../agent/src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../agent/src/codex/registry.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests } from "../../agent/src/run-quiescence.js";
import { runnerCommand } from "../../agent/src/runner-uid.js";
import { buildSdkEnv } from "../../agent/src/sdk-env.js";
import { workerSpawnEnv } from "../../agent/src/worker-spawn-mark.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
// The browser-proof run preloads the hermetic (empty) quiescence view into every test process;
// the Claude case clears it to scan the real procfs and puts it back afterwards.
import { restoreHermeticView } from "../../agent/test/setup/hermetic-proc.js";

// Independent scan of the REAL procfs (no hermetic view): a live process whose real uid is the given
// uid and whose comm or argv[0] basename looks like a browser. Polled up to settleMs before failing.
const BROWSER_NAME = /chrom|agent-browser|crashpad/i;
function browserProcesses(uid: number): string[] {
  const found: string[] = [];
  for (const entry of fs.readdirSync("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const status = fs.readFileSync(`/proc/${entry}/status`, "utf8");
      const real = /^Uid:\s+(\d+)/m.exec(status);
      if (!real || Number(real[1]) !== uid) continue;
      if (/^State:\s+Z/m.test(status)) continue; // a zombie is not a live process
      const comm = fs.readFileSync(`/proc/${entry}/comm`, "utf8").trim();
      const argv0 = path.basename(fs.readFileSync(`/proc/${entry}/cmdline`, "utf8").split("\0")[0] ?? "");
      if (BROWSER_NAME.test(comm) || BROWSER_NAME.test(argv0)) found.push(`${entry}:${comm}:${argv0}`);
    } catch (err) {
      // Only "the process exited while being read" is benign; any other read failure on a candidate
      // process must not let a live browser pass unseen.
      const code = (err as NodeJS.ErrnoException).code;
      if (code !== "ENOENT" && code !== "ESRCH") found.push(`${entry}:unreadable:${String(code)}`);
    }
  }
  return found;
}
async function assertNoBrowserProcesses(uid: number, settleMs = 5000): Promise<void> {
  const deadline = Date.now() + settleMs;
  let found = browserProcesses(uid);
  while (found.length > 0 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 200));
    found = browserProcesses(uid);
  }
  assert.deepEqual(found, [], `browser processes still live for uid ${String(uid)} (independent procfs scan)`);
}

// Required inventory must reject a skipped run: only the production worker with the split
// runner identity, the image supervisor and the unmasked /nix can supply this evidence.
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== 10001 ||
  process.env.UZI_UID_SPLIT !== "1"
  ? "requires Linux worker UID 10001 with the production runner 10002 split, the image supervisor and an unmasked /nix"
  : false;

const META = "claudecode/toolUseId";
const QUIET_LOG = { debug() {}, info() {}, warn() {} };
const HELPER_ENV = workerSpawnEnv({ PATH: "/usr/bin:/bin", LANG: "C" });
// A solid red 200x200 block at the page origin on a white page.
const PAGE_HTML = '<!doctype html><body style="margin:0;background:#fff"><div style="width:200px;height:200px;background:rgb(255,0,0)"></div></body>';
const SHOT_FLAGS = "--headless --window-size=320,240";
const WRAPPER = "/usr/local/bin/uzi-chromium";

/** Decode an 8-bit RGB/RGBA non-interlaced PNG and return the pixel reader. */
function decodePng(png: Buffer) {
  assert.deepEqual([...png.subarray(0, 8)], [137, 80, 78, 71, 13, 10, 26, 10], "PNG signature");
  let width = 0;
  let height = 0;
  let channels = 0;
  const idat: Buffer[] = [];
  for (let offset = 8; offset + 12 <= png.length;) {
    const length = png.readUInt32BE(offset);
    const type = png.toString("latin1", offset + 4, offset + 8);
    const data = png.subarray(offset + 8, offset + 8 + length);
    if (type === "IHDR") {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      assert.equal(data.readUInt8(8), 8, "8-bit samples");
      assert.ok([2, 6].includes(data.readUInt8(9)), `RGB or RGBA, got color type ${String(data.readUInt8(9))}`);
      assert.equal(data.readUInt8(12), 0, "not interlaced");
      channels = data.readUInt8(9) === 2 ? 3 : 4;
    } else if (type === "IDAT") idat.push(data);
    offset += 12 + length;
  }
  assert.ok(width > 0 && height > 0 && idat.length > 0, "IHDR and IDAT present");
  const raw = zlib.inflateSync(Buffer.concat(idat));
  const stride = width * channels;
  const pixels = Buffer.alloc(stride * height);
  for (let y = 0; y < height; y++) {
    const filter = raw.readUInt8(y * (stride + 1));
    for (let i = 0; i < stride; i++) {
      const x = raw.readUInt8(y * (stride + 1) + 1 + i);
      const a = i >= channels ? pixels.readUInt8(y * stride + i - channels) : 0;
      const b = y > 0 ? pixels.readUInt8((y - 1) * stride + i) : 0;
      const c = y > 0 && i >= channels ? pixels.readUInt8((y - 1) * stride + i - channels) : 0;
      const p = a + b - c;
      const [pa, pb, pc] = [Math.abs(p - a), Math.abs(p - b), Math.abs(p - c)];
      const predictors = [0, a, b, (a + b) >> 1, pa <= pb && pa <= pc ? a : pb <= pc ? b : c];
      assert.ok(filter <= 4, `PNG filter ${String(filter)}`);
      pixels.writeUInt8((x + predictors[filter]!) & 255, y * stride + i);
    }
  }
  return { width, height, rgb: (px: number, py: number) => [0, 1, 2].map((k) => pixels.readUInt8(py * stride + px * channels + k)) };
}

/** The screenshot exists and some pixel within 5 px of (50, 50) is approximately rgb(255, 0, 0). */
function assertRendersRed(file: string, what: string): void {
  const image = decodePng(fs.readFileSync(file));
  assert.ok(image.width > 55 && image.height > 55, `${what}: screenshot ${String(image.width)}x${String(image.height)} is too small`);
  const red = [];
  for (let y = 45; y <= 55; y++) for (let x = 45; x <= 55; x++) {
    const [r, g, b] = image.rgb(x, y) as [number, number, number];
    if (r >= 240 && g <= 15 && b <= 15) red.push([x, y]);
  }
  assert.ok(red.length > 0, `${what}: no red pixel near (50,50); got rgb(${image.rgb(50, 50).join(",")})`);
}

/** Run a short helper at the runner uid (the only uid that may remove runner-owned files). */
function runnerRun(command: string, args: readonly string[]) {
  const wrapped = runnerCommand(command, args);
  return spawnSync(wrapped.command, wrapped.args, { env: HELPER_ENV, encoding: "utf8", timeout: 20_000 });
}

function newContext(commands: WorkerMemoryCommands): CapturedMemoryContext {
  const flight = {};
  const context = commands.begin({
    run_id: randomUUID(), worker_id: randomUUID(), register_nonce: "browser-real",
    claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  return { commands, context, flight, lifecycleRevision: 1 };
}

type HookArg = Parameters<ClaudeCommandLane["hook"]>[0];
let seq = 0;
/** One model call: the PreToolUse hook (attribution record), then the MCP handler. */
async function modelCall(lane: ClaudeCommandLane, cwd: string, command: string) {
  const id = `toolu_browser_${String(++seq)}_${randomUUID().slice(0, 8)}`;
  const args = { command, timeout: 90_000 };
  const hook = await lane.hook({
    hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: args, tool_use_id: id,
    session_id: "s", transcript_path: "/t", cwd,
  } as HookArg, id, { signal: new AbortController().signal });
  assert.deepEqual(hook, {}, "the hook admits the call");
  return lane.handle(args, { _meta: { [META]: id } });
}

describe("real browser discovery under the worker identities", { skip: INIT_SKIP }, () => {
  it("launches Chromium and agent-browser from the exported discovery paths in the Claude lane and leaves no browser process behind", { timeout: 200_000 }, async () => {
    // A worker-owned 0777 run dir, created the way claude-command-real creates its rigs; the
    // runner uid writes the HOME state and the screenshots into it.
    const dir = fs.mkdtempSync(path.join("/tmp", "uzi-browser-real-"));
    fs.chmodSync(dir, 0o777);
    const home = path.join(dir, "home");
    fs.mkdirSync(home);
    fs.chmodSync(home, 0o777);
    const page = path.join(dir, "page.html");
    fs.writeFileSync(page, PAGE_HTML, { mode: 0o644 });
    const url = `file://${page}`;
    const shot = (name: string) => path.join(dir, name);
    // agent-browser's daemon socket lives under $HOME/.agent-browser; a Unix socket path is limited
    // to about 103 bytes. This HOME is short, so no AGENT_BROWSER_SOCKET_DIR override is needed.
    const session = "ab3907";
    assert.ok(path.join(home, ".agent-browser", `${session}.sock`).length < 90, "the default agent-browser socket path fits a Unix socket");

    let lane: ClaudeCommandLane | undefined;
    const attempt = newRunAttempt(randomUUID(), 1, dir, () => lane?.recordedRoots() ?? []);
    // The production command env: buildSdkEnv for the CLI, guardedCommandEnv for each command.
    const cliEnv = buildSdkEnv("browser-proof-fake-token", home, {}, undefined, attempt);
    const commandEnv = guardedCommandEnv(cliEnv);
    assert.equal(commandEnv.CHROME_PATH, WRAPPER);
    assert.equal(commandEnv.AGENT_BROWSER_EXECUTABLE_PATH, WRAPPER);
    const errors: unknown[] = [];
    try {
      const options: ClaudeCommandLaneOptions = {
        worktree: dir, cliEnv, secretPaths: [], dockerWired: false, secrets: [], log: QUIET_LOG,
        capturedMemory: () => newContext(new WorkerMemoryCommands()), tripTurn: () => undefined,
        graceMs: 1000, cleanupMs: 10_000,
      };
      lane = new ClaudeCommandLane(options);
      lane.beginTurn();
      const run = async (command: string, what: string) => {
        const result = await modelCall(lane!, dir, command);
        const text = result.content.map((block) => ("text" in block ? block.text : "")).join("");
        assert.equal(result.isError, false, `${what}: ${text}`);
        assert.match(text, /^Exit code: 0\n/, `${what}: ${text}`);
      };
      const profiles = () => fs.readdirSync(path.join(home, ".uzi-chromium")).filter((name) => name.startsWith("profile."));

      await run(`"$CHROME_PATH" ${SHOT_FLAGS} --screenshot=${shot("chrome-path.png")} ${url} && chmod a+r ${shot("chrome-path.png")}`, "CHROME_PATH");
      await run(`"$AGENT_BROWSER_EXECUTABLE_PATH" ${SHOT_FLAGS} --screenshot=${shot("ab-path.png")} ${url} && chmod a+r ${shot("ab-path.png")}`, "AGENT_BROWSER_EXECUTABLE_PATH");
      // One command: the daemon must outlive each agent-browser call, and the lane drains the whole
      // tree when the command ends.
      await run([
        "set -e",
        `export AGENT_BROWSER_IDLE_TIMEOUT_MS=2000`,
        `agent-browser --session ${session} open ${url}`,
        `agent-browser --session ${session} screenshot ${shot("agent-browser.png")}`,
        `agent-browser --session ${session} close`,
        `chmod a+r ${shot("agent-browser.png")}`,
      ].join("\n"), "agent-browser through the shim");

      const before = new Set(profiles());
      await Promise.all(["a", "b"].map((id) =>
        run(`"$CHROME_PATH" ${SHOT_FLAGS} --screenshot=${shot(`concurrent-${id}.png`)} ${url} && chmod a+r ${shot(`concurrent-${id}.png`)}`, `concurrent launch ${id}`)));
      const created = profiles().filter((name) => !before.has(name));
      assert.equal(created.length, 2, `two concurrent launches without --user-data-dir get two private profiles: ${created.join(",")}`);

      for (const name of ["chrome-path", "ab-path", "agent-browser", "concurrent-a", "concurrent-b"]) {
        assertRendersRed(shot(`${name}.png`), name);
      }
    } catch (error) {
      errors.push(error);
    }
    // Cleanup is always attempted, and an unconfirmed one fails the test even when every command passed.
    if (lane) {
      try { await lane.quiesce(20_000); } catch (error) { errors.push(error); }
      if (lane.poisoned) errors.push(new Error("the lane is poisoned: a command's disposal was not confirmed"));
      // The real runner-uid quiescence helper reads the real procfs: clear the hermetic empty view.
      setQuiescenceViewForTests(undefined);
      try {
        const registry = new LiveAttemptRegistry();
        registry.add(attempt);
        const own = await quiesceRunAttempt({
          mode: "own", attempt, cloneKey: attempt.cloneKey, targetPaths: [dir],
          processes: true, dockerHost: undefined, registry, otherClaimInFlight: false,
        });
        // Nothing may be left for the reap to kill: browser and agent-browser daemon trees are gone.
        if (own.process?.state !== "quiescent" || own.process.killed.length > 0) {
          errors.push(new Error(`browser processes survived the lane: ${JSON.stringify(own.process)}`));
        }
      } catch (error) { errors.push(error); } finally { restoreHermeticView(); }
      // Independent of the helpers above: scan the real procfs for any live runner-uid browser.
      try { await assertNoBrowserProcesses(10002); } catch (error) { errors.push(error); }
    }
    try {
      // The runner made the HOME state and the shots; remove them as the runner, then the dir.
      runnerRun("sh", ["-c", 'rm -rf -- "$1"/* "$1"/.[!.]* "$1"/..?*', "sh", dir]);
      fs.rmSync(dir, { recursive: true, force: true });
    } catch (error) { errors.push(error); }
    if (errors.length > 0) throw errors.length === 1 ? errors[0] : new AggregateError(errors, "browser lane proof and cleanup failures");
  });

  it("launches Chromium from the exported discovery paths in an off-mode Codex command and closes the registry cleanly", { timeout: 150_000 }, async () => {
    // The production fixture for real worker-uid clone dirs: a worker-owned, runner-group 3775 root
    // that it removes again as each real uid.
    const { runnerTeardownFixture } = await import("../../agent/test/runner-teardown-fixtures.js");
    await runnerTeardownFixture(async (root) => {
      // The command identity (10003, supplementary group runner) works in a 2770 runner-group clone dir.
      const worktree = path.join(root, "worktree");
      fs.mkdirSync(worktree, { mode: 0o2770 });
      fs.chmodSync(worktree, 0o2770);
      fs.writeFileSync(path.join(worktree, "page.html"), PAGE_HTML, { mode: 0o644 });
      const url = `file://${path.join(worktree, "page.html")}`;
      const shots = [path.join(worktree, "chrome-path.png"), path.join(worktree, "ab-path.png")];
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(3907));
      const spawnCommand = makeDefaultSpawnCommand(
        registry, (spec) => launchCodexEffectRoot(spec), 15_000, worktree,
        buildCommandEnv("/tmp", {}, undefined, true), "off",
      );
      const errors: unknown[] = [];
      try {
        const result = await spawnCommand(["/bin/sh", "-c", [
          "set -eu",
          `"$CHROME_PATH" ${SHOT_FLAGS} --screenshot=${shots[0]} ${url}`,
          `"$AGENT_BROWSER_EXECUTABLE_PATH" ${SHOT_FLAGS} --screenshot=${shots[1]} ${url}`,
          `chmod a+r ${shots.join(" ")}`,
          'ls -d "$HOME"/.uzi-chromium/profile.* >/dev/null',
          "id -u",
        ].join("\n")], { cwd: worktree });
        assert.equal(result.code, 0, `command failed: ${result.stderr}`);
        assert.equal(result.stdout.trim().split("\n").at(-1), "10003", "the command ran as the command uid");
        shots.forEach((file, index) => { assertRendersRed(file, ["CHROME_PATH", "AGENT_BROWSER_EXECUTABLE_PATH"][index]!); });
      } catch (error) {
        errors.push(error);
      }
      // The supported close sequence: an unconfirmed step fails the test even when the command passed.
      let drained = false;
      try {
        const closed = await registry.quiesceChildren(10_000);
        if (closed.kind === "quiescent") drained = (await registry.reapProcesses(15_000, closed.epoch)).kind === "observed_empty";
        if (!drained) errors.push(new Error(`the registry did not drain: ${JSON.stringify(closed)}`));
      } catch (error) { errors.push(error); }
      try {
        const disposed = await registry.disposeTools(15_000);
        if (!(disposed.kind === "disposed" && registry.acceptsTerminalDisposal(disposed) && !registry.isPoisoned())) {
          errors.push(new Error(`terminal disposal was not accepted: ${JSON.stringify(disposed)}`));
        }
      } catch (error) { errors.push(error); }
      // Independent real-procfs scan for any live command-uid browser after the close sequence.
      try { await assertNoBrowserProcesses(10003); } catch (error) { errors.push(error); }
      if (errors.length > 0) throw errors.length === 1 ? errors[0] : new AggregateError(errors, "codex lane proof and cleanup failures");
    });
  });
});
