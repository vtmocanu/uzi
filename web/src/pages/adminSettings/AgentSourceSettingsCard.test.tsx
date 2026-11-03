// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { AgentSourceSettingsCard } from "./AgentSourceSettingsCard";
import { api, type AgentSourceView } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { getAgentSource: vi.fn() } };
});
const mockApi = vi.mocked(api);

const view: AgentSourceView = {
  config: { url: "https://example.test/skills", ref: "v1", folder: ".claude/agents", enabled: true, interval: "1h", credential_configured: false },
  status: {},
  staged: {
    fetched_sha: "0123456789abcdef",
    source_url: "https://example.test/skills",
    source_ref: "v1",
    pending: true,
    counts: { staged: 2, changed: 2, failed: 0 },
    roles: [
      { name: "lead", ok: true, claude_model: "", codex_model: "", prompt_body: "lead body" },
      { name: "coder", ok: true, claude_model: "", codex_model: "", prompt_body: "coder body" },
    ],
    diff: [
      { name: "lead", action: "add" },
      { name: "coder", action: "add" },
    ],
  },
};

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("AgentSourceSettingsCard staged review model label (#3255)", () => {
  it.each([
    ["gpt-6-sol", "", "gpt-6-sol"],
    ["gpt-6-luna", "", "gpt-6-luna"],
    ["us.anthropic.claude-custom:v1", "us.anthropic.claude-custom:v1", ""],
  ])("shows separate pins for raw %s", async (model, claudeModel, codexModel) => {
    const snapshot = structuredClone(view);
    snapshot.staged!.roles = [{
      name: "coder", ok: true, model, claude_model: claudeModel,
      codex_model: codexModel, prompt_body: "coder body", body_sanitized: true,
    }];
    snapshot.staged!.diff = [{ name: "coder", action: "add" }];
    mockApi.getAgentSource.mockResolvedValue({ agent_source: snapshot });
    render(<AgentSourceSettingsCard />);
    const button = await screen.findByRole("button", { name: "Show body" });
    const row = within(button.closest("li")!);
    expect(row.getByText(/Hidden formatting characters were removed/)).toBeTruthy();
    fireEvent.click(button);
    expect(row.getByText("Claude model").nextElementSibling?.textContent)
      .toBe(claudeModel || "Default · lead's model");
    expect(row.getByText("Codex model").nextElementSibling?.textContent)
      .toBe(codexModel || "Default · lead's model");
    expect(row.queryByText("Model", { exact: true })).toBeNull();
  });

  it("uses derived review pins even when the sanitized raw model looks pinned", async () => {
    const snapshot = structuredClone(view);
    snapshot.staged!.roles = [{
      name: "coder", ok: true, model: "gpt-6-sol", claude_model: "",
      codex_model: "", prompt_body: "coder body",
    }];
    snapshot.staged!.diff = [{ name: "coder", action: "add" }];
    mockApi.getAgentSource.mockResolvedValue({ agent_source: snapshot });
    render(<AgentSourceSettingsCard />);
    const button = await screen.findByRole("button", { name: "Show body" });
    fireEvent.click(button);
    const row = within(button.closest("li")!);
    expect(row.getByText("Claude model").nextElementSibling?.textContent).toBe("Default · lead's model");
    expect(row.getByText("Codex model").nextElementSibling?.textContent).toBe("Default · lead's model");
    expect(row.queryByText("gpt-6-sol")).toBeNull();
  });

  it("shows the honest Default label for a role with no model, never 'inherit'", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: view });
    render(<AgentSourceSettingsCard />);
    const buttons = await screen.findAllByRole("button", { name: "Show body" });
    buttons.forEach((b) => fireEvent.click(b));
    const lead = within(screen.getByText("lead", { selector: "code" }).closest("li")!);
    const coder = within(screen.getByText("coder", { selector: "code" }).closest("li")!);
    expect(lead.getByText("Default · SDK/account default (built-in)")).toBeTruthy();
    expect(lead.getByText("Default · built-in (gpt-6.1-sol)")).toBeTruthy();
    expect(coder.getAllByText("Default · lead's model")).toHaveLength(2);
    for (const row of [lead, coder]) {
      expect(row.getByText("Claude model")).toBeTruthy();
      expect(row.getByText("Codex model")).toBeTruthy();
      expect(row.queryByText("Model", { exact: true })).toBeNull();
    }
    expect(screen.queryByText(/^inherit$/i)).toBeNull();
  });
});
