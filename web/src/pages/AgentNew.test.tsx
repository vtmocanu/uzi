// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentNew } from "./AgentNew";
import { useAuth } from "../auth/AuthContext";
import { api } from "../lib/api";

const mockNavigate = vi.hoisted(() => vi.fn());

vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => mockNavigate };
});

// Mock the editor to a bare submit trigger: this test is about the navigate()
// sink's encoding, not the editor's form. onSubmit is the same callback the real
// editor invokes with the built AgentTemplateInput.
vi.mock("../components/AgentTemplateEditor", () => ({
  AgentTemplateEditor: ({
    onSubmit,
  }: {
    onSubmit: (input: unknown) => void;
  }) => (
    <button type="button" onClick={() => onSubmit({ name: "my-agent", prompt_body: "x" })}>
      Create template
    </button>
  ),
}));

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

// Per-call-site open-redirect hardening (issue #644): the dynamic navigate() sink
// must encode the server-derived id, so an id carrying a path-escaping character
// is percent-encoded rather than injected into the path. A no-op for today's UUID
// ids, but structural — it holds even if the id format ever changes.
describe("AgentNew — encodes the server id at the navigate() sink (#644)", () => {
  it.each([
    ["../evil", "/agents/..%2Fevil"],
    ["a/b", "/agents/a%2Fb"],
  ])("navigates to an encoded path for id %j", async (id, expectedPath) => {
    vi.mocked(useAuth).mockReturnValue({
      user: { id: "u1", is_admin: false },
    } as unknown as ReturnType<typeof useAuth>);
    vi.mocked(api.createAgentTemplate).mockResolvedValue({
      template: { id },
    } as Awaited<ReturnType<typeof api.createAgentTemplate>>);

    render(
      <MemoryRouter>
        <AgentNew />
      </MemoryRouter>,
    );

    fireEvent.click(screen.getByRole("button", { name: /create template/i }));

    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith(expectedPath));
    // Guard against the injection shape: the raw "/" must never reach navigate().
    expect(mockNavigate).not.toHaveBeenCalledWith(`/agents/${id}`);
  });
});
