// @vitest-environment jsdom
//
// The guard's own rules, isolated from the page (PRD #3263 example 6): it blocks a dirty
// viewer's route change, but never /login, never a same-path change, and never once the live
// viewer is gone or different. The viewer is read from the latest render, so a logout's
// navigate("/login"), which can reach the router before that render, is covered by the /login
// rule.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
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
