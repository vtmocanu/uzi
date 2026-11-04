// Worker execution-environment lane (issue #3894, PR1: M0 preflight + M1 focused proofs).
//
// Runs in the ROOT-START worker container (worker uid 10001, UZI_UID_SPLIT=1, runner 10002,
// command identity 10003) over the image's REAL /nix (an image-seeded, invocation-owned docker
// volume, never a tmpfs mask) with anonymous network and no credential of any kind. Each proof
// states the CORRECT behaviour: a drift that is not fixed yet leaves its proof red until it is.
//
// Command paths are driven through the worker's own production code, never one shared env:
//   - Claude: the real ClaudeCommandLane (hook then handle) with the env buildSdkEnv builds for
//     the CLI and guardedCommandEnv sanitizes, as e2e/worker-uid/claude-command-real.test.mts does.
//   - Codex: the real CodexCallbackBroker.handleToolCall over the real effect launcher and command
//     sandbox, with withCommandGitTrust(buildCommandEnv(...)), the env the executor hands commands.
// Tools come from provisionRunTools (explicit synthetic run context, repo devbox opt-in).
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { guardedCommandEnv } from "../../agent/src/claude-command/env.js";
import { GUARDED_COMMAND_TOOL } from "../../agent/src/claude-command/grants.js";
import { ClaudeCommandLane } from "../../agent/src/claude-command/lane.js";
import { CodexCallbackBroker, type CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { buildCommandEnv, canonicalCheckoutPath, makeDefaultSpawnCommand, withCommandGitTrust } from "../../agent/src/codex/codex-executor.js";
import { launchCodexEffectRoot } from "../../agent/src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../agent/src/codex/registry.js";
import type { RunContext } from "../../agent/src/executor.js";
import type { Logger } from "../../agent/src/log.js";
import { provisionRunTools } from "../../agent/src/provision-run.js";
import { COMMAND_UID, RUNNER_UID, WORKER_UID, runnerCommand, runnerTmpdir } from "../../agent/src/runner-uid.js";
import { buildSdkEnv } from "../../agent/src/sdk-env.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
import { workerSpawnEnv } from "../../agent/src/worker-spawn-mark.js";

type ProofId = "preflight" | "toolchain" | "forgeFake" | "claudePrivate" | "claudeShared" | "codexBroker";
const PROOFS = JSON.parse(fs.readFileSync(new URL("./proofs.json", import.meta.url), "utf8")) as { leaves: Record<ProofId, string> };

/** Register one required leaf under the exact name proofs.json declares (check.py counts them). */
function proof(id: ProofId, timeoutMs: number, body: () => Promise<void>): void {
  const name = PROOFS.leaves[id];
  assert.ok(name, `proofs.json has no leaf ${id}`);
  void test(name, { timeout: timeoutMs }, async () => {
    const started = performance.now();
    try { await body(); } finally { console.log(`worker-env-timing ${id} ${((performance.now() - started) / 1000).toFixed(1)}s`); }
  });
}

const BASE_PATH = "/usr/local/bin:/usr/bin:/bin";
const CHECKOUT_REL = "checkout";
const META = "claudecode/toolUseId";
const QUIET_LOG = { debug() {}, info() {}, warn() {} };
const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";

// The tools gate:repo needs. Baked into the image's Nix profile (agent/devbox-global/devbox.json)
// except gitleaks (a separate Dockerfile step) and the repo tier-2 extras (root devbox.json),
// which only provisionRunTools with the repo opt-in may supply. Node, git and bash are base-image.
const BAKED_TOOLS = ["go", "task", "shellcheck", "yamllint", "actionlint", "zizmor", "semgrep", "python3", "gitleaks", "git", "bash", "node"];
const TIER2_TOOLS = ["ruby", "freeze"];

interface State {
  readonly base: string;
  readonly checkout: string;
  readonly home: string;
  toolEnv?: Record<string, string>;
}
let state: State | undefined;
const need = (): State => {
  assert.ok(state, "the preflight proof did not establish the run-shaped checkout; later proofs cannot run");
  return state;
};
const needTools = (): Record<string, string> => {
  const tools = need().toolEnv;
  assert.ok(tools?.PATH, "the toolchain proof did not provision tools; later proofs cannot run");
  return tools;
};

/** Run a short helper at the runner uid, the identity that owns a run's checkout and HOME. */
function asRunner(command: string, args: readonly string[], env: NodeJS.ProcessEnv = { PATH: BASE_PATH, LANG: "C" }, timeoutMs = 120_000) {
  const wrapped = runnerCommand(command, args);
  return spawnSync(wrapped.command, wrapped.args, { env: workerSpawnEnv(env), encoding: "utf8", timeout: timeoutMs, maxBuffer: 16 * 1024 * 1024 });
}
function mustRunAsRunner(command: string, args: readonly string[], env?: NodeJS.ProcessEnv, timeoutMs?: number): string {
  const result = asRunner(command, args, env, timeoutMs);
  assert.equal(result.status, 0, `${command} ${args.join(" ")} failed (status ${String(result.status)}): ${result.stderr}${result.error?.message ?? ""}`);
  return result.stdout;
}
const diag = (label: string, text: string) => { for (const line of text.split("\n")) if (line.length > 0) console.log(`worker-env-diagnostic ${label}: ${line}`); };

const productionLikeLogger = {
  debug() {}, info(message: string, fields?: Record<string, unknown>) { console.log("worker-env-provision info:", message, JSON.stringify(fields ?? {})); },
  warn(message: string, fields?: Record<string, unknown>) { console.log("worker-env-provision WARN:", message, JSON.stringify(fields ?? {})); },
  error(message: string, fields?: Record<string, unknown>) { console.log("worker-env-provision ERROR:", message, JSON.stringify(fields ?? {})); },
  addSecret() {}, removeSecret() {},
} as unknown as Logger;

function tail(text: string, lines = 40): string { return text.split("\n").slice(-lines).join("\n"); }

/**
 * Wrap a gate command so the result carries a bounded evidence view of its output and the exact
 * exit code: positive evidence lines first, then the tail. The scratch file lives under the
 * command's own TMPDIR, so the wrapper also exercises that layout.
 */
function evidenceCommand(command: string, evidence: string): string {
  return `out="$(mktemp)"; ${command} >"$out" 2>&1; rc=$?; grep -E ${quote(evidence)} "$out" | tail -n 5; echo '--- tail'; tail -n 40 "$out"; echo "worker-env-rc=$rc"; rm -f "$out"; exit $rc`;
}

// ---------------------------------------------------------------------------------------------
// Claude lane: the real guarded command lane, production env construction and sanitization.
// ---------------------------------------------------------------------------------------------
function claudeCliEnv(tmpdir?: string): Record<string, string> {
  const sdk = buildSdkEnv("", need().home, needTools());
  if (tmpdir !== undefined) sdk.TMPDIR = tmpdir; // what runnerTmpdir() yields on a layout with a shared TMPDIR
  return guardedCommandEnv(sdk);
}

let seq = 0;
async function runViaClaudeLane(command: string, cliEnv: Record<string, string>, timeoutMs: number): Promise<string> {
  const commands = new WorkerMemoryCommands();
  const flight = {};
  const context = commands.begin({ run_id: randomUUID(), worker_id: randomUUID(), register_nonce: "worker-env",
    claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1 });
  const captured: CapturedMemoryContext = { commands, context, flight, lifecycleRevision: 1 };
  const trips: Error[] = [];
  const checkout = need().checkout;
  const lane = new ClaudeCommandLane({
    worktree: checkout, cliEnv, secretPaths: [], dockerWired: false, secrets: [], log: QUIET_LOG,
    capturedMemory: () => captured, tripTurn: (error) => { trips.push(error); }, graceMs: 1000, cleanupMs: 5000,
  });
  lane.beginTurn();
  const args = { command, timeout: timeoutMs };
  const id = `toolu_wenv_${String(++seq)}_${randomUUID().slice(0, 8)}`;
  try {
    const hook = await lane.hook({
      hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: args, tool_use_id: id,
      session_id: "s", transcript_path: "/t", cwd: checkout,
    } as Parameters<ClaudeCommandLane["hook"]>[0], id, { signal: new AbortController().signal });
    assert.deepEqual(hook, {}, "the guard admits the call");
    const result = await lane.handle(args, { _meta: { [META]: id }, signal: undefined });
    const text = result.content.map((block) => ("text" in block ? block.text ?? "" : "")).join("");
    assert.deepEqual(trips, [], "the lane did not trip the turn");
    return text;
  } finally {
    await lane.quiesce(15_000).catch(() => undefined);
  }
}

function assertGateRan(text: string, evidence: RegExp, what: string): void {
  assert.match(text, /^Exit code: 0$/m, `${what} must exit 0 under this lane:\n${tail(text, 60)}`);
  assert.match(text, evidence, `${what} must show positive execution evidence (not a skipped or empty run):\n${tail(text, 60)}`);
  assert.match(text, /worker-env-rc=0/);
}

const CI_SAFE_PATHS = { command: "ruby scripts/ci-safe-paths.test.rb", evidence: /ci-safe-paths: \d+ assertions passed/ };

// ---------------------------------------------------------------------------------------------
// M0
// ---------------------------------------------------------------------------------------------
proof("preflight", 120_000, async () => {
  assert.equal(process.platform, "linux");
  assert.equal(process.getuid?.(), WORKER_UID, "the proof runs as the worker uid after the root entrypoint drop");
  assert.equal(process.env.UZI_UID_SPLIT, "1", "the root-start entrypoint established the worker/runner split");
  assert.match(process.env.WORKER_ENV_IMAGE_ID ?? "", /^sha256:[0-9a-f]{64}$/, "the host harness passes the image identity");
  console.log(`worker-env-identity image=${process.env.WORKER_ENV_IMAGE_ID} node=${process.version}`);
  assert.equal(fs.readFileSync("/etc/uzi-runner-layout", "utf8").trim(), "runner-shared-v1");

  // The image's real Nix store on a docker volume: not a tmpfs mask.
  const mountinfo = fs.readFileSync("/proc/self/mountinfo", "utf8").split("\n").map((line) => line.split(" "));
  const nixMount = mountinfo.find((fields) => fields[4] === "/nix");
  assert.ok(nixMount, "/nix is its own mount");
  const fsType = nixMount[nixMount.indexOf("-") + 1];
  assert.notEqual(fsType, "tmpfs", "/nix must be the image's seeded store, not a tmpfs mask");
  const profile = fs.realpathSync("/opt/uzi-toolchain");
  assert.ok(profile.startsWith("/nix/store/"), `the stable toolchain handle resolves into the store: ${profile}`);
  assert.equal(fs.readFileSync("/etc/uzi-toolchain-profile", "utf8").trim(), profile, "the image's toolchain marker matches the seeded store");
  assert.ok(fs.readdirSync("/nix/store").length > 50, "the seeded store holds the baked toolchain closure");

  // Sanitized env: nothing credential-shaped reaches the proof process, hence nothing it spawns.
  const risky = Object.keys(process.env).filter((key) => /TOKEN|SECRET|PASSW|API_?KEY|CREDENTIAL|^GH_|^GITHUB|^GITLAB|^ANTHROPIC|^OPENAI|^CLAUDE_CODE/i.test(key));
  assert.deepEqual(risky, [], "no credential-shaped variable in the proof environment");

  // The data layout the entrypoint establishes: shared, setgid, sticky carve-out roots.
  for (const dir of ["/data/runner", "/data/agent-home", "/data/provision"]) {
    assert.equal(fs.statSync(dir).mode & 0o7777, 0o3775, `${dir} is the shared setgid sticky root`);
  }

  // A run-shaped checkout: runner-owned clone content, group-writable for the runner group.
  const base = fs.mkdtempSync("/tmp/wenv-");
  fs.chmodSync(base, 0o777);
  const checkout = path.join(base, CHECKOUT_REL);
  const home = path.join(base, "home");
  mustRunAsRunner("sh", ["-c", 'mkdir "$0" "$1" && tar --exclude=./.uzi -C /src -cf - . | tar -C "$0" -xf - && chmod -R g+rwX "$0"', checkout, home]);
  state = { base, checkout, home };
  const stat = fs.statSync(checkout);
  assert.equal(stat.uid, RUNNER_UID, "the checkout is owned by the runner uid, as a run's clone is");
  assert.notEqual(stat.uid, COMMAND_UID, "the command identity does not own the checkout (the premise of the Codex trust env)");
  for (const rel of [".git", "Taskfile.yml", "devbox.json", "scripts/ci-safe-paths.test.rb", "e2e/forge-fake/operation-contracts.test.mjs"]) {
    assert.ok(fs.existsSync(path.join(checkout, rel)), `checkout carries ${rel}`);
  }
  assert.equal(mustRunAsRunner("git", ["-C", checkout, "rev-parse", "--is-inside-work-tree"]).trim(), "true",
    "the runner (checkout owner) can use git on its checkout with no extra trust");
});

proof("toolchain", 360_000, async () => {
  const { checkout } = need();
  const events: string[] = [];
  // An explicit synthetic run context with the repo's devbox opt-in: tier-1 empty, tier-2 from
  // the checkout's own devbox.json. No claim, no server, no credential.
  const ctx = {
    runId: randomUUID(), worktreePath: checkout,
    config: { tool_packages: [], repo_devbox_opt_in: true, denied_tool_packages: [] },
    emit: (message: { payload?: { text?: string } }) => { events.push(message.payload?.text ?? ""); },
  } as unknown as RunContext;
  const started = performance.now();
  const result = await provisionRunTools(ctx, { provisionRoot: "/data/provision", homeDir: "/data/agent-home", log: productionLikeLogger });
  const seconds = (performance.now() - started) / 1000;
  console.log(`worker-env-timing provisionRunTools ${seconds.toFixed(1)}s`);
  diag("provision events", events.join("\n"));

  // Production provisioning degrades silently after a tier-2 failure: assert it did not.
  assert.ok(events.some((text) => /^merged 2 package\(s\) from this repo's devbox\.json$/.test(text)), "both repo extras were merged");
  assert.ok(events.every((text) => !/provisioning failed|retrying without|skipping them|dropped/.test(text)), `provisioning did not degrade: ${events.join(" | ")}`);
  assert.ok(events.includes("tools provisioned"));
  const toolPath = result.toolEnv.PATH;
  assert.ok(toolPath, "provisioning produced a tool PATH");
  state = { ...need(), toolEnv: result.toolEnv };
  assert.deepEqual(Object.keys(result.toolEnv).filter((key) => !["PATH", "NIX_SSL_CERT_FILE", "LOCALE_ARCHIVE"].includes(key)), [],
    "only the provisioning allowlist reaches the tool env");

  // The Claude CLI env the lane derives commands from, and what survives guardedCommandEnv.
  const cliEnv = claudeCliEnv();
  assert.ok(!("CLAUDE_CODE_OAUTH_TOKEN" in cliEnv) && !("ANTHROPIC_API_KEY" in cliEnv), "no provider credential in the command env");
  assert.ok(!Object.keys(cliEnv).some((key) => /^UZI_WORKER_TOKEN|^UZI_API_URL$/.test(key)), "no worker authority in the command env");
  diag("claude command env keys", Object.keys(cliEnv).sort().join(" "));

  // Every tool resolves from the command PATH and the repo extras really came from provisioning.
  const resolve = (tool: string) => asRunner("sh", ["-c", 'command -v "$0"', tool], cliEnv).stdout.trim();
  const missing = [...BAKED_TOOLS, ...TIER2_TOOLS].filter((tool) => resolve(tool) === "");
  assert.deepEqual(missing, [], "every gate:repo tool, gitleaks and the repo extras resolve on the command PATH");
  for (const tool of TIER2_TOOLS) {
    const found = toolPath.split(":").some((dir) => dir !== "" && fs.existsSync(path.join(dir, tool)));
    assert.ok(found, `${tool} came from the provisioned PATH, not the image`);
  }
  assert.match(mustRunAsRunner("ruby", ["--version"], cliEnv), /^ruby 4\.0\.6/, "the repo-pinned ruby");
  assert.equal(mustRunAsRunner("node", ["--version"], cliEnv).trim(), process.version, "commands use the image Node the worker runs");
  for (const [tool, flag] of [["go", "version"], ["task", "--version"], ["gitleaks", "version"], ["shellcheck", "--version"], ["actionlint", "-version"]] as const) {
    diag("tool", `${tool}: ${mustRunAsRunner(tool, [flag], cliEnv).split("\n")[0]}`);
  }

  // TZDIR origin hunt (unresolved at the time of writing): record where, if anywhere, it is defined.
  const origin = asRunner("sh", ["-c", [
    'printf "claude-env TZDIR=%s\\n" "${TZDIR-<unset>}"',
    'ruby -e \'puts "ruby sees TZDIR=" + ENV.fetch("TZDIR", "<unset>")\'',
    'dir="$(dirname "$(command -v ruby)")"; echo "ruby bindir $dir"; grep -rIl TZDIR "$dir" 2>/dev/null | head -n 5 | sed "s/^/text referencing TZDIR: /"',
  ].join("; ")], cliEnv);
  diag("TZDIR", `worker process TZDIR=${process.env.TZDIR ?? "<unset>"}; provisioned keys=${Object.keys(result.toolEnv).join(",")}`);
  diag("TZDIR", origin.stdout + origin.stderr);
});

// ---------------------------------------------------------------------------------------------
// M1 focused proofs (Claude lane)
// ---------------------------------------------------------------------------------------------
proof("forgeFake", 330_000, async () => {
  // #3823: the contract bootstrap rejected transform mode on the worker's Node 26. Run the real
  // task, over the real image Node, through the guarded lane; the Go contract must run and pass.
  const text = await runViaClaudeLane(evidenceCommand("task test:forge-fake-contract", "^--- PASS: TestForgeFakeOperationContracts|^# Node "), claudeCliEnv(), 300_000);
  assertGateRan(text, /^--- PASS: TestForgeFakeOperationContracts /m, "test:forge-fake-contract");
  assert.ok(text.includes(`# Node ${process.version}`), "the contract bootstrap ran under the image's Node");
});

proof("claudePrivate", 200_000, async () => {
  // The private layout: the runner's own 0700 TMPDIR, as the compose/split entrypoint exports it.
  const tmp = runnerTmpdir();
  assert.ok(tmp, "the entrypoint exports a runner TMPDIR");
  const stat = fs.statSync(tmp);
  assert.equal(stat.mode & 0o7777, 0o700, "the runner TMPDIR is private (0700)");
  assert.equal(stat.uid, RUNNER_UID);
  const made = mustRunAsRunner("sh", ["-c", 'd="$(mktemp -d)"; stat -c %a "$d"; rmdir "$d"'], { PATH: BASE_PATH, TMPDIR: tmp });
  assert.equal(parseInt(made.trim(), 8) & 0o2000, 0, "a private TMPDIR hands no setgid bit to what is created in it");
  const text = await runViaClaudeLane(evidenceCommand(CI_SAFE_PATHS.command, "ci-safe-paths: "), claudeCliEnv(tmp), 180_000);
  assertGateRan(text, CI_SAFE_PATHS.evidence, "ci-safe-paths");
});

proof("claudeShared", 200_000, async () => {
  // The shared layout of a runner-shared pod: TMPDIR on a setgid, group-writable, sticky
  // directory. The mode comes from the controller's golden runner-init script, not from here.
  const golden = fs.readFileSync(new URL("../../controller/internal/kube/testdata/runner-shared-init.sh", import.meta.url), "utf8");
  const mode = /^\s*chmod (\d{4}) "\$R" "\$T"/m.exec(golden)?.[1];
  assert.equal(mode, "3775", "the controller's shared scratch mode (runner-shared-init.sh) is the setgid sticky one");
  const shared = path.join(need().base, "shared-tmp");
  mustRunAsRunner("sh", ["-c", 'mkdir "$0" && chmod "$1" "$0"', shared, mode]);
  assert.equal(fs.statSync(shared).mode & 0o7777, 0o3775);
  // Premise: what a command creates there inherits the group and the setgid bit.
  const made = mustRunAsRunner("sh", ["-c", 'd="$(mktemp -d)"; stat -c %a "$d"; rmdir "$d"'], { PATH: BASE_PATH, TMPDIR: shared });
  assert.notEqual(parseInt(made.trim(), 8) & 0o2000, 0, `a shared scratch hands the setgid bit to new directories (saw ${made.trim()})`);
  const text = await runViaClaudeLane(evidenceCommand(CI_SAFE_PATHS.command, "ci-safe-paths: "), claudeCliEnv(shared), 180_000);
  assertGateRan(text, CI_SAFE_PATHS.evidence, "ci-safe-paths");
});

// ---------------------------------------------------------------------------------------------
// M1 focused proof (Codex broker): inherited checkout trust (#3871) through handleToolCall.
// ---------------------------------------------------------------------------------------------
proof("codexBroker", 240_000, async () => {
  const { checkout } = need();
  const canonical = await canonicalCheckoutPath(checkout);
  // The executor builds exactly this, once per run (commandEnv in CodexExecutor.run).
  const plain = buildCommandEnv("/tmp", needTools());
  const trusted = withCommandGitTrust(plain, canonical);
  assert.deepEqual(
    Object.fromEntries(Object.entries(trusted).filter(([key]) => key.startsWith("GIT_CONFIG_"))),
    { GIT_CONFIG_COUNT: "2", GIT_CONFIG_KEY_0: "safe.directory", GIT_CONFIG_VALUE_0: "", GIT_CONFIG_KEY_1: "safe.directory", GIT_CONFIG_VALUE_1: canonical },
    "the command env carries the checkout trust pairs",
  );
  diag("codex command env keys", Object.keys(trusted).sort().join(" "));

  // The production effect launcher (codex-executor productionEffectLaunch): the image's real
  // supervisor and command sandbox, no injected helper binaries.
  const launch: Parameters<typeof makeDefaultSpawnCommand>[1] = (spec, deadlineMs) =>
    launchCodexEffectRoot(spec, deadlineMs === undefined ? {} : { deadlines: { started: deadlineMs } });
  let calls = 0;
  const bash = async (env: NodeJS.ProcessEnv, command: string) => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(++calls));
    const abort = new AbortController();
    try {
      const broker = new CodexCallbackBroker({
        registry, worktreePath: checkout, signal: abort.signal,
        spawnCommand: makeDefaultSpawnCommand(registry, launch, 10_000, checkout, env, "off", undefined, undefined, 220_000),
        grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
        fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
      });
      const result = await broker.handleToolCall({ threadId: "wenv-thread", turnId: "wenv-turn", callId: `wenv-call-${String(calls)}` }, "Bash", { command }, "root");
      assert.equal(result.ok, true, `the broker admitted and ran the command: ${JSON.stringify(result)}`);
      if (!result.ok) throw new Error("unreachable");
      return result.output as { code: number; stdout: string; stderr: string };
    } finally {
      abort.abort();
      assert.equal(registry.isPoisoned(), false, "the command root drained cleanly");
      assert.equal(registry.hasLiveCommandRoot(), false);
    }
  };

  // Negative control: without the trust pairs the different-uid command identity cannot use the
  // runner-owned checkout, so the positive result below is not vacuous.
  const control = await bash(plain, "git rev-parse --show-toplevel");
  assert.notEqual(control.code, 0, "an untrusted command uid must be refused by git's ownership check");
  assert.match(control.stderr, /dubious ownership/);
  const trustedGit = await bash(trusted, "git rev-parse --show-toplevel");
  assert.equal(trustedGit.code, 0, `the trusted env reaches the checkout: ${trustedGit.stderr}`);
  assert.equal(trustedGit.stdout.trim(), canonical);

  // The fixture itself, with the real inherited GIT_CONFIG_COUNT trust in force.
  const gate = await bash(trusted, evidenceCommand(CI_SAFE_PATHS.command, "ci-safe-paths: "));
  assert.equal(gate.code, 0, `ci-safe-paths under the Codex command env:\n${tail(gate.stdout + gate.stderr, 60)}`);
  assert.match(gate.stdout, CI_SAFE_PATHS.evidence, `positive execution evidence:\n${tail(gate.stdout, 60)}`);
});

// Best effort: the container is discarded, and the runner owns most of what the proofs created.
process.on("exit", () => { if (state) asRunner("rm", ["-rf", "--", state.base], undefined, 30_000); });
