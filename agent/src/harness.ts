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

export interface RunHarness {
  readonly kind: HarnessKind;
  inspectSession(id: string): Promise<SessionPresence>;
  startTurn(request: RunTurnRequest): HarnessTurn;
}

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

// NOTE (PRD #1146 M2, milestone m1): the advice-lane surface
// (AdviceRequest/AdviceResult/AdviceResultPolicy/AdviceHarness and the JsonObject
// it used) is intentionally NOT authored here yet. This milestone extracts the RUN
// lane only; no advice adapter exists, so exporting those types would leave
// AdviceHarness with no M2 referent and redden the zero-unused-export gate
// (deadcode:agent). They land with the advice extraction that wires model-pass.ts.
