// @vitest-environment jsdom
//
// A Retry that is still in flight when its card unmounts (PRD #4686, "Failure isolation"):
// the retry is ONE shared promise for the whole of re-registration plus load, so a remount
// during either phase joins it instead of starting an independent load, and its transient
// failure is recorded per document so the new card offers Reload page next to Retry.
//
// This file mocks ./federationEntry (not ./federation): the federationEntry wrapper discards
// the return value of the federation module's reRegisterModule, so only a mock at this
// boundary can hold re-registration open.
import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const ent = vi.hoisted(() => ({
  reRegisterModule: vi.fn(),
  loadModuleRemote: vi.fn(),
  registerModuleRemotes: vi.fn(),
}));
vi.mock("./federationEntry", () => ent);

vi.mock("../auth/AuthContext", () => ({
  useAuth: () => ({
    user: { id: "u1", email: "a@b.c", display_name: "Ada", is_admin: true },
  }),
}));

import { ModuleLoader } from "./ModuleLoader";
import { resetModuleLoaderForTests } from "./loader";
import type { ModuleEntry } from "./manifest";

const entry: ModuleEntry = {
  id: "demo",
  version: "4.5.6",
  page: "Page",
  label: "Demo",
  icon: "package",
  navGroup: "work",
  adminOnly: false,
};

const mount = (ui: ReactElement = <ModuleLoader entry={entry} />) => render(<MemoryRouter>{ui}</MemoryRouter>);
const retry = () => screen.queryByRole("button", { name: "Retry" });
const reload = () => screen.queryByRole("button", { name: "Reload page" });
const down = () => new TypeError("Failed to fetch");

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// First load fails transient and shows Retry; returns the mounted tree.
async function firstFailure() {
  ent.loadModuleRemote.mockRejectedValueOnce(down());
  const view = mount();
  await waitFor(() => expect(retry()).not.toBeNull());
  expect(reload()).toBeNull();
  expect(ent.loadModuleRemote).toHaveBeenCalledTimes(1);
  return view;
}

beforeEach(() => {
  resetModuleLoaderForTests();
  ent.reRegisterModule.mockReset();
  ent.loadModuleRemote.mockReset();
  ent.registerModuleRemotes.mockReset();
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("navigating away and back while a Retry is pending", () => {
  it("a remount during re-registration joins the retry, which then fails transient: Reload page and Retry", async () => {
    const view = await firstFailure();
    const hold = deferred<void>();
    ent.reRegisterModule.mockReturnValue(hold.promise);
    ent.loadModuleRemote.mockRejectedValue(down());

    fireEvent.click(retry()!);
    expect(ent.reRegisterModule).toHaveBeenCalledTimes(1);
    view.unmount();
    mount();

    // The new mount must not start its own load while the retry is re-registering.
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("status")).toBeTruthy();

    hold.resolve();
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(2);
  });

  it("a remount during re-registration joins the retry, whose re-registration then rejects: Reload page and Retry", async () => {
    const view = await firstFailure();
    const hold = deferred<void>();
    ent.reRegisterModule.mockReturnValue(hold.promise);

    fireEvent.click(retry()!);
    view.unmount();
    mount();
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(1);

    hold.reject(new Error("registerRemotes exploded"));
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(1);
  });

  it("a remount while the retry's load is in flight joins it, and its transient failure shows Reload page and Retry", async () => {
    const view = await firstFailure();
    const hold = deferred<unknown>();
    ent.reRegisterModule.mockResolvedValue(undefined);
    ent.loadModuleRemote.mockReturnValueOnce(hold.promise);

    fireEvent.click(retry()!);
    await waitFor(() => expect(ent.loadModuleRemote).toHaveBeenCalledTimes(2));
    view.unmount();
    mount();

    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("status")).toBeTruthy();

    hold.reject(down());
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(2);
  });
});

describe("a Retry whose re-registration never settles", () => {
  afterEach(() => {
    delete (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__;
  });

  it("is bounded by the load timeout, so a joined remount ends in the card, not a spinner", async () => {
    await firstFailure();
    (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__ = 30;
    ent.reRegisterModule.mockReturnValue(new Promise(() => {}));
    fireEvent.click(retry()!);
    cleanup();
    mount();
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
    expect(screen.getByTestId("module-error-reason").textContent).toContain("Timed out after 30 ms");
    expect(ent.loadModuleRemote).toHaveBeenCalledTimes(1);
  });
});
