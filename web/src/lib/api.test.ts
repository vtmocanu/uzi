import { afterEach, describe, expect, it, vi } from "vitest";
import {
  api,
  ApiError,
  egressProfileProblems,
  gateRevisionMismatchCurrent,
  isOutcomePendingConfirmation,
  MOCK_MODE,
  setUnauthorizedHandler,
} from "./api";

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

describe("testSecret request", () => {
  it("posts to the selected credential without a provider body and returns the flat verdict", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => fakeResponse(200, { status: "inconclusive", reason: "vault_locked" }));
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal("document", { cookie: "" });

    await expect(api.testSecret("openai_api_key", "sec-2")).resolves.toEqual({ status: "inconclusive", reason: "vault_locked" });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/me/secrets/openai_api_key/sec-2/test");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBeUndefined();
  });
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

// PRD #1371 (AC #4): getRecoveryHolds bounds the read to ?state=open so the listing carries
// only still-open holds (released/discarded are excluded). The server validates state as
// absent|open; the web always sends open. This pins the REQUEST the client builds so a future
// edit that drops the query reddens a web test rather than silently widening the read.
describe("getRecoveryHolds bounds the read to ?state=open (PRD #1371)", () => {
  it("issues a GET to the recovery holds path with state=open", async () => {
    const emptyHolds = {
      aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
      holds: [],
    };
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, emptyHolds),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.getRecoveryHolds();

    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toContain("/api/recovery/holds");
    const qs = new URLSearchParams(url.split("?")[1] ?? "");
    expect(qs.get("state")).toBe("open");
  });
});

// PRD #1391 Run B M3d (D13): the cancel-confirmation gate. The server refuses a cancel of a
// run whose worker holds a finished-but-unlanded outcome with a typed 409 carrying
// reason:"outcome_pending_confirmation_required", and the web routes ONLY that specific 409
// into a discard-confirmation modal (never a generic 409). isOutcomePendingConfirmation is
// the discriminator; these pin that it keys on the reason field, not a bare status.
describe("isOutcomePendingConfirmation (PRD #1391 M3d)", () => {
  it("is true for a 409 whose body reason is outcome_pending_confirmation_required", () => {
    const err = new ApiError(409, "cancel refused: pending outcome", {
      error: "cancel refused: pending outcome",
      reason: "outcome_pending_confirmation_required",
    });
    expect(isOutcomePendingConfirmation(err)).toBe(true);
  });

  it("is false for a 409 with a different reason", () => {
    const err = new ApiError(409, "conflict", { reason: "issue_has_open_mr" });
    expect(isOutcomePendingConfirmation(err)).toBe(false);
  });

  it("is false for the right reason on the wrong status", () => {
    const err = new ApiError(400, "bad", {
      reason: "outcome_pending_confirmation_required",
    });
    expect(isOutcomePendingConfirmation(err)).toBe(false);
  });

  it("is false for a non-ApiError value", () => {
    expect(isOutcomePendingConfirmation(new Error("boom"))).toBe(false);
    expect(isOutcomePendingConfirmation(null)).toBe(false);
  });
});

// PRD #1391 Run B M3d (D13): the confirmed retry must carry discard_pending_outcome:true so
// the server takes the atomic no-live-poller cancel branch; an ordinary cancel omits it so a
// completed outcome is never silently discarded. This pins the REQUEST body the client builds.
describe("submitRunInput threads discard_pending_outcome (PRD #1391 M3d)", () => {
  const stubDoc = () => vi.stubGlobal("document", { cookie: "" });

  it("adds discard_pending_outcome:true when the flag is set", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, { server_side: true }),
    );
    vi.stubGlobal("fetch", fetchMock);
    stubDoc();

    // submitRunInput(id, kind, body, selection, overrideCapabilities, discardPendingOutcome)
    await api.submitRunInput("run-1", "cancel", "", undefined, undefined, true);

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.kind).toBe("cancel");
    expect(body.discard_pending_outcome).toBe(true);
  });

  it("omits discard_pending_outcome on an ordinary cancel", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, { server_side: true }),
    );
    vi.stubGlobal("fetch", fetchMock);
    stubDoc();

    await api.submitRunInput("run-1", "cancel");

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body).not.toHaveProperty("discard_pending_outcome");
  });
});

// PRD #1795 D5: a plan-gate verdict carries the revision the owner is looking at, and only
// when there is one; a run without a revision keeps today's body byte for byte.
describe("submitRunInput threads expected_gate_revision (PRD #1795 M4)", () => {
  const stubDoc = () => vi.stubGlobal("document", { cookie: "" });

  it("adds expected_gate_revision when a revision is given", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, { server_side: false }),
    );
    vi.stubGlobal("fetch", fetchMock);
    stubDoc();

    await api.submitRunInput("run-1", "approve_plan", "", undefined, undefined, undefined, 3);

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.kind).toBe("approve_plan");
    expect(body.expected_gate_revision).toBe(3);
  });

  it("omits expected_gate_revision when no revision is given", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(200, { server_side: false }),
    );
    vi.stubGlobal("fetch", fetchMock);
    stubDoc();

    await api.submitRunInput("run-1", "approve_plan");

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    // Paired with the positive case above, so this cannot pass vacuously.
    expect(body.kind).toBe("approve_plan");
    expect(body).not.toHaveProperty("expected_gate_revision");
  });
});

describe("gateRevisionMismatchCurrent (PRD #1795 M4)", () => {
  it("returns the current revision from the typed 409", () => {
    const err = new ApiError(409, "the plan changed", {
      error: "the plan changed",
      reason: "gate_revision_mismatch",
      current_gate_revision: 4,
    });
    expect(gateRevisionMismatchCurrent(err)).toBe(4);
  });

  it("is null for another 409 reason, another status, or a non-ApiError", () => {
    expect(
      gateRevisionMismatchCurrent(new ApiError(409, "x", { reason: "outcome_pending_confirmation_required" })),
    ).toBeNull();
    expect(
      gateRevisionMismatchCurrent(new ApiError(400, "x", { reason: "gate_revision_mismatch", current_gate_revision: 2 })),
    ).toBeNull();
    expect(gateRevisionMismatchCurrent(new Error("boom"))).toBeNull();
  });
});

describe("egressProfileProblems (PRD #1906 M1w)", () => {
  const refused = (problems: unknown) =>
    new ApiError(422, "the egress profile is invalid: see problems", { reason: "invalid_egress_profile", problems });

  it("returns the well-formed problems of an invalid_egress_profile 422", () => {
    const p = { field: "hosts[0]", code: "scheme", message: "list a host name, not a URL" };
    expect(egressProfileProblems(refused([p, { field: 3 }, null]))).toEqual([p]);
  });

  it("is null when nothing usable is left, so the editor falls back to the generic error", () => {
    expect(egressProfileProblems(refused([]))).toBeNull();
    expect(egressProfileProblems(refused([{ field: "name" }, "x", null]))).toBeNull();
    expect(egressProfileProblems(refused("not a list"))).toBeNull();
  });

  it("is null for any other error", () => {
    expect(egressProfileProblems(new ApiError(422, "x", { reason: "other", problems: [] }))).toBeNull();
    expect(egressProfileProblems(new ApiError(409, "taken"))).toBeNull();
    expect(egressProfileProblems(new Error("boom"))).toBeNull();
  });
});

// PRD #1976 M2: a site-list name is free text, so the path segment must be percent-encoded.
describe("product site-list allowance URLs", () => {
  it.each([
    ["PUT", () => api.adminAllowProductEgressProfile("prod-a", "model cards/v2")],
    ["DELETE", () => api.adminDisallowProductEgressProfile("prod-a", "model cards/v2")],
  ] as const)("%s encodes the list name into one path segment", async (method, call) => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      fakeResponse(method === "PUT" ? 200 : 204, method === "PUT" ? { egress_profiles: [] } : null),
    );
    vi.stubGlobal("fetch", fetchMock);
    // A write reads the CSRF cookie; this file runs in node, so give it an empty jar.
    vi.stubGlobal("document", { cookie: "" });

    await call();

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("/admin/products/prod-a/egress-profiles/model%20cards%2Fv2");
    expect((init as RequestInit).method).toBe(method);
  });
});
