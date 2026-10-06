import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import type { SpawnSyncOptions } from "node:child_process";
import { readFileSync } from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { PassThrough, Writable } from "node:stream";
import { fileURLToPath } from "node:url";

import { CODEX_SESSION_GID, COMMAND_UID, WORKER_UID, setprivArgsForUid, setprivRunnerArgs } from "../src/runner-uid.js";
import {
  CodexUnsupportedProfileError,
  TMP_RETAINED_REASONS,
  launchCodexRoot,
  launchCodexEffectRoot,
  type CodexLaunchSpec,
  type LauncherDeps,
  type RunnerTreeRequest,
  type SupervisorProcess,
} from "../src/codex/launcher.js";
import { registeredRoot } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { makeTextRedactor } from "../src/redact.js";
import { SESSION_SEED_ENTRYPOINT } from "../src/codex/session-seed-cli.js";
import type { ProvisionSpawnSync } from "../src/codex/launcher.js";
import { runLaunchCli, type LaunchCliDeps } from "../src/codex/launch-cli.js";
import { WORKER_SPAWN_ENV, workerRunnerRootPids } from "../src/worker-spawn-mark.js";
import { ResidueQuarantinedError, latchResidueQuarantine } from "../src/residue-quarantine.js";
import { nullLogger } from "./helpers.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

// PRD #1156 (M3a): the isolated per-root launcher uses a fake supervisor.
// Most privileged steps are injected; the worker-UID regression exercises the
// production runner-tree creator with real setpriv and the worker image binaries.

const CODEX_BIN = "/opt/uzi-codex/0.159.3/bin/codex";
const SUPERVISOR_BIN = "/usr/local/bin/uzi-codex-supervisor";
const RUNNER_UID = 10002;
const DATA_ROOT = "/data/run/root-1";

function baseSpec(overrides: Partial<CodexLaunchSpec> = {}): CodexLaunchSpec {
  return {
    ownedDataRoot: DATA_ROOT,
    provider: { name: "uzi-codex", baseUrl: "http://127.0.0.1:9/v1", envKey: "CODEX_PROVIDER_KEY", credentialValue: "dummy-key" },
    model: "gpt-5-codex",
    codexBin: CODEX_BIN,
    supervisorBin: SUPERVISOR_BIN,
    kind: "provider",
    childArgv: ["app-server"],
    cwd: "/work/repo",
    ...overrides,
  };
}

interface FakeOpts {
  uid?: number;
  nondumpable?: boolean;
  capBoundingSet?: "0x0" | "0xc0" | "0xff";
  autoStarted?: boolean;
  disposeState?: "drained" | "unconfirmed";
  singleDisposeResponse?: boolean;
  disposeAuthority?: string;
  exitCode?: number;
  disposeNearBudget?: boolean;
  exitDelayMs?: number;
  /** When set (even to a malformed value), attached verbatim as the drained dispose's `tmpCleanup`. */
  disposeTmpCleanup?: unknown;
}

/** A fake supervisor: 5 PassThrough fds, reads control on fd3, emits the pinned
 *  evidence frames on fd4, and emits "exit" like a ChildProcess. */
class FakeSupervisor extends EventEmitter {
  readonly pid = 4242;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stderr = new PassThrough();
  readonly control = new PassThrough();
  readonly evidence = new PassThrough();
  readonly stdio: PassThrough[];
  private readonly disposeState: "drained" | "unconfirmed";
  private readonly singleDisposeResponse: boolean;
  private readonly disposeAuthority: string;
  private readonly exitCode: number;
  private readonly disposeNearBudget: boolean;
  private readonly exitDelayMs: number;
  private readonly tmpCleanup: { value: unknown } | undefined;
  readonly disposeTimeouts: number[] = [];

  constructor(opts: FakeOpts = {}) {
    super();
    this.stdio = [this.stdin, this.stdout, this.stderr, this.control, this.evidence];
    this.disposeState = opts.disposeState ?? "drained";
    this.singleDisposeResponse = opts.singleDisposeResponse ?? false;
    this.disposeAuthority = opts.disposeAuthority ?? "ECHILD+__WALL";
    this.exitCode = opts.exitCode ?? 0;
    this.disposeNearBudget = opts.disposeNearBudget ?? false;
    this.exitDelayMs = opts.exitDelayMs ?? 0;
    this.tmpCleanup = "disposeTmpCleanup" in opts ? { value: opts.disposeTmpCleanup } : undefined;
    createInterface({ input: this.control }).on("line", (line) => this.onControl(line));
    if (opts.autoStarted ?? true) {
      this.writeEvidence({
        event: "started", supervisorPid: this.pid, childPid: this.pid + 1,
        subreaper: true, nondumpable: opts.nondumpable ?? true, uid: opts.uid ?? RUNNER_UID,
        liveCapsZero: true, capBoundingSet: opts.capBoundingSet ?? "0xc0", noNewPrivs: true,
      });
    }
  }

  writeEvidence(value: unknown): void { this.evidence.write(`${JSON.stringify(value)}\n`); }
  writeRawEvidence(text: string): void { this.evidence.write(`${text}\n`); }
  emitAbnormal(reason: string): void { this.writeEvidence({ event: "abnormal", reason, cleanup: { state: "unconfirmed" } }); }
  emitChildExit(code: number): void { this.writeEvidence({ event: "child_exit", code }); }
  exitWith(code: number, signal: NodeJS.Signals | null = null): void { this.emit("exit", code, signal); }

  private onControl(line: string): void {
    const cmd = JSON.parse(line) as { op: string; id: number; timeoutMs?: number };
    if (cmd.op === "snapshot") {
      this.writeEvidence({ event: "snapshot", id: cmd.id, processes: [{ pid: this.pid + 1, ppid: this.pid, pgid: this.pid, comm: "codex" }] });
    } else if (cmd.op === "dispose") {
      this.disposeTimeouts.push(cmd.timeoutMs ?? 0);
      if (this.singleDisposeResponse && this.disposeTimeouts.length > 1) return;
      if (this.disposeState === "drained") {
        const emit = (): void => {
          this.writeEvidence({
            event: "dispose", id: cmd.id, state: "drained", authority: this.disposeAuthority, killed: [], reaped: [this.pid + 1],
            ...(this.tmpCleanup ? { tmpCleanup: this.tmpCleanup.value } : {}),
          });
          setTimeout(() => this.exitWith(this.exitCode), this.exitDelayMs);
        };
        if (this.disposeNearBudget) setTimeout(emit, Math.max(0, (cmd.timeoutMs ?? 0) - 5));
        else emit();
      } else {
        this.writeEvidence({ event: "dispose", id: cmd.id, state: "unconfirmed", reason: "deadline", killed: [], reaped: [], children: [this.pid + 1] });
      }
    }
  }

  destroyAll(): void {
    for (const s of this.stdio) s.destroy();
  }
}

const fakes: FakeSupervisor[] = [];
const spawnCalls: { command: string; args: readonly string[]; options: { cwd: string; env: NodeJS.ProcessEnv } }[] = [];
const treeCalls: RunnerTreeRequest[] = [];
const treeRemoveCalls: Array<Pick<RunnerTreeRequest, "uid" | "kind" | "root">> = [];
let treeCleanupFailures = 0;

function baseDeps(fake: FakeSupervisor, overrides: Partial<LauncherDeps> = {}): LauncherDeps {
  return {
    env: { UZI_UID_SPLIT: "1" },
    resolveRunnerUid: () => RUNNER_UID,
    assertNoUnexpectedSystemConfig: () => { /* no /etc/codex in unit tests */ },
    makeRunnerTrees: (req) => { treeCalls.push(req); },
    removeRunnerTree: (req) => { treeRemoveCalls.push(req); },
    reportRunnerTreeCleanupFailure: () => { treeCleanupFailures += 1; },
    spawnSupervisor: (command, args, options) => {
      spawnCalls.push({ command, args, options });
      return fake as unknown as SupervisorProcess;
    },
    deadlines: { started: 1000, snapshot: 1000, dispose: 1000, exit: 1000 },
    ...overrides,
  };
}

function newFake(opts: FakeOpts = {}): FakeSupervisor {
  const fake = new FakeSupervisor(opts);
  fakes.push(fake);
  return fake;
}

let savedSplit: string | undefined;
let savedCanary: string | undefined;
beforeEach(() => {
  savedSplit = process.env.UZI_UID_SPLIT;
  savedCanary = process.env.CODEX_CANARY_LEAK;
  process.env.UZI_UID_SPLIT = "1"; // so the composed runnerCommand wraps in setpriv
  process.env.CODEX_CANARY_LEAK = "SHOULD-NOT-LEAK";
  spawnCalls.length = 0;
  treeCalls.length = 0;
  treeRemoveCalls.length = 0;
  treeCleanupFailures = 0;
});
afterEach(() => {
  for (const f of fakes) f.destroyAll();
  fakes.length = 0;
  if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = savedSplit;
  if (savedCanary === undefined) delete process.env.CODEX_CANARY_LEAK; else process.env.CODEX_CANARY_LEAK = savedCanary;
});

describe("launchCodexRoot: fail-closed supported-profile gate", () => {
  it("(a) REFUSES to launch when the uid-split is not active", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec(), baseDeps(fake, { env: { PATH: "/usr/bin" } })),
      (err: unknown) => {
        assert.ok(err instanceof CodexUnsupportedProfileError);
        assert.match((err as Error).message, /shared-uid \/ single-uid/);
        assert.match((err as Error).message, /prerequisite/);
        return true;
      },
    );
    assert.equal(spawnCalls.length, 0, "no supervisor spawn under an unsupported profile");
    assert.equal(treeCalls.length, 0, "no runner trees created under an unsupported profile");
  });
});

describe("launchCodexRoot: env allowlist, trees, argv", () => {
  it("(b) builds a REPLACED env of EXACTLY the allowed keys; a canary is excluded", async () => {
    const fake = newFake();
    await launchCodexRoot(baseSpec(), baseDeps(fake));
    const env = spawnCalls[0]?.options.env ?? {};
    assert.deepEqual(
      Object.keys(env).sort(),
      [
        "CODEX_HOME", "CODEX_PROVIDER_KEY", "HOME", "LANG", "PATH", "SHELL", "TERM", "TMPDIR",
        "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
        // issue #1783 (R4): NO worker spawn mark — the app-server runs model-directed work.
      ].sort(),
    );
    assert.equal(env.CODEX_PROVIDER_KEY, "dummy-key");
    assert.equal(env.HOME, `${DATA_ROOT}/home`);
    assert.equal(env.CODEX_HOME, `${DATA_ROOT}/codex`);
    assert.equal(env.TMPDIR, `${DATA_ROOT}/tmp`);
    assert.equal(env.SHELL, "/bin/sh");
    assert.equal(env.LANG, "C");
    assert.equal(env.TERM, "dumb");
    // The credentialed launch lane resolves only system tools, never the writable store.
    assert.equal(env.PATH, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin");
    assert.ok(String(env.PATH).split(":").every((entry) => !entry.startsWith("/opt/uzi-toolchain") && !entry.startsWith("/nix")));
    assert.doesNotMatch(String(env.PATH), /uzi-codex/);
    // No leak of the worker/host env.
    assert.equal(env.CODEX_CANARY_LEAK, undefined);
    assert.equal(env.UZI_UID_SPLIT, undefined);
  });

  it("(b') is credential-FREE for kind:command", async () => {
    const fake = newFake({ uid: COMMAND_UID });
    await launchCodexRoot(baseSpec({ kind: "command", childArgv: ["exec", "--", "echo"] }), baseDeps(fake));
    const env = spawnCalls[0]?.options.env ?? {};
    assert.equal(env.CODEX_PROVIDER_KEY, undefined, "no provider credential for a command root");
    assert.equal(Object.keys(env).includes("CODEX_PROVIDER_KEY"), false);
  });

  it("issue #1783: a runner-uid provider root is a recorded worker-launched root until it exits; a command root is not; neither carries the worker mark", async () => {
    const provider = newFake();
    await launchCodexRoot(baseSpec(), baseDeps(provider, { rootStartTime: (pid) => pid + 7 }));
    assert.equal(spawnCalls[0]?.options.env[WORKER_SPAWN_ENV], undefined, "no worker mark on the model-driving provider root");
    assert.ok(
      workerRunnerRootPids().some((r) => r.pid === provider.pid && r.startTime === provider.pid + 7),
      "the non-dumpable provider supervisor is attributable (pid + the start time read at registration)",
    );
    provider.exitWith(0);
    assert.equal(workerRunnerRootPids().some((r) => r.pid === provider.pid), false, "unregistered once it exited");

    const command = newFake({ uid: COMMAND_UID });
    await launchCodexRoot(baseSpec({ kind: "command", childArgv: ["exec", "--", "echo"] }), baseDeps(command, { rootStartTime: (pid) => pid + 7 }));
    assert.equal(spawnCalls[1]?.options.env[WORKER_SPAWN_ENV], undefined, "no worker mark on a command root");
    assert.equal(workerRunnerRootPids().some((r) => r.pid === command.pid), false, "a runner-cmd root is never scanned, so never recorded");
  });

  it("(c) creates the fresh trees 0700 as the runner uid via the injected step", async () => {
    const fake = newFake();
    await launchCodexRoot(baseSpec(), baseDeps(fake));
    const req = treeCalls[0];
    assert.ok(req, "makeRunnerTrees was called");
    assert.equal(req.uid, RUNNER_UID);
    assert.equal(req.root, DATA_ROOT);
    assert.equal(req.sharedSessionRead, undefined, "the transitional provider tree stays owner-only");
    assert.deepEqual([...req.dirs], [
      `${DATA_ROOT}/home`, `${DATA_ROOT}/codex`, `${DATA_ROOT}/xdg-config`,
      `${DATA_ROOT}/xdg-cache`, `${DATA_ROOT}/xdg-data`, `${DATA_ROOT}/xdg-state`, `${DATA_ROOT}/tmp`,
    ]);
    assert.equal(req.files.length, 1);
    const configFile = req.files[0];
    assert.ok(configFile);
    assert.equal(configFile.path, `${DATA_ROOT}/codex/config.toml`);
    assert.equal(configFile.mode, 0o600);
    assert.match(configFile.content, /project_doc_max_bytes = 0/);
    assert.match(configFile.content, /trust_level = "untrusted"/);
    assert.doesNotMatch(configFile.content, /bypass_hook_trust/);
  });

  it("(d) constructs the launcher-fixed supervisor argv, composing runnerCommand (setpriv) unchanged", async () => {
    const fake = newFake();
    await launchCodexRoot(baseSpec(), baseDeps(fake));
    const call = spawnCalls[0];
    assert.ok(call);
    // Under the split, runnerCommand wraps in setpriv-to-runner, verbatim.
    assert.equal(call.command, "/bin/setpriv");
    assert.deepEqual([...call.args], [
      ...setprivRunnerArgs(), SUPERVISOR_BIN, "--expect-uid", String(RUNNER_UID), "--", CODEX_BIN, "app-server",
    ]);
    // The supervisor portion is EXACTLY --expect-uid <N> -- <codexBin> <childArgv>.
    const full = [call.command, ...call.args];
    const at = full.indexOf(SUPERVISOR_BIN);
    assert.ok(at >= 0);
    assert.deepEqual(full.slice(at + 1), ["--expect-uid", String(RUNNER_UID), "--", CODEX_BIN, "app-server"]);
  });

  it("rejects a started posture that does not match the expected runner uid", async () => {
    const fake = newFake({ uid: 12345 });
    await assert.rejects(launchCodexRoot(baseSpec(), baseDeps(fake)), /unsafe start posture/);
  });

  it("rejects a started posture with nondumpable=false (the channel-boundary anchor)", async () => {
    const fake = newFake({ nondumpable: false });
    await assert.rejects(launchCodexRoot(baseSpec(), baseDeps(fake)), /unsafe start posture.*nondumpable=false/s);
  });

  it("rejects a started posture with an unexpected capability bounding set", async () => {
    const fake = newFake({ capBoundingSet: "0xff" });
    await assert.rejects(launchCodexRoot(baseSpec(), baseDeps(fake)), /unsafe start posture.*capBoundingSet=0xff/s);
  });
});

describe("launchCodexRoot: app-server auth (production config, no env credential)", () => {
  it("useAppServerAuth emits the PRODUCTION config and injects NO provider env credential", async () => {
    const fake = newFake();
    // The spec still carries a provider credentialValue; under app-server auth the launcher
    // must NOT propagate it to the env (the token flows over the login RPC, not /proc environ).
    await launchCodexRoot(baseSpec({ useAppServerAuth: true, authMode: "subscription" }), baseDeps(fake));
    const env = spawnCalls[0]?.options.env ?? {};
    assert.equal(env.CODEX_PROVIDER_KEY, undefined, "no provider credential var under app-server auth");
    assert.equal(Object.keys(env).includes("CODEX_PROVIDER_KEY"), false);
    // Exactly the base allowlist, MINUS the provider credential var.
    assert.deepEqual(
      Object.keys(env).sort(),
      [
        "CODEX_HOME", "HOME", "LANG", "PATH", "SHELL", "TERM", "TMPDIR",
        "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
      ].sort(),
    );
    // The config is buildCodexProductionConfigToml: native-disabled lines + the built-in
    // `openai` provider, with NO re-declared [model_providers.*] table (which would set
    // requires_openai_auth=false and BYPASS the account/login/start the auth session performs).
    const configFile = treeCalls[0]?.files[0];
    assert.ok(configFile);
    assert.match(configFile.content, /project_doc_max_bytes = 0/);
    assert.match(configFile.content, /trust_level = "untrusted"/);
    assert.match(configFile.content, /model_provider = "openai"/);
    assert.doesNotMatch(configFile.content, /\[model_providers\./, "production config declares no custom provider table");
    assert.doesNotMatch(configFile.content, /requires_openai_auth/);
    assert.doesNotMatch(configFile.content, /base_url/);
    assert.deepEqual(treeCalls[0]?.sharedSessionRead, {
      gid: CODEX_SESSION_GID,
      codexHome: `${DATA_ROOT}/codex`,
      sessionDir: `${DATA_ROOT}/codex/sessions`,
    });
  });

  it("the explicit M3b seam uses an authenticated HTTP-only loopback provider", async () => {
    const fake = newFake();
    await launchCodexRoot(
      baseSpec({ useAppServerAuth: true, authMode: "subscription" }),
      baseDeps(fake, { appServerAuthOpenAIBaseUrlForTest: "http://127.0.0.1:43123/v1" }),
    );
    const configFile = treeCalls[0]?.files[0];
    assert.ok(configFile);
    assert.match(configFile.content, /^model_provider = "uzi-m3b-openai"$/m);
    assert.match(configFile.content, /^\[model_providers\."uzi-m3b-openai"\]$/m);
    assert.match(configFile.content, /^base_url = "http:\/\/127\.0\.0\.1:43123\/v1"$/m);
    assert.match(configFile.content, /^supports_websockets = false$/m);
    assert.match(configFile.content, /^requires_openai_auth = true$/m);
    assert.equal(spawnCalls[0]?.options.env.CODEX_PROVIDER_KEY, undefined);
  });

  it("derives the only allowed session-seed source from the managed provider root", async () => {
    const fake = newFake();
    await launchCodexRoot(
      baseSpec({ useAppServerAuth: true, authMode: "subscription", seedSession: true }),
      baseDeps(fake),
    );
    assert.equal(treeCalls[0]?.sessionSeedDir, `${DATA_ROOT}.session-seed/sessions`);
    assert.ok(treeCalls[0]?.sharedSessionRead, "the final 0710/2750 posture is provisioned before seeding");
    assert.equal(spawnCalls.length, 1, "the supervisor starts only after makeRunnerTrees completes");
  });

  it("the real session seed succeeds with an epoch TMPDIR longer than a Unix socket path", {
    skip: process.platform === "linux" ? false : "the real seed copier requires Linux /proc/self/fd; it runs in Linux CI without a worker-uid gate",
  }, async (t) => {
    const parent = await fs.mkdtemp(path.join(os.tmpdir(), "codex-seed-long-"));
    const root = path.join(parent, "agent-home", "a".repeat(36), "codex-data", `${"b".repeat(36)}-epoch-1`);
    const seedDir = `${root}.session-seed/sessions`;
    const sessions = path.join(root, "codex", "sessions");
    const tmp = path.join(root, "tmp");
    const helper = fileURLToPath(new URL("../src/codex/session-seed-cli.ts", import.meta.url));
    const spawnSync = childProcess.spawnSync;
    let seedCalls = 0;
    let sharedSessionPosture = false;
    try {
      await fs.mkdir(seedDir, { recursive: true });
      await fs.mkdir(sessions, { recursive: true });
      await fs.mkdir(path.join(root, "home"), { recursive: true });
      await fs.mkdir(tmp, { recursive: true });
      await fs.writeFile(path.join(seedDir, "rollout-planted.jsonl"), "saved session\n");
      assert.ok(Buffer.byteLength(path.join(tmp, `tsx-${process.getuid?.() ?? 0}`, "1.pipe")) > 108,
        "even the shortest CLI socket name exceeds Linux and macOS Unix socket limits");

      // Keep the launcher's real provisioning/seed command construction. Stub only the
      // privileged shell steps and translate image paths/uid wrapper for this host; the
      // seed subprocess, loader, helper, TMPDIR, environment and stdio are real.
      const mocked = t.mock.method(childProcess, "spawnSync", (command: string, args: readonly string[], options: childProcess.SpawnSyncOptions) => {
        assert.equal(command, "/bin/setpriv");
        const prefix = setprivRunnerArgs();
        assert.deepEqual(args.slice(0, prefix.length), prefix);
        const executable = args[prefix.length]!;
        if (executable === "/bin/sh" || executable === "/bin/rm") {
          let stdout = Buffer.alloc(0);
          const shellArgs = args.slice(prefix.length + 1);
          if (executable === "/bin/sh" && shellArgs[1] === 'stat -c "%u:%g:%a" -- "$1"') {
            const requestedPath = shellArgs[3];
            assert.equal(typeof requestedPath, "string");
            const codexHome = path.join(root, "codex");
            const configPath = path.join(codexHome, "config.toml");
            assert.ok([
              root, codexHome, sessions, configPath, tmp, path.join(root, "home"),
              ...["config", "cache", "data", "state"].map((dir) => path.join(root, `xdg-${dir}`)),
            ].includes(requestedPath!));
            const shared = sharedSessionPosture && [root, codexHome, sessions].includes(requestedPath!);
            const mode = requestedPath === configPath ? "600"
              : shared ? (requestedPath === sessions ? "2750" : "710") : "700";
            stdout = Buffer.from(`${RUNNER_UID}:${shared ? CODEX_SESSION_GID : RUNNER_UID}:${mode}\n`);
          } else if (executable === "/bin/sh") {
            if (shellArgs[1]?.includes("chgrp")) sharedSessionPosture = true;
            else if (shellArgs[1]?.includes("mkdir -m 700")) sharedSessionPosture = false;
          }
          return { status: 0, signal: null, pid: 0, output: [], stdout, stderr: Buffer.alloc(0) };
        }
        seedCalls++;
        const argv = args.slice(prefix.length + 1).map((arg) => arg === "/app/src/codex/session-seed-cli.ts" ? helper : arg);
        const localExecutable = executable === "/app/node_modules/.bin/tsx"
          ? process.execPath : executable;
        if (executable === "/app/node_modules/.bin/tsx") argv.unshift(fileURLToPath(import.meta.resolve("tsx/cli")));
        assert.equal(options.env?.PATH, "/usr/local/bin:/usr/bin:/bin");
        assert.equal(options.env?.TMPDIR, tmp);
        assert.equal(options.env?.HOME, path.join(root, "home"));
        assert.equal(options.env?.CODEX_CANARY_LEAK, undefined);
        assert.deepEqual(options.stdio, ["ignore", "ignore", "pipe"]);
        const result = spawnSync(localExecutable, argv, options);
        if (seedCalls === 1 && result.status !== 0) t.diagnostic(String(result.stderr));
        return result;
      });
      syncBuiltinESMExports();
      try {
        await launchCodexRoot(
          baseSpec({ ownedDataRoot: root, useAppServerAuth: true, authMode: "subscription", seedSession: true }),
          baseDeps(newFake(), { makeRunnerTrees: undefined }),
        );
        assert.equal(seedCalls, 1);
        assert.equal(await fs.readFile(path.join(sessions, "rollout-planted.jsonl"), "utf8"), "saved session\n");
        assert.equal(spawnCalls.length, 1, "the supervisor starts after the real seed succeeds");

        // An empty seed still fails closed; changing the interpreter must not turn a
        // rejected helper invocation into permission to start the provider.
        await fs.rm(path.join(seedDir, "rollout-planted.jsonl"));
        await assert.rejects(launchCodexRoot(
          baseSpec({ ownedDataRoot: root, useAppServerAuth: true, authMode: "subscription", seedSession: true }),
          baseDeps(newFake(), { makeRunnerTrees: undefined }),
        ), /runner-owned session seed failed \(exit 2\)/);
        assert.equal(spawnCalls.length, 1, "the failed seed never starts another supervisor");
      } finally {
        mocked.mock.restore();
        syncBuiltinESMExports();
      }
    } finally {
      await fs.rm(parent, { recursive: true, force: true });
    }
  });

  it("refuses the M3b redirect without app-server auth", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(
        baseSpec(),
        baseDeps(fake, { appServerAuthOpenAIBaseUrlForTest: "http://127.0.0.1:43123/v1" }),
      ),
      /requires app-server auth/,
    );
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });

  it("refuses session seeding outside a managed-auth provider root", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ seedSession: true }), baseDeps(fake)),
      /session seeding requires a managed-auth provider root/,
    );
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });

  it("the ABSENT flag keeps the transitional env-key path (custom table + injected credential)", async () => {
    const fake = newFake();
    await launchCodexRoot(baseSpec(), baseDeps(fake)); // no useAppServerAuth
    const env = spawnCalls[0]?.options.env ?? {};
    assert.equal(env.CODEX_PROVIDER_KEY, "dummy-key", "the transitional path still injects the credential var");
    const configFile = treeCalls[0]?.files[0];
    assert.ok(configFile);
    assert.match(configFile.content, /\[model_providers\./, "the transitional path declares the custom provider table");
    assert.match(configFile.content, /requires_openai_auth = false/);
  });

  it("rejects useAppServerAuth without an explicit auth mode (fail-closed, before provisioning)", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ useAppServerAuth: true }), baseDeps(fake)),
      /app-server auth requires an explicit/,
    );
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });

  it("rejects useAppServerAuth on a non-provider (command) root", async () => {
    const fake = newFake({ uid: COMMAND_UID });
    await assert.rejects(
      launchCodexRoot(
        baseSpec({ kind: "command", childArgv: ["exec"], useAppServerAuth: true, authMode: "api_key" }),
        baseDeps(fake),
      ),
      /only supported for a provider root/,
    );
  });
});

describe("launchCodexEffectRoot: supervised command identity", () => {
  // PRD #1493 M3: the missing negative twin of launchCodexRoot's supported-profile gate.
  // Both launcher guards stay EXACTLY as they are; this only pins the effect-root refusal.
  it("REFUSES to launch (CodexUnsupportedProfileError) when the uid split is not active", async () => {
    const fake = newFake({ uid: COMMAND_UID });
    await assert.rejects(
      launchCodexEffectRoot(
        {
          identity: "command",
          command: "/bin/sh",
          args: ["-c", "true"],
          cwd: "/work/repo",
          env: { PATH: "/usr/bin:/bin", HOME: "/tmp", TMPDIR: "/tmp" },
          supervisorBin: SUPERVISOR_BIN,
        },
        // An env WITHOUT UZI_UID_SPLIT models a single-uid / shared-uid profile.
        baseDeps(fake, { env: { PATH: "/usr/bin" }, resolveCommandUid: () => COMMAND_UID }),
      ),
      (err: unknown) => {
        assert.ok(err instanceof CodexUnsupportedProfileError);
        assert.match((err as Error).message, /effect roots require the A1 uid split/);
        return true;
      },
    );
    assert.equal(spawnCalls.length, 0, "no supervisor spawn under an unsupported profile");
  });

  it("launches uid 10003 with a replaced env and reports the primary child status", async () => {
    const fake = newFake({ uid: COMMAND_UID });
    const env = { PATH: "/usr/bin:/bin", HOME: "/tmp", TMPDIR: "/tmp" };
    // Runtime assembly preserves the UUID-format cleanup-token assertion without
    // committing a generic-api-key-shaped false positive to the public tree.
    const cleanupToken = ["01234567", "89ab", "cdef", "0123", "456789abcdef"].join("-");
    const handle = await launchCodexEffectRoot(
      {
        identity: "command",
        command: "/bin/sh",
        args: ["-c", "exit 7"],
        cwd: "/work/repo",
        env,
        supervisorBin: SUPERVISOR_BIN,
        cleanupToken,
      },
      baseDeps(fake, { resolveCommandUid: () => COMMAND_UID }),
    );
    const call = spawnCalls[0];
    assert.ok(call);
    assert.equal(call.command, "/bin/setpriv");
    assert.deepEqual(call.args, [
      ...setprivArgsForUid(COMMAND_UID),
      SUPERVISOR_BIN,
      "--expect-uid", String(COMMAND_UID),
      "--cleanup-token", cleanupToken,
      "--", "/bin/sh", "-c", "exit 7",
    ]);
    // issue #1783 (R4): a command/effect root runs model-directed work: NO worker spawn mark.
    assert.deepEqual(call.options.env, env);
    fake.emitChildExit(7);
    assert.deepEqual(await handle.waitChild(), { event: "child_exit", code: 7 });
  });

  it("keeps a permit-held worker action at uid 10001 while still using the fixed supervisor", async () => {
    const fake = newFake({ uid: WORKER_UID });
    const env = { PATH: "/usr/bin:/bin", GIT_CONFIG_COUNT: "0" };
    await launchCodexEffectRoot(
      {
        identity: "worker_pat",
        command: "/usr/bin/git",
        args: ["status"],
        cwd: "/work/repo",
        env,
        supervisorBin: SUPERVISOR_BIN,
      },
      baseDeps(fake, { resolveWorkerUid: () => WORKER_UID }),
    );
    const call = spawnCalls[0];
    assert.ok(call);
    assert.equal(call.command, "/bin/setpriv", "the worker uid is retained while its controller caps are cleared");
    assert.deepEqual(call.args, [
      ...setprivArgsForUid(WORKER_UID), SUPERVISOR_BIN,
      "--expect-uid", String(WORKER_UID), "--drop-controller-caps", "--", "/usr/bin/git", "status",
    ]);
    // issue #1783 (R4): a command/effect root runs model-directed work: NO worker spawn mark.
    assert.deepEqual(call.options.env, env);
  });

  it("issue #2213: a quarantined worker spawns no effect root whose env carries the forge credential", async () => {
    const fake = newFake({ uid: WORKER_UID });
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    const spec = {
      identity: "worker_pat" as const,
      command: "/usr/bin/git",
      args: ["fetch"],
      cwd: "/work/repo",
      supervisorBin: SUPERVISOR_BIN,
    };
    await assert.rejects(
      launchCodexEffectRoot(
        { ...spec, env: { PATH: "/usr/bin:/bin", GIT_CONFIG_VALUE_0: "Authorization: Basic ZmFrZTpmYWtl" } },
        baseDeps(fake, { resolveWorkerUid: () => WORKER_UID }),
      ),
      ResidueQuarantinedError,
    );
    assert.equal(spawnCalls.length, 0);
    await launchCodexEffectRoot({ ...spec, env: { PATH: "/usr/bin:/bin" } }, baseDeps(fake, { resolveWorkerUid: () => WORKER_UID }));
    assert.equal(spawnCalls.length, 1, "a credential-free effect root is untouched");
  });
});

describe("launchCodexRoot: trusted construction contract", () => {
  it("rejects a caller-selected supervisor before provisioning any tree", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ supervisorBin: "/data/runner/fake-supervisor" }), baseDeps(fake)),
      /supervisorBin must equal the immutable image path/,
    );
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });

  it("rejects a provider root that does not use the pinned Codex app-server target", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ codexBin: "/data/runner/fake-codex" }), baseDeps(fake)),
      /provider roots must use the pinned Codex app-server target/,
    );
  });

  it("trusted refusal: rejects allowlist-reserved and malformed provider env keys", async () => {
    for (const envKey of ["PATH", "BAD-KEY"]) {
      await assert.rejects(
        launchCodexRoot(baseSpec({ provider: { ...baseSpec().provider, envKey } }), baseDeps(newFake())),
        (error: unknown) => error instanceof TrustedExecutionRefusal
          && error.message === "provider envKey is invalid or reserved by the launcher allowlist",
      );
    }
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });

  it("rejects an owned data root that overlaps the target repository", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ ownedDataRoot: "/work/repo/.codex-state" }), baseDeps(fake)),
      /ownedDataRoot and cwd must be disjoint/,
    );
  });

  it("the production tree creator refuses the chosen existing provider root before launch and preserves its contents",
    { skip: process.getuid?.() === WORKER_UID ? false : "requires the worker uid to launch the runner-owned creator" },
    async () => {
      // The root entrypoint makes TMPDIR worker-private (0700). The real runner
      // must traverse this fixture's parent to observe EEXIST, rather than EACCES.
      const parent = await fs.mkdtemp(path.join("/tmp", "uzi-codex-existing-"));
      const root = path.join(parent, "chosen-provider-root");
      const marker = path.join(root, "keep.txt");
      try {
        await fs.chmod(parent, 0o777);
        await fs.mkdir(root);
        await fs.writeFile(marker, "existing root must survive");
        const fake = newFake();
        await assert.rejects(
          launchCodexRoot(baseSpec({ ownedDataRoot: root }), baseDeps(fake, { makeRunnerTrees: undefined, redactDiagnostic: (t) => t })),
          // EEXIST specifically: a reuse-permitting create would fail later (or not at all), never here.
          /runner-owned tree creation failed[\s\S]*File exists/,
        );
        assert.equal(await fs.readFile(marker, "utf8"), "existing root must survive");
        assert.equal(spawnCalls.length, 0, "the provider supervisor was never launched");
      } finally {
        await fs.rm(parent, { recursive: true, force: true });
      }
    });

  it("rejects a non-canonical owned data root before provisioning", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ ownedDataRoot: "/data/run/../root-1" }), baseDeps(fake)),
      /canonical absolute non-root path/,
    );
    assert.equal(treeCalls.length, 0);
    assert.equal(spawnCalls.length, 0);
  });
});

describe("launchCodexRoot: happy-path lifecycle over the fake supervisor", () => {
  it("(e) surfaces started, snapshot, and a drained dispose", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));

    assert.equal(handle.started.event, "started");
    assert.equal(handle.started.uid, RUNNER_UID);
    assert.equal(handle.started.childPid, fake.pid + 1);
    assert.equal(handle.supervisorPid, fake.pid);
    assert.ok(handle.transport.stdin, "app-server transport stdin exposed");
    assert.ok(handle.transport.stdout, "app-server transport stdout exposed");

    const snap = await handle.snapshot();
    assert.equal(snap.event, "snapshot");
    assert.equal(snap.processes[0]?.comm, "codex");

    const outcome = await handle.dispose(1500);
    assert.equal(outcome.clean, true);
    assert.ok(outcome.clean && outcome.event.state === "drained");
    assert.deepEqual(treeRemoveCalls, [{ uid: RUNNER_UID, kind: "provider", root: DATA_ROOT }]);
    assert.equal((await handle.dispose(1500)).clean, true, "dispose stays idempotent");
    assert.equal(treeRemoveCalls.length, 1, "a clean tree removal runs once");
    assert.equal(handle.failed, undefined);
  });

  it("coalesces concurrent registered root reap and disposal without poisoning", async () => {
    const fake = newFake({ singleDisposeResponse: true, exitDelayMs: 50 });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const root = registeredRoot(handle, "boundary_action");
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reservation = registry.reserveLaunch(root.kind);
    assert.equal(reservation.kind, "reserved");
    if (reservation.kind !== "reserved") return;
    registry.registerRoot(reservation.reservation, root);
    const reap = registry.reapRoot(root, 1000);
    const disposal = root.dispose(1000);
    const results = await Promise.allSettled([reap, disposal]);
    assert.equal(fake.disposeTimeouts.length, 1, "one supervisor disposal request");
    assert.deepEqual(results, [
      { status: "fulfilled", value: { ok: true } },
      { status: "fulfilled", value: undefined },
    ]);
    assert.equal(registry.isPoisoned(), false);
    assert.equal(handle.failed, undefined);
    assert.equal((await handle.dispose(500)).clean, true);
    assert.equal(fake.disposeTimeouts.length, 1, "repeated clean disposal stays idempotent");
    assert.equal(treeRemoveCalls.length, 1);
  });

  it("bounds a shorter concurrent disposal caller without a second request", async () => {
    const fake = newFake({ singleDisposeResponse: true, exitDelayMs: 150 });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const first = handle.dispose(2000);
    const start = Date.now();
    const second = await handle.dispose(20);
    assert.equal(second.clean, false, "short caller cannot inherit the first caller's budget");
    assert.ok(Date.now() - start < 100, "short caller returns before the supervisor exits");
    assert.equal((await first).clean, true);
    assert.equal(fake.disposeTimeouts.length, 1);
    assert.equal(handle.failed, undefined);
    assert.equal((await handle.dispose(500)).clean, true);
  });

  it("retries after a shared unconfirmed disposal without removing the owned tree", async () => {
    const fake = newFake({ disposeState: "unconfirmed" });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcomes = await Promise.all([handle.dispose(500), handle.dispose(500)]);
    assert.equal(fake.disposeTimeouts.length, 1);
    assert.equal(outcomes[0]?.clean, false);
    assert.deepEqual(outcomes[1], outcomes[0]);
    assert.equal((await handle.dispose(500)).clean, false);
    assert.equal(fake.disposeTimeouts.length, 2, "an unclean operation is not cached");
    assert.equal(treeRemoveCalls.length, 0);
  });

  it("reserves caller budget for exit confirmation after a near-deadline drain", async () => {
    const fake = newFake({ disposeNearBudget: true, exitDelayMs: 10 });
    const handle = await launchCodexRoot(
      baseSpec(),
      baseDeps(fake, { deadlines: { started: 1000, snapshot: 1000, dispose: 1000, exit: 300 } }),
    );

    // Budgets are wide enough that CPU contention cannot eat the exit-confirmation
    // margin on real timers; the four-fifths ratio below is the property under test.
    const outcome = await handle.dispose(1000);
    assert.equal(outcome.clean, true);
    assert.ok((fake.disposeTimeouts[0] ?? 1000) <= 800, "drain received no more than four fifths of the total budget");
  });
});

describe("createHandle: strict tmpCleanup evidence", () => {
  it("accepts and surfaces a removed tmpCleanup on the clean dispose outcome", async () => {
    const fake = newFake({ disposeTmpCleanup: { state: "removed", reason: "" } });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, true);
    assert.deepEqual(outcome.clean ? outcome.event.tmpCleanup : undefined, { state: "removed", reason: "" });
    assert.equal(handle.failed, undefined);
  });

  it("a retained tmpCleanup is still a clean disposal and surfaces its reason", async () => {
    const fake = newFake({ disposeTmpCleanup: { state: "retained", reason: "absent" } });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, true, "a retained tmp never makes the process disposal unclean");
    assert.deepEqual(outcome.clean ? outcome.event.tmpCleanup : undefined, { state: "retained", reason: "absent" });
  });

  it("a dispose without tmpCleanup carries no field", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, true);
    assert.equal(outcome.clean && "tmpCleanup" in outcome.event, false);
  });

  const malformed: ReadonlyArray<[string, unknown]> = [
    ["an unknown state", { state: "deleted", reason: "" }],
    ["a missing state", { reason: "" }],
    ["an uppercase reason", { state: "retained", reason: "Mismatch" }],
    ["a reason over 32 chars", { state: "retained", reason: "a".repeat(33) }],
    ["a reason with a path", { state: "retained", reason: "/tmp/x" }],
    ["a non-string reason", { state: "retained", reason: 5 }],
    ["a missing reason", { state: "removed" }],
    ["an extra key", { state: "removed", reason: "", path: "/tmp/x" }],
    ["a string", "removed"],
    ["null", null],
    ["an array", ["removed", ""]],
  ];
  for (const [label, value] of malformed) {
    it(`fails closed on a dispose tmpCleanup with ${label}`, async () => {
      const fake = newFake({ disposeTmpCleanup: value });
      const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
      const outcome = await handle.dispose(500);
      assert.equal(outcome.clean, false);
      assert.match(outcome.clean ? "" : outcome.reason, /malformed tmpCleanup evidence/);
      assert.match(String(handle.failed?.message), /malformed tmpCleanup evidence/);
    });
  }

  it("fails closed on an abnormal event with a malformed tmpCleanup", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeEvidence({ event: "abnormal", reason: "control EOF", cleanup: { state: "drained" }, tmpCleanup: { state: "RETAINED", reason: "" } });
    const failure = await handle.whenFailed;
    assert.match(failure.message, /malformed tmpCleanup evidence/);
  });

  it("an abnormal event with a valid tmpCleanup still fails the root as abnormal", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeEvidence({ event: "abnormal", reason: "control EOF", cleanup: { state: "drained" }, tmpCleanup: { state: "removed", reason: "" } });
    const failure = await handle.whenFailed;
    assert.match(failure.message, /supervisor abnormal: control EOF/);
  });
});

describe("TMP_RETAINED_REASONS matches Go safetree.Reason", () => {
  it("holds exactly the non-empty string literals func Reason returns", () => {
    const goSrc = readFileSync(new URL("../codex/supervisor/internal/safetree/safetree.go", import.meta.url), "utf8");
    const body = /^func Reason\(err error\) string \{\n([\s\S]*?)^\}$/m.exec(goSrc)?.[1];
    assert.ok(body !== undefined, "func Reason not found in safetree.go");
    const returned = [...body.matchAll(/\breturn "([^"]*)"/g)].map((m) => m[1]);
    // The "" for a nil error is not a retained reason.
    assert.ok(returned.includes(""), "func Reason no longer returns \"\" for nil");
    const goReasons = new Set(returned.filter((r) => r !== ""));
    assert.ok(goReasons.size >= 6, `only ${goReasons.size} reasons parsed from func Reason`);
    assert.deepEqual([...TMP_RETAINED_REASONS].sort(), [...goReasons].sort());
  });
});

describe("createHandle: tmpCleanup must carry meaning, not just shape", () => {
  /** whenFailed, bounded: an accepted record never fails the root, and must redden the test rather than hang it. */
  const failedWithin = (handle: { whenFailed: Promise<Error> }, ms = 1000): Promise<Error> =>
    Promise.race([handle.whenFailed, new Promise<Error>((resolve) => setTimeout(() => resolve(new Error("root did not fail")), ms).unref())]);

  for (const reason of ["mismatch", "owner", "deadline", "io", "absent", "name"]) {
    it(`accepts a retained tmpCleanup with the fixed reason "${reason}"`, async () => {
      const fake = newFake({ disposeTmpCleanup: { state: "retained", reason } });
      const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
      const outcome = await handle.dispose(500);
      assert.equal(outcome.clean, true);
      assert.deepEqual(outcome.clean ? outcome.event.tmpCleanup : undefined, { state: "retained", reason });
    });
  }

  const meaningless: ReadonlyArray<[string, unknown]> = [
    ["a removed state with a non-empty reason", { state: "removed", reason: "absent" }],
    ["a retained state with an empty reason", { state: "retained", reason: "" }],
    ["a retained state with an unknown lowercase word", { state: "retained", reason: "gone" }],
    ["a retained state with a reason that only prefixes a fixed word", { state: "retained", reason: "iox" }],
  ];
  for (const [label, value] of meaningless) {
    it(`fails closed on a drained dispose tmpCleanup with ${label}`, async () => {
      const fake = newFake({ disposeTmpCleanup: value });
      const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
      const outcome = await handle.dispose(500);
      assert.equal(outcome.clean, false);
      assert.match(String(handle.failed?.message), /malformed tmpCleanup evidence/);
    });
  }

  const valid = { state: "removed", reason: "" };
  const misplaced: ReadonlyArray<[string, Record<string, unknown>]> = [
    ["an unconfirmed dispose", { event: "dispose", id: 99, state: "unconfirmed", reason: "deadline", killed: [], reaped: [], children: [], tmpCleanup: valid }],
    ["a dispose with no state", { event: "dispose", id: 99, tmpCleanup: valid }],
    ["a snapshot", { event: "snapshot", id: 99, processes: [], tmpCleanup: valid }],
    ["a child_exit", { event: "child_exit", code: 0, tmpCleanup: valid }],
    ["an abnormal whose cleanup is unconfirmed", { event: "abnormal", reason: "control EOF", cleanup: { state: "unconfirmed" }, tmpCleanup: valid }],
    ["a pre-fork abnormal with no cleanup", { event: "abnormal", reason: "command tmp setup failed", tmpCleanup: valid }],
    ["an abnormal whose cleanup is not an object", { event: "abnormal", reason: "control EOF", cleanup: "drained", tmpCleanup: valid }],
    ["an unknown event", { event: "later", tmpCleanup: valid }],
  ];
  for (const [label, record] of misplaced) {
    it(`fails closed on a valid tmpCleanup on ${label}`, async () => {
      const fake = newFake();
      const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
      fake.writeEvidence(record);
      const failure = await failedWithin(handle);
      assert.match(failure.message, /malformed tmpCleanup evidence/);
      const outcome = await handle.dispose(500);
      assert.equal(outcome.clean, false);
      assert.equal(outcome.clean ? undefined : outcome.tmpCleanup, undefined, "a rejected tmpCleanup is never surfaced");
    });
  }

  it("a started event carrying tmpCleanup rejects the launch", async () => {
    const fake = newFake({ autoStarted: false });
    const launched = launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeEvidence({
      event: "started", supervisorPid: fake.pid, childPid: fake.pid + 1, subreaper: true, nondumpable: true, uid: RUNNER_UID,
      liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true, tmpCleanup: valid,
    });
    await assert.rejects(launched, /malformed tmpCleanup evidence/);
  });

  it("surfaces a drained abnormal's retained tmpCleanup on the unclean dispose outcome", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeEvidence({ event: "abnormal", reason: "control EOF", cleanup: { state: "drained", authority: "ECHILD+__WALL", killed: [], reaped: [] }, tmpCleanup: { state: "retained", reason: "owner" } });
    const failure = await failedWithin(handle);
    assert.match(failure.message, /supervisor abnormal: control EOF/);
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, false);
    assert.deepEqual(outcome.clean ? undefined : outcome.tmpCleanup, { state: "retained", reason: "owner" });
  });

  it("an abnormal without tmpCleanup leaves the unclean outcome without one", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.emitAbnormal("control EOF");
    await failedWithin(handle);
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, false);
    assert.equal(outcome.clean ? undefined : outcome.tmpCleanup, undefined);
  });
});

describe("launchCodexRoot: abnormal paths never claim clean disposal", () => {
  it("treats a provider primary-child exit as a root failure", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.emitChildExit(17);
    const failure = await handle.whenFailed;
    assert.match(failure.message, /provider child exited unexpectedly.*17/);
  });

  it("(f1) an `abnormal` evidence frame fails the root", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.emitAbnormal("controller loss");
    await handle.whenFailed;
    assert.ok(handle.failed);
    const outcome = await handle.dispose();
    assert.equal(outcome.clean, false);
    assert.match(outcome.clean ? "" : outcome.reason, /abnormal/);
  });

  it("(f2) a spontaneous non-zero exit fails the root (no clean disposal)", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.exitWith(2);
    await handle.whenFailed;
    const outcome = await handle.dispose();
    assert.equal(outcome.clean, false);
  });

  it("(f3) a dispose `unconfirmed` is NOT clean", async () => {
    const fake = newFake({ disposeState: "unconfirmed" });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(200);
    assert.equal(outcome.clean, false);
    assert.ok(!outcome.clean && outcome.event?.state === "unconfirmed");
    assert.match(outcome.clean ? "" : outcome.reason, /unconfirmed/);
    assert.equal(treeRemoveCalls.length, 0, "an unconfirmed disposal retains the owned tree");
  });

  it("retains a runner-owned tree without replacing clean supervisor disposal when removal fails", async () => {
    const fake = newFake();
    let attempts = 0;
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake, {
      removeRunnerTree: () => {
        attempts += 1;
        throw new Error("cleanup refused");
      },
    }));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, true, "disk reclamation does not replace proven process disposal");
    assert.equal(attempts, 1);
    assert.equal(treeCleanupFailures, 1, "the path-free cleanup diagnostic fires once");
    assert.equal((await handle.dispose(500)).clean, true);
    assert.equal(attempts, 1, "a failed best-effort cleanup is not retried on idempotent dispose");
  });

  it("(f4) a drained report contradicted by a non-zero exit is NOT clean", async () => {
    const fake = newFake({ disposeState: "drained", exitCode: 3 });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, false);
    assert.match(outcome.clean ? "" : outcome.reason, /non-zero/);
  });

  it("(f4b) a drained report without ECHILD+__WALL authority is NOT clean", async () => {
    const fake = newFake({ disposeAuthority: "process-group" });
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    const outcome = await handle.dispose(500);
    assert.equal(outcome.clean, false);
    assert.match(outcome.clean ? "" : outcome.reason, /missing required authority/);
  });

  it("(f5) an oversized evidence line is a bounded rejection", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeRawEvidence(JSON.stringify({ event: "snapshot", id: 999, pad: "x".repeat(70000) }));
    await handle.whenFailed;
    assert.match(String(handle.failed?.message), /oversized evidence line/);
    await assert.rejects(handle.snapshot(200), /oversized evidence line/);
  });

  it("(f6) a malformed evidence line is a bounded rejection", async () => {
    const fake = newFake();
    const handle = await launchCodexRoot(baseSpec(), baseDeps(fake));
    fake.writeRawEvidence("{not json");
    await handle.whenFailed;
    assert.match(String(handle.failed?.message), /malformed evidence line/);
  });

  it("times out (bounded) when `started` never arrives", async () => {
    const fake = newFake({ autoStarted: false });
    await assert.rejects(
      launchCodexRoot(baseSpec(), baseDeps(fake, { deadlines: { started: 40, snapshot: 40, dispose: 40, exit: 40 } })),
      /started deadline exceeded/,
    );
  });
});

describe("runLaunchCli: packaged entrypoint (knip-visible import)", () => {
  it("awaits started, disposes on the shutdown trigger, and returns 0 on a clean dispose", async () => {
    // A minimal fake handle — this test exercises the CLI flow, not the launcher.
    const disposed: number[] = [];
    const fakeHandle = {
      started: { event: "started" as const, supervisorPid: 1, childPid: 2, subreaper: true, nondumpable: true, uid: RUNNER_UID, liveCapsZero: true, capBoundingSet: "0xc0" as const, noNewPrivs: true },
      supervisorPid: 1,
      transport: { stdin: null, stdout: null, stderr: null },
      snapshot: () => Promise.reject(new Error("unused")),
      waitChild: () => Promise.resolve({ event: "child_exit" as const, code: 0 }),
      dispose: () => { disposed.push(1); return Promise.resolve({ clean: true as const, event: { event: "dispose" as const, id: 1, state: "drained" as const } }); },
      failed: undefined,
      whenFailed: new Promise<Error>(() => { /* never fails in this test */ }),
    };
    const lines: string[] = [];
    const stderr = new Writable({ write(chunk, _enc, cb) { lines.push(String(chunk)); cb(); } });
    const deps: LaunchCliDeps = {
      launch: () => Promise.resolve(fakeHandle),
      loadSpec: () => baseSpec(),
      proxyStdio: false,
      until: Promise.resolve("eof"),
      stderr,
    };
    const code = await runLaunchCli(["node", "launch-cli", "/spec.json"], deps);
    assert.equal(code, 0);
    assert.equal(disposed.length, 1, "the CLI disposed the root");
    const joined = lines.join("");
    assert.match(joined, /"event":"ready"/);
    assert.match(joined, /"event":"final"/);
    assert.match(joined, /"clean":true/);
  });

  it("returns 2 and reports a spec-error on an unloadable spec", async () => {
    const lines: string[] = [];
    const stderr = new Writable({ write(chunk, _enc, cb) { lines.push(String(chunk)); cb(); } });
    const code = await runLaunchCli(["node", "launch-cli"], {
      loadSpec: () => { throw new Error("usage: launch-cli <spec.json>"); },
      stderr,
    });
    assert.equal(code, 2);
    assert.match(lines.join(""), /"event":"spec-error"/);
  });
});

describe("launchCodexRoot: provisioning failure diagnostics", () => {
  const SEED_ARG = "/app/src/codex/session-seed-cli.ts";
  type Step = "tree" | "share" | "seed" | "write" | "rm" | "statTree" | "statShare" | "statWrite";
  type Result = Partial<{ status: number | null; signal: NodeJS.Signals | null; stderr: string; stdout: string; error: Error }>;
  const provisionCalls: { command: string; args: readonly string[]; step: Step; options: SpawnSyncOptions }[] = [];

  function scripted(results: Partial<Record<Step, Result>>): ProvisionSpawnSync {
    let action: "tree" | "share" | "write" = "tree";
    return (command, args, options) => {
      const a = [...args];
      const metadata = a.includes('stat -c "%u:%g:%a" -- "$1"');
      const step: Step = metadata ? (action === "tree" ? "statTree" : action === "share" ? "statShare" : "statWrite")
        : a.includes(SEED_ARG) ? "seed"
        : a.includes("--") && a.includes("-rf") ? "rm"
        : provisionCalls.length === 0 ? "tree"
        : a.some((x) => x.includes("chgrp")) ? "share" : "write";
      if (step === "tree" || step === "share" || step === "write") action = step;
      provisionCalls.push({ command, args: a, step, options });
      const r = results[step] ?? {};
      const sharedSession = a.at(-1)?.endsWith("/sessions") === true;
      const mode = action === "share" ? (sharedSession ? "2750" : "710") : action === "write" ? "600" : "700";
      const gid = action === "share" ? CODEX_SESSION_GID : RUNNER_UID;
      return {
        pid: 1, output: [], stdout: Buffer.from(r.stdout ?? (metadata ? `${RUNNER_UID}:${gid}:${mode}\n` : "")),
        status: r.status === undefined ? 0 : r.status,
        signal: r.signal ?? null,
        stderr: Buffer.from(r.stderr ?? ""),
        ...(r.error === undefined ? {} : { error: r.error }),
      } as ReturnType<ProvisionSpawnSync>;
    };
  }
  function seedSpec(): CodexLaunchSpec {
    return baseSpec({ useAppServerAuth: true, authMode: "subscription", seedSession: true });
  }
  async function failure(results: Partial<Record<Step, Result>>, deps: Partial<LauncherDeps>, spec = seedSpec()): Promise<string> {
    provisionCalls.length = 0;
    const fake = newFake();
    try {
      await launchCodexRoot(spec, baseDeps(fake, { makeRunnerTrees: undefined, provisionSpawnSync: scripted(results), ...deps }));
    } catch (e) {
      return (e as Error).message;
    }
    assert.fail("expected the launch to fail");
  }
  for (const [name, step, result, typed, expected] of [
    ["owner mismatch", "statTree", { stdout: "999:10002:700\n" }, true, "runner-owned tree creation failed (exit 1): "],
    ["mode mismatch", "statTree", { stdout: "10002:10002:755\n" }, true, "runner-owned tree creation failed (exit 1): "],
    ["group mismatch", "statShare", { stdout: "10002:999:710\n" }, true, "runner-owned session export posture failed (exit 1): "],
    ["file mode mismatch", "statWrite", { stdout: "10002:10002:644\n" }, true, "runner-owned file write failed for /data/run/root-1/codex/config.toml (exit 1): "],
    ["malformed shared metadata", "statShare", { stdout: "(bad)" }, false, "runner-owned session export posture failed (exit 1): "],
    ["malformed file metadata", "statWrite", { stdout: "[1,2]" }, false, "runner-owned file write failed for /data/run/root-1/codex/config.toml (exit 1): "],
    ["metadata I/O", "statTree", { status: 1, stderr: "stat: EIO" }, false, "runner-owned tree creation failed (exit 1): stat: EIO"],
    ["malformed metadata", "statTree", { stdout: "10002:10002:700\nextra" }, false, "runner-owned tree creation failed (exit 1): "],
    ["raw creation I/O", "tree", { status: 1, stderr: "mkdir: EIO" }, false, "runner-owned tree creation failed (exit 1): mkdir: EIO"],
  ] as const) {
    it(`trusted refusal: provisioning distinguishes ${name}`, async () => {
      provisionCalls.length = 0;
      await assert.rejects(launchCodexRoot(seedSpec(), baseDeps(newFake(), {
        makeRunnerTrees: undefined, provisionSpawnSync: scripted({ [step]: result }), redactDiagnostic: (text) => text,
      })), (error: unknown) => {
        assert.ok(error instanceof Error);
        assert.equal(error instanceof TrustedExecutionRefusal, typed);
        assert.equal(error.message, expected);
        return true;
      });
      assert.equal(spawnCalls.length, 0);
      if (step !== "tree") {
        assert.ok(provisionCalls.some((call) => call.step === "rm"));
        const observation = provisionCalls.find((call) => call.step === step);
        assert.deepEqual(observation?.options.stdio, ["ignore", "pipe", "pipe"]);
        assert.equal(observation?.options.timeout, 5000);
      }
    });
  }

  it("trusted refusal: valid metadata permits tree, share, seed and file provisioning", async () => {
    provisionCalls.length = 0;
    const handle = await launchCodexRoot(seedSpec(), baseDeps(newFake(), {
      makeRunnerTrees: undefined, provisionSpawnSync: scripted({}), redactDiagnostic: (text) => text,
    }));
    try {
      assert.equal(spawnCalls.length, 1);
      assert.ok(provisionCalls.some((call) => call.step === "statTree"));
      assert.equal(provisionCalls.filter((call) => call.step === "statShare").length, 3);
      assert.equal(provisionCalls.filter((call) => call.step === "seed").length, 1);
      assert.equal(provisionCalls.filter((call) => call.step === "statWrite").length, 1);
    } finally {
      await handle.dispose(1000);
    }
  });

  const SECRET = "tok-" + "abcdefgh12345678";
  function noFragment(text: string, secret: string): void {
    for (let i = 0; i + 8 <= secret.length; i++) assert.ok(!text.includes(secret.slice(i, i + 8)), `leaked fragment ${secret.slice(i, i + 8)}`);
  }

  it("trusted refusal: the seed step names the stderr when a redactor is supplied", async () => {
    const msg = await failure({ seed: { status: 1, stderr: "seed boom: EACCES" } }, { redactDiagnostic: makeTextRedactor([]) });
    assert.match(msg, /^runner-owned session seed failed \(exit 1\): seed boom: EACCES$/);
    assert.ok(provisionCalls.find((c) => c.step === "seed")?.args.includes(SESSION_SEED_ENTRYPOINT));
  });

  it("trusted refusal: spawns the seed with stderr piped, stdin/stdout ignored, and HOME/TMPDIR under the root", async () => {
    await failure({ seed: { status: 1, stderr: "x" } }, { redactDiagnostic: (t) => t });
    const opts = provisionCalls.find((c) => c.step === "seed")?.options;
    assert.deepEqual(opts?.stdio, ["ignore", "ignore", "pipe"]);
    assert.equal(opts?.env?.HOME, `${DATA_ROOT}/home`);
    assert.equal(opts?.env?.TMPDIR, `${DATA_ROOT}/tmp`);
  });

  it("trusted refusal: keeps only a bounded tail of oversized stderr", async () => {
    const big = "HEAD-MARK" + "x".repeat(10_000) + "TAIL-MARK";
    const msg = await failure({ seed: { status: 1, stderr: big } }, { redactDiagnostic: (t) => t });
    assert.ok(msg.includes("…[truncated]"));
    assert.ok(msg.endsWith("TAIL-MARK"));
    assert.ok(!msg.includes("HEAD-MARK"));
    assert.ok(msg.length < 4096 + 200, `message length ${msg.length}`);
  });

  it("trusted refusal: reports the signal, and a spawn error code but never its message", async () => {
    const killed = await failure({ seed: { status: null, signal: "SIGKILL", stderr: "" } }, { redactDiagnostic: (t) => t });
    assert.match(killed, /\(exit null, signal SIGKILL\)/);
    const err = Object.assign(new Error("argv-echo UNIQUE-ERR-MSG"), { code: "ENOENT" });
    const spawnErr = await failure({ tree: { status: null, error: err } }, { redactDiagnostic: (t) => t });
    assert.match(spawnErr, /spawn error ENOENT/);
    assert.ok(!spawnErr.includes("UNIQUE-ERR-MSG"));
  });

  it("trusted refusal: redacts a secret in stderr", async () => {
    const msg = await failure({ seed: { status: 1, stderr: `cannot read ${SECRET} now` } }, { redactDiagnostic: makeTextRedactor([SECRET]) });
    assert.ok(msg.includes("***REDACTED***"), msg);
    noFragment(msg, SECRET);
  });

  it("trusted refusal: redacts before truncating, so a secret straddling the tail boundary leaves no fragment", async () => {
    // Cutting the RAW stderr to its last 4096 chars would land 12 chars into SECRET and keep
    // its final 8 chars; redacting the whole text first must leave nothing of it.
    const stderr = "z".repeat(500) + SECRET + "y".repeat(4096 - 12);
    assert.equal(stderr.length - 4096, 500 + SECRET.length - 12, "fixture places the raw cut inside the secret");
    const msg = await failure({ seed: { status: 1, stderr } }, { redactDiagnostic: makeTextRedactor([SECRET]) });
    assert.ok(msg.includes("…[truncated]"));
    noFragment(msg, SECRET);
  });

  it("trusted refusal: withholds stderr entirely without a redactor, on every step", async () => {
    const marker = "UNIQUE-STDERR-MARK";
    const tree = await failure({ tree: { status: 3, stderr: marker } }, {});
    assert.match(tree, /^runner-owned tree creation failed \(exit 3\): stderr withheld \(no redactor\)$/);
    const seed = await failure({ seed: { status: null, signal: "SIGTERM", stderr: marker } }, {});
    assert.match(seed, /^runner-owned session seed failed \(exit null, signal SIGTERM\): stderr withheld \(no redactor\)$/);
    const share = await failure({ share: { status: 2, stderr: marker } }, {});
    assert.ok(share.endsWith("stderr withheld (no redactor)") && !share.includes(marker));
    for (const m of [tree, seed]) assert.ok(!m.includes(marker));
  });

  it("trusted refusal: removes the tree and never spawns the supervisor after a seed failure", async () => {
    await failure({ seed: { status: 1, stderr: "x" } }, {});
    const rm = provisionCalls.find((c) => c.step === "rm");
    assert.ok(rm, "cleanup rm -rf was issued through the injected spawn");
    assert.deepEqual(rm.args.slice(-3), ["-rf", "--", DATA_ROOT]);
    assert.equal(spawnCalls.length, 0);
  });

  it("trusted refusal: carries the bounded tail for tree creation and file write failures with a redactor", async () => {
    const redactDiagnostic = makeTextRedactor([SECRET]);
    const tree = await failure({ tree: { status: 1, stderr: `mkdir: ${SECRET} exists` } }, { redactDiagnostic });
    assert.match(tree, /^runner-owned tree creation failed \(exit 1\): mkdir: \*\*\*REDACTED\*\*\* exists$/);
    const write = await failure({ write: { status: 1, stderr: "cat: denied" } }, { redactDiagnostic });
    assert.match(write, /^runner-owned file write failed for \S+ \(exit 1\): cat: denied$/);
  });
});
