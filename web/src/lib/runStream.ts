// Pure, framework-free logic behind the live run view: the lossless replay/merge
// of the seq-numbered message stream, and the Start-run precondition gate. Kept
// out of the React components so both can be unit-tested in isolation (the SPA
// has no component test harness — see runStream.test.ts).

import type { RunMessage, WsEvent } from "./api";
import type { HarnessSelection } from "./harnessSelection";

// StreamState is the client's view of a run's message log. `messages` are the
// contiguously-rendered messages in ascending seq order; `lastSeq` is the highest
// contiguous seq rendered; `pending` buffers messages that arrived ahead of a gap
// (a WS frame delivered before an earlier one) until the gap is filled.
export interface StreamState {
  messages: RunMessage[];
  lastSeq: number;
  pending: Map<number, RunMessage>;
}

export function emptyStream(): StreamState {
  return { messages: [], lastSeq: 0, pending: new Map() };
}

// ingest merges one message — from REST replay or a live WS frame — into the
// stream and reports whether a seq gap was detected (the caller then REST-replays
// from lastSeq to fill it). This is what makes the stream lossless and dup-free
// across a reconnect: the persisted log is authoritative and every path funnels
// through these three rules.
//
//   - seq <= lastSeq        already rendered → dedup, ignore.
//   - seq == lastSeq + 1    append, advance, then drain any now-contiguous pending.
//   - seq  > lastSeq + 1    buffer as pending and report a gap.
export function ingest(state: StreamState, msg: RunMessage): { state: StreamState; gap: boolean } {
  if (msg.seq <= state.lastSeq) {
    return { state, gap: false };
  }
  if (msg.seq > state.lastSeq + 1) {
    if (state.pending.has(msg.seq)) {
      return { state, gap: false };
    }
    const pending = new Map(state.pending);
    pending.set(msg.seq, msg);
    return { state: { messages: state.messages, lastSeq: state.lastSeq, pending }, gap: true };
  }
  const messages = state.messages.slice();
  const pending = new Map(state.pending);
  let lastSeq = msg.seq;
  messages.push(msg);
  while (pending.has(lastSeq + 1)) {
    const next = pending.get(lastSeq + 1)!;
    pending.delete(lastSeq + 1);
    messages.push(next);
    lastSeq = next.seq;
  }
  return { state: { messages, lastSeq, pending }, gap: false };
}

// ingestMany folds a batch (a REST replay page, always ascending) through ingest.
export function ingestMany(
  state: StreamState,
  msgs: RunMessage[],
): { state: StreamState; gap: boolean } {
  let cur = state;
  let gap = false;
  for (const m of msgs) {
    const r = ingest(cur, m);
    cur = r.state;
    gap = gap || r.gap;
  }
  return { state: cur, gap };
}

// FrameEffects are the side-effects a WS frame asks the caller to perform, kept
// out of the pure merge so the hook can debounce/coalesce them.
export interface FrameEffects {
  // replay: REST-replay from lastSeq. The caller MUST route this through its
  // debounced catch-up path so a burst of frames does not spam REST.
  replay: boolean;
  // refreshRun: re-read the run row (status/plan/mr/branch changed).
  refreshRun: boolean;
}

// applyFrame folds one live WS frame into the stream and reports the side-effects
// it triggers. A "message" frame is ingested (a seq gap asks for a replay to fill
// it). A "state" frame carries NO authoritative data — it asks for a run re-read
// AND a replay: the message that preceded the state report is persisted before
// the report, so if the hub dropped that tail message (a slow-subscriber drop
// right as the run went quiescent), the replay backfills it without a reconnect.
export function applyFrame(
  state: StreamState,
  frame: WsEvent,
): { state: StreamState; effects: FrameEffects } {
  if (frame.type === "message" && typeof frame.seq === "number") {
    const msg: RunMessage = {
      seq: frame.seq,
      kind: frame.kind ?? "",
      agent: frame.agent ?? null,
      // PRD #99: the lane identity rides the frame, so a live subagent message
      // lanes correctly with no REST re-read. Absent ⇒ null (the lead, an infra
      // frame, or a pre-migration replay), which the pane falls back off.
      agent_instance: frame.agent_instance ?? null,
      agent_label: frame.agent_label ?? null,
      payload: frame.payload,
      created_at: frame.created_at ?? new Date().toISOString(),
    };
    const r = ingest(state, msg);
    return { state: r.state, effects: { replay: r.gap, refreshRun: false } };
  }
  if (frame.type === "state") {
    return { state, effects: { replay: true, refreshRun: true } };
  }
  if (frame.type === "health") {
    // A run-health flag changed (PRD #47). Like "state" it carries no authoritative
    // data — re-read the run to pick up the (owner-gated) health fields. No replay:
    // a health flip does not imply a dropped tail message, so unlike "state" it need
    // not backfill the stream.
    return { state, effects: { replay: false, refreshRun: true } };
  }
  return { state, effects: { replay: false, refreshRun: false } };
}

// StartRunPreconditions are the facts the board knows about an issue + the user.
//
// PRD #764 removed the PRD-link precondition: a run no longer requires a linked
// prds/*.md file (the single `uzi` label is the eligibility gate, checked upstream by
// whether the card is runnable at all). The worker/token/closed/active preconditions
// stay.
//
// PRD #1429 M4a, D3 made the credential gate harness-aware: `hasToken` (Anthropic-only)
// is replaced by `claudeUsable`/`codexUsable` (mirroring the server's D11 availability
// rule — see lib/hasToken.ts's isCodexUsable) plus the `harness` this start will
// actually use ("inherit" lets the server's D11 resolver pick, the common case for a
// single-harness user whose picker is hidden). `hasCodexCredential` is used only to
// pick harness-appropriate copy when NEITHER harness is usable.
export interface StartRunPreconditions {
  closed: boolean;
  hasWorker: boolean;
  claudeUsable: boolean;
  codexUsable: boolean;
  // Any Codex-kind secret exists at all (usable or not) — see hasAnyCodexCredential.
  hasCodexCredential: boolean;
  harness: HarnessSelection;
  activeRunExists: boolean;
}

export interface StartRunGate {
  enabled: boolean;
  reason: string;
}

// startRunGate decides whether "Start run" is offered for a card, and if not, the
// single clearest reason. Order matters: the reason shown is the first unmet
// precondition, cheapest-to-fix last so the user is nudged toward the real
// blocker. Mirrors the server's own CreateRun rejections.
//
// The credential check is harness-aware (PRD #1429 D3): an EXPLICIT harness choice
// checks only that harness's usability (a Codex-only user picking Codex is never told
// to add an Anthropic token); "inherit" (the common single-harness path) checks that
// AT LEAST ONE harness is usable, since the server's D11 resolver will pick whichever
// is — and its copy stays the pre-M4a Anthropic wording unless the user has already
// started down the Codex path (hasCodexCredential), keeping a Claude-only user's flow
// byte-identical to today.
export function startRunGate(p: StartRunPreconditions): StartRunGate {
  if (p.closed) {
    return { enabled: false, reason: "This issue is closed." };
  }
  if (!p.hasWorker) {
    return { enabled: false, reason: "Connect a worker first (Settings → Workers)." };
  }
  if (p.harness === "codex") {
    if (!p.codexUsable) {
      return { enabled: false, reason: "Add a usable Codex credential first (Settings)." };
    }
  } else if (p.harness === "claude") {
    if (!p.claudeUsable) {
      return { enabled: false, reason: "Add your Anthropic token first (Settings)." };
    }
  } else if (!p.claudeUsable && !p.codexUsable) {
    return {
      enabled: false,
      reason: p.hasCodexCredential
        ? "Finish setting up a usable credential first (Settings)."
        : "Add your Anthropic token first (Settings).",
    };
  }
  if (p.activeRunExists) {
    return { enabled: false, reason: "A run is already in progress for this issue." };
  }
  return { enabled: true, reason: "" };
}
