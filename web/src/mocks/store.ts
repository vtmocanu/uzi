// In-memory store + event bus for mock mode. Mirrors the real system's one
// load-bearing rule: a message is PERSISTED first (appended to the run's log
// with the next gapless seq), then BROADCAST to any live mock sockets. The mock
// socket is therefore a live cache over this store, exactly like /api/ws is
// over Postgres, so useRunStream's replay/merge logic runs unmodified.

import type { Board, Run, RunMessage, RunStatus, User, WsEvent } from "../lib/api";
import {
  mockAdmin,
  mockAwaitingMessages,
  mockBoards,
  mockDoneMessages,
  mockFailedMessages,
  mockRuns,
} from "./data";

export interface MockState {
  session: User | null;
  runs: Map<string, Run>;
  messages: Map<string, RunMessage[]>;
  boards: Map<string, Board>;
  // Vault unlocked state (PRD #32). Starts unlocked so the demo is fully usable;
  // the Lock vault action flips it so the badge, banner, and "waiting for vault
  // unlock" run state are all browsable.
  vaultUnlocked: boolean;
}

function seed(): MockState {
  const runs = new Map<string, Run>();
  for (const r of mockRuns) runs.set(r.id, { ...r });
  const messages = new Map<string, RunMessage[]>();
  messages.set("run-done", [...mockDoneMessages]);
  messages.set("run-awaiting", [...mockAwaitingMessages]);
  messages.set("run-failed", [...mockFailedMessages]);
  messages.set("run-live", []);
  messages.set("run-cancelled", []);
  const boards = new Map<string, Board>();
  for (const [id, b] of Object.entries(mockBoards)) {
    boards.set(id, { ...b, columns: [...b.columns], cards: b.cards.map((c) => ({ ...c })) });
  }
  // Auth is instant/fake in mock mode: the session starts signed in as admin so
  // the whole app is browsable with zero steps. Logout still works (and any
  // login/register signs straight back in).
  return { session: { ...mockAdmin }, runs, messages, boards, vaultUnlocked: true };
}

export const state: MockState = seed();

// ── Event bus (one channel per run, like the WS hub) ─────────────────────────

type Listener = (frame: WsEvent) => void;
const listeners = new Map<string, Set<Listener>>();

export function subscribe(runId: string, fn: Listener): () => void {
  let set = listeners.get(runId);
  if (!set) {
    set = new Set();
    listeners.set(runId, set);
  }
  set.add(fn);
  return () => {
    set!.delete(fn);
  };
}

function broadcast(runId: string, frame: WsEvent) {
  for (const fn of listeners.get(runId) ?? []) fn(frame);
}

// ── Persist-first primitives ─────────────────────────────────────────────────

export function appendMessage(
  runId: string,
  kind: string,
  agent: string | null,
  payload: unknown,
): RunMessage {
  const log = state.messages.get(runId) ?? [];
  const msg: RunMessage = {
    seq: (log[log.length - 1]?.seq ?? 0) + 1,
    kind,
    agent,
    payload,
    created_at: new Date().toISOString(),
  };
  log.push(msg);
  state.messages.set(runId, log);
  broadcast(runId, { type: "message", ...msg });
  return msg;
}

export function patchRun(runId: string, patch: Partial<Run>): Run | undefined {
  const run = state.runs.get(runId);
  if (!run) return undefined;
  const next = { ...run, ...patch, updated_at: new Date().toISOString() };
  state.runs.set(runId, next);
  if (patch.status && patch.status !== run.status) {
    broadcast(runId, { type: "state", status: patch.status as RunStatus });
  }
  return next;
}

export function getRun(runId: string): Run | undefined {
  return state.runs.get(runId);
}

let runCounter = 0;
export function nextRunId(): string {
  return `run-new-${++runCounter}`;
}
