// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunsList } from "./RunsList";
import { api, type RunListItem } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Keep the real module (isTerminalRun etc.) and mock only the network + auth so the
// list renders offline. A non-admin viewer skips the admin fetches.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listRuns: vi.fn(), adminListRuns: vi.fn(), adminListWorkers: vi.fn() },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    kind: "issue",
    issue_iid: 7,
    issue_title: "A run",
    issue_description: "",
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    mr_iid: null,
    failure_reason: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "grp/proj",
    worker_name: "w1",
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { is_admin: false } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RunsList — waiting for vault unlock (PRD #32)", () => {
  it("renders own queued runs as waiting for vault unlock while locked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText(/waiting for vault unlock/)).toBeTruthy();
    // The bare "queued" pill must not also render for that run.
    expect(screen.queryByText("queued")).toBeNull();
  });

  it("admin all-users list: only the admin's OWN queued row shows waiting-for-unlock", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true, email: "me@uzi.test" },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "mine", issue_title: "My queued", status: "queued", owner_email: "me@uzi.test" }),
        aRun({ id: "theirs", issue_title: "Their queued", status: "queued", owner_email: "other@uzi.test" }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("My queued")).toBeTruthy());
    // Exactly one waiting badge — the admin's own row; the other owner's row stays
    // a plain "queued" pill (their vault state is unknown here).
    expect(screen.getAllByText(/waiting for vault unlock/)).toHaveLength(1);
    expect(screen.getByText("queued")).toBeTruthy();
  });

  it("renders a plain queued pill when the vault is unlocked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText("queued")).toBeTruthy();
    expect(screen.queryByText(/waiting for vault unlock/)).toBeNull();
  });
});

describe("RunsList — autopilot badge", () => {
  it("shows the autopilot badge only for an auto_approve run", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "auto", issue_title: "Autopilot run", auto_approve: true }),
        aRun({ id: "manual", issue_title: "Manual run", auto_approve: false }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Autopilot run")).toBeTruthy());
    expect(screen.getByText("Manual run")).toBeTruthy();
    // Exactly one badge — the manual run must not carry it.
    expect(screen.getAllByText("autopilot")).toHaveLength(1);
  });
});
