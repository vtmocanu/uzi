// @vitest-environment jsdom
//
// The host SDK (PRD #4686, "Host SDK"). Shape tests inject a transport; the api tests go
// through the REAL moduleApiRequest -> request() path with only globalThis.fetch stubbed,
// and a mock (demo) build is forced faithfully the way App.routes.test.tsx does it: stub
// the build flag, reset modules, import after.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, setUnauthorizedHandler, setVaultLockedHandler } from "../lib/api";
import { Badge, Button, Card, CardHeader, EmptyState, Select, Skeleton, Spinner, StatusPill } from "../components/ui";
import { createHostSdk, HOST_CONTRACT } from "./sdk";

const user = { id: "u1", email: "a@b.c", display_name: "Ada", is_admin: true };
const navigate = vi.fn();

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  document.cookie = "uzi_csrf=tok123; path=/";
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  setUnauthorizedHandler(null);
  setVaultLockedHandler(null);
  document.cookie = "uzi_csrf=; path=/; max-age=0";
});

describe("createHostSdk shape", () => {
  it("exposes contract 1, the user, the nine ui components and navigate", () => {
    const sdk = createHostSdk({ user, navigate, transport: vi.fn() });
    expect(sdk.contract).toBe(1);
    expect(HOST_CONTRACT).toBe(1);
    expect(sdk.user).toEqual({ id: "u1", displayName: "Ada", isAdmin: true });
    expect(sdk.ui).toEqual({ Card, CardHeader, Button, Select, Badge, StatusPill, EmptyState, Spinner, Skeleton });
    sdk.navigate("/runs");
    expect(navigate).toHaveBeenCalledWith("/runs");
  });

  it("falls back to the email when there is no display name", () => {
    expect(createHostSdk({ user: { ...user, display_name: null }, navigate }).user.displayName).toBe("a@b.c");
  });

  it("passes method, path and body to the injected transport", async () => {
    const transport = vi.fn().mockResolvedValue({ ok: 1 });
    const sdk = createHostSdk({ user, navigate, transport });
    await expect(sdk.api.request("POST", "/x", { a: 1 })).resolves.toEqual({ ok: 1 });
    expect(transport).toHaveBeenCalledWith("POST", "/x", { a: 1 });
  });
});

describe("sdk.api.request through the real path", () => {
  it("prepends /api, sends the CSRF header and the JSON body, and returns the payload", async () => {
    fetchMock.mockResolvedValue(json(200, { hello: "world" }));
    const sdk = createHostSdk({ user, navigate });
    await expect(sdk.api.request("POST", "/things?x=1", { a: 1 })).resolves.toEqual({ hello: "world" });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/things?x=1");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("same-origin");
    expect(init.body).toBe('{"a":1}');
    expect(init.headers).toMatchObject({ "X-CSRF-Token": "tok123", "Content-Type": "application/json" });
  });

  it("rejects with the host ApiError (status and server message)", async () => {
    fetchMock.mockResolvedValue(json(404, { error: "nope" }));
    const err = await createHostSdk({ user, navigate }).api.request("GET", "/missing").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, message: "nope" });
  });

  it("runs the global 401 handler", async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    fetchMock.mockResolvedValue(json(401, { error: "unauthorized" }));
    await expect(createHostSdk({ user, navigate }).api.request("GET", "/me")).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("runs the vault-locked handler on a 409 vault_locked", async () => {
    const onLocked = vi.fn();
    setVaultLockedHandler(onLocked);
    fetchMock.mockResolvedValue(json(409, { code: "vault_locked", error: "locked" }));
    await expect(createHostSdk({ user, navigate }).api.request("PUT", "/secrets/a", {})).rejects.toMatchObject({ status: 409 });
    expect(onLocked).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["a relative path", "things"],
    ["an absolute URL", "https://evil.example/api/x"],
    ["a protocol-relative URL", "//evil.example/x"],
    ["a double slash inside", "/a//b"],
    ["a backslash", "/a\\b"],
    ["a dot segment", "/a/./b"],
    ["a dot-dot segment", "/a/../b"],
    ["a leading dot-dot", "/../x"],
    ["a dot-dot before the query", "/a/..?q=1"],
    ["an encoded slash", "/a%2fb"],
    ["an encoded slash, upper case", "/a%2Fb"],
    ["an encoded backslash", "/a%5cb"],
    ["an encoded backslash, upper case", "/a%5Cb"],
    ["an encoded dot", "/%2e%2e/x"],
    ["an encoded dot, upper case", "/%2E%2E/x"],
    ["a tab", "/a\t/../x"],
    ["a newline", "/a\n/b"],
    ["a space", "/a b"],
    ["an empty path", ""],
  ])("rejects %s with no fetch", async (_n, path) => {
    const err = await createHostSdk({ user, navigate }).api.request("GET", path).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects a C1 control character (U+0085) with no fetch", async () => {
    const err = await createHostSdk({ user, navigate }).api.request("GET", "/a\u0085b").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it.each(["TRACE", "get", "CONNECT", ""])("rejects method %j with no fetch", async (method) => {
    const err = await createHostSdk({ user, navigate })
      .api.request(method as never, "/x")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 400 });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("allows %2F and // in the query and fetches under /api/", async () => {
    fetchMock.mockImplementation(() => Promise.resolve(json(200, {})));
    const sdk = createHostSdk({ user, navigate });
    await sdk.api.request("GET", "/x?branch=a%2Fb");
    await sdk.api.request("GET", "/x?next=//e#frag%2f");
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(["/api/x?branch=a%2Fb", "/api/x?next=//e#frag%2f"]);
  });

  it("accepts the root and a deep path", async () => {
    fetchMock.mockImplementation(() => Promise.resolve(json(200, {})));
    const sdk = createHostSdk({ user, navigate });
    await sdk.api.request("GET", "/");
    await sdk.api.request("GET", "/a/b-c/d_e.f");
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(["/api/", "/api/a/b-c/d_e.f"]);
  });
});

describe("sdk.api.request in a mock (demo) build", () => {
  it("rejects with ApiError 503 and never fetches", async () => {
    vi.stubEnv("VITE_UZI_MOCK", "1");
    vi.resetModules();
    const { createHostSdk: createMockSdk } = await import("./sdk");
    const { ApiError: MockApiError } = await import("../lib/apiError");
    const err = await createMockSdk({ user, navigate }).api.request("GET", "/runs").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(MockApiError);
    expect(err).toMatchObject({ status: 503, message: "not available in demo mode" });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
