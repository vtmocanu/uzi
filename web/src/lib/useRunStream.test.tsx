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
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { api, ApiError, type Run, type RunMessage, type SteerInput } from "./api";
import { useRunStream } from "./useRunStream";
import { isRevisingRun } from "./runBadge";
import { pauseOrigin } from "./pauseOrigin";

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
    harness: "claude", // PRD #1429 M1: harness joined RunDTO (NOT NULL, default claude).
    title: null,
    resume_of_run_id: null,
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    plan_cross_check_required: false,
    worker_id: "worker-1",
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
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    recovery_wait_cause: null,
    codex_account_action: null,
    codex_secret_id: null,
    codex_secret_label: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    provider_park_count: 0,
    provider_park_streak_started_at: null,
    can_stop_at_provider_hold: false,
    forge_park_max: 0,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "",
    updated_at: "",
  };
}

function msg(seq: number): RunMessage {
  return { seq, kind: "text", agent: null, agent_instance: null, agent_label: null, payload: `m${seq}`, created_at: "" };
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

describe("memory hold REST and socket projection", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });
  it("retains nullable allowance on REST and re-reads it after a socket state frame without acting", async () => {
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    let run: Run = { ...fakeRun(), status: "recovery_wait",
      recovery_wait_cause: "worker_memory_pressure",
      memory_hold: { episode: 4, used: 2, max: null, remaining: null } };
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      expect(init?.method).toBe("GET");
      const data = url === "/api/runs/run-1" ? { run }
        : url.includes("/inputs") ? { inputs: [] } : { messages: [] };
      return new Response(JSON.stringify(data), { status: 200 });
    });
    vi.stubGlobal("fetch", fetch);
    const resume = vi.spyOn(api, "resumeRun");
    const input = vi.spyOn(api, "submitRunInput");
    const { result, unmount } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.memory_hold)
      .toEqual({ episode: 4, used: 2, max: null, remaining: null }));
    await waitFor(() => expect(result.current.confirmedOwner).toBe(true));
    run = { ...run, memory_hold: { episode: 5, used: 3, max: 3, remaining: 0 } };
    act(() => LiveSocket.last!.deliver({ type: "state", status: "recovery_wait" }));
    await waitFor(() => expect(result.current.run?.memory_hold)
      .toEqual({ episode: 5, used: 3, max: 3, remaining: 0 }));
    expect(result.current.run?.recovery_wait_cause).toBe("worker_memory_pressure");
    expect(resume).not.toHaveBeenCalled();
    expect(input).not.toHaveBeenCalled();
    unmount();
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
      applied_at: null,
      included_at: null,
      inclusion_reported: true,
      kind: "follow_up",
      disposition: null,
    };
    // Mount refetch returns Queued; every later refetch returns it Received — so a
    // Received result can ONLY come from a post-mount refetch (open / input frame).
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

  it("ownership proof stays false pending a read, resets on navigation, and ignores stale replies", async () => {
    const replies: Array<(v: { inputs: SteerInput[] }) => void> = [];
    vi.spyOn(api, "getRunInputs").mockImplementation(() => new Promise((resolve) => replies.push(resolve)));
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-1" } });
    expect(result.current.canSteer).toBe(true);
    expect(result.current.confirmedOwner).toBe(false);
    await act(async () => replies[0]({ inputs: [] }));
    expect(result.current.confirmedOwner).toBe(true);
    await act(async () => LiveSocket.last!.open());
    rerender({ id: "run-2" });
    expect(result.current.confirmedOwner).toBe(false);
    await act(async () => replies[1]({ inputs: [] }));
    expect(result.current.confirmedOwner).toBe(false);
    await act(async () => replies[2]({ inputs: [] }));
    expect(result.current.confirmedOwner).toBe(true);
  });

  it("a 404 on /inputs marks the viewer a non-owner (canSteer false) — Decision 8/N2", async () => {
    vi.spyOn(api, "getRunInputs").mockRejectedValue(new ApiError(404, "run not found"));
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.canSteer).toBe(false));
    expect(result.current.confirmedOwner).toBe(false);
    // Silent: no run-level error banner from the 404.
    expect(result.current.error).toBe("");
    expect(result.current.inputs).toHaveLength(0);
  });

  it("a later non-owner response revokes confirmed ownership", async () => {
    vi.spyOn(api, "getRunInputs")
      .mockResolvedValueOnce({ inputs: [] })
      .mockRejectedValue(new ApiError(404, "run not found"));
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.confirmedOwner).toBe(true));
    await act(async () => result.current.refreshInputs());
    expect(result.current.confirmedOwner).toBe(false);
    expect(result.current.canSteer).toBe(false);
  });

  it("a 200-empty response keeps the owner steering (canSteer stays true)", async () => {
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.id).toBe("run-1"));
    expect(result.current.confirmedOwner).toBe(true);
    // An empty queue is NOT a non-owner signal; the owner still steers.
    expect(result.current.canSteer).toBe(true);
  });

  it("a transient (non-404) inputs error does not flip an owner to non-owner", async () => {
    // First mount refetch 200s (owner confirmed), a later refetch 500s (transient).
    vi.spyOn(api, "getRunInputs")
      .mockResolvedValueOnce({ inputs: [] })
      .mockRejectedValue(new ApiError(500, "boom"));
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.id).toBe("run-1"));
    expect(result.current.canSteer).toBe(true);
    expect(result.current.confirmedOwner).toBe(true);
    // The socket opens → a second refetch fires and 500s; canSteer must NOT flip.
    await act(async () => {
      LiveSocket.last!.open();
    });
    await waitFor(() => expect(result.current.connected).toBe(true));
    expect(result.current.canSteer).toBe(true);
    expect(result.current.confirmedOwner).toBe(true);
  });
});

// Issue #1430: a REST read issued for the previous run must not land after the view
// moved to another run. Each read is held on a deferred promise so the test decides
// the resolution order: the new run's reads resolve first, the old run's last.
describe("useRunStream stale fetch after run change (issue #1430)", () => {
  type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void };
  function deferred<T>(): Deferred<T> {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }
  const runWith = (id: string): Run => ({ ...fakeRun(), id });
  const input = (id: number, body: string): SteerInput => ({
    id,
    body,
    created_at: "",
    consumed_at: null,
    applied_at: null,
    included_at: null,
    inclusion_reported: false,
    kind: "follow_up",
    disposition: null,
  });

  // pending holds each run id's deferred reads, one per call, in call order.
  let pending: {
    run: Record<string, Deferred<{ run: Run }>[]>;
    msgs: Record<string, Deferred<{ messages: RunMessage[] }>[]>;
    inputs: Record<string, Deferred<{ inputs: SteerInput[] }>[]>;
  };
  const push = <T,>(m: Record<string, Deferred<T>[]>, id: string) => {
    const d = deferred<T>();
    (m[id] ??= []).push(d);
    return d.promise;
  };

  beforeEach(() => {
    pending = { run: {}, msgs: {}, inputs: {} };
    vi.stubGlobal("WebSocket", DeadSocket as unknown as typeof WebSocket);
    vi.spyOn(api, "getRun").mockImplementation((id: string) => push(pending.run, id));
    vi.spyOn(api, "getRunMessages").mockImplementation((id: string) => push(pending.msgs, id));
    vi.spyOn(api, "getRunInputs").mockImplementation((id: string) => push(pending.inputs, id));
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("drops run A's run, messages and inputs when they resolve after navigating to run B", async () => {
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-a" } });
    rerender({ id: "run-b" });

    await act(async () => {
      pending.run["run-b"][0].resolve({ run: runWith("run-b") });
      pending.msgs["run-b"][0].resolve({ messages: [msg(1)] });
      pending.inputs["run-b"][0].resolve({ inputs: [input(2, "for b")] });
    });
    await waitFor(() => expect(result.current.run?.id).toBe("run-b"));

    // Run A's reads land last. Before the fix each one overwrote run B's state.
    await act(async () => {
      pending.run["run-a"][0].resolve({ run: runWith("run-a") });
      pending.msgs["run-a"][0].resolve({ messages: [msg(1), msg(2), msg(3)] });
      pending.inputs["run-a"][0].resolve({ inputs: [input(1, "for a")] });
    });

    expect(result.current.run?.id).toBe("run-b");
    expect(result.current.messages.map((m) => m.payload)).toEqual(["m1"]);
    expect(result.current.inputs.map((i) => i.body)).toEqual(["for b"]);
  });

  it("drops a stale failure for run A: no error banner and canSteer untouched on run B", async () => {
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-a" } });
    rerender({ id: "run-b" });
    await act(async () => {
      pending.run["run-b"][0].resolve({ run: runWith("run-b") });
      pending.inputs["run-b"][0].resolve({ inputs: [] });
    });
    await waitFor(() => expect(result.current.run?.id).toBe("run-b"));

    await act(async () => {
      pending.run["run-a"][0].reject(new ApiError(404, "run not found"));
      pending.inputs["run-a"][0].reject(new ApiError(404, "run not found"));
    });

    expect(result.current.error).toBe("");
    expect(result.current.canSteer).toBe(true);
    expect(result.current.confirmedOwner).toBe(true);
  });

  it("an old run's submit() resolving after navigation does not fetch and show that run", async () => {
    const submitted = deferred<{ server_side: boolean }>();
    vi.spyOn(api, "submitRunInput").mockReturnValue(submitted.promise as ReturnType<typeof api.submitRunInput>);
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-a" } });
    await act(async () => {
      pending.run["run-a"][0].resolve({ run: runWith("run-a") });
    });
    await waitFor(() => expect(result.current.run?.id).toBe("run-a"));

    // Start A's submit, then navigate to B and load it.
    let submitting!: Promise<void>;
    act(() => {
      submitting = result.current.submit("approve_plan");
    });
    rerender({ id: "run-b" });
    await act(async () => {
      pending.run["run-b"][0].resolve({ run: runWith("run-b") });
    });
    await waitFor(() => expect(result.current.run?.id).toBe("run-b"));

    // A's submit completes and calls A's refreshRun. It must not start a read of A:
    // one started now would capture B's generation and be accepted.
    await act(async () => {
      submitted.resolve({ server_side: false });
      await submitting;
    });
    expect(pending.run["run-a"]).toHaveLength(1);
    await act(async () => {
      pending.run["run-a"][1]?.resolve({ run: runWith("run-a") });
    });
    expect(result.current.run?.id).toBe("run-b");
  });

  it("drops the first visit's read after navigating A -> B -> A (same id, older generation)", async () => {
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-a" } });
    rerender({ id: "run-b" });
    rerender({ id: "run-a" });

    const fresh = { ...runWith("run-a"), status: "completed" as const };
    await act(async () => {
      pending.run["run-a"][1].resolve({ run: fresh });
    });
    await waitFor(() => expect(result.current.run?.status).toBe("completed"));

    // The first visit's read (older status) resolves last and must not win.
    await act(async () => {
      pending.run["run-a"][0].resolve({ run: runWith("run-a") });
    });
    expect(result.current.run?.status).toBe("completed");
  });
});

// Issue #1430 review: the previous run's terminal status must not leak into the next
// run and stop its socket from reconnecting before the new run's own status arrives.
describe("useRunStream reconnect after switching from a terminal run (issue #1430)", () => {
  beforeEach(() => {
    LiveSocket.last = null;
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("reconnects B's socket while B's run read is still pending, after A was completed", async () => {
    vi.spyOn(api, "getRun").mockImplementation((id: string) =>
      id === "run-a"
        ? Promise.resolve({ run: { ...fakeRun(), id: "run-a", status: "completed" } })
        : new Promise(() => {}), // B's status never arrives in this test
    );
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-a" } });
    await waitFor(() => expect(result.current.run?.status).toBe("completed"));
    await act(async () => {
      LiveSocket.last!.open();
    });
    expect(result.current.connected).toBe(true);

    rerender({ id: "run-b" });
    const bSocket = LiveSocket.last!;
    expect(bSocket.url).toContain("run-b");
    // B has not opened yet: the view must not still claim A's connection.
    expect(result.current.connected).toBe(false);

    const setTimeoutSpy = vi.spyOn(window, "setTimeout");
    act(() => {
      bSocket.close();
    });
    // A's "completed" leaking into B would skip the reconnect entirely.
    expect(setTimeoutSpy.mock.calls.some(([, ms]) => ms === 1500)).toBe(true);
  });
});

// Issue #2760: a revise replan flips the server-derived is_revising / progress.state, and
// only a run re-read carries them, so the plan_revising and plan frames must refresh it.
describe("useRunStream live revise cycle (issue #2760)", () => {
  beforeEach(() => {
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    LiveSocket.last = null;
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const progressOf = (state: "waiting" | "planning") => ({
    state,
    pct: null,
    milestone_done: 0,
    milestone_total: 0,
    active_milestone_id: "",
    phase: "" as const,
  });
  const gate = (revising: boolean): Run => ({
    ...fakeRun(),
    status: "awaiting_approval",
    is_revising: revising,
    progress: progressOf(revising ? "planning" : "waiting"),
  });
  const frame = (seq: number, kind: string) => ({
    type: "message",
    seq,
    kind,
    agent: "lead",
    payload: {},
    created_at: "2026-10-11T10:00:00Z",
  });

  it("drops the owner-waiting state on plan_revising and restores it on the newer plan", async () => {
    // The server's answer follows the log: the gate for plan #1, revising once
    // plan_revising lands, a fresh gate once the next plan lands.
    let serverRevising = false;
    const getRun = vi.spyOn(api, "getRun").mockImplementation(async () => ({ run: gate(serverRevising) }));
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.status).toBe("awaiting_approval"));
    expect(isRevisingRun(result.current.run!)).toBe(false);
    expect(result.current.run?.progress?.state).toBe("waiting");
    await act(async () => {
      LiveSocket.last!.open();
    });

    serverRevising = true;
    const before = getRun.mock.calls.length;
    await act(async () => {
      LiveSocket.last!.deliver(frame(1, "plan_revising"));
    });
    await waitFor(() => expect(isRevisingRun(result.current.run!)).toBe(true));
    expect(getRun.mock.calls.length).toBeGreaterThan(before);
    expect(result.current.run?.progress?.state).toBe("planning");

    // A state frame re-reads the run: it must keep the revising value, not reset it.
    await act(async () => {
      LiveSocket.last!.deliver({ type: "state" });
    });
    await waitFor(() => expect(getRun.mock.calls.length).toBeGreaterThan(before + 1));
    expect(isRevisingRun(result.current.run!)).toBe(true);

    serverRevising = false;
    await act(async () => {
      LiveSocket.last!.deliver(frame(2, "plan"));
    });
    await waitFor(() => expect(isRevisingRun(result.current.run!)).toBe(false));
    expect(result.current.run?.progress?.state).toBe("waiting");
  });

  it("re-reads the run when a gap-repair replay ingests a plan_revising message", async () => {
    let serverRevising = false;
    vi.spyOn(api, "getRun").mockImplementation(async () => ({ run: gate(serverRevising) }));
    const getMsgs = vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.status).toBe("awaiting_approval"));
    await act(async () => {
      LiveSocket.last!.open();
    });
    expect(isRevisingRun(result.current.run!)).toBe(false);

    // The server entered the revise while frames were dropped: the live frame that
    // arrives is seq 3 (a gap), and the replay carries the missed plan_revising.
    serverRevising = true;
    getMsgs.mockResolvedValue({
      messages: [1, 2, 3].map((seq) => ({
        seq,
        kind: seq === 2 ? "plan_revising" : "text",
        agent: null,
        agent_instance: null,
        agent_label: null,
        payload: {},
        created_at: "",
      })),
    });
    await act(async () => {
      LiveSocket.last!.deliver(frame(3, "text"));
    });
    await waitFor(() => expect(isRevisingRun(result.current.run!)).toBe(true));
    expect(result.current.run?.progress?.state).toBe("planning");
  });

  it("re-reads the run on reconnect", async () => {
    let serverRevising = false;
    vi.spyOn(api, "getRun").mockImplementation(async () => ({ run: gate(serverRevising) }));
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });

    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.status).toBe("awaiting_approval"));
    await act(async () => {
      LiveSocket.last!.open();
    });

    serverRevising = true;
    await act(async () => {
      LiveSocket.last!.open();
    });
    await waitFor(() => expect(isRevisingRun(result.current.run!)).toBe(true));
  });
});

describe("useRunStream out-of-order run refresh (issue #2760)", () => {
  beforeEach(() => {
    LiveSocket.last = null;
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  // A plan_revising / plan frame re-reads the run while an earlier re-read (here the
  // state frame's) is still in flight. The older response resolving last must not
  // overwrite the newer revising flag, in either direction.
  for (const target of [true, false]) {
    it(`keeps the newest is_revising=${target} when an older refresh resolves last`, async () => {
      const gate = (revising: boolean): Run => ({ ...fakeRun(), status: "awaiting_approval", is_revising: revising });
      const getRun = vi.spyOn(api, "getRun").mockResolvedValue({ run: gate(!target) });
      vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
      vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
      const { result } = renderHook(() => useRunStream("run-1"));
      await waitFor(() => expect(result.current.run?.is_revising).toBe(!target));

      let resolveOld!: (value: { run: Run }) => void;
      const old = new Promise<{ run: Run }>((resolve) => {
        resolveOld = resolve;
      });
      getRun.mockImplementationOnce(() => old).mockResolvedValue({ run: gate(target) });
      const before = getRun.mock.calls.length;
      await act(async () => {
        LiveSocket.last!.deliver({ type: "state" });
      });
      expect(getRun.mock.calls.length).toBeGreaterThan(before);
      await act(async () => {
        LiveSocket.last!.deliver({
          type: "message",
          seq: 1,
          kind: target ? "plan_revising" : "plan",
          agent: "lead",
          payload: {},
          created_at: "",
        });
      });
      await waitFor(() => expect(result.current.run?.is_revising).toBe(target));

      await act(async () => {
        resolveOld({ run: gate(!target) });
        await old;
      });
      expect(result.current.run?.is_revising).toBe(target);
    });
  }
});


describe("useRunStream milestone reset propagation", () => {
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });
  it("only appends reset options when present, preserving ordinary and follow-up calls", async () => {
    vi.stubGlobal("WebSocket", DeadSocket);
    vi.spyOn(api, "getRun").mockResolvedValue({ run: fakeRun() });
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
    const submitted = vi.spyOn(api, "submitRunInput").mockResolvedValue({ server_side: true });
    const { result, unmount } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run).not.toBeNull());
    const selection = { source: "own" as const, exclusions: [] };
    const reset = { reconciliation: { mode: "reset" as const, reason: "Scope changed", expected_completion_revision: null },
      expected_reset_state_digest: "displayed-digest" };
    await act(async () => result.current.submit("approve_plan", "", selection, true, undefined, 4, reset));
    expect(submitted).toHaveBeenLastCalledWith("run-1", "approve_plan", "", selection, true, undefined, 4, reset);
    await act(async () => result.current.submit("approve_plan", "", selection));
    expect(submitted.mock.calls[1]).toEqual(["run-1", "approve_plan", "", selection, undefined, undefined, undefined]);
    await act(async () => result.current.submit("follow_up", "hello"));
    expect(submitted.mock.calls[2]).toEqual(["run-1", "follow_up", "hello", undefined]);
    unmount();
  });
});

describe("obsolete socket callbacks", () => {
  const since = "2026-10-16T12:00:00Z";
  const marker = (origin: string) => ({
    type: "message", seq: 1, kind: "paused", payload: { origin }, created_at: since,
  });
  const event = (frame: unknown) => ({ data: JSON.stringify(frame) } as MessageEvent);
  const retain = (socket: LiveSocket) => ({
    open: socket.onopen!, message: socket.onmessage!, close: socket.onclose!, error: socket.onerror!,
  });
  beforeEach(() => {
    vi.stubGlobal("WebSocket", LiveSocket);
    vi.spyOn(api, "getRun").mockImplementation(async id => ({
      run: { ...fakeRun(), id, status: "paused", status_since: since },
    }));
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
  });
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it.each([["b"], ["b", "a"]])("rejects retained callbacks after navigation through %j", async (...ids) => {
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "a" } });
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    const old = retain(LiveSocket.last!);
    for (const id of ids) {
      rerender({ id });
      await waitFor(() => expect(result.current.run?.id).toBe(id));
      await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    }
    await act(async () => LiveSocket.last!.open());
    const calls = [api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length);
    await act(async () => {
      old.message(event(marker("lead_park")));
      old.message(event({ type: "state" }));
      old.message(event({ type: "input" }));
      old.open(); old.error(); old.close();
    });
    expect(result.current.messages).toEqual([]);
    expect(result.current.pauseOriginReady).toBe(true);
    expect(result.current.connected).toBe(true);
    expect([api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length)).toEqual(calls);
    await act(async () => LiveSocket.last!.deliver(marker("owner")));
    expect(pauseOrigin(result.current.run!, result.current.messages, result.current.pauseOriginReady)).toBe("owner");
    await act(async () => LiveSocket.last!.deliver({ ...marker("lead_park"), seq: 2 }));
    expect(pauseOrigin(result.current.run!, result.current.messages, result.current.pauseOriginReady)).toBe("lead_park");
  });

  it("rejects inactive, replaced and unmounted socket callbacks without touching the replacement", async () => {
    const { result, unmount } = renderHook(() => useRunStream("a"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    vi.useFakeTimers();
    const socket = LiveSocket.last!;
    const old = retain(socket);
    act(() => socket.close());
    const calls = [api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length);
    await act(async () => { old.open(); old.message(event(marker("owner"))); old.error(); old.close(); });
    expect(result.current.pauseOriginReady).toBe(false);
    expect(result.current.messages).toEqual([]);
    expect([api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length)).toEqual(calls);
    await act(async () => vi.advanceTimersByTimeAsync(1500));
    const current = LiveSocket.last!;
    expect(current).not.toBe(socket);
    const close = vi.spyOn(current, "close");
    await act(async () => current.open());
    expect(result.current.pauseOriginReady).toBe(true);
    await act(async () => { old.close(); old.error(); old.open(); old.message(event(marker("lead_park"))); });
    expect(close).not.toHaveBeenCalled();
    expect(result.current.connected).toBe(true);
    expect(result.current.pauseOriginReady).toBe(true);
    expect(result.current.messages).toEqual([]);
    await act(async () => current.deliver(marker("owner")));
    expect(pauseOrigin(result.current.run!, result.current.messages, result.current.pauseOriginReady)).toBe("owner");
    const retainedCurrent = retain(current);
    unmount();
    const afterUnmount = [api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length);
    await act(async () => {
      retainedCurrent.open(); retainedCurrent.message(event({ type: "state" }));
      retainedCurrent.error(); retainedCurrent.close();
      await vi.advanceTimersByTimeAsync(1500);
    });
    expect([api.getRun, api.getRunMessages, api.getRunInputs].map(fn => vi.mocked(fn).mock.calls.length)).toEqual(afterUnmount);
    expect(LiveSocket.last).toBe(current);
  });
});

// Pause attribution is local display evidence; transport scheduling stays unchanged.
describe("pause origin readiness", () => {
  function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (reason: Error) => void;
    const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
    return { promise, resolve, reject };
  }
  beforeEach(() => {
    cleanup();
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
    vi.spyOn(api, "getRun").mockResolvedValue({ run: { ...fakeRun(), status: "paused", status_since: "2026-10-16T12:00:00Z" } });
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
  });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it("requires both mount reads, and resets immediately on run-id change", async () => {
    const metadata = deferred<{ run: Run }>();
    vi.mocked(api.getRun).mockReturnValueOnce(metadata.promise);
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-1" } });
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => {});
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => metadata.resolve({ run: fakeRun() }));
    expect(result.current.pauseOriginReady).toBe(true);
    const history = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(history.promise);
    rerender({ id: "run-2" });
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => history.resolve({ messages: [] }));
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("failed reconnect catchup neutralizes cached evidence and fences older replay success", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    const old = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(old.promise);
    act(() => LiveSocket.last!.open());
    expect(result.current.pauseOriginReady).toBe(false);
    vi.mocked(api.getRunMessages).mockRejectedValueOnce(new Error("offline"));
    const retired = LiveSocket.last!;
    act(() => retired.close());
    await waitFor(() => expect(LiveSocket.last).not.toBe(retired), { timeout: 2500 });
    await act(async () => LiveSocket.last!.open());
    await act(async () => old.resolve({ messages: [] }));
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => LiveSocket.last!.open());
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("requires successful replay with every pending gap resolved", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    act(() => LiveSocket.last!.deliver({ type: "message", seq: 3, kind: "paused" }));
    expect(result.current.pauseOriginReady).toBe(false);
    await waitFor(() => expect(api.getRunMessages).toHaveBeenCalledTimes(2));
    expect(result.current.pauseOriginReady).toBe(false);
    act(() => LiveSocket.last!.deliver({ type: "message", seq: 1, kind: "text" }));
    expect(result.current.pauseOriginReady).toBe(false);
    act(() => LiveSocket.last!.deliver({ type: "message", seq: 2, kind: "text" }));
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it.each([
    [false, false], [false, true], [true, false], [true, true],
  ])("superseded replay gap fill cannot certify readiness (late metadata %s, repeated marker %s)", async (lateMetadata, repeatedMarker) => {
    const old = deferred<{ messages: RunMessage[] }>();
    const metadata = deferred<{ run: Run }>();
    const paused = { ...fakeRun(), status: "paused", status_since: "2026-10-16T12:00:00Z" } satisfies Run;
    const marker: RunMessage = { ...msg(3), kind: "paused", payload: { origin: "lead_park" }, created_at: paused.status_since };
    vi.mocked(api.getRunMessages).mockReturnValueOnce(old.promise)
      .mockResolvedValueOnce({ messages: repeatedMarker ? [marker] : [] });
    if (lateMetadata) vi.mocked(api.getRun).mockReturnValueOnce(metadata.promise);
    const { result } = renderHook(() => useRunStream("run-1"));
    await act(async () => LiveSocket.last!.deliver({ type: "message", ...marker }));
    await waitFor(() => expect(api.getRunMessages).toHaveBeenCalledTimes(2));
    await act(async () => {});
    expect(result.current.pauseOriginReady).toBe(false);

    await act(async () => old.resolve({ messages: [msg(1), msg(2)] }));
    expect(result.current.messages.map(m => m.seq)).toEqual([1, 2, 3]);
    expect(result.current.pauseOriginReady).toBe(false);
    expect(pauseOrigin(paused, result.current.messages, result.current.pauseOriginReady)).toBe("");
    if (lateMetadata) await act(async () => metadata.resolve({ run: paused }));
    // Neither later metadata nor a current live commit may reuse the old success flag.
    await act(async () => LiveSocket.last!.deliver({ type: "message", ...msg(4) }));
    expect(result.current.pauseOriginReady).toBe(false);
    expect(pauseOrigin(paused, result.current.messages, result.current.pauseOriginReady)).toBe("");
    expect(api.getRunMessages).toHaveBeenCalledTimes(2);

    // An existing gap catch-up supplies fresh proof without changing read scheduling.
    vi.mocked(api.getRunMessages).mockResolvedValueOnce({ messages: [msg(5)] });
    act(() => LiveSocket.last!.deliver({ type: "message", ...msg(6) }));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    expect(pauseOrigin(paused, result.current.messages, result.current.pauseOriginReady)).toBe("lead_park");
    expect(api.getRunMessages).toHaveBeenCalledTimes(3);
  });

  it("a current replay already in flight can certify after a superseded gap fill", async () => {
    const old = deferred<{ messages: RunMessage[] }>();
    const current = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise);
    const { result } = renderHook(() => useRunStream("run-1"));
    act(() => LiveSocket.last!.deliver({ type: "message", ...msg(3) }));
    await waitFor(() => expect(api.getRunMessages).toHaveBeenCalledTimes(2));
    await act(async () => old.resolve({ messages: [msg(1), msg(2)] }));
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => current.resolve({ messages: [msg(1), msg(2), msg(3)] }));
    expect(result.current.pauseOriginReady).toBe(true);
    expect(api.getRunMessages).toHaveBeenCalledTimes(2);
  });

  it("a duplicate superseded response preserves current valid readiness", async () => {
    const old = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(old.promise)
      .mockResolvedValueOnce({ messages: [msg(1), msg(2), msg(3)] });
    const { result } = renderHook(() => useRunStream("run-1"));
    act(() => LiveSocket.last!.deliver({ type: "message", ...msg(3) }));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    await act(async () => old.resolve({ messages: [msg(1), msg(2)] }));
    expect(result.current.pauseOriginReady).toBe(true);
    expect(result.current.messages.map(m => m.seq)).toEqual([1, 2, 3]);
    expect(api.getRunMessages).toHaveBeenCalledTimes(2);
  });

  it("state-only invalidation rejects older metadata even after current replay", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    const old = deferred<{ run: Run }>();
    const current = deferred<{ run: Run }>();
    vi.mocked(api.getRun).mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise);
    let oldRead!: Promise<void>;
    act(() => { oldRead = result.current.refreshRun(); });
    act(() => LiveSocket.last!.deliver({ type: "state" }));
    expect(result.current.pauseOriginReady).toBe(false);
    await waitFor(() => expect(api.getRunMessages).toHaveBeenCalledTimes(2));
    await act(async () => { old.resolve({ run: fakeRun() }); await oldRead; });
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => current.resolve({ run: { ...fakeRun(), status: "paused", status_since: "2026-10-16T12:01:00Z" } }));
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("a failed latest metadata read cannot be certified by an earlier successful read", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    const old = deferred<{ run: Run }>();
    vi.mocked(api.getRun).mockReturnValueOnce(old.promise).mockRejectedValueOnce(new Error("offline"));
    let oldRead!: Promise<void>;
    act(() => { oldRead = result.current.refreshRun(); });
    await act(async () => result.current.refreshRun());
    await act(async () => { old.resolve({ run: fakeRun() }); await oldRead; });
    expect(result.current.pauseOriginReady).toBe(false);
  });

  it("mount replay failure stays neutral even after metadata succeeds", async () => {
    vi.mocked(api.getRunMessages).mockRejectedValueOnce(new Error("offline"));
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.run?.status).toBe("paused"));
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => LiveSocket.last!.open());
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("a changed status epoch from a refresh requires a fresh existing replay", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    vi.mocked(api.getRun).mockResolvedValue({ run: { ...fakeRun(), status: "paused", status_since: "2026-10-16T12:02:00Z" } });
    await act(async () => result.current.refreshRun());
    expect(result.current.pauseOriginReady).toBe(false);
    // A health refresh alone cannot certify the old log against this new epoch.
    await act(async () => LiveSocket.last!.deliver({ type: "health" }));
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => LiveSocket.last!.open());
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("an older metadata success after current success cannot replace certified metadata", async () => {
    const { result } = renderHook(() => useRunStream("run-1"));
    await waitFor(() => expect(result.current.pauseOriginReady).toBe(true));
    const old = deferred<{ run: Run }>();
    vi.mocked(api.getRun).mockReturnValueOnce(old.promise);
    let oldRead!: Promise<void>;
    act(() => { oldRead = result.current.refreshRun(); });
    await act(async () => result.current.refreshRun());
    expect(result.current.pauseOriginReady).toBe(true);
    await act(async () => { old.resolve({ run: fakeRun() }); await oldRead; });
    expect(result.current.run?.status).toBe("paused");
    expect(result.current.pauseOriginReady).toBe(true);
  });

  it("old run reads cannot certify an A → B → A generation", async () => {
    const old = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(old.promise);
    const { result, rerender } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-1" } });
    rerender({ id: "run-2" });
    const current = deferred<{ messages: RunMessage[] }>();
    vi.mocked(api.getRunMessages).mockReturnValueOnce(current.promise);
    rerender({ id: "run-1" });
    await act(async () => old.resolve({ messages: [msg(1)] }));
    expect(result.current.pauseOriginReady).toBe(false);
    expect(result.current.messages).toEqual([]);
    await act(async () => current.resolve({ messages: [] }));
    expect(result.current.pauseOriginReady).toBe(true);
  });
});

describe("quiet account cooldown metadata", () => {
  const now = Date.parse("2026-10-28T12:00:00Z");
  const held = (over: Partial<Run> = {}): Run => ({ ...fakeRun(), health: "waiting_account_cooldown", account_cooldown_class: "rate_limit",
    account_cooldown_eligible_at: "2026-10-28T12:01:37Z", ...over });
  const flush = () => act(async () => { await Promise.resolve(); });
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(now);
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    vi.stubGlobal("WebSocket", LiveSocket as unknown as typeof WebSocket);
    vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
    vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
  });
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it("quiet metadata failure keeps last good run without a foreground banner through recovery", async () => {
    const get = vi.spyOn(api, "getRun").mockResolvedValue({ run: held() });
    const { result } = renderHook(() => useRunStream("run-1"));
    await flush();
    get.mockRejectedValueOnce(new ApiError(503, "temporary metadata outage"));
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(result.current.run?.id).toBe("run-1");
    expect(result.current.error).toBe("");
    expect(result.current.pauseOriginReady).toBe(false);
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(get).toHaveBeenCalledTimes(3);
    expect(result.current.error).toBe("");
  });

  it("refreshes quiet extension/class changes and lease expiry with no activity or new operations", async () => {
    const get = vi.spyOn(api, "getRun").mockResolvedValue({ run: held() });
    const { result, unmount } = renderHook(() => useRunStream("run-1"));
    await flush();
    expect(result.current.run?.account_cooldown_class).toBe("rate_limit");
    const replayCount = vi.mocked(api.getRunMessages).mock.calls.length;
    get.mockResolvedValue({ run: held({ account_cooldown_class: "overload", account_cooldown_eligible_at: "2026-10-28T12:05:37Z" }) });
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(result.current.run?.account_cooldown_eligible_at).toBe("2026-10-28T12:05:37Z");
    expect(result.current.run?.account_cooldown_class).toBe("overload");
    get.mockResolvedValue({ run: held({ health: "ok", account_cooldown_class: undefined, account_cooldown_eligible_at: undefined }) });
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(result.current.run?.health).toBe("ok");
    expect(result.current.messages).toEqual([]);
    expect(api.getRunMessages).toHaveBeenCalledTimes(replayCount);
    unmount();
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(get).toHaveBeenCalledTimes(3);
  });

  it("coalesces pending quiet reads and drops a pre-cancel GET after the owner submits", async () => {
    let resolve!: (value: { run: Run }) => void;
    const pending = new Promise<{ run: Run }>(done => { resolve = done; });
    const get = vi.spyOn(api, "getRun").mockResolvedValue({ run: held() });
    const send = vi.spyOn(api, "submitRunInput").mockResolvedValue({ server_side: true });
    const { result } = renderHook(() => useRunStream("run-1"));
    await flush();
    get.mockReturnValueOnce(pending);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(get).toHaveBeenCalledTimes(2);
    get.mockResolvedValue({ run: held({ status: "cancelled", health: "ok", account_cooldown_class: undefined, account_cooldown_eligible_at: undefined }) });
    await act(async () => { await result.current.submit("cancel"); });
    expect(send).toHaveBeenCalledWith("run-1", "cancel", "", undefined, undefined, undefined, undefined);
    await act(async () => { resolve({ run: held() }); });
    expect(result.current.run?.status).toBe("cancelled");
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(get).toHaveBeenCalledTimes(3);
  });

  it("ignores a quiet GET across source roundtrip and stops polling after unmount", async () => {
    let resolve!: (value: { run: Run }) => void;
    const get = vi.spyOn(api, "getRun").mockResolvedValue({ run: held() });
    const { result, rerender, unmount } = renderHook(({ id }) => useRunStream(id), { initialProps: { id: "run-1" } });
    await flush();
    get.mockReturnValueOnce(new Promise(done => { resolve = done; }));
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    rerender({ id: "run-2" });
    await flush();
    get.mockResolvedValue({ run: held({ health: "ok", account_cooldown_class: undefined, account_cooldown_eligible_at: undefined }) });
    rerender({ id: "run-1" });
    await flush();
    await act(async () => { resolve({ run: held() }); });
    expect(result.current.run?.health).toBe("ok");
    unmount();
    const count = get.mock.calls.length;
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(get).toHaveBeenCalledTimes(count);
  });
});
