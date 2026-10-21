// Real RunRunner + WorkerClient + CodexExecutor, launched by the isolated Go HTTP proof.
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { PassThrough } from "node:stream";
import { WorkerClient } from "../../src/client.js";
import { GitCache, gitEnv } from "../../src/git.js";
import { RunRunner } from "../../src/runner.js";
import { ActiveRunRegistry } from "../../src/active-run-registry.js";
import { CodexExecutor, type CodexExecutorDeps } from "../../src/codex/codex-executor.js";
import { CodexSessionStore } from "../../src/codex/session-state.js";
import { selectCodexBinding } from "../../src/codex/select.js";
import type { CodexRootHandle } from "../../src/codex/launcher.js";
import type { CodexExecutionSafetyImpl } from "../../src/codex/safety.js";
import { ExecutionRegistry } from "../../src/codex/registry.js";
import { ENV_PROBE_SCRIPT } from "../../src/env-probe.js";
import { setQuiescenceViewForTests } from "../../src/run-quiescence.js";
import { scopedRealView } from "../fake-proc.js";
import { defaultGitleaksShim } from "../gitleaks-shim.js";
import type { Logger } from "../../src/log.js";
import { FakeTransport } from "./codex-retained-stop-transport.js";

const cfg = JSON.parse(process.env.UZI_RETAINED_STOP_FIXTURE ?? "") as {
  url: string; token: string; run: string; origin: string; scratch: string;
};
assert.ok(path.isAbsolute(cfg.scratch));
process.umask(0o077);
setQuiescenceViewForTests(scopedRealView());
const local = fs.mkdtempSync(path.join(cfg.scratch, "worker-"));
const lines: string[] = [];
const log: Logger = {
  debug: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  info: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  warn: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  error: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  addSecret() {}, removeSecret() {}, child: () => log,
};
function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: gitEnv(), encoding: "utf8", stdio: "pipe" }).trim();
}
const ident = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false"];
const seed = path.join(local, "seed");
execFileSync("git", ["init", "-b", "main", seed], { env: gitEnv(), stdio: "pipe" });
fs.writeFileSync(path.join(seed, "README.md"), "# retained stop\n");
git(seed, ["add", "README.md"]);
git(seed, [...ident, "commit", "-m", "initial"]);
execFileSync("git", ["clone", "--bare", seed, cfg.origin], { env: gitEnv(), stdio: "pipe" });
const forgeConfig = path.join(local, "forge-config");
fs.writeFileSync(forgeConfig, `[url "${cfg.origin}"]\n\tinsteadOf = https://forge.example/fixture.git\n`);
process.env.GIT_CONFIG_GLOBAL = forgeConfig;
const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, {
  terminalRetrySchedule: [5, 5], codexHTTPTimeoutMs: 2000,
});
type Snapshot = {
  status: string; session: string | null; claim_generation: number; worker_id: string;
  holds: number[]; cap_revoked: boolean; fail_free: boolean; requeues: number;
  released: boolean; snapshot_epoch: number; nonce: string;
};
async function control(action: string): Promise<Snapshot> {
  const response = await fetch(cfg.url + "/fixture/" + action, {
    method: "POST", headers: { Authorization: "Bearer " + cfg.token }, signal: AbortSignal.timeout(10_000),
  });
  assert.equal(response.ok, true, "fixture control: " + await response.clone().text());
  return await response.json() as Snapshot;
}
function barrier() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
const entered = barrier(), recoveredTurn = barrier(), releaseRecovered = barrier();
let factories = 0, launches = 0, reaps = 0, disposals = 0, readiness = 0;
let predecessor = "", successor = "", committed = "";
const session = randomUUID();
const homes: string[] = [];
const executors: CodexExecutor[] = [];
const registries: ExecutionRegistry[] = [];
const registerRoot = ExecutionRegistry.prototype.registerRoot;
ExecutionRegistry.prototype.registerRoot = function (...args) {
  if (args[1].kind === "provider") registries.push(this);
  return registerRoot.apply(this, args);
};
let expireCapture: (() => void) | undefined;
let expired = false, paused = false;
const transports: FakeTransport[] = [];
const home = path.join(local, "home");
fs.mkdirSync(home);
function rootHandle(): CodexRootHandle {
  return {
    started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
      nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
    supervisorPid: 200,
    transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
    waitChild: async () => ({ event: "child_exit", code: 0 }),
    dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
    failed: undefined, whenFailed: new Promise<Error>(() => {}),
  };
}
const cache = new GitCache(path.join(local, "data"), log, undefined, {
  gitleaksBin: defaultGitleaksShim(),
  retainedReadinessLaunchForTest: async spec => {
    readiness++;
    const request = JSON.parse(spec.args[2]!) as { roots: string[]; identities: string[]; deadline: number };
    const bare = cache.barePathFor("https://forge.example/fixture.git");
    assert.deepEqual(request.roots, [predecessor, path.join(predecessor, ".git"),
      path.join(predecessor, ".git/objects"), bare, path.join(bare, "objects")]);
    assert.deepEqual(request.identities, request.roots.map(p => {
      const st = fs.lstatSync(p, { bigint: true });
      assert.ok(st.isDirectory() && !st.isSymbolicLink());
      return String(st.dev) + ":" + String(st.ino);
    }));
    assert.ok(request.deadline > Date.now());
    const objects = request.roots[2]!;
    const objectsStat = fs.lstatSync(objects);
    if (objectsStat.uid === 10003) fs.chmodSync(objects, (objectsStat.mode & 0o7777) | 0o050);
    // Mirror the readiness helper\'s group-read/traverse repair on the validated
    // object directory. Production checks all root identities after the join.
    const handle = rootHandle();
    let joined = false;
    handle.waitChild = async () => {
      (handle.transport.stdout! as PassThrough).end("ok 1 0\n");
      (handle.transport.stderr! as PassThrough).end();
      joined = true;
      return { event: "child_exit", code: 0 };
    };
    handle.dispose = async () => {
      assert.equal(joined, true);
      return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
    };
    return handle;
  },
});
const active = new ActiveRunRegistry();
const runner = new RunRunner(client, cache, (_run, codex) => {
  factories++;
  const factory = factories;
  const selected = selectCodexBinding({ codex });
  assert.equal(selected.kind, "codex");
  if (selected.kind !== "codex") throw new Error("missing binding");
  const deps: CodexExecutorDeps = {
    installDeps: async () => ({ results: [], truncated: false }),
    rootStartTime: pid => pid * 10,
    idleMs: 30_000, wallMs: 60_000, boundaryDeadlineMs: 30_000, childTurnDeadlineMs: 30_000,
    commandTmpdir: path.join(local, "tmp"),
    spawnCommand: async argv => {
      assert.equal(argv[2], ENV_PROBE_SCRIPT, "only the environment probe is scripted");
      return { code: 0, stdout: JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok" }) + "\n", stderr: "" };
    },
    launchProviderRoot: async spec => {
      launches++;
      const ownedHome = path.join(spec.ownedDataRoot, "codex");
      homes.push(ownedHome);
      fs.mkdirSync(path.join(ownedHome, "sessions"), { recursive: true });
      fs.writeFileSync(path.join(ownedHome, "sessions", "rollout-" + session + ".jsonl"),
        JSON.stringify({ type: "session_meta", payload: { id: session, cwd: spec.cwd } }) + "\n");
      if (launches === 1) {
        predecessor = spec.cwd;
        fs.writeFileSync(path.join(predecessor, "COMMITTED.txt"), "committed source\n");
        git(predecessor, ["add", "COMMITTED.txt"]);
        git(predecessor, [...ident, "commit", "-m", "source work"]);
        committed = git(predecessor, ["rev-parse", "HEAD"]);
        fs.writeFileSync(path.join(predecessor, "DIRTY.txt"), "dirty source\n");
      } else {
        assert.equal(factory, 3, "recovery capture executor must be replaced before model execution");
        successor = spec.cwd;
        assert.notEqual(successor, predecessor);
        assert.match(path.basename(successor), /\.attempt-/);
        assert.equal(fs.readFileSync(path.join(successor, "COMMITTED.txt"), "utf8"), "committed source\n");
        assert.equal(fs.readFileSync(path.join(successor, "DIRTY.txt"), "utf8"), "dirty source\n");
        git(successor, ["merge-base", "--is-ancestor", committed, "HEAD"]);
      }
      const transport = new FakeTransport(c => {
        if (c.method === "thread/start" || c.method === "thread/resume") return { thread: { id: session } };
        if (c.method === "turn/start") {
          c.transport.push({ kind: "thread_started", method: "thread/started", threadId: session, params: { thread: { id: session } } });
          if (launches === 1) entered.resolve();
          else {
            recoveredTurn.resolve();
            void releaseRecovered.promise.then(() => c.transport.push({
              kind: "turn_completed", method: "turn/completed", threadId: session, turnId: "turn-recovered",
              status: "interrupted", params: { threadId: session, turn: { id: "turn-recovered", status: "interrupted" } },
            }));
          }
          return { turn: { id: launches === 1 ? "turn-first" : "turn-recovered" } };
        }
        return {};
      });
      transports.push(transport);
      return {
        transport, supervisorPid: 1000 + launches,
        root: {
          kind: "provider",
          reap: async () => { reaps++; fs.rmSync(spec.ownedDataRoot, { recursive: true, force: true }); return { ok: true }; },
          dispose: async () => { disposals++; fs.rmSync(spec.ownedDataRoot, { recursive: true, force: true }); },
        },
      };
    },
    wireFileop: () => ({ client: { op: async () => ({ ok: true }) }, dispose: async () => {} }),
    sessionStore: CodexSessionStore,
    launchEffectRoot: async spec => {
      const handle = rootHandle();
      const argv = spec.command.endsWith("/uzi-codex-command-sandbox")
        ? spec.args.slice(spec.args.indexOf("--") + 1) : [spec.command, ...spec.args];
      assert.ok(argv[0]);
      if (argv[0] === "/usr/local/bin/uzi-codex-fileop") return handle;
      assert.ok(argv[0] === "/usr/bin/git" || argv[0] === defaultGitleaksShim() ||
        argv[0] === "/usr/local/bin/gitleaks", "only fixed Git/scanner fixture children: " + JSON.stringify(argv));
      const child = spawn(argv[0] === "/usr/local/bin/gitleaks" ? defaultGitleaksShim() : argv[0], argv.slice(1),
        { cwd: spec.cwd, env: spec.env, stdio: "pipe" });
      const joined = new Promise<{ event: "child_exit"; code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("close", code => resolve({ event: "child_exit", code: code ?? 1 }));
      });
      const transport = { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr };
      handle.waitChild = async () => {
        const result = await joined;
        if (paused && !expired && argv.includes("pack-objects")) {
          assert.ok(expireCapture, "actual capture armed its deadline");
          expired = true;
          expireCapture();
        }
        return result;
      };
      handle.dispose = async () => {
        if (child.exitCode === null) child.kill("SIGKILL");
        await joined;
        return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
      };
      return { ...handle, transport };
    },
  };
  const executor = new CodexExecutor(log, home, {
    binding: selected.binding, client,
    provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
  }, deps);
  executors.push(executor);
  return { executor, homeDir: home };
}, log, 20, cfg.token, {
  activeRuns: active, pollMs: 5, checkpointTickIntervalMs: 0, recoveryRetryMs: 10,
  checkpointTestHooks: { retryBackoffMs: [0, 0] },
  codexBoundaryDeadlineMs: 30_000,
});
const park = runner["handlePausePark"].bind(runner);
let safety: CodexExecutionSafetyImpl | undefined;
runner["handlePausePark"] = async (...args) => {
  paused = true;
  safety = args[1].executor.safety as CodexExecutionSafetyImpl;
  safety["seams"].armDeadline = (_request, _ms, fire) => {
    expireCapture = fire;
    return () => {};
  };
  const result = await park(...args);
  assert.equal(result, false, "expired capture has no paused ACK");
  assert.equal(args[1].refusedPauseCaptureSafety, safety, "exact safety identity needed by dispatchFailure");
  return result;
};
let execution: Promise<void> | undefined;
try {
  const registration = await client.register("retained-stop", undefined, 1, [], [
    "codex_harness_v1", "codex_runtime_v2", "codex_completion_interlock_v1", "completion_interlock_v1",
    "recovery_archive_v1", "recovery_archive_v2", "credential_switch_v1", "codex_refresh_recovery_v1",
    "active_run_snapshot",
  ], active.buildRegisterSnapshot());
  assert.ok(client.hasFeature("active_run_snapshot"));
  const first = await client.claimRun(active.build());
  assert.ok(first);
  assert.equal(first.run_id, cfg.run);
  execution = runner.execute(first);
  await Promise.race([entered.promise, execution.then(() => { throw Error("initial execution ended before model turn: " + lines.join("\n")); })]);
  assert.equal(active.has(cfg.run), true);
  await client.heartbeat(undefined, undefined, active.build());
  await control("pause");
  await execution;
  assert.equal(expired, true);
  assert.ok(lines.some(l => l.includes("retained recovery stopped; source and accounting kept")),
    "real epoch guard must reach RetainedRecoveryStop");
  assert.equal(safety!.completedCaptureForTeardown(), undefined);
  assert.equal(launches, 1, "failed execution admits no replacement provider epoch");
  assert.equal(registries.length, 1);
  assert.equal(active.has(cfg.run), false, "finally removes the actual registry entry");
  assert.ok(reaps > 0 && disposals > 0);
  const stopped = await control("inspect");
  assert.equal(stopped.status, "running");
  assert.equal(stopped.fail_free, true);
  assert.equal(stopped.session, session);
  assert.equal(stopped.claim_generation, first.claim_generation);
  assert.equal(stopped.worker_id, registration.worker_id);
  assert.deepEqual(stopped.holds, [first.claim_generation]);
  assert.equal(fs.readFileSync(path.join(predecessor, "COMMITTED.txt"), "utf8"), "committed source\n");
  assert.equal(fs.readFileSync(path.join(predecessor, "DIRTY.txt"), "utf8"), "dirty source\n");
  const beforeHead = git(predecessor, ["rev-parse", "HEAD"]);
  const bare = cache.barePathFor(first.repo.clone_url);
  const beforeConfig = fs.readFileSync(path.join(bare, "config"), "utf8");
  assert.ok(beforeConfig.includes(cfg.run) && beforeConfig.includes(predecessor), "source journal names exact clone and run");
  const sessionStore = path.join(home, "codex-session-store");
  const generation = fs.readFileSync(path.join(sessionStore, "current"), "utf8");
  const rollout = path.join(sessionStore, "generations", generation.trim(), "sessions", "rollout-" + session + ".jsonl");
  const saved = fs.readFileSync(rollout, "utf8");
  assert.equal(await CodexSessionStore.inspectSession(sessionStore, session), "present");
  await client.heartbeat(undefined, undefined, active.build());
  assert.equal((await control("inspect")).status, "running", "missing snapshot before cutoff cannot requeue");
  await control("fence");
  await client.heartbeat(undefined, undefined, active.build());
  const queued = await control("inspect");
  assert.equal(queued.status, "queued");
  assert.equal(queued.requeues, stopped.requeues + 1);
  assert.equal(queued.cap_revoked, true);
  assert.equal(queued.fail_free, true);
  assert.equal(queued.released, false, "snapshot requeue is a logical release");
  assert.equal(queued.claim_generation, first.claim_generation);
  assert.equal(queued.worker_id, stopped.worker_id);
  assert.deepEqual(queued.holds, stopped.holds);
  assert.ok(queued.snapshot_epoch > stopped.snapshot_epoch);
  assert.equal(queued.nonce, stopped.nonce);
  assert.equal(git(predecessor, ["rev-parse", "HEAD"]), beforeHead);
  assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), beforeConfig);
  assert.equal(fs.readFileSync(path.join(sessionStore, "current"), "utf8"), generation);
  assert.equal(fs.readFileSync(rollout, "utf8"), saved);
  const reclaimed = await client.claimRun(active.build());
  assert.ok(reclaimed);
  assert.equal(reclaimed.run_id, first.run_id);
  assert.equal(reclaimed.claim_generation, first.claim_generation! + 1);
  assert.notEqual(reclaimed.secrets.codex?.capability, first.secrets.codex?.capability);
  execution = runner.execute(reclaimed);
  await Promise.race([recoveredTurn.promise, execution.then(() => { throw Error("recovery ended before fresh model: " + lines.join("\n")); })]);
  assert.equal(factories, 3, "first executor, independent recovery executor, fresh model executor");
  assert.equal(readiness, 1);
  assert.equal(launches, 2);
  assert.notEqual(executors[0], executors[1]);
  assert.notEqual(executors[1], executors[2]);
  assert.notEqual(registries[0], registries[1]);
  assert.notEqual(homes[0], homes[1]);
  assert.equal(registries[0]!.state(), "disposed");
  assert.equal(transports[0]!.turnStartCount, 1);
  assert.equal(transports[1]!.turnStartCount, 1);
  assert.ok(lines.some(l => l.includes("retained work verified on a fresh local attempt")) ||
    git(successor, ["merge-base", "--is-ancestor", committed, "HEAD"]) === "");
  console.log("M1 retained stop -> nonce heartbeat in-budget requeue -> reclaim -> independent recovery -> fresh Codex model turn");
} finally {
  runner.shutdown();
  releaseRecovered.resolve();
  await execution?.catch(() => {});
  ExecutionRegistry.prototype.registerRoot = registerRoot;
}
