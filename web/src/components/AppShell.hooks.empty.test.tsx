// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { useAppVersion, useBuildInfo } from "./AppShell";
import { api } from "../lib/api";

// A FOURTH file, for one response shape, for the same reason as the other three:
// the promise behind these hooks is memoised at module scope with no reset seam,
// and vitest isolates per FILE, so a file that has already resolved it cannot
// resolve it again differently.
//
// It renders the HOOKS rather than the shell, which is what keeps the file small —
// only `api.version` is reachable from here, so none of AppShell's other polls
// need stubbing.
//
// What it pins: `useAppVersion` is `useBuildInfo()?.version || null`, and that `||`
// is load-bearing. Flipping it to `??` — the single most likely future "cleanup",
// since `??` is usually the more correct operator — leaves typecheck clean and
// every other test green, while changing what WorkerUpgradeBadge receives for a
// resolved-but-unstamped version. Without this assertion nothing notices.

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: { version: vi.fn() },
}));
vi.mocked(api).version.mockResolvedValue({ version: "", founded: "2026-07-03" });

afterEach(cleanup);

describe("useAppVersion over useBuildInfo — one promise, two shapes", () => {
  it("projects an EMPTY version to null while useBuildInfo still reports the object", async () => {
    const { result } = renderHook(() => ({ version: useAppVersion(), info: useBuildInfo() }));

    await waitFor(() => expect(result.current.info).not.toBeNull());

    // The build info resolved and is a real object — this is NOT the in-flight state.
    expect(result.current.info).toEqual({ version: "", founded: "2026-07-03" });
    expect(result.current.info!.version).toBe("");

    // …and the projection maps that empty string to null. `??` would return "" here
    // and this assertion is the only thing standing between the two operators.
    expect(result.current.version).toBeNull();

    // One request served both hooks.
    expect(vi.mocked(api).version).toHaveBeenCalledTimes(1);
  });
});
