// Neutral harness contract (PRD #1146 / #1106 M2, milestone m1).
//
// The provider-neutral type surface the run lane is being extracted behind. It
// has NO provider SDK import — only type-only imports from the two uzi domain
// modules that already own these shapes (`executor.ts`, `protocol.ts`). The
// Claude adapter (`claude-harness.ts`) and the pure reducer (`harness-reducer.ts`)
// speak these types; `sdk-executor.ts` remains the workflow owner that drives
// them. See `e2e/codex-m0/harness-contract.md` §Contracts for the accepted design
// (this file authors the M2 subset with a live referent; the process-safety and
// delegation surface is deferred to M3/M4 and intentionally omitted here so knip's
// zero-unused-export gate stays green).

import type { EmittedMessage } from "./executor.js";
import type {
  AskUserQuestion,
  Milestone,
  MilestoneProgress,
  Proposal,
} from "./protocol.js";

export type HarnessKind = "claude" | "codex";
export type HarnessEffort = "low" | "medium" | "high" | "xhigh" | "max";
export type SessionPresence = "present" | "absent" | "unknown";
export type JsonObject = Readonly<Record<string, unknown>>;

// Missing measurements mean unavailable, never zero. These are independent,
// non-overlapping input buckets; output includes reasoning when reported.
// An adapter must leave a bucket absent if it cannot establish its semantics.
export interface HarnessTokens {
  inputUncachedTokens?: number;
  inputCacheReadTokens?: number;
  inputCacheWriteTokens?: number;
  inputTotalTokens?: number;
  outputTokens?: number;
  reasoningOutputTokens?: number; // subset of outputTokens, never added again
}

export type HarnessCost =
  | { kind: "metered"; usd: number; source: "provider" | "price_table" }
  | { kind: "subscription" }
  | { kind: "unreported" };

export interface HarnessModelUsage {
  tokens: HarnessTokens;
  cost: HarnessCost;
}

export interface HarnessUsage {
  basis: "call" | "turn" | "session";
  tokens: HarnessTokens;
  models?: Readonly<Record<string, HarnessModelUsage>>;
  // Exact existing uzi output fields. These are not used for lifecycle logic.
  // Call wire: attach as payload.usage to one surviving item.
  // Result wire: attach as payload.usage and payload.modelUsage respectively.
  wire?: { usage: unknown; modelUsage?: unknown };
}

export interface HarnessContext {
  used: number;
  window: number;
  pct: number;
}

// The reducer requests the lead context read (fire-and-forget) and later awaits
// it; the adapter owns the bounded, never-throwing read (Claude: 2000ms). This is
// the neutral seam that keeps the reducer SDK-free while the placement decision —
// "which item survived signal filtering" — stays in the reducer.
export interface HarnessContextHook {
  request(): void;
  get(): Promise<HarnessContext | undefined>;
}

export interface HarnessMetrics {
  turnCount?: number;
  durationMs?: number;
  cost: HarnessCost;
  // Optional normalized measurements do not replace D0's exact wire values.
  wire?: {
    num_turns: unknown;
    duration_ms: unknown;
    total_cost_usd: unknown;
  };
}

export interface HarnessRateLimit {
  status: string; // preserve an unknown status; it is not implicitly rejected
  resetsAtMs?: number;
  window?: string; // unvalidated provider vocabulary; API owns its allowlist
}

export interface HarnessLimitEvidence {
  explicitExhaustion: boolean;
  latest?: HarnessRateLimit;
}

export interface HarnessLimitFailure {
  resetsAtMs?: number;
  window?: string;
}

export type HarnessErrorCategory =
  | "aborted"
  | "timeout"
  | "authentication"
  | "authorization"
  | "rate_limit"
  | "model"
  | "effort"
  | "transport"
  | "tool"
  | "protocol"
  | "session_missing"
  | "unknown";

export interface HarnessError {
  category: HarnessErrorCategory;
  message: string;
  limit?: HarnessLimitFailure;
}

// In-memory exception only. Never serialize original or spread it into a frame.
// The Claude compatibility boundary may rethrow the original Error unchanged.
export interface HarnessThrownFailure {
  failure: HarnessError;
  original: unknown;
}

export type HarnessOrigin =
  | { kind: "main" }
  | { kind: "subagent"; role?: string; instanceId?: string }
  | { kind: "unknown" };

// A separate presentation contract. Claude intentionally projects some replay
// frames to agent="lead" even when their origin is a subagent invocation.
export interface HarnessAttribution {
  agent?: string;
  agentInstance?: string;
  agentLabel?: string;
}

export type HarnessSignalName =
  | "submit_plan"
  | "signal_done"
  | "ask_user"
  | "report_progress"
  | "checkpoint";

export type HarnessItem =
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | {
      kind: "tool";
      phase: "started";
      id?: string;
      name?: string;
      input?: unknown;
      signal?: HarnessSignalName;
    }
  | {
      kind: "tool";
      phase: "finished";
      id?: string;
      name?: string;
      output?: unknown;
      isError?: boolean;
    };

export interface HarnessTerminal {
  outcome: "success" | "failed";
  // Exact uzi display subtype and String-mapped provider error array.
  subtype: string;
  errors: readonly string[];
  // Deferred compatibility construction, independent of display subtype.
  // Invoke only at the lane's existing terminal-classification point.
  failure?: HarnessTerminalFailure;
  usage?: HarnessUsage;
  metrics: HarnessMetrics;
  limitEvidence?: HarnessLimitEvidence;
}

export interface HarnessTerminalFailure {
  // No raw provider event escapes this closure. It privately retains values
  // whose truthiness/conversion cannot be reconstructed from display fields.
  // With limit facts, construct the original typed limit exception; without
  // them, construct the existing generic terminal exception. May itself throw
  // during malformed-value conversion, at this call site, never during decode.
  materialize(limit?: HarnessLimitFailure): HarnessThrownFailure;
}

export interface HarnessEventMeta {
  // Every input event is liveness, including ignored/partial provider frames.
  // IDs are accepted lazily and may occur on otherwise ignored frames.
  sessionId?: string;
  rateLimit?: HarnessRateLimit;
  // Existing detector runs on every raw message, including ignored kinds.
  orphanInstanceFrameKind?: string;
}

export type HarnessEvent = HarnessEventMeta &
  (
    | { kind: "activity" }
    | { kind: "initialized"; model?: string }
    | {
        kind: "frame";
        origin: HarnessOrigin;
        attribution: HarnessAttribution;
        items: readonly HarnessItem[];
        usage?: HarnessUsage; // call basis only on assistant-derived frames
        model?: string;
        // Neutral scanned-signal carrier: the workflow signals the adapter read
        // off this raw frame's tool_use blocks (scanSignals), main-thread-gated by
        // the adapter. This is NOT an EmittedMessage transport — persisted output
        // travels through `items` — it is the reducer's signal-fold input.
        signals?: Readonly<Partial<TurnSignals>>;
      }
    | { kind: "turn_finished"; terminal: HarnessTerminal }
  );

// Tool inheritance is distinct from an explicitly empty allowlist. Conversion
// of today's absent/null/empty template tools to inherit occurs upstream once.
export type HarnessToolSet =
  | { kind: "inherit" }
  | { kind: "allow"; names: readonly string[] };

export interface HarnessAgent {
  description: string;
  prompt: string;
  model?: string;
  tools: HarnessToolSet;
  deniedTools: readonly string[];
  toolServers: readonly string[];
  skills: readonly string[]; // canonical bare names, explicit [] disables all
}

export interface RunTurnRequest {
  prompt: string;
  systemPrompt: string;
  resumeSessionId?: string;
  signal: AbortSignal;
  model?: string;
  effort?: HarnessEffort;
  phase: "plan" | "implement";
  agents: Readonly<Record<string, HarnessAgent>>;
  leadSkills: readonly string[];
  // false suppresses attribution; true/absent preserve today's SDK default.
  attributionEnabled?: boolean;
}

export interface HarnessTurn {
  events: AsyncIterable<HarnessEvent>;
  // Immediate cancellation request; not a stopped-process assertion.
  requestStop(reason: "terminal" | "cancel" | "timeout"): void;
  // Bounds and swallows absent/unsupported/error/hang to undefined. In Claude,
  // triggered exactly when the existing reducer first attaches lead usage.
  readContext(timeoutMs: number): Promise<HarnessContext | undefined>;
  // Idempotent root iterator/transport closure only. May throw a distinct
  // transport/iterator-close exception. Does not drain children or kill groups.
  close(): Promise<void>;
}

// ---------------------------------------------------------------------------
// M3/M4 process-safety surface (PRD #1171). These types were deliberately omitted
// from the M2 subset (see the file header, lines 9-11) so knip's zero-unused-export
// gate stayed green; they are added here for the Codex adapter's fail-closed
// execution spine. Shapes are the accepted contract's, implemented EXACTLY
// (harness-contract.md §Contracts, lines 270-332). `HarnessError` (above) is reused.
// ---------------------------------------------------------------------------

export type SafeBoundary =
  | "checkpoint"
  | "park"
  | "shutdown"
  | "terminal"
  | "finalize"
  | "credentialed_git";

export interface BoundaryRequest {
  boundary: SafeBoundary;
  deadlineMs: number; // absolute wall-clock deadline, never an unbounded wait
}

export type ChildQuiescence =
  | { kind: "quiescent"; epoch: number }
  | { kind: "legacy_unobserved" }
  | { kind: "incomplete"; errors: readonly HarnessError[] };

export type ProcessReap =
  | {
      kind: "observed_empty";
      // The safety owner supplies this only after every registered supervisor
      // root has reaped its descendants, including process-group escapees.
      evidence: "supervisor_echild";
      epoch: number; // must match the quiescent epoch held by the safety owner
    }
  | { kind: "legacy_dispatched" }
  | { kind: "incomplete"; errors: readonly HarnessError[] };

export type ToolDisposal =
  | { kind: "disposed" }
  | { kind: "legacy_in_process" }
  | { kind: "incomplete"; errors: readonly HarnessError[] };

export interface RunHarness {
  readonly kind: HarnessKind;
  inspectSession(id: string): Promise<SessionPresence>;
  startTurn(request: RunTurnRequest): HarnessTurn;
  // The three process-safety methods below are OPTIONAL here, UNLIKE the contract's
  // non-optional listing (harness-contract.md:298-315). This divergence is deliberate
  // and load-bearing: making them optional keeps the existing `ClaudeHarness` and the
  // test stub byte-for-byte unchanged — they simply never implement these, so their
  // legacy `killAgentTree` cleanup branch stays literal and Claude behavior is
  // preserved. ONLY `CodexHarness` (a later M3 unit) implements them. Do not remove
  // or reorder the three members above.
  //
  // Closes admission to spawn/tool callbacks, settles accepted callbacks,
  // resolves all owned child turns,
  // cancels/settles owned provider cells/terminals, accounts for discovery races.
  // Successful quiescence freezes the epoch until an explicit later startTurn.
  quiesceChildren?(request: BoundaryRequest): Promise<ChildQuiescence>;
  // OS operation over recorded ownership only; cannot establish remote child
  // quiescence. Every registered supervisor must observe ECHILD, using __WALL where
  // required, rather than infer emptiness from CLI exit or its original PGID.
  // Must never select unrelated processes by namespace/glob.
  reapProcesses?(request: BoundaryRequest, closedEpoch: number): Promise<ProcessReap>;
  // Revokes per-run callback authorization, rejects pending admissions, drains
  // handlers, closes listeners/transports, and drops token/handler references.
  disposeTools?(request: BoundaryRequest): Promise<ToolDisposal>;
}

// Module-private brand; callers cannot manufacture a permit from observations.
declare const boundaryPermitBrand: unique symbol;

export interface BoundaryPermit {
  readonly [boundaryPermitBrand]: true;
  readonly epoch: number;
  readonly boundary: SafeBoundary;
}

export interface CodexExecutionSafety {
  readonly kind: "codex";
  withBoundary<T>(
    request: BoundaryRequest,
    action: (permit: BoundaryPermit) => Promise<T>,
  ): Promise<T>;
}

// M3 addition required on the existing outer Executor contract in executor.ts:
// safety?: CodexExecutionSafety;
// This facade is owned by uzi, so the adapter does not gain git/workflow policy.

export interface TurnSignals {
  plan?: string;
  done: boolean;
  prdDonePath?: string;
  milestonesCompleted?: string[];
  questions?: AskUserQuestion[];
  milestones?: Milestone[];
  progress?: MilestoneProgress;
  checkpoint?: boolean;
  summary?: string;
  reportOnly?: boolean;
  proposal?: Proposal;
}

export interface ReducedTurnResult extends TurnSignals {
  sessionId?: string;
  finalText?: string;
  subagentActivity?: boolean;
  /** issue #1197 (D-RC2a): the SDK terminal's POSITIVELY-reported turn count
   *  (`terminal.metrics.wire.num_turns`), coerced to a number ONLY when it is a
   *  finite number (mirrors limit.ts normalizeResetsAt's shape). Missing or garbage
   *  metrics leave it `undefined` — NEVER defaulted to 0 — so a turn that ran but
   *  reported no count is never mistaken for a positively-empty (zero-turn) result. */
  numTurns?: number;
  /** issue #1197 (D-RC2a): true when ANY assistant/tool frame (items) or usage was
   *  folded this turn — the positive "the model did something" signal. Left false/
   *  undefined only when the turn streamed no model output at all. Paired with
   *  `numTurns === 0` it is the evidence a turn was positively empty. */
  sawModelActivity?: boolean;
}

export interface TurnReduction {
  messages: EmittedMessage[];
  // Immediate effects, not deferred to turn completion. The owner delivers
  // onProgress without awaiting the network report, exactly as today.
  firstSessionId?: string;
  progress?: MilestoneProgress;
  diagnostics: readonly { kind: "orphan_instance"; frameKind: string }[];
}

export type TurnStreamEnd =
  | { kind: "terminal"; terminal: HarnessTerminal }
  | { kind: "exhausted" } // legacy Claude clean EOF; no fabricated success frame
  | { kind: "thrown"; thrown: HarnessThrownFailure; terminal?: HarnessTerminal };

export interface ReducedTurnCompletion {
  result: ReducedTurnResult;
  end: TurnStreamEnd;
}

export interface RunTurnReducer {
  // Turn lifecycle: the owner calls beginTurn() at the top of every turn, before
  // the first accept(), so per-turn state is cleared structurally regardless of
  // how the prior turn ended. Run-scoped state (e.g. the first-session-id latch)
  // persists across turns and is not reset here.
  beginTurn(): void;
  accept(event: HarnessEvent): Promise<TurnReduction>;
  finish(end: TurnStreamEnd): ReducedTurnCompletion;
}

export interface AdviceRequest {
  label: "judge" | "review" | "summary";
  systemPrompt: string;
  prompt: string;
  model?: string;
  /** Reasoning effort; applied to the SDK query only when set. */
  effort?: HarnessEffort;
  output: { kind: "text" } | { kind: "json"; schema: JsonObject };
  signal: AbortSignal;
  timeoutMs: number;
  graceMs?: number;
}

export interface AdviceResult {
  text: string;
  end: TurnStreamEnd;
  usage?: HarnessUsage;
}

export interface AdviceResultPolicy {
  onTerminal(
    terminal: HarnessTerminal,
    context: { isError: boolean; latest: HarnessRateLimit | undefined },
  ): void;
}

export interface AdviceHarness {
  readonly kind: HarnessKind;
  // One isolated pass, with its own disposable HOME and no run-tool authority.
  // Both permit isolated pure calculation; current Claude stays tool-less.
  // This interface requires no new Claude calculator. Terminal
  // provider failures are data; setup/transport/timeout failures are thrown.
  run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult>;
}
