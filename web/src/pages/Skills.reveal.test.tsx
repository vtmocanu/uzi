// @vitest-environment jsdom
//
// Issue #4373: `/skills?skill=<encodeURIComponent(id)>` opens that skill's view on direct
// entry and on in-app navigation, once per location.key; product-scope and unknown ids
// leave the list in place.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { Skills } from "./Skills";
import { api, type Skill } from "../lib/api";
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
const TRICKY_ID = "a b/c#d";

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
  skill({ id: "s-global", name: "argocd-debugging", scope: "global" }),
  skill({ id: TRICKY_ID, name: "odd-skill", scope: "global" }),
  skill({ id: "s-product", name: "product-only", scope: "product" }),
];

const linkFor = (id: string) => `/skills?skill=${encodeURIComponent(id)}`;

function Nav({ to }: { to: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      go
    </button>
  );
}
function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

function BrowserBack() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(-1)}>
      browser back
    </button>
  );
}

function renderAt(entry: string, navTo?: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      {navTo && <Nav to={navTo} />}
      <BrowserBack />
      <Where />
      <Skills />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.listSkills.mockResolvedValue({ skills: SKILLS.map((s) => ({ ...s })) });
  vi.mocked(useAuth).mockReturnValue({ user: { id: "u-mira", is_admin: false } } as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Skills deep-link view (#4373)", () => {
  it("direct entry opens the skill view after load", async () => {
    renderAt(linkFor("s-global"));
    expect(await screen.findByRole("heading", { name: "argocd-debugging" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back" })).toBeTruthy();
  });

  it("in-app navigation opens the view on the mounted list", async () => {
    renderAt("/skills", linkFor("s-global"));
    await screen.findByText("odd-skill");
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    expect(await screen.findByRole("heading", { name: "argocd-debugging" })).toBeTruthy();
  });

  it("round-trips an id with reserved characters", async () => {
    renderAt(linkFor(TRICKY_ID));
    expect(await screen.findByRole("heading", { name: "odd-skill" })).toBeTruthy();
  });

  it("an unknown id stays on the list", async () => {
    renderAt(linkFor("nope"));
    await screen.findByText("odd-skill");
    await act(async () => {});
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
  });

  it("a product-scope skill id opens nothing", async () => {
    renderAt(linkFor("s-product"));
    await screen.findByText("odd-skill");
    await act(async () => {});
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
    expect(screen.queryByText("product-only")).toBeNull();
  });

  it("Back drops the skill param and a reload does not reopen the view", async () => {
    renderAt(linkFor("s-global"));
    await screen.findByRole("heading", { name: "argocd-debugging" });
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await screen.findByText("odd-skill");
    expect(screen.getByTestId("where").textContent).toBe("/skills");
    await act(async () => {});
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
  });

  it("browser Back from a deep-linked view returns to the list", async () => {
    renderAt("/skills", linkFor("s-global"));
    await screen.findByText("odd-skill");
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    await screen.findByRole("heading", { name: "argocd-debugging" });
    fireEvent.click(screen.getByRole("button", { name: "browser back" }));
    await screen.findByText("odd-skill");
    expect(screen.getByTestId("where").textContent).toBe("/skills");
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
  });

  it("navigating from a deep-linked view to an unknown or product id closes that view", async () => {
    for (const id of ["nope", "s-product"]) {
      const view = renderAt(linkFor("s-global"), linkFor(id));
      await screen.findByRole("heading", { name: "argocd-debugging" });
      fireEvent.click(screen.getByRole("button", { name: "go" }));
      await screen.findByText("odd-skill");
      expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
      view.unmount();
    }
  });

  it("re-navigating to the same skill after Back reopens it (new location.key)", async () => {
    renderAt(linkFor("s-global"), linkFor("s-global"));
    await screen.findByRole("heading", { name: "argocd-debugging" });
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await screen.findByText("odd-skill");
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    expect(await screen.findByRole("heading", { name: "argocd-debugging" })).toBeTruthy();
  });
});
