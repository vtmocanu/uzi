// @vitest-environment jsdom
//
// startRunWithOptions and runStartBody (PRD #1247 M7, PRD #3250 M2): the shared start-run
// flow behind IssueView and Board. The load-bearing behaviour: the ONE builder omits every
// Default value and every value whose dialog row is hidden, the built object rides the
// create call, and it is RE-SENT unchanged across the open-MR force retry (issue #856).
// The wire-level byte pins live in startRun.wire.test.ts (real request(), stubbed fetch).
import { afterEach, describe, expect, it, vi } from "vitest";
import { DEFAULT_RUN_START_OPTIONS, runStartBody, startRunWithOptions, type RunStartFacts, type RunStartOptions } from "./startRun";
import { api, ApiError } from "./api";

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  // Keep ApiError / isOpenMRConflict / openMRConflictMRIID real; stub only createRun.
  return { ...actual, api: { ...actual.api, createRun: vi.fn() } };
});

const mockCreateRun = vi.mocked(api.createRun);

function handlers() {
  return { onCreated: vi.fn(), onError: vi.fn(), onSettled: vi.fn() };
}

const openMRConflict = () =>
  new ApiError(409, "issue #7 already has open MR !57", { code: "issue_has_open_mr", mr_iid: 57 });

// Fact sets: a Claude-only user, a Codex-only user, and a user with both harnesses usable.
const CLAUDE_ONLY: RunStartFacts = { claudeUsable: true, codexUsable: false, defaultHarness: null };
const CODEX_ONLY: RunStartFacts = { claudeUsable: false, codexUsable: true, defaultHarness: null };
const BOTH: RunStartFacts = { claudeUsable: true, codexUsable: true, defaultHarness: null };

const opts = (o: Partial<RunStartOptions>): RunStartOptions => ({ ...DEFAULT_RUN_START_OPTIONS, ...o });
const created = (id: string) => ({ run: { id } }) as Awaited<ReturnType<typeof api.createRun>>;

afterEach(() => vi.clearAllMocks());

describe("runStartBody (the one builder)", () => {
  it("builds an empty object from the defaults for every fact set", () => {
    for (const facts of [CLAUDE_ONLY, CODEX_ONLY, BOTH]) {
      expect(runStartBody(DEFAULT_RUN_START_OPTIONS, facts)).toEqual({});
    }
  });

  it("omits Default model and effort, sends chosen ones verbatim", () => {
    expect(runStartBody(opts({ effort: "high" }), CLAUDE_ONLY)).toEqual({ effort: "high" });
    expect(runStartBody(opts({ model: "opus" }), CLAUDE_ONLY)).toEqual({ model: "opus" });
    expect(runStartBody(opts({ model: "claude-x-1", effort: "max" }), CLAUDE_ONLY)).toEqual({
      model: "claude-x-1",
      effort: "max",
    });
  });

  it("drops the harness when its picker is hidden (only one harness usable)", () => {
    // A stale "codex" pick left from before a precondition refresh hid the picker.
    expect(runStartBody(opts({ harness: "codex" }), CLAUDE_ONLY)).toEqual({});
    expect(runStartBody(opts({ harness: "codex" }), BOTH)).toEqual({ harness: "codex" });
  });

  it("drops the token when the run will start on Codex", () => {
    const pinned = opts({ credential: { mode: "pinned", secret_id: "sec-9" } });
    expect(runStartBody(pinned, CLAUDE_ONLY)).toEqual({ credential_override: { mode: "pinned", secret_id: "sec-9" } });
    // Codex-only: the run resolves to Codex with no picker shown.
    expect(runStartBody(pinned, CODEX_ONLY)).toEqual({});
    // Both usable, explicit Codex pick: the token row is hidden, the harness is sent.
    expect(runStartBody({ ...pinned, harness: "codex" }, BOTH)).toEqual({ harness: "codex" });
    // Both usable with a usable Codex default: inherit resolves to Codex too.
    expect(runStartBody(pinned, { ...BOTH, defaultHarness: "codex" })).toEqual({});
  });

  it("sends an effort-only choice with nothing else added (example 3)", () => {
    expect(runStartBody(opts({ effort: "high" }), BOTH)).toEqual({ effort: "high" });
  });
});

describe("startRunWithOptions", () => {
  it("sends the empty object for the defaults, and navigates on success", async () => {
    mockCreateRun.mockResolvedValue(created("run-1"));
    const h = handlers();
    await startRunWithOptions("repo-1", 7, DEFAULT_RUN_START_OPTIONS, CLAUDE_ONLY, h);
    // No force argument at all on a first attempt.
    expect(mockCreateRun.mock.calls).toEqual([["repo-1", 7, {}]]);
    expect(h.onCreated).toHaveBeenCalledWith("run-1");
  });

  it("sends the built body for chosen options", async () => {
    mockCreateRun.mockResolvedValue(created("run-2"));
    const h = handlers();
    await startRunWithOptions(
      "repo-1",
      8,
      opts({ harness: "claude", credential: { mode: "pinned", secret_id: "sec-9" }, model: "opus", effort: "xhigh" }),
      BOTH,
      h,
    );
    expect(mockCreateRun.mock.calls).toEqual([
      ["repo-1", 8, { credential_override: { mode: "pinned", secret_id: "sec-9" }, harness: "claude", model: "opus", effort: "xhigh" }],
    ]);
  });

  it("RE-SENDS the same object on the open-MR force retry", async () => {
    mockCreateRun.mockRejectedValueOnce(openMRConflict()).mockResolvedValueOnce(created("run-3"));
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const h = handlers();

    await startRunWithOptions("repo-1", 9, opts({ harness: "codex", model: "gpt-6-sol", effort: "high" }), BOTH, h);

    expect(confirmSpy).toHaveBeenCalledTimes(1);
    expect(mockCreateRun).toHaveBeenCalledTimes(2);
    const body = { harness: "codex", model: "gpt-6-sol", effort: "high" };
    expect(mockCreateRun.mock.calls[0]).toEqual(["repo-1", 9, body]);
    expect(mockCreateRun.mock.calls[1]).toEqual(["repo-1", 9, body, true]);
    // The very same object, not a rebuilt one.
    expect(mockCreateRun.mock.calls[1][2]).toBe(mockCreateRun.mock.calls[0][2]);
    expect(h.onCreated).toHaveBeenCalledWith("run-3");
    confirmSpy.mockRestore();
  });

  it("RE-SENDS a pinned token on the force retry", async () => {
    mockCreateRun.mockRejectedValueOnce(openMRConflict()).mockResolvedValueOnce(created("run-4"));
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    await startRunWithOptions("repo-1", 10, opts({ credential: { mode: "pinned", secret_id: "sec-9" } }), CLAUDE_ONLY, handlers());
    expect(mockCreateRun.mock.calls[1]).toEqual(["repo-1", 10, { credential_override: { mode: "pinned", secret_id: "sec-9" } }, true]);
    confirmSpy.mockRestore();
  });

  it("settles without navigating when the user declines the open-MR overwrite", async () => {
    mockCreateRun.mockRejectedValueOnce(openMRConflict());
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    const h = handlers();

    await startRunWithOptions("repo-1", 11, opts({ credential: { mode: "auto" } }), CLAUDE_ONLY, h);

    expect(mockCreateRun).toHaveBeenCalledTimes(1);
    expect(h.onCreated).not.toHaveBeenCalled();
    expect(h.onError).not.toHaveBeenCalled();
    expect(h.onSettled).toHaveBeenCalledTimes(1);
    confirmSpy.mockRestore();
  });

  it("surfaces a 422 refusal's text unparsed, then settles", async () => {
    const text = "opus is not a model a Codex run can start with, and this run resolved to the codex harness; start it on the Claude harness or choose one of: gpt-5.6-sol";
    mockCreateRun.mockRejectedValueOnce(new ApiError(422, text, { code: "model_incompatible_with_harness", harness: "codex" }));
    const h = handlers();
    await startRunWithOptions("repo-1", 12, opts({ model: "opus" }), CODEX_ONLY, h);
    expect(h.onError).toHaveBeenCalledWith(text);
    expect(h.onSettled).toHaveBeenCalledTimes(1);
    expect(h.onError.mock.invocationCallOrder[0]).toBeLessThan(h.onSettled.mock.invocationCallOrder[0]);
  });
});
