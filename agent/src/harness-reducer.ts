// The run-lane turn reducer (PRD #1146 / #1106 M2, milestone m1).
//
// The pure fold that today's `sdk-executor.ts` driveTurn (L2566-2755) performed
// inline over the SDK stream, lifted onto the neutral {@link HarnessEvent} record
// so it is SDK-FREE: it imports ONLY the neutral contract (`harness.ts`) and the
// projection core (`harness-messages.ts`). It never touches the SDK, sdk-messages'
// SDK path, signals.ts or limit.ts — the adapter (`claude-harness.ts`) does the
// SDK-shaped decoding and hands this neutral events.
//
// Lifetime is PER RUN: `reportedSessionId` (the first-truthy-once-per-run latch)
// persists across turns, while the signal accumulator, lead-text buffer,
// subagent-activity flag, per-turn session id and the context-requested guard all
// RESET at each turn boundary — driven by the owner's `beginTurn` at the top of every
// turn, so a prior turn that ended by throw (skipping `finish`) cannot leak per-turn
// state into the next. The reducer does NOT own
// trip/precedence/limit-classify/materialize — those stay with the workflow owner,
// which also delivers ctx.emit / onProgress / ctx.onSessionId / orphan-warn from
// the reductions this returns.

import type { EmittedMessage } from "./executor.js";
import type {
  HarnessContextHook,
  HarnessEvent,
  ReducedTurnCompletion,
  ReducedTurnResult,
  RunTurnReducer,
  TurnReduction,
  TurnSignals,
  TurnStreamEnd,
} from "./harness.js";
import { projectInit, projectItem, projectResult } from "./harness-messages.js";

const LEAD = "lead";

export class RunTurnReducerImpl implements RunTurnReducer {
  /** First-truthy-once-per-run latch (run-level; persists across turns). */
  private reportedSessionId = false;

  // --- Per-turn state, reset at each turn boundary by the owner's `beginTurn`. ---
  private result: ReducedTurnResult = { done: false };
  /** The lead's own text this turn, in emit order (no-progress detector input). */
  private leadText: string[] = [];
  /** The last truthy session id seen within the turn (the per-turn result id). */
  private turnSessionId: string | undefined;
  /** Fired the lead context read once per turn (mirrors driveTurn's contextPromise
   *  "already fired" guard). */
  private contextRequested = false;

  constructor(private readonly context: HarnessContextHook) {}

  /** Reset the per-turn accumulators for the turn about to stream. The OWNER calls
   *  this at the top of every turn, before the first `accept`. Because it fires
   *  unconditionally each turn (not off `finish`), each turn begins with clean
   *  per-turn state structurally, robust to a prior turn that ended by throw and
   *  SKIPPED `finish`. The run-level `reportedSessionId` latch is NOT reset here. */
  beginTurn(): void {
    this.resetPerTurn();
  }

  async accept(event: HarnessEvent): Promise<TurnReduction> {
    const reduction: TurnReduction = { messages: [], diagnostics: [] };

    // Session id is accepted lazily off ANY event (including ignored/partial
    // frames): last-truthy within the turn for the per-turn result, and first-
    // truthy once per run for the run callback. Empty string never advances.
    if (event.sessionId) {
      this.turnSessionId = event.sessionId;
      if (!this.reportedSessionId) {
        this.reportedSessionId = true;
        reduction.firstSessionId = event.sessionId;
      }
    }

    // The existing content-free orphan detector runs on every raw message; the
    // owner logs it (this reducer has no logger).
    if (event.orphanInstanceFrameKind !== undefined) {
      reduction.diagnostics = [
        { kind: "orphan_instance", frameKind: event.orphanInstanceFrameKind },
      ];
    }

    switch (event.kind) {
      case "activity":
        break;
      case "initialized":
        reduction.messages.push(projectInit(event.model));
        break;
      case "frame":
        this.acceptFrame(event, reduction);
        break;
      case "turn_finished":
        await this.acceptTerminal(event, reduction);
        break;
    }
    return reduction;
  }

  private acceptFrame(
    event: Extract<HarnessEvent, { kind: "frame" }>,
    reduction: TurnReduction,
  ): void {
    const at = event.attribution;
    // issue #1197 (D-RC2a): a frame that carries model output (items) or usage is
    // POSITIVE evidence the model did something this turn. Latched broadly on entry —
    // any assistant/tool frame or attached usage — so the empty-turn detector below
    // (numTurns === 0 && !sawModelActivity) only fires for a turn that streamed no
    // model output at all. Signal-only tool_use frames (e.g. submit_plan) also set it,
    // but those turns are never positively-empty anyway (they set plan/questions).
    if (event.items.length > 0 || event.usage) {
      this.result.sawModelActivity = true;
    }
    // usageAttached is PER-FRAME: the frame's per-call usage + model ride the FIRST
    // item that survives the signal filter (co-gated — model only where usage is).
    let usageAttached = false;
    for (const item of event.items) {
      // Group-before-filter: drop every recognized signal tool_use from persisted
      // output (the adapter marked them). Tool RESULTS are never dropped.
      if (item.kind === "tool" && item.phase === "started" && item.signal !== undefined) {
        continue;
      }
      const em: EmittedMessage = projectItem(item, at);
      // Display-based no-progress derivation: the lead's own text (verbatim-repeat
      // input) and whether any subagent produced a frame this turn (work in flight).
      if (em.kind === "text" && em.agent === LEAD) {
        const t = em.payload["text"];
        if (typeof t === "string" && t) this.leadText.push(t);
      }
      if (em.agent !== undefined && em.agent !== LEAD) {
        this.result.subagentActivity = true;
      }
      if (event.usage && !usageAttached) {
        em.payload["usage"] = event.usage.wire?.usage;
        if (event.model !== undefined) em.payload["model"] = event.model;
        usageAttached = true;
        // issue #1197 (D-RC2a): usage folded ⇒ model activity (belt-and-braces beside
        // the on-entry latch above; kept here so a future refactor of the entry guard
        // cannot silently drop the usage signal).
        this.result.sawModelActivity = true;
        // Fire the lead context read once per turn, on the first surviving
        // usage-bearing item whose displayed agent is the lead — NOT awaited here
        // (the adapter's read runs concurrently while the turn streams).
        if (!this.contextRequested && em.agent === LEAD) {
          this.context.request();
          this.contextRequested = true;
        }
      }
      reduction.messages.push(em);
    }

    // Authorize signal reduction using origin, separately from presentation
    // attribution. Claude marks a nonempty subagent_type OR parent_tool_use_id as a
    // subagent (the adapter already returns {} signals for those); this main-only
    // gate is the load-bearing guarantee independent of that.
    if (event.origin.kind === "main" && event.signals) {
      this.foldSignals(event.signals, reduction);
    }
  }

  /** Preserve the exact signal folds (driveTurn L2684-2719): plan/milestones and
   *  the declaration fields are last-wins; done/checkpoint/reportOnly latch;
   *  questions concatenate; progress is last-wins AND delivered immediately via the
   *  reduction so the owner reports it off the hot stream without awaiting. */
  private foldSignals(s: Readonly<Partial<TurnSignals>>, reduction: TurnReduction): void {
    if (s.plan !== undefined) this.result.plan = s.plan;
    if (s.milestones) this.result.milestones = s.milestones;
    if (s.progress) {
      this.result.progress = s.progress;
      reduction.progress = s.progress;
    }
    if (s.done) this.result.done = true;
    if (s.checkpoint) this.result.checkpoint = true;
    if (s.prdDonePath !== undefined) this.result.prdDonePath = s.prdDonePath;
    if (s.milestonesCompleted !== undefined) {
      this.result.milestonesCompleted = s.milestonesCompleted;
    }
    if (s.summary !== undefined) this.result.summary = s.summary;
    if (s.reportOnly) this.result.reportOnly = true;
    if (s.proposal !== undefined) this.result.proposal = s.proposal;
    if (s.questions?.length) {
      this.result.questions = [...(this.result.questions ?? []), ...s.questions];
    }
  }

  private async acceptTerminal(
    event: Extract<HarnessEvent, { kind: "turn_finished" }>,
    reduction: TurnReduction,
  ): Promise<void> {
    const terminal = event.terminal;
    // issue #1197 (D-RC2a): the SDK terminal's positively-reported turn count is typed
    // `unknown` on the wire. Coerce it ONLY when it is a finite number (mirroring
    // limit.ts normalizeResetsAt's guard shape); missing/garbage metrics MUST stay
    // undefined — NEVER default to 0 — so a turn that ran but reported no count is not
    // mistaken for a positively-empty (zero-turn) result.
    const n = terminal.metrics.wire?.num_turns;
    if (typeof n === "number" && Number.isFinite(n)) this.result.numTurns = n;
    const em = projectResult({
      outcome: terminal.outcome,
      subtype: terminal.subtype,
      errors: terminal.errors,
      wire: {
        usage: terminal.usage?.wire?.usage,
        modelUsage: terminal.usage?.wire?.modelUsage,
        num_turns: terminal.metrics.wire?.num_turns,
        duration_ms: terminal.metrics.wire?.duration_ms,
        total_cost_usd: terminal.metrics.wire?.total_cost_usd,
      },
    });
    // Attach the (concurrently-read) lead context to the turn's terminal frame on
    // success OR failure. Only turn completion waits, and only up to the read's own
    // timeout on a genuine hang; absence/error/hang omit context and never fail.
    if (this.contextRequested) {
      const context = await this.context.get();
      if (context) em.payload["context"] = context;
    }
    reduction.messages.push(em);
  }

  finish(end: TurnStreamEnd): ReducedTurnCompletion {
    // The per-turn result carries the last truthy session id seen this turn and the
    // lead's concatenated text. The owner maps a clean EOF (`exhausted`) to the
    // accumulated result, exactly as legacy Claude returns accumulated signals.
    this.result.sessionId = this.turnSessionId;
    if (this.leadText.length > 0) this.result.finalText = this.leadText.join("\n");
    const result = this.result;

    // Return the accumulated per-turn result; keep the run-level session latch. The
    // per-turn accumulators are NOT reset here — the owner's `beginTurn` is the single
    // source of truth for that, firing at the top of the next turn regardless of how
    // this one ended (clean finish, or a throw path that skips `finish` entirely).
    return { result, end };
  }

  /** Reset the per-turn accumulators; leaves the run-level `reportedSessionId` latch
   *  untouched. */
  private resetPerTurn(): void {
    // Replacing `this.result` wholesale also clears the issue #1197 (D-RC2a) evidence
    // fields (`numTurns`, `sawModelActivity`), which live on `this.result`, so each
    // turn begins with no carried-over empty-turn evidence.
    this.result = { done: false };
    this.leadText = [];
    this.turnSessionId = undefined;
    this.contextRequested = false;
  }
}
