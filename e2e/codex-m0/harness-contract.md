# Proposed neutral harness contract

**Status:** proposed for [ADR-1106](../../adr/1106-codex-harness.md); M0 remains incomplete.
**Source inventory:** `bc5a0a8b11f5c98a7067c1fc4202d37a0f27f92e`, paths and symbols below.
This is an M2 extraction contract, not a production adapter or new test result.
The Claude preservation rules describe source behavior; Codex obligations require
separate implementation and characterization. Both subscription and API-key
support remain required, with credential identity/CAS/routing/pricing still open.

The Codex advice policy is awaiting the user's choice between isolated calculations
and literally no tools. Neither choice may grant shell, filesystem, network,
delegation or worker callbacks. The type surface below grants no such authority;
its final construction policy remains pending. Claude advice stays unchanged.

## Approach

Use a per-run provider adapter with a per-turn stream handle, plus a separate
advice adapter with no run-tool authority. Keep the workflow loop, signal reduction, checkpoint
policy, git, and state transitions in uzi. Keep provider configuration, query
construction, wire decoding, session inspection and process ownership inside
the adapter. The reducer consumes only neutral records.

The extraction must preserve two independent contracts: the existing uzi
message payload and the semantic result that drives the workflow. In particular,
display attribution is not authorization; a terminal provider failure is not a
transport exception; and ending the root turn is not child quiescence or process
reaping.

Two rejected alternatives:

* A single `{outcome, text, usage}` terminal record loses init events, grouped
  assistant usage, exact result accounting, the judge's typed inner error, and
  iterator-close-error precedence.
* Reconstructing all existing accounting payloads from validated numeric fields
  changes D0. `mapResult` currently forwards accounting unguarded, including
  unknown fields and explicit `undefined` object keys asserted by existing tests.
  Retain an opaque **uzi wire projection**, alongside normalized measurements.
  It contains existing outgoing payload data, never an SDK message/discriminant.

The code below is the proposed type surface for `agent/src/harness.ts` in M2.
Existing domain imports are intentional; these types already exist and remain
owned by `protocol.ts` / `executor.ts`. The artifact itself adds no M2 code.

## Contracts

```ts
import type { EmittedMessage } from "./executor.js";
import type {
  AskUserQuestion, Milestone, MilestoneProgress, Proposal,
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
  | "aborted" | "timeout" | "authentication" | "authorization"
  | "rate_limit" | "model" | "effort" | "transport" | "tool"
  | "protocol" | "session_missing" | "unknown";

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
  | "submit_plan" | "signal_done" | "ask_user" | "report_progress"
  | "checkpoint";

export type HarnessItem =
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | {
      kind: "tool"; phase: "started";
      id?: string; name?: string; input?: unknown;
      signal?: HarnessSignalName;
    }
  | {
      kind: "tool"; phase: "finished";
      id?: string; name?: string; output?: unknown; isError?: boolean;
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

export type HarnessEvent = HarnessEventMeta & (
  | { kind: "activity" }
  | { kind: "initialized"; model?: string }
  | {
      kind: "frame";
      origin: HarnessOrigin;
      attribution: HarnessAttribution;
      items: readonly HarnessItem[];
      usage?: HarnessUsage; // call basis only on assistant-derived frames
      model?: string;
    }
  | {
      kind: "delegation";
      phase: "started" | "completed" | "failed";
      identifiers: {
        agentId?: string; agentType?: string; senderId?: string;
        receiverThreadIds?: readonly string[];
      };
      error?: HarnessError;
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

export type SafeBoundary =
  | "checkpoint" | "park" | "shutdown" | "terminal" | "finalize" | "credentialed_git";
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
      // The proposed Linux supervisor supplies this only after owning and
      // reaping every descendant, including process-group escapees.
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
  // Closes admission to spawn/tool callbacks, settles accepted callbacks,
  // resolves all owned child turns,
  // cancels/settles owned provider cells/terminals, accounts for discovery races.
  // Successful quiescence freezes the epoch until an explicit later startTurn.
  quiesceChildren(request: BoundaryRequest): Promise<ChildQuiescence>;
  // OS operation over recorded ownership only; cannot establish remote child
  // quiescence. The proposed supervisor must observe ECHILD, using __WALL where
  // required, rather than infer emptiness from CLI exit or its original PGID.
  // Must never select unrelated processes by namespace/glob.
  reapProcesses(request: BoundaryRequest, closedEpoch: number): Promise<ProcessReap>;
  // Revokes per-run callback authorization, rejects pending admissions, drains
  // handlers, closes listeners/transports, and drops token/handler references.
  disposeTools(request: BoundaryRequest): Promise<ToolDisposal>;
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

// Proposed addition to the existing outer Executor contract in executor.ts:
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
  accept(event: HarnessEvent): Promise<TurnReduction>;
  finish(end: TurnStreamEnd): ReducedTurnCompletion;
}

export interface AdviceRequest {
  label: "judge" | "review" | "summary";
  systemPrompt: string;
  prompt: string;
  model?: string;
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

export interface AdviceHarness {
  readonly kind: HarnessKind;
  // One isolated pass, with its own disposable HOME and no run-tool authority.
  // Codex calculation policy is pending; Claude remains tool-less. Terminal
  // provider failures are data; setup/transport/timeout failures are thrown.
  run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult>;
}
```

### Accounting and wire preservation

The numeric view is for neutral reasoning about measured usage. The `wire`
capsule preserves the existing uzi message contract during extraction. On Claude
result frames the reducer must always construct `num_turns`, `duration_ms`,
`total_cost_usd`, `usage` and `modelUsage`, even when their value is `undefined`;
existing mapper tests assert those keys before JSON serialization. Unknown
accounting members must survive. The capsule is not inspected by lifecycle code.

Claude assistant usage is per API call. Result usage/modelUsage is cumulative
across resumed sessions as consumed by the existing API fold. Never add those
snapshots together in the worker. The adapter supplies `basis:"session"` for
that result source. Codex's declared turn usage must be labeled `basis:"turn"`;
how the API stores that without re-addition remains M5 accounting work.

Claude call `input_tokens` and cache read/write fields map to separate input
buckets. Do not assume Codex's named input counter has the same inclusion rules:
the Codex adapter must establish that mapping from pinned protocol/source and
fixtures before assigning normalized totals. Reasoning output is a subset,
never an additional output bucket to sum. Unknown fields remain absent, not 0.

Subscription cost is `kind:"subscription"`, never metered USD 0. API-key cost is
`metered` only when supplied or derived from an accepted versioned price table;
otherwise it is `unreported`. This represents both required auth modes without
pretending M0 has solved the currently open SC5 pricing contract. Claude's exact
reported cost remains in its wire capsule, including failed results.

### Event, reducer and error authority

1. Each raw input becomes one neutral event, including `activity` for ignored
   system/partial messages. This preserves liveness and any attached session ID.
   Codex item updates are activity only. `initialized` preserves Claude's
   model-bearing init status, distinct from a bare turn-start notification.
2. The reducer reports the first **truthy** session ID once per run through
   `firstSessionId`, retaining the current callback's catch-and-warn behavior;
   the turn result retains the last truthy ID seen within that turn. Do not
   implement the misleading `sessionIdOf` comment's “first one wins” as the turn
   result rule. Empty string has never advanced the current session.
3. Group a complete assistant frame before filtering. Drop every recognized
   signal `tool_use` from persisted output, regardless of origin. Attach the
   frame usage and model to exactly the first surviving item; model is co-gated
   with usage. If all items are filtered both are lost, as today. Tool results
   are not removed by the signal-use filter.
4. Authorize signal reduction using `origin.kind === "main"`, separately from
   presentation attribution. Claude main/subagent decoding preserves its exact
   current marker tests: a nonempty string subagent_type OR parent_tool_use_id
   marks a subagent. Its display agent still uses the existing string-or-lead
   projection, including empty strings. Codex unknown remains unknown and cannot
   latch signals. Hook identity alone does not fabricate message attribution.
5. Preserve all signal parsing and fold rules: plan/milestones/progress and
   declaration fields are last-wins; done/checkpoint/reportOnly latch;
   questions concatenate. Retain existing malformed-input handling and caps
   by moving the existing neutral parsers, not rewriting them.
6. Preserve the current display-based no-progress derivation: concatenate text
   whose emitted agent equals `lead`, separated by newlines; set subagentActivity
   when an emitted agent is defined and differs from `lead`. A repo subagent
   actually named `lead` and replay anomalies must not acquire a silent D0 fix.
7. Fire `readContext` once, on first **surviving usage-bearing displayed-lead**
   item, without awaiting the hot stream. Wait for it only when emitting the
   terminal result; attach `{used,window,pct}` on success OR failure. Default
   Claude timeout stays 2000ms. Absence, error and hang omit context and never
   fail the turn. This placement belongs in the reducer, since the adapter cannot
   know which item survives signal filtering.
8. Emit terminal accounting before settling the workflow outcome. Run turns
   request root stop with reason `terminal` and close the iterator after the
   terminal frame. Advice success does not gain a new explicit abort: its
   existing `for await` break/iterator closure behavior must remain.
9. **Run-lane authority:** the existing first-wins local watchdog/cancel reason wins over
   every other failure. A distinct query-creation/iteration/iterator-close throw
   wins next; an already emitted result message is not withdrawn. If consumption
   closes normally, terminal provider failure determines the failure next.
   A provider failure is represented once, as terminal data; adapters must not
   also throw that same failure. No terminal and clean EOF is `exhausted`, not a
   fabricated success/error result. Claude currently returns accumulated signals
   or advice text in that case. Codex may treat its own unexpected EOF as a
   protocol throw; that decoding rule cannot retroactively change Claude EOF.
10. Preserve existing Claude exception behavior at the compatibility boundary:
    iterator Error objects are rethrown; non-Error throws become
    `new Error(errMessage(value))`. Terminal failure materialization happens
    after run result emission and successful iterator closure/classification,
    exactly where current `driveTurn` constructs the exception. Do not eagerly
    stringify a malformed subtype during decode and lose emitted accounting.
    Advice uses the distinct pre-close policy point below.

`HarnessTerminalFailure.materialize` privately retains the executor's runtime
`subtype ?? "unknown"` value. Display subtype, generic message and limit suffix
are three different projections. For a failed explicit-limit terminal,
`subtype:false` produces no detail suffix; `subtype:["false"]` produces `": false"`,
although both display as `"unknown"` and have the same generic interpolated
message. A truthy value stringifying to empty still contributes `": "`.
The materializer preserves the presence/truthiness decision, interpolation count
and timing, and original `LimitReachedError` class. It must not derive the suffix
from the normalized subtype/message, coerce the original value twice, or turn
an unknown provider message into an invented auth/model/effort category. An
exception thrown by conversion is the current call-site exception; it does not
retroactively withdraw the result already emitted. This deferred factory is the
neutral representation of that compatibility requirement, not a raw SDK escape.

### Rate limits and advice

Keep the current terminal-only limit policy. For Claude, the adapter translates
`blocking_limit`/`rapid_refill_breaker` to `explicitExhaustion:true`; it maps the
latest observation's reset via existing `normalizeResetsAt`. Classification is
performed at the existing caller completion point with `Date.now()`, not earlier
in the stream. A successful terminal never becomes a limit failure. On a failed
terminal, explicit exhaustion classifies even without a future reset; otherwise
latest `status:"rejected"` plus a future reset classifies. Keep only a future
reset in the failure facts. Unknown status is not rejected.

The currently documented residual is intentional: rejected + future reset can
classify an unrelated error_max_turns as a limit. Do not silently add a subtype
allowlist in M2. Transient retry frames never independently park a run.

`AdviceHarness.run(request, policy)` invokes the synchronous **uzi policy** once
inside the terminal iteration body, before `break`, iterator return/closure and
HOME cleanup. The helper supplies the existing generic default when no override
is present; a supplied override replaces that default, rather than adding to it.
It must not defer the callback until the returned `AdviceResult` is available.

```ts
export interface AdviceResultPolicy {
  onTerminal(
    terminal: HarnessTerminal,
    context: { isError: boolean; latest: HarnessRateLimit | undefined },
  ): void;
}
```

Keep `ReadOnlyModelPassOpts.onResult(msg: unknown, ...)` through an explicitly
Claude-only compatibility shim, so existing callback tests stay unchanged.
Production judge policy can consume the neutral terminal. The shim stays inside
the Claude boundary; raw provider events never reach the neutral reducer.
Judge limit classification calls `Date.now()` in that callback, before closure.
Its callback/default body-thrown exception retains JavaScript's existing
precedence over an iterator-return exception; successful policy then `break`
still exposes a close exception. This differs from run-lane classification after
normal closure. Neither lane gains a global, interchangeable error ordering.

The synchronous policy may throw. The helper-supplied default throws on a
failed terminal:
`${label} model call returned an error result`. Judge's callback classifies the
limit at the pre-close callback point and throws the existing `LimitReachedError`
class, without the run
lane's subtype detail, or the existing generic judge error. Success captures
the mapped terminal usage message. Review and summary retain generic errors,
even for identical limit-shaped input. Structured-output parsing stays with
the current consumers. All existing Claude advice calls use `output:{kind:"text"}`;
they do not acquire native outputSchema enforcement simply because their prompts
ask for JSON. Codex's output schema support must be lane-specific and measured.

Crucial actual caller behavior: `judge()` catches ALL model failures, including
LimitReachedError, and returns the deterministic fallback. `execute()` posts it
and reports completed with no usage frame. It does not park, and this model-error
path does not reach safeReportFailed. The older comment about a structured limit
failed-state report is not the behavior of this path. Review converts model
failure to a failed-review payload; summary catches and returns null. Preserve
both the inner typed error and those outer fallbacks.

Advice timeout still aborts AND rejects the wall-clock race using the exact
label/timeout message. It waits for query settlement or the current 500ms default
grace before best-effort HOME removal. Cleanup warnings do not replace the
primary failure. Returning partial text after a timeout would change D0.

### Skills and policy inputs

`HarnessAgent.skills` and `leadSkills` are explicit lists, including empty lists.
They carry canonical names; the Claude adapter qualifies them as `uzi:<name>`.
Construction receives the sanitized, materialized survivor catalog outside the
clone. Own-template agents receive their allocated delivered survivors plus repo
survivors, in current order; repo-source agents receive the full surviving union.
The lead receives the full run union. Unknown/dropped skill names never reappear.

Preserve `tools`, `deniedTools` and `toolServers` independently. Inherit-all must
not become a generated allowlist. Claude's per-agent forge/findings server
references are attached only where its resolved explicit allowlist requires
them; the inherited-tool case remains omitted. Skill enablement must not widen
the tool allowlist. The plan roster remains the existing copied/write-stripped
roster and the implement roster remains selected after approval. This interface
does not claim that Codex role TOML can enforce any of these inputs.

Use separately gated run and advice construction functions for Codex. An advice
constructor must not accept agents, MCP handlers, general tools or a run working
directory. Runtime credentials remain a discriminated private constructor input
for Claude OAuth, Codex subscription auth and OpenAI API key, with no auth fallback.
The per-run home/workspace/process launcher/secret paths and handler registry are
immutable construction inputs. Credential row selection and refresh write-back
remain the unresolved wider PRD contracts, not guessed constructor defaults.

### Lifecycle operation order and evidence

The candidate policy and measured limits live in
[ADR-1106](../../adr/1106-codex-harness.md#proposed-execution-policy).
The proposed outer `Executor.safety.withBoundary` is a uzi facade over adapter
operations; the runner still owns the action and its git/workflow policy.
An internal poisoned adapter alone cannot guard the runner's exception recovery.
A Codex-selected executor must provide this safety facade or fail before execution;
optional absence preserves existing Claude/stub callers, never a Codex fallback.

| Boundary | Proposed operation and acceptance evidence |
|---|---|
| Ordinary root turn end | Request root stop and close its iterator. Synchronous Codex delegation must prevent a child escaping the parent turn. This alone grants no checkpoint/publication permit. |
| Codex checkpoint, park, shutdown, terminal/finalize or credentialed git | Enter the outer gate; close admission before discovery; settle accepted starts/callbacks; freeze child quiescence at one epoch; reap processes for that same epoch; invoke the runner action only after joint acceptance. |
| Action lifetime | Hold the permit and closed epoch through the complete action and its owned work. `startTurn` cannot reopen it. Serialize overlapping boundary acquisitions. Release only after actual settlement, never because a timeout race returned while its git child kept running. |
| Incomplete barrier | Record poison independently of the primary run exception; never invoke the action. Preserve primary failure and cleanup evidence separately. All later publication/recovery paths reject the poison. Attempt remaining applicable cleanup stages. |
| Run disposal | Permanently revoke callback admission/registry; settle handlers, then drop routing and references. A listener close alone cannot stop a handler already making an API call. |

A permit requires `ChildQuiescence.quiescent.epoch ===
ProcessReap.observed_empty.epoch` for the current held epoch. Historical results
cannot mint it. The epoch number is descriptive; the module-private brand and
safety owner's held state enforce authority. Deadlines bound cleanup. On action
timeout, terminate/settle owned work or retain the closed, poisoned state until
it settles; a hung action is never permission to reopen admission.

Every Codex sink uses the outer gate, including limit parking, shutdown durability,
checkpoint overlay/default-tip fetch, terminal handoff, finalize and best-effort
recovery publication. A primary `LimitReachedError` may still select the park
branch, but incomplete cleanup prevents that branch's action from running.
For Codex, ordinary `reap:false` fetch-back may remain credential-free local work;
**any subsequent checkpoint publication or credentialed operation still requires
the full gate**. The flag is never a Codex permit. Successful ordinary boundaries
keep tool services behind the closed epoch; only an explicitly permitted next
turn may reopen, after the prior action settles and absent poison/disposal.

The absent/Claude safety branch invokes today's synchronous `killAgentTree?.()`
directly where it currently does and preserves `reap:false`. Do not add an
unconditional `await` of an optional/undefined safety gate to Claude paths.
`legacy_unobserved`, `legacy_dispatched` and `legacy_in_process` preserve today's
behavior and never grant a Codex permit. Setup currently precedes the run's
`try/finally`; extraction must not silently move that boundary either.

Supervisor evidence now includes actual app-server/code-mode hosts, active and
yielded cells, and separate setsid/double-fork/non-SIGCHLD-clone controls.
`supervisor_echild` denotes observed `ECHILD` with `__WALL` over recorded owned
descendants, not leader exit or RPC acknowledgement. The broader outer gate and
its runner sinks are still a proposed production contract, not implemented M0 code.

## Exact source map and M2 handoff

All references below are paths plus searchable symbols at the SHA above.

| Source | D0 behavior to move or preserve |
|---|---|
| `agent/src/sdk-executor.ts` `SdkQueryFn`, `ContextUsageReading`, `readLeadContext` | Existing queryFn injection remains accepted so existing tests need no edit; optional control getter, 2000ms context bound, context errors swallowed. |
| `agent/src/sdk-executor.ts` `phaseSetup`, `baseOptions` | Sparse env; literal settingSources []; skills plugin with skipMcpDiscovery; explicit skills; agents; in-process servers; bypassPermissions plus allowDangerouslySkipPermissions; async-deferral denial; exact four PreToolUse matchers; includePartialMessages false; model/effort keys omitted when absent; attribution false alone adds settings. |
| `agent/src/sdk-executor.ts` `driveTurn` | Any event resets idle; first run callback/last turn session ID; grouped usage/filtering; display-based context and stall signals; immediate onProgress; signal folds; root abort at result; trip > throw > terminal failure precedence; clean EOF behavior. |
| `agent/src/sdk-executor.ts` `trip`, `armWall`, `disarmWall` | First trip wins; active turn abort plus current-child group kill; wall budget pauses outside turns and remains workflow policy. |
| `agent/src/sdk-messages.ts` `mapAssistant`, `mapUser`, `mapResult`, `mapSdkMessage` | Empty text/thinking omitted; unknown blocks ignored; tool-use keys explicit; arbitrary structured tool results and is_error===true; exact success/error result fields and unguarded accounting; only system/init persisted. |
| `agent/src/sdk-messages.ts` `attributionOf`, `orphanInstanceKind`, `assistantUsageOf`, `assistantModelOf` | Independent agent/instance/label field probes; no cross-frame correlation; content-free orphan warning; assistant-only object-shaped usage and string model. |
| `agent/src/signals.ts` `isSubagentFrame`, `scanSignals` | Nonempty marker checks; assistant tool_use only; main-only signal acceptance; exact validation/caps and folds. |
| `agent/src/limit.ts` `RateLimitObserver`, `normalizeResetsAt`, `classifyLimitFailure`, `LimitReachedError` | Latest observation; seconds/ms normalization; explicit terminal or rejected+future classification; success never limits; exception class and message. |
| `agent/src/agents.ts` `toDefinition`, `assembleAgents`, `subagentsFromTemplates`, `planTurnSubagents`, `selectSubagents`, `applySubagentModelOverride` | Prompt appends, omission/inheritance, deny list, explicit MCP reach, exact skill allocation/order, own-vs-repo lead handling, plan copies and drop rules. |
| `agent/src/model-pass.ts` `runReadOnlyModelPass`, `consume`, `awaitQuerySettled`, `safeReportFailed` | Separate ephemeral HOME/uid mode, sparse env/no cwd, deny-all tools and source isolation, exact timeout/grace/cleanup, callback-vs-default error authority; independent `reason.slice(0,500)` advice failure cap (UTF-16 code units, despite the source comment saying bytes). |
| `agent/src/judge-runner.ts` `runModel`, `judge`, `execute` | Typed inner limit throw; deterministic outer fallback on every model error; success usage only; usage posted before review and completed. |
| `agent/src/review-runner.ts` `runModel`, `review`; `agent/src/summary-runner.ts` `runModel` and public generators | Generic error labels; review fallback and summary-null behavior; caller-owned tolerant JSON parsers. |
| `agent/src/sdk-session.ts` `sessionTranscriptResolvable`; `agent/src/runner.ts` resume preflight | UUID shape failure and I/O uncertainty keep resume; only proven absence drops it. Existing bool can wrap a new tri-state lookup without changing callers. Keep existing warn/feed placement. |
| `agent/src/sdk-executor.ts` `run`, `killAgentTree`; `agent/src/runner.ts` `checkpoint` closure and killAgentTree calls | Setup currently occurs before run's try/finally; no hidden new cleanup scope in extraction. Recorded PID set is per run. Reap only at current safe sites, not reap:false iteration fetches. |
| `agent/src/sdk-spawn.ts` `spawnDetached`, `killProcessGroup`; `agent/src/runner-uid.ts` `killRunnerGroup` | Detached runner-uid launch; synchronous kill dispatch and fallback; boolean means dispatch, not observed process-tree death. |
| `agent/src/chat-executor.ts` `driveTurn` | Shared mapper/query compatibility must remain; chat stays Claude and must not accidentally acquire run reducer signal filtering/context behavior. |

Exhaustive source consumer search used (tracked content):

```sh
git grep -n -E 'mapSdkMessage|isErrorResult|assistantUsageOf|assistantModelOf|orphanInstanceKind|sessionIdOf|isResult|promptStream|defaultQueryFn|SdkQueryFn|ContextUsageReading' -- agent/src
```

Besides the files above it finds `judge-runner-stub.ts`; keep its queryFn seam.
Do not replace the stub's existing injection shape as collateral cleanup.

Suggested eventual file ownership, with names provisional until M0 acceptance:

* Create `agent/src/harness.ts`: neutral contracts above, no SDK imports.
* Create `agent/src/harness-reducer.ts`: move signal/grouped-emission/turn-result
  logic; adapt pure signal parsers without changing their behavior.
* Create `agent/src/claude-harness.ts`: move query options, decoder, query process
  ownership and session inspection; retain existing queryFn injection shim.
* Keep `sdk-executor.ts` as workflow owner and constructor compatibility entry;
  keep `sdk-messages.ts` callable for chat and existing tests. An extraction can
  have compatibility exports; it must not leave two independent mappers.
* Keep `model-pass.ts` as advice timeout/callback compatibility owner around the
  new advice adapter; judge/review/summary keep their exact outer policies.
* Defer production Codex adapter/tool transport implementation to M3. M0 only
  resolves these contracts and supplies the missing characterization evidence.

Acceptance for later M2: all existing `agent/test` files unchanged; the existing
agent gate passes; source options and old emitted-message objects match current
fixtures including undefined keys, grouped usage, replay attribution, subtype
errors, callback authority and cleanup timing. New differential tests may live
outside the forbidden existing-behavior-test diff if the lead explicitly scopes
them, but do not weaken or rewrite a current test to fit this proposal.

## Risks and open questions

* Code-mode, representative broker-policy and actual-host disposal fixtures now
  provide bounded evidence in the ADR. Acceptance of the integrated design and
  neutral contract remains M0 work; these types alone establish no enforcement.
  Production handler coverage and the outer runner permit remain later code work.
* The wider PRD's credential identity/CAS/routing/API-key pricing blockers remain.
  The credential union and cost representation must not be mistaken for solved
  row-selection, refresh-writeback or metering contracts.
* M2 cannot both promise unchanged Claude behavior and impose a newly observed
  process-death guarantee on the old `killAgentTree` path. The explicit legacy
  result states expose that boundary; they are not accepted Codex fallbacks.
* M0 review must accept or revise the uzi wire-preservation capsule. Removing it
  requires a scoped message/accounting contract migration, which is beyond D0.
* Existing server folds are still coupled to current usage payloads. This M0
  type proposal avoids changing them; per-turn Codex idempotence and metered-cost
  folding need the separately tracked API design before M3/M5 can ship.
