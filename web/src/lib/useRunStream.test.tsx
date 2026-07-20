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
import { act, renderHook, waitFor } from "@testing-library/react";
import { api, type Run, type RunMessage, type SteerInput } from "./api";
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

// LiveSocket lets a test drive the hook's socket boundary: fire onopen, deliver
// frames. The most-recently-constructed instance is exposed so a test can poke it.
class LiveSocket {
  static last: LiveSocket | null = null;
  onopen: (() => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    LiveSocket.last = this;
  }
  open() {
    this.onopen?.();
  }
  deliver(frame: unknown) {
    this.onmessage?.({ data: JSON.stringify(frame) } as MessageEvent);
  }
  close() {
    this.onclose?.();
  }
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

// PRD #95 M3: the steer queue's live path — optimistic send + reconcile (S2), and
// the REST refetch triggers that make a dropped `input` frame self-heal (S1).
describe("useRunStream steer queue (PRD #95 M3)", () => {
  beforeEach(() => {
    LiveSocket.last = null;
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    vi.spyOn(api, "getRun").mockResolvedValue({ run: fakeRun() });
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("optimistically appends a Queued entry on follow-up send, then adopts the returned row id (S2)", async () => {
    // The queue starts empty from the server; the entry can ONLY appear via the
    // optimistic path, so its presence proves we did not wait for a refetch.
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
    const submitSpy = vi
      .spyOn(api, "submitRunInput")
      .mockResolvedValue({ server_side: false, id: 42, created_at: "2026-07-20T10:05:00Z" });

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.id).toBe("run-1"));

    await act(async () => {
      await result.current.submit("follow_up", "please rerun the tests");
    });

    expect(submitSpy).toHaveBeenCalledWith("run-1", "follow_up", "please rerun the tests", undefined);
    // Exactly one entry, carrying the REAL id + created_at from the write response,
    // still unconsumed (Queued), body preserved.
    expect(result.current.inputs).toHaveLength(1);
    const only = result.current.inputs[0];
    expect(only.id).toBe(42);
    expect(only.created_at).toBe("2026-07-20T10:05:00Z");
    expect(only.body).toBe("please rerun the tests");
    expect(only.consumed_at).toBeNull();
  });

  it("rolls the optimistic entry back if the follow-up write fails", async () => {
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
    vi.spyOn(api, "submitRunInput").mockRejectedValue(new Error("boom"));

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.id).toBe("run-1"));

    await act(async () => {
      await expect(result.current.submit("follow_up", "nope")).rejects.toThrow("boom");
    });
    expect(result.current.inputs).toHaveLength(0);
  });

  it("refetches the steer queue on socket open and on the data-less `input` frame (S1)", async () => {
    const delivered: SteerInput = {
      id: 7,
      body: "resume",
      created_at: "2026-07-20T10:00:00Z",
      consumed_at: "2026-07-20T10:02:00Z",
    };
    // Mount refetch returns Queued; every later refetch returns it Delivered — so a
    // Delivered result can ONLY come from a post-mount refetch (open / input frame).
    const getInputs = vi
      .spyOn(api, "getRunInputs")
      .mockResolvedValueOnce({ inputs: [{ ...delivered, consumed_at: null }] })
      .mockResolvedValue({ inputs: [delivered] });

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.inputs).toHaveLength(1));
    // Mount saw it unconsumed.
    expect(result.current.inputs[0].consumed_at).toBeNull();

    // The socket opens: reconcile fires, picking up the Delivered stamp even though no
    // `input` frame has arrived (this is the reconnect self-heal).
    await act(async () => {
      LiveSocket.last!.open();
    });
    await waitFor(() => expect(result.current.inputs[0].consumed_at).toBe("2026-07-20T10:02:00Z"));
    const afterOpen = getInputs.mock.calls.length;

    // A data-less `input` frame triggers another refetch (the fast path).
    await act(async () => {
      LiveSocket.last!.deliver({ type: "input" });
    });
    await waitFor(() => expect(getInputs.mock.calls.length).toBeGreaterThan(afterOpen));
  });
});
