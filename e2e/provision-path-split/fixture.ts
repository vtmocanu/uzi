import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";

const src = process.env.PROVISION_SPLIT_SRC ?? "/app/src";
const load = async (name: string) => import(pathToFileURL(path.join(src, name)).href);
const exec = promisify(execFile);
const root = `/data/runner/provision-path-split-${process.pid}`;
const worktree = path.join(root, "worktree");
const provisionHome = "/data/provision";
const privateBin = path.join(provisionHome, ".nix-profile/bin");
const runnerPrivateBin = path.join(root, "runner-private-bin");
const missing = "uzi-path-split-absent-command";
const retained = "uzi-path-split-private-command";
const nixBin = "/nix/var/nix/profiles/default/bin";
const provisionBin = `/nix/provision-path-split-${process.pid}/bin`;
const provisioned = "uzi-provision-path-tool";

async function resolutionCode(
  wrap: (command: string, args: string[]) => { command: string; args: string[] },
  target: string,
  env: NodeJS.ProcessEnv,
): Promise<string> {
  const script = 'const r = require("node:child_process").spawnSync(process.argv[1], [], {env: process.env}); console.log(r.error?.code ?? (r.status === 0 ? "OK" : `EXIT_${r.status}`));';
  const child = wrap(process.execPath, ["-e", script, target]);
  return (await exec(child.command, child.args, { cwd: worktree, env })).stdout.trim();
}

async function main(): Promise<void> {
  if (process.env.UZI_UID_SPLIT !== "1" || process.getuid?.() !== 10001) {
    console.log("SKIP: root entrypoint did not establish worker/runner uid split");
    process.exitCode = 77;
    return;
  }
  const provision = await load("provision.js");
  const sdk = await load("sdk-env.js");
  const codex = await load("codex/codex-executor.js");
  const launcher = await load("codex/launcher.js");
  const registry = await load("codex/registry.js");
  const runner = await load("runner-uid.js");

  await fs.mkdir(worktree, { recursive: true, mode: 0o2770 });
  await fs.chmod(root, 0o2770);
  await fs.chmod(worktree, 0o2770);
  await fs.access(provisionHome, 3);
  const privateLocal = path.join(provisionHome, ".local");
  await fs.mkdir(path.join(privateLocal, "share/nix/profile/bin"), { recursive: true });
  await fs.chmod(privateLocal, 0o700);
  await fs.symlink(".local/share/nix/profile", path.join(provisionHome, ".nix-profile"));
  const createRunnerPrivate = runner.runnerCommand("/bin/sh", ["-c",
    'mkdir -p "$1"; chmod 700 "$1"; printf "#!/bin/sh\\nexit 0\\n" > "$1/$2"; chmod 755 "$1/$2"',
    "sh", runnerPrivateBin, retained]);
  await exec(createRunnerPrivate.command, createRunnerPrivate.args, { cwd: worktree, env: { PATH: "/usr/bin:/bin" } });
  await fs.access(path.join(nixBin, "nix"), 1);
  const createProvisioned = runner.runnerCommand("/bin/sh", ["-c",
    'mkdir -p "$1"; chmod 755 "$1"; printf "#!/bin/sh\\nprintf provisioned\\n" > "$1/$2"; chmod 755 "$1/$2"',
    "sh", provisionBin, provisioned]);
  await exec(createProvisioned.command, createProvisioned.args, { cwd: worktree, env: { PATH: "/usr/bin:/bin" } });
  const imagePath = process.env.UZI_RUNNER_PATH;
  assert.ok(imagePath?.includes(nixBin));
  const rawPath = `${privateBin}:${runnerPrivateBin}:${provisionBin}:${imagePath}`;
  const rawEnv = { PATH: rawPath };
  for (const [name, wrap] of [["runner", runner.runnerCommand], ["runner-cmd", runner.commandRootCommand]] as const) {
    assert.equal(await resolutionCode(wrap, missing, rawEnv), "EACCES", `${name}: raw missing`);
  }
  assert.equal(await resolutionCode(runner.runnerCommand, retained, rawEnv), "OK", "runner-private executable");
  assert.equal(await resolutionCode(runner.commandRootCommand, retained, rawEnv), "EACCES", "runner-cmd private executable");

  const messages: string[] = [];
  const toolEnv = (await provision.provisionTools(
    { packages: [], runDir: path.join(root, "run"), homeDir: path.join(root, "home") },
    {
      log: {
        info: (message: string) => { messages.push(message); },
        warn: (message: string) => { throw new Error(`unexpected warning: ${message}`); },
      },
      run: async (_cmd: string, args: string[]) => ({
        stdout: args[0] === "shellenv" ? `export PATH="${rawPath}";\n` : "",
        stderr: "",
      }),
      processEnv: process.env,
    },
  )).toolEnv as Record<string, string>;
  assert.ok(messages.includes("removed inaccessible PATH entries"));
  assert.ok(!toolEnv.PATH?.split(":").includes(privateBin));
  assert.ok(!toolEnv.PATH?.split(":").includes(runnerPrivateBin));
  assert.ok(toolEnv.PATH?.split(":").includes(provisionBin));
  for (const [name, wrap] of [["runner", runner.runnerCommand], ["runner-cmd", runner.commandRootCommand]] as const) {
    assert.equal(await resolutionCode(wrap, missing, { PATH: toolEnv.PATH }), "ENOENT", `${name}: filtered missing`);
    assert.equal(await resolutionCode(wrap, retained, { PATH: toolEnv.PATH }), "ENOENT", `${name}: filtered private executable`);
  }
  for (const [name, wrap] of [["runner", runner.runnerCommand], ["runner-cmd", runner.commandRootCommand]] as const) {
    assert.equal(await resolutionCode(wrap, provisioned, { PATH: toolEnv.PATH }), "OK", `${name}: provisioned tool`);
  }
  console.log("PASS: both identities get raw EACCES and filtered ENOENT; provisioned tool executes");

  const sdkEnv = sdk.buildSdkEnv("canary", path.join(root, "home"), toolEnv);
  const checkEnv = sdk.buildCheckEnv(process.env, path.join(root, "home"), toolEnv);
  const commandEnv = codex.buildCommandEnv("/tmp", toolEnv);
  assert.equal(sdkEnv.PATH, toolEnv.PATH, "SDK must preserve provisioned PATH order");
  assert.equal(checkEnv.PATH, toolEnv.PATH, "check must preserve provisioned PATH order");
  const fixedPrefix = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin";
  assert.equal(commandEnv.PATH, `${fixedPrefix}:${toolEnv.PATH}`, "Codex must keep its fixed prefix first");
  console.log("PASS: SDK/check provisioned ordering and Codex fixed-prefix ordering");

  const profile = path.join(provisionHome, ".devbox/nix/profile/default/bin");
  const create = runner.runnerCommand("/bin/sh", ["-c",
    'mkdir -p "$1"; chmod 755 "$1"; printf "#!/bin/sh\\nexit 0\\n" > "$1/profile-tool"; chmod 755 "$1/profile-tool"',
    "sh", profile]);
  await exec(create.command, create.args, { cwd: worktree, env: { PATH: "/usr/bin:/bin" } });
  const profileEnv = { PATH: `${profile}:/usr/bin:/bin` };
  const profileRunner = await resolutionCode(runner.runnerCommand, "profile-tool", profileEnv);
  const profileCommand = await resolutionCode(runner.commandRootCommand, "profile-tool", profileEnv);
  console.log(`OBSERVATION: /data/provision/.devbox profile runner=${profileRunner} runner-cmd=${profileCommand}`);
  assert.equal(profileRunner, "OK");
  assert.equal(profileCommand, "OK", "runner-cmd can search the devbox profile before sandboxing");

  const probe = spawnSync("/usr/local/bin/uzi-codex-command-sandbox", ["--probe"], { encoding: "utf8" });
  if (probe.error) throw probe.error;
  if (probe.status !== 0 && probe.status !== 10) throw new Error(`sandbox --probe: ${probe.status}: ${probe.stderr}`);
  const execute = async (mode: "best-effort" | "required") => {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "required" ? 169 : 168));
    const spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, worktree, commandEnv, mode);
    const script = `const { spawnSync } = require("node:child_process");
const missing = spawnSync(process.argv[1], [], { env: process.env });
const tool = spawnSync("${provisioned}", [], { env: process.env, encoding: "utf8" });
console.log(JSON.stringify({ missing: missing.error?.code ?? "OK", tool: tool.error?.code ?? (tool.status === 0 && /provisioned/.test(tool.stdout) ? "OK" : "FAIL") }));`;
    const result = await spawn([process.execPath, "-e", script, missing], { cwd: worktree });
    assert.equal(result.code, 0, `${mode}: ${result.stderr}`);
    assert.deepEqual(JSON.parse(result.stdout.trim()), { missing: "ENOENT", tool: "OK" }, `${mode}: ${result.stderr}`);
    if (mode === "best-effort") {
      const profileScript = 'const r = require("node:child_process").spawnSync("profile-tool", [], { env: process.env }); console.log(r.error?.code ?? (r.status === 0 ? "OK" : `EXIT_${r.status}`));';
      const profileResult = await spawn([process.execPath, "-e", profileScript], {
        cwd: worktree,
        env: { ...commandEnv, PATH: `${commandEnv.PATH}:${profile}` },
      });
      assert.equal(profileResult.code, 0, profileResult.stderr);
      console.log(`OBSERVATION: /data/provision/.devbox profile inside Codex sandbox: ${profileResult.stdout.trim()}`);
    }
    assert.equal(roots.hasLiveCommandRoot(), false);
    console.log(`PASS: Codex ${mode} via supervisor and runner-cmd`);
  };
  await execute("best-effort");
  if (probe.status === 0) await execute("required");
  else console.log("SKIP: Codex required mode; sandbox --probe returned 10");
}
main().catch((error: unknown) => { console.error(error); process.exitCode = 1; });
