import { after } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { type Readable } from "node:stream";
import {
  type CredentialFreeSettleOutcome,
  type Executor,
  type RunContext,
  type ExecutorResult,
} from "../src/executor.js";
import {
  createCodexExecutionSafety,
  type ArmBoundaryDeadline,
  type ReconcileBeforeBoundary,
  type SpawnedBoundaryProcess,
} from "../src/codex/safety.js";
import { buildRunLaneReconcile } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import type { BoundaryProcessRequest, BoundaryRequest, CodexExecutionSafety } from "../src/harness.js";
import { type StartupCleanupAuthorization } from "../src/codex/launcher.js";
import { GitLabClient } from "../src/forge.js";
import { WorkerClient, CodexRequestFailure, RequestError } from "../src/client.js";
import { FakeRecoveryClient, FakeRecoveryGit, makeRecoveryCoordinator } from "./codex-reap-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, runnerWith } from "./runner-harness.js";

// PRD #1171 m4 (Phase 2B) — every durability/publication sink in the runner routes through the
// optional Codex `withBoundary` reap facade, per-sink auth-mode reconcile, the pause-sink
// designation, and the F1 dispose relocation. Driven with a FAKE Codex executor whose `.safety`
// is a REAL {@link createCodexExecutionSafety} (over a real registry + a tracked provider root +
// the real {@link buildRunLaneReconcile} run-lane reconcile) wrapped to RECORD every withBoundary/
// dispose call. The runner stays harness-agnostic — it only ever sees `executor.safety`.

export const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

export function commitInTree(treePath: string, file: string, content: string): void {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
}

export function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => {});
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint: drain the pack and return a LANDED publish, so a park/pause
 *  checkpoint confirmably lands (its boundary/permit routing is what these tests assert). */
export function spyPublishLands(): () => void {
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

export const SUBSCRIPTION = {
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

export interface CodexRig {
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

export const RELEASE_TOK = "codex-release-tok-XXXXXXXX";
export const REFRESH_TOK = "codex-refresh-tok-XXXXXXXX";

/** Issue #1766: raw server body text; it must never reach a state body, feed line or log line. */
export const VAULT_BODY_SENTINEL = "vault-body-sentinel-XXXXXXXX";

/** Issue #1766: the api's real reply to a Codex refresh/release that passed authorization but hit
 *  a locked owner vault: HTTP 409 with a typed `reason`. */
/** Issue #1784: the server's 409 for a reconcile against a run that is no longer actively claimed. */
function notAvailableError(op: "refresh" | "release"): RequestError {
  return new RequestError(
    "POST",
    `/api/worker/runs/run-codex-sink/codex/${op}`,
    409,
    JSON.stringify({ error: "codex credential is not available" }),
  );
}

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
export function manualDeadline(boundary: BoundaryRequest["boundary"]): {
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

export const delay = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

export function codexRig(
  opts: {
    authMode?: "subscription" | "api_key";
    credentialClient?: Pick<WorkerClient, "releaseCodex" | "refreshCodex">;
    blockReconcile?: boolean;
    /** Forwarded to createCodexExecutionSafety (issue #1513): event-gates a boundary deadline. */
    armDeadline?: ArmBoundaryDeadline;
    /** Delay before each permit-held process spawn: reproduces suite load deterministically. */
    processSpawnDelayMs?: number;
    /** Replace one test-owned boundary process, leaving every other Git child real. */
    boundaryProcess?: (
      request: BoundaryProcessRequest,
      startupMs: number,
      startupCleanup?: StartupCleanupAuthorization,
    ) => SpawnedBoundaryProcess | undefined | Promise<SpawnedBoundaryProcess | undefined>;
    /** Issue #1766: while this answers true, refreshCodex/releaseCodex throw the api's real typed
     *  409 `vault_locked` RequestError (a locked owner vault after authorization). */
    vaultLocked?: () => boolean;
    refreshUnknown?: boolean;
    /** Issue #1784: while this answers true, refreshCodex/releaseCodex throw the api's 409 "codex
     *  credential is not available" (the server refuses a reconcile for a `paused` run). */
    refuseReconcile?: () => boolean;
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
      if (opts.refuseReconcile?.()) throw notAvailableError("release");
      if (opts.vaultLocked?.()) throw vaultLockedError("release");
      if (opts.blockReconcile) throw new Error("release contended");
      return { access_token: RELEASE_TOK };
    },
    refreshCodex: async (
      _runId: string,
      _req: { capability: string; operation_id: string; observed_generation: number },
    ) => {
      refreshCalls += 1;
      if (opts.refreshUnknown) throw new CodexRequestFailure("transport");
      if (opts.refuseReconcile?.()) throw notAvailableError("refresh");
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
    opts.credentialClient ?? fakeClient as never,
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
    async (request, startupMs, startupCleanup) => {
      processSpawns += 1;
      processArgv.push(request.argv);
      const injected = opts.boundaryProcess
        ? await opts.boundaryProcess(request, startupMs, startupCleanup)
        : undefined;
      if (injected) return injected;
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
export class FakeCodexExecutor implements Executor {
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

export function statuses(runId: string): string[] {
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
export const SHUTDOWN_LOAD_SPAWN_DELAY_MS = 110;
export const FINALIZE_LOAD_SPAWN_DELAY_MS = 250;

/** The finalize deadline contract: a stuck forge request is aborted by the (event-gated)
 *  finalize deadline and settles before withBoundary returns and before terminal dispose. */
export async function finalizeDeadlineScenario(
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
export async function shutdownDeadlineScenario(
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

export const VAULT_FEATURE = "recovery_cause_vault_locked";
export const VAULT_PUBLISHED =
  "Paused: the run owner's vault is locked. The recovery checkpoint is published; this run resumes automatically at its next retry once the vault is unlocked.";
export const VAULT_LOCAL =
  "Paused: the run owner's vault is locked. The recovery checkpoint is saved only on this worker; this run resumes automatically at its next retry once the vault is unlocked.";
export const VAULT_UNVERIFIED =
  "Recovery checkpoint could not be verified. Keeping the local work and session and retrying before pausing.";
export const VAULT_SETTLE_INCOMPLETE =
  "Waiting for this run's processes to stop before saving its work; keeping the local work and session and retrying.";

export function headOf(tree: string): string {
  return execFileSync("git", ["-C", tree, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
}

export function trackingTip(iid: number): string | null {
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

export function feedTexts(runId: string): string[] {
  return api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String((m.payload as { text?: unknown }).text));
}

export function parkReports(runId: string): Array<Record<string, unknown>> {
  return api.states
    .filter((s) => s.runId === runId && s.body.status === "recovery_wait")
    .map((s) => s.body as unknown as Record<string, unknown>);
}

/** The runner's credentialed custody settle, spied: a vault-lock park must never reach it. */
export function spyCustodySettle(runner: object): () => number {
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


export const VAULT_HELD =
  "The run owner's vault is locked and this run is not running, so it was not paused for recovery; its local work and session are kept on this worker.";
export const VAULT_CANCEL_REPORT_FAILED =
  "Could not record the cancellation yet; keeping the local work and session and retrying.";
export const VAULT_CONFIRM_UNKNOWN =
  "The run owner's vault is locked. Could not confirm this run is still running; keeping the local work and session and retrying before pausing.";

/** The content of `file` at the worker's tracking ref for issue `iid`, or null when absent. */
export function trackedFile(iid: number, file: string): string | null {
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

/** Every enabledRecovery() recoveryRoot, removed when the file ends (PRD #1809 M2). */
const recoveryRoots: string[] = [];
export function installRecoveryRootCleanup(): void {
  after(() => {
    for (const r of recoveryRoots.splice(0)) fs.rmSync(r, { recursive: true, force: true });
  });
}

/** A recovery-ENABLED coordinator, so the credentialed pre-report reap would really run if a vault
 *  path reached it (a token-less harness short-circuits it and hides the call). */
export function enabledRecovery(): { coord: ReturnType<typeof makeRecoveryCoordinator>["coord"]; archive: FakeRecoveryClient } {
  const archive = new FakeRecoveryClient();
  const { coord, root } = makeRecoveryCoordinator(archive, new FakeRecoveryGit());
  recoveryRoots.push(root);
  return { coord, archive };
}

/** An executor that commits COMMITTED.txt (never fetched back) and leaves DIRTY.txt uncommitted,
 *  then returns so the finalize reconcile hits the locked vault. `deferred()` flips once it ran. */
export function workThenDefer(rig: CodexRig, settle?: (ms: number) => Promise<CredentialFreeSettleOutcome>): {
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

export function assertCaptured(iid: number): void {
  assert.equal(trackedFile(iid, "COMMITTED.txt"), "committed, not yet fetched back\n", "the unfetched commit is in the tracking ref");
  assert.equal(trackedFile(iid, "DIRTY.txt"), "uncommitted edit\n", "the uncommitted edit is in the tracking ref (WIP commit)");
}


export function receiptError(): Error {
  const e = new Error("input receipt given up");
  e.name = "InputReceiptError";
  return e;
}


const SESSION_KEPT = "run interrupted by worker shutdown; preserving its plugin dir and HOME for a same-worker resume";

/** True when executeClaim's finally kept the run's plugin dir and HOME (preserveSession on an
 *  unparked flight: the only branch that logs this line). */
export function sessionKept(lines: unknown[]): boolean {
  return lines.some((l) => (l as { msg?: string }).msg === SESSION_KEPT);
}

/** Count client.publishCheckpoint calls made while `when()` holds; every call still lands. */
export function countPublishes(when: () => boolean): { count: () => number; restore: () => void } {
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
export function settleOnceIncomplete(rig: CodexRig, onFirst: () => void): (ms: number) => Promise<CredentialFreeSettleOutcome> {
  let calls = 0;
  return async (ms) => {
    if (++calls === 1) {
      onFirst();
      return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
    }
    return rig.settle(ms);
  };
}


export function manualStartedDeadline() {
  let callback: (() => void) | undefined;
  let cancelled = 0;
  let armed!: (ms: number) => void;
  const installed = new Promise<number>(resolve => { armed = resolve; });
  return {
    installed,
    arm: (fire: () => void, ms: number) => {
      assert.equal(callback, undefined, "one owned startup timer");
      callback = fire;
      armed(ms);
      return () => { cancelled += 1; callback = undefined; };
    },
    expire: () => {
      assert.ok(callback, "real launcher armed its startup callback");
      callback();
      assert.equal(callback, undefined, "expiry cancelled the injected timer");
    },
    assertCancelled: () => {
      assert.equal(cancelled, 1, "startup timer cancelled exactly once");
      assert.equal(callback, undefined);
    },
  };
}
