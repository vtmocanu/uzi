// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { JudgeSettingsCard } from "./JudgeSettingsCard";
import { api, type AppSettings } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const settings = {
  judge_enabled: "true",
  judge_model: "",
  codex_judge_model: "",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
} as AppSettings;

function renderCard() {
  render(
    <JudgeSettingsCard
      settings={settings}
      sources={{ judge_model: "default", codex_judge_model: "default" }}
      defaults={{ judge_model: "opus", codex_judge_model: "gpt-6.1-sol" }}
      onSaved={vi.fn()}
    />,
  );
}

const save = () => screen.getByRole("button", { name: "Save run judge settings" }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("JudgeSettingsCard model validation (#3255)", () => {
  it("blocks Save and warns beneath the picker for an invalid custom Claude judge ID", () => {
    renderCard();
    fireEvent.change(screen.getByLabelText("Claude judge model"), { target: { value: "custom" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Custom Claude judge model ID" }), { target: { value: "bad id" } });
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("warns beneath the Codex picker for a stored invalid Codex ID without locking unrelated saves", () => {
    mockApi.updateSettings.mockResolvedValue({
      settings: { ...settings, codex_judge_model: "bad id", judge_model: "sonnet" },
      sources: { judge_model: "db", codex_judge_model: "db" },
    } as never);
    render(
      <JudgeSettingsCard
        settings={{ ...settings, codex_judge_model: "bad id" }}
        sources={{ judge_model: "default", codex_judge_model: "db" }}
        defaults={{ judge_model: "opus", codex_judge_model: "gpt-6.1-sol" }}
        onSaved={vi.fn()}
      />,
    );
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Claude judge model"), { target: { value: "sonnet" } });
    // The untouched stored value is never sent, so it must not block saving the change.
    expect(save().disabled).toBe(false);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    const payload = mockApi.updateSettings.mock.calls[0]?.[0] ?? {};
    expect(payload).toMatchObject({ judge_model: "sonnet" });
    expect(payload).not.toHaveProperty("codex_judge_model");
  });

  it("enforce help no longer calls the built-in the strongest and points at the pickers", () => {
    renderCard();
    const help = screen.getByText(/Before\s+enforcing/).textContent ?? "";
    expect(help).toContain("Default follows the built-in model.");
    expect(help).not.toContain("strongest");
    // No judge copy ranks the built-in by capability: it is a pinned default.
    expect(document.body.textContent ?? "").not.toMatch(/strongest/i);
  });
});
