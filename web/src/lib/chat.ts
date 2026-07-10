// Framework-free logic behind the Chat page (PRD #39, M4): worker-liveness
// derivation, the composer gate, the turn-cap notice, the queued-behind-active
// note, and conversation ordering. Kept out of the components so each rule is
// unit-tested without a DOM (same split as runStream.ts / useFollowScroll.ts).

import { isTerminalRun, type Chat, type Run, type RunMessage, type Worker } from "./api";

// CHAT_MAX_TURNS is the fallback per-conversation turn cap used before the
// GET /api/chats envelope (max_turns) has loaded. The authoritative value is the
// instance constant on that envelope (Decision 3b, WORKER default 50).
export const CHAT_MAX_TURNS = 50;

// chatFromRun maps a runDTO (POST /api/chats and .../continue return one under
// `run`) into the unified Chat view type the components consume. A chat runDTO
// carries title + resume_of_run_id; turn_count is not on the runDTO (it is a
// per-list computation) so a freshly created run starts at 0 until the list or
// stream refines it, and last_message_at is unknown here (null).
export function chatFromRun(run: Run): Chat {
  return {
    id: run.id,
    title: run.title,
    status: run.status,
    turn_count: 0,
    resume_of_run_id: run.resume_of_run_id,
    last_message_at: null,
    created_at: run.created_at,
    updated_at: run.updated_at,
  };
}

// hasOnlineWorker reports whether the user has at least one connected worker —
// the signal behind the worker-offline banner (Decision 15). Derived from the
// existing heartbeat-tracked worker list, so chat needs no new liveness endpoint.
export function hasOnlineWorker(workers: Worker[]): boolean {
  return workers.some((w) => w.status === "online");
}

// chatIsEnded: the conversation's run reached a terminal state (idle-completed,
// failed, or cancelled). A terminal run refuses inputs (ErrRunTerminal), so the
// UI offers Continue instead of a composer (Decision 11).
export function chatIsEnded(chat: { status: string }): boolean {
  return isTerminalRun(chat.status);
}

// countUserTurns derives the live turn count from the message stream — a
// user_message per turn (the schema-declared kind, §37). Lets the conversation
// view gate the composer off the same stream it already renders, no extra fetch.
export function countUserTurns(messages: RunMessage[]): number {
  return messages.reduce((n, m) => (m.kind === "user_message" ? n + 1 : n), 0);
}

export interface ComposerGate {
  enabled: boolean;
  reason: string; // "" when enabled
}

// composerGate decides whether the composer accepts input, and if not, the one
// clearest reason. A worker being offline is deliberately NOT a gate
// (Decision 15) — the message queues and is served when a worker connects — so
// it never disables here; the banner communicates it separately.
export function composerGate(gate: {
  status: string;
  turnCount: number;
  maxTurns: number;
}): ComposerGate {
  if (chatIsEnded(gate)) {
    return { enabled: false, reason: "This conversation has ended." };
  }
  if (gate.turnCount >= gate.maxTurns) {
    return { enabled: false, reason: `Turn limit reached (${gate.maxTurns} turns).` };
  }
  return { enabled: true, reason: "" };
}

// turnCapNotice is the heads-up shown above the composer as a conversation nears
// its cap; null until within the warn window. The hard stop is composerGate — this
// only warns. At/over the cap it explains the Continue/new-chat recovery.
export function turnCapNotice(
  gate: { turnCount: number; maxTurns: number },
  warnWithin = 5,
): string | null {
  const left = gate.maxTurns - gate.turnCount;
  if (left <= 0) {
    return `This conversation reached its ${gate.maxTurns}-turn limit. Start a new chat to keep going.`;
  }
  if (left <= warnWithin) {
    return `${left} turn${left === 1 ? "" : "s"} left in this conversation.`;
  }
  return null;
}

// queuedBehindActive: WORKER_CHAT_SESSIONS defaults to 1, so one live
// conversation per user-worker (Decision 4) — a second chat queues until the
// first ends. True when THIS chat is queued and some OTHER chat is still active.
export function queuedBehindActive(
  chat: { id: string; status: string },
  all: { id: string; status: string }[],
): boolean {
  if (chat.status !== "queued") return false;
  return all.some((c) => c.id !== chat.id && !isTerminalRun(c.status));
}

// conversationSortKey orders the list by most-recent activity (last message,
// falling back to the row's own updated_at when nothing has been said yet).
export function conversationSortKey(c: Chat): string {
  return c.last_message_at ?? c.updated_at;
}

export function sortConversations(chats: Chat[]): Chat[] {
  return [...chats].sort((a, b) => conversationSortKey(b).localeCompare(conversationSortKey(a)));
}

// conversationTitle is the display label for a conversation row — the derived
// title, or a stable placeholder before one exists.
export function conversationTitle(c: Chat): string {
  const t = (c.title ?? "").trim();
  return t === "" ? "Untitled chat" : t;
}
