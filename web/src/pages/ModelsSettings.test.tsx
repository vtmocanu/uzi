// @vitest-environment jsdom
//
// PRD #3263 M1 and M2: the Models page against a stubbed API and a controllable viewer. One draft,
// one save request, server errors pinned to their cells, saved-vs-draft wording, reset
// semantics, viewer fencing of late responses, failure reconciliation and pending-save
// disabling. Every wait is event-driven (findBy/waitFor on rendered state), never a sleep.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { api, ApiError, type EffectiveSettings, type UserSettings } from "../lib/api";
import type { CrossCheckPinDTO } from "../lib/apiTypes";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../lib/codexModels";
import { ModelsSettings } from "./ModelsSettings";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    // putMySettings is present only to prove the Models save never calls it.
    api: { getMySettings: vi.fn(), getEffectiveSettings: vi.fn(), putMyModelSettings: vi.fn(), putMySettings: vi.fn() },
  };
});

// A tiny external store standing in for AuthContext, so a test can switch the viewer.
const auth = vi.hoisted(() => {
  let user: { id: string } | null = { id: "u1" };
  const listeners = new Set<() => void>();
  return {
    get: () => user,
    set(next: { id: string } | null) {
      user = next;
      listeners.forEach((l) => l());
    },
    subscribe(l: () => void) {
      listeners.add(l);
      return () => listeners.delete(l);
    },
  };
});
vi.mock("../auth/AuthContext", async () => {
  const { useSyncExternalStore } = await import("react");
  return { useAuth: () => ({ user: useSyncExternalStore(auth.subscribe, auth.get) }) };
});

const mockApi = vi.mocked(api);

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function makeSettings(over: Partial<UserSettings> = {}): UserSettings {
  return {
    default_harness: null, default_model: null, default_claude_model: null, default_codex_model: null,
    default_effort: null, default_codex_effort: null, judge_model: null, summary_model: null, judge_runs_on: null,
    harness_overflow_enabled: false, theme: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
    planning_claude_model: null, planning_claude_effort: null, planning_codex_model: null, planning_codex_effort: null,
    planning_models: [
      { harness: "claude", model: null, default_model: "opus", default_source: "template", resolved_model: "opus", source: "template" },
      { harness: "codex", model: null, default_model: "gpt-6.2-sol", default_source: "builtin", resolved_model: "gpt-6.2-sol", source: "builtin" },
    ],
    planning_efforts: [
      { harness: "claude", effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit" },
      { harness: "codex", effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit" },
    ],
    ...over,
  };
}

const modelCell = (setting: "worker_model" | "planning_model", harness: "claude" | "codex", resolved: string, source: string) => ({
  harness, setting, model: null, default_model: resolved, default_source: source, resolved_model: resolved, source,
  fixed_at: "claim", ignored: null, notes: [],
}) as EffectiveSettings["models"][number];
const effortCell = (setting: "worker_effort" | "planning_effort", harness: "claude" | "codex", resolved: string | null, source: string) => ({
  harness, setting, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: resolved, source,
  mode: resolved === null ? "runtime_default" : "explicit", fixed_at: "claim", notes: [],
}) as EffectiveSettings["efforts"][number];

function makeEffective(over: { claudeModel?: string; codexModel?: string; codexSource?: string; harness?: string | null } = {}): EffectiveSettings {
  return {
    version: 1,
    harnesses: [
      { harness: "claude", usable: true, refusal: null, refusal_detail: null, credential: null },
      { harness: "codex", usable: false, refusal: "no_credential_for_harness", refusal_detail: null, credential: null },
    ],
    start: [{
      choice: "default",
      harness: over.harness
        ? { value: over.harness, source: "user_default", fixed_at: "create", notes: [] }
        : { value: "claude", source: "automatic", fixed_at: "create", notes: [] },
      refusal: null, refusal_detail: null, anthropic_override_allowed: true,
    }],
    models: [
      modelCell("worker_model", "claude", over.claudeModel ?? "opus", "template"),
      modelCell("worker_model", "codex", over.codexModel ?? "gpt-6.2-sol", over.codexSource ?? "builtin"),
      modelCell("planning_model", "claude", "opus", "template"),
      modelCell("planning_model", "codex", "gpt-6.2-sol", "builtin"),
    ],
    efforts: [
      effortCell("worker_effort", "claude", "medium", "builtin"),
      effortCell("worker_effort", "codex", "medium", "builtin"),
      effortCell("planning_effort", "claude", "medium", "builtin"),
      effortCell("planning_effort", "codex", "medium", "builtin"),
    ],
    cross_check: { models: [], efforts: [] },
    judge_harness: { value: null, source: "same_as_run", fixed_at: "create", notes: [] },
  };
}

function mount() {
  const router = createMemoryRouter(
    [
      { path: "/settings/models", element: <ModelsSettings /> },
      { path: "*", element: <p>elsewhere</p> },
    ],
    { initialEntries: ["/settings/models"] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

const control = (name: string) => screen.getByLabelText(name) as HTMLSelectElement;
const select = (name: string, value: string) => fireEvent.change(control(name), { target: { value } });
const bar = () => screen.getByTestId("models-save-bar");
const WARNING = "models-earlier-request-warning";
const saveButton = () => within(bar()).getByRole("button", { name: /Save changes|Saving/ }) as HTMLButtonElement;
const discardButton = () => within(bar()).getByRole("button", { name: "Discard" }) as HTMLButtonElement;
const status = () => screen.getByTestId("models-save-status").textContent ?? "";
// The prediction line a control is described by.
function describedText(name: string): string {
  const ids = (control(name).getAttribute("aria-describedby") ?? "").split(" ").filter(Boolean);
  return ids.map((id) => document.getElementById(id)?.textContent ?? "").join(" | ");
}

beforeEach(() => {
  auth.set({ id: "u1" });
  mockApi.getMySettings.mockResolvedValue({ settings: makeSettings() });
  mockApi.getEffectiveSettings.mockResolvedValue(makeEffective());
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Models page", () => {
  it("names every control <Row> — <Family> and describes it with its saved prediction", async () => {
    mount();
    await screen.findByLabelText("Worker model — Claude");
    for (const name of [
      "Default harness", "Worker model — Claude", "Worker model — Codex", "Worker effort — Claude", "Worker effort — Codex",
      "Planning model — Claude", "Planning model — Codex", "Planning effort — Claude", "Planning effort — Codex",
    ]) {
      expect(control(name).tagName, name).toBe("SELECT");
      expect(describedText(name), name).toContain("Saved effective value:");
    }
    expect(describedText("Worker model — Claude")).toContain("Saved effective value: opus · lead template");
    expect(describedText("Default harness")).toContain("Saved effective value: Claude · automatic");
    // A family without a usable credential says so, as a reason, without disabling anything.
    expect(describedText("Worker model — Codex")).toContain("No usable Codex credential");
    expect(control("Worker model — Codex").disabled).toBe(false);
    expect(status()).toBe("No unsaved changes");
  });

  it("saves several cells in exactly one request and installs the returned settings as the clean baseline", async () => {
    const saved = makeSettings({ default_harness: "codex", default_codex_model: "gpt-6-sol", planning_claude_effort: "high" });
    mockApi.putMyModelSettings.mockResolvedValue({
      settings: saved,
      effective: makeEffective({ codexModel: "gpt-6-sol", codexSource: "user_default", harness: "codex" }),
    });
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Default harness", "codex");
    select("Worker model — Codex", "gpt-6-sol");
    select("Planning effort — Claude", "high");
    expect(status()).toBe("3 unsaved changes: Default harness, Worker model — Codex, Planning effort — Claude");
    fireEvent.click(saveButton());
    await within(bar()).findByText("Saved 3 changes. Your next runs use them.");
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({
      default_harness: "codex", default_codex_model: "gpt-6-sol", planning_claude_effort: "high",
    });
    expect(status()).toBe("No unsaved changes");
    expect(control("Default harness").value).toBe("codex");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(describedText("Worker model — Codex")).toContain("Saved effective value: gpt-6-sol · your choice");
    expect(describedText("Worker model — Codex")).not.toContain("Unsaved");
    expect(saveButton().disabled).toBe(true);
  });

  it("labels the saved prediction as saved and marks a changed cell Unsaved", async () => {
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    const text = describedText("Worker model — Codex");
    expect(text).toContain("Saved effective value: gpt-6.2-sol · built-in");
    expect(text).toContain("Unsaved");
    expect(describedText("Worker model — Claude")).not.toContain("Unsaved");
    // Back to the stored value is no change at all.
    select("Worker model — Codex", "inherit");
    expect(status()).toBe("No unsaved changes");
  });

  it("keeps Runtime default distinct from Default, and offers no Runtime default for planning", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_codex_effort: "runtime" }) });
    mockApi.putMyModelSettings.mockResolvedValue({ settings: makeSettings({ default_effort: "runtime" }), effective: makeEffective() });
    mount();
    await screen.findByLabelText("Worker effort — Claude");
    const claude = control("Worker effort — Claude");
    expect(Array.from(claude.options).map((o) => o.value)).toEqual(["", "runtime", "low", "medium", "high", "xhigh", "max"]);
    expect(Array.from(control("Planning effort — Claude").options).map((o) => o.value)).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
    expect(control("Worker effort — Codex").value).toBe("runtime");
    select("Worker effort — Claude", "runtime");
    select("Worker effort — Codex", "");
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ default_effort: "runtime", default_codex_effort: null });
  });

  // Moved from Run defaults (PRD #3263 M3): each family's Runtime default option names what it
  // means, Codex's from the server's catalog_default on the saved effort cell.
  it("labels Runtime default per family: Claude reports no value, Codex names its catalog default", async () => {
    const effortSetting = (harness: "claude" | "codex", extra: object = {}) => ({
      harness, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit", ...extra,
    });
    const known = { state: "known", effort: "medium", model: "gpt-6.1-sol", runtime_version: "0.164.0", observed_at: "2026-11-05T09:30:00Z" };
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({
      effort_settings: [effortSetting("claude"), effortSetting("codex", { catalog_default: known })] as UserSettings["effort_settings"],
    }) });
    mount();
    await screen.findByLabelText("Worker effort — Codex");
    const runtimeText = (name: string) => Array.from(control(name).options).find((o) => o.value === "runtime")?.text;
    expect(runtimeText("Worker effort — Claude")).toBe("Runtime default · value not reported");
    expect(runtimeText("Worker effort — Codex")).toBe("Runtime default · catalog default: medium (gpt-6.1-sol, Codex 0.164.0)");
    cleanup();
    // Without a reported catalog default the Codex option says so rather than guessing.
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings() });
    mount();
    await screen.findByLabelText("Worker effort — Codex");
    expect(runtimeText("Worker effort — Codex")).toBe("Runtime default · catalog default unknown until a Codex run reports it");
  });

  it("switching the default harness twice keeps both families' model choices", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_claude_model: "sonnet", default_codex_model: "gpt-6-sol" }) });
    mount();
    await screen.findByLabelText("Default harness");
    select("Default harness", "codex");
    select("Default harness", "claude");
    expect(control("Worker model — Claude").value).toBe("sonnet");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(status()).toBe("1 unsaved change: Default harness");
    select("Default harness", "inherit");
    expect(status()).toBe("No unsaved changes");
  });

  it("labels the planning Default as Same as the run, with the server's resolution", async () => {
    mount();
    await screen.findByLabelText("Planning model — Claude");
    expect(control("Planning model — Claude").options[0].text).toBe("Same as the run · opus (lead template)");
    expect(control("Planning model — Codex").options[0].text).toBe("Same as the run · gpt-6.2-sol (built-in)");
    expect(control("Planning effort — Codex").options[0].text).toBe("Same as the run · medium (built-in)");
  });

  it("pins each 400 error to its family and row, links it through aria-describedby, and keeps the draft", async () => {
    const errors = [
      { field: "default_codex_model", message: "default_codex_model: a Claude model is not valid for the Codex lane" },
      { field: "planning_claude_effort", message: "planning_claude_effort: effort must be one of low, medium, high, xhigh, max" },
    ];
    mockApi.putMyModelSettings.mockRejectedValue(
      new ApiError(400, errors[0].message, { error: errors[0].message, field: errors[0].field, errors }),
    );
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    select("Planning effort — Claude", "high");
    fireEvent.click(saveButton());
    const alert = await within(bar()).findByRole("alert");
    expect(alert.textContent).toContain("2 values need attention. Nothing was saved");
    expect(describedText("Worker model — Codex")).toContain(errors[0].message);
    expect(describedText("Planning effort — Claude")).toContain(errors[1].message);
    expect(describedText("Worker model — Claude")).not.toContain("not valid");
    expect(describedText("Planning effort — Codex")).not.toContain("effort must be");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(status()).toBe("2 unsaved changes: Worker model — Codex, Planning effort — Claude");
    // Editing the cell clears its error.
    select("Worker model — Codex", "gpt-6-astra");
    expect(describedText("Worker model — Codex")).not.toContain(errors[0].message);
  });

  it("falls back to the flat field when the 400 carries no errors array", async () => {
    mockApi.putMyModelSettings.mockRejectedValue(new ApiError(400, "default_harness: must be one of claude, codex", {
      error: "default_harness: must be one of claude, codex", field: "default_harness",
    }));
    mount();
    await screen.findByLabelText("Default harness");
    select("Default harness", "codex");
    fireEvent.click(saveButton());
    await within(bar()).findByRole("alert");
    expect(describedText("Default harness")).toContain("default_harness: must be one of claude, codex");
  });

  it("disables the form and both buttons while a save is pending, so Save cannot repeat", async () => {
    const pending = deferred<Awaited<ReturnType<typeof api.putMyModelSettings>>>();
    mockApi.putMyModelSettings.mockReturnValue(pending.promise);
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await waitFor(() => expect(saveButton().disabled).toBe(true));
    expect(saveButton().textContent).toBe("Saving…");
    expect(discardButton().disabled).toBe(true);
    expect((screen.getByRole("group", { name: "Model settings" }) as HTMLFieldSetElement).disabled).toBe(true);
    fireEvent.click(saveButton());
    fireEvent.submit(saveButton().closest("form")!);
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
    await act(async () => pending.resolve({ settings: makeSettings({ default_codex_model: "gpt-6-sol" }), effective: makeEffective() }));
    await waitFor(() => expect(status()).toBe("No unsaved changes"));
  });

  it("commit-unknown keeps the draft and Reload saved settings reconciles without retrying", async () => {
    mockApi.putMyModelSettings.mockRejectedValue(
      new ApiError(500, "the save may or may not have been applied", { reason: "settings_commit_unknown" }),
    );
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    select("Worker effort — Claude", "high");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(status()).toContain("2 unsaved changes");
    // The save had in fact committed the Codex model only.
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_codex_model: "gpt-6-sol" }) });
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await within(bar()).findByText("Reloaded the saved settings. 1 edit differs from what is stored.");
    expect(status()).toBe("1 unsaved change: Worker effort — Claude");
    expect(control("Worker effort — Claude").value).toBe("high");
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
  });

  it("sends an explicitly re-chosen baseline value after an unconfirmed save", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(new TypeError("connection lost"));
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    select("Worker model — Codex", "inherit");
    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenLastCalledWith({ default_codex_model: null }));
  });

  it("reload keeps the pending edit and adopts unrelated stored changes", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(new TypeError("connection lost"));
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_effort: "low" }) });
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await waitFor(() => expect(control("Worker effort — Claude").value).toBe("low"));
    await within(bar()).findByText("Reloaded the saved settings.");
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();
    expect(status()).toBe("1 unsaved change: Worker model — Codex");
    expect(within(bar()).getByRole("button", { name: "Reload saved settings" })).toBeTruthy();
  });

  // #4639: a save whose response was lost may still commit after any later request, so the
  // standing warning (with the page's only Reload button) outlives every later outcome.
  describe("a lost-response save keeps its standing warning", () => {
    const savedWith = { settings: makeSettings({ default_codex_model: "gpt-6-sol" }) };
    const reloadButtons = () => within(bar()).getAllByRole("button", { name: "Reload saved settings" });
    const expectStanding = () => {
      expect(within(bar()).getByTestId(WARNING).textContent).toContain(
        "An earlier save whose response was lost may still finish and replace the values shown here. Reloading shows a snapshot; it cannot confirm that request has finished.",
      );
      expect(reloadButtons()).toHaveLength(1);
    };
    async function loseResponse() {
      mockApi.putMyModelSettings.mockRejectedValueOnce(new TypeError("Failed to fetch"));
      mount();
      await screen.findByLabelText("Worker model — Codex");
      select("Worker model — Codex", "gpt-6-sol");
      fireEvent.click(saveButton());
      await within(bar()).findByText(/could not confirm whether this save was applied/);
      expectStanding();
    }

    it("a retry that saves without a prediction", async () => {
      await loseResponse();
      // No `effective`: the commit succeeded but its prediction could not be read.
      mockApi.putMyModelSettings.mockResolvedValueOnce(savedWith as Awaited<ReturnType<typeof api.putMyModelSettings>>);
      fireEvent.click(saveButton());
      await within(bar()).findByText(/Every change committed, but the new effective values could not be read/);
      expectStanding();
    });

    it("a retry whose readback failed, then a later edit", async () => {
      await loseResponse();
      mockApi.putMyModelSettings.mockRejectedValueOnce(
        new ApiError(500, "readback failed", { reason: "settings_saved_readback_failed" }),
      );
      fireEvent.click(saveButton());
      await within(bar()).findByText(/reading them back failed/);
      expectStanding();
      select("Worker effort — Claude", "high");
      expectStanding();
    });

    it("a 400 retry drops the unconfirmed suffix the standing warning replaces", async () => {
      await loseResponse();
      const message = "default_codex_model: not valid";
      mockApi.putMyModelSettings.mockRejectedValueOnce(
        new ApiError(400, message, { error: message, field: "default_codex_model" }),
      );
      fireEvent.click(saveButton());
      await within(bar()).findByText(/1 value needs attention/);
      expect(within(bar()).queryByText(/An earlier save is still unconfirmed/)).toBeNull();
      expectStanding();
    });

    it("a plain 500 retry", async () => {
      await loseResponse();
      mockApi.putMyModelSettings.mockRejectedValueOnce(new ApiError(500, "boom"));
      fireEvent.click(saveButton());
      await within(bar()).findByText(/The save failed and nothing was changed: boom/);
      expect(within(bar()).queryByText(/An earlier save is still unconfirmed/)).toBeNull();
      expectStanding();
    });

    it("a commit-unknown retry, then a successful one", async () => {
      await loseResponse();
      mockApi.putMyModelSettings.mockRejectedValueOnce(
        new ApiError(500, "unknown", { reason: "settings_commit_unknown" }),
      );
      fireEvent.click(saveButton());
      await within(bar()).findByText(/could not confirm whether this save was applied/);
      expectStanding();
      mockApi.putMyModelSettings.mockResolvedValueOnce({ ...savedWith, effective: makeEffective() });
      fireEvent.click(saveButton());
      await within(bar()).findByText("Saved 1 change.");
      expect(within(bar()).queryByText(/Your next runs use/)).toBeNull();
      expectStanding();
    });

    it("a successful retry keeps the lost request's keys: re-choosing the saved value is sent again", async () => {
      await loseResponse();
      mockApi.putMyModelSettings.mockResolvedValueOnce({ ...savedWith, effective: makeEffective() });
      fireEvent.click(saveButton());
      await within(bar()).findByText("Saved 1 change.");
      expect(status()).toBe("No unsaved changes");
      // The lost request may still overwrite this key, so an explicit re-choice of the value
      // the retry stored counts as a change and is sent again.
      select("Worker model — Codex", "inherit");
      select("Worker model — Codex", "gpt-6-sol");
      expect(status()).toBe("1 unsaved change: Worker model — Codex");
      mockApi.putMyModelSettings.mockResolvedValueOnce({ ...savedWith, effective: makeEffective() });
      fireEvent.click(saveButton());
      await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenLastCalledWith({ default_codex_model: "gpt-6-sol" }));
      await within(bar()).findByText("Saved 1 change.");
      expectStanding();
    });

    it("a failed Reload", async () => {
      await loseResponse();
      mockApi.getMySettings.mockRejectedValueOnce(new Error("offline"));
      fireEvent.click(reloadButtons()[0]);
      await within(bar()).findByText(/Could not reload the saved settings/);
      expectStanding();
    });

    it("negative control: commit-unknown alone is fully cleared by a successful retry", async () => {
      mockApi.putMyModelSettings.mockRejectedValueOnce(
        new ApiError(500, "unknown", { reason: "settings_commit_unknown" }),
      );
      mount();
      await screen.findByLabelText("Worker model — Codex");
      select("Worker model — Codex", "gpt-6-sol");
      fireEvent.click(saveButton());
      await within(bar()).findByText(/could not confirm whether this save was applied/);
      expect(within(bar()).queryByTestId(WARNING)).toBeNull();
      mockApi.putMyModelSettings.mockResolvedValueOnce({ ...savedWith, effective: makeEffective() });
      fireEvent.click(saveButton());
      await within(bar()).findByText("Saved 1 change. Your next runs use it.");
      expect(within(bar()).queryByTestId(WARNING)).toBeNull();
    });

    it("negative control: a plain save never shows the warning", async () => {
      mockApi.putMyModelSettings.mockResolvedValueOnce({ ...savedWith, effective: makeEffective() });
      mount();
      await screen.findByLabelText("Worker model — Codex");
      select("Worker model — Codex", "gpt-6-sol");
      fireEvent.click(saveButton());
      await within(bar()).findByText("Saved 1 change. Your next runs use it.");
      expect(within(bar()).queryByTestId(WARNING)).toBeNull();
    });
  });

  it("Discard after an unconfirmed save keeps the uncertainty and Reload, without claiming the stored values", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(
      new ApiError(500, "the save may or may not have been applied", { reason: "settings_commit_unknown" }),
    );
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    fireEvent.click(discardButton());
    expect(control("Worker model — Codex").value).toBe("inherit");
    // The alert no longer says the edits are still here; it says an earlier save may apply.
    expect(within(bar()).queryByText(/Your edits are still here/)).toBeNull();
    expect(within(bar()).getByText(/Your edits were discarded here, but an earlier save may still have been applied/)).toBeTruthy();
    // The save had in fact committed: Reload adopts the stored value instead of inventing a
    // pending "revert" edit from the discarded pre-save draft.
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_codex_model: "gpt-6-sol" }) });
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await within(bar()).findByText("Reloaded the saved settings. The page now shows what is stored.");
    expect(status()).toBe("No unsaved changes");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
  });

  it("a clean reload after Discard never claims the discarded edit was stored", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(
      new ApiError(500, "the save may or may not have been applied", { reason: "settings_commit_unknown" }),
    );
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    fireEvent.click(discardButton());
    // The save did NOT commit: the server still holds the pre-save value.
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await within(bar()).findByText("Reloaded the saved settings. The page now shows what is stored.");
    expect(within(bar()).queryByText(/They match your edits/)).toBeNull();
    expect(control("Worker model — Codex").value).toBe("inherit");
    expect(status()).toBe("No unsaved changes");
  });

  it("takes the worker effort Default from the template-aware effective cell, not the preference-only settings cell", async () => {
    const effective = makeEffective();
    effective.efforts = effective.efforts.map((c) =>
      c.setting === "worker_effort" && c.harness === "claude"
        ? { ...c, default_effort: "high", default_source: "template", resolved_effort: "high", source: "template" }
        : c,
    );
    mockApi.getEffectiveSettings.mockResolvedValue(effective);
    const preferenceOnly = (harness: "claude" | "codex") => ({
      harness, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit",
    });
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({
      effort_settings: [preferenceOnly("claude"), preferenceOnly("codex")] as UserSettings["effort_settings"],
    }) });
    mount();
    await screen.findByLabelText("Worker effort — Claude");
    // /me/settings' preference cell says built-in medium; the effective cell knows the lead template.
    expect(control("Worker effort — Claude").options[0].text).toBe("Default · high (lead template)");
  });

  it("says a saved-but-unreadable save was saved, and a plain 500 that nothing changed", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(
      new ApiError(500, "settings saved; reading them back failed", { reason: "settings_saved_readback_failed" }),
    );
    mockApi.putMyModelSettings.mockRejectedValueOnce(new ApiError(500, "internal error", { error: "internal error" }));
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/Your changes were saved, but reading them back failed/);
    expect(within(bar()).getByRole("button", { name: "Reload saved settings" })).toBeTruthy();
    fireEvent.click(saveButton());
    await within(bar()).findByText(/The save failed and nothing was changed: internal error/);
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
  });

  it("shows Unavailable when predictions cannot be read, on load and after a save without them", async () => {
    mockApi.getEffectiveSettings.mockRejectedValue(new ApiError(500, "boom"));
    mockApi.putMyModelSettings.mockResolvedValue({
      settings: makeSettings({ default_codex_model: "gpt-6-sol" }), effective: null, effective_error: "settings_effective_readback_failed",
    });
    mount();
    await screen.findByLabelText("Worker model — Codex");
    expect(describedText("Worker model — Claude")).toContain("Saved effective value: Unavailable");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/Saved\. Every change committed, but the new effective values could not be read/);
    expect(status()).toBe("No unsaved changes");
    expect(describedText("Worker model — Codex")).toContain("Saved effective value: Unavailable");
  });

  it("Discard restores the saved baseline", async () => {
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(discardButton());
    expect(control("Worker model — Codex").value).toBe("inherit");
    expect(status()).toBe("No unsaved changes");
  });

  it("blocks Save on an empty custom model ID instead of saving it as Default", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_claude_model: "opus" }) });
    mount();
    await screen.findByLabelText("Worker model — Claude");
    select("Worker model — Claude", "custom");
    expect(describedText("Worker model — Claude")).toContain("Enter a custom model ID, or choose Default.");
    expect(saveButton().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Worker model — Claude, custom model ID"), { target: { value: "claude-x" } });
    expect(saveButton().disabled).toBe(false);
  });

  it("blocks Save on an empty custom ID even when that cell is not dirty, and Discard leaves custom mode", async () => {
    // Stored Default (""), then Other with an empty box: the value is still "", so not dirty.
    mount();
    await screen.findByLabelText("Worker model — Claude");
    select("Worker model — Claude", "custom");
    select("Worker model — Codex", "gpt-6-sol");
    // Save is disabled by the empty box, and the bar says so after the count.
    expect(status()).toBe("1 unsaved change: Worker model — Codex · fix 1 before saving");
    expect(describedText("Worker model — Claude")).toContain("Enter a custom model ID, or choose Default.");
    // The errored cell's controls are aria-invalid; a clean neighbour's are not.
    expect(control("Worker model — Claude").getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByLabelText("Worker model — Claude, custom model ID").getAttribute("aria-invalid")).toBe("true");
    expect(control("Worker model — Codex").hasAttribute("aria-invalid")).toBe(false);
    expect(saveButton().disabled).toBe(true);
    fireEvent.click(discardButton());
    expect(control("Worker model — Claude").value).toBe("inherit");
    expect(screen.queryByLabelText("Worker model — Claude, custom model ID")).toBeNull();
    expect(describedText("Worker model — Claude")).not.toContain("Enter a custom model ID");
    expect(control("Worker model — Claude").hasAttribute("aria-invalid")).toBe(false);
    // With only the non-dirty empty custom entry left, the bar says what blocks Save rather
    // than "No unsaved changes", and Discard still clears it.
    select("Worker model — Claude", "custom");
    expect(status()).toBe("Fix 1 value before saving");
    expect(discardButton().disabled).toBe(false);
    fireEvent.click(discardButton());
    expect(control("Worker model — Claude").value).toBe("inherit");
  });

  it("a Save attempted while Reload is pending cannot leave Reload stuck", async () => {
    mockApi.putMyModelSettings.mockRejectedValue(new TypeError("Failed to fetch"));
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    const reloadPending = deferred<{ settings: UserSettings }>();
    mockApi.getMySettings.mockReturnValueOnce(reloadPending.promise);
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await waitFor(() => expect(within(bar()).getByRole("button", { name: "Reloading…" })).toBeTruthy());
    // Save while the reload is in flight: it must not start a request that outranks the reload.
    fireEvent.click(saveButton());
    await act(async () => {});
    await act(async () => reloadPending.reject(new TypeError("Failed to fetch")));
    const reload = await within(bar()).findByRole("button", { name: "Reload saved settings" });
    expect((reload as HTMLButtonElement).disabled).toBe(false);
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
  });
});

describe("viewer fencing", () => {
  it("ignores a delayed load that answers after the viewer changed", async () => {
    const first = deferred<{ settings: UserSettings }>();
    mockApi.getMySettings.mockReturnValueOnce(first.promise);
    mockApi.getMySettings.mockResolvedValueOnce({ settings: makeSettings({ default_codex_model: "gpt-6-astra" }) });
    mount();
    act(() => auth.set({ id: "u2" }));
    await waitFor(() => expect(control("Worker model — Codex").value).toBe("gpt-6-astra"));
    await act(async () => first.resolve({ settings: makeSettings({ default_codex_model: "gpt-6-sol" }) }));
    expect(control("Worker model — Codex").value).toBe("gpt-6-astra");
  });

  it("drops the previous viewer's draft and ignores their late save", async () => {
    const pending = deferred<Awaited<ReturnType<typeof api.putMyModelSettings>>>();
    mockApi.putMyModelSettings.mockReturnValue(pending.promise);
    mount();
    await screen.findByLabelText("Worker model — Codex");
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(saveButton());
    await waitFor(() => expect(saveButton().disabled).toBe(true));
    mockApi.getMySettings.mockResolvedValue({ settings: makeSettings({ default_codex_model: "gpt-6-astra" }) });
    act(() => auth.set({ id: "u2" }));
    await waitFor(() => expect(control("Worker model — Codex").value).toBe("gpt-6-astra"));
    expect(status()).toBe("No unsaved changes");
    await act(async () => pending.resolve({ settings: makeSettings({ default_codex_model: "gpt-6-sol" }), effective: makeEffective() }));
    expect(control("Worker model — Codex").value).toBe("gpt-6-astra");
    expect(within(bar()).queryByText(/Saved 1 change/)).toBeNull();
  });
});

// ── PRD #3263 M2: helper and cross-check cells in the same draft and the same one save ──────

const settingCell = (harness: "claude" | "codex", defaultModel: string, source = "builtin", model: string | null = null) => ({
  harness, model, default_model: defaultModel, default_source: source, resolved_model: model ?? defaultModel, source: model ? "user_default" : source,
});
const pinDto = (stage: "plan" | "code", harness: "claude" | "codex", over: Partial<CrossCheckPinDTO> = {}): CrossCheckPinDTO => ({
  stage, harness, model: null, effort: null,
  worker_default_model: harness === "claude" ? "custom-template-model" : "gpt-6.2-sol",
  resolved_model: harness === "claude" ? "custom-template-model" : "gpt-6.2-sol", resolved_effort: "high",
  model_source: "worker default", effort_source: "cross-check default", active: true, ...over,
});

function helperSettings(over: Partial<UserSettings> = {}): UserSettings {
  return makeSettings({
    judge_models: [settingCell("claude", "opus", "instance_default"), settingCell("codex", "gpt-6.2-sol")],
    summary_models: [settingCell("claude", "haiku"), settingCell("codex", "gpt-6-luna")],
    task_review_models: [settingCell("claude", ""), settingCell("codex", "gpt-6-sol")],
    task_review_efforts: [
      { harness: "claude", effort: null, default_effort: "runtime", default_source: "user_default", resolved_effort: null, source: "user_default", mode: "runtime_default" },
      { harness: "codex", effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit" },
    ],
    cross_check_pins: [pinDto("plan", "claude"), pinDto("plan", "codex"), pinDto("code", "claude"), pinDto("code", "codex")],
    ...over,
  });
}

function helperEffective(over: { judgeOn?: "claude" | "codex" | null; codexUsable?: boolean } = {}): EffectiveSettings {
  const base = makeEffective();
  const cell = (setting: string, harness: "claude" | "codex", resolved: string, source: string, notes: string[] = []) =>
    ({ ...modelCell("worker_model", harness, resolved, source), setting, notes }) as EffectiveSettings["models"][number];
  const xEffort = (setting: string, harness: "claude" | "codex") =>
    ({ ...effortCell("worker_effort", harness, "high", "cross_check_default"), setting }) as EffectiveSettings["efforts"][number];
  return {
    ...base,
    harnesses: base.harnesses.map((h) => (h.harness === "codex" ? { ...h, usable: over.codexUsable ?? false } : h)),
    models: [
      ...base.models,
      cell("judge_model", "claude", "opus", "instance_default", over.judgeOn === "codex" ? ["not_used"] : []),
      cell("judge_model", "codex", "gpt-6.2-sol", "builtin", over.judgeOn === "claude" ? ["not_used"] : []),
      cell("summary_model", "claude", "haiku", "builtin"),
      cell("summary_model", "codex", "gpt-6-luna", "builtin"),
      cell("task_review_model", "claude", "", "builtin", ["sdk_default"]),
      cell("task_review_model", "codex", "gpt-6-sol", "builtin"),
    ],
    efforts: [
      ...base.efforts,
      { ...effortCell("worker_effort", "claude", null, "user_default"), setting: "task_review_effort" } as EffectiveSettings["efforts"][number],
      { ...effortCell("worker_effort", "codex", "medium", "builtin"), setting: "task_review_effort" } as EffectiveSettings["efforts"][number],
    ],
    cross_check: {
      models: (["cross_check_plan", "cross_check_code"] as const).flatMap((s) => [
        cell(s, "claude", "custom-template-model", "cross_check_worker_default"),
        cell(s, "codex", "gpt-6.2-sol", "cross_check_worker_default"),
      ]),
      efforts: (["cross_check_plan", "cross_check_code"] as const).flatMap((s) => [xEffort(s, "claude"), xEffort(s, "codex")]),
    },
    judge_harness: over.judgeOn
      ? { value: over.judgeOn, source: "user_default", fixed_at: "create", notes: [] }
      : { value: null, source: "same_as_run", fixed_at: "create", notes: [] },
  };
}

const HELPER_NAMES = [
  "Judge runs on", "Judge model — Claude", "Judge model — Codex", "Summary model — Claude", "Summary model — Codex",
  "Task review model — Claude", "Task review model — Codex", "Task review effort — Claude", "Task review effort — Codex",
  "Plan cross-check model — Claude", "Plan cross-check model — Codex", "Plan cross-check effort — Claude", "Plan cross-check effort — Codex",
  "Code cross-check model — Claude", "Code cross-check model — Codex", "Code cross-check effort — Claude", "Code cross-check effort — Codex",
];
const cellOf = (name: string) => control(name).closest("[data-field]") as HTMLElement;
const options = (name: string) => Array.from(control(name).options).map((o) => o.value);

describe("Models page — helper and cross-check rows (M2)", () => {
  beforeEach(() => {
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings() });
    mockApi.getEffectiveSettings.mockResolvedValue(helperEffective());
  });

  it("names every helper and checker control <Row> — <Family> with its saved effective value, and Summaries run on is read-only", async () => {
    mount();
    await screen.findByLabelText("Judge runs on");
    for (const name of HELPER_NAMES) {
      expect(control(name).tagName, name).toBe("SELECT");
      expect(describedText(name), name).toContain("Saved effective value:");
    }
    expect(describedText("Judge runs on")).toContain("Saved effective value: Same as the run");
    expect(describedText("Judge model — Claude")).toContain("Saved effective value: opus · instance default");
    expect(describedText("Task review model — Claude")).toContain("Saved effective value: the SDK decides · built-in");
    expect(describedText("Task review effort — Claude")).toContain("Saved effective value: Runtime default · your choice");
    expect(describedText("Plan cross-check model — Claude")).toContain("Saved effective value: custom-template-model · worker default");
    expect(describedText("Code cross-check effort — Codex")).toContain("Saved effective value: high · cross-check default");
    // Summaries run on: a fact with no control, explaining the run's engine and credential.
    expect(screen.queryByLabelText("Summaries run on")).toBeNull();
    const summaries = screen.getByRole("heading", { name: "Summaries run on" }).closest("[data-row]") as HTMLElement;
    expect(within(summaries).queryByRole("combobox")).toBeNull();
    expect(summaries.textContent).toContain("Same as the run");
    expect(summaries.textContent).toContain("run’s own engine and credential");
    // A missing credential is a reason, never a disabled control.
    expect(describedText("Judge model — Codex")).toContain("No usable Codex credential");
    for (const name of HELPER_NAMES) expect(control(name).disabled, name).toBe(false);
  });

  it("one Save sends exactly one PUT carrying exactly the changed helper, worker and pin keys, and no other save runs", async () => {
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings(), effective: helperEffective() });
    mount();
    await screen.findByLabelText("Judge runs on");
    select("Worker model — Codex", "gpt-6-sol");
    select("Judge runs on", "codex");
    select("Judge model — Claude", "sonnet");
    select("Summary model — Codex", "gpt-6-luna");
    select("Summary model — Codex", "gpt-6-sol");
    select("Task review effort — Codex", "runtime");
    select("Code cross-check model — Codex", "gpt-6-astra");
    select("Plan cross-check effort — Claude", "max");
    expect(status()).toContain("7 unsaved changes");
    fireEvent.click(saveButton());
    await within(bar()).findByText("Saved 7 changes. Your next runs use them.");
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.putMyModelSettings.mock.calls[0][0]).toEqual({
      default_codex_model: "gpt-6-sol",
      judge_runs_on: "codex",
      judge_model: "sonnet",
      codex_summary_model: "gpt-6-sol",
      codex_task_review_effort: "runtime",
      cross_check_pins: [
        { stage: "plan", harness: "claude", effort: "max" },
        { stage: "code", harness: "codex", model: "gpt-6-astra" },
      ],
    });
    expect(mockApi.putMySettings).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /Save (judge|cross-check|Claude|Codex)/ })).toBeNull();
  });

  it("edits one cross-check member without touching its row, its stage sibling or the other family", async () => {
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings(), effective: helperEffective() });
    mount();
    await screen.findByLabelText("Plan cross-check effort — Claude");
    select("Plan cross-check effort — Claude", "low");
    expect(status()).toBe("1 unsaved change: Plan cross-check effort — Claude");
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ cross_check_pins: [{ stage: "plan", harness: "claude", effort: "low" }] });
  });

  it("dims the judge lane the DRAFT routing leaves unused, with a linked reason, and still saves its dirty value", async () => {
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings({ judge_runs_on: "claude" }), effective: helperEffective({ judgeOn: "claude" }) });
    mount();
    await screen.findByLabelText("Judge runs on");
    // Same as the run: both lanes are in play.
    expect(cellOf("Judge model — Claude").dataset.dimmed).toBeUndefined();
    expect(cellOf("Judge model — Codex").dataset.dimmed).toBeUndefined();
    expect(describedText("Judge runs on")).toContain("Claude runs use the Claude judge model, Codex runs the Codex model.");

    select("Judge runs on", "claude");
    expect(cellOf("Judge model — Codex").dataset.dimmed).toBe("true");
    expect(cellOf("Judge model — Claude").dataset.dimmed).toBeUndefined();
    const reasonId = (control("Judge model — Codex").getAttribute("aria-describedby") ?? "").split(" ").find((id) => id.endsWith("-not-used"))!;
    expect(document.getElementById(reasonId)?.textContent).toBe("Not used: the judge runs on Claude");
    expect(describedText("Judge runs on")).toContain("Every run is judged on Claude, so Codex runs get a Claude judge.");
    // Dimmed, never disabled: the dormant lane is still edited, validated and saved.
    expect(control("Judge model — Codex").disabled).toBe(false);
    select("Judge model — Codex", "gpt-6-astra");
    expect(describedText("Judge model — Codex")).toContain("Unsaved");
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ judge_runs_on: "claude", codex_judge_model: "gpt-6-astra" });

    // The saved routing now drives the dim; switching the draft to Codex moves it.
    await waitFor(() => expect(status()).toBe("No unsaved changes"));
    expect(describedText("Judge runs on")).toContain("Saved effective value: Claude · your setting");
    // The dimmed cell says Not used once: its own line, not the saved prediction's server note too.
    expect(describedText("Judge model — Codex")).toContain("Not used: the judge runs on Claude");
    expect(describedText("Judge model — Codex")).not.toContain("not used: the judge runs on the other harness");
    select("Judge runs on", "codex");
    // Undimmed, the saved prediction keeps the server's note (it describes the saved routing).
    expect(describedText("Judge model — Codex")).toContain("not used: the judge runs on the other harness");
    expect(describedText("Judge model — Codex")).not.toContain("Not used: the judge runs on");
    expect(cellOf("Judge model — Claude").dataset.dimmed).toBe("true");
    expect(describedText("Judge model — Claude")).toContain("Not used: the judge runs on Codex");
    expect(cellOf("Judge model — Codex").dataset.dimmed).toBeUndefined();
    select("Judge runs on", "");
    expect(cellOf("Judge model — Claude").dataset.dimmed).toBeUndefined();
  });

  it("validates a dirty value in a dimmed lane and blocks Save on its cell", async () => {
    mount();
    await screen.findByLabelText("Judge runs on");
    select("Judge runs on", "codex");
    select("Judge model — Claude", "custom");
    fireEvent.change(screen.getByLabelText("Judge model — Claude, custom model ID"), { target: { value: "gpt-6-sol" } });
    expect(cellOf("Judge model — Claude").dataset.dimmed).toBe("true");
    expect(describedText("Judge model — Claude")).toContain("Not a Claude model: gpt-6-sol is a Codex model");
    expect(control("Judge model — Claude").getAttribute("aria-invalid")).toBe("true");
    expect(saveButton().disabled).toBe(true);
    expect(status()).toBe("2 unsaved changes: Judge runs on, Judge model — Claude · fix 1 before saving");
  });

  it("says where the judge is skipped when the drafted harness has no usable credential, without disabling it", async () => {
    mount();
    await screen.findByLabelText("Judge runs on");
    expect(describedText("Judge runs on")).not.toContain("no usable credential");
    select("Judge runs on", "codex");
    expect(describedText("Judge runs on")).toContain("Codex has no usable credential, so the judge is skipped until you add one. You can still save this choice.");
    expect(control("Judge runs on").disabled).toBe(false);
  });

  it("pins helper and checker 400 errors to their cells by field and stage/harness/member", async () => {
    const errors = [
      { field: "codex_judge_model", message: "codex_judge_model: \"gpt-x\" is not a supported Codex model" },
      { field: "cross_check_pins", message: "cross_check_pins[code/claude].effort: invalid effort", stage: "code" as const, harness: "claude" as const, member: "effort" as const },
      { field: "task_review_effort", message: "task_review_effort: invalid effort" },
    ];
    mockApi.putMyModelSettings.mockRejectedValue(new ApiError(400, errors[0].message, { error: errors[0].message, field: errors[0].field, errors }));
    mount();
    await screen.findByLabelText("Judge runs on");
    select("Judge model — Codex", "gpt-6-astra");
    select("Code cross-check effort — Claude", "max");
    select("Task review effort — Claude", "high");
    fireEvent.click(saveButton());
    const alert = await within(bar()).findByRole("alert");
    expect(alert.textContent).toContain("3 values need attention. Nothing was saved");
    for (const [name, message] of [
      ["Judge model — Codex", errors[0].message],
      ["Code cross-check effort — Claude", errors[1].message],
      ["Task review effort — Claude", errors[2].message],
    ] as const) {
      const errorId = (control(name).getAttribute("aria-describedby") ?? "").split(" ").find((id) => id.endsWith("-error"))!;
      expect(document.getElementById(errorId)?.textContent, name).toBe(message);
      expect(control(name).getAttribute("aria-invalid"), name).toBe("true");
    }
    // The neighbours are untouched: the other stage, the other family, the other member.
    for (const name of ["Plan cross-check effort — Claude", "Code cross-check effort — Codex", "Code cross-check model — Claude", "Judge model — Claude"]) {
      expect(control(name).hasAttribute("aria-invalid"), name).toBe(false);
      expect(control(name).getAttribute("aria-describedby"), name).not.toContain("-error");
    }
    expect(control("Code cross-check effort — Claude").value).toBe("max");
  });

  it("keeps each catalog: curated Codex judge, Codex role models for summary and task review, custom IDs where allowed", async () => {
    mount();
    await screen.findByLabelText("Judge runs on");
    expect(options("Judge model — Codex")).toEqual(["inherit", ...CODEX_CURATED_MODELS]);
    expect(options("Summary model — Codex")).toEqual(["inherit", ...CODEX_ROLE_MODELS]);
    expect(options("Task review model — Codex")).toEqual(["inherit", ...CODEX_ROLE_MODELS]);
    for (const name of ["Judge model — Claude", "Summary model — Claude", "Task review model — Claude"]) {
      expect(options(name), name).toEqual(["inherit", "opus", "sonnet", "haiku", "fable", "custom"]);
    }
    for (const name of ["Plan cross-check model — Codex", "Code cross-check model — Claude", "Worker model — Codex"]) {
      expect(options(name), name).toContain("custom");
    }
  });

  it("accepts custom IDs for a checker and the Codex worker, trimmed, in the same save", async () => {
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings(), effective: helperEffective() });
    mount();
    await screen.findByLabelText("Judge runs on");
    select("Plan cross-check model — Codex", "custom");
    fireEvent.change(screen.getByLabelText("Plan cross-check model — Codex, custom model ID"), { target: { value: "  arbitrary-codex-id  " } });
    select("Worker model — Codex", "custom");
    fireEvent.change(screen.getByLabelText("Worker model — Codex, custom model ID"), { target: { value: "codex-preview-x" } });
    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({
      default_codex_model: "codex-preview-x",
      cross_check_pins: [{ stage: "plan", harness: "codex", model: "arbitrary-codex-id" }],
    });
  });

  it.each([
    ["Code cross-check model — Claude", "gpt-6.1-sol"],
    ["Code cross-check model — Codex", "sonnet"],
  ])("guards %s against the other family while accepting a custom ID", async (name, invalid) => {
    mount();
    await screen.findByLabelText(name);
    select(name, "custom");
    const custom = screen.getByLabelText(`${name}, custom model ID`);
    fireEvent.change(custom, { target: { value: invalid } });
    expect(describedText(name)).toContain("Choose a model from the checker's model family.");
    expect(control(name).getAttribute("aria-invalid")).toBe("true");
    expect(saveButton().disabled).toBe(true);
    fireEvent.change(custom, { target: { value: "custom-checker" } });
    expect(describedText(name)).not.toContain("model family");
    expect(saveButton().disabled).toBe(false);
  });

  it.each(["opus", "two words", "x".repeat(101), "bad‮", "�", "é".repeat(51), "bad﻿", "a\u0085b", "a\u0001b"])(
    "blocks the invalid checker model %j without sending anything",
    async (value) => {
      mount();
      await screen.findByLabelText("Plan cross-check model — Codex");
      select("Plan cross-check model — Codex", "custom");
      fireEvent.change(screen.getByLabelText("Plan cross-check model — Codex, custom model ID"), { target: { value } });
      expect(saveButton().disabled).toBe(true);
      fireEvent.submit(saveButton().closest("form")!);
      expect(mockApi.putMyModelSettings).not.toHaveBeenCalled();
    },
  );

  it("labels task-review effort Default as following the worker effort (incl. Runtime default) from the server cell", async () => {
    mount();
    await screen.findByLabelText("Task review effort — Claude");
    expect(control("Task review effort — Claude").options[0].text).toBe("Default · follows worker effort (Runtime default)");
    expect(control("Task review effort — Codex").options[0].text).toBe("Default · follows worker effort (medium)");
    expect(options("Task review effort — Claude")).toContain("runtime");
    expect(control("Task review model — Claude").options[0].text).toBe("Default · SDK default (built-in)");
    expect(control("Task review model — Codex").options[0].text).toBe("Default · gpt-6-sol (built-in)");
    expect(control("Judge model — Claude").options[0].text).toBe("Default · opus (instance default)");
  });

  it("keeps the checker effort Default at the cross-check default, with no Runtime default, whatever the worker effort is", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({ default_effort: "runtime", default_codex_effort: "max" }) });
    mount();
    await screen.findByLabelText("Plan cross-check effort — Claude");
    for (const name of ["Plan cross-check effort — Claude", "Plan cross-check effort — Codex", "Code cross-check effort — Claude", "Code cross-check effort — Codex"]) {
      expect(control(name).options[0].text, name).toBe("Default · high (cross-check default)");
      expect(options(name), name).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
    }
  });

  it("labels the checker model Default from the stored pin's worker default, and follows it after a worker save", async () => {
    const legacy = pinDto("code", "codex");
    delete (legacy as Partial<CrossCheckPinDTO>).worker_default_model;
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({
      cross_check_pins: [pinDto("plan", "claude", { worker_default_model: null }), pinDto("plan", "codex"), pinDto("code", "claude"), legacy],
    }) });
    mockApi.putMyModelSettings.mockResolvedValue({
      settings: helperSettings({ default_claude_model: "haiku", cross_check_pins: [pinDto("plan", "claude", { worker_default_model: "haiku" })] }),
      effective: helperEffective(),
    });
    mount();
    await screen.findByLabelText("Plan cross-check model — Claude");
    expect(control("Plan cross-check model — Claude").options[0].text).toBe("Default · SDK/account default");
    expect(control("Plan cross-check model — Codex").options[0].text).toBe("Default · gpt-6.2-sol (worker default)");
    expect(control("Code cross-check model — Claude").options[0].text).toBe("Default · custom-template-model (worker default)");
    expect(control("Code cross-check model — Codex").options[0].text).toBe("Default · Unavailable (worker default)");
    select("Worker model — Claude", "haiku");
    fireEvent.click(saveButton());
    await waitFor(() => expect(control("Plan cross-check model — Claude").options[0].text).toBe("Default · haiku (worker default)"));
    expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ default_claude_model: "haiku" });
  });

  it("shows Unavailable for every saved effective value when the effective view is unavailable, never a /me/settings guess", async () => {
    mockApi.getEffectiveSettings.mockRejectedValue(new ApiError(500, "boom"));
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({
      judge_runs_on: "codex",
      cross_check_pins: [pinDto("plan", "claude", { model: "sonnet", resolved_model: "sonnet", model_source: "pin", effort: "max", resolved_effort: "max", effort_source: "pin" })],
    }) });
    mount();
    await screen.findByLabelText("Judge runs on");
    for (const name of ["Worker model — Claude", ...HELPER_NAMES]) {
      expect(describedText(name), name).toContain("Saved effective value: Unavailable");
    }
    // The stored values still load into the controls; only the prediction is withheld.
    expect(control("Judge runs on").value).toBe("codex");
    expect(control("Plan cross-check model — Claude").value).toBe("sonnet");
  });

  it("reads a checker's saved effective value only from effective.cross_check, never from the stored pin", async () => {
    const effective = helperEffective();
    effective.cross_check.models = effective.cross_check.models.map((c) =>
      c.setting === "cross_check_plan" && c.harness === "claude" ? { ...c, resolved_model: "server-checker", source: "cross_check_pin" } : c,
    );
    // The code stage's Claude cells are missing from the response.
    effective.cross_check.models = effective.cross_check.models.filter((c) => !(c.setting === "cross_check_code" && c.harness === "claude"));
    effective.cross_check.efforts = effective.cross_check.efforts.filter((c) => !(c.setting === "cross_check_code" && c.harness === "claude"));
    mockApi.getEffectiveSettings.mockResolvedValue(effective);
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({
      cross_check_pins: [
        pinDto("plan", "claude", { model: "stale-pin", resolved_model: "stale-pin", model_source: "pin" }),
        pinDto("plan", "codex"),
        pinDto("code", "claude", { model: "sonnet", resolved_model: "sonnet", model_source: "pin", effort: "max", resolved_effort: "max", effort_source: "pin" }),
        pinDto("code", "codex"),
      ],
    }) });
    mount();
    await screen.findByLabelText("Plan cross-check model — Claude");
    expect(describedText("Plan cross-check model — Claude")).toContain("Saved effective value: server-checker · checker pin");
    expect(describedText("Plan cross-check model — Claude")).not.toContain("stale-pin");
    expect(describedText("Plan cross-check effort — Claude")).toContain("Saved effective value: high · cross-check default");
    expect(describedText("Code cross-check model — Claude")).toContain("Saved effective value: Unavailable");
    expect(describedText("Code cross-check effort — Claude")).toContain("Saved effective value: Unavailable");
  });

  it("shows Unavailable for a helper row the effective view has no cell for", async () => {
    const effective = helperEffective();
    effective.models = effective.models.filter((c) => c.setting !== "summary_model");
    mockApi.getEffectiveSettings.mockResolvedValue(effective);
    mount();
    await screen.findByLabelText("Summary model — Claude");
    expect(describedText("Summary model — Claude")).toContain("Saved effective value: Unavailable");
    expect(describedText("Summary model — Codex")).toContain("Saved effective value: Unavailable");
    expect(describedText("Judge model — Claude")).toContain("Saved effective value: opus");
  });

  it("flags a stored value its validator refuses as information, without blocking a save of something else", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({
      judge_model: "gpt-6-sol",
      cross_check_pins: [pinDto("plan", "claude"), pinDto("plan", "codex", { model: "sonnet" }), pinDto("code", "claude"), pinDto("code", "codex")],
    }) });
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings({ judge_model: "gpt-6-sol" }), effective: helperEffective() });
    mount();
    await screen.findByLabelText("Plan cross-check model — Codex");
    expect(describedText("Judge model — Claude")).toContain("The saved value is not valid here. Not a Claude model: gpt-6-sol is a Codex model");
    expect(describedText("Plan cross-check model — Codex")).toContain("The saved value is not valid here. Choose a model from the checker's model family.");
    expect(control("Judge model — Claude").hasAttribute("aria-invalid")).toBe(false);
    expect(status()).toBe("No unsaved changes");
    select("Task review effort — Claude", "high");
    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ task_review_effort: "high" }));
  });

  it.each([
    ["Worker model — Claude", "gpt-6-sol", "Not a Claude model: gpt-6-sol is a Codex model, so Claude runs skip it. Choose Default to clear it."],
    ["Worker model — Codex", "opus", "Not a Codex model: opus is a Claude model alias, so the Codex lane cannot use it. Choose Default to clear it."],
    ["Planning model — Codex", "sonnet", "Not a Codex model: sonnet is a Claude model alias, so the Codex lane cannot use it. Choose Same as the run to clear it."],
  ])("blocks Save on %s set to the other family's model %s, and a custom ID clears it", async (name, invalid, message) => {
    mount();
    await screen.findByLabelText(name);
    select(name, "custom");
    const custom = screen.getByLabelText(`${name}, custom model ID`);
    fireEvent.change(custom, { target: { value: invalid } });
    expect(describedText(name)).toContain(message);
    expect(control(name).getAttribute("aria-invalid")).toBe("true");
    expect(saveButton().disabled).toBe(true);
    fireEvent.submit(saveButton().closest("form")!);
    expect(mockApi.putMyModelSettings).not.toHaveBeenCalled();
    fireEvent.change(custom, { target: { value: "custom-lane-id" } });
    expect(describedText(name)).not.toContain("Not a ");
    expect(saveButton().disabled).toBe(false);
  });

  it("flags a stored cross-family worker or planning model as information, and Default clears it in the save", async () => {
    mockApi.getMySettings.mockResolvedValue({ settings: helperSettings({ default_claude_model: "gpt-6-sol", planning_codex_model: "opus" }) });
    mockApi.putMyModelSettings.mockResolvedValue({ settings: helperSettings({ planning_codex_model: "opus" }), effective: helperEffective() });
    mount();
    await screen.findByLabelText("Worker model — Claude");
    expect(describedText("Worker model — Claude")).toContain("The saved value is not valid here. Not a Claude model: gpt-6-sol is a Codex model, so Claude runs skip it.");
    expect(describedText("Planning model — Codex")).toContain("The saved value is not valid here. Not a Codex model: opus is a Claude model alias");
    expect(control("Worker model — Claude").hasAttribute("aria-invalid")).toBe(false);
    expect(status()).toBe("No unsaved changes");
    select("Worker model — Claude", "inherit");
    expect(describedText("Worker model — Claude")).not.toContain("The saved value is not valid here");
    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.putMyModelSettings).toHaveBeenCalledWith({ default_claude_model: null }));
  });

  it("a 400 on a retry after an unconfirmed save still says the earlier save is unconfirmed, and keeps Reload", async () => {
    mockApi.putMyModelSettings.mockRejectedValueOnce(new ApiError(500, "may or may not", { reason: "settings_commit_unknown" }));
    const errors = [{ field: "cross_check_pins", message: "cross_check_pins[plan/codex].model: invalid", stage: "plan" as const, harness: "codex" as const, member: "model" as const }];
    mockApi.putMyModelSettings.mockRejectedValueOnce(new ApiError(400, errors[0].message, { error: errors[0].message, field: "cross_check_pins", errors }));
    mount();
    await screen.findByLabelText("Plan cross-check model — Codex");
    select("Plan cross-check model — Codex", "gpt-6-astra");
    fireEvent.click(saveButton());
    await within(bar()).findByText(/could not confirm whether this save was applied/);
    fireEvent.click(saveButton());
    const alert = await within(bar()).findByRole("alert");
    expect(alert.textContent).toContain("1 value needs attention. Nothing was saved; your edits are still here. An earlier save is still unconfirmed; reload to see what is stored.");
    expect(within(bar()).getByRole("button", { name: "Reload saved settings" })).toBeTruthy();
    expect(describedText("Plan cross-check model — Codex")).toContain(errors[0].message);
    expect(mockApi.putMyModelSettings).toHaveBeenCalledTimes(2);
  });
});
