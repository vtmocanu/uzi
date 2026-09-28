// @vitest-environment jsdom
//
// PlanPanel gate CREDENTIAL path (PRD #1247 M7), tested as a component (RunView itself
// needs routing + a live stream to mount). The gate's token picker is seeded from the
// run's stored override and, on approve, must set the run credential BEFORE onApprove
// when the user CHANGED it — and must ABORT the approve (surfacing the error, not calling
// onApprove) if that set fails. The untouched/inherit path must skip the credential call
// and preserve the exact 1-arg onApprove contract.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { PlanPanel } from "./PlanPanel";
import { act } from "react";
import { api, ApiError, type AgentSelectionInput, type Run, type RunMessage, type SecretMeta } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, setRunCredential: vi.fn(), listSecrets: vi.fn() } };
});
const mockApi = vi.mocked(api);

beforeEach(() => {
  // The gate picker's seed loads the user's Anthropic tokens (self-fetch path).
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function token(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "console-key",
    is_default: false,
    enabled: true,
    disabled_at: null,
    auto_eligible: true,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function run(over: Partial<Run> = {}): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    kind: "issue",
    issue_iid: 87,
    issue_title: "Add rate limiting",
    issue_description: "d",
    harness: "claude", // PRD #1429 M1: harness joined RunDTO.
    title: null,
    resume_of_run_id: null,
    status: "awaiting_approval",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
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
    recovery_wait_cause: null,
    codex_account_action: null,
    codex_secret_id: null,
    codex_secret_label: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function renderPanel(over: Partial<Run> = {}, onApprove = vi.fn()) {
  render(
    <PlanPanel
      run={run(over)}
      busy={false}
      canSteer
      onApprove={onApprove}
      onReject={vi.fn()}
    />,
  );
  return { onApprove };
}

describe("PlanPanel — gate credential path (PRD #1247 M7)", () => {
  it("sets the run credential BEFORE onApprove when the user pins a token", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    mockApi.setRunCredential.mockResolvedValue({ run: run() });
    const { onApprove } = renderPanel();

    // Pin the token in the gate picker (this is the user CHANGING the selection).
    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    const opt = (await within(select).findByRole("option", { name: /console-key/ })) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));

    await waitFor(() =>
      expect(mockApi.setRunCredential).toHaveBeenCalledWith("r1", { mode: "pinned", secret_id: "sec-1" }),
    );
    await waitFor(() => expect(onApprove).toHaveBeenCalled());
    // The credential set landed BEFORE the approve submitted.
    expect(mockApi.setRunCredential.mock.invocationCallOrder[0]).toBeLessThan(
      onApprove.mock.invocationCallOrder[0],
    );
    // The 1-arg approve contract is preserved (no capability override).
    expect(onApprove.mock.calls[0]).toHaveLength(1);
  });

  it("ABORTS the approve and surfaces the error when setRunCredential fails", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    mockApi.setRunCredential.mockRejectedValue(new ApiError(400, "pinned token was deleted"));
    const { onApprove } = renderPanel();

    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    const opt = (await within(select).findByRole("option", { name: /console-key/ })) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));

    // The endpoint's error is surfaced at the gate…
    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/pinned token was deleted/i));
    // …and the approve was ABORTED — the run must not implement on the wrong credential.
    expect(onApprove).not.toHaveBeenCalled();
  });

  it("guards against a double-submit: two rapid approve clicks set the credential and approve once", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    // Hold the credential set in-flight so the second click races the first (the exact
    // window doApprove's await opens before RunView's `busy` can rise).
    let resolveSet: (v: { run: Run }) => void = () => {};
    mockApi.setRunCredential.mockReturnValue(
      new Promise<{ run: Run }>((res) => {
        resolveSet = res;
      }),
    );
    const { onApprove } = renderPanel();

    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    const opt = (await within(select).findByRole("option", { name: /console-key/ })) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });

    const approve = screen.getByRole("button", { name: /Approve plan/ });
    // Two synchronous clicks while the first credential set is still pending.
    fireEvent.click(approve);
    fireEvent.click(approve);

    // The synchronous in-flight ref blocked the second click: exactly one credential set.
    expect(mockApi.setRunCredential).toHaveBeenCalledTimes(1);

    // Let the in-flight set resolve; the approve submits exactly once.
    resolveSet({ run: run() });
    await waitFor(() => expect(onApprove).toHaveBeenCalledTimes(1));
  });

  it("skips setRunCredential and keeps the 1-arg approve when the picker is untouched (inherit)", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    const { onApprove } = renderPanel();
    // Let the seed's self-fetch settle so the untouched decision is genuine.
    await waitFor(() => expect(mockApi.listSecrets).toHaveBeenCalled());

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));

    await waitFor(() => expect(onApprove).toHaveBeenCalled());
    expect(mockApi.setRunCredential).not.toHaveBeenCalled();
    expect(onApprove.mock.calls[0]).toHaveLength(1);
  });

  it("leaves a seeded existing override untouched: shows it selected, sends nothing on approve", async () => {
    // A run created WITH a pinned override: the picker seeds to that token (re-resolved
    // label→id once the list loads), and leaving it untouched must NOT re-send it — the
    // create-time override is preserved by sending nothing.
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    const { onApprove } = renderPanel({ credential_override: { mode: "pinned", label: "console-key" } });

    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    // The seed resolved to the pinned token's id (positive: it shows the current choice).
    await waitFor(() => expect(select.value).toBe("sec-1"));

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));

    await waitFor(() => expect(onApprove).toHaveBeenCalled());
    expect(mockApi.setRunCredential).not.toHaveBeenCalled();
    expect(onApprove.mock.calls[0]).toHaveLength(1);
  });

  it("sends an explicit inherit when the user touches an overridden picker back to Inherit", async () => {
    // A run WITH an override. Picking Inherit is a MEANINGFUL action at the gate: it must
    // clear the run's override back to the worker binding, so a TOUCHED inherit sends the
    // switch-body {mode:"inherit"} — NOT nothing. On the old code runCredentialBody(inherit)
    // returned undefined and the call was skipped, leaving the stale override in place.
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    mockApi.setRunCredential.mockResolvedValue({ run: run() });
    const { onApprove } = renderPanel({ credential_override: { mode: "pinned", label: "console-key" } });

    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    // The seed resolves to the pinned token first (so the switch to Inherit is a real edit)…
    await waitFor(() => expect(select.value).toBe("sec-1"));
    // …then the user picks Inherit.
    fireEvent.change(select, { target: { value: "mode:inherit" } });

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));

    // The explicit inherit is sent BEFORE approve — the run's override is cleared.
    await waitFor(() => expect(mockApi.setRunCredential).toHaveBeenCalledWith("r1", { mode: "inherit" }));
    await waitFor(() => expect(onApprove).toHaveBeenCalled());
    expect(mockApi.setRunCredential.mock.invocationCallOrder[0]).toBeLessThan(
      onApprove.mock.invocationCallOrder[0],
    );
  });

  it("states accurate help copy: worker binding with no override, keep-current with one", () => {
    // No override → inherit follows the worker binding.
    renderPanel();
    expect(screen.getByText(/Inherit keeps the worker’s current binding\./)).toBeTruthy();
    cleanup();

    // With an override → leaving the current choice keeps this run's override (it does NOT
    // revert to the worker binding at the gate).
    renderPanel({ credential_override: { mode: "auto", label: null } });
    expect(screen.getByText(/Leaving the current choice keeps this run’s override\./)).toBeTruthy();
  });

  // Fix 2 (M4a review): a run whose ACTUAL harness is codex cannot switch/set an
  // Anthropic token at the gate (D5 — using the control would 422), so the gate token
  // picker must be hidden — gated on run.harness, not just canSteer.
  it("hides the plan-gate token picker for a codex-harness run", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    renderPanel({ harness: "codex" });
    // Give the (should-be-skipped) self-fetch a tick to settle either way.
    await waitFor(() => expect(mockApi.listSecrets).not.toHaveBeenCalled());
    expect(screen.queryByLabelText("Anthropic token for the implementation phase")).toBeNull();
  });

  // Positive control for the case above: an otherwise-identical claude-harness run
  // still shows the gate token picker — the fix must not hide it universally.
  it("still shows the plan-gate token picker for a claude-harness run", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    renderPanel({ harness: "claude" });
    expect(await screen.findByLabelText("Anthropic token for the implementation phase")).toBeTruthy();
  });
});

// PRD #1795 D5: every verdict is bound to the plan-gate revision of the plan the owner is
// looking at, frozen when the action starts, so a refetch that re-presents a newer plan while
// the owner is mid-action cannot silently re-point the verdict at a plan they have not read.
describe("PlanPanel — verdicts bound to the gate revision (PRD #1795 M4)", () => {
  type Handlers = {
    onApprove: ReturnType<typeof vi.fn<(s: AgentSelectionInput, o?: boolean, r?: number) => void>>;
    onReject: ReturnType<typeof vi.fn<(reason: string, r?: number) => void>>;
    onRequestChanges: ReturnType<typeof vi.fn<(feedback: string, r?: number) => void | Promise<boolean>>>;
  };
  function panel(
    r: Run,
    h: Handlers,
    revisionMismatch: { current: number; expected?: number } | null = null,
    messages: RunMessage[] = [],
  ) {
    return (
      <PlanPanel
        run={r}
        messages={messages}
        busy={false}
        canSteer
        onApprove={h.onApprove}
        onReject={h.onReject}
        onRequestChanges={h.onRequestChanges}
        revisionMismatch={revisionMismatch}
      />
    );
  }
  function handlers(): Handlers {
    return {
      onApprove: vi.fn<(s: AgentSelectionInput, o?: boolean, r?: number) => void>(),
      onReject: vi.fn<(reason: string, r?: number) => void>(),
      onRequestChanges: vi.fn<(feedback: string, r?: number) => void | Promise<boolean>>(),
    };
  }
  function msg(seq: number, kind: string, payload: unknown): RunMessage {
    return { seq, kind, agent: "lead", agent_instance: null, agent_label: null, payload, created_at: "2026-09-27T00:00:00Z" };
  }

  it("approve sends the displayed revision", async () => {
    const h = handlers();
    render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));
    await waitFor(() => expect(h.onApprove).toHaveBeenCalledTimes(1));
    expect(h.onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] }, undefined, 2);
  });

  it("the capability-override approve sends the displayed revision too", async () => {
    const h = handlers();
    render(panel(run({ gate_revision: 5, required_capabilities: ["docker"], worker_id: null }), h));
    fireEvent.click(screen.getByRole("button", { name: /Run without docker/ }));
    await waitFor(() => expect(h.onApprove).toHaveBeenCalledTimes(1));
    expect(h.onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] }, true, 5);
  });

  // Behavioural, not a pin of one line: doApprove closes over the render that was clicked, so
  // the value it sends is the clicked render's revision whether or not it is copied into a
  // local first. This guards a refactor that reads a latest-run ref after the await.
  it("approve binds the revision shown at the click even when a refetch lands during the credential await", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [token()] });
    let resolveSet: (v: { run: Run }) => void = () => {};
    mockApi.setRunCredential.mockReturnValue(
      new Promise<{ run: Run }>((res) => {
        resolveSet = res;
      }),
    );
    const h = handlers();
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    const select = screen.getByLabelText("Anthropic token for the implementation phase") as HTMLSelectElement;
    const opt = (await within(select).findByRole("option", { name: /console-key/ })) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });

    fireEvent.click(screen.getByRole("button", { name: /Approve plan/ }));
    // A refetch lands mid-await with a re-presented plan.
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h));
    resolveSet({ run: run() });

    await waitFor(() => expect(h.onApprove).toHaveBeenCalledTimes(1));
    expect(h.onApprove.mock.calls[0][2]).toBe(2);
  });

  it("the reject composer freezes the revision it was opened on through a refetch", async () => {
    const h = handlers();
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));

    // While the owner types, a refetch re-presents revision 3.
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h));
    expect(screen.getByText("plan three")).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText(/sent back to the agent/), { target: { value: "no" } });
    fireEvent.click(screen.getByRole("button", { name: "Send rejection" }));

    expect(h.onReject).toHaveBeenCalledWith("no", 2);
  });

  it("the request-changes composer freezes the revision it was opened on through a refetch", async () => {
    const h = handlers();
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));

    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h));
    fireEvent.change(screen.getByPlaceholderText(/sent to the planning session/), {
      target: { value: "split M2" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));

    expect(h.onRequestChanges).toHaveBeenCalledWith("split M2", 2);
  });

  it("cancelling a composer releases the freeze: the next action binds to the displayed revision", async () => {
    const h = handlers();
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    fireEvent.click(screen.getByRole("button", { name: "Send rejection" }));
    expect(h.onReject).toHaveBeenCalledWith("", 3);
  });

  // PRD #1795 B6: a refusal never re-points an open composer. The caller's refetch is bounded and
  // may not land (or may show a revision other than the one the 409 reported), so the retry still
  // names the revision the composer was opened on and is refused again, instead of being applied to
  // a plan the owner has not seen. Re-binding is explicit, to the DISPLAYED plan (below).
  const RESTART = /Decide on the displayed plan/;
  const composers = [
    {
      name: "reject",
      open: "Reject",
      box: /sent back to the agent/,
      send: "Send rejection",
      calls: (h: Handlers) => h.onReject,
    },
    {
      name: "request-changes",
      open: "Request changes",
      box: /sent to the planning session/,
      send: /Send & revise/,
      calls: (h: Handlers) => h.onRequestChanges,
    },
  ] as const;

  for (const c of composers) {
    it(`a mismatch keeps the open ${c.name} composer frozen, with the notice, before and after the refetch`, async () => {
      const h = handlers();
      const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
      fireEvent.click(screen.getByRole("button", { name: c.open }));
      fireEvent.change(screen.getByPlaceholderText(c.box), { target: { value: "draft" } });
      fireEvent.click(screen.getByRole("button", { name: c.send }));
      expect(c.calls(h)).toHaveBeenLastCalledWith("draft", 2);

      // The refusal arrives (current 3) but the refetch has not landed: plan two is still shown.
      rerender(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, { current: 3, expected: 2 }));
      expect(
        screen.getByText("Your decision was not applied: the run now shows plan revision 3. Review it and decide again."),
      ).toBeTruthy();
      // Nothing to re-bind to yet: the plan shown is the one the composer is frozen on.
      expect(screen.queryByRole("button", { name: RESTART })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: c.send }));
      expect(c.calls(h)).toHaveBeenLastCalledWith("draft", 2);

      // The refetch lands with plan three: the retry is still bound to 2 (refused again server-side).
      rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h, { current: 3, expected: 2 }));
      expect(screen.getByText("plan three")).toBeTruthy();
      const send = screen.getByRole("button", { name: c.send }) as HTMLButtonElement;
      expect(send.disabled).toBe(false);
      fireEvent.click(send);
      expect(c.calls(h)).toHaveBeenLastCalledWith("draft", 2);
      expect(c.calls(h)).toHaveBeenCalledTimes(3);
    });

    it(`the ${c.name} composer offers the restart only once a newer plan is displayed, and it binds to it`, async () => {
      const h = handlers();
      const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
      fireEvent.click(screen.getByRole("button", { name: c.open }));
      expect(screen.queryByRole("button", { name: RESTART })).toBeNull();
      fireEvent.change(screen.getByPlaceholderText(c.box), { target: { value: "keep this" } });

      rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h, { current: 3, expected: 2 }));
      const restart = screen.getByRole("button", { name: "Decide on the displayed plan (revision 3)" });
      expect(screen.getByText(/This draft is for plan revision 2; the plan shown is revision 3/)).toBeTruthy();
      fireEvent.click(restart);

      // Re-bound: the offer is gone and the draft survives.
      expect(screen.queryByRole("button", { name: RESTART })).toBeNull();
      expect((screen.getByPlaceholderText(c.box) as HTMLTextAreaElement).value).toBe("keep this");
      fireEvent.click(screen.getByRole("button", { name: c.send }));
      expect(c.calls(h)).toHaveBeenLastCalledWith("keep this", 3);
    });

    // N+2: the 409 reported 3 but the refetch already shows 4. The restart binds to what is on
    // screen (4), never to the reported revision (3), which the owner never saw.
    it(`the ${c.name} restart binds to the displayed revision, not the one the 409 reported`, async () => {
      const h = handlers();
      const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
      fireEvent.click(screen.getByRole("button", { name: c.open }));
      fireEvent.change(screen.getByPlaceholderText(c.box), { target: { value: "n+2" } });
      fireEvent.click(screen.getByRole("button", { name: c.send }));

      rerender(panel(run({ gate_revision: 4, plan_md: "plan four" }), h, { current: 3, expected: 2 }));
      expect(screen.queryByRole("button", { name: "Decide on the displayed plan (revision 3)" })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Decide on the displayed plan (revision 4)" }));
      expect((screen.getByPlaceholderText(c.box) as HTMLTextAreaElement).value).toBe("n+2");
      fireEvent.click(screen.getByRole("button", { name: c.send }));
      expect(c.calls(h)).toHaveBeenLastCalledWith("n+2", 4);
    });
  }

  // With no composer open at the refusal, the next composer opens on the plan displayed at that
  // moment (nothing carries the refused or reported revision forward).
  it("after a mismatch with no composer open, the next composer binds to the displayed revision", async () => {
    const h = handlers();
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h, { current: 3, expected: 2 }));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    expect(screen.queryByRole("button", { name: RESTART })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Send rejection" }));
    expect(h.onReject).toHaveBeenLastCalledWith("", 3);
  });

  it("a same-revision refusal asks for a retry and does not claim the plan changed", async () => {
    const h = handlers();
    render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, { current: 2, expected: 2 }));
    const notice = screen.getByText(/Your decision on plan revision 2 was not applied/);
    expect(notice.textContent).toMatch(/Check the plan and try again/);
    expect(notice.textContent).not.toMatch(/plan changed|now shows|no longer waiting/);
  });

  it("a refusal at revision 0 names no revision", async () => {
    const h = handlers();
    render(panel(run({ plan_md: "legacy" }), h, { current: 0, expected: 4 }));
    const notice = screen.getByText(/Your decision was not applied because the plan gate changed/);
    expect(notice.textContent).not.toMatch(/revision 0/);
  });

  // Blocking regression (#1795 M4 review): the composer used to survive the revising state and
  // reopen on plan N+1 still frozen at N, so every second revise round was refused with a 409.
  it("a second request-changes round binds the new revision (revising resets the composer)", async () => {
    const h = handlers();
    const plan2 = [msg(1, "plan", { plan_md: "plan two" })];
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, null, plan2));
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    fireEvent.change(screen.getByPlaceholderText(/sent to the planning session/), { target: { value: "split M2" } });
    fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));
    expect(h.onRequestChanges).toHaveBeenLastCalledWith("split M2", 2);

    // The run stays awaiting_approval while the planner revises: the panel stays mounted.
    const revising = [...plan2, msg(2, "plan_feedback", { feedback: "split M2" }), msg(3, "plan_revising", {})];
    rerender(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, null, revising));
    expect(screen.getByText("Revising the plan")).toBeTruthy();

    // Plan three arrives at gate revision 3.
    const plan3 = [...revising, msg(4, "plan", { plan_md: "plan three" })];
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h, null, plan3));
    // The old composer is gone: the header actions are back and the feedback box is not open.
    expect(screen.queryByPlaceholderText(/sent to the planning session/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    const box = screen.getByPlaceholderText(/sent to the planning session/) as HTMLTextAreaElement;
    expect(box.value).toBe("");
    fireEvent.change(box, { target: { value: "tighten M3" } });
    fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));
    expect(h.onRequestChanges).toHaveBeenLastCalledWith("tighten M3", 3);
  });

  // Blocking regression (#1795 round-2 review): the revising reset belongs to the request-changes
  // composer only. An open REJECT composer keeps the revision it was opened on through a revising
  // round, so a rejection typed against plan N is refused (409) rather than applied to plan N+1.
  it("an open reject composer keeps its frozen revision through a revising round", async () => {
    const h = handlers();
    const plan2 = [msg(1, "plan", { plan_md: "plan two" })];
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, null, plan2));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));

    const revising = [...plan2, msg(2, "plan_feedback", { feedback: "split M2" }), msg(3, "plan_revising", {})];
    rerender(panel(run({ gate_revision: 2, plan_md: "plan two" }), h, null, revising));
    expect(screen.getByText("Revising the plan")).toBeTruthy();

    const plan3 = [...revising, msg(4, "plan", { plan_md: "plan three" })];
    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h, null, plan3));
    fireEvent.click(screen.getByRole("button", { name: "Send rejection" }));
    expect(h.onReject).toHaveBeenCalledWith("", 2);
  });

  // The same reset fires on a send the caller reports accepted, for a feed that never delivers
  // the plan_revising frame (a dropped frame; the next plan arrives by refetch).
  it("an accepted revise closes the composer even without a plan_revising frame", async () => {
    const h = handlers();
    h.onRequestChanges.mockResolvedValue(true);
    const { rerender } = render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    fireEvent.change(screen.getByPlaceholderText(/sent to the planning session/), { target: { value: "split M2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));
    });
    expect(screen.queryByPlaceholderText(/sent to the planning session/)).toBeNull();

    rerender(panel(run({ gate_revision: 3, plan_md: "plan three" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    fireEvent.change(screen.getByPlaceholderText(/sent to the planning session/), { target: { value: "again" } });
    fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));
    expect(h.onRequestChanges).toHaveBeenLastCalledWith("again", 3);
  });

  it("a refused revise keeps the composer open with the owner's text", async () => {
    const h = handlers();
    h.onRequestChanges.mockResolvedValue(false);
    render(panel(run({ gate_revision: 2, plan_md: "plan two" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    fireEvent.change(screen.getByPlaceholderText(/sent to the planning session/), { target: { value: "split M2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Send & revise/ }));
    });
    const box = screen.getByPlaceholderText(/sent to the planning session/) as HTMLTextAreaElement;
    expect(box.value).toBe("split M2");
  });

  it("a run without a gate revision keeps the legacy arg counts (no revision sent)", async () => {
    const h = handlers();
    render(panel(run({ plan_md: "legacy" }), h));
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    fireEvent.click(screen.getByRole("button", { name: "Send rejection" }));
    expect(h.onReject).toHaveBeenCalledTimes(1);
    expect(h.onReject.mock.calls[0]).toHaveLength(1);
    // No notice without a refusal (paired with the positive notice case above).
    expect(screen.queryByText(/was not applied/)).toBeNull();
  });
});
