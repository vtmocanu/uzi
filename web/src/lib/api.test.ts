import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, MOCK_MODE, setUnauthorizedHandler } from "./api";

// These exercise the real request() layer, so the suite must be running unmocked
// (mockApi never touches fetch and never 401s). Guard the assumption explicitly.
function fakeResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

afterEach(() => {
  setUnauthorizedHandler(null);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("request() global 401 handling", () => {
  it("runs against the real API client (not mock mode)", () => {
    expect(MOCK_MODE).toBe(false);
  });

  it("invokes the unauthorized handler and still throws ApiError(401) on a 401", async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    vi.stubGlobal("fetch", vi.fn(async () => fakeResponse(401, { error: "session expired" })));

    await expect(api.listRepos()).rejects.toMatchObject({ status: 401 });
    await expect(api.listRepos().catch((e) => e)).resolves.toBeInstanceOf(ApiError);
    // Once per failed call (two calls above); the point is it fires at all.
    expect(onUnauthorized).toHaveBeenCalled();
  });

  it("does not invoke the handler for a non-401 error", async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    vi.stubGlobal("fetch", vi.fn(async () => fakeResponse(500, { error: "boom" })));

    await expect(api.listRepos()).rejects.toMatchObject({ status: 500 });
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("does not invoke the handler on a successful response", async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    vi.stubGlobal("fetch", vi.fn(async () => fakeResponse(200, { repos: [] })));

    await expect(api.listRepos()).resolves.toEqual({ repos: [] });
    expect(onUnauthorized).not.toHaveBeenCalled();
  });
});
