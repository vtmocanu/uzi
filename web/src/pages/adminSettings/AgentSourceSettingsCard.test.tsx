// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
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
      { name: "lead", ok: true, description: "Leads.", prompt_body: "lead body" },
      { name: "coder", ok: true, description: "Codes.", prompt_body: "coder body" },
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
  it("shows the honest Default label for a role with no model, never 'inherit'", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: view });
    render(<AgentSourceSettingsCard />);
    const buttons = await screen.findAllByRole("button", { name: "Show body" });
    buttons.forEach((b) => fireEvent.click(b));
    expect(screen.getByText("Default · SDK/account default (built-in)")).toBeTruthy();
    expect(screen.getByText("Default · lead's model")).toBeTruthy();
    expect(screen.queryByText(/^inherit$/i)).toBeNull();
  });
});
