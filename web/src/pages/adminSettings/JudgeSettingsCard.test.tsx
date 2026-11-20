// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { JudgeSettingsCard } from "./JudgeSettingsCard";
import { api, type AppSettings, type SettingSource } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const settings = {
  judge_enabled: "true",
  judge_model: "opus",
  codex_judge_model: "gpt-6.1-sol",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
} as AppSettings;
const defaults = { judge_model: "opus", codex_judge_model: "gpt-6.1-sol" };
const unpinned: Record<string, SettingSource> = { judge_model: "default", codex_judge_model: "default" };

function renderCard(
  s: AppSettings = settings,
  sources: Record<string, SettingSource> = unpinned,
  d: Record<string, string> = defaults,
  onSaved: (...args: unknown[]) => void = vi.fn(),
) {
  return render(<JudgeSettingsCard settings={s} sources={sources} defaults={d} onSaved={onSaved} />);
}

const save = () => screen.getByRole("button", { name: "Save run judge settings" }) as HTMLButtonElement;
const instanceLine = () => (screen.getByTestId("judge-instance-default").textContent ?? "").replace(/\s+/g, " ");

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("JudgeSettingsCard policy save (#3259)", () => {
  it("sends exactly the four policy keys and never a model key", async () => {
    const resp = { settings: { ...settings, judge_cooldown_seconds: "120" }, sources: {} };
    mockApi.updateSettings.mockResolvedValue(resp as never);
    const onSaved = vi.fn();
    renderCard(settings, unpinned, defaults, onSaved);
    expect(save().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Per-user cooldown (seconds)"), { target: { value: "120" } });
    fireEvent.click(save());
    await screen.findByText("Run judge settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({
      judge_enabled: "true",
      judge_enforce_all: "false",
      judge_cooldown_seconds: "120",
      judge_daily_budget: "0",
    });
    // onSaved receives the response and the exact payload sent, so the page can
    // confirm only the keys this request carried.
    expect(onSaved).toHaveBeenCalledWith(resp, {
      judge_enabled: "true",
      judge_enforce_all: "false",
      judge_cooldown_seconds: "120",
      judge_daily_budget: "0",
    });
    const payload = mockApi.updateSettings.mock.calls[0]?.[0] ?? {};
    expect(Object.keys(payload).sort()).toEqual([
      "judge_cooldown_seconds",
      "judge_daily_budget",
      "judge_enabled",
      "judge_enforce_all",
    ]);
  });

  it("renders no model picker", () => {
    renderCard();
    expect(screen.queryByRole("combobox")).toBeNull();
  });
});

describe("JudgeSettingsCard read-only instance default line (#3259)", () => {
  it("shows the built-in for unpinned lanes and links to Model defaults", () => {
    renderCard();
    expect(instanceLine()).toContain(
      "Instance default: Claude opus (built-in) · Codex gpt-6.1-sol (built-in) · users may override.",
    );
    expect(screen.getAllByRole("link", { name: "Model defaults" }).every((a) => a.getAttribute("href") === "#model-defaults")).toBe(true);
  });

  it("stays neutral when the server sent no defaults", () => {
    renderCard(settings, unpinned, {});
    expect(instanceLine()).toContain("Instance default: Claude built-in · Codex built-in");
  });

  it("shows pinned values, marking an environment-set one", () => {
    renderCard(
      { ...settings, judge_model: "sonnet", codex_judge_model: "gpt-6-astra" },
      { judge_model: "db", codex_judge_model: "env" },
    );
    expect(instanceLine()).toContain("Instance default: Claude sonnet · Codex gpt-6-astra, from the environment");
  });

  it("follows props: a rerender with new settings updates the line", () => {
    const { rerender } = renderCard();
    expect(instanceLine()).toContain("Claude opus (built-in)");
    rerender(
      <JudgeSettingsCard
        settings={{ ...settings, judge_model: "sonnet" }}
        sources={{ judge_model: "db", codex_judge_model: "default" }}
        defaults={defaults}
        onSaved={vi.fn()}
      />,
    );
    expect(instanceLine()).toContain("Instance default: Claude sonnet");
    expect(instanceLine()).not.toContain("opus (built-in)");
  });

  it("does not make the card dirty when the instance default changes", () => {
    const { rerender } = renderCard();
    rerender(
      <JudgeSettingsCard
        settings={{ ...settings, judge_model: "sonnet" }}
        sources={{ judge_model: "db", codex_judge_model: "default" }}
        defaults={defaults}
        onSaved={vi.fn()}
      />,
    );
    expect(save().disabled).toBe(true);
  });
});

// PRD #3257 M1: the intro says the judge follows each user's "Judge runs on" choice,
// defaulting to the run's harness.
describe("JudgeSettingsCard judge harness copy (#3257)", () => {
  it("says the judge runs on each user's Judge runs on choice, by default the run's own", () => {
    renderCard();
    const text = (document.body.textContent ?? "").replace(/\s+/g, " ");
    expect(text).toContain("for the harness their “Judge runs on” choice picks, by default the run’s own");
  });
});

// #3259: the enforce help states the Judge runs on default, the run's own harness.
describe("JudgeSettingsCard enforce harness default (#3259)", () => {
  it("says enforced judges follow Judge runs on, by default the run's own harness", () => {
    renderCard();
    const help = screen.getByText(/A user with no usable credential/).textContent?.replace(/\s+/g, " ") ?? "";
    expect(help).toContain("following their “Judge runs on” choice (default: the run’s own harness)");
  });
});

// PRD #3257 M2: the enforce help says a user with no usable credential is skipped WITH A REASON
// SHOWN (the run page's skip notice) and still that the other provider is never charged.
describe("JudgeSettingsCard enforce copy (#3257 M2)", () => {
  it("says a user with no usable credential is skipped with a reason shown, never charged elsewhere", () => {
    renderCard();
    const help = screen.getByText(/A user with no usable credential/).textContent?.replace(/\s+/g, " ") ?? "";
    expect(help).toContain(
      "A user with no usable credential for that harness is skipped, with a reason shown on the run, and is never charged to the other provider.",
    );
  });

  it("points at Model defaults instead of the retired pickers", () => {
    renderCard();
    const help = screen.getByText(/A user with no usable credential/).textContent?.replace(/\s+/g, " ") ?? "";
    expect(help).toContain("Consider a cheaper judge model in Model defaults before enforcing.");
    expect(document.body.textContent ?? "").not.toMatch(/pickers above|strongest/i);
  });
});

describe("JudgeSettingsCard kill-switch precedence", () => {
  it("never sends enforce-all on when the judge is disabled", async () => {
    mockApi.updateSettings.mockResolvedValue({
      settings: { ...settings, judge_enabled: "false", judge_enforce_all: "false" },
      sources: {},
    } as never);
    renderCard({ ...settings, judge_enforce_all: "true" });
    fireEvent.click(screen.getByRole("checkbox", { name: "Enable the run judge for this instance" }));
    expect((screen.getByRole("checkbox", { name: /Enforce the judge/ }) as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(save());
    await screen.findByText("Run judge settings saved.");
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toMatchObject({
      judge_enabled: "false",
      judge_enforce_all: "false",
    });
  });
});
