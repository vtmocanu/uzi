// Issue #4416 M3: a Docker login in one sandboxed Codex command persists for the run's later
// commands through the per-run `<runHome>/.docker` the command sandbox creates and Landlock-grants.
//
// Opt-in packaged Linux profile. It runs INSIDE the real worker image through the root-start
// entrypoint (the worker uid 10001 over the real uid split), drives the PRODUCTION
// makeDefaultSpawnCommand + launchCodexEffectRoot through uzi-codex-supervisor, and talks to a
// password-protected private registry and the ambient Docker daemon (DOCKER_HOST). The harness is
// e2e/docker-config-persist/run.sh, which supplies:
//
//   UZI_DOCKER_CONFIG_LINUX=1   enables this suite
//   UZI_DCP_REGISTRY            host:port of the private registry, as the daemon dials it
//   UZI_DCP_USER, UZI_DCP_PASS  its basic-auth credential (assembled at runtime by the harness)
//   UZI_DCP_IMAGE               a private image ref in that registry
//   UZI_DCP_PREFIX              name prefix for every image this suite builds (dcp-test-<hex>)
//
// Every step prints `DCP_STEP <leg> <step> PASS|FAIL`; the required-mode legs print
// `NOT PROVEN` when Landlock is unavailable in the container.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs/promises";

import { makeDefaultSpawnCommand, prepareCodexRunHome } from "../src/codex/codex-executor.js";
import { launchCodexEffectRoot } from "../src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { runnerCommand } from "../src/runner-uid.js";

const ENABLED = process.platform === "linux" && process.env.UZI_DOCKER_CONFIG_LINUX === "1";
const SANDBOX_BIN = "/usr/local/bin/uzi-codex-command-sandbox";
const HOME_ROOT = "/data/agent-home";
const COMMAND_UID = 10003;

type Mode = "off" | "required";

function need(name: string): string {
  const value = process.env[name];
  assert.ok(value, `${name} is required`);
  return value;
}

/** Print the per-step verdict the harness greps for, then rethrow a failure. */
async function step(leg: string, name: string, body: () => Promise<void>): Promise<void> {
  try {
    await body();
  } catch (error) {
    console.log(`DCP_STEP ${leg} ${name} FAIL`);
    throw error;
  }
  console.log(`DCP_STEP ${leg} ${name} PASS`);
}

function landlockAvailable(): boolean {
  const probe = spawnSync(SANDBOX_BIN, ["--probe"], { stdio: "ignore" });
  return probe.status === 0;
}

let counter = 0;
function uniq(label: string): string {
  counter += 1;
  return `${label}-${process.pid}-${counter}`;
}

/** A run home prepared the way production prepares it (worker:runner 03770), and a worktree. */
async function freshRun(label: string): Promise<{ runHome: string; worktree: string; dockerDir: string }> {
  const runHome = `${HOME_ROOT}/${uniq(`dcp-${label}`)}`;
  await prepareCodexRunHome(runHome);
  const root = `/data/runner/${uniq(`dcp-wt-${label}`)}`;
  const worktree = `${root}/worktree`;
  await fs.mkdir(worktree, { recursive: true, mode: 0o2770 });
  await fs.chmod(root, 0o2770);
  await fs.chmod(worktree, 0o2770);
  return { runHome, worktree, dockerDir: `${runHome}/.docker` };
}

function spawner(worktree: string, mode: Mode, dockerConfigDir: string | undefined, extraEnv: Record<string, string> = {}) {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(41 + counter));
  counter += 1;
  const commandEnv: NodeJS.ProcessEnv = {
    PATH: "/usr/local/bin:/usr/bin:/bin",
    LANG: "C",
    ...(process.env.DOCKER_HOST ? { DOCKER_HOST: process.env.DOCKER_HOST } : {}),
    ...extraEnv,
  };
  return makeDefaultSpawnCommand(
    registry,
    (spec) => launchCodexEffectRoot(spec),
    5000,
    worktree,
    commandEnv,
    mode,
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
    dockerConfigDir,
  );
}

async function run(spawn: ReturnType<typeof spawner>, worktree: string, script: string) {
  return spawn(["/bin/sh", "-c", script], { cwd: worktree });
}

function describeResult(r: { code: number | null; stdout: string; stderr: string }): string {
  return `code=${String(r.code)} stdout=${JSON.stringify(r.stdout.slice(-400))} stderr=${JSON.stringify(r.stderr.slice(-800))}`;
}

const DEGRADE_LINE = /uzi-codex-command-sandbox: Docker config directory unavailable \([^)\n]*\); this command runs without a persistent Docker login/;

describe("Codex command Docker login persistence (Linux packaged profile)", { skip: ENABLED ? false : "opt-in packaged Linux profile" }, () => {
  const registry = process.env.UZI_DCP_REGISTRY ?? "";
  const image = process.env.UZI_DCP_IMAGE ?? "";
  const prefix = process.env.UZI_DCP_PREFIX ?? "dcp-test-local";
  const modes: Mode[] = ["off"];
  const hasLandlock = ENABLED && landlockAvailable();
  if (hasLandlock) modes.push("required");
  else if (ENABLED) console.log("DCP_STEP required ALL NOT PROVEN (Landlock unavailable in this container)");

  const credEnv = (): Record<string, string> => ({ DCP_REG: need("UZI_DCP_REGISTRY"), DCP_USER: need("UZI_DCP_USER"), DCP_PW: need("UZI_DCP_PASS") });
  const login = `printf '%s' "$DCP_PW" | docker login "$DCP_REG" -u "$DCP_USER" --password-stdin`;

  it("runs as the worker uid under the real split", () => {
    assert.equal(process.getuid?.(), 10001, "the suite must run as the worker uid");
    assert.equal(process.env.UZI_UID_SPLIT, "1", "the root-start entrypoint established the split");
    need("UZI_DCP_REGISTRY"); need("UZI_DCP_USER"); need("UZI_DCP_PASS"); need("UZI_DCP_IMAGE");
    assert.ok(registry && image);
  });

  for (const mode of modes) {
    it(`mode ${mode}: login in one command persists to later pull and build`, async () => {
      const leg = `persist-${mode}`;
      const { worktree, dockerDir, runHome } = await freshRun(`persist-${mode}`);
      const cmd = (extra: Record<string, string> = {}) => spawner(worktree, mode, dockerDir, extra);
      await run(cmd(), worktree, `docker rmi -f "${image}" >/dev/null 2>&1; true`);

      await step(leg, "a-pull-refused-before-login", async () => {
        const r = await run(cmd(), worktree, `docker pull "${image}"`);
        assert.notEqual(r.code, 0, `pull of a private image must be refused before login: ${describeResult(r)}`);
      });

      await step(leg, "b-login", async () => {
        const r = await run(cmd(credEnv()), worktree, login);
        assert.equal(r.code, 0, `docker login failed: ${describeResult(r)}`);
        assert.ok(!r.stderr.includes(need("UZI_DCP_PASS")) && !r.stdout.includes(need("UZI_DCP_PASS")));
        const st = await fs.lstat(dockerDir);
        assert.equal(st.uid, COMMAND_UID, ".docker is owned by the command uid");
        assert.equal(st.mode & 0o7777, 0o700, ".docker is exactly 0700");
      });

      await step(leg, "c-later-command-pulls", async () => {
        const r = await run(cmd(), worktree, `docker pull "${image}"`);
        assert.equal(r.code, 0, `a later command must reuse the login: ${describeResult(r)}`);
      });

      await step(leg, "d-later-command-builds-FROM-private", async () => {
        const rm = await run(cmd(), worktree, `docker rmi -f "${image}"`);
        assert.equal(rm.code, 0, `rmi failed: ${describeResult(rm)}`);
        await fs.writeFile(`${worktree}/Dockerfile`, `FROM ${image}\nLABEL dcp=1\n`);
        const r = await run(cmd(), worktree, `DOCKER_BUILDKIT=1 docker build -t "${prefix}-built-${mode}:1" .`);
        assert.equal(r.code, 0, `a build FROM the private image must reuse the login: ${describeResult(r)}`);
      });

      await run(cmd(), worktree, `docker rmi -f "${prefix}-built-${mode}:1" "${image}" >/dev/null 2>&1; true`);
      await fs.access(runHome);
    });

    it(`mode ${mode}: two concurrent first commands in a fresh run home both succeed and .docker is 0700 owned by the command uid`, async () => {
      const leg = `race-${mode}`;
      const { worktree, dockerDir } = await freshRun(`race-${mode}`);
      await step(leg, "e-concurrent-first-commands", async () => {
        const script = `test -d "$DOCKER_CONFIG" && test -w "$DOCKER_CONFIG" && : > "$DOCKER_CONFIG/marker-$$"`;
        const results = await Promise.all([
          run(spawner(worktree, mode, dockerDir), worktree, script),
          run(spawner(worktree, mode, dockerDir), worktree, script),
        ]);
        for (const r of results) assert.equal(r.code, 0, describeResult(r));
        const st = await fs.lstat(dockerDir);
        assert.equal(st.uid, COMMAND_UID);
        assert.equal(st.mode & 0o7777, 0o700);
      });
    });

    it(`mode ${mode}: a planted .docker owned by another uid degrades with one stderr line and the command still runs`, async () => {
      const leg = `degrade-${mode}`;
      const { worktree, dockerDir } = await freshRun(`degrade-${mode}`);
      await fs.mkdir(dockerDir, { mode: 0o700 }); // created as the worker uid, so not owned by the command uid
      await step(leg, "f-planted-foreign-docker-dir", async () => {
        const planted = await fs.lstat(dockerDir);
        assert.notEqual(planted.uid, COMMAND_UID);
        const r = await run(
          spawner(worktree, mode, dockerDir, { DOCKER_CONFIG: "/planted/.docker" }),
          worktree,
          `printf '%s' "\${DOCKER_CONFIG-unset}"; docker version --format '{{.Client.Version}}' >/dev/null`,
        );
        assert.equal(r.code, 0, `the command must still run: ${describeResult(r)}`);
        assert.equal(r.stdout, "unset", "DOCKER_CONFIG is removed from a degraded command");
        assert.match(r.stderr, DEGRADE_LINE);
        assert.equal(r.stderr.split("\n").filter((l) => l.includes("Docker config directory unavailable")).length, 1);
        const after = await fs.lstat(dockerDir);
        assert.equal(after.uid, planted.uid, "the foreign directory was not taken over");
      });
    });
  }

  it("required mode: another run's .docker is not writable from this run's sandbox", { skip: hasLandlock ? false : "Landlock unavailable" }, async () => {
    const a = await freshRun("iso-a");
    const b = await freshRun("iso-b");
    await step("isolation-required", "g-sibling-docker-dir-denied", async () => {
      const mk = await run(spawner(b.worktree, "required", b.dockerDir), b.worktree, `: > "$DOCKER_CONFIG/mine"`);
      assert.equal(mk.code, 0, describeResult(mk));
      const own = await run(spawner(a.worktree, "required", a.dockerDir), a.worktree, `: > "$DOCKER_CONFIG/mine"`);
      assert.equal(own.code, 0, describeResult(own));
      const other = await run(spawner(a.worktree, "required", a.dockerDir), a.worktree, `if { printf x > "${b.dockerDir}/intruder"; } 2>/dev/null; then exit 7; fi; exit 0`);
      assert.equal(other.code, 0, `a sibling run's .docker was writable: ${describeResult(other)}`);
    });
  });

  it("Claude leg: a login in one runner process persists to a later separate process (DOCKER_CONFIG set explicitly to the value buildSdkEnv sets)", async () => {
    const claudeHome = `${HOME_ROOT}/${uniq("dcp-claude")}`;
    await fs.mkdir(claudeHome);
    await fs.chmod(claudeHome, 0o2770);
    const env = {
      PATH: "/usr/local/bin:/usr/bin:/bin",
      HOME: claudeHome,
      DOCKER_CONFIG: `${claudeHome}/.docker`,
      ...(process.env.DOCKER_HOST ? { DOCKER_HOST: process.env.DOCKER_HOST } : {}),
      ...credEnv(),
    };
    const asRunner = (script: string) => {
      const wrapped = runnerCommand("/bin/sh", ["-c", script]);
      const r = spawnSync(wrapped.command, wrapped.args, { env, cwd: "/tmp", encoding: "utf8" });
      return { code: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
    };
    assert.equal(execFileSync("id", ["-u"], { encoding: "utf8" }).trim(), "10001");
    asRunner(`docker rmi -f "${image}" >/dev/null 2>&1; true`);
    await step("claude", "a-pull-refused-before-login", async () => {
      assert.notEqual(asRunner(`docker pull "${image}"`).code, 0);
    });
    await step("claude", "b-login", async () => {
      const r = asRunner(login);
      assert.equal(r.code, 0, describeResult(r));
    });
    await step("claude", "c-later-process-pulls", async () => {
      const r = asRunner(`docker pull "${image}"`);
      assert.equal(r.code, 0, describeResult(r));
    });
    asRunner(`docker rmi -f "${image}" >/dev/null 2>&1; true`);
  });
});
