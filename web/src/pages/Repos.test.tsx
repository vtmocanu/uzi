// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Repos } from "./Repos";
import { api, ApiError, type ForgeConnection, type Repo } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listConnections: vi.fn(),
      listProjects: vi.fn(),
      setRepoEnabled: vi.fn(),
      deleteRepo: vi.fn(),
      setRepoSkillsEnabled: vi.fn(),
      setRepoClaudemdEnabled: vi.fn(),
      setRepoTrustFlags: vi.fn(),
      getRepoToolProfile: vi.fn(),
      listToolAllowlist: vi.fn(),
      setRepoToolProfile: vi.fn(),
      setRepoDevboxOptIn: vi.fn(),
      setRepoRequiredCapabilities: vi.fn(),
      setRepoGuardrailOverride: vi.fn(),
      clearRepoGuardrailOverride: vi.fn(),
      getProjectSyncStatus: vi.fn(),
      getProjectSyncOwnerType: vi.fn(),
      provisionProjectSync: vi.fn(),
      adoptProjectSync: vi.fn(),
      resyncProjectSync: vi.fn(),
      autocreateProjectSyncColumns: vi.fn(),
      disableProjectSync: vi.fn(),
      getProjectSyncVisibility: vi.fn(),
      setProjectSyncVisibility: vi.fn(),
      shareProjectSync: vi.fn(),
      unshareProjectSync: vi.fn(),
    },
  };
});

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function asAdmin(isAdmin: boolean) {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: isAdmin },
  } as unknown as ReturnType<typeof useAuth>);
}

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
    guardrail_blocked: false,
    docker_allowlisted: false,
    docker_blocked: false,
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
  asAdmin(false);
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

describe("Repos — always-visible repo-agents guide link (PRD #57 M3)", () => {
  it("renders the repo agents guide link in the page intro", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    const docLink = screen.getByRole("link", { name: "repo agents" });
    expect(docLink.getAttribute("href")).toBe("/docs/repo-agents");
  });
});

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

describe("Repos — guardrail blocking badge (PRD #66 M4/M9, D4/D8)", () => {
  // A connection whose privilege report marks one repo block-severity (runs
  // refused), one warn-only (advisory), and leaves the third with no entry (clean).
  const CONN_WITH_REPORT: ForgeConnection = {
    ...CONN,
    privilege_status: "violations",
    privilege_checked_at: "2026-08-13T00:00:00Z",
    privilege_report: {
      checked_at: "2026-08-13T00:00:00Z",
      token: { scopes: [], active: true, violations: [], warnings: [] },
      status: "violations",
      repos: [
        {
          repo_id: "repo-uzi",
          path: "vtmocanu/uzi",
          role: "write",
          member: true,
          findings: [
            {
              code: "write_role_can_push",
              severity: "block",
              message: "the write role may push to protected main",
            },
          ],
        },
        {
          repo_id: "repo-atlas",
          path: "vtmocanu/atlas",
          role: "admin",
          member: true,
          findings: [
            {
              code: "bot_role_above_write",
              severity: "warn",
              message: "the bot is above the write role",
            },
          ],
        },
      ],
    },
  };

  // M9 (D8): the badge STATE is the SERVER's guardrail_blocked, not re-derived from
  // the report. repo-uzi is blocked; the report supplies the finding MESSAGE for the
  // title/modal. repo-atlas carries a warn-only finding and is not blocked.
  const REPORTED_REPOS: Repo[] = [
    { ...REPOS[0], guardrail_blocked: true },
    { ...REPOS[1], guardrail_blocked: false },
    { ...REPOS[2], guardrail_blocked: false },
  ];

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [CONN_WITH_REPORT] });
    mockApi.listProjects.mockResolvedValue({ repos: REPORTED_REPOS.map((r) => ({ ...r })) });
  });

  it("shows the 'runs blocked' badge on a server-blocked repo", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    const row = within(rowFor("vtmocanu/uzi"));
    const badge = row.getByText(/runs blocked/i);
    expect(badge).toBeTruthy();
    // The block finding's message is the actionable "sign on the wall" (D4).
    expect(badge.getAttribute("title")).toContain("the write role may push to protected main");
    // It is NOT the advisory wording.
    expect(row.queryByText(/privilege warning/i)).toBeNull();
  });

  it("keeps a warn-only repo on the advisory badge, never 'runs blocked'", async () => {
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    const row = within(rowFor("vtmocanu/atlas"));
    expect(row.queryByText(/runs blocked/i)).toBeNull();
    expect(row.getByText(/privilege warning/i)).toBeTruthy();
  });

  it("shows neither badge on a clean repo (no findings entry)", async () => {
    renderPage();
    await screen.findByText("example/website");
    const row = within(rowFor("example/website"));
    expect(row.queryByText(/runs blocked/i)).toBeNull();
    expect(row.queryByText(/privilege warning/i)).toBeNull();
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
    // The devbox trust toggle (distinct from the capability checkboxes below it).
    const toggle = await screen.findByRole("checkbox", { name: /Also trust this repo/i });
    fireEvent.click(toggle);

    await waitFor(() => expect(mockApi.setRepoDevboxOptIn).toHaveBeenCalledWith("repo-uzi", true));
  });
});

describe("Repos — static capability hint (PRD #84 M2)", () => {
  it("ticks a capability and PATCHes required_capabilities alone", async () => {
    mockApi.setRepoRequiredCapabilities.mockResolvedValue({
      repo: { ...REPOS[0], required_capabilities: ["docker"] },
    });
    renderPage();
    await screen.findByText("vtmocanu/uzi");

    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: "Tools" }));
    // The fixed vocabulary renders as two checkboxes; tick docker.
    const docker = await screen.findByRole("checkbox", { name: "docker" });
    expect((docker as HTMLInputElement).checked).toBe(false);
    fireEvent.click(docker);

    await waitFor(() =>
      expect(mockApi.setRepoRequiredCapabilities).toHaveBeenCalledWith("repo-uzi", ["docker"]),
    );
  });

  it("unticks a capability and PATCHes the shrunk list", async () => {
    mockApi.listProjects.mockResolvedValue({
      repos: [{ ...REPOS[0], required_capabilities: ["docker", "jvm"] }],
    });
    mockApi.setRepoRequiredCapabilities.mockResolvedValue({
      repo: { ...REPOS[0], required_capabilities: ["jvm"] },
    });
    renderPage();
    await screen.findByText("vtmocanu/uzi");

    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: "Tools" }));
    const docker = (await screen.findByRole("checkbox", { name: "docker" })) as HTMLInputElement;
    expect(docker.checked).toBe(true);
    fireEvent.click(docker);

    await waitFor(() =>
      expect(mockApi.setRepoRequiredCapabilities).toHaveBeenCalledWith("repo-uzi", ["jvm"]),
    );
  });
});

describe("Repos — admin per-repo override UI (PRD #66 M9, D8)", () => {
  const blockedRepo = (): Repo =>
    repo({ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi", guardrail_blocked: true });
  const overriddenRepo = (): Repo =>
    repo({
      id: "repo-atlas",
      path_with_namespace: "vtmocanu/atlas",
      guardrail_blocked: false,
      guardrail_override: { reason: "forge fix scheduled", by: "admin@x", at: "2026-08-10T00:00:00Z" },
    });

  it("a member sees 'ask an admin' and NO Allow-anyway control on a blocked repo", async () => {
    asAdmin(false);
    mockApi.listProjects.mockResolvedValue({ repos: [blockedRepo()] });
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    const row = within(rowFor("vtmocanu/uzi"));
    expect(row.getByText(/runs blocked/i)).toBeTruthy();
    expect(row.getByText(/ask an admin to allow this repo/i)).toBeTruthy();
    expect(row.queryByRole("button", { name: /allow anyway/i })).toBeNull();
  });

  it("an admin sees Allow-anyway; the modal requires a reason before POSTing", async () => {
    asAdmin(true);
    mockApi.listProjects.mockResolvedValue({ repos: [blockedRepo()] });
    mockApi.setRepoGuardrailOverride.mockResolvedValue({ repo: { ...blockedRepo(), guardrail_blocked: false } });
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /allow anyway/i }));

    // The modal opens and its Allow button is disabled until a reason is typed.
    const dialog = await screen.findByRole("dialog", { name: /allow runs on vtmocanu\/uzi/i });
    const allowBtn = within(dialog).getByRole("button", { name: /allow anyway/i });
    expect((allowBtn as HTMLButtonElement).disabled).toBe(true);

    // A blank/whitespace reason keeps it disabled and never calls the API.
    const textarea = within(dialog).getByRole("textbox");
    fireEvent.change(textarea, { target: { value: "   " } });
    expect((allowBtn as HTMLButtonElement).disabled).toBe(true);

    // A real reason enables it; clicking POSTs the reason and refetches.
    fireEvent.change(textarea, { target: { value: "accepting the risk until the forge fix" } });
    expect((allowBtn as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(allowBtn);
    await waitFor(() =>
      expect(mockApi.setRepoGuardrailOverride).toHaveBeenCalledWith(
        "repo-uzi",
        "accepting the risk until the forge fix",
      ),
    );
    // Refetch after the write.
    await waitFor(() => expect(mockApi.listProjects).toHaveBeenCalledTimes(2));
  });

  it("an overridden repo shows 'allowed by admin' and (admin) a Revoke that clears it", async () => {
    asAdmin(true);
    mockApi.listProjects.mockResolvedValue({ repos: [overriddenRepo()] });
    mockApi.clearRepoGuardrailOverride.mockResolvedValue({
      repo: { ...overriddenRepo(), guardrail_override: null },
    });
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    const row = within(rowFor("vtmocanu/atlas"));
    expect(row.getByText(/allowed by admin/i)).toBeTruthy();
    fireEvent.click(row.getByRole("button", { name: /revoke/i }));
    await waitFor(() => expect(mockApi.clearRepoGuardrailOverride).toHaveBeenCalledWith("repo-atlas"));
  });

  it("a member sees 'allowed by admin' but NO Revoke control", async () => {
    asAdmin(false);
    mockApi.listProjects.mockResolvedValue({ repos: [overriddenRepo()] });
    renderPage();
    await screen.findByText("vtmocanu/atlas");
    const row = within(rowFor("vtmocanu/atlas"));
    expect(row.getByText(/allowed by admin/i)).toBeTruthy();
    expect(row.queryByRole("button", { name: /revoke/i })).toBeNull();
  });

  it("closes the Allow-anyway modal on Escape and manages focus (a11y: shared Modal)", async () => {
    asAdmin(true);
    mockApi.listProjects.mockResolvedValue({ repos: [blockedRepo()] });
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    fireEvent.click(within(rowFor("vtmocanu/uzi")).getByRole("button", { name: /allow anyway/i }));

    const dialog = await screen.findByRole("dialog", { name: /allow runs on vtmocanu\/uzi/i });
    // On open, focus is inside the dialog, never left on the page behind the backdrop.
    expect(dialog.contains(document.activeElement)).toBe(true);

    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("Repos — per-repo remove action (PRD #357 M2, D2/D3)", () => {
  it("a disabled repo shows Remove; confirming calls deleteRepo and drops the row", async () => {
    mockApi.deleteRepo.mockResolvedValue(null);
    renderPage();
    // example/website is the disabled repo in the fixture.
    await screen.findByText("example/website");
    const row = within(rowFor("example/website"));

    // The Remove button reveals the row-inline confirm; nothing is deleted yet.
    fireEvent.click(row.getByRole("button", { name: /^Remove$/ }));
    expect(mockApi.deleteRepo).not.toHaveBeenCalled();
    const confirm = screen.getByRole("group", { name: /Confirm removing example\/website/ });
    // Confirm copy names the permanent consequence (board + run history).
    expect(within(confirm).getByText(/permanent and deletes its board and run history/i)).toBeTruthy();

    fireEvent.click(within(confirm).getByRole("button", { name: /Confirm remove/ }));
    await waitFor(() => expect(mockApi.deleteRepo).toHaveBeenCalledWith("repo-www"));
    // The row is gone from the list.
    await waitFor(() => expect(screen.queryByText("example/website")).toBeNull());
  });

  it("an enabled repo shows NO Remove button", async () => {
    renderPage();
    await screen.findByText("vtmocanu/uzi");
    // Sanity: the enabled repo still offers Disable, so the row is really rendered.
    const row = within(rowFor("vtmocanu/uzi"));
    expect(row.getByRole("button", { name: /^Disable$/ })).toBeTruthy();
    expect(row.queryByRole("button", { name: /^Remove$/ })).toBeNull();
  });
});

describe("Repos — enable guardrail 422 violations (PRD #345)", () => {
  it("renders the refusal violations and leaves the repo disabled, then clears on a later success", async () => {
    mockApi.setRepoEnabled.mockRejectedValue(
      new ApiError(422, "this repo was not enabled", {
        violations: ["reason one", "reason two"],
      }),
    );
    renderPage();
    // example/website is the disabled repo in the fixture — it shows an Enable toggle.
    await screen.findByText("example/website");
    const row = () => within(rowFor("example/website"));
    fireEvent.click(row().getByRole("button", { name: /^Enable$/ }));

    // POSITIVE: both violation strings surface in the DOM.
    expect(await screen.findByText("reason one")).toBeTruthy();
    expect(screen.getByText("reason two")).toBeTruthy();

    // NEGATIVE: the repo stayed disabled — its toggle still reads Enable, never Disable.
    expect(row().getByRole("button", { name: /^Enable$/ })).toBeTruthy();
    expect(row().queryByRole("button", { name: /^Disable$/ })).toBeNull();

    // A subsequent successful toggle clears the violations from the DOM.
    mockApi.setRepoEnabled.mockResolvedValue({
      repo: { ...REPOS[2], enabled: true },
    });
    fireEvent.click(row().getByRole("button", { name: /^Enable$/ }));
    await waitFor(() => expect(screen.queryByText("reason one")).toBeNull());
    expect(screen.queryByText("reason two")).toBeNull();
  });
});

describe("Repos — GitHub Projects sync (PRD #534 M3)", () => {
  const GH_CONN: ForgeConnection = {
    ...CONN,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
  };
  const GH_REPO: Repo = repo({
    id: "repo-gh",
    path_with_namespace: "vtmocanu/gh",
    connection_id: "conn-gh",
  });

  const linkedStatus = {
    project_number: 42,
    owned_by_uzi: true,
    last_synced_at: "2026-08-20T10:00:00Z",
    last_error: null,
    item_count: 7,
  };

  // Open the Project-sync panel for a GitHub repo row and return its group element.
  async function openSyncPanel(name: string): Promise<HTMLElement> {
    renderPage();
    await screen.findByText(name);
    fireEvent.click(within(rowFor(name)).getByRole("button", { name: /Project sync settings for/ }));
    return screen.getByRole("group", { name: new RegExp(`Project sync for ${name}`) });
  }

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [GH_CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [{ ...GH_REPO }] });
    // Default: not linked (404) so the panel opens on the provision/adopt state.
    mockApi.getProjectSyncStatus.mockRejectedValue(new ApiError(404, "project sync not enabled for this repo"));
    // Default visibility: private. Linked cases override; the not-linked default
    // never reaches this call (loadSyncStatus fetches it only on a 200 status).
    mockApi.getProjectSyncVisibility.mockResolvedValue({ public: false });
  });

  it("renders the Sync badge + Manage on an enabled GitHub repo", async () => {
    renderPage();
    await screen.findByText("vtmocanu/gh");
    const row = within(rowFor("vtmocanu/gh"));
    expect(row.getByText("Sync")).toBeTruthy();
    expect(row.getByRole("button", { name: /Project sync settings for vtmocanu\/gh/ })).toBeTruthy();
  });

  it("renders — (no Manage) for a GitLab connection's repo", async () => {
    // The default gitlab CONN with a gitlab repo: the cell is non-applicable.
    mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [repo({ id: "repo-gl", path_with_namespace: "vtmocanu/gl" })] });
    renderPage();
    await screen.findByText("vtmocanu/gl");
    const row = within(rowFor("vtmocanu/gl"));
    expect(row.queryByRole("button", { name: /Project sync settings/ })).toBeNull();
    expect(row.queryByText("Sync")).toBeNull();
  });

  it("opening Manage fetches status; a 404 shows the Provision + Adopt forms", async () => {
    const panel = await openSyncPanel("vtmocanu/gh");
    await waitFor(() => expect(mockApi.getProjectSyncStatus).toHaveBeenCalledWith("repo-gh"));
    expect(within(panel).getByText("Not linked")).toBeTruthy();
    expect(within(panel).getByRole("button", { name: /Provision/ })).toBeTruthy();
    expect(within(panel).getByRole("button", { name: /Adopt/ })).toBeTruthy();
  });

  it("a 200 status shows the linked readout + Disable, and Disable calls disableProjectSync", async () => {
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedStatus });
    mockApi.disableProjectSync.mockResolvedValue(null);
    const panel = await openSyncPanel("vtmocanu/gh");

    await within(panel).findByText("Linked");
    // The readout surfaces the project number, ownership, and item count.
    expect(within(panel).getByText("#42")).toBeTruthy();
    expect(within(panel).getByText("owned by uzi")).toBeTruthy();

    fireEvent.click(within(panel).getByRole("button", { name: /Disable sync/ }));
    await waitFor(() => expect(mockApi.disableProjectSync).toHaveBeenCalledWith("repo-gh"));
  });

  it("Provision calls provisionProjectSync with the chosen owner_kind + title", async () => {
    mockApi.provisionProjectSync.mockResolvedValue({ status: "provisioned" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");

    fireEvent.change(within(panel).getByLabelText("Provision owner"), { target: { value: "org" } });
    fireEvent.change(within(panel).getByLabelText("Project title"), { target: { value: "My board" } });
    fireEvent.click(within(panel).getByRole("button", { name: /Provision/ }));

    await waitFor(() =>
      expect(mockApi.provisionProjectSync).toHaveBeenCalledWith("repo-gh", {
        owner_kind: "org",
        title: "My board",
      }),
    );
  });

  it("Adopt calls adoptProjectSync with the project number + owner_kind", async () => {
    mockApi.adoptProjectSync.mockResolvedValue({ status: "linked" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");

    fireEvent.change(within(panel).getByLabelText("Adopt owner"), { target: { value: "viewer" } });
    fireEvent.change(within(panel).getByLabelText("Project number"), { target: { value: "7" } });
    fireEvent.click(within(panel).getByRole("button", { name: /Adopt/ }));

    await waitFor(() =>
      expect(mockApi.adoptProjectSync).toHaveBeenCalledWith("repo-gh", {
        project_number: 7,
        owner_kind: "viewer",
      }),
    );
  });

  it("a 409 from provision surfaces the 'ask an admin' message", async () => {
    mockApi.provisionProjectSync.mockRejectedValue(new ApiError(409, "project sync disabled"));
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");

    fireEvent.click(within(panel).getByRole("button", { name: /Provision/ }));
    expect(
      await within(panel).findByText(/turned off for this instance — ask an admin to enable it/i),
    ).toBeTruthy();
  });
});

describe("Repos — skipped columns + Resync (PRD #576 M3)", () => {
  const GH_CONN: ForgeConnection = {
    ...CONN,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
  };
  const GH_REPO: Repo = repo({
    id: "repo-gh",
    path_with_namespace: "vtmocanu/gh",
    connection_id: "conn-gh",
  });

  const linkedWithUnmatched = {
    project_number: 42,
    owned_by_uzi: false,
    last_synced_at: "2026-08-20T10:00:00Z",
    last_error: null,
    item_count: 7,
    unmatched_columns: ["Planned", "bug"],
  };

  async function openSyncPanel(name: string): Promise<HTMLElement> {
    renderPage();
    await screen.findByText(name);
    fireEvent.click(within(rowFor(name)).getByRole("button", { name: /Project sync settings for/ }));
    return screen.getByRole("group", { name: new RegExp(`Project sync for ${name}`) });
  }

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [GH_CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [{ ...GH_REPO }] });
    mockApi.getProjectSyncVisibility.mockResolvedValue({ public: false });
  });

  it("lists the skipped columns and renders a Resync button", async () => {
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedWithUnmatched });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Linked");

    // Both skipped column names appear in the advisory copy.
    expect(within(panel).getByText(/Planned, bug/)).toBeTruthy();
    expect(
      within(panel).getByText(/no matching Status option and won.?t\s+sync/i),
    ).toBeTruthy();
    expect(within(panel).getByRole("button", { name: /Resync/ })).toBeTruthy();
  });

  it("clicking Resync calls resyncProjectSync with the repo id and reloads status", async () => {
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedWithUnmatched });
    mockApi.resyncProjectSync.mockResolvedValue({ status: "resynced" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Linked");

    mockApi.getProjectSyncStatus.mockClear();
    fireEvent.click(within(panel).getByRole("button", { name: /Resync/ }));

    await waitFor(() => expect(mockApi.resyncProjectSync).toHaveBeenCalledWith("repo-gh"));
    // Status is reloaded after the resync so the readout reflects the result.
    await waitFor(() => expect(mockApi.getProjectSyncStatus).toHaveBeenCalledWith("repo-gh"));
  });

  it("renders the auto-create button with the two-status-field tradeoff copy (PRD #576 M6)", async () => {
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedWithUnmatched });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Linked");

    // The tradeoff is documented near the button.
    expect(within(panel).getByText(/two status-like fields/i)).toBeTruthy();
    expect(
      within(panel).getByRole("button", { name: /Auto-create the missing columns/ }),
    ).toBeTruthy();
  });

  it("clicking Auto-create calls autocreateProjectSyncColumns and reloads status (PRD #576 M6)", async () => {
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedWithUnmatched });
    mockApi.autocreateProjectSyncColumns.mockResolvedValue({ status: "columns_created" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Linked");

    mockApi.getProjectSyncStatus.mockClear();
    fireEvent.click(
      within(panel).getByRole("button", { name: /Auto-create the missing columns/ }),
    );

    await waitFor(() =>
      expect(mockApi.autocreateProjectSyncColumns).toHaveBeenCalledWith("repo-gh"),
    );
    // Status is reloaded so the (now empty) skipped-columns list reflects the result.
    await waitFor(() => expect(mockApi.getProjectSyncStatus).toHaveBeenCalledWith("repo-gh"));
  });
});

describe("Repos — Adopt-first sync (PRD #576 M1)", () => {
  const GH_CONN: ForgeConnection = {
    ...CONN,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
  };
  const GH_REPO: Repo = repo({
    id: "repo-gh",
    path_with_namespace: "vtmocanu/gh",
    connection_id: "conn-gh",
  });

  async function openSyncPanel(name: string): Promise<HTMLElement> {
    renderPage();
    await screen.findByText(name);
    fireEvent.click(within(rowFor(name)).getByRole("button", { name: /Project sync settings for/ }));
    return screen.getByRole("group", { name: new RegExp(`Project sync for ${name}`) });
  }

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [GH_CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [{ ...GH_REPO }] });
    // Not linked (404) so the panel opens on the provision/adopt state.
    mockApi.getProjectSyncStatus.mockRejectedValue(new ApiError(404, "project sync not enabled for this repo"));
    mockApi.getProjectSyncVisibility.mockResolvedValue({ public: false });
  });

  it("fetches owner type on open and renders the Adopt-first explainer, Adopt before Provision", async () => {
    mockApi.getProjectSyncOwnerType.mockResolvedValue({ owner_type: "Organization" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");
    await waitFor(() => expect(mockApi.getProjectSyncOwnerType).toHaveBeenCalledWith("repo-gh"));

    // The terse one-line explainer renders.
    expect(
      within(panel).getByText(/Adopt a Project you already created \(recommended\)/i),
    ).toBeTruthy();

    // Adopt is presented first (foreground): its heading precedes Provision's in DOM order.
    const adoptHeading = within(panel).getByText("Adopt an existing project");
    const provisionHeading = within(panel).getByText("Create a new project");
    expect(
      adoptHeading.compareDocumentPosition(provisionHeading) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("for a user-owned repo, Provision is disabled with its reason rendered", async () => {
    mockApi.getProjectSyncOwnerType.mockResolvedValue({ owner_type: "User" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");
    await waitFor(() => expect(mockApi.getProjectSyncOwnerType).toHaveBeenCalledWith("repo-gh"));

    // Provision is disabled...
    await waitFor(() =>
      expect(
        (within(panel).getByRole("button", { name: /Provision/ }) as HTMLButtonElement).disabled,
      ).toBe(true),
    );
    // ...and its reason text is in the DOM.
    expect(
      within(panel).getByText(/can.?t own a Project under a personal account/i),
    ).toBeTruthy();
    // Adopt stays available.
    expect(
      (within(panel).getByRole("button", { name: /Adopt/ }) as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  it("for an org-owned repo, Provision is enabled", async () => {
    mockApi.getProjectSyncOwnerType.mockResolvedValue({ owner_type: "Organization" });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");
    await waitFor(() => expect(mockApi.getProjectSyncOwnerType).toHaveBeenCalledWith("repo-gh"));

    expect(
      (within(panel).getByRole("button", { name: /Provision/ }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(within(panel).queryByText(/personal account/i)).toBeNull();
  });

  it("on owner-type fetch rejection, both forms are enabled (fallback)", async () => {
    mockApi.getProjectSyncOwnerType.mockRejectedValue(new ApiError(500, "boom"));
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");
    await waitFor(() => expect(mockApi.getProjectSyncOwnerType).toHaveBeenCalledWith("repo-gh"));

    expect(
      (within(panel).getByRole("button", { name: /Provision/ }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(
      (within(panel).getByRole("button", { name: /Adopt/ }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(within(panel).queryByText(/personal account/i)).toBeNull();
  });
});

describe("Repos — sync-health badge (PRD #576 M2)", () => {
  const GH_CONN: ForgeConnection = {
    ...CONN,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
  };

  function ghRepo(over: Partial<Repo>): Repo {
    return repo({
      id: "repo-gh",
      path_with_namespace: "vtmocanu/gh",
      connection_id: "conn-gh",
      ...over,
    });
  }

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [GH_CONN] });
    mockApi.getProjectSyncStatus.mockRejectedValue(new ApiError(404, "project sync not enabled for this repo"));
    mockApi.getProjectSyncVisibility.mockResolvedValue({ public: false });
  });

  // The Badge renders a single <span> whose className carries the tone hue class
  // (text-ok / text-danger / text-neutral-fg). Assert on that rendered output, not
  // component internals — the same technique the Setup-chip test uses.
  function syncBadge(name: string): HTMLElement {
    return within(rowFor(name)).getByText("Sync");
  }

  it("linked && healthy → ok (green) tone", async () => {
    mockApi.listProjects.mockResolvedValue({
      repos: [ghRepo({ github_project_sync: { linked: true, healthy: true, last_synced_at: "2026-08-20T10:00:00Z" } })],
    });
    renderPage();
    await screen.findByText("vtmocanu/gh");
    const badge = syncBadge("vtmocanu/gh");
    expect(badge.className).toContain("text-ok");
    // Paired negatives so the tone assertion is not vacuous.
    expect(badge.className).not.toContain("text-danger");
    expect(badge.className).not.toContain("text-neutral-fg");
  });

  it("linked && !healthy (last_error set) → danger tone, with the error surfaced", async () => {
    mockApi.listProjects.mockResolvedValue({
      repos: [
        ghRepo({
          github_project_sync: { linked: true, healthy: false, last_error: "provision failed: owner mismatch" },
        }),
      ],
    });
    renderPage();
    await screen.findByText("vtmocanu/gh");
    const badge = syncBadge("vtmocanu/gh");
    expect(badge.className).toContain("text-danger");
    expect(badge.className).not.toContain("text-ok");
    // The error text is surfaced for the user (title/aria), not swallowed.
    expect(badge.getAttribute("title")).toContain("provision failed: owner mismatch");
    expect(badge.getAttribute("aria-label")).toContain("provision failed: owner mismatch");
  });

  it("not linked (field absent) → neutral tone (current look)", async () => {
    mockApi.listProjects.mockResolvedValue({ repos: [ghRepo({})] });
    renderPage();
    await screen.findByText("vtmocanu/gh");
    const badge = syncBadge("vtmocanu/gh");
    expect(badge.className).toContain("text-neutral-fg");
    expect(badge.className).not.toContain("text-ok");
    expect(badge.className).not.toContain("text-danger");
  });

  it("linked but explicitly null field → neutral tone", async () => {
    mockApi.listProjects.mockResolvedValue({ repos: [ghRepo({ github_project_sync: null })] });
    renderPage();
    await screen.findByText("vtmocanu/gh");
    const badge = syncBadge("vtmocanu/gh");
    expect(badge.className).toContain("text-neutral-fg");
  });
});

describe("Repos — Board access (PRD #557 M4)", () => {
  const GH_CONN: ForgeConnection = {
    ...CONN,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
  };
  const GH_REPO: Repo = repo({
    id: "repo-gh",
    path_with_namespace: "vtmocanu/gh",
    connection_id: "conn-gh",
  });
  const linkedStatus = {
    project_number: 42,
    owned_by_uzi: true,
    last_synced_at: "2026-08-20T10:00:00Z",
    last_error: null,
    item_count: 7,
  };

  async function openSyncPanel(name: string): Promise<HTMLElement> {
    renderPage();
    await screen.findByText(name);
    fireEvent.click(within(rowFor(name)).getByRole("button", { name: /Project sync settings for/ }));
    return screen.getByRole("group", { name: new RegExp(`Project sync for ${name}`) });
  }

  beforeEach(() => {
    mockApi.listConnections.mockResolvedValue({ connections: [GH_CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [{ ...GH_REPO }] });
    mockApi.getProjectSyncStatus.mockResolvedValue({ ...linkedStatus });
    mockApi.getProjectSyncVisibility.mockResolvedValue({ public: false });
  });

  it("shows the Board-access block for a linked GitHub repo, with the write-only note", async () => {
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");
    // The visibility toggle is present…
    expect(within(panel).getByRole("switch", { name: "Board visibility" })).toBeTruthy();
    // …and the honesty note renders (POSITIVE assertion — not the absence of a list).
    expect(
      within(panel).getByText(/GitHub does not expose a board.s current sharing list/i),
    ).toBeTruthy();
    await waitFor(() => expect(mockApi.getProjectSyncVisibility).toHaveBeenCalledWith("repo-gh"));
  });

  it("is absent for a not-linked repo (provision/adopt forms show instead)", async () => {
    mockApi.getProjectSyncStatus.mockRejectedValue(
      new ApiError(404, "project sync not enabled for this repo"),
    );
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Not linked");
    expect(within(panel).queryByText("Board access")).toBeNull();
    expect(within(panel).queryByRole("switch", { name: "Board visibility" })).toBeNull();
    // Sanity: the not-linked entry state is really rendered.
    expect(within(panel).getByRole("button", { name: /Provision/ })).toBeTruthy();
    expect(within(panel).getByRole("button", { name: /Adopt/ })).toBeTruthy();
  });

  it("is absent for a GitLab connection's repo (no Manage trigger at all)", async () => {
    mockApi.listConnections.mockResolvedValue({ connections: [CONN] });
    mockApi.listProjects.mockResolvedValue({ repos: [repo({ id: "repo-gl", path_with_namespace: "vtmocanu/gl" })] });
    renderPage();
    await screen.findByText("vtmocanu/gl");
    const row = within(rowFor("vtmocanu/gl"));
    // No Project-sync Manage trigger → no path to a Board-access control.
    expect(row.queryByRole("button", { name: /Project sync settings/ })).toBeNull();
    expect(screen.queryByText("Board access")).toBeNull();
    expect(screen.queryByRole("switch", { name: "Board visibility" })).toBeNull();
  });

  it("the visibility toggle reflects the fetched value and PUTs on change", async () => {
    mockApi.setProjectSyncVisibility.mockResolvedValue({ public: true });
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    const toggle = within(panel).getByRole("switch", { name: "Board visibility" });
    // Fetched value is private → off, and no internet-visible caption yet.
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("false"));
    expect(within(panel).queryByText(/visible to anyone on the internet/i)).toBeNull();

    fireEvent.click(toggle);
    await waitFor(() =>
      expect(mockApi.setProjectSyncVisibility).toHaveBeenCalledWith("repo-gh", true),
    );
    // The response's public:true drives the caption and the switch state.
    expect(await within(panel).findByText(/visible to anyone on the internet/i)).toBeTruthy();
    expect(within(panel).getByRole("switch", { name: "Board visibility" }).getAttribute("aria-checked")).toBe("true");
  });

  it("Share calls the collaborators endpoint and shows a success confirmation", async () => {
    mockApi.shareProjectSync.mockResolvedValue(null);
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    fireEvent.change(within(panel).getByLabelText("Share with a GitHub user (Reader)"), {
      target: { value: "octocat" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: /Share \(Reader\)/ }));

    await waitFor(() => expect(mockApi.shareProjectSync).toHaveBeenCalledWith("repo-gh", "octocat"));
    expect(await within(panel).findByText(/Shared with octocat as Reader/i)).toBeTruthy();
    // The just-granted username gets a Revoke affordance.
    expect(within(panel).getByRole("button", { name: /Revoke/ })).toBeTruthy();
  });

  it("Revoke calls the unshare endpoint and drops the entry from the session list", async () => {
    mockApi.shareProjectSync.mockResolvedValue(null);
    mockApi.unshareProjectSync.mockResolvedValue(null);
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    fireEvent.change(within(panel).getByLabelText("Share with a GitHub user (Reader)"), {
      target: { value: "octocat" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: /Share \(Reader\)/ }));
    const revoke = await within(panel).findByRole("button", { name: /Revoke/ });

    fireEvent.click(revoke);
    await waitFor(() => expect(mockApi.unshareProjectSync).toHaveBeenCalledWith("repo-gh", "octocat"));
    // The revoked username leaves the just-granted list.
    await waitFor(() =>
      expect(within(panel).queryByRole("button", { name: /Revoke/ })).toBeNull(),
    );
  });

  it("a 422 bad username surfaces the bad-username copy inline, not a crash", async () => {
    mockApi.shareProjectSync.mockRejectedValue(new ApiError(422, "no github user with that username"));
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    fireEvent.change(within(panel).getByLabelText("Share with a GitHub user (Reader)"), {
      target: { value: "nouser" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: /Share \(Reader\)/ }));

    expect(await within(panel).findByText(/No GitHub user named "nouser"/i)).toBeTruthy();
    // No success confirmation, and no Revoke affordance for a failed grant.
    expect(within(panel).queryByText(/Shared with/i)).toBeNull();
    expect(within(panel).queryByRole("button", { name: /Revoke/ })).toBeNull();
  });

  it("a 409 instance-disabled surfaces the 'ask an admin' copy inline", async () => {
    mockApi.setProjectSyncVisibility.mockRejectedValue(new ApiError(409, "project sync disabled"));
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    fireEvent.click(within(panel).getByRole("switch", { name: "Board visibility" }));
    expect(
      await within(panel).findByText(/turned off for this instance — ask an admin to enable it/i),
    ).toBeTruthy();
    // The toggle stayed off — the failed PUT never flipped it.
    expect(within(panel).getByRole("switch", { name: "Board visibility" }).getAttribute("aria-checked")).toBe("false");
  });

  it("clears the just-granted session list when the board is re-linked in place (disable re-enters loadSyncStatus, bypassing the panel-open reset)", async () => {
    mockApi.shareProjectSync.mockResolvedValue(null);
    mockApi.disableProjectSync.mockResolvedValue(null);
    const panel = await openSyncPanel("vtmocanu/gh");
    await within(panel).findByText("Board access");

    // Grant octocat → a session-scoped Revoke affordance appears.
    fireEvent.change(within(panel).getByLabelText("Share with a GitHub user (Reader)"), {
      target: { value: "octocat" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: /Share \(Reader\)/ }));
    await within(panel).findByRole("button", { name: /Revoke/ });

    // Disable re-enters loadSyncStatus WITHOUT the panel-open reset effect. With the
    // status mock still linked (a board re-linked in place), the readout returns — and
    // the stale "granted this session" entry must NOT survive onto it, else its Revoke
    // would fire against a board octocat was never granted on (PRD #557 review finding).
    fireEvent.click(within(panel).getByRole("button", { name: /Disable sync/ }));
    await waitFor(() => expect(mockApi.disableProjectSync).toHaveBeenCalledWith("repo-gh"));
    await waitFor(() => expect(within(panel).queryByRole("button", { name: /Revoke/ })).toBeNull());
  });
});

describe("Repos — Setup chip (PRD #361 M4)", () => {
  it("renders only for enabled repos and reflects the flags (docker_blocked → info)", async () => {
    // A docker_blocked enabled repo (escalates to info) plus the disabled fixture row.
    mockApi.listProjects.mockResolvedValue({
      repos: [
        repo({ id: "repo-blocked", path_with_namespace: "vtmocanu/blocked", docker_blocked: true }),
        repo({ id: "repo-off", path_with_namespace: "example/website", enabled: false }),
      ],
    });
    renderPage();
    await screen.findByText("vtmocanu/blocked");

    // PRESENT on the enabled row, ABSENT on the disabled one.
    const enabledChip = within(rowFor("vtmocanu/blocked")).getByRole("button", {
      name: /^(Setup|Ready)$/,
    });
    expect(enabledChip).toBeTruthy();
    expect(
      within(rowFor("example/website")).queryByRole("button", { name: /^(Setup|Ready)$/ }),
    ).toBeNull();

    // Reflects the flag: a blocked repo's chip carries the info tone (positive)…
    const badge = enabledChip.querySelector("span");
    expect(badge?.className).toContain("text-info");
    // …and never a red/amber warning (paired negative, so it is not vacuous).
    expect(badge?.className).not.toContain("text-danger");
    expect(badge?.className).not.toContain("text-warn");
  });
});
