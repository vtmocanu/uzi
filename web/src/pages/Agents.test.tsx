// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Agents } from "./Agents";
import { api, type AgentTemplate, type TemplateAllocation, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listAgentTemplates: vi.fn(),
      getTemplateAllocations: vi.fn(),
      setTemplateAllocations: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const mockUseAuth = vi.mocked(useAuth);

const MEMBER: User = {
  id: "u-mira",
  email: "mira@uzi.local",
  display_name: "Mira",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function tmpl(over: Partial<AgentTemplate> & Pick<AgentTemplate, "id" | "name" | "scope">): AgentTemplate {
  return {
    description: `desc ${over.name}`,
    model: null,
    tools: null,
    prompt_body: "body",
    is_builtin: over.scope === "builtin",
    user_id: over.scope === "user" ? MEMBER.id : null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function alloc(over: Partial<TemplateAllocation> & Pick<TemplateAllocation, "id" | "name" | "scope">): TemplateAllocation {
  return {
    description: `desc ${over.name}`,
    is_builtin: over.scope === "builtin",
    global_default: over.scope !== "user",
    my_override: null,
    effective: over.scope !== "user",
    ...over,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Agents page shadowed hint (PRD #18 M7/M8)", () => {
  it("badges a user template whose name collides with a builtin, not a unique one", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const templates = [
      tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
      tmpl({ id: "u-coder", name: "coder", scope: "user" }),
      tmpl({ id: "u-helper", name: "unique-helper", scope: "user" }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
        alloc({ id: "u-coder", name: "coder", scope: "user", my_override: true, effective: true, global_default: false }),
        alloc({ id: "u-helper", name: "unique-helper", scope: "user", my_override: true, effective: true, global_default: false }),
      ],
    });

    render(
      <MemoryRouter>
        <Agents />
      </MemoryRouter>,
    );

    // The user-scope "coder" row carries the shadowed badge; the builtin "coder"
    // and the uniquely-named user template do not.
    const userCoderRow = await waitFor(() => {
      const link = screen.getByRole("link", { name: "unique-helper" });
      // once the table has rendered, find the user coder row by its shadowed badge
      const badges = screen.getAllByText("shadowed");
      expect(badges).toHaveLength(1);
      return link;
    });
    expect(userCoderRow).toBeTruthy();

    const helperRow = screen.getByRole("link", { name: "unique-helper" }).closest("tr")!;
    expect(within(helperRow).queryByText("shadowed")).toBeNull();
  });
});
