// @vitest-environment jsdom
//
// PRD #1247 M7 run-view credential surfaces, tested as UNITS (RunView itself needs
// routing + a live stream to mount). Covers: the override + pending-switch badges, the
// stale-switch suppression (a null credential_switch shows no badge), the epoch history
// (labels + reasons + times, with a Cf label sanitized), and the "Switch token" action —
// HIDDEN for every refused lane (chat / judge / self_improve / task_review), inert for a
// non-owner, shown for a switchable owner, and surfacing the D6 warning on success.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import {
  CredentialEpochList,
  RunCredentialOverride,
  SwitchTokenAction,
} from "./RunCredentialOverride";
import { api, type CredentialEpoch, type Run, type SecretMeta } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, setRunCredential: vi.fn(), listSecrets: vi.fn() } };
});
const mockApi = vi.mocked(api);

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

function run(over: Partial<Run>): Run {
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
    status: "running",
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

describe("RunCredentialOverride — override + pending switch", () => {
  it("renders a pinned override's mode and label", () => {
    render(<RunCredentialOverride run={run({ credential_override: { mode: "pinned", label: "console-key" } })} />);
    expect(screen.getByText(/token override:/)).toBeTruthy();
    expect(screen.getByText(/pinned/)).toBeTruthy();
    expect(screen.getByText(/console-key/)).toBeTruthy();
  });

  it("renders the pending-switch badge for a requested switch", () => {
    render(<RunCredentialOverride run={run({ credential_switch: "requested" })} />);
    expect(screen.getByText("switch pending")).toBeTruthy();
  });

  it("renders the released-switch badge for a released switch", () => {
    render(<RunCredentialOverride run={run({ credential_switch: "released" })} />);
    expect(screen.getByText("switch released")).toBeTruthy();
  });

  it("shows NO stale switch badge once the DTO reports credential_switch null (lingering-stamp)", () => {
    // The applied switch left an override + epochs, but credential_switch is null — the
    // Step-A suppression. The override still renders; no pending/released badge does.
    const { container } = render(
      <RunCredentialOverride
        run={run({ credential_override: { mode: "pinned", label: "console-key" }, credential_switch: null })}
      />,
    );
    expect(screen.getByText(/token override:/)).toBeTruthy();
    expect(within(container).queryByText("switch pending")).toBeNull();
    expect(within(container).queryByText("switch released")).toBeNull();
  });
});

describe("CredentialEpochList", () => {
  const epochs: CredentialEpoch[] = [
    { claim_generation: 1, label: "default", select_reason: "default", applied_at: "2026-01-01T10:00:00Z" },
    { claim_generation: 2, label: "console-key", select_reason: "override_pinned", applied_at: "2026-01-01T11:00:00Z" },
  ];

  it("renders one row per epoch with its token label and reason", () => {
    render(<CredentialEpochList epochs={epochs} />);
    expect(screen.getByText("Token history")).toBeTruthy();
    expect(screen.getByText("default")).toBeTruthy();
    expect(screen.getByText("console-key")).toBeTruthy();
    expect(screen.getByText(/pinned by override/)).toBeTruthy();
  });

  it("routes a user-authored label through sanitizeLabel and onto its title/aria-label", () => {
    const bidi = String.fromCodePoint(0x202e); // U+202E RTL override, category Cf
    render(
      <CredentialEpochList
        epochs={[{ claim_generation: 1, label: `con${bidi}sole`, select_reason: "pinned", applied_at: "2026-01-01T10:00:00Z" }]}
      />,
    );
    const labelEl = screen.getByText("console");
    expect(labelEl.getAttribute("title")).toBe("console");
    expect(labelEl.getAttribute("aria-label")).toBe("console");
    expect(labelEl.textContent).not.toContain(bidi);
  });
});

describe("SwitchTokenAction — refused lanes are hidden", () => {
  it.each([
    ["chat", run({ kind: "chat" })],
    ["judge", run({ kind: "judge" })],
    ["self_improve", run({ kind: "self_improve" })],
    ["task_review (trigger_source)", run({ kind: "task", trigger_source: "task_review" })],
  ])("renders nothing for a %s run", (_name, refused) => {
    const { container } = render(<SwitchTokenAction run={refused} canSteer tokens={[token()]} />);
    // Positive structural assertion: the component rendered nothing at all.
    expect(container.firstChild).toBeNull();
  });
});

describe("SwitchTokenAction — owner vs non-owner", () => {
  it("shows the Switch token control for a switchable owner", () => {
    render(<SwitchTokenAction run={run({ kind: "issue", status: "running" })} canSteer tokens={[token()]} />);
    expect(screen.getByRole("button", { name: "Switch token" })).toBeTruthy();
  });

  it("shows inert text (no button) for a non-owner", () => {
    render(<SwitchTokenAction run={run({ kind: "issue", status: "running" })} canSteer={false} tokens={[token()]} />);
    expect(screen.getByText(/Only the run.s owner can switch its token/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Switch token" })).toBeNull();
  });
});

describe("SwitchTokenAction — switching + D6 warning", () => {
  it("shows the interrupt caveat for a running run and surfaces the endpoint's warning", async () => {
    const updated = run({ kind: "issue", status: "running", credential_override: { mode: "pinned", label: "console-key" } });
    mockApi.setRunCredential.mockResolvedValue({
      run: updated,
      warning: "No token is currently eligible in your auto pool; the run will hold until one frees up.",
    });
    const onSwitched = vi.fn();
    render(
      <SwitchTokenAction run={run({ kind: "issue", status: "running" })} canSteer tokens={[token()]} onSwitched={onSwitched} />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Switch token" }));
    // The interrupt caveat is shown for a live (running) run.
    expect(screen.getByText(/loses at most the in-flight step/)).toBeTruthy();

    // Pin a token, then confirm the switch.
    const select = screen.getByLabelText("Switch this run's Anthropic token") as HTMLSelectElement;
    const opt = within(select).getByRole("option", { name: /console-key/ }) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });
    fireEvent.click(screen.getByRole("button", { name: "Switch to this token" }));

    await waitFor(() =>
      expect(mockApi.setRunCredential).toHaveBeenCalledWith("r1", { mode: "pinned", secret_id: "sec-1" }),
    );
    // The D6 warning the endpoint returned is surfaced.
    await waitFor(() => expect(screen.getByText(/no token is currently eligible/i)).toBeTruthy());
    expect(onSwitched).toHaveBeenCalledWith(updated);
  });
});
