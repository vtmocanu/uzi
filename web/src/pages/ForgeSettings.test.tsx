// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeSettings } from "./ForgeSettings";
import { api, ApiError, type ForgeConnection, type PrivilegeReport } from "../lib/api";
import { mockForgeConfigMultiForge, mockForgeConfigSyncForges } from "../mocks/data";

// Only the api object is swapped; ApiError and the types stay real so the page's
// `instanceof ApiError` checks match what the mocked methods throw. The page loads
// forgeConfig + listConnections and drives both the identity-mapping (PRD #19) and
// privilege (PRD #5) endpoints.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
      createConnection: vi.fn(),
      verifyConnection: vi.fn(),
      updateConnection: vi.fn(),
      privilegeCheck: vi.fn(),
      deleteConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function conn(over: Partial<ForgeConnection> = {}): ForgeConnection {
  return {
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
    ...over,
  };
}

function violationReport(): PrivilegeReport {
  return {
    checked_at: "2026-07-05T12:00:00Z",
    status: "violations",
    token: { scopes: ["api"], active: true, violations: [], warnings: [] },
    repos: [
      {
        repo_id: "repo-atlas",
        path: "vtmocanu/atlas-api",
        role: "write",
        member: true,
        findings: [
          {
            code: "default_branch_unprotected",
            severity: "block",
            message: 'default branch "main" is not protected',
          },
        ],
      },
    ],
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ForgeSettings />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.forgeConfig.mockResolvedValue({
    allowed_base_urls: ["https://gitlab.example.com"],
    forge_types: ["gitlab"],
  });
  mockApi.listConnections.mockResolvedValue({ connections: [conn()] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const usernameField = () => screen.getByLabelText(/Your username on/i) as HTMLInputElement;

describe("ForgeSettings — forge identity mapping (PRD #19 M3)", () => {
  it("shows the identity section seeded from the stored username", async () => {
    mockApi.listConnections.mockResolvedValue({ connections: [conn({ human_username: "vlad" })] });
    renderPage();
    await waitFor(() => expect(usernameField().value).toBe("vlad"));
    // Explains the autopilot attribution purpose.
    expect(screen.getByText(/attribute an/i)).toBeTruthy();
  });

  it("saves an edited username", async () => {
    mockApi.updateConnection.mockResolvedValue({ connection: conn({ human_username: "vlad" }) });
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "vlad" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    await waitFor(() => expect(mockApi.updateConnection).toHaveBeenCalledWith("conn-1", "vlad"));
    expect(await screen.findByText(/Saved your forge username: vlad/i)).toBeTruthy();
  });

  it("surfaces the verified-or-warned warning while still saving", async () => {
    mockApi.updateConnection.mockResolvedValue({
      connection: conn({ human_username: "ghost" }),
      warning: "Saved, but no forge account with this username was found — double-check it.",
    });
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "ghost" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    // The warning Alert carries the not-found message.
    expect(await screen.findByText(/no forge account with this username/i)).toBeTruthy();
  });

  it("hard-rejects a username already mapped by another user (409)", async () => {
    mockApi.updateConnection.mockRejectedValue(
      new ApiError(409, "that forge username is already mapped by another user on this host"),
    );
    renderPage();
    await screen.findByLabelText(/Your username on/i);
    fireEvent.change(usernameField(), { target: { value: "taken" } });
    fireEvent.click(screen.getByRole("button", { name: /save username/i }));

    expect(await screen.findByText(/already mapped by another user/i)).toBeTruthy();
  });
});

describe("ForgeSettings — always-visible bot-setup guide link (PRD #57 M2)", () => {
  it("renders the happy-path connect-card bot-setup link on the default gitlab forge", async () => {
    renderPage();
    await screen.findByText("unchecked"); // page loaded, no over-privilege card shown
    const docLink = screen.getByRole("link", { name: "bot setup" });
    expect(docLink.getAttribute("href")).toBe("/docs/gitlab-bot-setup");
  });
});

describe("ForgeSettings privilege surfacing", () => {
  it("renders an unchecked badge for a never-checked connection", async () => {
    renderPage();
    expect(await screen.findByText("unchecked")).toBeTruthy();
  });

  it("renders least-privilege ✓ and expands to the findings", async () => {
    mockApi.listConnections.mockResolvedValue({
      connections: [
        conn({
          privilege_status: "ok",
          privilege_checked_at: "2026-07-05T12:00:00Z",
          privilege_report: {
            checked_at: "2026-07-05T12:00:00Z",
            status: "ok",
            token: { scopes: ["api"], active: true, violations: [], warnings: [] },
            repos: [{ repo_id: "r1", path: "vtmocanu/uzi", role: "write", member: true, findings: [] }],
          },
        }),
      ],
    });
    renderPage();
    const badge = await screen.findByText("least-privilege ✓");
    fireEvent.click(badge);
    // The expanded panel names the repo and reports it clean.
    expect(await screen.findByText("vtmocanu/uzi")).toBeTruthy();
    expect(screen.getByText(/Everything is least-privilege/)).toBeTruthy();
  });

  it("shows a violation count and the specific finding when expanded", async () => {
    mockApi.listConnections.mockResolvedValue({
      connections: [
        conn({ privilege_status: "violations", privilege_checked_at: "2026-07-05T12:00:00Z", privilege_report: violationReport() }),
      ],
    });
    renderPage();
    const badge = await screen.findByText("1 violation");
    fireEvent.click(badge);
    expect(await screen.findByText(/default branch "main" is not protected/)).toBeTruthy();
  });

  it("runs a privilege check and updates the badge", async () => {
    renderPage();
    await screen.findByText("unchecked");
    mockApi.privilegeCheck.mockResolvedValue({
      report: {
        checked_at: "2026-07-05T12:30:00Z",
        status: "ok",
        token: { scopes: ["api"], active: true, violations: [], warnings: [] },
        repos: [],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: /Check privileges/ }));
    expect(mockApi.privilegeCheck).toHaveBeenCalledWith("conn-1");
    expect(await screen.findByText("least-privilege ✓")).toBeTruthy();
  });

  it("renders 422 violations with a doc link when a save is rejected", async () => {
    mockApi.createConnection.mockRejectedValue(
      new ApiError(422, "the bot token is over-privileged and was not saved", {
        violations: ["token scopes [api sudo] exceed the required [api]"],
      }),
    );
    renderPage();
    await screen.findByText("unchecked"); // page loaded
    const tokenInput = document.querySelector('input[type="password"]') as HTMLInputElement;
    fireEvent.change(tokenInput, { target: { value: "glpat-oops" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    expect(await screen.findByText(/token scopes \[api sudo\] exceed the required \[api\]/)).toBeTruthy();
    const docLink = screen.getByRole("link", { name: /bot setup guide/ });
    expect(docLink.getAttribute("href")).toBe("/docs/gitlab-bot-setup");
  });

  it("points the 422 doc link at the FORGEJO guide when connecting a forgejo bot (M6b)", async () => {
    // Two-forge config → the picker is visible; choosing forgejo must route the
    // over-privilege guide to the forgejo doc, not always GitLab's.
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    mockApi.createConnection.mockRejectedValue(
      new ApiError(422, "the bot token is over-privileged and was not saved", {
        violations: ["token scopes [all] exceed the required [write:repository write:issue read:user]"],
      }),
    );
    renderPage();
    const picker = (await screen.findByLabelText("Forge type")) as HTMLSelectElement;
    fireEvent.change(picker, { target: { value: "forgejo" } });
    const tokenInput = document.querySelector('input[type="password"]') as HTMLInputElement;
    fireEvent.change(tokenInput, { target: { value: "forgejo-oops" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    await screen.findByText(/exceed the required/);
    const docLink = screen.getByRole("link", { name: /bot setup guide/ });
    expect(docLink.getAttribute("href")).toBe("/docs/forgejo-bot-setup");
  });
});

describe("ForgeSettings — forge-type picker (PRD #65 D11, lands dark)", () => {
  const tokenInput = () => document.querySelector('input[type="password"]') as HTMLInputElement;

  it("hides the picker and sends forge_type gitlab while only one type is advertised (dark landing)", async () => {
    // Default beforeEach config advertises forge_types: ["gitlab"].
    mockApi.createConnection.mockResolvedValue({ connection: conn() });
    renderPage();
    await screen.findByText("unchecked"); // page loaded

    // No forge-type picker while a single type is advertised — the form is unchanged.
    expect(screen.queryByLabelText("Forge type")).toBeNull();

    fireEvent.change(tokenInput(), { target: { value: "glpat-ok" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    // The connect call still carries forge_type "gitlab" — byte-identical to before.
    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith("https://gitlab.example.com", "glpat-ok", "gitlab"),
    );
  });

  it("shows the picker when >1 type is advertised and defaults the selection to gitlab", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    mockApi.createConnection.mockResolvedValue({ connection: conn() });
    renderPage();
    await screen.findByText("unchecked");

    const picker = screen.getByLabelText("Forge type") as HTMLSelectElement;
    expect(picker).toBeTruthy();
    // Options render the friendly platform names, default selected is the first (gitlab).
    expect(picker.value).toBe("gitlab");
    expect(screen.getByRole("option", { name: "GitLab" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Forgejo" })).toBeTruthy();

    fireEvent.change(tokenInput(), { target: { value: "glpat-ok" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    // Untouched selection still sends gitlab.
    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith("https://gitlab.example.com", "glpat-ok", "gitlab"),
    );
  });

  it("threads the chosen forge type through createConnection", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    mockApi.createConnection.mockResolvedValue({ connection: conn({ forge_type: "forgejo" }) });
    renderPage();
    const picker = (await screen.findByLabelText("Forge type")) as HTMLSelectElement;

    fireEvent.change(picker, { target: { value: "forgejo" } });
    fireEvent.change(tokenInput(), { target: { value: "tok-forgejo" } });
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);

    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith(
        "https://gitlab.example.com",
        "tok-forgejo",
        "forgejo",
      ),
    );
  });
});

describe("ForgeSettings — base-URL ⇄ forge-type sync (PRD #337)", () => {
  const baseUrlSelect = () => screen.getByLabelText("Forge base URL") as HTMLSelectElement;
  const typeSelect = () => screen.getByLabelText("Forge type") as HTMLSelectElement;

  it("URL → type: choosing a recognized github.com URL switches the type to github and the hints follow", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigSyncForges);
    renderPage();
    await screen.findByLabelText("Forge type");
    // Landing pair is the first URL / gitlab (sync is change-only, D5).
    expect(baseUrlSelect().value).toBe("https://gitlab.example.com");
    expect(typeSelect().value).toBe("gitlab");

    fireEvent.change(baseUrlSelect(), { target: { value: "https://github.com" } });

    await waitFor(() => expect(typeSelect().value).toBe("github"));
    // connectHints copy tracks the resulting github type.
    expect(screen.getByText("Bot personal access token (scope: repo)")).toBeTruthy();
    expect(screen.getByPlaceholderText("ghp_…")).toBeTruthy();
  });

  it("type → URL: choosing the github type moves the base URL to github.com", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigSyncForges);
    renderPage();
    await screen.findByLabelText("Forge type");
    expect(baseUrlSelect().value).toBe("https://gitlab.example.com");

    fireEvent.change(typeSelect(), { target: { value: "github" } });

    await waitFor(() => expect(baseUrlSelect().value).toBe("https://github.com"));
  });

  it("forgejo round-trip: URL→type sets forgejo, and type→URL moves the URL to the forgejo host", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigSyncForges);
    renderPage();
    await screen.findByLabelText("Forge type");

    // URL → type
    fireEvent.change(baseUrlSelect(), { target: { value: "https://forgejo.example.com" } });
    await waitFor(() => expect(typeSelect().value).toBe("forgejo"));

    // Now from a gitlab URL, choosing the forgejo type moves the URL to the forgejo host.
    fireEvent.change(baseUrlSelect(), { target: { value: "https://gitlab.example.com" } });
    await waitFor(() => expect(typeSelect().value).toBe("gitlab"));
    fireEvent.change(typeSelect(), { target: { value: "forgejo" } });
    await waitFor(() => expect(baseUrlSelect().value).toBe("https://forgejo.example.com"));
  });

  it("null-target keep-URL (D8): choosing a type with no recognized allowlist URL keeps the current URL", async () => {
    // mockForgeConfigMultiForge advertises forgejo, but its only non-gitlab host is
    // the unrecognized forge.example.com (infers to null) — so there is no recognized
    // forgejo URL. The current gitlab URL must be kept, not blanked or mis-set.
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    renderPage();
    await screen.findByLabelText("Forge type");
    expect(baseUrlSelect().value).toBe("https://gitlab.example.com");

    fireEvent.change(typeSelect(), { target: { value: "forgejo" } });

    await waitFor(() => expect(typeSelect().value).toBe("forgejo"));
    // URL unchanged — no recognized forgejo target in the allowlist.
    expect(baseUrlSelect().value).toBe("https://gitlab.example.com");
  });

  it("unrecognized host opts out of sync in both directions", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigSyncForges);
    renderPage();
    await screen.findByLabelText("Forge type");

    // URL → type: an unrecognized/self-hosted host leaves the type on the manual pick.
    fireEvent.change(baseUrlSelect(), { target: { value: "https://git.example.com" } });
    await waitFor(() => expect(baseUrlSelect().value).toBe("https://git.example.com"));
    expect(typeSelect().value).toBe("gitlab");

    // type → URL: with the current host unrecognized, changing the type keeps the URL.
    fireEvent.change(typeSelect(), { target: { value: "forgejo" } });
    await waitFor(() => expect(typeSelect().value).toBe("forgejo"));
    expect(baseUrlSelect().value).toBe("https://git.example.com");
  });
});

describe("ForgeSettings — PAT reveal toggle (PRD #337)", () => {
  const patInput = () => document.querySelector("#forge-bot-pat") as HTMLInputElement;

  it("is masked on mount with a 'Show token' button (aria-pressed false)", async () => {
    renderPage();
    await screen.findByText("unchecked"); // page loaded
    expect(patInput().getAttribute("type")).toBe("password");
    const btn = screen.getByRole("button", { name: "Show token" });
    expect(btn.getAttribute("aria-pressed")).toBe("false");
    // No "Hide token" button while masked.
    expect(screen.queryByRole("button", { name: "Hide token" })).toBeNull();
  });

  it("reveal flips the input type and aria, and hiding flips them back", async () => {
    renderPage();
    await screen.findByText("unchecked");
    expect(patInput().getAttribute("type")).toBe("password");

    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    expect(patInput().getAttribute("type")).toBe("text");
    const hideBtn = screen.getByRole("button", { name: "Hide token" });
    expect(hideBtn.getAttribute("aria-pressed")).toBe("true");

    fireEvent.click(hideBtn);
    expect(patInput().getAttribute("type")).toBe("password");
    const showBtn = screen.getByRole("button", { name: "Show token" });
    expect(showBtn.getAttribute("aria-pressed")).toBe("false");
  });

  it("re-masks after a successful connect clears the token (D7 leak guard)", async () => {
    mockApi.createConnection.mockResolvedValue({ connection: conn() });
    renderPage();
    await screen.findByText("unchecked");

    // Reveal the field, then type a token into it.
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    expect(patInput().getAttribute("type")).toBe("text");
    fireEvent.change(patInput(), { target: { value: "glpat-secret" } });
    expect(patInput().value).toBe("glpat-secret");

    // Submit → connect resolves → setToken("") runs → the field re-masks.
    fireEvent.submit(document.querySelector("form") as HTMLFormElement);
    await waitFor(() =>
      expect(mockApi.createConnection).toHaveBeenCalledWith("https://gitlab.example.com", "glpat-secret", "gitlab"),
    );
    await waitFor(() => expect(patInput().getAttribute("type")).toBe("password"));
    expect(screen.getByRole("button", { name: "Show token" }).getAttribute("aria-pressed")).toBe("false");
    expect(screen.queryByRole("button", { name: "Hide token" })).toBeNull();
  });
});

describe("ForgeSettings — connect-form token hints follow the forge (M6b, D6b)", () => {
  it("shows GitLab's api scope + Developer role + glpat placeholder by default", async () => {
    renderPage();
    await screen.findByText("unchecked");
    // Byte-identical to pre-M6b — GitLab dark landing.
    expect(screen.getByText("Bot personal access token (scope: api)")).toBeTruthy();
    expect(screen.getByPlaceholderText("glpat-…")).toBeTruthy();
    expect(screen.getByText(/add it as Developer to the projects/)).toBeTruthy();
  });

  it("switches to Forgejo's D6b scopes + write role + neutral placeholder when forgejo is picked", async () => {
    mockApi.forgeConfig.mockResolvedValue(mockForgeConfigMultiForge);
    renderPage();
    const picker = (await screen.findByLabelText("Forge type")) as HTMLSelectElement;
    fireEvent.change(picker, { target: { value: "forgejo" } });

    // The exact D6b required set (must match what VerifyToken accepts) and the write role.
    expect(
      screen.getByText("Bot personal access token (scopes: write:repository, write:issue, read:user)"),
    ).toBeTruthy();
    expect(screen.getByPlaceholderText("your Forgejo token")).toBeTruthy();
    expect(screen.getByText(/add it at the write role to the projects/)).toBeTruthy();
    // GitLab's hints are gone — no wrong-forge instructions remain.
    expect(screen.queryByText("Bot personal access token (scope: api)")).toBeNull();
    expect(screen.queryByPlaceholderText("glpat-…")).toBeNull();
  });
});
