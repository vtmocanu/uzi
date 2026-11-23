// @vitest-environment jsdom
//
// PRD #3263 M1: the Models page against a stubbed API and a controllable viewer. One draft,
// one save request, server errors pinned to their cells, saved-vs-draft wording, reset
// semantics, viewer fencing of late responses, failure reconciliation and pending-save
// disabling. Every wait is event-driven (findBy/waitFor on rendered state), never a sleep.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { api, ApiError, type EffectiveSettings, type UserSettings } from "../lib/api";
import { ModelsSettings } from "./ModelsSettings";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { getMySettings: vi.fn(), getEffectiveSettings: vi.fn(), putMyModelSettings: vi.fn() },
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
      { harness: "codex", model: null, default_model: "gpt-6.1-sol", default_source: "builtin", resolved_model: "gpt-6.1-sol", source: "builtin" },
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
      modelCell("worker_model", "codex", over.codexModel ?? "gpt-6.1-sol", over.codexSource ?? "builtin"),
      modelCell("planning_model", "claude", "opus", "template"),
      modelCell("planning_model", "codex", "gpt-6.1-sol", "builtin"),
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
    expect(text).toContain("Saved effective value: gpt-6.1-sol · built-in");
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
    expect(control("Planning model — Codex").options[0].text).toBe("Same as the run · gpt-6.1-sol (built-in)");
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
