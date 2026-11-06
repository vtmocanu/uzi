// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentNew } from "./AgentNew";
import { useAuth } from "../auth/AuthContext";
import { api } from "../lib/api";

const mockNavigate = vi.hoisted(() => vi.fn());

vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => mockNavigate };
});

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

function fillRequiredFields() {
  fireEvent.change(screen.getByPlaceholderText("my-helper"), { target: { value: "my-agent" } });
  fireEvent.change(screen.getByLabelText(/Description/), { target: { value: "Routes useful work." } });
  // The prompt currently has a visible heading rather than an associated label.
  const prompt = screen.getAllByRole("textbox").find((el) => el.tagName === "TEXTAREA")!;
  fireEvent.change(prompt, { target: { value: "Do useful work." } });
}

function renderNew() {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: false },
  } as unknown as ReturnType<typeof useAuth>);
  vi.mocked(api.createAgentTemplate).mockResolvedValue({
    template: { id: "created" },
  } as Awaited<ReturnType<typeof api.createAgentTemplate>>);
  return render(<MemoryRouter><AgentNew /></MemoryRouter>);
}

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

describe("AgentNew real editor model pins", () => {
  it("labels both controls, offers all five Codex role models without Other, and explains Default", () => {
    renderNew();
    const claude = screen.getByRole("combobox", { name: "Claude model" });
    const codex = screen.getByRole("combobox", { name: "Codex model" });
    expect((claude as HTMLSelectElement).value).toBe("inherit");
    expect((codex as HTMLSelectElement).value).toBe("inherit");
    expect(within(codex).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "Default · lead's model", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna",
    ]);
    expect(within(claude).getByRole("option", { name: "Default · lead's model" })).toBeTruthy();
    const help = screen.getByText("Leave a harness on Default so this subagent uses whatever the lead runs on. A schedule with “Use the run's resolved model for subagents too” on overrides these pins.");
    expect(claude.getAttribute("aria-describedby")).toBe(help.id);
    expect(codex.getAttribute("aria-describedby")).toBe(help.id);

    fireEvent.change(screen.getByPlaceholderText("my-helper"), { target: { value: "lead" } });
    expect(within(claude).getByRole("option", { name: "Default · SDK/account default (built-in)" })).toBeTruthy();
    expect(within(codex).getByRole("option", { name: "Default · built-in (gpt-6.1-sol)" })).toBeTruthy();
  });

  it.each([
    ["claude-private-v2", "gpt-6-luna"],
    [null, "gpt-6-luna"],
    ["claude-private-v2", null],
    [null, null],
  ])("creates with independent pins Claude=%j Codex=%j after real picker transitions", async (model, codexModel) => {
    renderNew();
    fillRequiredFields();
    const claude = screen.getByRole("combobox", { name: "Claude model" });
    const codex = screen.getByRole("combobox", { name: "Codex model" });
    fireEvent.change(claude, { target: { value: "sonnet" } });
    fireEvent.change(claude, { target: { value: "custom" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Custom model ID" }), {
      target: { value: "claude-private-v2" },
    });
    fireEvent.change(codex, { target: { value: "gpt-6-sol" } });
    fireEvent.change(codex, { target: { value: "gpt-6-luna" } });
    if (model === null) fireEvent.change(claude, { target: { value: "inherit" } });
    if (codexModel === null) fireEvent.change(codex, { target: { value: "inherit" } });
    expect((claude as HTMLSelectElement).value).toBe(model === null ? "inherit" : "custom");
    expect((codex as HTMLSelectElement).value).toBe(codexModel ?? "inherit");
    fireEvent.click(screen.getByRole("button", { name: "Create template" }));

    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith("/agents/created"));
    expect(api.createAgentTemplate).toHaveBeenCalledExactlyOnceWith({
      name: "my-agent", scope: "user", description: "Routes useful work.",
      model, codex_model: codexModel, tools: null, prompt_body: "Do useful work.",
    });
  });

  it("blocks a Codex model in the Claude field even on programmatic submit, then recovers", async () => {
    const { container } = renderNew();
    fillRequiredFields();
    const claude = screen.getByRole("combobox", { name: "Claude model" });
    fireEvent.change(claude, { target: { value: "custom" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Custom model ID" }), {
      target: { value: "gpt-6-luna" },
    });
    expect(screen.getByText("Claude template model must not be a Codex role model ID.")).toBeTruthy();
    const submit = screen.getByRole("button", { name: "Create template" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.click(submit);
    fireEvent.submit(container.querySelector("form")!);
    expect(api.createAgentTemplate).not.toHaveBeenCalled();
    expect(mockNavigate).not.toHaveBeenCalled();

    fireEvent.change(claude, { target: { value: "inherit" } });
    expect(submit.disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith("/agents/created"));
    expect(api.createAgentTemplate).toHaveBeenCalledExactlyOnceWith({
      name: "my-agent", scope: "user", description: "Routes useful work.",
      model: null, codex_model: null, tools: null, prompt_body: "Do useful work.",
    });
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

    fillRequiredFields();
    fireEvent.click(screen.getByRole("button", { name: /create template/i }));

    expect(api.createAgentTemplate).toHaveBeenCalledExactlyOnceWith({
      name: "my-agent",
      scope: "user",
      description: "Routes useful work.",
      model: null,
      codex_model: null,
      tools: null,
      prompt_body: "Do useful work.",
    });

    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith(expectedPath));
    // Guard against the injection shape: the raw "/" must never reach navigate().
    expect(mockNavigate).not.toHaveBeenCalledWith(`/agents/${id}`);
  });
});
