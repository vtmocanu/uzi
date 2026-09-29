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
// to grow (it feeds the page and the card) without reddening these.
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
    // Issue #1723: a HUMAN done (no set_via), so the plain "✓ Done" chip is exercised too.
    expect(mine.some((f) => f.status === "done" && f.set_via === undefined)).toBe(true);
    expect(mine.some((f) => f.status === "dismissed" && f.dismiss_reason != null)).toBe(true);
    // Every coordinate carries a disposition_id (the bulk-dismiss / undo key).
    expect(mine.every((f) => !!f.disposition_id)).toBe(true);
    // An evidence-less coordinate (evidence cascaded away: no File/Dismiss, but still Mark done,
    // issue #1723) and more than one repo, so grouping and the null-finding_id row are both
    // exercised by the surfaces.
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

  it("filters by repo and omits the finding_id on an evidence-less coordinate", async () => {
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
    const row = await api.undoFinding("disp-1");
    expect(row.status).toBe("open");
    expect(row.dismiss_reason).toBeUndefined();
    // It is back under to_file.
    const toFile = await api.listFindings("to_file");
    expect(toFile.findings.some((f) => f.disposition_id === "disp-1")).toBe(true);
  });

  it("undo of an open or filed coordinate is a 404", async () => {
    const api = await freshApi();
    // disp-3 is filed, disp-1 is open: neither carries an undoable verdict.
    await expect(api.undoFinding("disp-3")).rejects.toMatchObject({ status: 404 });
    await expect(api.undoFinding("disp-1")).rejects.toMatchObject({ status: 404 });
    await expect(api.undoFinding("disp-does-not-exist")).rejects.toMatchObject({ status: 404 });
  });
});

describe("mockApi findings human Mark done + Undo (issue #1723)", () => {
  it("single Mark done keys on the evidence id and returns the disposition id", async () => {
    const api = await freshApi();
    const res = await api.markFindingDone("find-1");
    expect(res).toEqual({ status: "done", disposition_id: "disp-1" });
    const done = await api.listFindings("done");
    const row = done.findings.find((f) => f.disposition_id === "disp-1");
    expect(row?.status).toBe("done");
    // A human done carries no set_via (only the issue-close sync stamps one).
    expect(row).toBeTruthy();
    expect(row?.set_via).toBeUndefined();
    await expect(api.markFindingDone("find-does-not-exist")).rejects.toMatchObject({ status: 404 });
  });

  it("bulk Mark done applies open, filed and dismissed rows and returns only those", async () => {
    const api = await freshApi();
    // disp-1 open, disp-3 filed, disp-5 dismissed (reason cleared), unknown skipped, duplicate deduped.
    const res = await api.markFindingsDone(["disp-1", "disp-3", "disp-5", "disp-1", "disp-does-not-exist"]);
    expect(res.updated).toBe(3);
    expect(new Set(res.findings.map((f) => f.disposition_id))).toEqual(new Set(["disp-1", "disp-3", "disp-5"]));
    expect(res.findings.every((f) => f.status === "done" && f.set_via === undefined)).toBe(true);
    expect(res.findings.every((f) => f.dismiss_reason === undefined)).toBe(true);
    // The filed row keeps its issue link, so Undo can return it to filed.
    expect(res.findings.find((f) => f.disposition_id === "disp-3")?.filed_issue_iid).toBe(512);
    const tooMany = Array.from({ length: 101 }, (_, i) => `disp-${i}`);
    await expect(api.markFindingsDone(tooMany)).rejects.toMatchObject({ status: 400 });
  });

  it("undo of a done returns to filed when an issue link remains, else to open (never to dismissed)", async () => {
    const api = await freshApi();
    await api.markFindingsDone(["disp-3", "disp-5"]);
    const filed = await api.undoFinding("disp-3");
    expect(filed.status).toBe("filed");
    expect(filed.filed_issue_iid).toBe(512);
    expect(filed.resolved_at).toBeTruthy();
    // disp-5 was dismissed before the done: Undo returns it to open, not to its dismissal.
    const reopened = await api.undoFinding("disp-5");
    expect(reopened.status).toBe("open");
    expect(reopened.dismiss_reason).toBeUndefined();
    expect(reopened.resolved_at).toBeUndefined();
  });

  it("undo of a sync done returns to filed and moves the stats", async () => {
    const api = await freshApi();
    const before = await api.getFindingsStats();
    // disp-6 is the issue-close sync's done, still linked to #344.
    const row = await api.undoFinding("disp-6");
    expect(row.status).toBe("filed");
    expect(row.set_via).toBeUndefined();
    const after = await api.getFindingsStats();
    expect(after.done).toBe(before.done - 1);
    expect(after.filed).toBe(before.filed + 1);
  });
});

describe("mockApi grouped filing (issue #1724)", () => {
  const openUzi = mine.filter((f) => f.status === "open" && f.finding_id && f.repo_id === "repo-uzi");
  const openOther = mine.find((f) => f.status === "open" && f.finding_id && f.repo_id !== "repo-uzi");

  it("fixture has two open uzi rows and an open row in another repo", () => {
    expect(openUzi.length).toBeGreaterThanOrEqual(2);
    expect(openOther).toBeTruthy();
  });

  it("drafts every member's location and title, then files them all under one shared issue", async () => {
    const api = await freshApi();
    const ids = openUzi.slice(0, 2).map((f) => f.disposition_id);
    const draft = await api.findingGroupIssueDraft(ids);
    expect(draft.repo_id).toBe("repo-uzi");
    expect(draft.disposition_ids).toEqual(ids);
    for (const f of openUzi.slice(0, 2)) {
      expect(draft.description).toContain(f.location);
      expect(draft.description).toContain(f.last_title);
    }

    const res = await api.fileFindingGroup({ ids, title: "edited" });
    expect(res.phase).toBe("settled");
    expect(res.disposition_ids).toEqual(ids);
    expect(res.issue?.title).toBe("edited");

    const filed = (await api.listFindings("filed")).findings.filter((f) => ids.includes(f.disposition_id ?? ""));
    expect(filed).toHaveLength(2);
    expect(new Set(filed.map((f) => f.filed_issue_iid))).toEqual(new Set([res.issue?.iid]));
    expect(new Set(filed.map((f) => f.filed_issue_url))).toEqual(new Set([res.issue?.web_url]));

    // A second attempt on the now-filed members is the 409.
    await expect(api.fileFindingGroup({ ids })).rejects.toMatchObject({ status: 409 });
  });

  it("mirrors the server errors: mixed repos 400, unknown 404, non-fileable 409, bad count 400", async () => {
    const api = await freshApi();
    const [a] = openUzi;
    await expect(
      api.findingGroupIssueDraft([a.disposition_id, openOther!.disposition_id]),
    ).rejects.toMatchObject({ status: 400, message: "mixed repositories" });
    await expect(api.findingGroupIssueDraft([a.disposition_id, "nope"])).rejects.toMatchObject({ status: 404 });
    const filed = mine.find((f) => f.status === "filed")!;
    await expect(api.findingGroupIssueDraft([a.disposition_id, filed.disposition_id])).rejects.toMatchObject({
      status: 409,
    });
    await expect(api.findingGroupIssueDraft([])).rejects.toMatchObject({ status: 400 });
    const many = Array.from({ length: 51 }, (_, i) => `id-${i}`);
    await expect(api.findingGroupIssueDraft(many)).rejects.toMatchObject({ status: 400 });
  });
});
