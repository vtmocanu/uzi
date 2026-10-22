// M1: real repo opt-in provisioning through the production Codex command boundary.
import assert from "node:assert/strict";
import { displayJSON } from "./display.js";
import { execFile, spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";
import type { RunContext } from "../../agent/src/executor.js";
import { readRegularFile, sourceId, MANIFEST_MAX_BYTES, HELPER_MAX_BYTES } from "./inputs.js";

const exec = promisify(execFile);
const src = "/app/src";
const load = (name: string) => import(pathToFileURL(path.join(src, name)).href);
const started = performance.now();
const record = (kind: string, fields: Record<string, unknown>) =>
  console.log(displayJSON({ kind, elapsedMs: Math.round(performance.now() - started), ...fields }));

async function main(): Promise<void> {
  if (process.env.UZI_UID_SPLIT !== "1" || process.getuid?.() !== 10001) {
    console.error("SKIP: real entrypoint did not establish worker uid 10001 / runner split");
    process.exitCode = 77;
    return;
  }
  process.umask(0o002);
  const provision = await load("provision-run.js");
  const codex = await load("codex/codex-executor.js");
  const launcher = await load("codex/launcher.js");
  const registry = await load("codex/registry.js");
  const runner = await load("runner-uid.js");
  const { createLogger } = await load("log.js");
  const warnings: string[] = [];
  const log = createLogger("info");
  const originalWarn = log.warn.bind(log);
  log.warn = (message: string, fields?: Record<string, unknown>) => {
    warnings.push(message);
    originalWarn(message, fields);
  };
  const runAsRunner = async (command: string, args: string[]) => {
    const wrapped = runner.runnerCommand(command, args);
    return exec(wrapped.command, wrapped.args, {
      env: { PATH: runner.runnerPath(process.env), HOME: "/data/agent-home",
        TMPDIR: runner.runnerTmpdir(process.env) },
      timeout: 30_000, maxBuffer: 1024 * 1024,
    });
  };
  assert.equal((await runAsRunner("/usr/bin/id", ["-u"])).stdout.trim(), "10002");
  const helperIds: Record<string, string> = {};
  for (const name of ["uzi-codex-supervisor", "uzi-codex-fileop", "uzi-codex-command-sandbox"]) {
    helperIds[name] = createHash("sha256").update(await readRegularFile(`/usr/local/bin/${name}`, HELPER_MAX_BYTES)).digest("hex");
  }
  record("identity", {
    imageId: process.env.UXLAB_RENDER_IMAGE_ID,
    imageVersion: process.env.UZI_AGENT_VERSION,
    sourceSha256: await sourceId(src), helpers: helperIds, node: process.version,
    devbox: (await runAsRunner("devbox", ["version"])).stdout.trim(),
    nix: (await runAsRunner("nix", ["--version"])).stdout.trim(),
    workerUid: process.getuid?.(), runnerUid: 10002,
  });
  const probe = spawnSync("/usr/local/bin/uzi-codex-command-sandbox", ["--probe"], {
    encoding: "utf8", timeout: 10_000, maxBuffer: 1024 * 1024,
  });
  assert.ifError(probe.error);
  record("landlock", { status: probe.status, stderr: probe.stderr });
  assert.equal(probe.status, 0, "required Landlock must be available; never skip required mode");

  const runId = randomUUID();
  const checkout = `/data/runner/uxlab-${runId}`;
  const wrapped = runner.runnerCommand("/bin/sh", ["-ceu",
    'mkdir "$1"; chmod 2770 "$1"; git init -q "$1"', "sh", checkout]);
  await exec(wrapped.command, wrapped.args, { timeout: 10_000, maxBuffer: 1024 * 1024 });
  assert.equal((await fs.stat(checkout)).uid, 10002);
  const manifest = await readRegularFile("/work/root-devbox.json", MANIFEST_MAX_BYTES);
  await fs.writeFile(path.join(checkout, "devbox.json"), manifest);
  record("manifest", { sha256: createHash("sha256").update(manifest).digest("hex") });
  const events: string[] = [];
  const context = {
    runId, worktreePath: checkout,
    config: { tool_packages: [], repo_devbox_opt_in: true },
    emit: (message: { payload: Record<string, unknown> }) => {
      events.push(String(message.payload.text));
      record("provision-event", { text: message.payload.text });
    },
  } as RunContext;
  const filesystems: Record<string, unknown> = {};
  for (const dir of ["/data", "/nix", "/tmp"]) {
    const stat = await fs.statfs(dir);
    filesystems[dir] = { blockSize: stat.bsize, freeBlocks: stat.bavail, freeInodes: stat.ffree };
  }
  record("filesystems", filesystems);
  const provisionStarted = performance.now();
  // No provision/run/probe dependency injection: this must perform the real install.
  const result = await provision.provisionRunTools(context, {
    provisionRoot: "/data/provision", homeDir: "/data/agent-home", log,
  });
  record("provision", { durationMs: Math.round(performance.now() - provisionStarted), ...result });
  const { extractRepoDevboxPackages } = await load("repo-tools.js");
  const packages: string[] = await extractRepoDevboxPackages(checkout);
  assert(packages.includes("ruby@4.0.6"), "canary requires the real root Ruby package");
  const expectedEvents = [
    `merged ${packages.length} package(s) from this repo's devbox.json`,
    `provisioning ${packages.length} tool(s): ${packages.join(", ")}`,
    "tools provisioned",
  ];
  assert.equal(events.length, expectedEvents.length, "unexpected provisioning event count");
  for (let index = 0; index < expectedEvents.length; index++) {
    assert(events[index] === expectedEvents[index],
      `provision event ${index} differs; see provision-event records (degradation is failure)`);
  }
  assert.deepEqual(warnings, [], "provisioning must not warn or degrade");
  assert.equal(result.provisionDir, `/data/provision/${runId}`);
  assert(result.toolEnv.PATH?.includes(`/data/provision/${runId}/.devbox`));
  const commandEnv = codex.withCommandGitTrust(
    codex.buildCommandEnv("/tmp", result.toolEnv),
    await codex.canonicalCheckoutPath(checkout),
  );
  const homes = new Set<string>();
  // Four serial, bounded commands; any failure stops the canary. Markers make reuse
  // of a private HOME/TMPDIR observable on the next invocation.
  for (const mode of ["off", "required"] as const) {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "off" ? 1 : 2));
    const spawn = codex.makeDefaultSpawnCommand(
      roots, launcher.launchCodexEffectRoot, 5000, checkout, commandEnv, mode,
      log, undefined, 30_000,
    );
    for (let invocation = 0; invocation < 2; invocation++) {
      const commandStarted = performance.now();
      const output = await spawn(["/bin/sh", "-ceu", [
        'test "$HOME" = "$TMPDIR"',
        'entries="$(ls -A "$HOME")"; test -z "$entries"',
        'test "$(stat -c %u "$HOME")" = 10003',
        'test "$(stat -c %a "$HOME")" = 700',
        'test "$(stat -c %a "$TMPDIR")" = 700',
        'printf "UID=%s\\nHOME=%s\\nTMPDIR=%s\\nHOME_MODE=%s\\nTMPDIR_MODE=%s\\n" "$(id -u)" "$HOME" "$TMPDIR" "$(stat -c %a "$HOME")" "$(stat -c %a "$TMPDIR")"',
        "command -v ruby && ruby --version",
        ...(packages.includes("charm-freeze@0.2.2") ? ["command -v freeze && freeze --version"] : []),
        'touch "$HOME/uxlab-marker"',
      ].join("\n")], { cwd: checkout });
      record("canary", { mode, invocation, durationMs: Math.round(performance.now() - commandStarted), ...output });
      assert.equal(output.code, 0, `${mode} canary failed: ${output.stderr}`);
      assert.match(output.stdout, /^UID=10003$/m);
      assert.match(output.stdout, /^HOME_MODE=700$/m);
      assert.match(output.stdout, /^TMPDIR_MODE=700$/m);
      const home = /^HOME=(.+)$/m.exec(output.stdout)?.[1];
      const tmp = /^TMPDIR=(.+)$/m.exec(output.stdout)?.[1];
      assert(home?.startsWith("/tmp/uzi-codex-command-"));
      assert.equal(tmp, home);
      assert(!homes.has(home), "each invocation needs a fresh HOME/TMPDIR");
      homes.add(home);
      const ruby = output.stdout.split("\n").find((line: string) => line.startsWith("/data/provision/"));
      assert(ruby?.startsWith(`/data/provision/${runId}/.devbox/`) && ruby.endsWith("/ruby"),
        "command -v must resolve Ruby through this run's provisioned devbox");
      assert.match(output.stdout, /^ruby 4\.0\.6(?:\s|$)/m);
      if (packages.includes("charm-freeze@0.2.2")) assert.match(output.stdout, /\b0\.2\.2\b/);
      assert.equal(roots.hasLiveCommandRoot(), false);
    }
  }
  assert.deepEqual(warnings, []);
  record("result", { status: "PASS", action: "canary", commands: homes.size });
}

main().catch((error) => {
  record("result", { status: "FAIL", error: String(error), stack: error?.stack });
  process.exitCode = 1;
});
