import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import {
  type CredentialFreeSettleOutcome,
  type Executor,
  type RunContext,
  type ExecutorResult,
} from "../src/executor.js";
import { LimitReachedError } from "../src/limit.js";
import { PauseNowSignal } from "../src/steering.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import {
  createCodexExecutionSafety,
  type ArmBoundaryDeadline,
  type ReconcileBeforeBoundary,
} from "../src/codex/safety.js";
import { buildRunLaneReconcile, CodexCredentialDeferredError } from "../src/codex/codex-executor.js";
import {
  ExecutionRegistry,
  newLocalExecutionEpoch,
  type RegisteredRoot,
} from "../src/codex/registry.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import type { BoundaryRequest, CodexExecutionSafety } from "../src/harness.js";
import { resolveBoundaryExecutable } from "../src/git.js";
import { GitLabClient } from "../src/forge.js";
import { RequestError } from "../src/client.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { FakeRecoveryClient, FakeRecoveryGit, makeRecoveryCoordinator } from "./codex-reap-fixture.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

describe("Codex durability executable resolution", () => {
  it("pins Git and gitleaks to absolute image paths and rejects other relative names", () => {
    assert.equal(resolveBoundaryExecutable("git"), "/usr/bin/git");
    assert.equal(resolveBoundaryExecutable("gitleaks"), "/usr/local/bin/gitleaks");
    assert.throws(() => resolveBoundaryExecutable("relative-tool"), /trusted absolute path/);
  });
});

// PRD #1171 m4 (Phase 2B) — every durability/publication sink in the runner routes through the
// optional Codex `withBoundary` reap facade, per-sink auth-mode reconcile, the pause-sink
// designation, and the F1 dispose relocation. Driven with a FAKE Codex executor whose `.safety`
// is a REAL {@link createCodexExecutionSafety} (over a real registry + a tracked provider root +
// the real {@link buildRunLaneReconcile} run-lane reconcile) wrapped to RECORD every withBoundary/
// dispose call. The runner stays harness-agnostic — it only ever sees `executor.safety`.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): void {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => {});
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint: drain the pack and return a LANDED publish, so a park/pause
 *  checkpoint confirmably lands (its boundary/permit routing is what these tests assert). */
function spyPublishLands(): () => void {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return () => {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
  };
}

const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};
const API_KEY = {
  auth_mode: "api_key",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
};

function bindingOf(block: Record<string, unknown>): CodexBinding {
  const sel = selectCodexBinding({ codex: block });
  if (sel.kind !== "codex") throw new Error("expected a codex selection");
  return sel.binding;
}

/** A registry-ownable provider root whose reap/dispose are IDEMPOTENT and counted (a boundary in
 *  a retry loop re-quiesces a closed registry and re-reaps each root — see the recovery sink). */
function trackedRoot(): { root: RegisteredRoot; reaps: () => number; disposes: () => number } {
  let reaps = 0;
  let disposes = 0;
  const root: RegisteredRoot = {
    kind: "provider",
    reap: async () => {
      reaps += 1;
      return { ok: true };
    },
    dispose: async () => {
      disposes += 1;
    },
  };
  return { root, reaps: () => reaps, disposes: () => disposes };
}

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  if (reserved.kind === "reserved") reg.registerRoot(reserved.reservation, root);
}

interface CodexRig {
  registry: ExecutionRegistry;
  safety: CodexExecutionSafety;
  /** Every withBoundary(req.boundary) recorded, in order. */
  boundaries: string[];
  /** Every dispose(req.boundary) recorded (F1: the runner's terminal dispose). */
  disposeBoundaries: string[];
  refreshCalls: () => number;
  releaseCalls: () => number;
  registeredTokens: string[];
  reaps: () => number;
  disposes: () => number;
  processSpawns: () => number;
  processArgv: readonly (readonly string[])[];
  /** Issue #1766: the real facade's credential-free capture settle (for a vault-lock park). */
  settle: (deadlineMs: number) => Promise<CredentialFreeSettleOutcome>;
}

const RELEASE_TOK = "codex-release-tok-XXXXXXXX";
const REFRESH_TOK = "codex-refresh-tok-XXXXXXXX";

/** Issue #1766: raw server body text; it must never reach a state body, feed line or log line. */
const VAULT_BODY_SENTINEL = "vault-body-sentinel-XXXXXXXX";

/** Issue #1766: the api's real reply to a Codex refresh/release that passed authorization but hit
 *  a locked owner vault: HTTP 409 with a typed `reason`. */
function vaultLockedError(op: "refresh" | "release"): RequestError {
  return new RequestError(
    "POST",
    `/api/worker/runs/run-codex-sink/codex/${op}`,
    409,
    JSON.stringify({ error: VAULT_BODY_SENTINEL, reason: "vault_locked" }),
  );
}

/** Issue #1513: an event-gated boundary deadline. For the named boundary the deadline fires
 *  only when the test calls `fire()`, so a slow (loaded) boundary can never expire before
 *  the step under test is reached; every other boundary keeps a real unref'd timer. */
function manualDeadline(boundary: BoundaryRequest["boundary"]): {
  armDeadline: ArmBoundaryDeadline;
  armed: () => number;
  fire: () => void;
  fired: () => number;
} {
  let armed = 0;
  let fired = 0;
  let pending: (() => void) | undefined;
  const armDeadline: ArmBoundaryDeadline = (request, ms, fireDeadline) => {
    if (request.boundary !== boundary) {
      const timer = setTimeout(fireDeadline, ms);
      timer.unref?.();
      return () => clearTimeout(timer);
    }
    armed += 1;
    pending = fireDeadline;
    return () => {
      pending = undefined;
    };
  };
  return {
    armDeadline,
    armed: () => armed,
    fire: () => {
      if (!pending) throw new Error(`no armed ${boundary} deadline to fire`);
      fired += 1;
      pending();
    },
    fired: () => fired,
  };
}

const delay = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function codexRig(
  opts: {
    authMode?: "subscription" | "api_key";
    blockReconcile?: boolean;
    /** Forwarded to createCodexExecutionSafety (issue #1513): event-gates a boundary deadline. */
    armDeadline?: ArmBoundaryDeadline;
    /** Delay before each permit-held process spawn: reproduces suite load deterministically. */
    processSpawnDelayMs?: number;
    /** Issue #1766: while this answers true, refreshCodex/releaseCodex throw the api's real typed
     *  409 `vault_locked` RequestError (a locked owner vault after authorization). */
    vaultLocked?: () => boolean;
  } = {},
): CodexRig {
  const authMode = opts.authMode ?? "subscription";
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const tr = trackedRoot();
  registerRoot(registry, tr.root);
  const binding = bindingOf(authMode === "subscription" ? SUBSCRIPTION : API_KEY);

  let refreshCalls = 0;
  let releaseCalls = 0;
  const fakeClient = {
    releaseCodex: async (_runId: string, _req: { capability: string }) => {
      releaseCalls += 1;
      if (opts.vaultLocked?.()) throw vaultLockedError("release");
      if (opts.blockReconcile) throw new Error("release contended");
      return { access_token: RELEASE_TOK };
    },
    refreshCodex: async (
      _runId: string,
      _req: { capability: string; operation_id: string; observed_generation: number },
    ) => {
      refreshCalls += 1;
      if (opts.vaultLocked?.()) throw vaultLockedError("refresh");
      if (opts.blockReconcile) throw new Error("refresh contended");
      return { access_token: REFRESH_TOK, generation: 7, outcome: "advanced" };
    },
  };
  const registeredTokens: string[] = [];
  let processSpawns = 0;
  const processArgv: (readonly string[])[] = [];
  const reconcile: ReconcileBeforeBoundary = buildRunLaneReconcile(
    "run-codex-sink",
    fakeClient as never,
    binding,
    (t) => registeredTokens.push(t),
  );

  const inner = createCodexExecutionSafety(
    registry,
    async () => {
      throw new Error("boundary-action spawn is not used by these sink tests");
    },
    reconcile,
    undefined,
    async (request) => {
      processSpawns += 1;
      processArgv.push(request.argv);
      const [command, ...args] = request.argv;
      if (!command) throw new Error("empty test process argv");
      if (opts.processSpawnDelayMs) await delay(opts.processSpawnDelayMs);
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => { await terminal; return { ok: true }; },
          dispose: async () => { if (child.exitCode === null) child.kill("SIGKILL"); await terminal.catch(() => undefined); },
        },
        stdin: child.stdin,
        stdout: child.stdout,
        stderr: child.stderr,
        waitChild: async () => terminal,
      };
    },
    opts.armDeadline,
  );
  const boundaries: string[] = [];
  const disposeBoundaries: string[] = [];
  const safety: CodexExecutionSafety = {
    kind: "codex",
    withBoundary: (req, action) => {
      boundaries.push(req.boundary);
      return inner.withBoundary(req, action);
    },
    spawnBoundaryProcess: (permit, request) => inner.spawnBoundaryProcess(permit, request),
    dispose: (req) => {
      disposeBoundaries.push(req.boundary);
      return inner.dispose(req);
    },
  };
  return {
    registry,
    safety,
    boundaries,
    disposeBoundaries,
    refreshCalls: () => refreshCalls,
    releaseCalls: () => releaseCalls,
    registeredTokens,
    reaps: tr.reaps,
    disposes: tr.disposes,
    processSpawns: () => processSpawns,
    processArgv,
    settle: (deadlineMs) => inner.settleForCredentialFreeCapture(deadlineMs),
  };
}

/** A fake Codex executor: carries a `.safety`, NO killAgentTree, and runs `behavior`. Issue
 *  #1766: an optional `settle` is exposed as the harness-agnostic credential-free capture settle. */
class FakeCodexExecutor implements Executor {
  safety: CodexExecutionSafety;
  settleForCredentialFreeCapture?: (deadlineMs: number) => Promise<CredentialFreeSettleOutcome>;
  constructor(
    safety: CodexExecutionSafety,
    private readonly behavior: (ctx: RunContext) => Promise<ExecutorResult>,
    settle?: (deadlineMs: number) => Promise<CredentialFreeSettleOutcome>,
  ) {
    this.safety = safety;
    if (settle) this.settleForCredentialFreeCapture = settle;
  }
  run(ctx: RunContext): Promise<ExecutorResult> {
    return this.behavior(ctx);
  }
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

// Issue #1513: per-spawn delays that make the permit-held Git before the upload / forge call
// outlast the OLD wall-clock budgets (500ms shutdown, 2000ms finalize), reproducing the
// suite-load flake deterministically. The event-gated deadline must make that irrelevant.
// The loaded tests' lower bounds (>= 500 / >= 2000) depend on how many permit-held git
// spawns precede the upload / forge call: about 8 on the shutdown path and about 11 on the
// finalize path today. The delays keep headroom against that count shrinking: 5 shutdown
// spawns alone (550ms) and 8 finalize spawns alone (2000ms) already reach the bound. If a
// boundary refactor drops spawns below that, raise the delay rather than the bound.
const SHUTDOWN_LOAD_SPAWN_DELAY_MS = 110;
const FINALIZE_LOAD_SPAWN_DELAY_MS = 250;

/** The finalize deadline contract: a stuck forge request is aborted by the (event-gated)
 *  finalize deadline and settles before withBoundary returns and before terminal dispose. */
async function finalizeDeadlineScenario(
  issue: number,
  processSpawnDelayMs?: number,
): Promise<{ entryToHttpStartMs: number }> {
  const deadline = manualDeadline("finalize");
  const events: string[] = [];
  let httpCalls = 0;
  let httpSettled = false;
  let httpStartedAt = 0;
  let httpSettledAt = 0;
  let finalizeEnteredAt = 0;
  const stuckForge = new GitLabClient({
    httpTimeoutMs: 5_000,
    fetchFn: async (_url, init) => {
      httpCalls += 1;
      httpStartedAt = Date.now();
      events.push("http-start");
      await new Promise<void>((_, reject) => {
        const abort = (): void => {
          httpSettled = true;
          httpSettledAt = Date.now();
          events.push("http-settled");
          reject(new Error("finalize forge request aborted"));
        };
        if (init.signal?.aborted) abort();
        else init.signal?.addEventListener("abort", abort, { once: true });
        events.push("deadline-fired");
        deadline.fire();
      });
      throw new Error("unreachable");
    },
  });
  const rig = codexRig({ armDeadline: deadline.armDeadline, processSpawnDelayMs });
  const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
  rig.safety.withBoundary = async (request, action) => {
    if (request.boundary === "finalize") finalizeEnteredAt = Date.now();
    try {
      return await originalWithBoundary(request, action);
    } finally {
      if (request.boundary === "finalize") events.push("finalize-boundary-returned");
    }
  };
  const originalDispose = rig.safety.dispose.bind(rig.safety);
  let disposeBeforeHttpSettlement = false;
  rig.safety.dispose = async (request) => {
    events.push("dispose");
    if (!httpSettled) disposeBeforeHttpSettlement = true;
    return originalDispose(request);
  };
  const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
    commitInTree(ctx.worktreePath, "STUCK-FORGE.txt", "finalize must cancel\n");
    return { branch: ctx.branch };
  });
  await runnerWith(() => ({ executor: exec }), stuckForge, undefined, undefined, {
    codexBoundaryDeadlineMs: 60_000,
  }).execute(gitlabClaim(issue));
  assert.equal(httpCalls, 1, "deadline cancellation prevented forge retry/backoff");
  assert.equal(httpSettled, true, "the in-flight forge request observed permit cancellation");
  assert.equal(deadline.armed(), 1, "the finalize deadline was armed once");
  assert.equal(deadline.fired(), 1, "the finalize deadline fired once, from the forge call");
  const at = (event: string): number => {
    const i = events.indexOf(event);
    assert.ok(i >= 0, `${event} recorded; events=${JSON.stringify(events)}`);
    return i;
  };
  assert.ok(at("deadline-fired") < at("http-settled"), `the deadline caused the HTTP settlement; events=${JSON.stringify(events)}`);
  assert.ok(at("http-settled") < at("finalize-boundary-returned"), `withBoundary returned only after HTTP settlement; events=${JSON.stringify(events)}`);
  assert.ok(at("http-settled") < at("dispose"), `terminal dispose waited for HTTP settlement; events=${JSON.stringify(events)}`);
  assert.equal(disposeBeforeHttpSettlement, false, "terminal dispose waited for HTTP settlement");
  assert.ok(httpSettledAt - httpStartedAt < 4000, "the independent 5s forge timeout did not control finalize");
  return { entryToHttpStartMs: httpStartedAt - finalizeEnteredAt };
}

/** The shutdown deadline contract: the (event-gated) shutdown deadline cancels the in-flight
 *  checkpoint upload, and terminal dispose waits for the upload to settle. */
async function shutdownDeadlineScenario(
  issue: number,
  processSpawnDelayMs?: number,
): Promise<{ entryToUploadStartMs: number }> {
  const { gitlab } = fakeGitlab();
  const deadline = manualDeadline("shutdown");
  const originalPublish = client.publishCheckpoint.bind(client);
  let uploadSettled = false;
  let uploadCalls = 0;
  let uploadStartedAt = 0;
  let shutdownEnteredAt = 0;
  let uploadSignal: AbortSignal | undefined;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
    signal?: AbortSignal,
  ) => {
    uploadCalls += 1;
    uploadStartedAt = Date.now();
    uploadSignal = signal;
    await drain(pack);
    if (!signal) throw new Error("the shutdown upload must carry the boundary signal");
    // Prove the deadline (not an already-aborted signal) is what aborts the upload.
    assert.equal(signal.aborted, false, "the upload signal is live until the deadline fires");
    deadline.fire();
    await new Promise<void>((resolve) => {
      if (signal.aborted) resolve();
      else signal.addEventListener("abort", () => resolve(), { once: true });
    });
    await delay(30);
    uploadSettled = true;
    throw new Error("test upload aborted");
  };
  try {
    const rig = codexRig({ armDeadline: deadline.armDeadline, processSpawnDelayMs });
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = (request, action) => {
      if (request.boundary === "shutdown") shutdownEnteredAt = Date.now();
      return originalWithBoundary(request, action);
    };
    const originalDispose = rig.safety.dispose.bind(rig.safety);
    let disposeBeforeSettlement = false;
    rig.safety.dispose = async (request) => {
      if (!uploadSettled) disposeBeforeSettlement = true;
      return originalDispose(request);
    };
    let started!: () => void;
    const startedP = new Promise<void>((resolve) => { started = resolve; });
    const runner = runnerWith(() => ({ executor: new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "SHUT-TIMEOUT.txt", "work before timeout\n");
      started();
      await new Promise<void>((_, reject) => ctx.signal!.addEventListener("abort", () => reject(new Error("aborted")), { once: true }));
      return { branch: ctx.branch };
    }) }), gitlab, undefined, undefined, {
      codexBoundaryDeadlineMs: 60_000,
      shutdownPublishTimeoutMs: 60_000,
    });
    const run = runner.execute(gitlabClaim(issue, { wait_on_limit: true }));
    await startedP;
    runner.shutdown();
    await run;
    assert.equal(uploadSettled, true);
    assert.equal(disposeBeforeSettlement, false, "terminal dispose cannot overtake the timed-out durability action");
    assert.deepEqual(rig.disposeBoundaries, ["terminal"]);
    assert.equal(uploadCalls, 1, "the checkpoint upload was attempted exactly once");
    assert.equal(deadline.armed(), 1, "the shutdown deadline was armed once");
    assert.equal(deadline.fired(), 1, "the shutdown deadline fired once, from the upload");
    assert.equal(uploadSignal?.aborted, true, "the upload's signal was aborted by the shutdown deadline");
  } finally {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
  }
  return { entryToUploadStartMs: uploadStartedAt - shutdownEnteredAt };
}

// ================================================================================
describe("RunRunner m4 — Codex durability sinks route through withBoundary", () => {
  it("aborts and settles a stuck finalize forge request before withBoundary/dispose returns", async () => {
    await finalizeDeadlineScenario(1219);
  });

  it("finalize deadline contract holds when permit-held Git is slow (suite load, issue #1513)", async () => {
    const r = await finalizeDeadlineScenario(1223, FINALIZE_LOAD_SPAWN_DELAY_MS);
    assert.ok(
      r.entryToHttpStartMs >= 2_000,
      `the loaded finalize boundary outlasted the old 2000ms wall-clock budget before the forge call (${r.entryToHttpStartMs}ms)`,
    );
  });

  it("(1) phasePublish FINALIZE routes through withBoundary; the trusted push/MR runs inside it (subscription reconcile + reap before publish)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1200);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.ok(rig.boundaries.includes("finalize"), `finalize boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // The trusted action ran INSIDE the boundary: the MR was opened.
    assert.equal(calls.length, 1, "the MR (the finalize action) was opened inside the boundary");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The provider root was reaped as part of the finalize boundary, BEFORE the credentialed push.
    assert.ok(rig.reaps() >= 1, "the provider root was reaped inside the finalize boundary");
    assert.ok(rig.processSpawns() > 0, "permit-held Git launched through registered boundary-action roots");
    assert.ok(rig.processArgv.every((argv) => argv[0]?.startsWith("/")), "every permit-held executable is absolute");
    // The subscription reconcile ran before the boundary (one refresh; zero standalone release).
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile refreshed before the boundary");
    // F1: the runner disposed the registry ONCE after the sinks settled.
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner disposed the registry once, in the finally");
    assert.equal(rig.registry.state(), "disposed", "the registry ends disposed");
    assert.ok(rig.disposes() >= 1, "the provider root was disposed");
  });

  it("reports the committed MR terminal only after the Codex finalize boundary releases", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig();
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    let insideFinalize = false;
    rig.safety.withBoundary = (request, action) =>
      originalWithBoundary(request, async (permit) => {
        if (request.boundary === "finalize") insideFinalize = true;
        try {
          return await action(permit);
        } finally {
          if (request.boundary === "finalize") insideFinalize = false;
        }
      });

    const originalReportState = client.reportState.bind(client);
    let terminalInsideFinalize: boolean | undefined;
    let terminalCarriedBoundarySignal: boolean | undefined;
    client.reportState = async (runId, body, signal) => {
      if (body.status === "completed" && body.mr_iid !== undefined) {
        terminalInsideFinalize = insideFinalize;
        terminalCarriedBoundarySignal = signal !== undefined;
      }
      return originalReportState(runId, body, signal);
    };
    try {
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "TERMINAL.txt", "report after boundary\n");
        return { branch: ctx.branch };
      });
      const claim = gitlabClaim(1221);
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

      assert.equal(calls.length, 1, "the MR was created inside the trusted finalize action");
      assert.equal(terminalInsideFinalize, false, "the committed MR terminal runs after boundary release");
      assert.equal(terminalCarriedBoundarySignal, false, "the terminal retry schedule is not poisoned by the expired boundary signal");
      assert.ok(statuses(claim.run_id).includes("completed"));
    } finally {
      client.reportState = originalReportState;
    }
  });

  it("reports the committed MR instead of failing when the finalize boundary errors after publication", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig();
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      const value = await originalWithBoundary(request, action);
      if (request.boundary === "finalize") {
        const error = new Error("deadline expired after action settlement");
        error.name = "CodexBoundaryError";
        throw error;
      }
      return value;
    };
    const { logger, lines } = recordingLogger();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LATE-BOUNDARY.txt", "published before boundary error\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1222);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    assert.equal(calls.length, 1, "the MR was already created before the boundary error");
    assert.ok(statuses(claim.run_id).includes("completed"), "the committed publish is reported completed");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the committed publish is never misreported failed");
    assert.ok(
      lines.some((line) =>
        (line as { msg?: string }).msg ===
        "Codex finalize boundary failed after committed publish; reporting committed terminal outcome"),
      "the swallowed post-publish boundary error is logged",
    );
  });

  it("(1) checkpoint reap:true routes through withBoundary (boundary=checkpoint)", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1201);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
    assert.ok(rig.boundaries.includes("checkpoint"), `checkpoint boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // finalize still runs afterwards (a second boundary re-quiesces the closed registry).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary also runs");
  });

  it("(1) park routes through withBoundary (boundary=park)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-park-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "PARK.txt", "committed before the limit\n");
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      });
      const claim = gitlabClaim(1202, { wait_on_limit: true });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
      assert.ok(rig.boundaries.includes("park"), `park boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(statuses(claim.run_id).includes("limit_wait"), "the run parked at limit_wait");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the registry was disposed once even on the park path");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(1) graceful shutdown routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      const rig = codexRig();
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const runner = runnerWith(() => ({ executor: new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "SHUT.txt", "committed before shutdown\n");
        started();
        await new Promise<void>((_, reject) => ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }));
        return { branch: ctx.branch };
      }) }), gitlab);
      const p = runner.execute(gitlabClaim(1203, { wait_on_limit: true }));
      await startedP;
      runner.shutdown();
      await p;
      assert.ok(rig.boundaries.includes("shutdown"), `shutdown boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    } finally {
      restore();
    }
  });

  it("shutdown deadline cancels the upload and awaits its settlement before safety.dispose", async () => {
    await shutdownDeadlineScenario(1209);
  });

  it("shutdown deadline contract holds when permit-held Git is slow (suite load, issue #1513)", async () => {
    const r = await shutdownDeadlineScenario(1224, SHUTDOWN_LOAD_SPAWN_DELAY_MS);
    assert.ok(
      r.entryToUploadStartMs >= 500,
      `the loaded shutdown boundary outlasted the old 500ms wall-clock budget before the upload (${r.entryToUploadStartMs}ms)`,
    );
  });

  it("(1) recovery publication routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-rec-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "REC.txt", "recovered work\n");
        throw new TransientRecoveryError();
      });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1204));
      assert.ok(rig.boundaries.includes("shutdown"), `recovery boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "the run parked at recovery_wait",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ================================================================================
describe("RunRunner m4 — per-sink reconcile before the boundary", () => {
  it("(2) refresh-failure BEFORE the action: reconcile blocked → CodexBoundaryError, action never ran, registry poisoned, later publication blocked", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription", blockReconcile: true });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1210);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    // The finalize boundary was attempted, its reconcile blocked, and it threw a
    // CodexBoundaryError("reconcile") — the trusted publish/push never ran (no MR), the run
    // reported failed, and the registry is poisoned (later publication blocked).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary was attempted");
    assert.equal(calls.length, 0, "the trusted push/MR NEVER ran (publication blocked)");
    assert.ok(rig.reaps() === 0, "the provider root was NOT reaped (reconcile blocked before quiesce/reap)");
    assert.ok(statuses(claim.run_id).includes("failed"), "the failed-run report fires for a finalize CodexBoundaryError");
    assert.ok(rig.registry.isPoisoned(), "the registry is poisoned (sink counter zero)");
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile attempted a refresh");
    // F1: the runner STILL disposes the registry in the finally even on the blocked/failed path —
    // the provider root is torn down (disposeTools disposes roots regardless of poison).
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the terminal dispose ran even though the boundary was blocked");
    assert.ok(rig.disposes() >= 1, "the provider root was torn down by the terminal dispose");
  });

  it("(3) api_key reconcile: zero refreshCodex calls, releaseCodex called, the action runs", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "api_key" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1211);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.equal(rig.refreshCalls(), 0, "an api_key run performs ZERO subscription refresh calls");
    assert.ok(rig.releaseCalls() >= 1, "an api_key reconcile freshly releases (re-authorizes)");
    assert.equal(calls.length, 1, "the trusted push/MR ran (reconcile ready)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
  });
});

// ================================================================================
describe("RunRunner m4 — credential-free sinks mint NO permit", () => {
  it("(4) the pause sink mints NO permit — withBoundary is not called for the credential-free pause publish", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-pause-"));
    try {
      const rig = codexRig();
      let boundariesAtPause: string[] = [];
      let parkedResult: boolean | undefined;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        await ctx.checkpoint?.({ reap: false }); // fetch-back (also credential-free, no permit)
        const at = { completedCount: 2, total: 3 };
        parkedResult = await ctx.parkForPause?.(at);
        boundariesAtPause = [...rig.boundaries]; // capture the moment the pause sink returns
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      });
      const claim = gitlabClaim(1220);
      await runnerWith(() => ({ executor: exec, homeDir: path.join(homeRoot, "h") }), gitlab).execute(claim);
      assert.equal(parkedResult, true, "the pause parked (its credential-free publish landed)");
      assert.deepEqual(
        boundariesAtPause,
        [],
        `neither the reap:false checkpoint NOR the pause sink minted a permit; got ${JSON.stringify(boundariesAtPause)}`,
      );
      assert.ok(statuses(claim.run_id).includes("paused"), "the run reported paused");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Issue #1764: a Codex run that PARKED on an owner pause returns pausedAt. phasePublish finalizes
  // nothing for it, and it must run OUTSIDE the finalize boundary: that boundary's per-sink
  // credential reconcile (refreshCodex/releaseCodex) is refused by the server once the run is
  // `paused`, which would fail a durably parked run. The terminal registry dispose still runs.
  it("(T11) a Codex run parked on an owner pause bypasses the finalize boundary: no permit, no reconcile, no push/MR, no terminal report; terminal dispose still runs", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-pause-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      let parkedResult: boolean | undefined;
      const runHome = path.join(homeRoot, "h");
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        // The per-run HOME holds the resumable session; a parked run must keep it.
        fs.mkdirSync(runHome, { recursive: true });
        fs.writeFileSync(path.join(runHome, "session.marker"), "resume me\n");
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        const at = { completedCount: 1, total: 2 };
        parkedResult = await ctx.parkForPause?.(at);
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      });
      const claim = gitlabClaim(1222);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      assert.equal(parkedResult, true, "the pause parked");
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the parked run's HOME (its resumable session) was retained");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile for a paused run");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile for a paused run");
      assert.equal(calls.length, 0, "no push/MR for a paused run");
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), "the run reported paused");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // T12 (issue #1764): a plan-phase `now` pause makes the Codex executor reject with a
  // PauseNowSignal (the plan turn has no in-loop park seam). The runner's PauseNowSignal catch owns
  // the park: handlePausePark publishes the checkpoint credential-free and parks on a `paused` ACK,
  // or reports pause_failed and preserves the session for a requeue. Either way no finalize runs,
  // so no permit is minted and no Codex credential reconcile fires, and the finally still disposes
  // the Codex registry exactly once.
  const planPhasePauseNow = (safety: CodexExecutionSafety, runHome: string): FakeCodexExecutor =>
    new FakeCodexExecutor(safety, async (ctx) => {
      fs.mkdirSync(runHome, { recursive: true });
      fs.writeFileSync(path.join(runHome, "session.marker"), "plan session\n");
      commitInTree(ctx.worktreePath, "PLAN-WORK.txt", "committed before the plan-phase pause\n");
      throw new PauseNowSignal();
    });

  it("(T12 parked) a Codex plan-phase PauseNowSignal parks through the runner catch: paused, HOME kept, no terminal, no permit/reconcile, registry disposed", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-plan-now-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      const runHome = path.join(homeRoot, "h");
      const exec = planPhasePauseNow(rig.safety, runHome);
      const claim = gitlabClaim(1225);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), `the run parked (paused ACK); got ${JSON.stringify(st)}`);
      assert.ok(!st.includes("pause_failed"), "the checkpoint landed, so no pause_failed");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the parked run's HOME was retained for the resume");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile");
      assert.equal(calls.length, 0, "no push/MR");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner disposed the Codex registry once");
      assert.equal(rig.registry.state(), "disposed", "the registry ends disposed");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(T12 declined) a Codex plan-phase PauseNowSignal whose checkpoint fails reports pause_failed, preserves the session for requeue, never fails, and disposes the registry", async () => {
    const { gitlab, calls } = fakeGitlab();
    const orig = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string,
      _tipOid: string,
      pack: Readable,
    ) => {
      await drain(pack);
      return { ok: false, httpStatus: 500 };
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-plan-now-fail-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      const runHome = path.join(homeRoot, "h");
      const exec = planPhasePauseNow(rig.safety, runHome);
      const claim = gitlabClaim(1226);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("pause_failed"), `pause_failed was reported; got ${JSON.stringify(st)}`);
      assert.ok(!st.includes("paused"), "the run did not park");
      assert.ok(!st.includes("failed") && !st.includes("completed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the session HOME was preserved for the requeue");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile");
      assert.equal(calls.length, 0, "no push/MR");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(5) a reap:false checkpoint mints NO permit — no withBoundary for the iteration-boundary publish", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    let boundariesAfterCheckpoint: string[] = [];
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "ITER.txt", "iteration work\n");
      await ctx.checkpoint!({ reap: false });
      boundariesAfterCheckpoint = [...rig.boundaries];
      return { branch: ctx.branch };
    });
    await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1221));
    assert.deepEqual(
      boundariesAfterCheckpoint,
      [],
      `a reap:false checkpoint never minted a permit; got ${JSON.stringify(boundariesAfterCheckpoint)}`,
    );
    // ...and finalize still runs afterwards (the ONLY boundary of this run).
    assert.deepEqual(rig.boundaries, ["finalize"], "the only permit this run minted was the finalize sink");
  });
});

// ================================================================================
describe("RunRunner m4 — Claude/stub legacy path: literal killAgentTree reaps, plus the issue #1783 quiescence proofs", () => {
  it("(6) an executor with killAgentTree only (no safety) takes the legacy branch — withBoundary is never referenced", async () => {
    const { gitlab, calls } = fakeGitlab();
    const kills: string[] = [];
    // A LEGACY executor: killAgentTree only, NO `safety`. reapForSink/withCodexBoundaryOnly must
    // take the literal killAgentTree branch (finalize wrapper does NOT reap: the security-boundary
    // killAgentTree already did). No `safety` ⇒ the runner's terminal dispose is skipped entirely.
    const legacy: Executor = {
      run: async (ctx) => {
        commitInTree(ctx.worktreePath, "IMPL.txt", "claude work\n");
        return { branch: ctx.branch };
      },
      killAgentTree: () => kills.push("kill"),
    };
    const claim = gitlabClaim(1230);
    await runnerWith(() => ({ executor: legacy }), gitlab).execute(claim);
    assert.equal(calls.length, 1, "the MR opened exactly as before (finalize wrapper is a plain call)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The security-boundary reap fired once (per the untouched killAgentTree at the boundary),
    // plus exactly one more per issue #1783 quiescence proof, whose first step is the same literal
    // killAgentTree: the finalize boundary and the terminal retire. `=== 3` — not `>= 3` — still
    // guards the double-reap hazard: if withCodexBoundaryOnly's legacy branch erroneously
    // re-reaped, this would be 4.
    assert.equal(kills.length, 3, "security boundary + finalize quiescence + terminal-retire quiescence");
  });
});

// ================================================================================
// Issue #1766 M3b: a Codex refresh/release deferred by a LOCKED owner vault (the api's typed 409
// `vault_locked`) parks the run for recovery, credential-free, instead of failing it.

const VAULT_FEATURE = "recovery_cause_vault_locked";
const VAULT_PUBLISHED =
  "Paused: the run owner's vault is locked. The recovery checkpoint is published; this run resumes automatically at its next retry once the vault is unlocked.";
const VAULT_LOCAL =
  "Paused: the run owner's vault is locked. The recovery checkpoint is saved only on this worker; this run resumes automatically at its next retry once the vault is unlocked.";
const VAULT_UNVERIFIED =
  "Recovery checkpoint could not be verified. Keeping the local work and session and retrying before pausing.";
const VAULT_SETTLE_INCOMPLETE =
  "Waiting for this run's processes to stop before saving its work; keeping the local work and session and retrying.";

function headOf(tree: string): string {
  return execFileSync("git", ["-C", tree, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
}

function trackingTip(iid: number): string | null {
  try {
    return execFileSync(
      "git",
      ["-C", git.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`],
      { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] },
    ).trim();
  } catch {
    return null;
  }
}

function feedTexts(runId: string): string[] {
  return api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String((m.payload as { text?: unknown }).text));
}

function parkReports(runId: string): Array<Record<string, unknown>> {
  return api.states
    .filter((s) => s.runId === runId && s.body.status === "recovery_wait")
    .map((s) => s.body as unknown as Record<string, unknown>);
}

/** The runner's credentialed custody settle, spied: a vault-lock park must never reach it. */
function spyCustodySettle(runner: object): () => number {
  let calls = 0;
  const r = runner as Record<string, (...args: unknown[]) => Promise<unknown>>;
  for (const name of ["reapThenSettleRecoveryGeneration", "settleRecoveryGeneration"] as const) {
    const orig = r[name]!.bind(runner);
    r[name] = async (...args: unknown[]) => {
      calls += 1;
      return orig(...args);
    };
  }
  return () => calls;
}

describe("RunRunner #1766 — a vault-locked Codex deferral parks the run for recovery", () => {
  it("(a) a finalize reconcile 409 vault_locked parks a TYPED recovery_wait when the api advertises it; never failed, no MR, tracking ref covers HEAD, no boundary or credential call after the deferral", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      client.protocolFeatures = [VAULT_FEATURE];
      const rig = codexRig({ vaultLocked: () => true });
      let head = "";
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "VAULT.txt", "work before the vault locked\n");
        head = headOf(ctx.worktreePath);
        return { branch: ctx.branch };
      }, rig.settle);
      const claim = gitlabClaim(1766);
      const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      const custody = spyCustodySettle(runner);
      await runner.execute(claim);

      const parks = parkReports(claim.run_id);
      assert.equal(parks.length, 1, "exactly one recovery_wait park");
      assert.equal(parks[0]!.recovery_cause, "vault_locked", "the park is typed when the feature is advertised");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a vault-locked deferral never fails the run");
      assert.ok(!statuses(claim.run_id).includes("completed"), "nothing completed");
      assert.equal(calls.length, 0, "no MR was created (phasePublish never ran)");
      assert.equal(trackingTip(1766), head, "the worker tracking ref covers the run's HEAD");
      assert.deepEqual(rig.boundaries, ["finalize"], "the capture opened no Codex boundary");
      assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (only the finalize reconcile)");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex at all");
      assert.equal(custody(), 0, "the credentialed custody settle never ran (the hold is kept)");
      assert.ok(feedTexts(claim.run_id).includes(VAULT_PUBLISHED), "the published vault park line is on the feed");
    } finally {
      restore();
    }
  });

  it("(a) without the advertised feature the park is UNTYPED recovery_wait (still never failed)", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [];
    const rig = codexRig({ vaultLocked: () => true });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "VAULT.txt", "work\n");
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1767);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    const parks = parkReports(claim.run_id);
    assert.equal(parks.length, 1);
    assert.equal(parks[0]!.recovery_cause, undefined, "an api without the feature gets the untyped park");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(calls.length, 0);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_LOCAL) || feed.includes(VAULT_PUBLISHED), `a vault park line; feed=${JSON.stringify(feed)}`);
  });

  it("a milestone-checkpoint reconcile 409 vault_locked parks instead of failing", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let head = "";
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
      head = headOf(ctx.worktreePath);
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1768);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "parked at recovery_wait");
    assert.equal(parkReports(claim.run_id)[0]!.recovery_cause, "vault_locked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the checkpoint deferral never fails the run");
    assert.deepEqual(rig.boundaries, ["checkpoint"], "no finalize and no capture boundary after the deferral");
    assert.equal(calls.length, 0);
    assert.equal(trackingTip(1768), head);
  });

  it("a post-approval epoch recreation CodexCredentialDeferredError reports running, then parks", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig();
    let statesAtThrow = -1;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "PLAN.txt", "approved plan work\n");
      statesAtThrow = api.states.length;
      throw new CodexCredentialDeferredError();
    }, rig.settle);
    const claim = gitlabClaim(1769);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    const after = api.states.slice(statesAtThrow).filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepEqual(after, ["running", "recovery_wait"], "the arm confirms running before the park, nothing else");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.deepEqual(rig.boundaries, [], "no Codex boundary was opened by the park");
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 0, "no credential call from the park");
  });

  it("(d) a stale_claim ack on the confirming running report stops silently: no park, no failure", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "STALE.txt", "work\n");
      deferred = true;
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1770);
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", {
      runStatus: "running",
      disposition: "stale_claim",
    });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "a superseded claim is never parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "no failure report");
    assert.equal(fs.existsSync(worktreeDirFor(1770)), false, "normal teardown (the new claim owns the run)");
  });

  it("(d) an ownership probe showing ANOTHER generation stops silently: no park, no failure", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const claim = gitlabClaim(1771, { claim_generation: 5 });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "GEN.txt", "work\n");
      deferred = true;
      api.setOwnershipStatus(claim.run_id, "running", 6);
      return { branch: ctx.branch };
    }, rig.settle);
    // The confirming running report is lost (a non-retried 400), so only the probe can decide.
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", { httpStatus: 400 });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "another generation owns the run: never parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "no failure report");
  });

  it("a lost running-transition reply: the probe reads running at THIS generation, so the run parks", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const claim = gitlabClaim(1772, { claim_generation: 5 });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LOST.txt", "work\n");
      deferred = true;
      api.setOwnershipStatus(claim.run_id, "running", 5);
      return { branch: ctx.branch };
    }, rig.settle);
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", { httpStatus: 400 });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "the probe proved running at this generation: parked");
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("(e) resume: the parked run re-claims on the same worker, reconciles ready and completes with an MR", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    let locked = true;
    const rig1 = codexRig({ vaultLocked: () => locked });
    const claim = gitlabClaim(1773);
    const exec1 = new FakeCodexExecutor(rig1.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "RESUME.txt", "work saved across the vault lock\n");
      return { branch: ctx.branch };
    }, rig1.settle);
    await runnerWith(() => ({ executor: exec1 }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "flight 1 parked");
    assert.equal(calls.length, 0, "flight 1 opened no MR");
    // The park carried no frozen-plan, contract or budget field: nothing the resume reads changed.
    const flight1 = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body as unknown as Record<string, unknown>);
    const parked = flight1.find((b) => b.status === "recovery_wait")!;
    assert.deepEqual(
      Object.keys(parked).filter((k) => !["status", "recovery_cause", "claim_generation", "session_id"].includes(k)),
      [],
      "the park report changes no milestone, contract or budget field",
    );

    locked = false;
    const rig2 = codexRig({ vaultLocked: () => locked });
    let resumedContent = "";
    const exec2 = new FakeCodexExecutor(rig2.safety, async (ctx) => {
      resumedContent = fs.readFileSync(path.join(ctx.worktreePath, "RESUME.txt"), "utf8");
      return { branch: ctx.branch };
    }, rig2.settle);
    await runnerWith(() => ({ executor: exec2 }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 })
      .execute({ ...claim, claim_generation: 2 });
    assert.equal(resumedContent, "work saved across the vault lock\n", "flight 2 reseeded the parked work");
    assert.equal(calls.length, 1, "flight 2 created the MR");
    assert.ok(statuses(claim.run_id).includes("completed"), "flight 2 completed");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.ok(rig2.refreshCalls() >= 1, "flight 2's finalize reconciled ready");
  });

  it("(f) a failed fetch-back and a failed park report retry visibly, then park; custody is never released", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let armed = false;
    let fetchFailures = 0;
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = async (...args) => {
      if (armed && fetchFailures === 0) {
        fetchFailures += 1;
        throw new Error("injected fetch-back failure");
      }
      return fetch(...args);
    };
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "RETRY.txt", "work\n");
      armed = true;
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1774);
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", { httpStatus: 400 });
    let parkAttempts = 0;
    const original = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") parkAttempts += 1;
      return original(runId, body, signal);
    };
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(fetchFailures, 1, "the first fetch-back failed");
    assert.equal(parkAttempts, 2, "the failed park report was retried");
    assert.equal(parkReports(claim.run_id).length, 1, "then the run parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the run stayed running until the park");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_UNVERIFIED), `the unverified retry is visible; feed=${JSON.stringify(feed)}`);
    assert.ok(
      feed.some((t) => t.startsWith("The run owner's vault is locked. Could not record the pause yet")),
      `the failed park report is visible; feed=${JSON.stringify(feed)}`,
    );
    assert.equal(custody(), 0, "custody is never released");
    assert.equal(rig.refreshCalls(), 1, "no credential call while retrying");
  });

  it("(f) an incomplete settle (a surviving writer) never parks: work is retained and the settle retried", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let allow = false;
    let incomplete = 0;
    let reachedTwice!: () => void;
    const twice = new Promise<void>((r) => (reachedTwice = r));
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!allow) {
        incomplete += 1;
        if (incomplete === 2) reachedTwice();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "only copy\n");
      return { branch: ctx.branch };
    }, settle);
    const claim = gitlabClaim(1775);
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        twice,
        execution.then(() => { throw new Error("the flight ended before the settle retried"); }),
      ]);
      assert.equal(parkReports(claim.run_id).length, 0, "no park while the settle is incomplete");
      assert.equal(
        fs.readFileSync(path.join(worktreeDirFor(1775), "UNCOMMITTED.txt"), "utf8"),
        "only copy\n",
        "the local work is retained",
      );
      assert.ok(feedTexts(claim.run_id).includes(VAULT_SETTLE_INCOMPLETE), "the wait is visible on the feed");
      allow = true;
      await execution;
      assert.equal(parkReports(claim.run_id).length, 1, "parked once the settle completed");
      assert.ok(!statuses(claim.run_id).includes("failed"));
    } finally {
      allow = true;
      runner.shutdown();
      await execution;
    }
  });

  it("(g) no credential material leaks into state bodies, feed, logs or the boundary error", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const boundaryErrors: unknown[] = [];
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      try {
        return await originalWithBoundary(request, action);
      } catch (e) {
        boundaryErrors.push(e);
        throw e;
      }
    };
    const { logger, lines } = recordingLogger();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LEAK.txt", "work\n");
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1776);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1);
    assert.equal(boundaryErrors.length, 1, "the finalize reconcile block was observed");
    const be = boundaryErrors[0] as { message: string; deferral?: string; errors: { message: string }[] };
    assert.equal(be.deferral, "vault_locked");
    const haystack = JSON.stringify({
      states: api.states.filter((s) => s.runId === claim.run_id),
      feed: api.messages(claim.run_id),
      lines,
      boundary: { message: be.message, errors: be.errors.map((e) => e.message) },
    });
    for (const secret of [
      SUBSCRIPTION.access_token,
      SUBSCRIPTION.capability,
      RELEASE_TOK,
      REFRESH_TOK,
      VAULT_BODY_SENTINEL,
    ]) {
      assert.ok(!haystack.includes(secret), `no ${secret} in any state body, feed line, log line or boundary error`);
    }
  });

  // Composition of #1764 and #1766: an ACKed owner pause (parkForPause answered `paused`, so the
  // executor returns pausedAt) must bypass the finalize boundary even with the vault locked. Were
  // it to enter the boundary, the per-sink reconcile would hit the 409 vault_locked and the
  // vault-deferral arm would park a durably paused run as recovery_wait. So: zero credential
  // calls, no permit, no recovery park, no vault feed line, and the run stays paused.
  it("(T11 x vault) an owner pause ACKed as paused makes no refreshCodex/releaseCodex call and mints no finalize permit while the vault is locked; it never becomes a vault park", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      client.protocolFeatures = [VAULT_FEATURE];
      const rig = codexRig({ authMode: "subscription", vaultLocked: () => true });
      let parkedResult: boolean | undefined;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "PAUSE.txt", "work before the owner pause\n");
        const at = { completedCount: 1, total: 2 };
        parkedResult = await ctx.parkForPause?.(at);
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      }, rig.settle);
      const claim = gitlabClaim(1764);
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
      assert.equal(parkedResult, true, "the owner pause was ACKed as paused");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex for an owner-paused run");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex for an owner-paused run");
      assert.equal(parkReports(claim.run_id).length, 0, "the owner pause never became a recovery_wait park");
      const feed = feedTexts(claim.run_id);
      assert.ok(!feed.some((t) => /vault/i.test(t)), `no vault park line on the feed; feed=${JSON.stringify(feed)}`);
      assert.equal(calls.length, 0, "no push/MR for a paused run");
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), "the run reported paused");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      restore();
    }
  });
});

// ================================================================================
// Issue #1766 M3b rework: every vault-lock exit that ends the flight on a terminal status or would
// clear the preserve flags captures the run's work FIRST (credential-free), and each confirm /
// report branch keeps its posture. These are the branches a mutation used to survive.

const VAULT_HELD =
  "The run owner's vault is locked and this run is not running, so it was not paused for recovery; its local work and session are kept on this worker.";
const VAULT_CANCEL_REPORT_FAILED =
  "Could not record the cancellation yet; keeping the local work and session and retrying.";
const VAULT_CONFIRM_UNKNOWN =
  "The run owner's vault is locked. Could not confirm this run is still running; keeping the local work and session and retrying before pausing.";

/** The content of `file` at the worker's tracking ref for issue `iid`, or null when absent. */
function trackedFile(iid: number, file: string): string | null {
  try {
    return execFileSync(
      "git",
      ["-C", git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:${file}`],
      { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] },
    );
  } catch {
    return null;
  }
}

/** A recovery-ENABLED coordinator, so the credentialed pre-report reap would really run if a vault
 *  path reached it (a token-less harness short-circuits it and hides the call). */
function enabledRecovery(): { coord: ReturnType<typeof makeRecoveryCoordinator>["coord"]; archive: FakeRecoveryClient } {
  const archive = new FakeRecoveryClient();
  const { coord } = makeRecoveryCoordinator(archive, new FakeRecoveryGit());
  return { coord, archive };
}

/** An executor that commits COMMITTED.txt (never fetched back) and leaves DIRTY.txt uncommitted,
 *  then returns so the finalize reconcile hits the locked vault. `deferred()` flips once it ran. */
function workThenDefer(rig: CodexRig, settle?: (ms: number) => Promise<CredentialFreeSettleOutcome>): {
  exec: FakeCodexExecutor;
  deferred: () => boolean;
  ctx: () => RunContext | undefined;
} {
  let ran = false;
  let seen: RunContext | undefined;
  const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
    seen = ctx;
    commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed, not yet fetched back\n");
    fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "uncommitted edit\n");
    ran = true;
    return { branch: ctx.branch };
  }, settle ?? rig.settle);
  return { exec, deferred: () => ran, ctx: () => seen };
}

function assertCaptured(iid: number): void {
  assert.equal(trackedFile(iid, "COMMITTED.txt"), "committed, not yet fetched back\n", "the unfetched commit is in the tracking ref");
  assert.equal(trackedFile(iid, "DIRTY.txt"), "uncommitted edit\n", "the uncommitted edit is in the tracking ref (WIP commit)");
}

function receiptError(): Error {
  const e = new Error("input receipt given up");
  e.name = "InputReceiptError";
  return e;
}

describe("RunRunner #1766 — vault-lock exits capture before cleanup, and every confirm branch holds", () => {
  it("a terminal ack on the confirming running report captures the unfetched commit and the dirty file before cleanup", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1780);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "a terminal run is never parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "nothing re-reported over the terminal status");
    assertCaptured(1780);
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral");
    assert.equal(rig.releaseCalls(), 0, "no releaseCodex");
    assert.equal(custody(), 0, "no custody settle");
  });

  it("a terminal ack whose exit capture cannot verify keeps the clone (never discards the only copy)", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    git.commitWipMarker = async () => false;
    const claim = gitlabClaim(1781);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1781), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "an unverified capture keeps the clone and its uncommitted edit",
    );
  });

  it("receipt_failed captures first and reports failed with no credentialed reap and no custody settle", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw receiptError();
      return report(runId, body, signal);
    };
    const claim = gitlabClaim(1782);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"), "a given-up receipt fails the run");
    assert.equal(parkReports(claim.run_id).length, 0, "never parked");
    assertCaptured(1782);
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (no credentialed pre-report reap)");
    assert.equal(rig.releaseCalls(), 0, "no releaseCodex after the deferral");
    assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
    assert.equal(custody(), 0, "no custody settle");
    assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
  });

  it("cancel during a vault park captures the unfetched commit and the dirty file, with no credentialed reap", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    // The run stays in the vault park loop (settle incomplete) until the cancel is seen.
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const claim = gitlabClaim(1783);
    // The first cancel report is lost in transport; the retry is visible and then lands.
    let cancelReports = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "failed" && body.failure_reason === "run cancelled" && ++cancelReports === 1) {
        throw new Error("cancel report lost");
      }
      return report(runId, body, signal);
    };
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const custody = spyCustodySettle(runner);
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel was reported",
      );
      assert.equal(parkReports(claim.run_id).length, 0, "a cancelled run is not parked");
      assert.equal(cancelReports, 2, "the lost cancel report was retried");
      assert.ok(feedTexts(claim.run_id).includes(VAULT_CANCEL_REPORT_FAILED), "the cancel-report retry is visible");
      assertCaptured(1783);
      assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (no credentialed reap)");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex after the deferral");
      assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
      assert.equal(custody(), 0, "no custody settle");
      assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("held: a live non-running ack captures, posts an owner-neutral feed line, keeps the clone and never parks or fails", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1784);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "paused" });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "never parked over a live status");
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1784);
    assert.ok(fs.existsSync(worktreeDirFor(1784)), "the clone is kept");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(!feed.some((t) => /your vault/i.test(t)), `the feed is owner-neutral; feed=${JSON.stringify(feed)}`);
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 1, "no credential call after the deferral");
  });

  it("a probe transport failure is retried as unknown: the park waits for a later probe to confirm running", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    // The confirming running report never lands, so only the ownership probe can confirm. The first
    // probe after the deferral fails in transport; every later one reads running.
    const events: string[] = [];
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw new Error("running report lost");
      if (body.status === "recovery_wait") events.push("park");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    let probes = 0;
    client.getRunOwnership = async (runId) => {
      if (!w.deferred()) return probe(runId);
      if (++probes === 1) {
        events.push("probe-failed");
        throw new Error("ownership probe transport failure");
      }
      events.push("probe-ok");
      return probe(runId);
    };
    const claim = gitlabClaim(1785, { claim_generation: 5 });
    api.setOwnershipStatus(claim.run_id, "running", 5);
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    // unknown -> retry: a confirming probe (ok), then the loop's ownership read (ok), then the park.
    // Treating the failure as confirmed would park after ONE ok probe.
    assert.deepEqual(events, ["probe-failed", "probe-ok", "probe-ok", "park"]);
    const feed = feedTexts(claim.run_id);
    assert.equal(feed.filter((t) => t === VAULT_CONFIRM_UNKNOWN).length, 1, `the retry is visible once; feed=${JSON.stringify(feed)}`);
  });

  it("a failed ownership read in the park loop (after running is confirmed) retries visibly", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const probe = client.getRunOwnership.bind(client);
    let failures = 0;
    client.getRunOwnership = async (runId) => {
      if (w.deferred() && failures < 2) {
        failures += 1;
        throw new Error("ownership probe transport failure");
      }
      return probe(runId);
    };
    const claim = gitlabClaim(1790);
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(failures, 2);
    assert.equal(parkReports(claim.run_id).length, 1, "parked once the read recovered");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_CONFIRM_UNKNOWN), `the loop's probe retry is visible; feed=${JSON.stringify(feed)}`);
  });

  it("a probe reading running with NO claim_generation is unknown: no park until this generation is confirmed", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1786, { claim_generation: 5 });
    api.setOwnershipStatus(claim.run_id, "running");
    let healthy = false;
    let genlessProbes = 0;
    let parksWhileUnconfirmed = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait" && !healthy) parksWhileUnconfirmed += 1;
      if (w.deferred() && body.status === "running" && !healthy) throw new Error("running report lost");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      const own = await probe(runId);
      if (w.deferred() && !healthy && ++genlessProbes === 3) {
        healthy = true;
        api.setOwnershipStatus(claim.run_id, "running", 5);
      }
      return own;
    };
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(genlessProbes, 3);
    assert.equal(parksWhileUnconfirmed, 0, "a generation-less running probe never parks");
    assert.equal(parkReports(claim.run_id).length, 1, "parked once running at this generation was confirmed");
  });

  it("a server wall park on the confirming running report retains the clone: no park, no failure", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1787);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", {
      runStatus: "paused",
      holdReason: "budget_exhausted",
      disposition: "stale_claim",
    });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "the server parked it; the worker reports nothing");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1787), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "the clone and its uncommitted edit are retained",
    );
  });

  it("a server wall park on the park report retains the clone and does not re-send the park", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1788);
    let parkAttempts = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") parkAttempts += 1;
      return report(runId, body, signal);
    };
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", {
      runStatus: "paused",
      holdReason: "budget_exhausted",
      disposition: "stale_claim",
    });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkAttempts, 1, "the park is not retried over a server wall park");
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.ok(fs.existsSync(worktreeDirFor(1788)), "the clone is retained");
  });

  it("a 404 on the confirming probe keeps the clone and session, like the park loop's 404", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1791);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { httpStatus: 400 });
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      if (w.deferred()) api.setOwnershipNotOwned(claim.run_id);
      return probe(runId);
    };
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1791), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "no verified capture exists, so the clone and its uncommitted edit are kept",
    );
  });

  it("the retry backoff doubles from recoveryRetryMs and caps at 16x", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    let healthy = false;
    let failedProbes = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running" && !healthy) throw new Error("running report lost");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      if (w.deferred() && !healthy) {
        if (++failedProbes === 7) healthy = true;
        throw new Error("ownership probe transport failure");
      }
      return probe(runId);
    };
    const claim = gitlabClaim(1789);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const waits: number[] = [];
    (runner as unknown as { waitRecoveryRetry: (f: unknown, c: boolean, ms?: number) => Promise<void> }).waitRecoveryRetry =
      async (_f, _c, ms) => {
        waits.push(ms ?? 5);
      };
    await runner.execute(claim);
    assert.deepEqual(waits.slice(0, 7), [5, 10, 20, 40, 80, 80, 80], "doubling, capped at 16x recoveryRetryMs");
    assert.equal(parkReports(claim.run_id).length, 1);
  });
});

// ================================================================================
// Issue #1766 M3b follow-up: the park LOOP's exits (after running is confirmed), a cancel whose
// capture is unverified, the loop's 404, the journaled keepCustody arm, `publish: false` on a
// terminal exit, and a live non-running park or cancel ack.

const SESSION_KEPT = "run interrupted by worker shutdown; preserving its plugin dir and HOME for a same-worker resume";

/** True when executeClaim's finally kept the run's plugin dir and HOME (preserveSession on an
 *  unparked flight: the only branch that logs this line). */
function sessionKept(lines: unknown[]): boolean {
  return lines.some((l) => (l as { msg?: string }).msg === SESSION_KEPT);
}

/** Count client.publishCheckpoint calls made while `when()` holds; every call still lands. */
function countPublishes(when: () => boolean): { count: () => number; restore: () => void } {
  let n = 0;
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    if (when()) n += 1;
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return {
    count: () => n,
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

/** A settle that reports incomplete ONCE (running `onFirst` then), and settles for real after. */
function settleOnceIncomplete(rig: CodexRig, onFirst: () => void): (ms: number) => Promise<CredentialFreeSettleOutcome> {
  let calls = 0;
  return async (ms) => {
    if (++calls === 1) {
      onFirst();
      return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
    }
    return rig.settle(ms);
  };
}

describe("RunRunner #1766 — the park loop's exits after running is confirmed", () => {
  it("a terminal ownership read in the loop (settle retried) captures the unfetched commit and the dirty file, unpublished", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1792);
    const w = workThenDefer(rig, settleOnceIncomplete(rig, () => api.setOwnershipStatus(claim.run_id, "failed")));
    const pubs = countPublishes(() => w.deferred());
    try {
      const { logger, lines } = recordingLogger();
      const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
      const custody = spyCustodySettle(runner);
      await runner.execute(claim);
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "running"),
        "running was confirmed before the loop",
      );
      assert.equal(parkReports(claim.run_id).length, 0, "a terminal run is never parked");
      assert.ok(!statuses(claim.run_id).includes("failed"), "nothing re-reported over the terminal status");
      assertCaptured(1792);
      assert.equal(pubs.count(), 0, "a run already known terminal publishes no checkpoint");
      assert.equal(sessionKept(lines), false, "a verified capture releases the session");
      assert.equal(custody(), 0, "no custody settle");
      assert.equal(rig.refreshCalls() + rig.releaseCalls(), 1, "no credential call after the deferral");
    } finally {
      pubs.restore();
    }
  });

  it("a terminal ownership read in the loop whose exit capture cannot verify keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1793);
    const w = workThenDefer(rig);
    // The in-loop capture fails (unverified, retried); on that failure the run turns terminal.
    git.commitWipMarker = async () => {
      api.setOwnershipStatus(claim.run_id, "failed");
      return false;
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(feedTexts(claim.run_id).includes(VAULT_UNVERIFIED), "the loop retried an unverified capture first");
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1793), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "an unverified exit capture keeps the clone and its uncommitted edit",
    );
    assert.ok(sessionKept(lines), "and keeps the session");
  });

  it("a paused ownership read in the loop after an in-loop capture posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1794);
    // The capture verifies, then the park report is lost and the run is paused underneath it.
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        api.setOwnershipStatus(claim.run_id, "paused");
        throw new Error("park report lost");
      }
      return report(runId, body, signal);
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "never parked over a live status");
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1794);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(fs.existsSync(path.join(worktreeDirFor(1794), "DIRTY.txt")), "the clone is kept after the in-loop capture");
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a 404 in the loop with no verified capture keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1795);
    const w = workThenDefer(rig, settleOnceIncomplete(rig, () => api.setOwnershipNotOwned(claim.run_id)));
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1795), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "the clone and its uncommitted edit are kept",
    );
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a 404 in the loop after a verified capture releases the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1796);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        api.setOwnershipNotOwned(claim.run_id);
        throw new Error("park report lost");
      }
      return report(runId, body, signal);
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assertCaptured(1796);
    assert.equal(fs.existsSync(worktreeDirFor(1796)), false, "the verified capture lets the clone go");
    assert.equal(sessionKept(lines), false, "and the session");
  });

  it("a cancel whose capture is not verified (fetch-back fails) keeps the clone and session after a terminal ack", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = async (...args) => {
      if (w.deferred()) throw new Error("injected fetch-back failure");
      return fetch(...args);
    };
    const claim = gitlabClaim(1797);
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel was reported",
      );
      assert.equal(trackedFile(1797, "DIRTY.txt"), null, "the capture never reached the tracking ref");
      assert.equal(
        fs.readFileSync(path.join(worktreeDirFor(1797), "DIRTY.txt"), "utf8"),
        "uncommitted edit\n",
        "the clone (the only copy) is kept",
      );
      assert.ok(sessionKept(lines), "the session is kept");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("a cancel ack with a live non-running status posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const claim = gitlabClaim(1798);
    api.failStateWhen(claim.run_id, (b) => b.status === "failed" && b.failure_reason === "run cancelled", {
      runStatus: "paused",
    });
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assertCaptured(1798);
      const feed = feedTexts(claim.run_id);
      assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
      assert.ok(fs.existsSync(path.join(worktreeDirFor(1798), "DIRTY.txt")), "the clone is kept");
      assert.ok(sessionKept(lines), "the session is kept");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("a park ack with a live non-running status posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1799);
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", { runStatus: "awaiting_approval" });
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1799);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(fs.existsSync(path.join(worktreeDirFor(1799), "DIRTY.txt")), "the clone is kept after the verified capture");
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a terminal ack on the confirming running report publishes no checkpoint (publish: false)", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const pubs = countPublishes(() => w.deferred());
    try {
      const claim = gitlabClaim(1800);
      api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
      await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
      assertCaptured(1800);
      assert.equal(pubs.count(), 0, "a run already known terminal publishes no checkpoint");
    } finally {
      pubs.restore();
    }
  });

  it("receipt_failed over an already-resolved terminal keeps custody: no credentialed reap, no settle", async () => {
    // Reachable: a permanent message-failure trip (batcher.onPermanentFailureReport) can resolve
    // this generation's `failed` terminal while the finalize reconcile is deferring, and the
    // confirming running report then gives up its input receipt. The trip is modelled by latching
    // flight.terminalResolved at the confirm, which is exactly what journalAndSendTerminal leaves.
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw receiptError();
      return report(runId, body, signal);
    };
    const claim = gitlabClaim(1801);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const r = runner as unknown as { confirmRunningForVaultPark: (f: { terminalResolved?: boolean }, ...rest: unknown[]) => Promise<unknown> };
    const confirm = r.confirmRunningForVaultPark.bind(runner);
    r.confirmRunningForVaultPark = async (flight, ...rest) => {
      flight.terminalResolved = true;
      return confirm(flight, ...rest);
    };
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.ok(!statuses(claim.run_id).includes("failed"), "no second failed over the resolved terminal");
    assertCaptured(1801);
    assert.equal(custody(), 0, "no credentialed reap-then-settle");
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral");
    assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
    assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
  });
});
