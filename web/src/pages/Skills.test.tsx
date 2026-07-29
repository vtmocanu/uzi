// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Skills } from "./Skills";
import { api, type Skill, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSkills: vi.fn(),
      createSkill: vi.fn(),
      updateSkill: vi.fn(),
      deleteSkill: vi.fn(),
      resetSkill: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const mockUseAuth = vi.mocked(useAuth);

const ADMIN: User = {
  id: "u-admin",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};
const MEMBER: User = { ...ADMIN, id: "u-mira", email: "mira@uzi.local", is_admin: false };

function skill(over: Partial<Skill> & Pick<Skill, "id" | "name" | "scope">): Skill {
  return {
    description: `desc for ${over.name}`,
    body: `# ${over.name}\n\nbody`,
    user_id: null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const SKILLS: Skill[] = [
  skill({ id: "s-builtin", name: "ci-cd-norms", scope: "builtin" }),
  skill({ id: "s-global", name: "argocd-debugging", scope: "global" }),
  skill({ id: "s-mine", name: "qdrant-kb", scope: "user", user_id: "u-mira" }),
];

function setAuth(u: User) {
  mockUseAuth.mockReturnValue({
    user: u,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled: false,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  });
}

function renderPage() {
  return render(
    <MemoryRouter>
      <Skills />
    </MemoryRouter>,
  );
}

function rowFor(name: string): HTMLElement {
  const cell = screen.getByText(name);
  const tr = cell.closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

beforeEach(() => {
  mockApi.listSkills.mockResolvedValue({ skills: SKILLS.map((s) => ({ ...s })) });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Skills page — authz-conditional rendering", () => {
  it("admin: builtin rows show Edit + Reset and never Delete", async () => {
    setAuth(ADMIN);
    renderPage();
    await screen.findByText("ci-cd-norms");
    const row = within(rowFor("ci-cd-norms"));
    expect(row.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(row.getByRole("button", { name: "Reset" })).toBeTruthy();
    expect(row.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("admin: global rows show Edit + Delete", async () => {
    setAuth(ADMIN);
    renderPage();
    await screen.findByText("argocd-debugging");
    const row = within(rowFor("argocd-debugging"));
    expect(row.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(row.getByRole("button", { name: "Delete" })).toBeTruthy();
    expect(row.queryByRole("button", { name: "Reset" })).toBeNull();
  });

  it("non-admin: builtin and global rows are view-only", async () => {
    setAuth(MEMBER);
    renderPage();
    await screen.findByText("ci-cd-norms");
    const builtin = within(rowFor("ci-cd-norms"));
    expect(builtin.getByRole("button", { name: "View" })).toBeTruthy();
    expect(builtin.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(builtin.queryByRole("button", { name: "Reset" })).toBeNull();

    const global = within(rowFor("argocd-debugging"));
    expect(global.getByRole("button", { name: "View" })).toBeTruthy();
    expect(global.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("non-admin: owns the Mine skill with Edit + Delete", async () => {
    setAuth(MEMBER);
    renderPage();
    await screen.findByText("qdrant-kb");
    const mine = within(rowFor("qdrant-kb"));
    expect(mine.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(mine.getByRole("button", { name: "Delete" })).toBeTruthy();
  });
});

describe("Skills page — create scope gating", () => {
  it("admin sees a scope selector with a Global option", async () => {
    setAuth(ADMIN);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /New skill/ }));
    // Create form: name is an editable input, scope is a select with Global.
    expect(screen.getByPlaceholderText("ci-cd-norms")).toBeTruthy();
    const scope = screen.getByLabelText("Scope") as HTMLSelectElement;
    expect(within(scope).getByRole("option", { name: /Global/ })).toBeTruthy();
  });

  it("non-admin gets a fixed personal scope, no Global option", async () => {
    setAuth(MEMBER);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /New skill/ }));
    expect(screen.getByPlaceholderText("ci-cd-norms")).toBeTruthy();
    expect(screen.queryByLabelText("Scope")).toBeNull();
    expect(screen.getByText(/only your runs will see this skill/i)).toBeTruthy();
  });

  it("sends scope and name to createSkill", async () => {
    setAuth(MEMBER);
    mockApi.createSkill.mockResolvedValue({ skill: skill({ id: "s-new", name: "my-skill", scope: "user" }) });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /New skill/ }));
    fireEvent.change(screen.getByPlaceholderText("ci-cd-norms"), { target: { value: "my-skill" } });
    fireEvent.change(screen.getByPlaceholderText(/When and why/), {
      target: { value: "A one-line description." },
    });
    fireEvent.change(screen.getByPlaceholderText(/What this playbook covers/), {
      target: { value: "# my-skill\n\nbody" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create skill" }));
    await waitFor(() =>
      expect(mockApi.createSkill).toHaveBeenCalledWith({
        name: "my-skill",
        description: "A one-line description.",
        body: "# my-skill\n\nbody",
        scope: "user",
      }),
    );
  });
});

describe("Skills page — name-locked edit", () => {
  it("edit shows the name as static text, not an input", async () => {
    setAuth(ADMIN);
    renderPage();
    await screen.findByText("argocd-debugging");
    fireEvent.click(within(rowFor("argocd-debugging")).getByRole("button", { name: "Edit" }));
    // Heading names the skill; the create-only name input is gone.
    expect(screen.getByText("Edit argocd-debugging")).toBeTruthy();
    expect(screen.queryByPlaceholderText("ci-cd-norms")).toBeNull();
  });

  it("updateSkill is called without a name field", async () => {
    setAuth(ADMIN);
    mockApi.updateSkill.mockResolvedValue({ skill: skill({ id: "s-global", name: "argocd-debugging", scope: "global" }) });
    renderPage();
    await screen.findByText("argocd-debugging");
    fireEvent.click(within(rowFor("argocd-debugging")).getByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSkill).toHaveBeenCalled());
    const [, payload] = mockApi.updateSkill.mock.calls[0];
    expect(payload).not.toHaveProperty("name");
    expect(payload).toHaveProperty("description");
    expect(payload).toHaveProperty("body");
  });
});

describe("Skills page — Other users view badge", () => {
  it("labels another user's skill 'Other user', not 'Mine', in the read-only view", async () => {
    setAuth(ADMIN); // qdrant-kb is owned by u-mira, so it lands in "Other users"
    renderPage();
    await screen.findByText("qdrant-kb");
    fireEvent.click(within(rowFor("qdrant-kb")).getByRole("button", { name: "View" }));
    expect(screen.getByText("Other user")).toBeTruthy();
    expect(screen.queryByText("Mine")).toBeNull();
  });
});

describe("Skills page — builtin reset confirm", () => {
  it("confirms before resetting a builtin", async () => {
    setAuth(ADMIN);
    mockApi.resetSkill.mockResolvedValue({ skill: skill({ id: "s-builtin", name: "ci-cd-norms", scope: "builtin" }) });
    renderPage();
    await screen.findByText("ci-cd-norms");
    fireEvent.click(within(rowFor("ci-cd-norms")).getByRole("button", { name: "Reset" }));
    // A confirm step appears before the call fires.
    expect(mockApi.resetSkill).not.toHaveBeenCalled();
    fireEvent.click(within(rowFor("ci-cd-norms")).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(mockApi.resetSkill).toHaveBeenCalledWith("s-builtin"));
  });
});
