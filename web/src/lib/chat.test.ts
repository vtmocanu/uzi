import { describe, expect, it } from "vitest";
import {
  chatFromRun,
  chatIsEnded,
  composerGate,
  conversationTitle,
  countUserTurns,
  hasOnlineWorker,
  queuedBehindActive,
  sortConversations,
  turnCapNotice,
} from "./chat";
import type { Chat, Run, RunMessage, Worker } from "./api";

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "offline",
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: null,
    last_heartbeat_at: null,
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    ...over,
  };
}

function aChat(over: Partial<Chat> = {}): Chat {
  return {
    id: "c1",
    title: "A chat",
    status: "running",
    turn_count: 0,
    resume_of_run_id: null,
    last_message_at: null,
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
    ...over,
  };
}

describe("hasOnlineWorker (worker-offline signal, Decision 15)", () => {
  it("is false when every worker is offline", () => {
    expect(hasOnlineWorker([aWorker(), aWorker({ id: "w2" })])).toBe(false);
  });
  it("is true when at least one worker is online", () => {
    expect(hasOnlineWorker([aWorker(), aWorker({ id: "w2", status: "online" })])).toBe(true);
  });
  it("is false for an empty list", () => {
    expect(hasOnlineWorker([])).toBe(false);
  });
});

describe("composerGate", () => {
  it("enables input for an active conversation under the turn cap", () => {
    expect(composerGate({ status: "running", turnCount: 3, maxTurns: 50 })).toEqual({
      enabled: true,
      reason: "",
    });
  });

  it("disables on a terminal (ended) conversation", () => {
    for (const status of ["completed", "failed", "cancelled"]) {
      const g = composerGate({ status, turnCount: 0, maxTurns: 50 });
      expect(g.enabled).toBe(false);
      expect(g.reason).toMatch(/ended/i);
    }
  });

  it("disables when the turn cap is reached", () => {
    const g = composerGate({ status: "running", turnCount: 50, maxTurns: 50 });
    expect(g.enabled).toBe(false);
    expect(g.reason).toMatch(/turn limit/i);
  });

  it("terminal wins over turn-cap in the reason", () => {
    const g = composerGate({ status: "completed", turnCount: 50, maxTurns: 50 });
    expect(g.reason).toMatch(/ended/i);
  });
});

describe("turnCapNotice", () => {
  it("is null with plenty of turns left", () => {
    expect(turnCapNotice({ turnCount: 10, maxTurns: 50 })).toBeNull();
  });
  it("warns within the last few turns", () => {
    expect(turnCapNotice({ turnCount: 47, maxTurns: 50 })).toMatch(/3 turns left/);
    expect(turnCapNotice({ turnCount: 49, maxTurns: 50 })).toMatch(/1 turn left/);
  });
  it("explains recovery at/over the cap", () => {
    expect(turnCapNotice({ turnCount: 50, maxTurns: 50 })).toMatch(/50-turn limit/);
  });
});

describe("queuedBehindActive (one live conversation, Decision 4)", () => {
  it("is true when this chat is queued and another is active", () => {
    const me = { id: "c2", status: "queued" };
    const all = [{ id: "c1", status: "running" }, me];
    expect(queuedBehindActive(me, all)).toBe(true);
  });
  it("is false when no other chat is active", () => {
    const me = { id: "c2", status: "queued" };
    const all = [{ id: "c1", status: "completed" }, me];
    expect(queuedBehindActive(me, all)).toBe(false);
  });
  it("is false when this chat is not queued", () => {
    const me = { id: "c2", status: "running" };
    const all = [{ id: "c1", status: "running" }, me];
    expect(queuedBehindActive(me, all)).toBe(false);
  });
});

describe("countUserTurns", () => {
  it("counts only user_message rows", () => {
    const msgs: RunMessage[] = [
      { seq: 1, kind: "user_message", agent: null, payload: {}, created_at: "" },
      { seq: 2, kind: "text", agent: "chat", payload: {}, created_at: "" },
      { seq: 3, kind: "tool_use", agent: "chat", payload: {}, created_at: "" },
      { seq: 4, kind: "user_message", agent: null, payload: {}, created_at: "" },
    ];
    expect(countUserTurns(msgs)).toBe(2);
  });
});

describe("chatIsEnded / conversationTitle / sortConversations", () => {
  it("chatIsEnded tracks terminal run status", () => {
    expect(chatIsEnded({ status: "running" })).toBe(false);
    expect(chatIsEnded({ status: "completed" })).toBe(true);
  });

  it("conversationTitle falls back to a placeholder", () => {
    expect(conversationTitle(aChat({ title: null }))).toBe("Untitled chat");
    expect(conversationTitle(aChat({ title: "  " }))).toBe("Untitled chat");
    expect(conversationTitle(aChat({ title: "Real" }))).toBe("Real");
  });

  it("sortConversations orders by most-recent activity, last_message_at first", () => {
    const older = aChat({ id: "old", last_message_at: "2026-07-01T00:00:00Z" });
    const newer = aChat({ id: "new", last_message_at: "2026-07-05T00:00:00Z" });
    // Falls back to updated_at when last_message_at is null.
    const noMsg = aChat({ id: "nomsg", last_message_at: null, updated_at: "2026-07-03T00:00:00Z" });
    const sorted = sortConversations([older, noMsg, newer]);
    expect(sorted.map((c) => c.id)).toEqual(["new", "nomsg", "old"]);
  });
});

describe("chatFromRun (create/continue runDTO → unified Chat view)", () => {
  function aRun(over: Partial<Run> = {}): Run {
    return {
      id: "r1",
      repo_id: null,
      kind: "chat",
      issue_iid: null,
      issue_title: "How does it work?",
      issue_description: "",
      title: "How does it work?",
      resume_of_run_id: null,
      status: "running",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: "w1",
      branch: null,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
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
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: "2026-07-10T00:00:00Z",
      updated_at: "2026-07-10T00:00:00Z",
      ...over,
    };
  }

  it("maps a chat runDTO into the Chat view type (title + resume carried, turns start at 0)", () => {
    const chat = chatFromRun(aRun({ id: "abc", title: "T", resume_of_run_id: "prev" }));
    expect(chat).toMatchObject({
      id: "abc",
      title: "T",
      status: "running",
      turn_count: 0,
      resume_of_run_id: "prev",
      last_message_at: null,
    });
  });

  it("carries a null title through (worker has not derived one yet)", () => {
    expect(chatFromRun(aRun({ title: null })).title).toBeNull();
  });
});
