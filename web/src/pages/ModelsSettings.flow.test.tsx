// @vitest-environment jsdom
//
// PRD #3263 M1 and M2, end to end in jsdom: the REAL App (AppShell, sidebar, command menu, route
// guards, AuthProvider) in a memory DATA router, as main.tsx mounts it, against the stateful
// mockApi. Covers the settings flow (save, navigate away and back, invalid cell, injected write
// failure) and the unsaved-changes guard (tab, sidebar, command menu, back/forward, logout, a
// 401) plus the beforeunload listener lifecycle. Waits are event-driven only.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";

// The unauthorized handler AuthProvider registers, captured so a test can deliver a 401.
const captured = vi.hoisted(() => ({ onUnauthorized: null as null | (() => void) }));

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    MOCK_MODE: true,
    api: (await import("../mocks/mockApi")).mockApi,
    setUnauthorizedHandler: (handler: (() => void) | null) => {
      captured.onUnauthorized = handler;
      actual.setUnauthorizedHandler(handler);
    },
  };
});

import { appRoutes } from "../appRoutes";
import { mockApi } from "../mocks/mockApi";
import { failNextMockModelSettingsWrite } from "../mocks/mockApi/settings";
import { resetModuleListForTests } from "../modules/registry";

const DIALOG = "You have unsaved model changes";
const RESET = {
  default_harness: null, default_claude_model: null, default_codex_model: null, default_effort: null,
  default_codex_effort: null, planning_claude_model: null, planning_claude_effort: null,
  planning_codex_model: null, planning_codex_effort: null,
  judge_runs_on: null, judge_model: null, codex_judge_model: null, summary_model: null, codex_summary_model: null,
  task_review_model: null, codex_task_review_model: null, task_review_effort: null, codex_task_review_effort: null,
  cross_check_pins: (["plan", "code"] as const).flatMap((stage) =>
    (["claude", "codex"] as const).map((harness) => ({ stage, harness, model: null, effort: null }))),
};

// Records whether the guard dialog EVER appeared, so "no dialog" also means "no flash".
let dialogSeen = false;
let observer: MutationObserver | null = null;

function mountApp(path = "/settings/models") {
  const router = createMemoryRouter(
    appRoutes,
    { initialEntries: ["/schedules", path], initialIndex: 1 },
  );
  render(<RouterProvider router={router} />);
  return router;
}

const control = (name: string) => screen.getByLabelText(name) as HTMLSelectElement;
const select = (name: string, value: string) => fireEvent.change(control(name), { target: { value } });
const bar = () => screen.getByTestId("models-save-bar");
const status = () => screen.getByTestId("models-save-status").textContent ?? "";
const path = (router: ReturnType<typeof mountApp>) => router.state.location.pathname;
const dialog = () => screen.queryByRole("dialog", { name: DIALOG });

async function openModels(path?: string) {
  const router = mountApp(path);
  await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
  return router;
}

let routerErrors: string[] = [];
beforeEach(async () => {
  localStorage.clear();
  // appRoutes mounts ModulesProvider, which fetches /modules.json once per document (PRD
  // #4686): serve "no modules" instead of letting the request leave the test.
  vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("[]", { status: 200 }))));
  await mockApi.login("vlad@uzi.local", "x");
  await mockApi.putMyModelSettings(RESET);
  dialogSeen = false;
  observer = new MutationObserver(() => {
    if (document.querySelector(`[aria-label="${DIALOG}"]`)) dialogSeen = true;
  });
  observer.observe(document.body, { childList: true, subtree: true });
  routerErrors = [];
  const original = console.error;
  vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
    const text = args.map((a) => (a instanceof Error ? a.message : String(a))).join(" ");
    if (text.includes("Invalid blocker state transition")) routerErrors.push(text);
    original(...args);
  });
});
afterEach(() => {
  // A second blocker.proceed() lands in the router's error boundary (the page is replaced by
  // an error screen in the app), so any such error fails the test that caused it.
  const errors = routerErrors;
  routerErrors = [];
  observer?.disconnect();
  cleanup();
  vi.unstubAllGlobals();
  resetModuleListForTests();
  failNextMockModelSettingsWrite(null);
  vi.restoreAllMocks();
  expect(errors).toEqual([]);
});

describe("Models settings flow (stateful mock)", () => {
  it("saves several cells, clears dirty to the returned values, and reloads the same values after leaving", async () => {
    const router = await openModels();
    select("Default harness", "codex");
    select("Worker model — Codex", "gpt-6-sol");
    select("Planning effort — Claude", "high");
    select("Worker effort — Claude", "runtime");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText("Saved 4 changes. Your next runs use them.", {}, { timeout: 5000 });
    expect(status()).toBe("No unsaved changes");
    expect((await mockApi.getMySettings()).settings).toMatchObject({
      default_harness: "codex", default_codex_model: "gpt-6-sol", planning_claude_effort: "high", default_effort: "runtime",
    });

    // Clean, so leaving does not prompt; coming back loads the stored values.
    await act(() => router.navigate("/settings/appearance"));
    expect(path(router)).toBe("/settings/appearance");
    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    expect(control("Default harness").value).toBe("codex");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    expect(control("Planning effort — Claude").value).toBe("high");
    expect(control("Worker effort — Claude").value).toBe("runtime");
    expect(dialogSeen).toBe(false);
  }, 30000);

  it("an invalid cell saves nothing: the error is on its row and every field reloads at its saved value", async () => {
    await mockApi.putMyModelSettings({ default_codex_effort: "low" });
    const router = await openModels();
    select("Worker effort — Codex", "max");
    select("Worker model — Claude", "custom");
    fireEvent.change(screen.getByLabelText("Worker model — Claude, custom model ID"), { target: { value: "gpt-6-sol" } });
    // The Claude lane's own guard (the server's rejectCodexIDInClaudeLane) blocks Save locally.
    const save = within(bar()).getByRole("button", { name: "Save changes" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(save);
    const claude = control("Worker model — Claude");
    const errorId = (claude.getAttribute("aria-describedby") ?? "").split(" ").find((id) => id.endsWith("-error"))!;
    expect(document.getElementById(errorId)?.textContent).toContain("Not a Claude model: gpt-6-sol is a Codex model");
    expect(control("Worker effort — Codex").getAttribute("aria-describedby")).not.toContain("-error");
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_codex_effort: "low", default_claude_model: null });

    // Leave (discarding the draft) and come back: everything is at the previous saved values.
    await act(() => router.navigate("/settings/appearance"));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    expect(control("Worker effort — Codex").value).toBe("low");
    expect(control("Worker model — Claude").value).toBe("inherit");
  }, 30000);

  it("an injected write failure leaves every field at its saved value and keeps the draft", async () => {
    const router = await openModels();
    select("Default harness", "claude");
    select("Planning model — Codex", "gpt-6-astra");
    failNextMockModelSettingsWrite("write_failed");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/The save failed and nothing was changed/, {}, { timeout: 5000 });
    expect(control("Planning model — Codex").value).toBe("gpt-6-astra");
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_harness: null, planning_codex_model: null });
    await act(() => router.navigate("/settings/appearance"));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    expect(control("Default harness").value).toBe("inherit");
    expect(control("Planning model — Codex").value).toBe("inherit");
  }, 30000);
});

describe("Models settings flow: helper and cross-check cells (stateful mock)", () => {
  it("saves helper, checker and worker cells in one save, and shows them again after leaving", async () => {
    const put = vi.spyOn(mockApi, "putMyModelSettings");
    const putSettings = vi.spyOn(mockApi, "putMySettings");
    const router = await openModels();
    select("Judge runs on", "codex");
    select("Judge model — Claude", "sonnet");
    select("Summary model — Codex", "gpt-6-luna");
    select("Task review effort — Claude", "runtime");
    select("Plan cross-check effort — Codex", "max");
    select("Code cross-check model — Claude", "custom");
    fireEvent.change(screen.getByLabelText("Code cross-check model — Claude, custom model ID"), { target: { value: "custom-claude-checker" } });
    select("Worker effort — Codex", "low");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText("Saved 7 changes. Your next runs use them.", {}, { timeout: 5000 });
    expect(put).toHaveBeenCalledTimes(1);
    expect(putSettings).not.toHaveBeenCalled();
    const stored = (await mockApi.getMySettings()).settings;
    expect(stored).toMatchObject({
      judge_runs_on: "codex", judge_model: "sonnet", codex_summary_model: "gpt-6-luna",
      task_review_effort: "runtime", default_codex_effort: "low",
    });
    expect(stored.cross_check_pins?.find((p) => p.stage === "plan" && p.harness === "codex")).toMatchObject({ model: null, effort: "max" });
    expect(stored.cross_check_pins?.find((p) => p.stage === "code" && p.harness === "claude")).toMatchObject({ model: "custom-claude-checker", effort: null });

    await act(() => router.navigate("/settings/appearance"));
    expect(path(router)).toBe("/settings/appearance");
    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Judge runs on", {}, { timeout: 5000 });
    expect(control("Judge runs on").value).toBe("codex");
    expect(control("Judge model — Claude").value).toBe("sonnet");
    expect(control("Judge model — Claude").closest("[data-field]")?.getAttribute("data-dimmed")).toBe("true");
    expect(control("Summary model — Codex").value).toBe("gpt-6-luna");
    expect(control("Task review effort — Claude").value).toBe("runtime");
    expect(control("Plan cross-check effort — Codex").value).toBe("max");
    expect((screen.getByLabelText("Code cross-check model — Claude, custom model ID") as HTMLInputElement).value).toBe("custom-claude-checker");
    expect(control("Worker effort — Codex").value).toBe("low");
    expect(status()).toBe("No unsaved changes");
    expect(dialogSeen).toBe(false);
  }, 30000);

  it("an invalid helper cell saves nothing: the error is on its cell, and every helper, pin and worker value is unchanged", async () => {
    await mockApi.putMyModelSettings({ summary_model: "haiku", cross_check_pins: [{ stage: "code", harness: "codex", effort: "low" }] });
    const before = (await mockApi.getMySettings()).settings;
    const router = await openModels();
    select("Summary model — Claude", "sonnet");
    select("Code cross-check effort — Codex", "xhigh");
    select("Worker model — Codex", "gpt-6-sol");
    // The Codex task review lane is a closed list in the page, so drive the server-side refusal
    // with a stored value only the server checks: a Claude alias sent through the patch.
    const real = mockApi.putMyModelSettings.bind(mockApi);
    vi.spyOn(mockApi, "putMyModelSettings").mockImplementationOnce((patch) => real({ ...patch, codex_task_review_model: "opus" }));
    select("Task review model — Codex", "gpt-6-sol");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByRole("alert", {}, { timeout: 5000 });
    const errorId = (control("Task review model — Codex").getAttribute("aria-describedby") ?? "").split(" ").find((id) => id.endsWith("-error"))!;
    expect(document.getElementById(errorId)?.textContent).toContain("codex_task_review_model");
    expect(control("Task review model — Codex").getAttribute("aria-invalid")).toBe("true");
    expect(control("Summary model — Claude").hasAttribute("aria-invalid")).toBe(false);
    expect((await mockApi.getMySettings()).settings).toEqual(before);

    await act(() => router.navigate("/settings/appearance"));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Judge runs on", {}, { timeout: 5000 });
    expect(control("Summary model — Claude").value).toBe("haiku");
    expect(control("Code cross-check effort — Codex").value).toBe("low");
    expect(control("Worker model — Codex").value).toBe("inherit");
    expect(control("Task review model — Codex").value).toBe("inherit");
  }, 30000);
});

describe("unsaved-changes guard", () => {
  it("tab click: Stay keeps the draft, Discard changes navigates", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    // The settings tab comes first; the Cross-check group's note links to Run defaults too.
    const tab = () => screen.getAllByRole("link", { name: "Run defaults" })[0];
    fireEvent.click(tab());
    fireEvent.click(await screen.findByRole("button", { name: "Stay" }));
    await waitFor(() => expect(dialog()).toBeNull());
    expect(path(router)).toBe("/settings/models");
    expect(control("Worker model — Codex").value).toBe("gpt-6-sol");
    fireEvent.click(tab());
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/settings/run-defaults"));
  }, 30000);

  it("sidebar link: blocked until discarded", async () => {
    const router = await openModels();
    select("Worker effort — Claude", "high");
    fireEvent.click(screen.getAllByRole("link", { name: /^Schedules/ })[0]);
    expect(await screen.findByRole("dialog", { name: DIALOG })).toBeTruthy();
    expect(path(router)).toBe("/settings/models");
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/schedules"));
  }, 30000);

  it("command-menu navigation: Stay keeps the page", async () => {
    const router = await openModels();
    select("Planning effort — Codex", "low");
    fireEvent.keyDown(document.body, { key: "k", code: "KeyK", ctrlKey: true });
    const menu = await screen.findByRole("dialog", { name: "Command menu" });
    fireEvent.change(within(menu).getByRole("combobox"), { target: { value: "appearance" } });
    const option = await within(menu).findByRole("option", { name: /Settings › Appearance/ });
    fireEvent.click(option);
    fireEvent.click(await screen.findByRole("button", { name: "Stay" }));
    await waitFor(() => expect(dialog()).toBeNull());
    expect(path(router)).toBe("/settings/models");
    expect(control("Planning effort — Codex").value).toBe("low");
  }, 30000);

  it("browser back and forward are blocked until discarded", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    await act(() => router.navigate(-1));
    fireEvent.click(await screen.findByRole("button", { name: "Stay" }));
    await waitFor(() => expect(dialog()).toBeNull());
    expect(path(router)).toBe("/settings/models");
    await act(() => router.navigate(-1));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/schedules"));
    // Forward again: the discarded page loads clean and does not prompt on leaving.
    await act(() => router.navigate(1));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    expect(status()).toBe("No unsaved changes");
  }, 30000);

  it("does not prompt on a clean page, after a save, or after Discard", async () => {
    const router = await openModels();
    fireEvent.click(screen.getByRole("link", { name: "Appearance" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));

    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/Saved 1 change/, {}, { timeout: 5000 });
    fireEvent.click(screen.getByRole("link", { name: "Appearance" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));

    await act(() => router.navigate("/settings/models"));
    await screen.findByLabelText("Worker model — Codex", {}, { timeout: 5000 });
    select("Worker model — Codex", "gpt-6-astra");
    fireEvent.click(within(bar()).getByRole("button", { name: "Discard" }));
    fireEvent.click(screen.getByRole("link", { name: "Appearance" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    expect(dialogSeen).toBe(false);
  }, 30000);

  it("adds the beforeunload listener only while dirty and removes it on discard and save", async () => {
    const add = vi.spyOn(window, "addEventListener");
    const remove = vi.spyOn(window, "removeEventListener");
    const unloads = (spy: typeof add) => spy.mock.calls.filter(([type]) => type === "beforeunload").map(([, fn]) => fn);
    await openModels();
    expect(unloads(add)).toHaveLength(0);
    select("Worker model — Codex", "gpt-6-sol");
    expect(unloads(add)).toHaveLength(1);
    const listener = unloads(add)[0] as (e: Event) => void;
    const event = new Event("beforeunload", { cancelable: true });
    listener(event);
    expect(event.defaultPrevented).toBe(true);
    fireEvent.click(within(bar()).getByRole("button", { name: "Discard" }));
    expect(unloads(remove)).toContain(listener);

    select("Worker model — Codex", "gpt-6-astra");
    expect(unloads(add)).toHaveLength(2);
    const second = unloads(add)[1];
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/Saved 1 change/, {}, { timeout: 5000 });
    expect(unloads(remove)).toContain(second);
    expect(unloads(add)).toHaveLength(2);
  }, 30000);

  // Discarding while the PUT is in flight could not stop it committing the "discarded" values,
  // so the dialog waits: Discard is disabled while saving, Stay still works, a successful save
  // proceeds on its own (nothing is left dirty), and a failed save re-enables Discard.
  function deferNextSave() {
    const real = mockApi.putMyModelSettings.bind(mockApi);
    let release: () => void = () => {};
    vi.spyOn(mockApi, "putMyModelSettings").mockImplementationOnce(
      (patch) => new Promise((resolve) => { release = () => resolve(real(patch)); }),
    );
    return () => release();
  }

  it("while a save is in flight the dialog waits; a successful save then proceeds with the saved values", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    const release = deferNextSave();
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: DIALOG });
    expect(open.textContent).toContain("Saving 1 change");
    const discard = within(open).getByRole("button", { name: "Discard changes" }) as HTMLButtonElement;
    expect(discard.disabled).toBe(true);
    fireEvent.click(discard);
    expect(path(router)).toBe("/settings/models");
    // Stay still works while saving.
    fireEvent.click(within(open).getByRole("button", { name: "Stay" }));
    await waitFor(() => expect(dialog()).toBeNull());
    await act(() => router.navigate("/settings/appearance"));
    await screen.findByRole("dialog", { name: DIALOG });
    await act(async () => release());
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"), { timeout: 5000 });
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_codex_model: "gpt-6-sol" });
  }, 30000);

  it("a save that fails while the dialog is open re-enables Discard, which then discards and proceeds", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    const release = deferNextSave();
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: DIALOG });
    expect((within(open).getByRole("button", { name: "Discard changes" }) as HTMLButtonElement).disabled).toBe(true);
    failNextMockModelSettingsWrite("write_failed");
    await act(async () => release());
    await waitFor(() =>
      expect((within(open).getByRole("button", { name: "Discard changes" }) as HTMLButtonElement).disabled).toBe(false),
    );
    expect(open.textContent).toContain("1 change is not saved yet");
    fireEvent.click(within(open).getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_codex_model: null });
  }, 30000);

  // #4639: a save whose response was lost may still commit after anything the page does next,
  // so the standing warning survives a successful retry, Reload, an edit, the leave dialog,
  // Discard and a later save.
  it("a lost-response save keeps its warning through retry, Reload, edit, leave dialog, Discard and a later save", async () => {
    const WARNING = "models-earlier-request-warning";
    const put = vi.spyOn(mockApi, "putMyModelSettings").mockRejectedValueOnce(new TypeError("Failed to fetch"));
    const reloadButtons = () => within(bar()).getAllByRole("button", { name: "Reload saved settings" });
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/could not confirm whether this save was applied/, {}, { timeout: 5000 });
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();

    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText("Saved 1 change.", {}, { timeout: 5000 });
    expect(within(bar()).queryByText(/Your next runs/)).toBeNull();
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();

    fireEvent.click(reloadButtons()[0]);
    await within(bar()).findByText("Reloaded the saved settings.", {}, { timeout: 5000 });
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();
    expect(reloadButtons()).toHaveLength(1);

    select("Worker effort — Claude", "high");
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();

    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: "An earlier model settings save may still finish" });
    expect(within(open).queryByRole("button", { name: "Discard changes" })).toBeNull();
    fireEvent.click(within(open).getByRole("button", { name: "Stay" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(path(router)).toBe("/settings/models");

    fireEvent.click(within(bar()).getByRole("button", { name: "Discard" }));
    await within(bar()).findByText(/Your edits were discarded here/);
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();

    const before = put.mock.calls.length;
    select("Worker effort — Claude", "low");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText("Saved 1 change.", {}, { timeout: 5000 });
    expect(put.mock.calls.length).toBe(before + 1);
    expect(within(bar()).getByTestId(WARNING)).toBeTruthy();
  }, 30000);

  // A save that may have committed keeps the draft dirty, but leaving is then not a discard:
  // the dialog says the changes may already be saved, recommends Stay + Reload saved settings,
  // and its leave button makes no "Discard changes" (rollback) claim.
  const UNCONFIRMED = "Your model changes may already be saved";
  const expectUncertainCopy = (open: HTMLElement) => {
    expect(open.textContent).toContain("1 change may already be saved");
    expect(open.textContent).toContain("Reload saved settings");
    expect(open.textContent).not.toContain("not saved yet");
    expect(open.textContent).not.toContain("discard and continue");
    expect(open.textContent).not.toContain("You continue once the save finishes");
    expect(within(open).queryByRole("button", { name: "Discard changes" })).toBeNull();
    expect((within(open).getByRole("button", { name: "Leave anyway" }) as HTMLButtonElement).disabled).toBe(false);
  };

  it("a readback failure (saved, not confirmed) shows the uncertain dialog; Leave anyway leaves with the stored values", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    const release = deferNextSave();
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await act(() => router.navigate("/settings/appearance"));
    const saving = await screen.findByRole("dialog", { name: DIALOG });
    expect(saving.textContent).toContain("You continue once the save finishes");
    failNextMockModelSettingsWrite("saved_readback_failed");
    await act(async () => release());
    const open = await screen.findByRole("dialog", { name: UNCONFIRMED }, { timeout: 5000 });
    expectUncertainCopy(open);
    expect(open.textContent).toContain("An earlier save was stored");
    fireEvent.click(within(open).getByRole("button", { name: "Leave anyway" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_codex_model: "gpt-6-sol" });
  }, 30000);

  it("an unknown commit outcome shows the uncertain dialog when it opens later; Stay then Reload restores the normal guard", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    failNextMockModelSettingsWrite("commit_unknown");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/could not confirm whether this save was applied/, {}, { timeout: 5000 });
    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: UNCONFIRMED });
    expectUncertainCopy(open);
    expect(open.textContent).toContain("could not confirm whether an earlier save was applied");
    fireEvent.click(within(open).getByRole("button", { name: "Stay" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: UNCONFIRMED })).toBeNull());
    expect(path(router)).toBe("/settings/models");

    // The mock did not apply it: after Reload the edit is known to be unsaved, so the normal
    // "not saved yet … Discard changes" guard is honest again.
    fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
    await within(bar()).findByText(/Reloaded the saved settings/, {}, { timeout: 5000 });
    await act(() => router.navigate("/settings/appearance"));
    const normal = await screen.findByRole("dialog", { name: DIALOG });
    expect(normal.textContent).toContain("1 change is not saved yet");
    fireEvent.click(within(normal).getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(path(router)).toBe("/settings/appearance"));
    expect((await mockApi.getMySettings()).settings).toMatchObject({ default_codex_model: null });
  }, 30000);

  it("a failed Reload after an unknown commit keeps the uncertain dialog (no Discard claim)", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    failNextMockModelSettingsWrite("commit_unknown");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/could not confirm whether this save was applied/, {}, { timeout: 5000 });
    const spy = vi.spyOn(mockApi, "getMySettings").mockRejectedValueOnce(new Error("offline"));
    try {
      fireEvent.click(within(bar()).getByRole("button", { name: "Reload saved settings" }));
      await waitFor(() => expect(spy).toHaveBeenCalled());
      await waitFor(() => expect(within(bar()).queryByText(/Reloading/)).toBeNull(), { timeout: 5000 });
    } finally {
      spy.mockRestore();
    }
    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: UNCONFIRMED });
    expectUncertainCopy(open);
    expect(within(open).queryByRole("button", { name: "Discard changes" })).toBeNull();
  }, 30000);

  it("a failed retry after an unknown commit keeps Reload reachable and the guard uncertain", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    failNextMockModelSettingsWrite("commit_unknown");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/could not confirm whether this save was applied/, {}, { timeout: 5000 });
    failNextMockModelSettingsWrite("write_failed");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save changes" }));
    await within(bar()).findByText(/The save failed and nothing was changed/, {}, { timeout: 5000 });
    expect(within(bar()).getByRole("button", { name: "Reload saved settings" })).toBeTruthy();
    await act(() => router.navigate("/settings/appearance"));
    const open = await screen.findByRole("dialog", { name: UNCONFIRMED });
    expect(open.textContent).toContain("could not confirm whether an earlier save was applied");
    expect(within(open).queryByRole("button", { name: "Discard changes" })).toBeNull();
  }, 30000);

  it("logout while dirty reaches /login without the dialog", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    fireEvent.click(screen.getAllByRole("button", { name: "Log out" })[0]);
    await waitFor(() => expect(path(router)).toBe("/login"), { timeout: 5000 });
    expect(dialogSeen).toBe(false);
  }, 30000);

  it("a 401 while dirty does not trap the user in the old viewer's form", async () => {
    const router = await openModels();
    select("Worker model — Codex", "gpt-6-sol");
    expect(captured.onUnauthorized).not.toBeNull();
    await mockApi.logout();
    act(() => captured.onUnauthorized!());
    await waitFor(() => expect(path(router)).toBe("/login"), { timeout: 5000 });
    expect(screen.queryByLabelText("Worker model — Codex")).toBeNull();
    expect(dialogSeen).toBe(false);
  }, 30000);
});

it("renders the production root error element for an unexpected route error", async () => {
  const router = createMemoryRouter(
    [{ ...appRoutes[0], loader: () => { throw new Error("route fixture failed"); } }],
    { initialEntries: ["/settings/models"] },
  );
  render(<RouterProvider router={router} />);
  expect(await screen.findByRole("heading", { name: "Something went wrong" })).toBeTruthy();
  expect(screen.getByRole("alert").textContent).toContain("route fixture failed");
  expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
});
