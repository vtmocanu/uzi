// @vitest-environment jsdom
//
// startRunWithCredential (PRD #1247 M7): the shared start-run flow behind IssueView and
// Board. The load-bearing behaviour is that the chosen credential override rides the
// create body AND is RE-SENT unchanged across the open-MR force retry (issue #856) —
// never dropped — while an inherit choice sends no override at all.
import { afterEach, describe, expect, it, vi } from "vitest";
import { startRunWithCredential } from "./startRun";
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

afterEach(() => vi.clearAllMocks());

describe("startRunWithCredential", () => {
  it("sends NO override for an inherit choice, and navigates on success", async () => {
    mockCreateRun.mockResolvedValue({ run: { id: "run-1" } } as Awaited<ReturnType<typeof api.createRun>>);
    const h = handlers();
    await startRunWithCredential("repo-1", 7, { mode: "inherit" }, h);
    // inherit → the 3-arg form (no override), byte-identical to the pre-#1247 call.
    expect(mockCreateRun).toHaveBeenCalledWith("repo-1", 7, undefined);
    expect(h.onCreated).toHaveBeenCalledWith("run-1");
  });

  it("sends the pinned override on the create body", async () => {
    mockCreateRun.mockResolvedValue({ run: { id: "run-2" } } as Awaited<ReturnType<typeof api.createRun>>);
    const h = handlers();
    await startRunWithCredential("repo-1", 8, { mode: "pinned", secret_id: "sec-9" }, h);
    expect(mockCreateRun).toHaveBeenCalledWith("repo-1", 8, undefined, { mode: "pinned", secret_id: "sec-9" });
    expect(h.onCreated).toHaveBeenCalledWith("run-2");
  });

  it("RE-SENDS the same override on the open-MR force retry", async () => {
    // First call: open-MR conflict. Second (force) call: succeeds.
    mockCreateRun
      .mockRejectedValueOnce(openMRConflict())
      .mockResolvedValueOnce({ run: { id: "run-3" } } as Awaited<ReturnType<typeof api.createRun>>);
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const h = handlers();

    await startRunWithCredential("repo-1", 9, { mode: "pinned", secret_id: "sec-9" }, h);

    expect(confirmSpy).toHaveBeenCalledTimes(1);
    expect(mockCreateRun).toHaveBeenCalledTimes(2);
    // The FIRST attempt (no force) carries the override…
    expect(mockCreateRun.mock.calls[0]).toEqual(["repo-1", 9, undefined, { mode: "pinned", secret_id: "sec-9" }]);
    // …and the forced retry re-sends the SAME override (force=true), not a dropped one.
    expect(mockCreateRun.mock.calls[1]).toEqual(["repo-1", 9, true, { mode: "pinned", secret_id: "sec-9" }]);
    expect(h.onCreated).toHaveBeenCalledWith("run-3");
    confirmSpy.mockRestore();
  });

  it("settles without navigating when the user declines the open-MR overwrite", async () => {
    mockCreateRun.mockRejectedValueOnce(openMRConflict());
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    const h = handlers();

    await startRunWithCredential("repo-1", 10, { mode: "auto" }, h);

    expect(mockCreateRun).toHaveBeenCalledTimes(1);
    expect(h.onCreated).not.toHaveBeenCalled();
    expect(h.onSettled).toHaveBeenCalledTimes(1);
    confirmSpy.mockRestore();
  });
});
