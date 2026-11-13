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

// PRD #3257 M1: the instance help says the judge follows each user's "Judge runs on" choice,
// defaulting to the run's harness, and no longer that it always follows the run.
describe("JudgeSettingsCard judge harness copy (#3257)", () => {
  it("says the judge runs on each user's Judge runs on choice, by default the run's harness", () => {
    renderCard();
    const help = screen.getByText(/so each harness has its own judge model/).textContent ?? "";
    expect(help).toContain(
      "The judge runs on each user’s “Judge runs on” choice, by default the harness of the run it reviews",
    );
    expect(help).not.toContain("The judge runs on the harness of the run it reviews");
  });
});

// PRD #3257 M2: the enforce help says a user with no usable credential is skipped WITH A REASON
// SHOWN (the run page's skip notice), matching AfterAdmin.dc.html, and still that the other
// provider is never charged.
describe("JudgeSettingsCard skip copy (#3257 M2)", () => {
  it("says a user with no usable credential is skipped with a reason shown, never charged elsewhere", () => {
    renderCard();
    const help = screen.getByText(/Before\s+enforcing/).textContent?.replace(/\s+/g, " ") ?? "";
    expect(help).toContain(
      "A user with no usable credential for that harness is skipped, with a reason shown on the run, and is never charged to the other provider.",
    );
  });
});
