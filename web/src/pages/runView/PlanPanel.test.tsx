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
import { api, ApiError, type Run, type SecretMeta } from "../../lib/api";

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
});
