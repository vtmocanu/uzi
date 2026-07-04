// Pure, framework-free logic behind the live run view: the lossless replay/merge
// of the seq-numbered message stream, and the Start-run precondition gate. Kept
// out of the React components so both can be unit-tested in isolation (the SPA
// has no component test harness — see runStream.test.ts).

import type { RunMessage } from "./api";

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

// StartRunPreconditions are the facts the board knows about an issue + the user.
export interface StartRunPreconditions {
  hasPrdLink: boolean;
  closed: boolean;
  hasWorker: boolean;
  hasToken: boolean;
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
export function startRunGate(p: StartRunPreconditions): StartRunGate {
  if (p.closed) {
    return { enabled: false, reason: "This issue is closed." };
  }
  if (!p.hasPrdLink) {
    return { enabled: false, reason: "Add a link to a prds/*.md file in the description first." };
  }
  if (!p.hasWorker) {
    return { enabled: false, reason: "Connect a worker first (Settings → Workers)." };
  }
  if (!p.hasToken) {
    return { enabled: false, reason: "Add your Anthropic token first (Settings)." };
  }
  if (p.activeRunExists) {
    return { enabled: false, reason: "A run is already in progress for this issue." };
  }
  return { enabled: true, reason: "" };
}
