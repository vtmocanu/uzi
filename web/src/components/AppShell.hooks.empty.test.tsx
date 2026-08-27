// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { useAppVersion, useBuildInfo } from "./AppShell";
import { api } from "../lib/api";

// Own file, for the same reason as the sibling build-info test files: the promise
// behind these hooks is memoised at module scope with no reset seam, and vitest
// isolates per FILE, so a file that has already resolved it cannot resolve it
// again differently. It renders the HOOKS rather than the shell, which is what
// keeps it small — only `api.version` is reachable from here.
//
// WHAT IT NOW GUARDS: the SETTLED-BUT-UNKNOWN state, `""`, reached here by a body
// carrying an empty `version`. It is the middle of useAppVersion's three states and
// the one that is invisible from every other test — the panel copy it drives
// ("control-plane release unknown — targets unchecked") is identical whether the
// fetch failed or resolved empty, so only this file can tell you WHICH input
// produced it.
//
// THAT IS NOT A STYLISTIC SPLIT. Mutating each upstream cause separately shows the
// two files are each the SOLE guard on one of them:
//
//   revert useAppVersion wholesale    this RED    cpversion.failed RED
//   failed fetch folds back to null   this GREEN  cpversion.failed RED
//   empty body folds back to null     this RED    cpversion.failed GREEN
//
// So deleting either file leaves one cause of "" unguarded, silently, with the
// other file still green and looking like coverage.
//
// This file previously asserted the OPPOSITE: that `""` folded to `null`. That was
// the correct pin while the fold existed, and it is why flipping `||` to `??` used
// to redden here and nowhere else out of 1374 tests. The fold is gone now — a
// resolved empty version is settled information, not an absence — so the assertion
// inverted with the contract rather than being deleted with it.
//
// The complementary arm, a FAILED fetch reaching the same `""` through a different
// path, is in WorkersSettings.cpversion.failed.test.tsx, driven all the way to the
// rendered copy. Both are needed: this one pins the mapping, that one pins that a
// consumer actually renders it.

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: { version: vi.fn() },
}));
vi.mocked(api).version.mockResolvedValue({ version: "", founded: "2026-07-03" });

afterEach(cleanup);

describe("useAppVersion over useBuildInfo — three states from one promise", () => {
  it("reports a resolved EMPTY version as '' — settled and unknown, not in flight", async () => {
    const { result } = renderHook(() => ({ version: useAppVersion(), info: useBuildInfo() }));

    // In flight: null, and null ONLY here. Every later assertion depends on this
    // one having been true first, which is what makes "" mean settled.
    expect(result.current.version).toBeNull();

    await waitFor(() => expect(result.current.info).not.toBeNull());

    // The body resolved and is a real object — not the in-flight state.
    expect(result.current.info).toEqual({ version: "", founded: "2026-07-03" });
    expect(result.current.info!.version).toBe("");

    // …and the projection passes the empty string THROUGH rather than folding it to
    // null. `|| ""` would give the same answer here; the reason the line reads `??`
    // is stated at the hook. What must not come back is `null`, which would put a
    // settled answer back into the in-flight bucket and re-kill the panel's third
    // arm — the whole defect this change exists to fix.
    expect(result.current.version).toBe("");
    expect(result.current.version).not.toBeNull();

    // One request served both hooks.
    expect(vi.mocked(api).version).toHaveBeenCalledTimes(1);
  });
});
