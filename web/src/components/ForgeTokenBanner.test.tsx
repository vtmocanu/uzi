// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useLayoutEffect } from "react";

import { api } from "../lib/api";
import type { ForgeConnection } from "../lib/api";
import { ForgeTokenBanner } from "./ForgeTokenBanner";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: { listConnectionsBackground: vi.fn() },
}));

const DAY = 24 * 3600 * 1000;
const mockApi = vi.mocked(api);

function conn(
  id: string,
  kind: "expired" | "expiring" | "unverified" | "ok",
  daysOut = 3,
  forge = "github",
): ForgeConnection {
  const exp = kind === "expired" ? -DAY : kind === "expiring" ? daysOut * DAY : undefined;
  return {
    id,
    forge_type: forge,
    base_url: `https://${id}.example.com`,
    privilege_report: {
      status: kind === "unverified" ? "error" : "ok",
      checked_at: new Date().toISOString(),
      repos: [],
      token: {
        scopes: [],
        active: true,
        expires_at: exp === undefined ? undefined : new Date(Date.now() + exp).toISOString(),
        violations: [],
        warnings: [],
      },
    },
  } as unknown as ForgeConnection;
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

function ui(userId: string) {
  return (
    <MemoryRouter>
      <ForgeTokenBanner userId={userId} />
    </MemoryRouter>
  );
}

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

async function renderBanner(userId = "u1") {
  const r = render(ui(userId));
  await settle();
  return r;
}

beforeEach(() => {
  window.sessionStorage.clear();
  mockApi.listConnectionsBackground.mockReset();
});
afterEach(cleanup);

describe("ForgeTokenBanner", () => {
  it("renders an expiring banner with the date, host and settings link", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "expiring", 3)] });
    await renderBanner();
    const banner = screen.getByRole("status", { name: "Forge token expiring" });
    const date = new Date(Date.now() + 3 * DAY).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
    expect(banner.textContent).toContain(`Your GitHub connection's token expires on ${date}. Update it in Settings → Forge before then.`);
    expect(banner.textContent).toContain("gh.example.com");
    expect(screen.getByRole("link", { name: /Settings → Forge/ }).getAttribute("href")).toBe("/settings/forge");
  });

  it("shows each expiring connection with its own date", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({
      connections: [conn("a", "expiring", 2), conn("b", "expiring", 9, "gitlab")],
    });
    await renderBanner();
    const fmt = (d: number) =>
      new Date(Date.now() + d * DAY).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
    const text = screen.getByRole("status", { name: "Forge token expiring" }).textContent ?? "";
    expect(text).toContain(`Your GitHub connection's token expires on ${fmt(2)}.`);
    expect(text).toContain(`Your GitLab connection's token expires on ${fmt(9)}.`);
  });

  it("expired has an alert role, the verbatim copy and no dismiss control", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "expired")] });
    await renderBanner();
    const banner = screen.getByRole("alert", { name: "Forge token expired" });
    expect(banner.textContent).toContain(
      "Your GitHub connection's token has expired or is inactive. Repositories using it stop syncing and runs cannot push. Update the token in Settings → Forge.",
    );
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
  });

  it("an expired banner ignores a stored dismissal for the same signature", async () => {
    const c = conn("gh", "expired");
    // Seed every signature form an expired banner could have, with and without its date.
    const exp = c.privilege_report?.token.expires_at ?? "";
    const sigs = [`expired|gh:`, `expired|gh:${exp}`];
    window.sessionStorage.setItem("uzi:forge-token-banner:dismissed:u1", JSON.stringify(sigs));
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [c] });
    await renderBanner();
    expect(screen.getByRole("alert", { name: "Forge token expired" })).toBeTruthy();
  });

  it("unverified is neutral, lists forge and host, and carries no warn or danger classes", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "unverified")] });
    await renderBanner();
    const banner = screen.getByRole("status", { name: "Forge connection unverified" });
    expect(banner.textContent).toContain("uzi could not verify this forge connection. Check it in Settings → Forge.");
    expect(banner.textContent).toContain("GitHub · gh.example.com");
    expect(banner.className).not.toMatch(/warn|danger/);
  });

  it("dismisses expiring and unverified banners", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "expiring")] });
    const { unmount } = await renderBanner();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status", { name: "Forge token expiring" })).toBeNull();
    unmount();

    window.sessionStorage.clear();
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "unverified")] });
    await renderBanner();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status", { name: "Forge connection unverified" })).toBeNull();
  });

  it("a dismissal survives a remount for the same user", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "unverified")] });
    const first = await renderBanner("u1");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    first.unmount();
    await renderBanner("u1");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("keeps both dismissals when the data moves between two states", async () => {
    const stateA = { connections: [conn("a", "unverified")] };
    const stateB = { connections: [conn("b", "expiring")] };
    mockApi.listConnectionsBackground.mockResolvedValue(stateA);
    await renderBanner();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status")).toBeNull();

    mockApi.listConnectionsBackground.mockResolvedValue(stateB);
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status")).toBeNull();

    mockApi.listConnectionsBackground.mockResolvedValue(stateA);
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("re-shows when a new connection becomes affected", async () => {
    // Reuse A's fixture so only the added connection changes the signature.
    const a = conn("a", "expiring");
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [a] });
    await renderBanner();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status")).toBeNull();

    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [a] });
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.queryByRole("status")).toBeNull();

    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [a, conn("b", "expiring")] });
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });

  it("refetches on window focus and moves from expired to unverified", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "expired")] });
    await renderBanner();
    expect(screen.getByRole("alert", { name: "Forge token expired" })).toBeTruthy();
    expect(mockApi.listConnectionsBackground).toHaveBeenCalledTimes(1);

    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "unverified")] });
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(mockApi.listConnectionsBackground).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("status", { name: "Forge connection unverified" })).toBeTruthy();
  });

  it("an older response resolving after a newer one does not override it", async () => {
    const older = deferred<{ connections: ForgeConnection[] }>();
    mockApi.listConnectionsBackground.mockReturnValueOnce(older.promise);
    render(ui("u1"));
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [conn("gh", "expiring")] });
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
    await act(async () => {
      older.resolve({ connections: [conn("gh", "expired")] });
    });
    await settle();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });

  it("issues no request on focus after unmount", async () => {
    mockApi.listConnectionsBackground.mockResolvedValue({ connections: [] });
    const { unmount } = await renderBanner();
    unmount();
    window.dispatchEvent(new Event("focus"));
    expect(mockApi.listConnectionsBackground).toHaveBeenCalledTimes(1);
  });

  it("renders nothing when the api method is missing", async () => {
    const original = mockApi.listConnectionsBackground;
    (mockApi as unknown as Record<string, unknown>).listConnectionsBackground = undefined;
    try {
      const { container } = await renderBanner();
      expect(container.textContent).toBe("");
    } finally {
      (mockApi as unknown as Record<string, unknown>).listConnectionsBackground = original;
    }
  });

  it("renders nothing when the api method throws or rejects", async () => {
    mockApi.listConnectionsBackground.mockImplementation(() => {
      throw new Error("sync boom");
    });
    const first = await renderBanner();
    expect(first.container.textContent).toBe("");
    first.unmount();
    mockApi.listConnectionsBackground.mockRejectedValue(new Error("offline"));
    const second = await renderBanner();
    expect(second.container.textContent).toBe("");
  });

  it("keeps the last good data when a later read fails", async () => {
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [conn("gh", "expired")] });
    await renderBanner();
    mockApi.listConnectionsBackground.mockRejectedValueOnce(new Error("offline"));
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
    });
    await settle();
    expect(screen.getByRole("alert", { name: "Forge token expired" })).toBeTruthy();
  });

  it("drops a previous user's late response when userId changes on the same element", async () => {
    const aLate = deferred<{ connections: ForgeConnection[] }>();
    mockApi.listConnectionsBackground.mockReturnValueOnce(aLate.promise);
    const { rerender } = render(ui("A"));
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [conn("b", "expiring")] });
    rerender(ui("B"));
    await settle();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
    await act(async () => {
      aLate.resolve({ connections: [conn("a", "expired")] });
    });
    await settle();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });
  it("never commits the previous user's banner when userId changes on the same element", async () => {
    // A layout effect runs after the DOM is updated but before passive effects, so it sees
    // exactly what the first commit for the new user put on screen.
    const seen: { userId: string; alert: boolean }[] = [];
    function Probe({ userId }: { userId: string }) {
      useLayoutEffect(() => {
        seen.push({ userId, alert: document.querySelector('[role="alert"]') !== null });
      });
      return null;
    }
    const tree = (userId: string) => (
      <MemoryRouter>
        <ForgeTokenBanner userId={userId} />
        <Probe userId={userId} />
      </MemoryRouter>
    );
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [conn("a", "expired")] });
    const { rerender } = render(tree("A"));
    await settle();
    expect(screen.getByRole("alert", { name: "Forge token expired" })).toBeTruthy();
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [conn("b", "expiring")] });
    rerender(tree("B"));
    await settle();
    expect(seen.filter((x) => x.userId === "B").map((x) => x.alert)).not.toContain(true);
    expect(seen.filter((x) => x.userId === "B").length).toBeGreaterThan(0);
    expect(screen.getByRole("status", { name: "Forge token expiring" })).toBeTruthy();
  });
  it("dismisses in memory when sessionStorage rejects the write", async () => {
    const setItem = vi.fn(() => {
      throw new Error("blocked");
    });
    vi.stubGlobal("sessionStorage", {
      getItem: () => null,
      setItem,
      removeItem: () => {},
      clear: () => {},
      key: () => null,
      length: 0,
    });
    try {
      mockApi.listConnectionsBackground.mockResolvedValue({ connections: [conn("gh", "expiring")] });
      await renderBanner();
      fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
      expect(setItem).toHaveBeenCalled();
      expect(screen.queryByRole("status", { name: "Forge token expiring" })).toBeNull();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("re-evaluates the last good data on focus even when the read fails", async () => {
    const start = Date.now();
    const c = conn("gh", "expiring", 1);
    mockApi.listConnectionsBackground.mockResolvedValueOnce({ connections: [c] });
    await renderBanner();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status")).toBeNull();

    const clock = vi.spyOn(Date, "now").mockReturnValue(start + 2 * DAY);
    try {
      mockApi.listConnectionsBackground.mockRejectedValueOnce(new Error("offline"));
      await act(async () => {
        window.dispatchEvent(new Event("focus"));
      });
      await settle();
      expect(screen.getByRole("alert", { name: "Forge token expired" })).toBeTruthy();
    } finally {
      clock.mockRestore();
    }
  });
});
