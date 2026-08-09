// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Repos } from "./Repos";
import { api, type ForgeConnection, type Repo } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listConnections: vi.fn(),
      listProjects: vi.fn(),
      setRepoEnabled: vi.fn(),
      setRepoSkillsEnabled: vi.fn(),
      setRepoClaudemdEnabled: vi.fn(),
      setRepoTrustFlags: vi.fn(),
      getRepoToolProfile: vi.fn(),
      listToolAllowlist: vi.fn(),
      setRepoToolProfile: vi.fn(),
      setRepoDevboxOptIn: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const CONN: ForgeConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "uzi-bot",
  bot_forge_user_id: 1,
  human_username: null,
  created_at: "2026-01-01T00:00:00Z",
  last_verified_at: "2026-07-05T12:00:00Z",
  privilege_status: null,
  privilege_checked_at: null,
  privilege_report: null,
};

function repo(over: Partial<Repo> & Pick<Repo, "id" | "path_with_namespace">): Repo {
  return {
    connection_id: "conn-1",
    forge_project_id: 1,
    web_url: `https://gitlab.example.com/${over.path_with_namespace}`,
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
    ...over,
  };
}

const REPOS: Repo[] = [
  // Untrusted: exercises the master enable → confirm path.
  repo({ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }),
  // Trusted, with a mix (skills on, instructions off): exercises the sub-toggle
  // independence and the immediate master-off path.
  repo({
    id: "repo-atlas",
    path_with_namespace: "vtmocanu/atlas",
    repo_skills_enabled: true,
    repo_claudemd_enabled: false,
  }),
  repo({ id: "repo-www", path_with_namespace: "example/website", enabled: false }),
];

function rowFor(name: string): HTMLElement {
  const tr = screen.getByText(name).closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

beforeEach(() => {
  mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
  mockApi.listProjects.mockResolvedValue({ repos: REPOS.map((r) => ({ ...r })) });
  mockApi.getRepoToolProfile.mockResolvedValue({ packages: [] });
  mockApi.listToolAllowlist.mockResolvedValue({ allowlist: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  );
}

// Open the Trusted-repo panel for a repo row and return its group element.
async function openTrustPanel(name: string): Promise<HTMLElement> {
  renderPage();
  await screen.findByText(name);
  fireEvent.click(within(rowFor(name)).getByRole("button", { name: /Trusted repo settings for/ }));
  return screen.getByRole("group", { name: new RegExp(`Trusted repo for ${name}`) });
}

describe("Repos — Trusted repo cell", () => {
  it("shows an Off badge + Manage on an enabled, untrusted repo", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    const row = within(rowFor("vtmocanu/uzi"));
    expect(row.getByText("Off")).toBeTruthy();
    expect(row.getByRole("button", { name: /Trusted repo settings for vtmocanu\/uzi/ })).toBeTruthy();
  });

  it("shows a Trusted badge when a capability is on", async () => {
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    expect(within(rowFor("vtmocanu/atlas")).getByText("Trusted")).toBeTruthy();
  });

  it("a disabled repo has no Trusted-repo control", async () => {
    renderPage();
    await screen.findByText("example/website");
    const row = within(rowFor("example/website"));
    expect(row.queryByRole("button", { name: /Trusted repo settings/ })).toBeNull();
  });
});

describe("Repos — Trusted repo panel", () => {
  it("expands the panel below the table and renders the guardrails strip", async () => {
    const panel = await openTrustPanel("vtmocanu/uzi");
    expect(within(panel).getByText("Guardrails are unchanged")).toBeTruthy();
    // The master switch is present and reflects the derived (off) state.
    expect(within(panel).getByRole("switch", { name: "Trusted repo" }).getAttribute("aria-checked")).toBe("false");
  });

  it("renders the panel OUTSIDE the horizontal-scroll container (never clipped)", async () => {
    const panel = await openTrustPanel("vtmocanu/uzi");
    expect(panel.closest(".overflow-x-auto")).toBeNull();
  });

  it("moves focus into the panel when it opens", async () => {
    await openTrustPanel("vtmocanu/uzi");
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("switch", { name: "Trusted repo" })),
    );
  });

  it("enabling the master is gated behind a confirm mentioning CLAUDE.md + guardrails", async () => {
    mockApi.setRepoTrustFlags.mockResolvedValue({
      repo: { ...REPOS[0], repo_skills_enabled: true, repo_claudemd_enabled: true },
    });
    const panel = await openTrustPanel("vtmocanu/uzi");

    fireEvent.click(within(panel).getByRole("switch", { name: "Trusted repo" }));

    // The confirm spells out both capabilities and that guardrails do not change.
    expect(screen.getByText("CLAUDE.md")).toBeTruthy();
    expect(screen.getByText(/advisory/i)).toBeTruthy();
    // Unique to the confirm copy (the strip heading also says "guardrails are unchanged").
    expect(screen.getByText(/trust grants context, never/i)).toBeTruthy();
    // Nothing changed yet — enabling is behind the confirm.
    expect(mockApi.setRepoTrustFlags).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Mark as trusted" }));
    await waitFor(() =>
      expect(mockApi.setRepoTrustFlags).toHaveBeenCalledWith("repo-uzi", {
        repo_skills_enabled: true,
        repo_claudemd_enabled: true,
      }),
    );
  });

  it("disabling the master is immediate and patches both flags false", async () => {
    mockApi.setRepoTrustFlags.mockResolvedValue({
      repo: { ...REPOS[1], repo_skills_enabled: false, repo_claudemd_enabled: false },
    });
    const panel = await openTrustPanel("vtmocanu/atlas");

    fireEvent.click(within(panel).getByRole("switch", { name: "Trusted repo" }));
    await waitFor(() =>
      expect(mockApi.setRepoTrustFlags).toHaveBeenCalledWith("repo-atlas", {
        repo_skills_enabled: false,
        repo_claudemd_enabled: false,
      }),
    );
  });

  it("the Repo skills sub-toggle patches only repo_skills_enabled", async () => {
    mockApi.setRepoSkillsEnabled.mockResolvedValue({
      repo: { ...REPOS[1], repo_skills_enabled: false },
    });
    const panel = await openTrustPanel("vtmocanu/atlas");

    // Skills is on for atlas, so toggling turns it off.
    fireEvent.click(within(panel).getByRole("switch", { name: "Repo skills" }));
    await waitFor(() => expect(mockApi.setRepoSkillsEnabled).toHaveBeenCalledWith("repo-atlas", false));
    expect(mockApi.setRepoClaudemdEnabled).not.toHaveBeenCalled();
  });

  it("the Repo instructions sub-toggle patches only repo_claudemd_enabled", async () => {
    mockApi.setRepoClaudemdEnabled.mockResolvedValue({
      repo: { ...REPOS[1], repo_claudemd_enabled: true },
    });
    const panel = await openTrustPanel("vtmocanu/atlas");

    // Instructions is off for atlas, so toggling turns it on.
    fireEvent.click(within(panel).getByRole("switch", { name: "Repo instructions" }));
    await waitFor(() => expect(mockApi.setRepoClaudemdEnabled).toHaveBeenCalledWith("repo-atlas", true));
    expect(mockApi.setRepoSkillsEnabled).not.toHaveBeenCalled();
  });

  it("disables every trust control while any trust PATCH is in flight", async () => {
    // Hold the instructions PATCH open so we can observe the in-flight state.
    // Skills stays on throughout, so atlas remains trusted and the panel's
    // sub-toggles stay mounted before and after the PATCH resolves.
    let resolveClaudemd!: (v: { repo: Repo }) => void;
    mockApi.setRepoClaudemdEnabled.mockReturnValue(
      new Promise((r) => {
        resolveClaudemd = r;
      }),
    );
    const panel = await openTrustPanel("vtmocanu/atlas");

    // Instructions is off for atlas, so this toggles it on and leaves the PATCH pending.
    fireEvent.click(within(panel).getByRole("switch", { name: "Repo instructions" }));

    const skillsSwitch = () => within(panel).getByRole("switch", { name: "Repo skills" }) as HTMLButtonElement;

    // While that PATCH is pending, the sibling skills toggle and the master switch
    // are both disabled — no second PATCH can race the first and clobber its result.
    await waitFor(() => expect(skillsSwitch().disabled).toBe(true));
    expect((within(panel).getByRole("switch", { name: "Trusted repo" }) as HTMLButtonElement).disabled).toBe(true);

    // Resolving the PATCH clears the busy state and re-enables the controls.
    resolveClaudemd({ repo: { ...REPOS[1], repo_skills_enabled: true, repo_claudemd_enabled: true } });
    await waitFor(() => expect(skillsSwitch().disabled).toBe(false));
  });
});

describe("Repos — tier-2 devbox opt-in (PRD #18 M5)", () => {
  it("opens the Tools panel and toggles the repo devbox opt-in", async () => {
    mockApi.setRepoDevboxOptIn.mockResolvedValue({
      repo: { ...REPOS[0], repo_devbox_opt_in: true },
    });
    renderPage();
    await screen.findByText("vtmocanu/uzi");

    // Open the per-repo Tools panel.
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: "Tools" }));
    // The trust toggle appears (allowlist is empty, so it's the only checkbox).
    const toggle = await screen.findByRole("checkbox");
    fireEvent.click(toggle);

    await waitFor(() => expect(mockApi.setRepoDevboxOptIn).toHaveBeenCalledWith("repo-uzi", true));
  });
});
