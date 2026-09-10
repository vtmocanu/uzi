import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createInterface } from "node:readline";
import { PassThrough, Writable } from "node:stream";

import { CODEX_SESSION_GID, COMMAND_UID, WORKER_UID, setprivArgsForUid, setprivRunnerArgs } from "../src/runner-uid.js";
import {
  CodexUnsupportedProfileError,
  launchCodexRoot,
  launchCodexEffectRoot,
  type CodexLaunchSpec,
  type LauncherDeps,
  type RunnerTreeRequest,
  type SupervisorProcess,
} from "../src/codex/launcher.js";
import { runLaunchCli, type LaunchCliDeps } from "../src/codex/launch-cli.js";

// PRD #1156 (M3a) — the isolated per-root launcher. NO real Go binary, NO network,
// NO setpriv/root: the supervisor is a FAKE process, every privileged/uid-resolving
// step is injected.

const CODEX_BIN = "/opt/uzi-codex/0.153.2/bin/codex";
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
  disposeAuthority?: string;
  exitCode?: number;
  disposeNearBudget?: boolean;
  exitDelayMs?: number;
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
  private readonly disposeAuthority: string;
  private readonly exitCode: number;
  private readonly disposeNearBudget: boolean;
  private readonly exitDelayMs: number;
  readonly disposeTimeouts: number[] = [];

  constructor(opts: FakeOpts = {}) {
    super();
    this.stdio = [this.stdin, this.stdout, this.stderr, this.control, this.evidence];
    this.disposeState = opts.disposeState ?? "drained";
    this.disposeAuthority = opts.disposeAuthority ?? "ECHILD+__WALL";
    this.exitCode = opts.exitCode ?? 0;
    this.disposeNearBudget = opts.disposeNearBudget ?? false;
    this.exitDelayMs = opts.exitDelayMs ?? 0;
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
      if (this.disposeState === "drained") {
        const emit = (): void => {
          this.writeEvidence({ event: "dispose", id: cmd.id, state: "drained", authority: this.disposeAuthority, killed: [], reaped: [this.pid + 1] });
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
      ].sort(),
    );
    assert.equal(env.CODEX_PROVIDER_KEY, "dummy-key");
    assert.equal(env.HOME, `${DATA_ROOT}/home`);
    assert.equal(env.CODEX_HOME, `${DATA_ROOT}/codex`);
    assert.equal(env.TMPDIR, `${DATA_ROOT}/tmp`);
    assert.equal(env.SHELL, "/bin/sh");
    assert.equal(env.LANG, "C");
    assert.equal(env.TERM, "dumb");
    // Fixed PATH: system dirs + toolchain, NOT the codex bundle.
    assert.match(String(env.PATH), /\/opt\/uzi-toolchain\/bin/);
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
    assert.deepEqual(call.options.env, env);
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

  it("rejects allowlist-reserved provider env keys", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ provider: { ...baseSpec().provider, envKey: "PATH" } }), baseDeps(fake)),
      /envKey is invalid or reserved/,
    );
  });

  it("rejects an owned data root that overlaps the target repository", async () => {
    const fake = newFake();
    await assert.rejects(
      launchCodexRoot(baseSpec({ ownedDataRoot: "/work/repo/.codex-state" }), baseDeps(fake)),
      /ownedDataRoot and cwd must be disjoint/,
    );
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

  it("reserves caller budget for exit confirmation after a near-deadline drain", async () => {
    const fake = newFake({ disposeNearBudget: true, exitDelayMs: 10 });
    const handle = await launchCodexRoot(
      baseSpec(),
      baseDeps(fake, { deadlines: { started: 1000, snapshot: 1000, dispose: 1000, exit: 30 } }),
    );

    const outcome = await handle.dispose(100);
    assert.equal(outcome.clean, true);
    assert.ok((fake.disposeTimeouts[0] ?? 100) <= 80, "drain received no more than four fifths of the total budget");
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
