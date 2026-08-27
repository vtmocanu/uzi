// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { mockAdmin, mockFindings } from "./data";

// Each test re-imports a fresh mockApi so mutating verbs (file/dismiss) don't bleed across
// tests. The session starts signed in as admin, so requireSession resolves without a login.
async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => vi.resetModules());

// Expectations are DERIVED from the fixture, not snapshotted — the demo fixture must be free
// to grow (it feeds the page, the card, and the notification) without reddening these.
const mine = mockFindings.filter((f) => f.user_id === mockAdmin.id);
const openCount = mine.filter((f) => f.status === "open").length;

describe("mockApi findings backlog (PRD #333 M7)", () => {
  it("fixture is shaped so the assertions below can fail", () => {
    expect(openCount).toBeGreaterThan(0);
    expect(mine.some((f) => f.status === "filed")).toBe(true);
    expect(mine.some((f) => f.status === "dismissed")).toBe(true);
    // A display-only coordinate (evidence cascaded away) and more than one repo, so grouping
    // and the null-finding_id row are both exercised by the surfaces.
    expect(mine.some((f) => f.finding_id === null)).toBe(true);
    expect(new Set(mine.map((f) => f.repo_id)).size).toBeGreaterThan(1);
  });

  it("to_file returns only open coordinates and carries the open_count meta", async () => {
    const api = await freshApi();
    const res = await api.listFindings();
    expect(res.bucket).toBe("to_file");
    expect(res.open_count).toBe(openCount);
    expect(res.findings.every((f) => f.status === "open")).toBe(true);
  });

  it("open_count is stable across a bucket switch (a nav-badge count, not a row tally)", async () => {
    const api = await freshApi();
    const toFile = await api.listFindings("to_file");
    const filed = await api.listFindings("filed");
    expect(filed.open_count).toBe(toFile.open_count);
    expect(filed.findings.every((f) => f.status === "filed")).toBe(true);
  });

  it("filters by repo and omits the finding_id on a display-only coordinate", async () => {
    const api = await freshApi();
    const uzi = await api.listFindings("all", "repo-uzi");
    expect(uzi.findings.every((f) => f.repo_id === "repo-uzi")).toBe(true);
    const orphan = uzi.findings.find((f) => f.status === "filed" && f.finding_id === undefined);
    expect(orphan).toBeTruthy();
  });

  it("filters by run via the evidence semi-join", async () => {
    const api = await freshApi();
    const byRun = await api.listFindings("all", undefined, "run-live");
    expect(byRun.findings.length).toBeGreaterThan(0);
    // Every returned coordinate names a fixture row seen in run-live.
    for (const row of byRun.findings) {
      const src = mine.find((f) => f.finding_id === row.finding_id);
      expect(src?.run_ids).toContain("run-live");
    }
  });

  it("file flips an open coordinate to filed and a second file on it is a 409", async () => {
    const api = await freshApi();
    const res = await api.fileFinding("find-1");
    expect(res.issue.iid).toBeGreaterThan(0);
    expect(res.issue.web_url.startsWith("https://")).toBe(true);
    // Now it is filed — the to_file bucket no longer shows it, and a re-file is the claim 409.
    const toFile = await api.listFindings("to_file");
    expect(toFile.findings.some((f) => f.finding_id === "find-1")).toBe(false);
    await expect(api.fileFinding("find-1")).rejects.toMatchObject({ status: 409 });
  });

  it("dismiss requires a valid reason and refuses a non-open coordinate", async () => {
    const api = await freshApi();
    // A fresh module import gives a distinct ApiError class, so assert on the status the
    // structured error carries rather than the class identity.
    await expect(
      api.dismissFinding("find-1", "nope" as unknown as "wont_do"),
    ).rejects.toMatchObject({ status: 400 });
    const ok = await api.dismissFinding("find-1", "wont_do");
    expect(ok.status).toBe("dismissed");
    // A second dismiss (now dismissed) is a 409.
    await expect(api.dismissFinding("find-1", "wont_do")).rejects.toMatchObject({ status: 409 });
  });
});
