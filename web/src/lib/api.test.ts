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

// PRD #235 M2: getJudgeBacklog appends the selected labels as a comma-joined ?category=,
// enforced server-side before the row cap (the same shape as ?bucket=/?run=). The DTO does
// NOT echo it back (Decision 9); this only pins the REQUEST the client builds.
describe("getJudgeBacklog builds the ?category= query string (PRD #235)", () => {
  const emptyBacklog = { bucket: "todo", run: "", groups: [], truncated: false, triage: {} };

  it("joins the selected categories into a single comma-separated ?category= param", async () => {
    const fetchMock = vi.fn(async (_url: string) => fakeResponse(200, emptyBacklog));
    vi.stubGlobal("fetch", fetchMock);

    await api.getJudgeBacklog("todo", undefined, ["install_worker_tool", "improve_uzi"]);

    const url = fetchMock.mock.calls[0][0];
    expect(url).toContain("/api/me/judge/recommendations");
    const qs = new URLSearchParams(url.split("?")[1] ?? "");
    expect(qs.get("category")).toBe("install_worker_tool,improve_uzi");
    expect(qs.get("bucket")).toBe("todo");
  });

  it("omits ?category= when no labels are selected (empty means all)", async () => {
    const fetchMock = vi.fn(async (_url: string) => fakeResponse(200, emptyBacklog));
    vi.stubGlobal("fetch", fetchMock);

    await api.getJudgeBacklog("todo", undefined, []);

    const url = fetchMock.mock.calls[0][0];
    expect(url).not.toContain("category");
  });
});

// Issue #331: a passive listRuns (the hidden-tab favicon poll) tags its request with
// X-Uzi-Passive: 1 so the server authenticates it but skips the rolling refresh; a
// normal board/dashboard listRuns must NOT carry the header.
describe("listRuns passive-poll header (#331)", () => {
  const emptyRuns = { runs: [] };

  it("sends X-Uzi-Passive: 1 when called with { passive: true }", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, emptyRuns),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.listRuns({ passive: true });

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Uzi-Passive"]).toBe("1");
  });

  it("does not send X-Uzi-Passive on a normal listRuns", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, emptyRuns),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.listRuns();

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Uzi-Passive"]).toBeUndefined();
  });
});

// PRD #1349 M5/M6 (D7/D9): the exact custody-hold discard REQUIRES the ?confirm=discard query
// value — the server fail-fasts a missing or different value with a 400 BEFORE any SQL, so the
// client must always carry it. This pins the REQUEST the client builds so a future edit that
// drops the query (or renames the value) reddens a web test rather than silently 400ing.
describe("discardHold carries ?confirm=discard (PRD #1349)", () => {
  it("issues a DELETE to the run's recovery-hold path with confirm=discard", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, { discarded: true }),
    );
    vi.stubGlobal("fetch", fetchMock);
    // request() reads the CSRF cookie for non-GET methods; this file runs in the node env
    // (no jsdom document), so provide a minimal cookie source. cleaned up by afterEach's
    // vi.unstubAllGlobals().
    vi.stubGlobal("document", { cookie: "" });

    await api.discardHold("run-1", "hold-1");

    const url = fetchMock.mock.calls[0][0] as string;
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/api/runs/run-1/recovery-holds/hold-1");
    const qs = new URLSearchParams(url.split("?")[1] ?? "");
    expect(qs.get("confirm")).toBe("discard");
  });
});
