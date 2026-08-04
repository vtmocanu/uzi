// In-memory store + event bus for mock mode. Mirrors the real system's one
// load-bearing rule: a message is PERSISTED first (appended to the run's log
// with the next gapless seq), then BROADCAST to any live mock sockets. The mock
// socket is therefore a live cache over this store, exactly like /api/ws is
// over Postgres, so useRunStream's replay/merge logic runs unmodified.

import type { Board, IssueProposal, LatestRun, Run, RunMessage, RunStatus, User, WsEvent } from "../lib/api";
import {
  mockAdmin,
  mockAwaitingMessages,
  mockBoards,
  mockBusyMessages,
  mockChatMessages,
  mockChatRuns,
  mockCrewMessages,
  mockCrewRuns,
  mockDegradedMessages,
  mockDoneMessages,
  mockFailedMessages,
  mockLaneMessages,
  mockLaneRuns,
  mockLimitWaitMessages,
  mockProposals,
  mockRuns,
  mockSeededMessages,
  mockUnreadableQuestionMessages,
} from "./data";

export interface MockState {
  session: User | null;
  runs: Map<string, Run>;
  messages: Map<string, RunMessage[]>;
  boards: Map<string, Board>;
  // Issue proposals from chat (PRD #39): keyed by proposal id, mutated by the
  // confirm/dismiss mock endpoints. The card in the transcript renders from its
  // run_message payload; this map is the authoritative status the actions update.
  proposals: Map<string, IssueProposal>;
  // Vault unlocked state (PRD #32). Starts unlocked so the demo is fully usable;
  // the Lock vault action flips it so the badge, banner, and "waiting for vault
  // unlock" run state are all browsable.
  vaultUnlocked: boolean;
}

function seed(): MockState {
  const runs = new Map<string, Run>();
  for (const r of mockRuns) runs.set(r.id, { ...r });
  // Chat conversations ride the same run + message maps (PRD #39).
  for (const r of mockChatRuns) runs.set(r.id, { ...r });
  // Crew-roster demo runs (PRD #95 M2): health-varied so every crew state renders.
  for (const r of mockCrewRuns) runs.set(r.id, { ...r });
  // PRD #99 instance-lane demo runs: the only fixtures carrying a non-null
  // agent_instance, so they are the only ones that render the By-agent lane view as
  // anything other than legacy role lanes.
  for (const r of mockLaneRuns) runs.set(r.id, { ...r });
  const messages = new Map<string, RunMessage[]>();
  messages.set("run-crew", mockCrewMessages.map((m) => ({ ...m })));
  messages.set("run-lanes", mockLaneMessages.map((m) => ({ ...m })));
  messages.set("run-busy", mockBusyMessages.map((m) => ({ ...m })));
  // Same stream, degraded health — see the run-stalled comment in data.ts.
  messages.set("run-stalled", mockBusyMessages.map((m) => ({ ...m })));
  messages.set("run-degraded", mockDegradedMessages.map((m) => ({ ...m })));
  messages.set("run-done", [...mockDoneMessages]);
  // run-closed is a completed run (its MR was later closed unmerged); reuse the
  // done stream so it also shows the run-view usage surfaces (PRD #40 web-ux).
  messages.set("run-closed", [...mockDoneMessages]);
  messages.set("run-awaiting", [...mockAwaitingMessages]);
  // PRD #209 M5: the seeded-plan demo run. Its plan rides run.plan_md (SeededPlanPanel),
  // NOT a feed message, so this log carries none — see mockSeededMessages.
  messages.set("run-seeded", mockSeededMessages.map((m) => ({ ...m })));
  // PRD #88: the run parked on a question the UI cannot render. Seeded rather than
  // scripted, so the empty state is reachable by URL without walking the journey.
  messages.set("run-unreadable-question", [...mockUnreadableQuestionMessages]);
  messages.set("run-failed", [...mockFailedMessages]);
  // PRD #35: the only stream carrying `limit_wait` / `limit_hit` rows. The
  // degraded-countdown fixture shares it — the rows are the same shape and it exists
  // for the run-row states (expired stamp, 7-day window, suppressed attempt), not for
  // a different feed.
  messages.set("run-limit-wait", [...mockLimitWaitMessages]);
  messages.set("run-limit-wait-due", mockLimitWaitMessages.map((m) => ({ ...m })));
  messages.set("run-live", []);
  messages.set("run-cancelled", []);
  // The never-judged control fixture (PRD #119): a terminal run with no review AND no
  // pending judge, so the run page's unchanged "hasn't been judged yet" empty state and
  // its live Run-judge button stay demoable — run-failed used to be that fixture and now
  // carries a scheduled judge. An empty log rather than no entry: getRunMessages 404s on
  // an unseeded id, which the real API does not do for a run that exists.
  messages.set("run-unjudged", []);
  for (const [id, log] of Object.entries(mockChatMessages)) messages.set(id, log.map((m) => ({ ...m })));
  const proposals = new Map<string, IssueProposal>();
  for (const p of mockProposals) proposals.set(p.id, { ...p });
  const boards = new Map<string, Board>();
  for (const [id, b] of Object.entries(mockBoards)) {
    boards.set(id, { ...b, columns: [...b.columns], cards: b.cards.map((c) => ({ ...c })) });
  }
  // Auth is instant/fake in mock mode: the session starts signed in as admin so
  // the whole app is browsable with zero steps. Logout still works (and any
  // login/register signs straight back in).
  return { session: { ...mockAdmin }, runs, messages, boards, proposals, vaultUnlocked: true };
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
  // PRD #99: optional so every existing caller keeps its NULL-instance behaviour,
  // but present so a mock broadcast CAN carry a lane identity. Without it the mock
  // socket could only ever append role-keyed frames, and the live path this PRD
  // cares about most — a new agent_instance opening a new lane mid-run — had no way
  // to be exercised in mock mode.
  attribution: { instance?: string | null; label?: string | null } = {},
): RunMessage {
  const log = state.messages.get(runId) ?? [];
  const msg: RunMessage = {
    seq: (log[log.length - 1]?.seq ?? 0) + 1,
    kind,
    agent,
    agent_instance: attribution.instance ?? null,
    agent_label: attribution.label ?? null,
    payload,
    created_at: new Date().toISOString(),
  };
  log.push(msg);
  state.messages.set(runId, log);
  broadcast(runId, { type: "message", ...msg });
  return msg;
}

// The LatestRun fields a board card MIRRORS from the run row. Enumerated rather than
// spread wholesale because LatestRun is a projection, not a subset: it carries derived
// fields (run_count, is_mine, owner_name, worker_name) that a run patch must never
// clobber, and Run carries a great deal a card has no business holding.
const CARD_MIRRORED_FIELDS = [
  "status",
  "mr_iid",
  "mr_web_url",
  "mr_state",
  "failure_reason",
  "stop_kind",
  "health",
  "health_reason",
  "health_since",
] as const;

// The other half of the partition: fields a card carries that a RUN PATCH must never
// write. `id` is the join key; `updated_at` is stamped by syncCards itself; the rest are
// projections the server computes for the board and that no run row holds.
const CARD_OWN_FIELDS = [
  "id",
  "updated_at",
  "owner_name",
  "worker_name",
  "is_mine",
  "run_count",
  "created_at",
] as const;

// 🔴 COMPILE-TIME EXHAUSTIVENESS, and it is the point of splitting the list in two.
//
// The two arrays above must PARTITION `keyof LatestRun`. Without this line the
// enumeration is correct only when written: a field added to LatestRun elsewhere
// produces NO conflict in a merge, NO type error, and NO failing test — the card simply
// stops mirroring it, silently. That is the same shape as an enumeration invalidated by
// something that moved in another file, which this repo has been bitten by before.
//
// If this stops compiling, LatestRun gained (or lost) a field: put it in exactly one of
// the two lists. Do NOT widen the type to make it build.
type UnpartitionedCardField = Exclude<
  keyof LatestRun,
  (typeof CARD_MIRRORED_FIELDS)[number] | (typeof CARD_OWN_FIELDS)[number]
>;
const _cardFieldsArePartitioned: UnpartitionedCardField extends never ? true : never = true;
void _cardFieldsArePartitioned;

// syncCards propagates a run patch onto any board card whose latest_run IS this run.
//
// 🔴 WITHOUT THIS THE DEMO CONTRADICTS ITSELF, measured in the browser: the board's
// attention strip read "1 run needs an answer" while the very card it linked to was
// badged "awaiting approval", because the strip comes from listRuns (live) and the card
// from the board fixture (frozen at seed). Not specific to PRD #88 — every scripted
// status transition has been invisible on the board since the mock was written; the
// clarification park is just the first one whose strip and card were on screen together
// to disagree out loud.
//
// It also makes the awaiting_input CARD reachable at all. The badge, the warn ring and
// needsHumanAttention are product code with unit tests, but the surface was un-driven
// end to end: no fixture seeds that status, so nothing but this sync can produce it.
function syncCards(runId: string, patch: Partial<Run>): void {
  for (const board of state.boards.values()) {
    for (const card of board.cards) {
      if (card.latest_run?.id !== runId) continue;
      const merged = { ...card.latest_run, updated_at: new Date().toISOString() };
      for (const key of CARD_MIRRORED_FIELDS) {
        if (key in patch) (merged as Record<string, unknown>)[key] = patch[key];
      }
      card.latest_run = merged;
    }
  }
}

export function patchRun(runId: string, patch: Partial<Run>): Run | undefined {
  const run = state.runs.get(runId);
  if (!run) return undefined;
  const next = { ...run, ...patch, updated_at: new Date().toISOString() };
  state.runs.set(runId, next);
  syncCards(runId, patch);
  if (patch.status && patch.status !== run.status) {
    broadcast(runId, { type: "state", status: patch.status as RunStatus });
  }
  return next;
}

export function getRun(runId: string): Run | undefined {
  return state.runs.get(runId);
}

// listMessages is a read-only view of a run's log. The engine derives from it rather
// than keeping a parallel counter, so a mock script folds the feed the same way the real
// UI does — the derivation IS the contract under test (PRD #88 D-L), and a second source
// of truth in the mock would be a place for the two to disagree.
export function listMessages(runId: string): readonly RunMessage[] {
  return state.messages.get(runId) ?? [];
}

let runCounter = 0;
export function nextRunId(): string {
  return `run-new-${++runCounter}`;
}

// ── Issue proposals (PRD #39) ────────────────────────────────────────────────

export function getProposal(id: string): IssueProposal | undefined {
  return state.proposals.get(id);
}

// putProposal upserts a proposal row (create on scheduleChatReply, mutate on
// confirm/dismiss). No broadcast — the card is driven by its run_message payload
// and the action's response, not a state frame.
export function putProposal(p: IssueProposal): IssueProposal {
  state.proposals.set(p.id, p);
  return p;
}

let proposalCounter = 0;
export function nextProposalId(): string {
  return `prop-new-${++proposalCounter}`;
}
