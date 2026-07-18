// @vitest-environment jsdom
//
// Regression guard for the M5 "blank run view" deployment bug: the browser's
// live WebSocket never connected (nginx dropped the upgrade / the api rejected
// the Origin), and because the message-history REST replay was wired to the
// socket's onopen, a run with a full persisted log rendered "0 messages /
// Waiting for the agent…" forever. The pure-logic tests and the transport-mocked
// unit tests never exercised the dead-socket path, so it shipped.
//
// This drives the real hook with a WebSocket stub that NEVER opens and asserts
// the history still loads on mount. It is deliberately the one test that touches
// a live socket boundary — everything else stays in the pure runStream layer.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { api, type Run, type RunMessage } from "./api";
import { useRunStream } from "./useRunStream";

// DeadSocket looks like a WebSocket to the hook but never fires onopen (or any
// event): the exact failure mode of a broken proxy handshake.
class DeadSocket {
  onopen: (() => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {}
  close() {}
}

function fakeRun(): Run {
  return {
    id: "run-1",
    repo_id: "repo-1",
    kind: "issue",
    issue_iid: 7,
    issue_title: "Wire the runtime",
    issue_description: "",
    title: null,
    resume_of_run_id: null,
    forge_type: "gitlab",
    mr_web_url: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "worker-1",
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
    created_at: "",
    updated_at: "",
  };
}

function msg(seq: number): RunMessage {
  return { seq, kind: "text", agent: null, payload: `m${seq}`, created_at: "" };
}

describe("useRunStream mount load", () => {
  beforeEach(() => {
    vi.stubGlobal("WebSocket", DeadSocket as unknown as typeof WebSocket);
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("loads run + message history on mount even when the WS never connects", async () => {
    const getRun = vi.spyOn(api, "getRun").mockResolvedValue({ run: fakeRun() });
    const getRunMessages = vi
      .spyOn(api, "getRunMessages")
      .mockResolvedValue({ messages: [msg(1), msg(2), msg(3)] });

    const { result } = renderHook(() => useRunStream("run-1"));

    // History renders from the REST replay without the socket ever opening —
    // this is the fix: replay is triggered on mount, not by ws.onopen.
    await waitFor(() => expect(result.current.messages.map((m) => m.seq)).toEqual([1, 2, 3]));

    expect(getRun).toHaveBeenCalledWith("run-1");
    expect(getRunMessages).toHaveBeenCalled();
    expect(result.current.run?.id).toBe("run-1");
    // The socket never opened, so the live indicator honestly reads not-connected
    // — but the view is populated, not blank.
    expect(result.current.connected).toBe(false);
  });
});
