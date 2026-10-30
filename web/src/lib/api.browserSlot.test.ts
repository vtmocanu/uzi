import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";

afterEach(() => vi.unstubAllGlobals());

it("echoes current readable CSRF on GET/session probes, including after cookie rolling", async () => {
  const doc = { cookie: "uzi_csrf=first", hidden: false };
  vi.stubGlobal("document", doc);
  const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  await api.me();
  doc.cookie = "uzi_csrf=rolled";
  await api.getMySettings();
  doc.cookie = "";
  await api.me();
  expect(fetch.mock.calls.map(call => (call as unknown as [string, RequestInit])[1].headers)).toEqual([
    { "X-CSRF-Token": "first" }, { "X-CSRF-Token": "rolled" }, {},
  ]);
  for (const call of fetch.mock.calls) {
    const [url, init] = call as unknown as [string, RequestInit];
    expect(url.startsWith("/api/")).toBe(true);
    expect(init.credentials).toBe("same-origin");
    expect(init.body).toBeUndefined();
  }
});

it.each(["uzi_csrf=%", "uzi_csrf=%E0%A4"])("keeps ordinary reads functional for malformed readable cookie %s", async cookie => {
  vi.stubGlobal("document", { cookie, hidden: false });
  const fetch = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  await expect(api.me()).resolves.toEqual({});
  expect(fetch.mock.calls[0][1]?.headers).toEqual({});
});

it.each(["%00", "%0D%0A", "%E2%80%A8"])("skips header-unsafe readable CSRF %s without blocking a safe read", async encoded => {
  vi.stubGlobal("document", { cookie: `uzi_csrf=${encoded}`, hidden: false });
  const fetch = vi.fn(async (_url: string, init?: RequestInit) => {
    // Exercise the same header-value validation as the browser's native fetch.
    new Headers(init?.headers);
    return new Response("{}");
  });
  vi.stubGlobal("fetch", fetch);
  await expect(api.me()).resolves.toEqual({});
  expect(fetch.mock.calls[0][1]?.headers).toEqual({});
});

it("tags hidden dispatches passive, preserves explicit passive when visible, then permits foreground probes", async () => {
  const doc = { cookie: "uzi_csrf=readable", hidden: true };
  vi.stubGlobal("document", doc);
  const fetch = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  await api.me();
  doc.hidden = false;
  await api.listRuns({ passive: true });
  await api.getSlackStatus();
  await api.workerUpgradeSummary();
  await api.getRun("run", undefined, true);
  await api.getRunMessages("run", 0, true);
  await api.getRunInputs("run", true);
  await api.me();
  const headers = fetch.mock.calls.map(([, init]) => init?.headers);
  expect(headers.slice(0, -1)).toEqual(Array(7).fill({ "X-CSRF-Token": "readable", "X-Uzi-Passive": "1" }));
  expect(headers[headers.length - 1]).toEqual({ "X-CSRF-Token": "readable" });
});

it("tags hidden branding uploads while retaining write CSRF", async () => {
  vi.stubGlobal("document", { hidden: true, cookie: "uzi_csrf=readable" });
  const fetch = vi.fn(async (_url: string, _init?: RequestInit) => new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  await api.uploadBrandingLogo("primary", new File(["image"], "logo.png", { type: "image/png" }));
  expect(fetch.mock.calls[0][1]?.headers).toEqual({
    "Content-Type": "image/png", "X-CSRF-Token": "readable", "X-Uzi-Passive": "1",
  });
});
