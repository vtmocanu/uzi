// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { AuthProvider, useAuth } from "./AuthContext";
import { VaultLockedBanner } from "../components/VaultControls";
import { useFavicon } from "../lib/useFavicon";
import { useRunStream } from "../lib/useRunStream";

function Surface() {
  const { user, refresh, vaultUnlocked } = useAuth();
  useFavicon({ enabled: !!user, appLogoSrc: null });
  useRunStream("run");
  return <><VaultLockedBanner /><span data-testid="unlocked">{String(vaultUnlocked)}</span>
    <button onClick={() => void refresh()}>probe</button></>;
}

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); document.cookie = "uzi_csrf=; Max-Age=0"; });

it("keeps hidden restart, favicon and socket replay locked; focus supersedes an older passive probe", async () => {
  vi.useFakeTimers();
  let hidden = true;
  vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  document.cookie = "uzi_csrf=current";
  const sockets: Socket[] = [];
  class Socket {
    onopen: (() => void) | null = null;
    onclose: (() => void) | null = null;
    onmessage: ((ev: { data: string }) => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(readonly url: string) { sockets.push(this); }
    close() {}
  }
  vi.stubGlobal("WebSocket", Socket);
  let restored = false;
  let holdProbe = false;
  let late: ((response: Response) => void) | undefined;
  const session = (unlocked: boolean) => ({
    user: { id: "u1", email: "user@example.com" }, theme: "ember", default_theme: "ember",
    theme_override: null, has_password: true,
    vault: { mode: "protected", policy: "optional", unlocked, exists: true },
  });
  const requests: Array<{ url: string; passive: boolean; csrf: string | null }> = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    const headers = new Headers(init.headers);
    const passive = headers.get("X-Uzi-Passive") === "1";
    requests.push({ url, passive, csrf: headers.get("X-CSRF-Token") });
    if (url === "/api/auth/me" && holdProbe) {
      holdProbe = false;
      return new Promise<Response>(resolve => { late = resolve; });
    }
    if (!passive && headers.get("X-CSRF-Token") === "current") restored = true;
    const body = url === "/api/auth/me" ? session(restored)
      : url.includes("/messages") ? { messages: [] }
      : url.includes("/inputs") ? { inputs: [] }
      : url.startsWith("/api/runs?") ? { runs: [] }
      : { run: { id: "run", status: "running", status_since: "2026-10-29T00:00:00Z" } };
    return new Response(JSON.stringify(body));
  }));
  render(<AuthProvider><Surface /></AuthProvider>);
  await act(async () => {});
  expect(screen.getByTestId("unlocked").textContent).toBe("false");
  expect(screen.getByRole("button", { name: "Unlock" })).toBeTruthy();
  await act(async () => {
    sockets[0].onopen?.();
    sockets[0].onclose?.();
    await vi.advanceTimersByTimeAsync(1500);
    sockets[1].onopen?.();
    sockets[1].onmessage?.({ data: JSON.stringify({ type: "state" }) });
    await vi.advanceTimersByTimeAsync(20_000);
  });
  expect(restored).toBe(false);
  // Ambient socket replay stays passive even in a visible tab before focus.
  hidden = false;
  await act(async () => { sockets[1].onopen?.(); await vi.advanceTimersByTimeAsync(150); });
  expect(restored).toBe(false);
  hidden = true;
  expect(requests.some(r => r.url.startsWith("/api/runs?"))).toBe(true);
  expect(requests.filter(r => !r.url.includes("/auth/me")).every(r => r.passive)).toBe(true);
  holdProbe = true;
  fireEvent.click(screen.getByRole("button", { name: "probe" }));
  hidden = false;
  await act(async () => { window.dispatchEvent(new Event("focus")); });
  expect(requests.filter(r => r.url === "/api/auth/me")).toHaveLength(3);
  expect(requests[requests.length - 1]).toEqual({ url: "/api/auth/me", passive: false, csrf: "current" });
  expect(screen.getByTestId("unlocked").textContent).toBe("true");
  expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull();
  await act(async () => { late!(new Response(JSON.stringify(session(false)))); });
  expect(screen.getByTestId("unlocked").textContent).toBe("true");
});

it("focus supersedes a pending hidden initial probe before identity is known", async () => {
  let hidden = true;
  vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  document.cookie = "uzi_csrf=current";
  let late!: (response: Response) => void;
  const fetch = vi.fn((_url: string, _init?: RequestInit) => fetch.mock.calls.length === 1
    ? new Promise<Response>(resolve => { late = resolve; })
    : Promise.resolve(new Response(JSON.stringify({ user: null }))));
  vi.stubGlobal("fetch", fetch);
  function Probe() { const { loading } = useAuth(); return <span>{String(loading)}</span>; }
  render(<AuthProvider><Probe /></AuthProvider>);
  hidden = false;
  await act(async () => { window.dispatchEvent(new Event("focus")); });
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(new Headers(fetch.mock.calls[1][1]?.headers).get("X-Uzi-Passive")).toBeNull();
  await act(async () => { late(new Response(JSON.stringify({ user: null }))); });
  expect(screen.getByText("false")).toBeTruthy();
});
