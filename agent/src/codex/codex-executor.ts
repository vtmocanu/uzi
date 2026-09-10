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
// provider root (part D). It rides only through the app-server authentication RPC, never a
// launcher environment, frame, log, or tool result. The command + fileop effect surfaces run as the credential-free command identity
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
import { constants as FS } from "node:fs";
import type { Readable, Writable } from "node:stream";

import type { Logger } from "../log.js";
import type { WorkerClient } from "../client.js";
import { PlanRejectedError, type EmittedMessage, type Executor, type ExecutorResult, type RunContext } from "../executor.js";
import { makeMemoryToolHandlers, memoryToolNames, type MemoryToolHandlers } from "../memory-tools.js";
import { makeFindingsToolHandlers, reportIncidentalIssueToolName, type FindingsToolHandlers } from "../findings-tools.js";
import { FORGE_SERVER_NAME, makeForgeToolHandlers, type ForgeToolHandlers } from "../forge-tools.js";
import { provisionRunTools } from "../provision-run.js";
import { asText } from "../tool-evidence.js";
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
import { buildLeadSystemPrompt, buildRevisePlanPrompt } from "../prompt.js";
import { RUNNER_UID, WORKER_UID, uidSplitActive } from "../runner-uid.js";
import { errMessage } from "../util.js";
import type { AgentTemplate, ClaimSkill } from "../protocol.js";

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
  type ToolHandler,
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
import { CODEX_M3B_LOOPBACK_PROVIDER_NAME } from "./config.js";
import { buildCodexDynamicTools } from "./dynamic-tools.js";
import { CodexAdviceHarness, type LaunchAdviceRootSeam } from "./codex-advice-harness.js";
import {
  createCodexAppServerAuth,
  type CodexAppServerAuthConfig,
  type CodexAppServerAuthMode,
  type CodexSubscriptionRefreshBridge,
} from "./appserver-auth.js";

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

/** The fixed command-identity PATH: the system dirs plus the pinned worker toolchain,
 *  mirroring the provider lane's `CODEX_LAUNCH_PATH` (launcher.ts). These FIXED dirs are
 *  always present and come FIRST; a run's provisioned `toolEnv.PATH` is APPENDED after
 *  them (see {@link buildCommandEnv}), so provisioned tools resolve but can never displace
 *  or drop the boundary dirs. Every entry sits under a directory the command root's
 *  Landlock allows read+exec on (`/usr`, `/sbin`, `/bin`, `/opt/uzi-toolchain`, `/nix`),
 *  so a resolved binary is executable inside the sandbox. This env is never merged with
 *  `process.env`: NOTHING is inherited (no PAT/token/provider credential) — the
 *  credential-free command surface must not see the worker's environment. */
const COMMAND_ENV_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin";

/** Keys a provisioned `toolEnv` entry may NEVER set in the credential-free command env.
 *  UNIONS the command boundary literals (PATH/TMPDIR/LANG/HOME — the scrubbed command
 *  identity pins these, and sdk-env.ts's PROTECTED_ENV_KEYS deliberately OMITS
 *  PATH/TMPDIR/LANG, so relying on it alone would leave those open) with a MIRROR of that
 *  module-private PROTECTED_ENV_KEYS set (the OAuth credential + the two ANTHROPIC_* keys +
 *  the browser flag). So a hostile/malformed `toolEnv` can neither breach the command
 *  boundary NOR reintroduce a credential-adjacent key. PATH is additionally handled
 *  specially in {@link buildCommandEnv} (fixed prefix, provisioned value appended). */
const COMMAND_ENV_PROTECTED_KEYS: ReadonlySet<string> = new Set([
  // Command boundary literals.
  "PATH",
  "TMPDIR",
  "LANG",
  "HOME",
  // Mirror of sdk-env.ts PROTECTED_ENV_KEYS (module-private there; cannot be imported).
  "CLAUDE_CODE_OAUTH_TOKEN",
  "ANTHROPIC_API_KEY",
  "ANTHROPIC_AUTH_TOKEN",
  "AGENT_BROWSER_ARGS",
]);

const DEFAULT_IDLE_MS = 5 * 60 * 1000;
const DEFAULT_WALL_MS = 60 * 60 * 1000;
const DEFAULT_BOUNDARY_DEADLINE_MS = 30 * 1000;
const DEFAULT_CHILD_TURN_DEADLINE_MS = 10 * 60 * 1000;
// The single-milestone implement/review iteration budget when the claim omits one, matching
// sdk-executor's DEFAULT_MAX_ITERATIONS (PRD: RUN_MAX_ITERATIONS default 5). Codex carries no
// milestone-scaling served budget yet, so the claim value or this default is the whole cap.
const DEFAULT_MAX_ITERATIONS = 5;
const DEFAULT_MAX_REVISIONS = 3;

// Watchdog/cancel trip reasons (secret-free static strings). "run cancelled" matches the
// runner's terminal cancel wording so a Codex cancel routes identically.
const REASON_IDLE = "codex run idle timeout";
const REASON_WALL = "codex run wall-clock timeout";
const REASON_CANCEL = "run cancelled";
// The bounded implement/review loop's fail-closed exhaustion reason (secret-free static string),
// mirroring sdk-executor's REASON_MAX_ITERATIONS: the loop reached its iteration budget without
// the lead signalling done.
const REASON_MAX_ITERATIONS = "codex run reached its implement/review iteration budget without completing";

/** The reducer's lead-context hook is a NO-OP for Codex: `CodexHarness.readContext`
 *  returns `undefined` (no characterized context RPC yet), so the reducer never attaches a
 *  lead context reading. */
const NOOP_CONTEXT_HOOK: HarnessContextHook = {
  request(): void {},
  get: async (): Promise<undefined> => undefined,
};

/** Derive a positive integer budget from a claim-config value, mirroring sdk-executor's
 *  `positive`: a finite value > 0 is floored, anything else (absent/zero/negative/garbage)
 *  falls back to the default. */
function positiveOr(value: number | undefined, fallback: number): number {
  return typeof value === "number" && value > 0 ? Math.floor(value) : fallback;
}

/** Mirror the shared plan-gate contract: zero explicitly disables revisions, while an
 * absent or invalid claim value falls back to the server default. */
function planMaxRevisionsOf(config: RunContext["config"]): number {
  const value = config?.plan_max_revisions;
  return typeof value === "number" && Number.isFinite(value) && value >= 0
    ? Math.floor(value)
    : DEFAULT_MAX_REVISIONS;
}

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

  /** The immutable auth mode of the bound credential. Exposed so the advice composition can
   *  pick the app-server auth mode (subscription vs api_key) without reaching into the binding. */
  get authMode(): CodexBinding["authMode"] {
    return this.binding.authMode;
  }

  /** The provider-verified subscription account id, or undefined for api_key. Server-owned
   *  and immutable; the app-server login uses it, never the callback's untrusted hint. */
  get chatgptAccountId(): string | undefined {
    return this.binding.authMode === "subscription" ? this.binding.chatgptAccountId : undefined;
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

  /**
   * Build the advice lane's app-server subscription refresh bridge by REUSING the run-lane
   * {@link buildAppServerRefreshBridge}, so the advice lane's app-server refresh is byte-identical
   * to the run lane's — GENERATION-AFTER-DELIVERY. The pinned app-server auth owner retains the
   * SAME operation id across a delivery-failed retry (an HTTP refresh that succeeded but whose
   * token never reached Codex), so a bridge that advanced its observed generation IMMEDIATELY (as
   * {@link refresh} does) would re-send an ALREADY-ADVANCED observed_generation on that retry and
   * the real client's response validation would reject it (`generation <= observed`). Folding
   * pending→committed only on a NEW operation id avoids that. The committed cell is FRESH per
   * bridge, seeded from this binding's generation (the advice lane owns no shared run cell), and
   * `registerToken` redacts every refreshed token. Subscription only (throws for api_key, exactly
   * like the run lane).
   */
  buildSubscriptionRefreshBridge(registerToken: (token: string) => void): CodexSubscriptionRefreshBridge {
    return buildAppServerRefreshBridge(
      this.runId,
      this.client,
      this.binding,
      { value: this.binding.authMode === "subscription" ? this.binding.generation : undefined },
      registerToken,
    );
  }
}

/**
 * The run's ONE shared committed observed-generation holder (PRD #1171 m1). BOTH the run-lane
 * boundary reconcile ({@link buildRunLaneReconcile}) AND the app-server refresh bridge
 * ({@link buildAppServerRefreshBridge}) read and write this SAME cell, so neither keeps a
 * private generation: a during-run app-server refresh advances it and a post-run boundary
 * reconcile picks it up. Because the app-server bridge folds pending→committed ONLY on a NEW
 * operation id (generation-after-delivery), at run-end the cell holds the last
 * CONFIRMED-DELIVERED generation — one behind an undelivered final refresh, not "exactly where
 * the run left it". That is the conservative/safe direction: the server replays or advances
 * from a generation the worker actually installed, never from one that never reached Codex.
 * `value` is the subscription observed generation; `undefined` for api_key (which never refreshes).
 */
export interface CodexCommittedGenerationCell {
  value: number | undefined;
}

/**
 * PRD #1171 m1 (integration): the production app-server subscription refresh bridge. The
 * pinned app-server auth owner ({@link createCodexAppServerAuth}) calls this when Codex asks to
 * re-login a subscription; it forwards the exchange to {@link WorkerClient.refreshCodex} with
 * the SERVER-owned account id, the immutable subscription mode, the SHARED observed generation,
 * the app-server-supplied operation id (RETAINED across ambiguous/lost-delivery retries by the
 * auth owner, which mints a NEW id only after a fully-delivered success), and the caller's
 * cancellation signal (combined with WorkerClient's fixed 8s cap).
 *
 * GENERATION-AFTER-DELIVERY: the freshly-committed generation is held as `pending` and folded
 * into the SHARED committed cell ONLY when a genuinely NEW operation id arrives — proving the
 * prior refresh fully delivered to Codex, since the auth owner reuses the SAME id on a delivery
 * failure. A same-id RETRY therefore replays the OLD committed generation as `observed_generation`,
 * never the not-yet-delivered pending one, so the server replays/advances from the last generation
 * the worker actually installed. The fold never regresses the shared cell (a concurrent boundary
 * reconcile may have advanced it further). Subscription only — an api_key credential builds no bridge.
 */
export function buildAppServerRefreshBridge(
  runId: string,
  client: Pick<WorkerClient, "refreshCodex">,
  binding: CodexBinding,
  committed: CodexCommittedGenerationCell,
  registerToken: (token: string) => void,
): CodexSubscriptionRefreshBridge {
  if (binding.authMode !== "subscription") {
    // Fail-closed guard: an api_key credential can never refresh (mirrors the reconcile).
    throw new Error("codex app-server refresh bridge requires a subscription binding");
  }
  const { capability, chatgptAccountId } = binding;
  let lastSeenOperationId: string | undefined;
  let pendingGeneration: number | undefined;
  return {
    refresh: async ({ operationId, signal }): Promise<{ accessToken: string; accountId: string }> => {
      // A genuinely NEW operation id is the auth owner's DELIVERY signal for the prior refresh:
      // fold pending → committed (never regressing the shared cell). A same-id retry keeps the
      // OLD committed generation as the observed value it sends.
      if (operationId !== lastSeenOperationId) {
        if (pendingGeneration !== undefined && (committed.value === undefined || pendingGeneration > committed.value)) {
          committed.value = pendingGeneration;
        }
        lastSeenOperationId = operationId;
      }
      const observedGeneration = committed.value;
      if (observedGeneration === undefined) {
        // A subscription binding always carries a generation; its absence is a protocol fault.
        throw new Error("codex app-server refresh has no observed generation");
      }
      const res = await client.refreshCodex(
        runId,
        { capability, operation_id: operationId, observed_generation: observedGeneration },
        { authMode: "subscription", chatgptAccountId },
        signal,
      );
      registerToken(res.access_token);
      // Held, NOT committed: the shared cell only advances once a NEW operation id proves this
      // token reached Codex (see fold above).
      pendingGeneration = res.generation;
      return { accessToken: res.access_token, accountId: res.chatgpt_account_id };
    },
  };
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
 * `committed` is the run's ONE shared observed-generation cell (PRD #1171 m1): the same holder
 * the app-server refresh bridge reads/writes, so a subscription generation advanced by a
 * during-run app-server refresh carries into the post-run boundary reconciles and vice versa —
 * neither keeps a private copy. Omitted (the differential/isolated callers) it defaults to a
 * fresh per-call cell seeded from the binding, preserving the standalone semantics.
 *
 * Exported (with {@link CodexAdviceCredentialBridge}) so the differential test pins BOTH against
 * drift.
 */
export function buildRunLaneReconcile(
  runId: string,
  client: Pick<WorkerClient, "releaseCodex" | "refreshCodex">,
  binding: CodexBinding,
  registerToken: (token: string) => void,
  committed: CodexCommittedGenerationCell = { value: binding.authMode === "subscription" ? binding.generation : undefined },
): ReconcileBeforeBoundary {
  let reconcileOperationId: string | undefined;
  return async (_request: BoundaryRequest, signal: AbortSignal): Promise<ReconcileOutcome> => {
    try {
      if (binding.authMode === "subscription") {
        if (committed.value === undefined) {
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
            observed_generation: committed.value,
          },
          { authMode: "subscription", chatgptAccountId: binding.chatgptAccountId },
          signal,
        );
        registerToken(res.access_token);
        committed.value = res.generation; // durable commit → advance the SHARED cell BEFORE the permit
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
 * `bridge` and authenticated over the app-server login RPC. If the bridge fails closed
 * (authority unavailable) the release throws and NO advice root is built — the failure
 * propagates rather than degrading to an un-credentialed root. The auth owner is the ONLY
 * credential seam fed into the harness — NO workspace/registry/broker, no env credential.
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
  // Register EVERY advice token — the initial release AND any subscription refresh — with the
  // harness redactor so neither can ride a log line verbatim. The advice lane has no separate
  // per-worker eviction, so over-retention is the safe direction (mirrors the run lane's redactor
  // registration; this closes the advice-lane redactor gap for refreshed tokens).
  const registerToken = (token: string): void => {
    if (token) log.addSecret(token);
  };
  // Release the initial committed token (fail-closed: a release throw propagates and NO root is
  // built). It seeds the app-server login credential; the token flows over the login RPC, never env.
  const initial = await bridge.release(signal);
  registerToken(initial);
  const appServerAuth = createCodexAppServerAuth(buildAdviceAuthConfig(bridge, initial, registerToken));
  return new CodexAdviceHarness({ launchRoot, provider, appServerAuth, log });
}

/** Build the advice lane's app-server auth config from the released initial token. Subscription
 *  REUSES the run-lane {@link buildAppServerRefreshBridge} (via
 *  {@link CodexAdviceCredentialBridge.buildSubscriptionRefreshBridge}), so the advice lane's
 *  app-server refresh is byte-consistent with the run lane BY CONSTRUCTION — generation-after-
 *  delivery, not the immediate-advance {@link CodexAdviceCredentialBridge.refresh} primitive that
 *  a delivery-failed same-op retry would break. api_key builds a no-refresh key config. Unit-only
 *  (no live caller) — it keeps the advice lane fail-closed. */
function buildAdviceAuthConfig(
  bridge: CodexAdviceCredentialBridge,
  initial: string,
  registerToken: (token: string) => void,
): CodexAppServerAuthConfig {
  if (bridge.authMode !== "subscription") {
    return { mode: "api_key", apiKey: initial };
  }
  const accountId = bridge.chatgptAccountId;
  if (accountId === undefined) {
    // A subscription bridge always exposes its account id; absence is a protocol fault.
    throw new Error("codex advice subscription bridge has no account id");
  }
  return {
    mode: "subscription",
    initial: { accessToken: initial, accountId },
    bridge: bridge.buildSubscriptionRefreshBridge(registerToken),
  };
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

// ─── the production MCP/forge/memory/findings/skill tool-handler map ────────────
export interface CodexToolHandlerDeps {
  readonly client: WorkerClient;
  readonly runId: string;
  readonly log: Logger;
  /** The run's live-stream emit (the findings tool emits a `finding` card through it). */
  readonly emit: (msg: EmittedMessage) => void;
  /** The run's GRANTED skills (ctx.skills). The Skill handler returns the sanitized
   *  body/description of the requested granted skill; the broker has ALREADY gated the
   *  call on `allowedSkills` before the handler is reached. */
  readonly skills: readonly ClaimSkill[];
}

/**
 * Build the ONE per-run tool-handler map the root broker AND every delegated-child broker
 * key their `mcp` dispatch against (broker.dispatchMcp does `toolHandlers.get(name)` by the
 * RAW callback name and DENIES `denied_tool` when absent). Built ONCE per run() — NOT per
 * turn — because the forge and memory handlers hold per-RUN budget counters
 * (MAX_FORGE_CALLS_PER_RUN, the 5-writes/run memory cap) that a per-turn rebuild would
 * reset. It REUSES the Claude raw handler factories verbatim (makeMemoryToolHandlers /
 * makeFindingsToolHandlers / makeForgeToolHandlers), so the authority + redaction + budget
 * rules are byte-identical to the Claude lane; only the Skill delivery is Codex-specific.
 *
 * Keys:
 *   - `mcp__memory__save_memory`            → saveMemory
 *   - `mcp__findings__report_incidental_issue` → reportIncidentalIssue
 *   - each `mcp__forge__<tool>`             → the matching ForgeToolHandlers method
 *   - `Skill` (reads the skill name from args) → the Codex Skill handler. This is the ONLY
 *     skill key: render grants expose the canonical `"Skill"` tool to the model (the requested
 *     skill rides in args and the broker gates it on `allowedSkills`), so a per-skill
 *     `mcp__skills__<name>` key would be unreachable under those grants and its key/args
 *     mismatch a latent inconsistency — it is deliberately NOT registered.
 */
export function buildCodexToolHandlers(deps: CodexToolHandlerDeps): ReadonlyMap<string, ToolHandler> {
  const { client, runId, log, emit, skills } = deps;
  const map = new Map<string, ToolHandler>();

  // memory (one tool; ROOT-only — render never grants it to a subagent, and the broker's
  // allowedTools gate stops a subagent reaching this handler regardless).
  const memHandlers: MemoryToolHandlers = makeMemoryToolHandlers({ client, runId, log });
  map.set(
    memoryToolNames()[0]!,
    (args) => memHandlers.saveMemory(args as Parameters<MemoryToolHandlers["saveMemory"]>[0]),
  );

  // findings (granted to the lead AND every subagent — stays in the subagent base).
  const findingsHandlers: FindingsToolHandlers = makeFindingsToolHandlers({ client, runId, emit, log });
  map.set(
    reportIncidentalIssueToolName(),
    (args) => findingsHandlers.reportIncidentalIssue(args as Parameters<FindingsToolHandlers["reportIncidentalIssue"]>[0]),
  );

  // forge (8 tools; the 6 reads + 2 writes share the ONE per-run budget in makeForgeToolHandlers).
  // An explicit [suffix, handler] table so a rename cannot silently misalign the map; the
  // qualified keys equal forgeToolNames() by construction (same FORGE_SERVER_NAME + suffixes).
  const forgeHandlers: ForgeToolHandlers = makeForgeToolHandlers({ client, runId, log });
  const forgeEntries: ReadonlyArray<readonly [suffix: string, handler: ToolHandler]> = [
    ["get_issue", (a) => forgeHandlers.getIssue(a as Parameters<ForgeToolHandlers["getIssue"]>[0])],
    ["list_issues", (a) => forgeHandlers.listIssues(a as Parameters<ForgeToolHandlers["listIssues"]>[0])],
    ["get_merge_request", (a) => forgeHandlers.getMergeRequest(a as Parameters<ForgeToolHandlers["getMergeRequest"]>[0])],
    ["get_pipeline_jobs", (a) => forgeHandlers.getPipelineJobs(a as Parameters<ForgeToolHandlers["getPipelineJobs"]>[0])],
    ["latest_pipeline", (a) => forgeHandlers.latestPipeline(a as Parameters<ForgeToolHandlers["latestPipeline"]>[0])],
    ["list_issue_label_events", (a) => forgeHandlers.listIssueLabelEvents(a as Parameters<ForgeToolHandlers["listIssueLabelEvents"]>[0])],
    ["reply_mr_thread", (a) => forgeHandlers.replyMrThread(a as Parameters<ForgeToolHandlers["replyMrThread"]>[0])],
    ["resolve_mr_thread", (a) => forgeHandlers.resolveMrThread(a as Parameters<ForgeToolHandlers["resolveMrThread"]>[0])],
  ];
  for (const [suffix, handler] of forgeEntries) map.set(`mcp__${FORGE_SERVER_NAME}__${suffix}`, handler);

  // Skill — Codex-SPECIFIC delivery. Skills have NO Claude runtime tool analogue: Claude
  // enables them through the SDK `skills` plugin list (markdown loaded by the SDK), not a
  // callback handler, so there is nothing to reuse here. The broker gates the call on
  // `allowedSkills` (so the name reaching this handler is already a granted, kebab-case
  // skill), then requires a handler keyed by the raw callback name; this returns the granted
  // skill's description + body as the tool result. Keyed ONLY under the canonical "Skill"
  // (the requested skill rides in args) — that is the tool render grants actually expose. A
  // per-skill `mcp__skills__<name>` key would be unreachable under those grants and its
  // key/args mismatch a latent inconsistency, so it is deliberately not registered.
  const skillByName = new Map<string, ClaimSkill>();
  for (const s of skills) skillByName.set(s.name, s);
  const deliverSkill = (name: string): ToolTextResultLike => {
    const skill = skillByName.get(name);
    if (skill === undefined || skill.body.trim().length === 0) {
      // Bounded ack naming the skill (never "no handler wired", never a throw). `name` is a
      // granted, broker-validated skill name, so it is safe to echo.
      return asText(`skill "${name}" is granted but its content is not available in this run; proceed without it.`);
    }
    return asText(`${skill.description}\n\n${skill.body}`);
  };
  map.set("Skill", (args) => {
    const name = firstSkillName(args);
    return Promise.resolve(
      name === undefined ? asText("a skill invocation requires a skill name.") : deliverSkill(name),
    );
  });

  return map;
}

/** The single-text-block tool-result shape the reused Claude handlers (and the Skill
 *  handler above) return; the broker forwards it as the callback `output`. Mirrors
 *  tool-evidence.ts's ToolTextResult without re-importing the type. */
type ToolTextResultLike = ReturnType<typeof asText>;

/** Create one worker-owned, runner-group-accessible directory without following a
 * final symlink. The private runner-owned epoch roots live below these shared
 * directories; the credential-free store remains worker-owned beside them. */
async function ensureCodexSharedDirectory(dir: string): Promise<void> {
  try {
    await fs.mkdir(dir, { mode: 0o2770 });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
  }
  const handle = await fs.open(dir, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
  try {
    const before = await handle.stat();
    if (!before.isDirectory() || before.uid !== WORKER_UID || before.gid !== RUNNER_UID) {
      throw new Error("Codex shared data directory has an unexpected owner or group");
    }
    await handle.chmod(0o2770);
    const after = await handle.stat();
    if ((after.mode & 0o7777) !== 0o2770) {
      throw new Error("Codex shared data directory has an unexpected mode");
    }
  } finally {
    await handle.close();
  }
}

async function prepareCodexRunHome(homeRoot: string): Promise<void> {
  if (!path.isAbsolute(homeRoot) || path.resolve(homeRoot) !== homeRoot || homeRoot === path.parse(homeRoot).root) {
    throw new Error("Codex run HOME must be a canonical absolute non-root path");
  }
  if (process.getuid?.() !== WORKER_UID) {
    throw new Error("Codex run HOME must be prepared by the worker identity");
  }
  await ensureCodexSharedDirectory(homeRoot);
  await ensureCodexSharedDirectory(path.join(homeRoot, "codex-data"));
}

/** The requested skill name from a `Skill` callback's args (`skill` or `name`), else
 *  undefined. Mirrors the broker's own skill-name extraction (firstStrField). */
function firstSkillName(args: unknown): string | undefined {
  if (args === null || typeof args !== "object" || Array.isArray(args)) return undefined;
  const record = args as Record<string, unknown>;
  for (const key of ["skill", "name"]) {
    const v = record[key];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return undefined;
}

// ─── Injectable seams (production defaults; tests inject fakes) ──────────────────
export interface CodexExecutorDeps {
  /** Adapts the real M3a launcher for a PROVIDER root; a test injects a fake returning a
   *  scripted in-memory transport (NO real Codex). Receives the harness's launch spec AND the
   *  immutable app-server auth mode; the credential NO LONGER rides into the launcher env — it
   *  flows over the app-server login RPC (the harness's auth session), so the launcher emits the
   *  production config and injects no provider credential var. */
  readonly launchProviderRoot?: (spec: CodexLaunchRootSpec, authMode: CodexAppServerAuthMode) => Promise<CodexLaunchRootResult>;
  /** M3b packaged-proof seam. It preserves the real launcher and account/login/start,
   * selecting only a validated in-container HTTP loopback provider with WebSockets off.
   * Production leaves this absent and keeps pinned Codex's built-in provider. */
  readonly appServerAuthOpenAIBaseUrlForTest?: string;
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
  /** The credential-free session store (default: the real {@link CodexSessionStore}).
   *  `persist` is used before every provider-root reap and at terminal to capture the
   *  live session subset; `remove` is NO LONGER called by the executor (the runner's
   *  runHome lifecycle owns store removal — preserve on park, remove on terminal). */
  readonly sessionStore?: Pick<typeof CodexSessionStore, "adopt" | "inspect" | "remove" | "persist">;
  /** The shared tool-provisioning step (PRD #18). Production uses the real
   *  {@link provisionRunTools}; a unit test injects a stub so no real devbox/nix install
   *  runs. The resolved `toolEnv` is folded into the credential-free command env
   *  (bounded by PROVISION_ENV_ALLOWLIST; the boundary literals are protected). */
  readonly provisionRunTools?: typeof provisionRunTools;
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

// ─── The per-epoch provider bundle + the shared per-run context (m4) ────────────
/**
 * PRD #1171 m4: the SHARED per-run state every provider epoch is built from. Closed over ONCE in
 * `run()` and passed to every {@link CodexExecutor.startProviderEpoch} so a recreated epoch reuses
 * (never rebuilds) the released-token set, the committed-generation cell, the tool-handler map, the
 * reconcile + eviction closures, provisioning-derived command env, the effect launcher and the
 * boundary seams. Only the per-epoch registry/safety/harness/effect-roots + a fresh credential +
 * an owned HOME are minted anew per epoch.
 */
interface EpochSharedContext {
  readonly provider: CodexProviderConfig;
  readonly binding: CodexBinding;
  readonly worktreePath: string;
  readonly storeDir: string;
  readonly homeRoot: string;
  readonly boundaryDeadlineMs: number;
  readonly childTurnDeadlineMs: number;
  readonly commandEnv: NodeJS.ProcessEnv;
  readonly screenPolicy: ScreenPolicy;
  readonly toolHandlers: ReadonlyMap<string, ToolHandler>;
  readonly registerToken: (token: string) => void;
  readonly committedGeneration: CodexCommittedGenerationCell;
  readonly launchEffectRoot: (spec: CodexEffectLaunchSpec, deadlineMs?: number) => Promise<CodexRootHandle>;
  readonly spawnBoundaryRoot: SpawnRootSeam;
  readonly boundaryProcessSpawner: SpawnBoundaryProcessSeam;
  readonly reconcile: ReconcileBeforeBoundary;
  readonly evictTokens: () => void;
}

/**
 * PRD #1171 m4: ONE fresh provider epoch — its own {@link ExecutionRegistry} (a distinct
 * local-execution epoch), safety facade, provider {@link CodexHarness}, fileop/command effect
 * roots, and a per-epoch owned HOME (`codexHome`) that adopts the credential-free session subset
 * from the shared store. `run()` holds the CURRENT epoch and hot-swaps `this.safety` to it so the
 * runner's durability sinks always reap the LIVE provider root; a cooperative-checkpoint reap and
 * plan approval REPLACE it with a fresh epoch via {@link CodexExecutor.startProviderEpoch}.
 */
interface ProviderEpoch {
  readonly registry: ExecutionRegistry;
  readonly safety: CodexExecutionSafety;
  readonly harness: CodexHarness;
  readonly fileopHandle: FileopHelperHandle;
  readonly spawnCommand: SpawnCommandSeam;
  readonly fileop: FileopClient;
  readonly codexHome: string;
  /** The session id a fresh provider root resumes (the prior epoch's latest thread, or the
   *  incoming `ctx.sessionId` for a cross-worker resume; undefined ⇒ start a fresh session). */
  readonly resumeSessionId: string | undefined;
  readonly buildPhaseBroker: (phase: "plan" | "implement", signal?: AbortSignal) => CodexCallbackBroker;
  /** Persist THIS epoch's credential-free session subset into the shared store (best-effort). */
  persistSession(): Promise<void>;
  /** Full teardown of an ABANDONED epoch on recreation (quiesce + reap + disposeTools the
   *  registry, WITHOUT the token-eviction hook, then close the harness/fileop). */
  dispose(): Promise<void>;
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
  private readonly sessionStore: Pick<typeof CodexSessionStore, "adopt" | "inspect" | "remove" | "persist">;

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
    // The packaged-only loopback seam must override both halves of the app-server
    // provider selection. config.toml names the custom HTTP provider in launcher.ts,
    // while thread/start carries this object and would otherwise override it back to
    // the production `openai` provider, silently bypassing the in-container fake.
    const provider = this.deps.appServerAuthOpenAIBaseUrlForTest === undefined
      ? this.opts.provider
      : {
          ...this.opts.provider,
          name: CODEX_M3B_LOOPBACK_PROVIDER_NAME,
          baseUrl: this.deps.appServerAuthOpenAIBaseUrlForTest,
        };
    const worktreePath = ctx.worktreePath;
    const storeDir = path.join(this.homeRoot, "codex-session-store");
    const boundaryDeadlineMs = this.deps.boundaryDeadlineMs ?? DEFAULT_BOUNDARY_DEADLINE_MS;

    if (uidSplitActive()) await assertCommandWorktreePosture(worktreePath);

    // (C) SHARED across every provider epoch: each FRESH provider token released this run is
    // registered with the logger's secret set (`addSecret`) BEFORE use; without a matching
    // `removeSecret` a long-lived worker's secret set would grow permanently. A recreated epoch
    // releases a NEW token into this SAME set, and every epoch's tokens are evicted together at
    // the terminal (over-retention is the safe direction — never mid-run, where a late log line
    // could still carry a token). Idempotent per token so a token reused across paths registers once.
    const releasedTokens = new Set<string>();
    const registerToken = (token: string): void => {
      if (!token || releasedTokens.has(token)) return;
      this.log.addSecret(token);
      releasedTokens.add(token);
    };
    // (m1) The run's ONE shared committed observed-generation cell, SHARED across every epoch: the
    // app-server refresh bridge (during a turn) and the boundary reconcile (at a sink) of EVERY
    // epoch read/write it, so a generation advanced under epoch N carries into epoch N+1. Neither
    // keeps a private copy. Subscription seeds it from the binding; api_key never refreshes.
    const committedGeneration: CodexCommittedGenerationCell = {
      value: binding.authMode === "subscription" ? binding.generation : undefined,
    };

    // A SINGLE terminal try/finally spanning ALL provider epochs. `epoch` holds the CURRENT
    // provider epoch — undefined until the first startProviderEpoch succeeds, so a setup failure
    // BEFORE any epoch is built still reaches the finally to evict every registered token. Each
    // cooperative-checkpoint reap and the plan approval REPLACES it with a fresh epoch (a new
    // registry + safety facade + harness + fileop/command roots on a fresh credential + a new
    // local-execution epoch + the adopted session), disposing the old one. `epochIndex` numbers
    // each epoch's local-execution epoch and its own owned HOME; `provisionDir` is the ONE per-run
    // provisioning dir the finally removes.
    let epoch: ProviderEpoch | undefined;
    let epochIndex = 0;
    let provisionDir: string | undefined;
    try {
      // Provision the run's tool packages ONCE (PRD #18; SHARED across every epoch — the forge/
      // memory budget counters a per-epoch rebuild would reset). A tier-1 failure throws
      // REASON_PROVISION_FAILED, which this try's finally then cleans up after. Injected via
      // `deps.provisionRunTools` so a unit test stubs it. The provisioning HOME + root are SHARED
      // worker-lifetime paths (not the per-run codex home), matching sdk-executor.
      const provisionRunToolsFn = this.deps.provisionRunTools ?? provisionRunTools;
      const provisioned = await provisionRunToolsFn(ctx, {
        provisionRoot: path.join(path.dirname(this.homeRoot), "provision"),
        homeDir: this.homeRoot,
        log: this.log,
      });
      provisionDir = provisioned.provisionDir; // removed in the terminal finally (best-effort)

      // The SCRUBBED command-identity env — NOTHING from process.env (cross-root credential
      // boundary). The FIXED toolchain+system PATH comes first; the run's allowlisted provisioned
      // toolEnv (PATH appended, NIX_SSL_CERT_FILE/LOCALE_ARCHIVE folded) rides in without ever
      // overwriting a boundary literal or a PROTECTED_ENV_KEYS member. Built ONCE and shared by
      // every epoch's command + fileop effect surfaces (the roots are per-epoch; the env is not).
      const commandEnv: NodeJS.ProcessEnv = buildCommandEnv(
        this.deps.commandTmpdir ?? "/tmp",
        provisioned.toolEnv,
      );

      // The ONE per-run MCP/forge/memory/findings/skill handler map, SHARED by EVERY epoch's root
      // + child brokers. Built HERE (once per run(), NOT per epoch/turn) because the forge + memory
      // handlers hold per-RUN budget counters a rebuild would reset. render.ts decides WHICH names
      // each role may invoke (root gets forge + memory; a subagent never inherits memory); this map
      // only supplies the handler an already-authorized call routes to.
      const toolHandlers = buildCodexToolHandlers({
        client: this.opts.client,
        runId: ctx.runId,
        log: this.log,
        emit: (msg) => ctx.emit(msg),
        skills: ctx.skills ?? [],
      });

      // The low-level effect launcher, boundary seams and per-sink reconcile/eviction closures —
      // all SHARED across epochs (each epoch's registry-bound safety facade + effect roots are
      // built from these). The `spawnBoundaryRoot` legacy seam is unused by the runner (which
      // drives `spawnBoundaryProcess`); it stays a fail-closed reject stub. The reconcile + eviction
      // closures write the SHARED released-token set and committed-generation cell, so credential
      // state is continuous across a recreation.
      const launchEffectRoot = this.deps.launchEffectRoot ?? ((spec: CodexEffectLaunchSpec, deadlineMs?: number) =>
        launchCodexEffectRoot(spec, deadlineMs === undefined ? {} : { deadlines: { started: deadlineMs } }));
      const spawnBoundaryRoot: SpawnRootSeam =
        this.deps.spawnBoundaryRoot ??
        ((): Promise<RegisteredRoot> =>
          Promise.reject(new Error("codex boundary-action spawn seam is not wired (the runner drives spawnBoundaryProcess)")));
      const boundaryProcessSpawner = makeBoundaryProcessSpawner(launchEffectRoot);
      const reconcile = this.makeBoundaryReconcile(ctx.runId, registerToken, committedGeneration);
      // (C, F1) Terminal eviction of tokens released by the POST-RUN sink reconciles. The runner
      // calls safety.dispose after the last durability sink — by which point run()'s finally has
      // already evicted+cleared the DURING-run tokens — so the FINAL epoch's onDispose evicts only
      // the post-run tokens. `clear()` makes it idempotent. SHARED across epochs, but ONLY the
      // FINAL epoch's safety.dispose ever invokes it (an abandoned epoch's dispose tears down its
      // registry directly, WITHOUT this hook, so a mid-run recreation never evicts a live token).
      const evictTokens = (): void => {
        for (const token of releasedTokens) this.log.removeSecret(token);
        releasedTokens.clear();
      };

      // The per-run state every epoch is built from. Everything here is SHARED and closed over
      // ONCE; startProviderEpoch mints only the per-epoch registry/safety/harness/effect roots +
      // its own fresh credential + owned HOME on top of it.
      const shared: EpochSharedContext = {
        provider,
        binding,
        worktreePath,
        storeDir,
        homeRoot: this.homeRoot,
        boundaryDeadlineMs,
        childTurnDeadlineMs: this.deps.childTurnDeadlineMs ?? DEFAULT_CHILD_TURN_DEADLINE_MS,
        commandEnv,
        screenPolicy: { dockerWired: false },
        toolHandlers,
        registerToken,
        committedGeneration,
        launchEffectRoot,
        spawnBoundaryRoot,
        boundaryProcessSpawner,
        reconcile,
        evictTokens,
      };

      // Build the FIRST provider epoch (epoch 0): eager fresh credential release (fail-closed),
      // the registered fileop/command root, the app-server auth session on that token, the safety
      // facade, the phase brokers and the provider harness. `this.safety` is hot-swapped to the
      // current epoch's facade so the runner's durability sinks (which re-read executor.safety
      // fresh on every call) always reap the LIVE provider root. epoch 0 resumes ctx.sessionId (a
      // cross-worker resume) or starts a fresh session. `lastSessionId` tracks the most recent
      // turn's session id so a recreated epoch resumes the RIGHT thread.
      let lastSessionId = ctx.sessionId ?? undefined;
      epoch = await this.startProviderEpoch(ctx, shared, lastSessionId, epochIndex);
      this.safety = epoch.safety;

      const reducer = new RunTurnReducerImpl(NOOP_CONTEXT_HOOK);
      const idleMs = this.deps.idleMs ?? (ctx.config?.idle_timeout_seconds ? ctx.config.idle_timeout_seconds * 1000 : DEFAULT_IDLE_MS);
      const wallMs = this.deps.wallMs ?? (ctx.config?.run_timeout_seconds ? ctx.config.run_timeout_seconds * 1000 : DEFAULT_WALL_MS);

      // Plan → approval gate, exactly like SdkExecutor (fail-closed): a pre-approved resume
      // skips the planning turn and the gate.
      const preApproved = ctx.planApproved === true && !!ctx.approvedPlan?.trim();
      if (!preApproved && ctx.gatePlan) {
        // The PLAN turn(s) run under PLAN-phase grants: the broker denies every file write
        // (write_denied_in_plan) and child subagents inherit plan-phase grants, so nothing
        // mutates the worktree before the plan is approved. All revise iterations reuse this
        // epoch's plan-phase broker (the implement epoch, below, re-points to implement).
        let planResult = await this.driveCodexTurn(ctx, epoch.harness, reducer, "plan", this.planPrompt(ctx), epoch.resumeSessionId, idleMs, wallMs, epoch.buildPhaseBroker);
        let planMd = planResult.plan;
        if (planMd === undefined || planMd.trim().length === 0) {
          throw new Error("codex plan turn produced no plan");
        }
        let verdict = await ctx.gatePlan(planMd, planResult.milestones);
        const maxRevisions = planMaxRevisionsOf(ctx.config);
        let revisions = 0;
        while (verdict.kind === "revise") {
          const feedback = verdict.feedback;
          ctx.emit({ kind: "plan_feedback", agent: "worker", payload: { feedback } });
          if (revisions >= maxRevisions) {
            throw new Error("codex plan revision budget exhausted");
          }
          revisions++;
          ctx.emit({ kind: "plan_revising", agent: "worker", payload: { round: revisions } });
          planResult = await this.driveCodexTurn(ctx, epoch.harness, reducer, "plan", buildRevisePlanPrompt(feedback), epoch.resumeSessionId, idleMs, wallMs, epoch.buildPhaseBroker);
          planMd = planResult.plan;
          if (planMd === undefined || planMd.trim().length === 0) {
            throw new Error("codex plan turn produced no plan on revision");
          }
          verdict = await ctx.gatePlan(planMd, planResult.milestones);
        }
        if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
        if (verdict.kind === "cancel") throw new Error(REASON_CANCEL);

        // NEW-ROOT RESUME at plan approval. The plan turn's provider root holds a live credential
        // it does not need during the approval wait, so: persist the credential-free session,
        // recreate a fresh epoch (fresh credential + new local-execution epoch + adopted session)
        // that resumes the plan thread, hot-swap `this.safety` to it, then fully dispose the old
        // epoch. Implement now runs on a fresh credential/root — the plan root's credential was
        // released for the duration of the (possibly long) approval.
        if (planResult.sessionId) lastSessionId = planResult.sessionId;
        await epoch.persistSession();
        const old = epoch;
        epoch = await this.startProviderEpoch(ctx, shared, lastSessionId, ++epochIndex);
        this.safety = epoch.safety;
        await old.dispose();
      }

      // Implement ⇄ review loop (bounded). Each iteration drives ONE implement turn under
      // IMPLEMENT-phase grants on the CURRENT epoch. Off the reducer's per-turn ReducedTurnResult:
      //   - carry `latestProgress`, overwriting ONLY when the turn reported progress, so a quiet
      //     turn keeps the last known progress rather than blanking it (mirrors sdk-executor);
      //   - carry `lastSessionId` off each turn so a recreated epoch resumes the RIGHT thread;
      //   - a `checkpoint` that is NOT `done` is a cooperative milestone boundary → persist the
      //     live session, reap the CURRENT epoch (checkpoint reap:true routes through
      //     this.safety = epoch.safety), then NEW-ROOT RESUME: recreate a fresh epoch on a fresh
      //     credential + new local-execution epoch + the adopted session, and drive the next
      //     implement turn on the NEW root — the reaped root's registry is permanently closed, so
      //     the next turn REQUIRES a fresh registry/provider root;
      //   - `done` (a root signal_done folded via the m2 signal-routing frame) ends the loop;
      //   - reaching the bounded iteration budget fails closed (REASON_MAX_ITERATIONS);
      //   - otherwise an iteration-boundary fallback checkpoint (reap:false — credential-free, does
      //     NOT reap the provider → NO recreation; the SAME epoch drives the next turn), then continue.
      const maxIterations = positiveOr(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);
      let latestProgress: ReducedTurnResult["progress"];
      let iteration = 0;
      for (;;) {
        iteration++;
        const result = await this.driveCodexTurn(ctx, epoch.harness, reducer, "implement", this.implementPrompt(ctx), epoch.resumeSessionId, idleMs, wallMs, epoch.buildPhaseBroker);
        if (result.sessionId) lastSessionId = result.sessionId;
        // Only overwrite when THIS turn reported progress (a quiet turn keeps the last value).
        if (result.progress) latestProgress = result.progress;
        // A cooperative checkpoint that did not also finish: persist BEFORE the reap (so the live
        // session is captured before the provider root dies), reap the CURRENT epoch's roots, then
        // recreate a fresh provider epoch and continue on the NEW root. A turn that is BOTH
        // checkpoint and done still terminates (the done break wins below, reached because this
        // branch is skipped when done).
        if (result.checkpoint && !result.done) {
          await epoch.persistSession();
          await ctx.checkpoint?.({ reap: true, progress: latestProgress });
          const old = epoch;
          epoch = await this.startProviderEpoch(ctx, shared, lastSessionId, ++epochIndex);
          this.safety = epoch.safety;
          await old.dispose();
          continue;
        }
        if (result.done) break;
        if (iteration >= maxIterations) throw new Error(REASON_MAX_ITERATIONS);
        // Iteration-boundary fallback checkpoint (Decision 10b analogue): fetch-back WITHOUT
        // reaping so a backgrounded dev server the lead means to reuse survives, and the SAME
        // provider root/epoch drives the next turn (no recreation). Best-effort.
        await ctx.checkpoint?.({ reap: false, progress: latestProgress });
      }

      return { branch: ctx.branch };
    } finally {
      // Terminal (m4 F1). Capture the FINAL epoch's credential-free session into the store so a
      // park/preserve resume can adopt it (the runner's runHome lifecycle — preserve on park,
      // remove on terminal — now OWNS store removal; the executor only ever persists, NEVER
      // removes). Then tear down the final epoch: under deferRegistryTeardown leave its registry
      // ALIVE for the runner's post-run durability sinks (which reap the provider root and then
      // safety.dispose it), else BACKSTOP the registry teardown here. Intermediate epochs were
      // already fully disposed at their recreation. `epoch` is undefined only if the FIRST
      // startProviderEpoch threw before building an epoch — the token eviction below still runs.
      if (epoch) {
        await epoch.persistSession();
        await this.tearDownEpoch(epoch.registry, epoch.harness, epoch.fileopHandle, boundaryDeadlineMs, !this.deps.deferRegistryTeardown);
      }
      // (C) Evict the DURING-run released tokens (every epoch's provider-root launches + refreshes)
      // from the logger's secret set — AFTER the harness/transport is closed above, so no late log
      // line can still carry a token. `removeSecret` is reference-counted. `clear()` so the terminal
      // dispose hook (which evicts the POST-RUN sink tokens) does not double-remove these: under
      // deferRegistryTeardown this finally runs BEFORE the post-run sinks, so those sink-released
      // tokens are evicted by the onDispose hook wired into the FINAL epoch's safety.dispose.
      for (const token of releasedTokens) this.log.removeSecret(token);
      releasedTokens.clear();
      // Remove the per-run provisioning dir (the synthesized devbox.json + profile symlinks).
      // The nix STORE is global (on the data volume), NOT here, so this never evicts the
      // warm-start cache. Best-effort, mirroring sdk-executor. Absent ⇒ nothing was provisioned.
      if (provisionDir) await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  // ─── the per-epoch provider bundle (m4: new-root resume) ──────────────────────
  /**
   * Build ONE fresh provider epoch: a NEW {@link ExecutionRegistry} (a distinct local-execution
   * epoch), a FRESHLY-released committed credential, the app-server auth session on that token,
   * the registry-bound safety facade, the fileop/command effect roots, the phase-correct broker
   * builder + delegation runner, and the provider {@link CodexHarness} that adopts the
   * credential-free session subset into a per-epoch owned HOME. Everything per-run (the released-
   * token set, the committed-generation cell, the tool-handler map, the reconcile + eviction
   * closures, provisioning) is SHARED via {@link EpochSharedContext}; this only mints what a fresh
   * provider root needs. Returns a self-disposing {@link ProviderEpoch}.
   *
   * A recreated provider root REQUIRES a brand-new registry: after a durability boundary the prior
   * registry is permanently `"closed"` and denies `reserveLaunch("provider")` forever, and the
   * app-server auth session cannot move between transports — so both are minted afresh here.
   *
   * On ANY failure mid-build the partial epoch is best-effort torn down before rethrowing, so a
   * failed recreation never leaks a half-built registry/harness/fileop.
   */
  private async startProviderEpoch(
    ctx: RunContext,
    shared: EpochSharedContext,
    resumeSessionId: string | undefined,
    epochIndex: number,
  ): Promise<ProviderEpoch> {
    const {
      provider, binding, worktreePath, storeDir, homeRoot, boundaryDeadlineMs, childTurnDeadlineMs,
      commandEnv, screenPolicy, toolHandlers, registerToken, committedGeneration, launchEffectRoot,
      spawnBoundaryRoot, boundaryProcessSpawner, reconcile, evictTokens,
    } = shared;

    // The real launcher needs a worker-owned shared run HOME and codex-data parent: the
    // credential-free session store is worker-owned, while each child epoch is runner-owned
    // 0700 and removed as runner after its supervisor proves drained disposal. Injected
    // launchProviderRoot tests own their synthetic filesystem and skip this production step.
    if (this.deps.launchProviderRoot === undefined) {
      await prepareCodexRunHome(homeRoot);
    }

    // Each epoch gets its OWN owned data root / codexHome (M3a fresh-home semantics), so a
    // recreated provider root never inherits the prior root's auth/cache material; it adopts the
    // credential-free session subset from the SHARED store instead.
    const ownedDataRoot = path.join(homeRoot, "codex-data", `epoch-${epochIndex}`);
    const codexHome = path.join(ownedDataRoot, "codex");
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(epochIndex));
    // The safety facade is bound to THIS registry but carries the SHARED reconcile + eviction
    // closures (so credential/generation state is continuous across epochs). Only the FINAL
    // epoch's safety.dispose ever runs evictTokens (an abandoned epoch's dispose tears its
    // registry down directly, never through this facade).
    const safety = createCodexExecutionSafety(registry, spawnBoundaryRoot, reconcile, evictTokens, boundaryProcessSpawner);

    let harness: CodexHarness | undefined;
    let fileopHandle: FileopHelperHandle | undefined;
    try {
      // Release a FRESH committed credential for THIS provider root (fail-closed: a
      // capability/ownership loss rejects, never degrades to an un-credentialed root). Registered
      // with the redactor + tracked for terminal eviction BEFORE any use; it seeds the app-server
      // login (the token flows over `account/login/start`, NEVER the launcher env).
      const initialToken = await this.releaseInitialCredential(ctx, registerToken, committedGeneration);

      // The pinned app-server auth owner is built per epoch on the fresh token (it binds to ONE
      // transport and cannot move — see appserver-auth.ts). Subscription carries the SHARED-cell
      // refresh bridge; api_key carries no bridge.
      const authConfig: CodexAppServerAuthConfig = binding.authMode === "subscription"
        ? {
            mode: "subscription",
            initial: { accessToken: initialToken, accountId: binding.chatgptAccountId },
            bridge: buildAppServerRefreshBridge(ctx.runId, this.opts.client, binding, committedGeneration, registerToken),
          }
        : { mode: "api_key", apiKey: initialToken };
      const appServerAuth = createCodexAppServerAuth(authConfig);

      // (4) The command + fileop effect surfaces, registered in THIS epoch's registry. Route the
      // SCRUBBED command-identity env THROUGH the seam so an injected seam records it too.
      const baseSpawnCommand = this.deps.spawnCommand ?? makeDefaultSpawnCommand(
        registry,
        launchEffectRoot,
        boundaryDeadlineMs,
        worktreePath,
        commandEnv,
      );
      const spawnCommand: SpawnCommandSeam = (argv, spawnOpts) => baseSpawnCommand(argv, { ...spawnOpts, env: commandEnv });
      const fileopRoot = await launchRegisteredEffectRoot(
        registry,
        launchEffectRoot,
        commandEffectSpec(worktreePath, worktreePath, FILEOP_BIN, ["--root", worktreePath], commandEnv),
        boundaryDeadlineMs,
        "command",
      );
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

      // (5) The composition cycle (harness → broker → delegation → harness) is resolved by
      // late-binding `harness`: the delegation seam captures it by reference and is only invoked
      // once a spawn_agent callback fires, by which point `harness` is assigned.
      //
      // (3) PHASE-CORRECT broker + delegation, PER TURN, bound to THIS registry/fileop/spawnCommand
      // and the SHARED per-run tool-handler map. The broker enforces the plan-phase file-write ban
      // through `grants.phase`, so it MUST be phase-correct for the turn it serves; the harness
      // re-points it via useBroker before each turn.
      const providerLaunchRoot = this.deps.launchProviderRoot
        ?? ((spec, authMode) => defaultLaunchProviderRoot(
          spec,
          authMode,
          this.deps.appServerAuthOpenAIBaseUrlForTest,
        ));
      const buildPhaseBroker = (phase: "plan" | "implement", signal?: AbortSignal): CodexCallbackBroker => {
        const runPlan = buildCodexRunPlan(
          this.buildRunRequest(ctx, phase, this.phasePrompt(ctx, phase), undefined, new AbortController().signal),
        );
        const delegationRunner = new CodexDelegationRunner({
          registry,
          roles: runPlan.roles,
          startChildTurn: (spec: StartChildTurnSpec) => this.startChildTurn(harness!, provider, worktreePath, spec),
          spawnCommand,
          fileop,
          worktreePath,
          toolHandlers,
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
          toolHandlers,
          allowedRoles: runPlan.allowedRoles,
          screenPolicy,
          signal,
        });
      };
      // Seed the harness with the PLAN-phase broker (the fail-safe default: writes denied).
      const planBroker = buildPhaseBroker("plan");

      // The provider launch seam (part D): materialize the credential-free session subset into
      // a deterministic short-lived sibling staging tree. The launcher provisions the final
      // runner-owned HOME first, then copies the vetted seed AS runner before app-server spawn.
      // This keeps the worker read-only on the final 0710/2750 provider tree and keeps the
      // command identity out of the dedicated session-reader path. The staging tree is gone before
      // the harness can send initialize or any model-bearing request.
      const providerLaunchSeam: LaunchRootSeam = async (spec) => {
        if (this.deps.launchProviderRoot !== undefined) {
          // Unit seams own a synthetic filesystem and retain the direct adopt call so
          // their established control-flow timing and session-operation evidence stay
          // independent of the production launcher staging protocol.
          await this.sessionStore.adopt(storeDir, codexHome).catch(() => undefined);
          return providerLaunchRoot(spec, binding.authMode);
        }
        const sessionSeedHome = `${ownedDataRoot}.session-seed`;
        await fs.rm(sessionSeedHome, { recursive: true, force: true }).catch(() => undefined);
        const adopted = await this.sessionStore.adopt(
          storeDir,
          sessionSeedHome,
          { destination: "runner-seed" },
        ).catch(() => undefined);
        try {
          return await providerLaunchRoot(
            { ...spec, ...(adopted !== undefined && adopted.files > 0 ? { seedSession: true } : {}) },
            binding.authMode,
          );
        } finally {
          await fs.rm(sessionSeedHome, { recursive: true, force: true }).catch(() => undefined);
        }
      };

      harness = new CodexHarness({
        registry,
        launchRoot: providerLaunchSeam,
        broker: planBroker,
        provider,
        workspace: worktreePath,
        homeDir: ownedDataRoot,
        log: this.log,
        sessionInspect: () => this.sessionStore.inspect(storeDir),
        appServerAuth,
      });

      const epochHarness = harness;
      const epochFileop = fileopHandle;
      return {
        registry,
        safety,
        harness: epochHarness,
        fileopHandle: epochFileop,
        spawnCommand,
        fileop,
        codexHome,
        resumeSessionId,
        buildPhaseBroker,
        // Persist THIS epoch's credential-free session subset into the SHARED store. Best-effort:
        // a persist failure never blocks the reap/recreation (the store fails safe to a fresh
        // session on the next adopt).
        persistSession: async (): Promise<void> => {
          await this.sessionStore.persist(codexHome, storeDir).catch(() => undefined);
        },
        // Full teardown of an ABANDONED epoch on recreation: quiesce + reap (best-effort) +
        // disposeTools the registry (WITHOUT the onDispose token-eviction hook — a live token must
        // survive into the next epoch), then close the harness/transport and dispose the fileop.
        dispose: async (): Promise<void> => {
          await this.tearDownEpoch(registry, epochHarness, epochFileop, boundaryDeadlineMs, true);
        },
      };
    } catch (error) {
      // Never leak a half-built epoch (especially a failed recreation, where run()'s `epoch` still
      // points at the PREVIOUS live epoch): best-effort tear down whatever this build produced.
      await this.tearDownEpoch(registry, harness, fileopHandle, boundaryDeadlineMs, true).catch(() => undefined);
      throw error;
    }
  }

  /**
   * Tear down one provider epoch. When `disposeRegistry` is true (an abandoned epoch on
   * recreation, or the STANDALONE terminal backstop) it quiesces + reaps + disposes the registry;
   * when false (the FINAL epoch under deferRegistryTeardown) it leaves the registry ALIVE for the
   * runner's post-run durability sinks. The harness/transport and fileop CLIENT are executor-owned
   * and always closed here (idempotently, guarded for a setup failure that reached this before
   * either was built). The registry teardown here NEVER runs the onDispose token-eviction hook —
   * that fires only through the FINAL epoch's `safety.dispose` at the true terminal.
   */
  private async tearDownEpoch(
    registry: ExecutionRegistry,
    harness: CodexHarness | undefined,
    fileopHandle: FileopHelperHandle | undefined,
    deadlineMs: number,
    disposeRegistry: boolean,
  ): Promise<void> {
    if (disposeRegistry && registry.state() !== "disposed") {
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
    await harness?.close().catch(() => undefined);
    await fileopHandle?.dispose().catch(() => undefined);
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

  // ─── the initial credential release + per-sink auth-mode reconcile closure ────
  /**
   * Release the currently-committed access token (both auth modes) for a fresh provider root —
   * called once PER EPOCH (every provider-root start/resume freshly releases the committed
   * credential). Fails CLOSED (throws) on any release error — a capability/ownership loss must
   * reject the run, never degrade to an un-credentialed root. The freshly-released token is
   * registered with the redactor (via `registerToken`) before it is used to seed the app-server
   * login credential. For subscription the minimum-generation floor is the SHARED committed cell
   * (a during-run refresh may have advanced it past the binding's initial generation), falling back
   * to the binding's generation when the cell is unset.
   */
  private async releaseInitialCredential(
    ctx: RunContext,
    registerToken: (token: string) => void,
    committed: CodexCommittedGenerationCell,
  ): Promise<string> {
    const binding = this.opts.binding;
    const expected = binding.authMode === "subscription"
      ? {
          authMode: "subscription" as const,
          chatgptAccountId: binding.chatgptAccountId,
          minimumGeneration: committed.value ?? binding.generation,
        }
      : { authMode: "api_key" as const };
    const released = await this.opts.client.releaseCodex(
      ctx.runId,
      { capability: binding.capability },
      expected,
      ctx.signal,
    );
    registerToken(released.access_token); // BEFORE any use
    return released.access_token;
  }

  /**
   * The executor-owned auth-mode reconcile closure the safety facade runs BEFORE every boundary
   * quiesce/reap/mint. Delegates to the standalone {@link buildRunLaneReconcile} (pinned by the
   * differential test against the advice bridge), sharing the run's ONE committed-generation cell
   * and `registerToken` closure so the boundary reconcile and the app-server refresh bridge never
   * keep private state (part C / m1).
   */
  private makeBoundaryReconcile(
    runId: string,
    registerToken: (token: string) => void,
    committed: CodexCommittedGenerationCell,
  ): ReconcileBeforeBoundary {
    return buildRunLaneReconcile(runId, this.opts.client, this.opts.binding, registerToken, committed);
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
          environments: [],
          dynamicTools: buildCodexDynamicTools(spec.grants),
          config: { project_doc_max_bytes: 0, projects: { [workspace]: { trust_level: "untrusted" } } },
          developerInstructions: spec.systemPrompt,
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

/**
 * Build the fully-REPLACED credential-free command-identity env: the FIXED
 * toolchain+system PATH ({@link COMMAND_ENV_PATH}), TMPDIR and LANG, folding in the run's
 * allowlisted provisioned `toolEnv` (bounded upstream by PROVISION_ENV_ALLOWLIST to
 * {PATH, NIX_SSL_CERT_FILE, LOCALE_ARCHIVE}). NOTHING from process.env is inherited.
 *
 *   - PATH: the fixed boundary dirs come FIRST and are ALWAYS present; a provisioned
 *     `toolEnv.PATH` is APPENDED after them, so provisioned tools resolve but can never
 *     displace or drop the boundary dirs (the fixed prefix always wins on a collision).
 *   - the OTHER allowlisted vars (NIX_SSL_CERT_FILE / LOCALE_ARCHIVE) are folded in.
 *   - a `toolEnv` entry can NEVER overwrite a boundary literal (PATH/TMPDIR/LANG/HOME) or a
 *     PROTECTED_ENV_KEYS member — {@link COMMAND_ENV_PROTECTED_KEYS} unions both, because
 *     PROTECTED_ENV_KEYS alone omits PATH/TMPDIR/LANG. `commandEffectSpec` later overrides
 *     HOME/TMPDIR to the per-command private tmp, so those stay boundary-safe regardless.
 */
export function buildCommandEnv(tmpdir: string, toolEnv: Record<string, string>): NodeJS.ProcessEnv {
  const provisionedPath = toolEnv.PATH;
  const env: NodeJS.ProcessEnv = {
    // The fixed boundary dirs come first; a provisioned PATH is appended (never prepended,
    // never substituted), so a provisioned entry cannot shadow a boundary dir.
    PATH: provisionedPath ? `${COMMAND_ENV_PATH}:${provisionedPath}` : COMMAND_ENV_PATH,
    TMPDIR: tmpdir,
    LANG: "C",
  };
  for (const [k, v] of Object.entries(toolEnv)) {
    if (COMMAND_ENV_PROTECTED_KEYS.has(k)) continue; // never breach the boundary / reintroduce a credential key
    env[k] = v;
  }
  return env;
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
 *  pinned binary/supervisor/argv) under APP-SERVER AUTH — the launcher emits the production
 *  config and injects NO env credential; the credential enters over the login RPC — launches,
 *  and adapts the {@link CodexRootHandle} into a registry-ownable {@link RegisteredRoot} + the
 *  app-server transport. DARK: never run in tests (a fake `launchProviderRoot` is injected). */
async function defaultLaunchProviderRoot(
  spec: CodexLaunchRootSpec,
  authMode: CodexAppServerAuthMode,
  openAIBaseUrlForTest?: string,
): Promise<CodexLaunchRootResult> {
  const handle = await launchCodexRoot({
    ownedDataRoot: spec.ownedDataRoot,
    // No credentialValue: the token flows over `account/login/start`, never the launcher env.
    provider: {
      name: spec.provider.name,
      baseUrl: spec.provider.baseUrl,
      envKey: spec.provider.envKey,
    },
    model: spec.model,
    codexBin: CODEX_BIN,
    supervisorBin: SUPERVISOR_BIN,
    kind: "provider",
    childArgv: [...PROVIDER_CHILD_ARGV],
    cwd: spec.cwd,
    useAppServerAuth: true,
    authMode,
    seedSession: spec.seedSession,
  }, openAIBaseUrlForTest === undefined
    ? undefined
    : { appServerAuthOpenAIBaseUrlForTest: openAIBaseUrlForTest });
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
