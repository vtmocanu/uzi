// @vitest-environment jsdom
//
// ScheduleModal tells the label warning of each repo it checks whether that repo is linked
// to an issue tracker (PRD 4173 M6), so the warning can drop its create-labels action. The
// edit path checks the one repoint target; the create path checks each selected repo.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";
import { api, type Repo, type Schedule } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { browserTimezone } from "../lib/timezone";
import { mockApi as realMockApi } from "../mocks/mockApi";

const seen = new Map<string, boolean | undefined>();

vi.mock("./SweepLabelWarn", () => ({
  SweepLabelWarn: ({ repoId, linked }: { repoId: string; linked?: boolean }) => {
    seen.set(repoId, linked);
    return null;
  },
}));
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listScheduleCatalog: vi.fn(),
      listRepos: vi.fn(),
      previewSchedule: vi.fn(),
      createSchedule: vi.fn(),
      updateSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
      listSecrets: vi.fn(),
      getMySettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("../lib/timezone", () => ({ browserTimezone: vi.fn() }));

const mockApi = vi.mocked(api);

const LINK = {
  connection_id: "itc-1",
  project_id: "0-4",
  project_short_name: "ATL",
  tag_id: "6-2",
  tag_name: "uzi",
  linked_at: "2026-11-19T10:00:00Z",
};

const plainRepo = { id: "repo-plain", path_with_namespace: "acme/plain" };
const linkedRepo = { id: "repo-linked", path_with_namespace: "acme/linked", issue_tracker_link: LINK };

function setRepos(repos: unknown[]) {
  mockApi.listRepos.mockResolvedValue({ repos: repos as Repo[] });
}

beforeEach(async () => {
  vi.mocked(browserTimezone).mockReturnValue("UTC");
  vi.mocked(useAuth).mockReturnValue({ uziLabel: "uzi" } as unknown as ReturnType<typeof useAuth>);
  mockApi.listScheduleCatalog.mockResolvedValue(await realMockApi.listScheduleCatalog());
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({
    settings: { default_harness: null } as unknown as Awaited<ReturnType<typeof api.getMySettings>>["settings"],
  });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  seen.clear();
});

function sweepSchedule(repoId: string): Schedule {
  return {
    id: "sch-1",
    repo_id: repoId,
    repo_path: "acme/x",
    target: "sweep",
    issue_iid: null,
    labels: ["bug"],
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * 1-5",
    run_at: null,
    timezone: "UTC",
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    remove_label_on_dispatch: false,
    capacity_limit: null,
    capacity_room_needed: null,
    max_issues: 10,
    guidance: null,
    baked_guidance: null,
    model: "fable",
    output_mode: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
  } as unknown as Schedule;
}

function renderEdit(repoId: string) {
  return render(
    <MemoryRouter>
      <ScheduleModal editing={sweepSchedule(repoId)} onClose={vi.fn()} onSaved={vi.fn()} />
    </MemoryRouter>,
  );
}

function renderCreate() {
  render(
    <MemoryRouter>
      <ScheduleModal onClose={vi.fn()} onSaved={vi.fn()} />
    </MemoryRouter>,
  );
  fireEvent.click(screen.getByRole("radio", { name: /Label sweep/ }));
}

describe("ScheduleModal — the label warning knows a repo linked to an issue tracker (PRD 4173 M6)", () => {
  it("edit: passes linked for a linked repoint target", async () => {
    setRepos([plainRepo, linkedRepo]);
    renderEdit("repo-linked");
    await waitFor(() => expect(seen.get("repo-linked")).toBe(true));
  });

  it("edit: passes not linked for an unlinked repoint target", async () => {
    setRepos([plainRepo, linkedRepo]);
    renderEdit("repo-plain");
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    await waitFor(() => expect(seen.has("repo-plain")).toBe(true));
    expect(seen.get("repo-plain")).toBe(false);
  });

  it("create: passes linked for the selected linked repo", async () => {
    setRepos([linkedRepo, plainRepo]);
    renderCreate();
    await waitFor(() => expect(seen.get("repo-linked")).toBe(true));
  });

  it("create: passes not linked for the selected unlinked repo", async () => {
    setRepos([plainRepo, linkedRepo]);
    renderCreate();
    await waitFor(() => expect(seen.has("repo-plain")).toBe(true));
    expect(seen.get("repo-plain")).toBe(false);
  });

  it("create: with a linked and a plain repo both selected, each warning gets its own linked", async () => {
    setRepos([plainRepo, linkedRepo]);
    renderCreate();
    await waitFor(() => expect(seen.has("repo-plain")).toBe(true));
    // The default selection is the first repo; add the other so both are selected.
    fireEvent.click(await screen.findByRole("checkbox", { name: /acme\/linked/ }));
    await waitFor(() => expect(seen.has("repo-linked")).toBe(true));
    expect(seen.get("repo-plain")).toBe(false);
    expect(seen.get("repo-linked")).toBe(true);
  });
});
