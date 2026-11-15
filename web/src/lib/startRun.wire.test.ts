// @vitest-environment jsdom
//
// PRD #3250 M2, acceptance example 4: one-click Start run's request body is byte-identical
// to the pre-dialog base, pinned at the wire. This drives the REAL api.createRun through
// request() with fetch stubbed, so the bytes asserted are the bytes sent. The literals
// below are what the base (852a054d9) sent for an untouched start: `{"issue_iid":N}`, and
// `{"issue_iid":N,"force":true}` for the open-MR overwrite retry.
import { afterEach, describe, expect, it, vi } from "vitest";
import { DEFAULT_RUN_START_OPTIONS, startRunWithOptions, type RunStartFacts } from "./startRun";
import { MOCK_MODE } from "./api";

function response(status: number, body: unknown): Response {
  return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body) } as unknown as Response;
}

const FACTS: RunStartFacts[] = [
  { claudeUsable: true, codexUsable: false, defaultHarness: null },
  { claudeUsable: false, codexUsable: true, defaultHarness: null },
  { claudeUsable: true, codexUsable: true, defaultHarness: "codex" },
];

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function stubFetch(...responses: Response[]) {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => responses.shift() ?? response(500, {}));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const handlers = () => ({ onCreated: vi.fn(), onError: vi.fn(), onSettled: vi.fn() });

describe("one-click Start run wire body", () => {
  it("runs against the real request layer", () => {
    expect(MOCK_MODE).toBe(false);
  });

  it.each(FACTS)("sends exactly {\"issue_iid\":7} for the defaults (%o)", async (facts) => {
    const fetchMock = stubFetch(response(201, { run: { id: "run-1" } }));
    const h = handlers();
    await startRunWithOptions("repo-1", 7, DEFAULT_RUN_START_OPTIONS, facts, h);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/repos/repo-1/runs");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe('{"issue_iid":7}');
    expect(h.onCreated).toHaveBeenCalledWith("run-1");
  });

  it("the open-MR force retry sends exactly {\"issue_iid\":7,\"force\":true}", async () => {
    const fetchMock = stubFetch(
      response(409, { error: "issue #7 already has open MR !57", code: "issue_has_open_mr", mr_iid: 57 }),
      response(201, { run: { id: "run-2" } }),
    );
    vi.spyOn(window, "confirm").mockReturnValue(true);
    await startRunWithOptions("repo-1", 7, DEFAULT_RUN_START_OPTIONS, FACTS[0], handlers());
    expect(fetchMock.mock.calls.map(([, init]) => init?.body)).toEqual(['{"issue_iid":7}', '{"issue_iid":7,"force":true}']);
  });

  it("an effort-only dialog choice adds only effort (example 3), and the retry keeps it", async () => {
    const fetchMock = stubFetch(
      response(409, { error: "open MR", code: "issue_has_open_mr", mr_iid: 57 }),
      response(201, { run: { id: "run-3" } }),
    );
    vi.spyOn(window, "confirm").mockReturnValue(true);
    await startRunWithOptions("repo-1", 7, { ...DEFAULT_RUN_START_OPTIONS, effort: "high" }, FACTS[2], handlers());
    expect(fetchMock.mock.calls.map(([, init]) => init?.body)).toEqual([
      '{"issue_iid":7,"effort":"high"}',
      '{"issue_iid":7,"force":true,"effort":"high"}',
    ]);
  });
});
