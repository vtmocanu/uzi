// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentNew } from "./AgentNew";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      createAgentTemplate: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AgentNew — always-visible agent-templates guide link (PRD #57 M3)", () => {
  it("renders the agent templates guide link in the header intro", () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { id: "u1", is_admin: false },
    } as unknown as ReturnType<typeof useAuth>);

    render(
      <MemoryRouter>
        <AgentNew />
      </MemoryRouter>,
    );

    const docLink = screen.getByRole("link", { name: "agent templates" });
    expect(docLink.getAttribute("href")).toBe("/docs/agent-templates");
  });
});
