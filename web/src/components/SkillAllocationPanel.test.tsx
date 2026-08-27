// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { SkillAllocationPanel } from "./SkillAllocationPanel";
import { api, type Skill, type TemplateSkills } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSkills: vi.fn(),
      getTemplateSkills: vi.fn(),
      setTemplateSkills: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function skill(over: Partial<Skill> & Pick<Skill, "id" | "name" | "scope">): Skill {
  return {
    description: `desc for ${over.name}`,
    body: "",
    user_id: null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const SKILLS: Skill[] = [
  skill({ id: "s-builtin", name: "prd-lifecycle", scope: "builtin" }),
  skill({ id: "s-global", name: "argocd-debugging", scope: "global" }),
  skill({ id: "s-mine", name: "qdrant-kb", scope: "user", user_id: "u-mira" }),
];

const ALLOC: TemplateSkills = {
  shared: [{ skill_id: "s-builtin", name: "prd-lifecycle", description: "d", scope: "builtin" }],
  mine: [],
};

beforeEach(() => {
  mockApi.listSkills.mockResolvedValue({ skills: SKILLS.map((s) => ({ ...s })) });
  mockApi.getTemplateSkills.mockResolvedValue({ allocations: { shared: [...ALLOC.shared], mine: [] } });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPanel(isAdmin: boolean) {
  return render(<SkillAllocationPanel templateId="t-coder" isAdmin={isAdmin} userId="u-mira" />);
}

describe("SkillAllocationPanel", () => {
  it("renders the union of shared + mine as what the runs get", async () => {
    renderPanel(false);
    expect(await screen.findByText(/Your runs get \(1\)/)).toBeTruthy();
    // The shared prd-lifecycle shows in the union even for a non-admin.
    const union = within(screen.getByRole("list", { name: /Skills your runs get/i }));
    expect(union.getByText("prd-lifecycle")).toBeTruthy();
  });

  it("admin sees both the shared and mine checklists", async () => {
    renderPanel(true);
    expect(await screen.findByRole("group", { name: /Shared/ })).toBeTruthy();
    expect(screen.getByRole("group", { name: /My skills for this agent/ })).toBeTruthy();
  });

  it("non-admin sees only the mine checklist (no shared editor)", async () => {
    renderPanel(false);
    expect(await screen.findByRole("group", { name: /My skills for this agent/ })).toBeTruthy();
    expect(screen.queryByRole("group", { name: /^Shared/ })).toBeNull();
  });

  it("a non-admin save sends my_skill_ids only, leaving shared untouched", async () => {
    mockApi.setTemplateSkills.mockResolvedValue({
      allocations: { shared: [...ALLOC.shared], mine: [{ skill_id: "s-mine", name: "qdrant-kb", description: "d", scope: "user" }] },
    });
    renderPanel(false);
    const mine = within(await screen.findByRole("group", { name: /My skills for this agent/ }));
    fireEvent.click(mine.getByRole("checkbox", { name: /qdrant-kb/ }));
    fireEvent.click(screen.getByRole("button", { name: /Save allocations/ }));
    await waitFor(() => expect(mockApi.setTemplateSkills).toHaveBeenCalled());
    const [tid, payload] = mockApi.setTemplateSkills.mock.calls[0];
    expect(tid).toBe("t-coder");
    expect(payload).toEqual({ my_skill_ids: ["s-mine"] });
    expect(payload).not.toHaveProperty("shared_skill_ids");
  });

  it("an admin save of the shared half sends shared_skill_ids", async () => {
    mockApi.setTemplateSkills.mockResolvedValue({
      allocations: {
        shared: [
          { skill_id: "s-builtin", name: "prd-lifecycle", description: "d", scope: "builtin" },
          { skill_id: "s-global", name: "argocd-debugging", description: "d", scope: "global" },
        ],
        mine: [],
      },
    });
    renderPanel(true);
    const shared = within(await screen.findByRole("group", { name: /Shared/ }));
    fireEvent.click(shared.getByRole("checkbox", { name: /argocd-debugging/ }));
    fireEvent.click(screen.getByRole("button", { name: /Save allocations/ }));
    await waitFor(() => expect(mockApi.setTemplateSkills).toHaveBeenCalled());
    const [, payload] = mockApi.setTemplateSkills.mock.calls[0];
    expect(payload.shared_skill_ids).toEqual(expect.arrayContaining(["s-builtin", "s-global"]));
    expect(payload).not.toHaveProperty("my_skill_ids");
  });

  it("Save is disabled until an allocation changes", async () => {
    renderPanel(false);
    const save = (await screen.findByRole("button", { name: /Save allocations/ })) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    const mine = within(screen.getByRole("group", { name: /My skills for this agent/ }));
    fireEvent.click(mine.getByRole("checkbox", { name: /qdrant-kb/ }));
    expect(save.disabled).toBe(false);
  });

  // #166: a global skill's description is admin-authored and renders into another
  // user's tooltip via the badge `title` ATTRIBUTE. The #124 suite was blind to
  // this whole channel because it asserts over container.textContent, which
  // excludes attribute values — this covers the attribute directly so a future
  // regression in this sink is visible.
  it("carries the allocated skill description into the badge title ATTRIBUTE, not textContent", async () => {
    const DESC = "global skill tooltip — attribute channel, cross-principal";
    mockApi.listSkills.mockResolvedValue({
      skills: [skill({ id: "s-global", name: "argocd-debugging", scope: "global", description: DESC })],
    });
    mockApi.getTemplateSkills.mockResolvedValue({
      allocations: {
        shared: [{ skill_id: "s-global", name: "argocd-debugging", description: DESC, scope: "global" }],
        mine: [],
      },
    });
    renderPanel(false);
    const union = within(await screen.findByRole("list", { name: /Skills your runs get/i }));
    // getByTitle asserts the description reached the title attribute — the channel a
    // container.textContent assertion cannot see.
    const badge = union.getByTitle(DESC);
    expect(badge.textContent).toContain("argocd-debugging");
    // Proves the assertion targets the attribute, not the visible text.
    expect(badge.textContent).not.toContain(DESC);
  });
});
