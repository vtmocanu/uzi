// @vitest-environment jsdom
//
// The guard's own rules, isolated from the page (PRD #3263 example 6): it blocks a dirty
// viewer's route change, but never /login, never a same-path change, and never once the live
// viewer is gone or different. The viewer is read from the latest render, so a logout's
// navigate("/login"), which can reach the router before that render, is covered by the /login
// rule.
import { afterEach, describe, expect, it, vi } from "vitest";
import { useEffect, useState } from "react";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { UnsavedModelsGuard } from "./UnsavedModelsGuard";

const auth = vi.hoisted(() => ({ user: { id: "u1" } as { id: string } | null }));
vi.mock("../../auth/AuthContext", () => ({ useAuth: () => ({ user: auth.user }) }));

const DIALOG = "You have unsaved model changes";

function mount(dirtyCount: number) {
  const onDiscard = vi.fn();
  const router = createMemoryRouter(
    [
      { path: "/settings/models", element: <UnsavedModelsGuard dirtyCount={dirtyCount} viewerId="u1" onDiscard={onDiscard} /> },
      { path: "*", element: <p>elsewhere</p> },
    ],
    { initialEntries: ["/settings/models"] },
  );
  render(<RouterProvider router={router} />);
  return { router, onDiscard };
}

afterEach(() => {
  cleanup();
  auth.user = { id: "u1" };
});

describe("UnsavedModelsGuard", () => {
  it("blocks a dirty viewer's route change; Discard changes discards, then proceeds", async () => {
    const { router, onDiscard } = mount(2);
    await act(() => router.navigate("/settings/run-defaults"));
    expect(router.state.location.pathname).toBe("/settings/models");
    expect(screen.getByRole("dialog", { name: DIALOG }).textContent).toContain("2 changes are not saved yet");
    act(() => screen.getByRole("button", { name: "Discard changes" }).click());
    expect(onDiscard).toHaveBeenCalledTimes(1);
    expect(router.state.location.pathname).toBe("/settings/run-defaults");
  });

  it("proceeds when the draft stops being dirty while the dialog is open (a pending save finished)", async () => {
    const onDiscard = vi.fn();
    const host: { setDirty?: (n: number) => void } = {};
    function Host() {
      const [n, set] = useState(1);
      useEffect(() => {
        host.setDirty = set;
      }, []);
      return <UnsavedModelsGuard dirtyCount={n} viewerId="u1" onDiscard={onDiscard} />;
    }
    const router = createMemoryRouter(
      [
        { path: "/settings/models", element: <Host /> },
        { path: "*", element: <p>elsewhere</p> },
      ],
      { initialEntries: ["/settings/models"] },
    );
    render(<RouterProvider router={router} />);
    await act(() => router.navigate("/schedules"));
    expect(screen.getByRole("dialog", { name: DIALOG })).toBeTruthy();
    await act(async () => host.setDirty!(0));
    await waitFor(() => expect(router.state.location.pathname).toBe("/schedules"));
    expect(screen.queryByText(/0 changes/)).toBeNull();
    expect(onDiscard).not.toHaveBeenCalled();
  });

  it.each([
    ["unknown", "could not confirm whether an earlier save was applied"],
    ["saved_unreadable", "An earlier save was stored"],
  ] as const)("after a %s save, leaving is Leave anyway, never a discard claim", async (kind, copy) => {
    const onDiscard = vi.fn();
    const router = createMemoryRouter(
      [
        {
          path: "/settings/models",
          element: <UnsavedModelsGuard dirtyCount={2} saveUncertainty={kind} viewerId="u1" onDiscard={onDiscard} />,
        },
        { path: "*", element: <p>elsewhere</p> },
      ],
      { initialEntries: ["/settings/models"] },
    );
    render(<RouterProvider router={router} />);
    await act(() => router.navigate("/schedules"));
    const open = screen.getByRole("dialog", { name: "Your model changes may already be saved" });
    expect(open.textContent).toContain(copy);
    expect(open.textContent).toContain("2 changes may already be saved");
    expect(open.textContent).not.toMatch(/not saved yet|discard and continue|continue once the save finishes/);
    expect(screen.queryByRole("button", { name: "Discard changes" })).toBeNull();
    act(() => screen.getByRole("button", { name: "Leave anyway" }).click());
    expect(router.state.location.pathname).toBe("/schedules");
  });

  it("a lost-response request that may still finish gets its own title and copy, and no discard", async () => {
    const onDiscard = vi.fn();
    const router = createMemoryRouter(
      [
        {
          path: "/settings/models",
          element: (
            <UnsavedModelsGuard dirtyCount={2} saveUncertainty="unknown" requestMayStillFinish viewerId="u1" onDiscard={onDiscard} />
          ),
        },
        { path: "*", element: <p>elsewhere</p> },
      ],
      { initialEntries: ["/settings/models"] },
    );
    render(<RouterProvider router={router} />);
    await act(() => router.navigate("/schedules"));
    const open = screen.getByRole("dialog", { name: "An earlier model settings save may still finish" });
    expect(open.textContent).toContain(
      "An earlier save whose response was lost may still finish and change what is stored. Stay and choose Reload saved settings to see what is stored. Leaving does not undo anything the server stores.",
    );
    expect(screen.queryByRole("button", { name: "Discard changes" })).toBeNull();
    act(() => screen.getByRole("button", { name: "Leave anyway" }).click());
    expect(router.state.location.pathname).toBe("/schedules");
  });

  it("while saving, the saving copy wins over the may-still-finish copy", async () => {
    const router = createMemoryRouter(
      [
        {
          path: "/settings/models",
          element: (
            <UnsavedModelsGuard dirtyCount={2} saving saveUncertainty="unknown" requestMayStillFinish viewerId="u1" onDiscard={vi.fn()} />
          ),
        },
        { path: "*", element: <p>elsewhere</p> },
      ],
      { initialEntries: ["/settings/models"] },
    );
    render(<RouterProvider router={router} />);
    await act(() => router.navigate("/schedules"));
    const open = screen.getByRole("dialog", { name: DIALOG });
    expect(open.textContent).toContain("Saving 2 changes");
    expect(open.textContent).not.toContain("may still finish");
  });

  it("does not block /login, a same-path change, or a clean page", async () => {
    const dirty = mount(1);
    await act(() => dirty.router.navigate("/settings/models?tab=x#planning"));
    expect(dirty.router.state.location.search).toBe("?tab=x");
    await act(() => dirty.router.navigate("/login"));
    expect(dirty.router.state.location.pathname).toBe("/login");
    cleanup();
    const clean = mount(0);
    await act(() => clean.router.navigate("/schedules"));
    expect(clean.router.state.location.pathname).toBe("/schedules");
    expect(screen.queryByRole("dialog", { name: DIALOG })).toBeNull();
  });

  it.each([["signed out", null], ["another viewer", { id: "u2" }]])(
    "does not block once the live viewer is %s, even while still mounted",
    async (_label, next) => {
      const { router } = mount(1);
      auth.user = next;
      // A same-path change re-renders the guard with the new auth state (and is never blocked).
      await act(() => router.navigate("/settings/models?refresh=1"));
      await act(() => router.navigate("/schedules"));
      expect(router.state.location.pathname).toBe("/schedules");
      expect(screen.queryByRole("dialog", { name: DIALOG })).toBeNull();
    },
  );
});
