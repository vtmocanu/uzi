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
    // PRD #1183 M3/M4: a `done` row (issue-close sync) and a dismissed row carrying a reason, so
    // the done chip and the reasoned dismissed chip are both exercised by the surfaces.
    expect(mine.some((f) => f.status === "done" && f.set_via === "issue_close")).toBe(true);
    expect(mine.some((f) => f.status === "dismissed" && f.dismiss_reason != null)).toBe(true);
    // Every coordinate carries a disposition_id (the bulk-dismiss / undo key).
    expect(mine.every((f) => !!f.disposition_id)).toBe(true);
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

  it("the done bucket returns the issue-close coordinate with its provenance", async () => {
    const api = await freshApi();
    const done = await api.listFindings("done");
    expect(done.findings.length).toBeGreaterThan(0);
    expect(done.findings.every((f) => f.status === "done")).toBe(true);
    const row = done.findings.find((f) => f.set_via === "issue_close");
    expect(row).toBeTruthy();
    expect(row?.filed_issue_iid).toBeGreaterThan(0);
  });

  it("the dismissed bucket carries the dismiss_reason", async () => {
    const api = await freshApi();
    const dismissed = await api.listFindings("dismissed");
    expect(dismissed.findings.some((f) => f.dismiss_reason === "wont_do")).toBe(true);
  });

  it("every backlog row carries its disposition_id", async () => {
    const api = await freshApi();
    const all = await api.listFindings("all");
    expect(all.findings.length).toBeGreaterThan(0);
    expect(all.findings.every((f) => !!f.disposition_id)).toBe(true);
  });
});

describe("mockApi findings stats (PRD #1183 M3)", () => {
  it("computes the six counts from the seed, and equals a hand count per status", async () => {
    const api = await freshApi();
    const stats = await api.getFindingsStats();
    expect(stats.total).toBe(mine.length);
    expect(stats.todo).toBe(mine.filter((f) => f.status === "open").length);
    expect(stats.filed).toBe(mine.filter((f) => f.status === "filed").length);
    expect(stats.done).toBe(mine.filter((f) => f.status === "done").length);
    expect(stats.dismissed).toBe(mine.filter((f) => f.status === "dismissed").length);
    expect(stats.false_positives).toBe(
      mine.filter((f) => f.status === "dismissed" && f.dismiss_reason === "not_an_issue").length,
    );
  });

  it("is repo-scoped", async () => {
    const api = await freshApi();
    const atlas = await api.getFindingsStats("repo-atlas");
    const atlasRows = mine.filter((f) => f.repo_id === "repo-atlas");
    expect(atlas.total).toBe(atlasRows.length);
    expect(atlas.todo).toBe(atlasRows.filter((f) => f.status === "open").length);
  });

  it("moves the todo count after a dismiss (the nav-badge source)", async () => {
    const api = await freshApi();
    const before = await api.getFindingsStats();
    // disp-1 (find-1) is open — dismiss it via the bulk endpoint.
    await api.dismissFindings(["disp-1"], "wont_do");
    const after = await api.getFindingsStats();
    expect(after.todo).toBe(before.todo - 1);
    expect(after.dismissed).toBe(before.dismissed + 1);
  });
});

describe("mockApi findings bulk dismiss + undo (PRD #1183 M3)", () => {
  it("bulk-dismisses over disposition ids, skipping non-open and foreign ids silently", async () => {
    const api = await freshApi();
    // disp-1 + disp-2 are open; disp-3 is filed (skipped); an unknown id is skipped.
    const res = await api.dismissFindings(["disp-1", "disp-2", "disp-3", "disp-does-not-exist"], "wont_do");
    expect(res.updated).toBe(2);
    expect(res.findings.every((f) => f.status === "dismissed")).toBe(true);
    expect(res.findings.every((f) => f.dismiss_reason === "wont_do")).toBe(true);
    expect(new Set(res.findings.map((f) => f.disposition_id))).toEqual(new Set(["disp-1", "disp-2"]));
    // They no longer show under to_file.
    const toFile = await api.listFindings("to_file");
    expect(toFile.findings.some((f) => f.disposition_id === "disp-1")).toBe(false);
  });

  it("rejects a bulk dismiss with a bad reason (400) or over the 100-id cap (400)", async () => {
    const api = await freshApi();
    await expect(
      api.dismissFindings(["disp-1"], "nope" as unknown as "wont_do"),
    ).rejects.toMatchObject({ status: 400 });
    const tooMany = Array.from({ length: 101 }, (_, i) => `disp-${i}`);
    await expect(api.dismissFindings(tooMany, "wont_do")).rejects.toMatchObject({ status: 400 });
  });

  it("undo reopens a dismissed coordinate by disposition_id and clears the reason", async () => {
    const api = await freshApi();
    await api.dismissFindings(["disp-1"], "not_an_issue");
    const row = await api.undoDismissFinding("disp-1");
    expect(row.status).toBe("open");
    expect(row.dismiss_reason).toBeUndefined();
    // It is back under to_file.
    const toFile = await api.listFindings("to_file");
    expect(toFile.findings.some((f) => f.disposition_id === "disp-1")).toBe(true);
  });

  it("undo of a non-dismissed coordinate is a 404", async () => {
    const api = await freshApi();
    // disp-3 is filed, not dismissed.
    await expect(api.undoDismissFinding("disp-3")).rejects.toMatchObject({ status: 404 });
    await expect(api.undoDismissFinding("disp-does-not-exist")).rejects.toMatchObject({ status: 404 });
  });
});
