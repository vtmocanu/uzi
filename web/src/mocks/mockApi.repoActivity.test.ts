import { describe, expect, it, vi } from "vitest";
vi.mock("./mockApi/shared", async original => ({ ...await original<typeof import("./mockApi/shared")>(), delay: <T>(value: T) => Promise.resolve(value) }));
import { mockApi } from "./mockApi";

// Issue #4334: the mock admin repo-activity read mirrors the real endpoint's row rule (every
// enabled repo, plus a disabled repo only while it has a listed non-terminal run) so the demo
// Repos chip has an active disabled repo and a repo that never ran.
describe("mock adminRepoActivity", () => {
  it("lists enabled repos and a disabled repo with a listed active run, never null", async () => {
    const { repos } = await mockApi.adminRepoActivity();
    const byId = new Map(repos.map(r => [r.id, r]));
    const listed = (await mockApi.adminListRuns({ view: "summary" })).runs;

    const disabledActive = repos.filter(r => !r.enabled);
    expect(disabledActive.length).toBeGreaterThan(0);
    for (const r of disabledActive) expect(listed.some(run => run.repo_id === r.id)).toBe(true);
    // A disabled repo with no active run is not listed.
    expect(byId.has("repo-ledger")).toBe(false);
    // An enabled repo that never ran reads null.
    expect(repos.some(r => r.enabled && r.last_run_at === null)).toBe(true);
    // A repo with history carries a parseable time.
    expect(Number.isFinite(Date.parse(byId.get("repo-uzi")!.last_run_at!))).toBe(true);
  });
});
