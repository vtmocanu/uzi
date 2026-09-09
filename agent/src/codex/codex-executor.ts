// PRD #1171 (M3, milestone 3, Phase 2A) — the production `CodexExecutor` and the
// claim-aware DARK selection seam's Codex branch.
//
// This is the INTEGRATION KEYSTONE: it composes the merged #1188 dark core
// (transport, immutable {@link ExecutionRegistry}/{@link createCodexExecutionSafety}
// owner, {@link CodexCallbackBroker}, {@link CodexDelegationRunner}, the
// {@link CodexHarness} run loop, the credential-free {@link CodexSessionStore}, the
// openat2 fileop client and the M3a {@link launchCodexRoot} launcher) with the M1
// credential bridge ({@link WorkerClient.releaseCodex}/{@link WorkerClient.refreshCodex})
// into ONE `Executor`.
//
// It is DARK: nothing routes to Codex through public paths (M5 owns enablement). It is
// selected ONLY by `main.ts`'s `makeExecutor` when a claim carries a COMPLETE, validated
// `secrets.codex` binding, and it NEVER changes Claude/stub behavior (a claim without the
// block takes the exact legacy path).
//
// SECURITY: the provider credential is a PRIVATE construction input released FRESH per
// provider root (part D). It rides ONLY into the launcher env, never a frame/log/tool
// result. The command + fileop effect surfaces run as the credential-free command identity
// (`commandRootCommand`, uid 10003) with a SCRUBBED env — nothing inherited from
// `process.env`.
//
// SCOPE (R4): this does NOT clone SdkExecutor's Claude-lane extras (empty-turn retry,
// plan/intent summary hooks, health/interleave sentinels, interactive park/pause). It
// reuses `ctx.gatePlan` for the plan→approval gate exactly like SdkExecutor, but drives
// turns Codex-specific. The runner's durability SINKS run through `this.safety.withBoundary`
// (m4), each preceded by an executor-owned per-sink auth-mode reconcile (part 4). Under a
// runner (deferRegistryTeardown), run()'s finally leaves the registry ALIVE for those
// post-run sinks and the runner disposes it via `safety.dispose` after the last sink (F1);
// STANDALONE, run()'s finally backstops the registry teardown itself.

import path from "node:path";
import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import type { Readable, Writable } from "node:stream";

import type { Logger } from "../log.js";
import type { WorkerClient } from "../client.js";
import { PlanRejectedError, type Executor, type ExecutorResult, type RunContext } from "../executor.js";
import type {
  BoundaryRequest,
  BoundaryProcessRequest,
  CodexExecutionSafety,
  HarnessAgent,
  HarnessContextHook,
  HarnessEffort,
  HarnessError,
  ReducedTurnResult,
  RunTurnRequest,
  TurnStreamEnd,
} from "../harness.js";
import { RunTurnReducerImpl } from "../harness-reducer.js";
import { buildLeadSystemPrompt } from "../prompt.js";
import { RUNNER_UID, uidSplitActive } from "../runner-uid.js";
import { errMessage } from "../util.js";
import type { AgentTemplate } from "../protocol.js";

import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "./registry.js";
import {
  createCodexExecutionSafety,
  type ReconcileBeforeBoundary,
  type ReconcileOutcome,
  type SpawnRootSeam,
  type SpawnBoundaryProcessSeam,
  type SpawnedBoundaryProcess,
} from "./safety.js";
import {
  CodexHarness,
  type CodexChildSink,
  type CodexLaunchRootResult,
  type CodexLaunchRootSpec,
  type CodexProviderConfig,
  type LaunchRootSeam,
} from "./codex-harness.js";
import {
  CodexCallbackBroker,
  type FileopClient,
  type ScreenPolicy,
  type SpawnCommandResult,
  type SpawnCommandSeam,
} from "./broker.js";
import {
  CodexDelegationRunner,
  type ChildThreadController,
  type StartChildTurnSpec,
} from "./delegation.js";
import { buildCodexRunPlan } from "./run-builder.js";
import { CodexSessionStore } from "./session-state.js";
import { wireFileopHelper, type FileopHelperHandle } from "./fileop-client.js";
import {
  CODEX_BIN,
  PROVIDER_CHILD_ARGV,
  SUPERVISOR_BIN,
  launchCodexEffectRoot,
  launchCodexRoot,
  type CodexEffectLaunchSpec,
  type CodexRootHandle,
} from "./launcher.js";
import { createCodexTransport } from "./transport.js";
import type { CodexNotification } from "./transport.js";
import type { CodexBinding } from "./select.js";
import { CodexAdviceHarness, type LaunchAdviceRootSeam } from "./codex-advice-harness.js";

// ─── Part G: the FIXED production vendor targets ────────────────────────────────
/**
 * The FIXED production Codex provider targets. NONE of these ever comes from a claim,
 * model, repo, saved label or callback — they are immutable vendor endpoints wired only
 * here. The `wireApi` is pinned to `"responses"` downstream in `launcher.ts`
 * (buildCodexConfigToml), and the binary/supervisor/PATH are already fixed there too.
 *
 * 🔴 MAINTAINER-CONFIRMED-AT-LIVE-ACCEPTANCE: `model:"gpt-6-astra"` is the ADR's INTENDED
 * default (`adr/1106-codex-harness.md:162` marks the model names intended-not-live). These
 * targets are DARK and unreached by production routing; the maintainer confirms the exact
 * endpoint/model at live acceptance. The fake localhost provider stays ONLY in the
 * non-exported `launch-cli.ts` test composition — never reachable from `makeExecutor`.
 */
export const CODEX_PRODUCTION_PROVIDER: CodexProviderConfig = {
  name: "openai",
  baseUrl: "https://api.openai.com/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

/** The pinned, image-baked openat2 fileop helper — built + installed 0555 in both worker
 *  images (base + jvm) alongside the supervisor (m5 packaging). Dark: only reached once a
 *  Codex-bound claim selects the CodexExecutor. */
const FILEOP_BIN = "/usr/local/bin/uzi-codex-fileop";

/** The fully-REPLACED command-identity env (never merged with `process.env`): NOTHING
 *  inherited (no PAT/token/provider credential), only inert path/tmp/locale. This is the
 *  cross-root credential-read boundary — the credential-free command surface must not see
 *  the worker's environment. */
const COMMAND_ENV_PATH = "/usr/bin:/bin";

const DEFAULT_IDLE_MS = 5 * 60 * 1000;
const DEFAULT_WALL_MS = 60 * 60 * 1000;
const DEFAULT_BOUNDARY_DEADLINE_MS = 30 * 1000;
const DEFAULT_CHILD_TURN_DEADLINE_MS = 10 * 60 * 1000;

// Watchdog/cancel trip reasons (secret-free static strings). "run cancelled" matches the
// runner's terminal cancel wording so a Codex cancel routes identically.
const REASON_IDLE = "codex run idle timeout";
const REASON_WALL = "codex run wall-clock timeout";
const REASON_CANCEL = "run cancelled";

/** The reducer's lead-context hook is a NO-OP for Codex: `CodexHarness.readContext`
 *  returns `undefined` (no characterized context RPC yet), so the reducer never attaches a
 *  lead context reading. */
const NOOP_CONTEXT_HOOK: HarnessContextHook = {
  request(): void {},
  get: async (): Promise<undefined> => undefined,
};

/** Map one uzi {@link AgentTemplate} onto the neutral {@link HarnessAgent} the Codex
 *  renderer consumes. `null`/absent tools = inherit; a list = an explicit allowlist. */
function toHarnessAgent(t: AgentTemplate): HarnessAgent {
  return {
    description: t.description,
    prompt: t.prompt_body,
    model: t.model ?? undefined,
    tools: t.tools == null ? { kind: "inherit" } : { kind: "allow", names: t.tools },
    deniedTools: [],
    toolServers: [],
    skills: t.skills ?? [],
  };
}

// ─── A single-consumer child-frame queue (part C consumer side) ─────────────────
/**
 * The demux sink a delegation's child controller drains. The {@link CodexHarness} root
 * loop calls {@link push} for every frame carrying the child thread id; the delegation
 * runner drains {@link iterator}. {@link end} closes the child stream on teardown.
 */
class ChildFrameQueue implements CodexChildSink {
  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private consumed = false;

  push(note: CodexNotification): void {
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: note, done: false });
    } else {
      this.queue.push(note);
    }
  }

  end(): void {
    this.ended = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: undefined, done: true });
    }
  }

  iterator(): AsyncIterableIterator<CodexNotification> {
    if (this.consumed) throw new Error("codex child sink notifications() is single-consumer");
    this.consumed = true;
    // Arrow methods capture the queue's `this` lexically (no `self` alias); the object
    // references itself via the named `iter` const for [Symbol.asyncIterator].
    const iter: AsyncIterableIterator<CodexNotification> = {
      next: (): Promise<IteratorResult<CodexNotification>> =>
        new Promise((resolve) => {
          const q = this.queue.shift();
          if (q !== undefined) {
            resolve({ value: q, done: false });
            return;
          }
          if (this.ended) {
            resolve({ value: undefined, done: true });
            return;
          }
          this.waiter = resolve;
        }),
      return: (): Promise<IteratorResult<CodexNotification>> => Promise.resolve({ value: undefined, done: true }),
      [Symbol.asyncIterator]: (): AsyncIterableIterator<CodexNotification> => iter,
    };
    return iter;
  }
}

// ─── Part F: the advice auth bridge + factory (deliver only; NOT wired into model-pass) ──
/**
 * A NARROW credential bridge scoped to ONE selected credential's capability, for the
 * isolated Codex advice lane. It wraps {@link WorkerClient.releaseCodex} (a fresh committed
 * token, both modes) and — SUBSCRIPTION ONLY — {@link WorkerClient.refreshCodex}, minting
 * ONE `operation_id` per logical refresh and RETAINING it across retries so a lost reply
 * replays rather than starting a second provider exchange. An api_key credential can never
 * refresh (fail-closed throw). If authority is unavailable, `release`/`refresh` throw and no
 * advice root is built. It carries NO workspace/registry/broker — the advice ceiling.
 */
export class CodexAdviceCredentialBridge {
  private readonly runId: string;
  private readonly client: Pick<WorkerClient, "releaseCodex" | "refreshCodex">;
  private readonly binding: CodexBinding;
  // ONE operation id per logical refresh, retained across retries (part F). Reset via
  // {@link beginRefreshOperation} when a genuinely NEW logical refresh starts.
  private refreshOperationId: string | undefined;
  private observedGeneration: number | undefined;

  constructor(
    runId: string,
    client: Pick<WorkerClient, "releaseCodex" | "refreshCodex">,
    binding: CodexBinding,
  ) {
    this.runId = runId;
    this.client = client;
    this.binding = binding;
    this.observedGeneration = binding.generation; // subscription initial; undefined for api_key
  }

  /** Release the run's currently-committed access token (both auth modes). Fails CLOSED
   *  (throws) on any release error — capability/ownership loss must not fall back. */
  async release(signal?: AbortSignal): Promise<string> {
    const expected = this.binding.authMode === "subscription"
      ? {
          authMode: "subscription" as const,
          chatgptAccountId: this.binding.chatgptAccountId,
          minimumGeneration: this.observedGeneration ?? this.binding.generation,
        }
      : { authMode: "api_key" as const };
    const res = await this.client.releaseCodex(
      this.runId,
      { capability: this.binding.capability },
      expected,
      signal,
    );
    if (res.auth_mode === "subscription") this.observedGeneration = res.generation;
    return res.access_token;
  }

  /** Begin a NEW logical refresh: mint a fresh operation id. Call before the FIRST attempt
   *  of a logical refresh; do NOT call between retries (retries reuse the retained id). */
  beginRefreshOperation(): void {
    this.refreshOperationId = randomUUID();
  }

  /** Run the coordinated subscription refresh and return the freshly-committed token.
   *  SUBSCRIPTION ONLY — an api_key credential fails CLOSED. The operation id is minted
   *  lazily on the first call and RETAINED across retries. */
  async refresh(signal?: AbortSignal): Promise<string> {
    if (this.binding.authMode !== "subscription") {
      throw new Error("codex advice refresh is not permitted for an api_key credential");
    }
    if (this.observedGeneration === undefined) {
      throw new Error("codex advice refresh has no observed generation");
    }
    if (this.refreshOperationId === undefined) this.refreshOperationId = randomUUID();
    const res = await this.client.refreshCodex(
      this.runId,
      {
        capability: this.binding.capability,
        operation_id: this.refreshOperationId,
        observed_generation: this.observedGeneration,
      },
      { authMode: "subscription", chatgptAccountId: this.binding.chatgptAccountId },
      signal,
    );
    this.observedGeneration = res.generation; // the worker's NEXT observed generation
    // TODO(PRD#1171 integration): replace this bridge with the app-server callback owner,
    // which clears the retained operation id after this authenticated committed success
    // and mints a new one only for the next logical callback. Until then callers must use
    // beginRefreshOperation at that boundary; ambiguous failures retain this id.
    return res.access_token;
  }
}

/**
 * PRD #1171 m4 (part 4): the RUN-LANE per-sink auth-mode reconcile closure — the safety facade
 * runs it BEFORE every durability boundary. Mirrors {@link CodexAdviceCredentialBridge}: it
 * captures the runId, the client (release/refresh) and the immutable binding (authMode +
 * subscription generation), and ONLY its neutral {@link ReconcileOutcome} crosses back into the
 * generic safety code (never authMode/token/generation).
 *
 *  - SUBSCRIPTION: mint ONE operation_id per LOGICAL refresh and RETAIN it across retries (a lost
 *    reply replays rather than starting a second provider exchange); refreshCodex → advance the
 *    observed generation on success (a durable commit BEFORE the permit) and clear the operation
 *    id so the next boundary mints a fresh one. A contended/quarantined/persistence-failure (any
 *    HTTP error) → {kind:"blocked"} WITHOUT advancing/clearing, so a RETRIED boundary reuses the
 *    same operation id rather than beginning a second provider exchange.
 *  - API_KEY: ZERO refreshCodex calls; a fresh releaseCodex only. success → ready.
 *
 * `registerToken` is called with any freshly released token (subscription or api_key) so the
 * caller can register it with the run redactor and track it for terminal eviction.
 *
 * Exported (with {@link CodexAdviceCredentialBridge}) so the differential test pins BOTH against
 * drift.
 */
export function buildRunLaneReconcile(
  runId: string,
  client: Pick<WorkerClient, "releaseCodex" | "refreshCodex">,
  binding: CodexBinding,
  registerToken: (token: string) => void,
): ReconcileBeforeBoundary {
  let reconcileOperationId: string | undefined;
  let observedGeneration = binding.generation; // subscription initial; undefined for api_key
  return async (_request: BoundaryRequest, signal: AbortSignal): Promise<ReconcileOutcome> => {
    try {
      if (binding.authMode === "subscription") {
        if (observedGeneration === undefined) {
          // A subscription binding always carries a generation (the selector enforces it); its
          // absence here is a protocol fault, not a live credential, so fail closed.
          const errors: readonly HarnessError[] = [
            { category: "authorization", message: "codex subscription boundary reconcile has no observed generation" },
          ];
          return { kind: "blocked", errors };
        }
        // ONE operation id per LOGICAL refresh, RETAINED across retries until it SUCCEEDS.
        if (reconcileOperationId === undefined) reconcileOperationId = randomUUID();
        const res = await client.refreshCodex(
          runId,
          {
            capability: binding.capability,
            operation_id: reconcileOperationId,
            observed_generation: observedGeneration,
          },
          { authMode: "subscription", chatgptAccountId: binding.chatgptAccountId },
          signal,
        );
        registerToken(res.access_token);
        observedGeneration = res.generation; // durable commit → advance BEFORE the permit
        reconcileOperationId = undefined; // logical refresh done; next boundary mints fresh
        return { kind: "ready" };
      }
      // api_key: ZERO refresh; a fresh release re-authorizes only (no subscription fallback).
      const res = await client.releaseCodex(
        runId,
        { capability: binding.capability },
        { authMode: "api_key" },
        signal,
      );
      registerToken(res.access_token);
      return { kind: "ready" };
    } catch {
      // Fail CLOSED with a bounded, secret-free reason (authMode is safe to name). The op id and
      // observed generation are DELIBERATELY left intact so a retried boundary reuses them.
      const errors: readonly HarnessError[] = [
        { category: "authorization", message: `codex ${binding.authMode} boundary reconcile failed` },
      ];
      return { kind: "blocked", errors };
    }
  };
}

/**
 * Build an isolated {@link CodexAdviceHarness} whose credential is freshly released through
 * `bridge`. If the bridge fails closed (authority unavailable) the release throws and NO
 * advice root is built — the failure propagates rather than degrading to an un-credentialed
 * root. Only `credentialValue` is fed into the harness — NO workspace/registry/broker.
 *
 * This DELIVERS the factory + bridge; it is deliberately NOT wired into the shared
 * `model-pass.ts` (R3), which hardcodes `ClaudeAdviceHarness` and owns Claude parity.
 */
export async function makeCodexAdviceHarness(
  bridge: CodexAdviceCredentialBridge,
  provider: CodexProviderConfig,
  launchRoot: LaunchAdviceRootSeam,
  log: Logger,
  signal?: AbortSignal,
): Promise<CodexAdviceHarness> {
  const credentialValue = await bridge.release(signal);
  return new CodexAdviceHarness({ launchRoot, provider, credentialValue, log });
}

// ─── The fail-closed selection guard executor ───────────────────────────────────
/**
 * The executor `makeExecutor` returns when the claim carries a PRESENT-but-BROKEN Codex
 * block (a {@link import("./select.js").CodexSelectionError}). Its `run()` immediately
 * throws the bounded, secret-free error message so the run routes through the existing
 * failed-run catch — NEVER a silent Claude fallback and NEVER any model work.
 */
export class FailClosedExecutor implements Executor {
  constructor(private readonly message: string) {}

  run(_ctx: RunContext): Promise<ExecutorResult> {
    return Promise.reject(new Error(this.message));
  }
}

// ─── Injectable seams (production defaults; tests inject fakes) ──────────────────
export interface CodexExecutorDeps {
  /** Adapts the real M3a launcher for a PROVIDER root; a test injects a fake returning a
   *  scripted in-memory transport (NO real Codex). Receives the harness's launch spec AND
   *  the FRESHLY-RELEASED credential; the credential rides ONLY into the launcher env. */
  readonly launchProviderRoot?: (spec: CodexLaunchRootSpec, credential: string) => Promise<CodexLaunchRootResult>;
  /** Test-only high-level command seam. Production leaves this absent and uses
   * the registered supervisor-root implementation. */
  readonly spawnCommand?: SpawnCommandSeam;
  /** Test-only fileop client factory, invoked only after a command supervisor
   * root has been reserved, launched and registered. */
  readonly wireFileop?: (process: { stdin: Writable | null; stdout: Readable | null }) => FileopHelperHandle;
  /** Low-level supervised effect launcher. Production uses the M3a supervisor;
   * tests inject in-memory streams while retaining registry ownership. */
  readonly launchEffectRoot?: (spec: CodexEffectLaunchSpec, deadlineMs?: number) => Promise<CodexRootHandle>;
  /** The boundary-action spawn seam for the safety facade (m4 wires the real supervisor
   *  root). Never invoked in m3 (no runner sink is routed through `withBoundary` yet). */
  readonly spawnBoundaryRoot?: SpawnRootSeam;
  /** The credential-free session store (default: the real {@link CodexSessionStore}). */
  readonly sessionStore?: Pick<typeof CodexSessionStore, "adopt" | "inspect" | "remove">;
  readonly idleMs?: number;
  readonly wallMs?: number;
  readonly boundaryDeadlineMs?: number;
  readonly childTurnDeadlineMs?: number;
  /** Base TMPDIR exposed to an injected high-level command seam. The production
   * supervised path replaces it with a random per-root directory allowlisted by
   * the Landlock wrapper. */
  readonly commandTmpdir?: string;
  /** PRD #1171 m4 (F1): the runner OWNS the terminal registry teardown. When true, `run()`'s
   *  finally does NOT quiesce/reap/dispose the registry — its post-run durability sinks
   *  (park/shutdown/finalize) reap the provider root through `withBoundary`, and the runner's
   *  `executeClaim` finally calls `safety.dispose` once every sink has settled — so the
   *  registry must stay ALIVE across `run()`. When false/absent (a STANDALONE executor with no
   *  runner sink), `run()`'s finally is the sole teardown and reaps+disposes the registry
   *  itself (the backstop). The runner (main.ts) sets it true. */
  readonly deferRegistryTeardown?: boolean;
}

export interface CodexExecutorOptions {
  readonly binding: CodexBinding;
  readonly client: WorkerClient;
  readonly provider: CodexProviderConfig;
}

// ─── The production CodexExecutor ───────────────────────────────────────────────
export class CodexExecutor implements Executor {
  /** M3/M4 (PRD #1171): the Codex outer safety facade, POPULATED at the top of `run()` (before
   *  any model work) with the per-sink auth-mode reconcile closure. The runner's durability
   *  sinks route through it (m4). Absent means Claude/stub — this executor always sets it, so
   *  the runner never takes the legacy killAgentTree branch for a Codex run. This class
   *  deliberately does NOT implement `killAgentTree`. */
  safety?: CodexExecutionSafety;

  private readonly log: Logger;
  private readonly homeRoot: string;
  private readonly opts: CodexExecutorOptions;
  private readonly deps: CodexExecutorDeps;
  private readonly sessionStore: Pick<typeof CodexSessionStore, "adopt" | "inspect" | "remove">;

  constructor(log: Logger, homeRoot: string, opts: CodexExecutorOptions, deps: CodexExecutorDeps = {}) {
    this.log = log;
    this.homeRoot = homeRoot;
    this.opts = opts;
    this.deps = deps;
    this.sessionStore = deps.sessionStore ?? CodexSessionStore;
  }

  async run(ctx: RunContext): Promise<ExecutorResult> {
    // Everything that needs the worktree is built at the TOP of run() (the executor is
    // constructed before the claim's worktree exists).
    const binding = this.opts.binding;
    const provider = this.opts.provider;
    const worktreePath = ctx.worktreePath;
    const ownedDataRoot = path.join(this.homeRoot, "codex-data");
    const storeDir = path.join(this.homeRoot, "codex-session-store");
    const boundaryDeadlineMs = this.deps.boundaryDeadlineMs ?? DEFAULT_BOUNDARY_DEADLINE_MS;

    if (uidSplitActive()) await assertCommandWorktreePosture(worktreePath);

    // (C) Every FRESH provider token released this run is registered with the logger's
    // secret set (`addSecret`) before use; without a matching `removeSecret` a long-lived
    // worker's secret set would grow permanently, one entry per provider-root start. Track
    // them here and evict them ONLY at terminal cleanup (never mid-run, where a late log
    // line could still carry the token) — over-retention is the safe direction.
    const releasedTokens = new Set<string>();

    // (1) The immutable per-run local-execution-epoch registry.
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(0));
    const launchEffectRoot = this.deps.launchEffectRoot ?? ((spec: CodexEffectLaunchSpec, deadlineMs?: number) =>
      launchCodexEffectRoot(spec, deadlineMs === undefined ? {} : { deadlines: { started: deadlineMs } }));

    // (2) The outer safety facade — populated BEFORE any model work. The legacy
    // root-only seam remains for compatibility; permit-held Git uses the supervised
    // process seam wired below.
    const spawnBoundaryRoot: SpawnRootSeam =
      this.deps.spawnBoundaryRoot ??
      ((): Promise<RegisteredRoot> =>
        Promise.reject(new Error("codex boundary-action spawn is not wired until m4")));
    // (2b) PRD #1171 m4: the per-sink auth-mode reconcile closure. It runs INSIDE the safety
    // facade before every boundary quiesce/reap/mint, and ONLY its neutral ReconcileOutcome
    // crosses back into the generic code — the runner and generic withBoundary never see
    // authMode/token/generation. All credential logic lives here (mirroring the advice bridge).
    this.safety = createCodexExecutionSafety(
      registry,
      spawnBoundaryRoot,
      this.makeBoundaryReconcile(ctx.runId, releasedTokens),
      // (C, F1) Terminal eviction of tokens released by the POST-RUN sink reconciles. The
      // runner calls safety.dispose after the last durability sink — by which point run()'s
      // finally (below) has already evicted+cleared the DURING-run tokens — so tokens the
      // sinks added here are un-scrubbed at the true terminal. `clear()` makes it idempotent
      // (a second dispose, or the standalone backstop, sees an empty set and no-ops).
      () => {
        for (const token of releasedTokens) this.log.removeSecret(token);
        releasedTokens.clear();
      },
      makeBoundaryProcessSpawner(launchEffectRoot),
    );

    // The SCRUBBED command-identity env — NOTHING from process.env (cross-root credential
    // boundary). Both the shell effect surface and the fileop helper use it.
    const commandEnv: NodeJS.ProcessEnv = {
      PATH: COMMAND_ENV_PATH,
      TMPDIR: this.deps.commandTmpdir ?? "/tmp",
      LANG: "C",
    };

    // (4) The command + fileop effect surfaces (credential-free command identity).
    const baseSpawnCommand = this.deps.spawnCommand ?? makeDefaultSpawnCommand(
      registry,
      launchEffectRoot,
      boundaryDeadlineMs,
      worktreePath,
      commandEnv,
    );
    // Route the SCRUBBED command-identity env THROUGH the seam (not merely closed over by the
    // default seam) so an injected seam records the exact env command spawns run under — the
    // same scrubbed env the fileop seam already receives. [#1171 m5 review]
    const spawnCommand: SpawnCommandSeam = (argv, spawnOpts) => baseSpawnCommand(argv, { ...spawnOpts, env: commandEnv });
    const fileopRoot = await launchRegisteredEffectRoot(
      registry,
      launchEffectRoot,
      commandEffectSpec(
        worktreePath,
        worktreePath,
        FILEOP_BIN,
        ["--root", worktreePath],
        commandEnv,
      ),
      boundaryDeadlineMs,
      "command",
    );
    let fileopHandle: FileopHelperHandle;
    try {
      fileopHandle = (this.deps.wireFileop ?? wireFileopHelper)({
        stdin: fileopRoot.handle.transport.stdin,
        stdout: fileopRoot.handle.transport.stdout,
      });
    } catch (error) {
      await registry.reapRoot(fileopRoot.root, boundaryDeadlineMs);
      throw error;
    }
    const fileop: FileopClient = fileopHandle.client;

    const screenPolicy: ScreenPolicy = { dockerWired: false };
    const childTurnDeadlineMs = this.deps.childTurnDeadlineMs ?? DEFAULT_CHILD_TURN_DEADLINE_MS;

    // (5) The composition cycle (harness → broker → delegation → harness) is resolved by
    // late-binding `harness`: the delegation seam captures it by reference and is only
    // invoked once a spawn_agent callback fires, by which point `harness` is assigned.
    let harness!: CodexHarness;

    // (3) PHASE-CORRECT broker + delegation construction, PER TURN. The broker enforces the
    // plan-phase file-write ban through `grants.phase` (broker.dispatchFileWrite returns
    // `write_denied_in_plan`) and its delegation runner passes the SAME phase into each
    // child's grants. buildCodexRunPlan("plan") does NOT strip write tools — the phase field
    // IS the mechanism (render.ts) — so the broker's grants MUST be phase-correct for the turn
    // it serves. A single implement-phase broker (frozen at construction) would leave that ban
    // INERT during the plan turn, letting a model apply_patch/Write mutate the worktree BEFORE
    // approval, subagents included. So the broker + its delegation runner are rebuilt per turn
    // from a phase-correct run plan; only the phase-derived grants/roles/allowedRoles change.
    // The registry, effect seams, worktree, screen policy and cancel signal are reused, and the
    // provider root + transport (owned by the single `harness`, below) are built once and reused.
    const buildPhaseBroker = (phase: "plan" | "implement", signal?: AbortSignal): CodexCallbackBroker => {
      const runPlan = buildCodexRunPlan(
        this.buildRunRequest(ctx, phase, this.phasePrompt(ctx, phase), undefined, new AbortController().signal),
      );
      const delegationRunner = new CodexDelegationRunner({
        registry,
        roles: runPlan.roles,
        // The child-turn seam demuxes a child thread's frames off the SAME transport
        // (part C): start a child thread/turn, register a sink, hand the runner a controller.
        startChildTurn: (spec: StartChildTurnSpec) => this.startChildTurn(harness, provider, worktreePath, spec),
        spawnCommand,
        fileop,
        worktreePath,
        screenPolicy,
        signal,
        childTurnDeadlineMs,
      });
      return new CodexCallbackBroker({
        registry,
        spawnCommand,
        fileop,
        worktreePath,
        grants: runPlan.lead.grants,
        delegate: delegationRunner.toDelegateSeam(),
        allowedRoles: runPlan.allowedRoles,
        screenPolicy,
        signal,
      });
    };

    // Seed the harness with the PLAN-phase broker (the fail-safe default: writes denied).
    // `run()` re-points it to the phase-correct broker BEFORE each turn via `harness.useBroker`,
    // so a turn can never route through implement-phase grants unless the executor grants them.
    const planBroker = buildPhaseBroker("plan");

    // The FRESH-credential provider launch seam (part D): release a fresh committed token
    // per provider root, register it with the redactor, adopt the credential-free session
    // subset into the fresh home, then launch with the fresh token (which rides ONLY into
    // the launcher env). A new root always re-authorizes; a resume still gets a fresh token.
    const launchProviderRoot = this.deps.launchProviderRoot ?? defaultLaunchProviderRoot;
    const providerLaunchSeam: LaunchRootSeam = async (spec) => {
      const expected = binding.authMode === "subscription"
        ? {
            authMode: "subscription" as const,
            chatgptAccountId: binding.chatgptAccountId,
            minimumGeneration: binding.generation,
          }
        : { authMode: "api_key" as const };
      const released = await this.opts.client.releaseCodex(
        ctx.runId,
        { capability: binding.capability },
        expected,
        ctx.signal,
      );
      if (!releasedTokens.has(released.access_token)) {
        this.log.addSecret(released.access_token); // BEFORE any use
        releasedTokens.add(released.access_token); // evicted at terminal cleanup (part C)
      }
      const codexHome = path.join(spec.ownedDataRoot, "codex");
      // Best-effort credential-free seed. NOTE (m3b/m4): under the uid split the fresh home
      // is runner-owned 0700, so this cross-uid seed moves into the launcher's runner-
      // identity tree step; here (and in the unit composition) it runs against the worker.
      await this.sessionStore.adopt(storeDir, codexHome).catch(() => undefined);
      return launchProviderRoot({ ...spec, credentialValue: released.access_token }, released.access_token);
    };

    harness = new CodexHarness({
      registry,
      launchRoot: providerLaunchSeam,
      broker: planBroker,
      provider,
      workspace: worktreePath,
      homeDir: ownedDataRoot,
      log: this.log,
      // The store is per-run and credential-free; presence backs inspectSession.
      sessionInspect: () => this.sessionStore.inspect(storeDir),
      // The credential is NOT known at construction — it is released FRESH per root inside
      // `providerLaunchSeam`, so the harness carries none (undefined).
      credentialValue: undefined,
    });

    const reducer = new RunTurnReducerImpl(NOOP_CONTEXT_HOOK);
    const resumeId = ctx.sessionId ?? undefined;
    const idleMs = this.deps.idleMs ?? (ctx.config?.idle_timeout_seconds ? ctx.config.idle_timeout_seconds * 1000 : DEFAULT_IDLE_MS);
    const wallMs = this.deps.wallMs ?? (ctx.config?.run_timeout_seconds ? ctx.config.run_timeout_seconds * 1000 : DEFAULT_WALL_MS);

    try {
      // Plan → approval gate, exactly like SdkExecutor (fail-closed): a pre-approved resume
      // skips the planning turn and the gate.
      const preApproved = ctx.planApproved === true && !!ctx.approvedPlan?.trim();
      if (!preApproved && ctx.gatePlan) {
        // The PLAN turn(s) run under PLAN-phase grants: the broker denies every file write
        // (write_denied_in_plan) and child subagents inherit plan-phase grants, so nothing
        // mutates the worktree before the plan is approved. All revise iterations reuse this
        // plan-phase broker (the implement turn re-points it, below).
        let planResult = await this.driveCodexTurn(ctx, harness, reducer, "plan", this.planPrompt(ctx), resumeId, idleMs, wallMs, buildPhaseBroker);
        let planMd = planResult.plan;
        if (planMd === undefined || planMd.trim().length === 0) {
          throw new Error("codex plan turn produced no plan");
        }
        let verdict = await ctx.gatePlan(planMd, planResult.milestones);
        while (verdict.kind === "revise") {
          ctx.emit({ kind: "plan_feedback", agent: "worker", payload: { feedback: verdict.feedback } });
          planResult = await this.driveCodexTurn(ctx, harness, reducer, "plan", this.planPrompt(ctx), resumeId, idleMs, wallMs, buildPhaseBroker);
          planMd = planResult.plan;
          if (planMd === undefined || planMd.trim().length === 0) {
            throw new Error("codex plan turn produced no plan on revision");
          }
          verdict = await ctx.gatePlan(planMd, planResult.milestones);
        }
        if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
        if (verdict.kind === "cancel") throw new Error(REASON_CANCEL);
      }

      // Implement turn (Codex-specific driving; the full lifecycle is m5). Re-point the
      // broker to IMPLEMENT-phase grants (file writes permitted) BEFORE driving it. The plan
      // driveCodexTurn has returned; the next (implement) turn drives sequentially. The
      // re-point is race-safe not because every callback has drained (a backgrounded child
      // callback can outlive the plan turn) but because an in-flight child captured its OWN
      // plan-phase child broker at spawn — the root `this.broker` swap never touches it.
      await this.driveCodexTurn(ctx, harness, reducer, "implement", this.implementPrompt(ctx), resumeId, idleMs, wallMs, buildPhaseBroker);

      return { branch: ctx.branch };
    } finally {
      // Terminal cleanup (m4 F1): a BACKSTOP that reaps+disposes the registry ONLY when NO
      // runner sink will (deferRegistryTeardown unset); otherwise it leaves the registry ALIVE
      // for the runner's post-run durability sinks and only tears down the executor-owned
      // harness/transport, fileop handle and credential-free store.
      await this.terminalCleanup(registry, harness, fileopHandle, storeDir, boundaryDeadlineMs);
      // (C) Evict the DURING-run released tokens (provider-root launches) from the logger's
      // secret set — AFTER the harness/transport is closed above, so no late log line can
      // still carry the token. `removeSecret` is reference-counted. `clear()` so the terminal
      // dispose hook (which evicts the POST-RUN sink tokens) does not double-remove these:
      // under deferRegistryTeardown this finally runs BEFORE the post-run sinks, so those
      // sink-released tokens are evicted by the onDispose hook wired into safety.dispose.
      for (const token of releasedTokens) this.log.removeSecret(token);
      releasedTokens.clear();
    }
  }

  // ─── the per-turn drive (run-lane precedence, Codex-specific) ─────────────────
  /**
   * Drive ONE Codex turn, reimplementing the run-lane PRECEDENCE from sdk-executor.ts
   * driveTurn:
   *   (a) a watchdog/cancel trip is FIRST-WINS — checked before/after the loop and in the
   *       catch, so it beats a raw AbortError AND a later terminal;
   *   (b) a query/iteration/close THROW propagates (the harness's own unexpected-EOF is a
   *       `protocol` throw, NEVER a fabricated success);
   *   (c) a `turn_finished` `outcome:"failed"` is classified ONCE and materialized+thrown
   *       (the harness represents a provider terminal failure as DATA, never double-thrown);
   *   (d) a clean terminal → `finish({kind:"terminal"})`, else exhausted → `finish({exhausted})`.
   * Idle re-arms on every event; the wall arms at turn start and disarms in finally.
   */
  private async driveCodexTurn(
    ctx: RunContext,
    harness: CodexHarness,
    reducer: RunTurnReducerImpl,
    phase: "plan" | "implement",
    prompt: string,
    resumeId: string | undefined,
    idleMs: number,
    wallMs: number,
    buildPhaseBroker: (phase: "plan" | "implement", signal?: AbortSignal) => CodexCallbackBroker,
  ): Promise<ReducedTurnResult> {
    const turnAbort = new AbortController();
    let tripReason: string | undefined;
    const trip = (reason: string): void => {
      if (tripReason === undefined) {
        tripReason = reason;
        turnAbort.abort();
      }
    };
    // A cancel arrives on the run's own AbortSignal; treat it as a first-wins trip so it
    // beats a raw aborted transport error and a late terminal.
    const onCancel = (): void => trip(REASON_CANCEL);
    if (ctx.signal) {
      if (ctx.signal.aborted) trip(REASON_CANCEL);
      else ctx.signal.addEventListener("abort", onCancel, { once: true });
    }
    let idleTimer: ReturnType<typeof setTimeout> | undefined;
    const armIdle = (): void => {
      if (idleTimer) clearTimeout(idleTimer);
      idleTimer = setTimeout(() => trip(REASON_IDLE), idleMs);
      idleTimer.unref?.();
    };
    let wallTimer: ReturnType<typeof setTimeout> | undefined = setTimeout(() => trip(REASON_WALL), wallMs);
    wallTimer.unref?.();

    reducer.beginTurn();
    let sawTerminal = false;
    let terminal: import("../harness.js").HarnessTerminal | undefined;

    const request = this.buildRunRequest(ctx, phase, prompt, resumeId, turnAbort.signal);
    try {
      harness.useBroker(buildPhaseBroker(phase, turnAbort.signal));
      if (tripReason) throw this.tripError(tripReason);
      armIdle();
      const turn = harness.startTurn(request);
      for await (const event of turn.events) {
        armIdle(); // any event is liveness
        const reduction = await reducer.accept(event);
        if (reduction.firstSessionId !== undefined) {
          try {
            ctx.onSessionId?.(reduction.firstSessionId);
          } catch (err) {
            this.log.warn("codex onSessionId handler threw", { run_id: ctx.runId, error: errMessage(err) });
          }
        }
        for (const em of reduction.messages) ctx.emit(em);
        if (reduction.progress) void Promise.resolve(ctx.reportProgress?.(reduction.progress)).catch(() => undefined);
        if (event.kind === "turn_finished") {
          sawTerminal = true;
          terminal = event.terminal;
          turn.requestStop("terminal");
          break;
        }
      }
      // (a) FIRST-WINS trip.
      if (tripReason) throw this.tripError(tripReason);
      // (c) a failed terminal is classified ONCE and materialized+thrown here.
      if (sawTerminal && terminal && terminal.outcome === "failed") {
        const thrown = terminal.failure
          ? terminal.failure.materialize(undefined)
          : { original: new Error("codex run failed: unknown") };
        throw thrown.original;
      }
      // (d) clean terminal / exhausted.
      const end: TurnStreamEnd = sawTerminal && terminal ? { kind: "terminal", terminal } : { kind: "exhausted" };
      return reducer.finish(end).result;
    } catch (err) {
      // (a) again: a trip beats the raw aborted/protocol error the iterator threw.
      if (tripReason) throw this.tripError(tripReason);
      throw err instanceof Error ? err : new Error(errMessage(err));
    } finally {
      if (idleTimer) clearTimeout(idleTimer);
      if (wallTimer) clearTimeout(wallTimer);
      if (ctx.signal) ctx.signal.removeEventListener("abort", onCancel);
    }
  }

  private tripError(reason: string): Error {
    return new Error(reason);
  }

  // ─── the per-sink auth-mode reconcile closure (m4, part 4) ────────────────────
  /**
   * The executor-owned auth-mode reconcile closure the safety facade runs BEFORE every boundary
   * quiesce/reap/mint. Delegates to the standalone {@link buildRunLaneReconcile} (pinned by the
   * differential test against the advice bridge), registering any fresh token with the redactor
   * and tracking it for terminal eviction (part C), the same as a provider-root release.
   */
  private makeBoundaryReconcile(runId: string, releasedTokens: Set<string>): ReconcileBeforeBoundary {
    const log = this.log;
    return buildRunLaneReconcile(runId, this.opts.client, this.opts.binding, (token) => {
      if (!token || releasedTokens.has(token)) return;
      log.addSecret(token); // BEFORE any use; balanced by removeSecret in the terminal dispose hook (post-run sinks) or run()'s finally (during-run)
      releasedTokens.add(token);
    });
  }

  // ─── the child-turn demux seam (part C) ───────────────────────────────────────
  private startChildTurn(
    harness: CodexHarness,
    provider: CodexProviderConfig,
    workspace: string,
    spec: StartChildTurnSpec,
  ): Promise<ChildThreadController> {
    return (async (): Promise<ChildThreadController> => {
      // Start a CHILD thread on the SAME app-server transport as the root (same untrusted
      // project / doc-max-0 config, never a hook-trust bypass).
      const threadRes = await harness.requestOnTransport<{ thread?: { id?: string } }>(
        "thread/start",
        {
          model: spec.model ?? provider.model,
          modelProvider: provider.name,
          cwd: workspace,
          approvalPolicy: "never",
          ephemeral: true,
          config: { project_doc_max_bytes: 0, projects: { [workspace]: { trust_level: "untrusted" } } },
          instructions: spec.systemPrompt,
        },
        { signal: spec.signal },
      );
      const childThreadId = threadRes?.thread?.id;
      if (typeof childThreadId !== "string" || childThreadId.length === 0) {
        throw new Error("codex child thread/start returned no thread id");
      }
      // Register the sink BEFORE turn/start so no child turn frame is missed by the demux.
      const sink = new ChildFrameQueue();
      harness.registerChildSink(childThreadId, sink);
      let childTurnId: string;
      try {
        const turnParams: Record<string, unknown> = {
          threadId: childThreadId,
          input: [{ type: "text", text: spec.taskInput }],
        };
        if (spec.model !== undefined) turnParams.model = spec.model;
        if (spec.effort !== undefined) turnParams.modelReasoningEffort = spec.effort;
        const turnRes = await harness.requestOnTransport<{ turn?: { id?: string } }>("turn/start", turnParams, { signal: spec.signal });
        const id = turnRes?.turn?.id;
        if (typeof id !== "string" || id.length === 0) throw new Error("codex child turn/start returned no turn id");
        childTurnId = id;
      } catch (err) {
        harness.unregisterChildSink(childThreadId);
        sink.end();
        throw err;
      }
      return {
        threadId: childThreadId,
        turnId: childTurnId,
        notifications: () => sink.iterator(),
        respond: (requestId, reply) => harness.respondOnTransport(requestId, reply),
        interrupt: async (): Promise<void> => {
          await harness
            .requestOnTransport("turn/interrupt", { threadId: childThreadId, turnId: childTurnId }, { signal: spec.signal })
            .catch(() => undefined);
        },
        close: async (): Promise<void> => {
          harness.unregisterChildSink(childThreadId);
          sink.end();
        },
      };
    })();
  }

  // ─── request + prompt construction ────────────────────────────────────────────
  private buildRunRequest(
    ctx: RunContext,
    phase: "plan" | "implement",
    prompt: string,
    resumeId: string | undefined,
    signal: AbortSignal,
  ): RunTurnRequest {
    let leadBody: string | undefined;
    const agents: Record<string, HarnessAgent> = {};
    for (const t of ctx.agents ?? []) {
      // The lead is the ROOT thread, not a subagent — its body seeds the lead system
      // prompt; every other template is a delegation target.
      if (t.name === "lead") {
        leadBody = t.prompt_body;
        continue;
      }
      agents[t.name] = toHarnessAgent(t);
    }
    const systemPrompt = buildLeadSystemPrompt(leadBody, { kind: ctx.kind }).append;
    const leadSkills = (ctx.skills ?? []).map((s) => s.name);
    const effort = codexEffort(ctx);
    const request: RunTurnRequest = {
      prompt,
      systemPrompt,
      phase,
      agents,
      leadSkills,
      signal,
      model: this.opts.provider.model,
      ...(resumeId !== undefined ? { resumeSessionId: resumeId } : {}),
      ...(effort !== undefined ? { effort } : {}),
    };
    return request;
  }

  /** The phase-appropriate prompt for a run-plan build. buildCodexRunPlan reads only the
   *  request's grants/roles from this (the broker never consults the prompt text), so this
   *  only keeps the constructed run plan phase-consistent for clarity. */
  private phasePrompt(ctx: RunContext, phase: "plan" | "implement"): string {
    return phase === "plan" ? this.planPrompt(ctx) : this.implementPrompt(ctx);
  }

  private planPrompt(ctx: RunContext): string {
    const head = ctx.issueIid != null ? `Issue #${ctx.issueIid}: ${ctx.issueTitle}` : ctx.issueTitle;
    return `${head}\n\n${ctx.issueDescription}\n\nProduce a plan for this work and submit it for approval.`;
  }

  private implementPrompt(ctx: RunContext): string {
    const approved = ctx.approvedPlan?.trim();
    if (approved) return approved;
    const head = ctx.issueIid != null ? `Issue #${ctx.issueIid}: ${ctx.issueTitle}` : ctx.issueTitle;
    return `${head}\n\n${ctx.issueDescription}`;
  }

  // ─── terminal cleanup (m4 F1: the relocated registry teardown + backstop) ─────
  private async terminalCleanup(
    registry: ExecutionRegistry,
    harness: CodexHarness,
    fileopHandle: FileopHelperHandle,
    storeDir: string,
    deadlineMs: number,
  ): Promise<void> {
    // PRD #1171 m4 (F1): the REGISTRY teardown (quiesce → reap → disposeTools) is RELOCATED to
    // the runner when it drives this executor. Its post-run durability sinks
    // (park/shutdown/finalize) reap the provider root through `withBoundary` AFTER run()
    // returns, and the runner's executeClaim finally calls `safety.dispose` once the last sink
    // settles — so the registry must stay ALIVE here. `deferRegistryTeardown` therefore SKIPS
    // the registry teardown in that mode. When it is NOT set (a STANDALONE executor with no
    // runner sink — a direct caller, or an exception before/around the model turns), this
    // finally is the SOLE teardown, so it BACKSTOPS the registry here: reap+dispose the
    // provider root so it is torn down on every no-sink path. disposeTools is idempotent, so a
    // later runner dispose can never double-dispose.
    if (!this.deps.deferRegistryTeardown && registry.state() !== "disposed") {
      try {
        const q = await registry.quiesceChildren(deadlineMs);
        if (q.kind === "quiescent") await registry.reapProcesses(deadlineMs, q.epoch);
      } catch {
        /* best-effort terminal reap; the safety facade owns the poison bookkeeping */
      }
      try {
        await registry.disposeTools(deadlineMs);
      } catch {
        /* idempotent dispose */
      }
    }
    // The harness/transport, the fileop helper and the credential-free session store are
    // EXECUTOR-owned (not the registry) — always torn down here, idempotently, on every path.
    await harness.close().catch(() => undefined);
    await fileopHandle.dispose().catch(() => undefined);
    // The session subset is credential-free and re-seeded on the next claim's adopt, so it
    // is removed at the terminal boundary. (A park/resume-preserve carve-out is m4/m5.)
    await this.sessionStore.remove(storeDir).catch(() => undefined);
  }
}

// ─── production seam defaults (DARK; tests inject fakes) ────────────────────────
/** The HARD byte cap on ONE command's combined stdout+stderr capture. A model-steered
 *  `Bash` (`yes`, `base64 /dev/zero`, `cat /dev/urandom`) would otherwise grow these JS
 *  strings without bound and OOM the worker BEFORE the broker's post-accumulation 64 KiB
 *  display cap can run. 1 MiB is comfortably above that display cap yet firmly bounded; at
 *  the cap we STOP accumulating (keep the byte-bounded prefix) and SIGKILL the child, then
 *  RESOLVE (never reject) with the truncated output so the broker still applies its own cap.
 *  Turn abort and capture-cap trips now dispose the registered supervisor root and
 *  await ECHILD+__WALL before the callback settles. */
export const MAX_COMMAND_CAPTURE_BYTES = 1 << 20; // 1 MiB
/** The exit code reported when a command is SIGKILLed AT the capture cap. 137 = 128 + 9
 *  (SIGKILL), the conventional shell convention, so a capped result is distinguishable from
 *  a clean exit while still being a plain non-zero code the broker/reducer already tolerate. */
export const COMMAND_CAPTURE_KILLED_CODE = 137;

const COMMAND_SANDBOX_BIN = "/usr/local/bin/uzi-codex-command-sandbox";

async function assertCommandWorktreePosture(worktreePath: string): Promise<void> {
  const stat = await fs.stat(worktreePath);
  const required = 0o2070; // setgid plus group rwx
  if (stat.gid !== RUNNER_UID || (stat.mode & required) !== required) {
    throw new Error("Codex command worktree lacks the required runner-group setgid/write posture");
  }
}

interface RegisteredEffectRoot {
  readonly handle: CodexRootHandle;
  readonly root: RegisteredRoot;
}

export function registeredRoot(handle: CodexRootHandle, kind: RegisteredRoot["kind"]): RegisteredRoot {
  return {
    kind,
    reap: async (deadlineMs) => {
      const outcome = await handle.dispose(deadlineMs);
      return outcome.clean
        ? { ok: true }
        : { ok: false, error: { category: "tool", message: `${kind} supervisor root disposal not clean` } };
    },
    dispose: async (deadlineMs) => {
      const outcome = await handle.dispose(deadlineMs);
      if (!outcome.clean) throw new Error(`${kind} supervisor root disposal not clean`);
    },
  };
}

async function launchRegisteredEffectRoot(
  registry: ExecutionRegistry,
  launch: (spec: CodexEffectLaunchSpec, deadlineMs?: number) => Promise<CodexRootHandle>,
  spec: CodexEffectLaunchSpec,
  deadlineMs: number,
  kind: RegisteredRoot["kind"],
): Promise<RegisteredEffectRoot> {
  const reservation = registry.reserveLaunch(kind);
  if (reservation.kind !== "reserved") throw new Error(`${kind} launch admission is closed`);
  let handle: CodexRootHandle;
  try {
    handle = await launch(spec, deadlineMs);
  } catch (error) {
    registry.cancelReservation(reservation.reservation);
    throw error;
  }
  const root = registeredRoot(handle, kind);
  const admitted = registry.registerRoot(reservation.reservation, root);
  if (!admitted.ok) {
    await root.dispose(deadlineMs).catch(() => undefined);
    throw new Error(`${kind} root failed registry admission`);
  }
  return { handle, root };
}

/** Build the fixed argv for the root-owned Landlock wrapper used by uid 10003.
 * Landlock allowlists only this run's worktree, its random private tmp and
 * read-only system/toolchain paths, so sibling `/data`, `/run`, `/proc` and `/tmp`
 * state cannot be enumerated even though every run shares numeric uid 10003. */
export function commandSandboxArgv(
  worktreePath: string,
  cwd: string,
  command: string,
  args: readonly string[],
  privateTmp: string,
): string[] {
  const worktree = path.resolve(worktreePath);
  const workCwd = path.resolve(cwd);
  const rel = path.relative(worktree, workCwd);
  if (rel === ".." || rel.startsWith(`..${path.sep}`) || path.isAbsolute(rel)) {
    throw new Error("command cwd escapes the worktree sandbox");
  }
  if (!path.isAbsolute(privateTmp) || privateTmp === path.parse(privateTmp).root) {
    throw new Error("command private tmp must be an absolute non-root path");
  }
  return [
    "--root", worktree,
    "--tmp", privateTmp,
    "--cwd", workCwd,
    "--", command, ...args,
  ];
}

function commandEffectSpec(
  worktreePath: string,
  cwd: string,
  command: string,
  args: readonly string[],
  env: NodeJS.ProcessEnv,
): CodexEffectLaunchSpec {
  const cleanupToken = randomUUID();
  const privateTmp = `/tmp/uzi-codex-command-${cleanupToken}`;
  return {
    identity: "command",
    command: COMMAND_SANDBOX_BIN,
    args: commandSandboxArgv(worktreePath, cwd, command, args, privateTmp),
    cwd: worktreePath,
    env: { ...env, HOME: privateTmp, TMPDIR: privateTmp },
    supervisorBin: SUPERVISOR_BIN,
    cleanupToken,
  };
}

/** Run a model-authorized shell effect as a registered command supervisor root.
 * The callback returns only after the primary child and every backgrounded
 * descendant have settled; abort/cap paths also reap before returning. */
export function makeDefaultSpawnCommand(
  registry: ExecutionRegistry,
  launch: (spec: CodexEffectLaunchSpec, deadlineMs?: number) => Promise<CodexRootHandle>,
  reapDeadlineMs: number,
  worktreePath: string,
  commandEnv: NodeJS.ProcessEnv,
): SpawnCommandSeam {
  return async (argv, opts): Promise<SpawnCommandResult> => {
      const [cmd, ...rest] = argv;
      const launched = await launchRegisteredEffectRoot(
        registry,
        launch,
        commandEffectSpec(worktreePath, opts.cwd ?? worktreePath, cmd ?? "/bin/sh", rest, opts.env ?? commandEnv),
        reapDeadlineMs,
        "command",
      );
      let stdout = "";
      let stderr = "";
      let capturedBytes = 0;
      let killedAtCap = false;
      let capResolve!: () => void;
      const capTrip = new Promise<void>((resolve) => { capResolve = resolve; });
      // Accumulate up to MAX_COMMAND_CAPTURE_BYTES combined across BOTH streams. On the
      // chunk that would cross the cap, keep only the byte-bounded prefix, mark the capture
      // truncated, and SIGKILL the child (an unbounded producer cannot be allowed to OOM the
      // worker). A later chunk is dropped once truncated. The child is killed AT the cap.
      const accumulate = (chunk: string, onto: "stdout" | "stderr"): void => {
        if (killedAtCap) return;
        const remaining = MAX_COMMAND_CAPTURE_BYTES - capturedBytes;
        const chunkBytes = Buffer.byteLength(chunk, "utf8");
        if (chunkBytes <= remaining) {
          capturedBytes += chunkBytes;
          if (onto === "stdout") stdout += chunk;
          else stderr += chunk;
          return;
        }
        // The chunk crosses the cap: keep the byte-bounded prefix only, then kill.
        let prefix = Buffer.from(chunk, "utf8").subarray(0, remaining).toString("utf8");
        // A byte slice can end mid-codepoint; Buffer.toString would replace that
        // suffix with a three-byte U+FFFD and exceed a 1-2 byte remainder. Trim the
        // decoded suffix until its encoded form is within the hard byte budget.
        while (Buffer.byteLength(prefix, "utf8") > remaining) prefix = prefix.slice(0, -1);
        if (onto === "stdout") stdout += prefix;
        else stderr += prefix;
        capturedBytes = MAX_COMMAND_CAPTURE_BYTES;
        killedAtCap = true;
        capResolve();
      };
      launched.handle.transport.stdout?.setEncoding("utf8");
      launched.handle.transport.stdout?.on("data", (c: string) => accumulate(c, "stdout"));
      launched.handle.transport.stderr?.setEncoding("utf8");
      launched.handle.transport.stderr?.on("data", (c: string) => accumulate(c, "stderr"));
      const ended = (stream: Readable | null): Promise<void> => new Promise((resolve) => {
        if (!stream || stream.readableEnded || stream.destroyed) { resolve(); return; }
        stream.once("end", resolve);
        stream.once("close", resolve);
      });
      const outputEnded = Promise.all([
        ended(launched.handle.transport.stdout),
        ended(launched.handle.transport.stderr),
      ]);

      let onAbort: (() => void) | undefined;
      const aborted = new Promise<"aborted">((resolve) => {
        if (opts.signal?.aborted) resolve("aborted");
        else if (opts.signal) {
          onAbort = () => resolve("aborted");
          opts.signal.addEventListener("abort", onAbort, { once: true });
        }
      });
      const terminal = launched.handle.waitChild(DEFAULT_WALL_MS).then((result) => ({ kind: "exit" as const, code: result.code }));
      void terminal.catch(() => undefined);
      const first = await Promise.race([
        terminal,
        aborted.then(() => ({ kind: "aborted" as const })),
        capTrip.then(() => ({ kind: "cap" as const })),
      ]);
      if (onAbort && opts.signal) opts.signal.removeEventListener("abort", onAbort);
      const reaped = await registry.reapRoot(launched.root, reapDeadlineMs);
      if (!reaped.ok) throw new Error("command supervisor root did not reap cleanly");
      await outputEnded;
      if (first.kind === "aborted") throw new Error("command aborted");
      return {
        code: first.kind === "cap" || killedAtCap ? COMMAND_CAPTURE_KILLED_CODE : first.code,
        stdout,
        stderr,
      };
  };
}

function makeBoundaryProcessSpawner(
  launch: (spec: CodexEffectLaunchSpec, deadlineMs?: number) => Promise<CodexRootHandle>,
): SpawnBoundaryProcessSeam {
  return async (request: BoundaryProcessRequest, deadlineMs: number): Promise<SpawnedBoundaryProcess> => {
    const [command, ...args] = request.argv;
    if (!command) throw new Error("boundary process argv is empty");
    const spec: CodexEffectLaunchSpec = request.identity === "command"
      ? commandEffectSpec(request.cwd, request.cwd, command, args, request.env)
      : {
          identity: "worker_pat",
          command,
          args,
          cwd: request.cwd,
          env: request.env,
          supervisorBin: SUPERVISOR_BIN,
        };
    const handle = await launch(spec, deadlineMs);
    // stderr is consumed by GitCache for all boundary processes. The provider
    // adapter below drains its otherwise-unused stderr independently.
    return {
      root: registeredRoot(handle, "boundary_action"),
      stdin: handle.transport.stdin,
      stdout: handle.transport.stdout,
      stderr: handle.transport.stderr,
      waitChild: async (deadlineMs) => handle.waitChild(deadlineMs),
    };
  };
}

/** Adapt the real M3a launcher for a PROVIDER root. Builds the launcher-fixed spec (the
 *  pinned binary/supervisor/argv), launches, and adapts the {@link CodexRootHandle} into a
 *  registry-ownable {@link RegisteredRoot} + the app-server transport. DARK: never run in
 *  tests (a fake `launchProviderRoot` is injected). */
async function defaultLaunchProviderRoot(spec: CodexLaunchRootSpec, credential: string): Promise<CodexLaunchRootResult> {
  const handle = await launchCodexRoot({
    ownedDataRoot: spec.ownedDataRoot,
    provider: {
      name: spec.provider.name,
      baseUrl: spec.provider.baseUrl,
      envKey: spec.provider.envKey,
      credentialValue: credential,
    },
    model: spec.model,
    codexBin: CODEX_BIN,
    supervisorBin: SUPERVISOR_BIN,
    kind: "provider",
    childArgv: [...PROVIDER_CHILD_ARGV],
    cwd: spec.cwd,
  });
  const stdout = handle.transport.stdout;
  const stdin = handle.transport.stdin;
  if (!stdout || !stdin) throw new Error("codex provider root is missing a stdio transport channel");
  // Provider stderr is never model-visible or logged, but it must be drained: an
  // unread pipe can fill and deadlock the app-server before the registry can reap it.
  handle.transport.stderr?.resume();
  const transport = createCodexTransport({ inbound: stdout, outbound: stdin });
  const root = registeredRoot(handle, "provider");
  return { root, transport, supervisorPid: handle.supervisorPid ?? -1 };
}

/** The uzi effort contract value (subset), or undefined. Codex m3 carries none on the claim
 *  config yet, so this is undefined today; the seam exists so m5 can resolve a per-run
 *  effort without reshaping the request builder. */
function codexEffort(_ctx: RunContext): HarnessEffort | undefined {
  return undefined;
}
