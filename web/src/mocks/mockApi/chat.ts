import {
  type Chat,
  type CreatedIssue,
  type Run,
  type RunMessage,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { appendMessage, getProposal, getRun, nextRunId, patchRun, putProposal, state } from "../store";
import { handleInput, scheduleChatReply } from "../engine";
import { delay } from "./shared";
import { runsApi } from "./runs";

const CHAT_MAX_TURNS = 50;

// chatDTO derives the chatListDTO shape from a chat run + its message log: the
// title (the run's chat title, else derived from the first user turn), the
// user-turn count, and last activity from the newest message (PRD #39 wire). No
// max_turns here — that rides the list envelope as an instance constant.
function chatDTO(run: Run): Chat {
  const msgs: RunMessage[] = state.messages.get(run.id) ?? [];
  const firstUser = msgs.find((m) => m.kind === "user_message");
  const derived = (firstUser?.payload as { text?: string } | null)?.text;
  const title = run.title ?? (derived ? truncateChatTitle(derived) : run.issue_title || null);
  const turnCount = msgs.reduce((n, m) => (m.kind === "user_message" ? n + 1 : n), 0);
  return {
    id: run.id,
    title,
    status: run.status,
    turn_count: turnCount,
    resume_of_run_id: run.resume_of_run_id,
    last_message_at: msgs[msgs.length - 1]?.created_at ?? null,
    created_at: run.created_at,
    updated_at: run.updated_at,
  };
}

function truncateChatTitle(s: string): string {
  const t = s.trim().replace(/\s+/g, " ");
  return t.length > 60 ? `${t.slice(0, 59)}…` : t;
}

export const chatApi = {
  // ── Chat (PRD #39) — real M1 wire ─────────────────────────────────────────
  listChats: async () =>
    delay({
      chats: [...state.runs.values()].filter((r) => r.kind === "chat").map((r) => chatDTO(r)),
      max_turns: CHAT_MAX_TURNS, // the envelope constant, not per-chat
    }),
  createChat: async (message: string) => {
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: null,
      forge_type: "",
      mr_web_url: null,
      issue_web_url: null,
      kind: "chat",
      issue_iid: null,
      issue_title: truncateChatTitle(message),
      issue_description: "",
      title: truncateChatTitle(message),
      resume_of_run_id: null,
      status: "running",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: "w-laptop",
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: now,
      started_at: now,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "user_message", null, { text: message });
    scheduleChatReply(run.id, message);
    return delay({ run: { ...run } }, 300);
  },
  sendChatMessage: async (id: string, message: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "this conversation has ended");
    }
    appendMessage(id, "user_message", null, { text: message });
    scheduleChatReply(id, message);
    return delay({ server_side: false }, 150);
  },
  endChat: async (id: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    patchRun(id, { status: "completed", finished_at: new Date().toISOString() });
    return delay({ server_side: false }, 200);
  },
  continueChat: async (id: string) => {
    const src = getRun(id);
    if (!src || src.kind !== "chat") throw new ApiError(404, "chat not found");
    const now = new Date().toISOString();
    const run: Run = {
      ...src,
      id: nextRunId(),
      status: "running",
      resume_of_run_id: id,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "status", null, { text: "continuing the conversation on your worker" });
    return delay({ run: { ...run } }, 250);
  },
  confirmProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    if (p.status !== "pending") throw new ApiError(409, "proposal already resolved");
    // Mark resolved (a re-confirm 409s); the confirm response is the created issue.
    putProposal({ ...p, status: "confirmed" });
    const iid = 200 + Math.floor(Math.random() * 800);
    const issue: CreatedIssue = {
      iid,
      web_url: `https://gitlab.example.com/${p.repo_path ?? "grp/proj"}/-/issues/${iid}`,
      title: p.title,
    };
    // The created-issue link is appended to the conversation (Decision 8).
    appendMessage(chatId, "text", "chat", { text: `Created issue #${iid}: ${issue.web_url}` });
    return delay({ issue }, 350);
  },
  dismissProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    putProposal({ ...p, status: "dismissed" });
    appendMessage(chatId, "status", null, { text: "proposal dismissed — nothing written to the forge" });
    return delay(null, 200); // 204 No Content
  },
  startRunFromChat: async (repoPath: string, _issueIid: number) => {
    // PRD #191 M5: start a run from a chat's start-run card. Repo paths aren't modelled
    // in the mock state, so it resolves the first seeded board+card and mints a queued
    // issue run via the same path as createRun; the real endpoint applies the
    // PRD/ownership gate keyed by repo_path.
    const repoId = [...state.boards.keys()][0];
    const card = repoId ? state.boards.get(repoId)?.cards[0] : undefined;
    if (!repoId || !card) throw new ApiError(404, `repo ${repoPath} not found`);
    return runsApi.createRun(repoId, card.iid);
  },

  // PRD #322 M1: cancel a run from a chat's cancel card. run_id is untrusted; the real
  // endpoint re-resolves ownership/terminality server-side via SubmitInput(cancel), so
  // the mock reproduces its refusals — a missing run is 404, an already-terminal one 409
  // — rather than resolving 202 over a no-op.
  cancelRunFromChat: async (runId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    handleInput(runId, "cancel", "");
    return delay({ server_side: true }, 150);
  },

  // PRD #322 M3: steer a run from a chat's steer card with a human-edited follow-up.
  // run_id + message are untrusted; the real endpoint re-resolves ownership/terminality
  // via SubmitInput(follow_up), which additionally refuses a CHAT run (issue-runs-only),
  // so the mock reproduces its refusals — a missing run is 404, a terminal one 409, and a
  // chat-run target 409 — a follow_up on an issue run succeeds.
  steerRunFromChat: async (runId: string, message: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    if (run.kind === "chat") {
      throw new ApiError(409, "steering applies to issue runs, not chats");
    }
    handleInput(runId, "follow_up", message);
    return delay({ server_side: true }, 150);
  },
};
