import { beforeEach, expect, it, vi } from "vitest";
import { crossChecksApi } from "./mockApi/crossChecks";
import type { CrossCheckHistoryAttemptDTO as Attempt } from "../lib/apiTypes";
vi.mock("./mockApi/shared", () => ({
  requireSession: vi.fn(() => ({ id: "owner" })),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: () => "cross-check-history",
}));
beforeEach(() => vi.clearAllMocks());
const identities = (attempts: Attempt[]) => attempts.map(a => a.stage + ":" + a.round);
const plans = (first: number, last: number) => Array.from({ length: last - first + 1 }, (_, i) => `plan:${first + i}`);

it("retains exactly 31 Plan rounds and Code 1 through bounded chronological windows", async () => {
  expect(await crossChecksApi.crossCheckHistoryAvailability()).toEqual({ available: true });
  const page = await crossChecksApi.listCrossCheckHistory();
  expect(page.groups.map(g => g.parent_id)).toEqual([
    "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001",
  ]);
  const group = page.groups[0];
  expect(identities(group.attempts)).toEqual([...plans(23, 31), "code:1"]);
  expect(group.total_attempts).toBe(32);
  expect(group.matching_attempts).toBe(32);
  expect(group.active_attempts).toBe(3);
  expect(group.active_hidden).toBe(1);
  expect(group.active_context).toBe("Additional active attempts are filtered or outside the loaded window.");
  expect(group.omission).toBe("Earlier rounds omitted: load older attempts");
  expect(group.attempts.slice(-2).map(a => [a.execution, a.child_status])).toEqual([
    ["running", "running"], ["running", "running"],
  ]);
  const windows = [group.attempts];
  let cursor = group.older_cursor;
  // Three older windows exhaust the remaining 22 identities; never retry a cursor.
  for (const expected of [plans(13, 22), plans(3, 12), plans(1, 2)]) {
    expect(cursor).toBeTruthy();
    const older = await crossChecksApi.listCrossCheckHistory({ attempt_cursor: [cursor!] });
    expect(older.groups).toHaveLength(1);
    expect(older.next_cursor).toBeUndefined();
    expect(identities(older.groups[0].attempts)).toEqual(expected);
    windows.unshift(older.groups[0].attempts);
    cursor = older.groups[0].older_cursor;
  }
  expect(cursor).toBeUndefined();
  const all = windows.flat();
  expect(identities(all)).toEqual([...plans(1, 31), "code:1"]);
  expect(new Set(identities(all)).size).toBe(32);
  expect(all[0]).toMatchObject({ active: true, execution: "running" });
  for (const a of all.filter(a => ["running", "finished"].includes(a.execution))) {
    expect(a.child_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(a.model).toBe("gpt-5.4");
    expect(a.effort).toBe("high");
    expect(a.child_status).toBe(a.execution === "running" ? "running" : "completed");
  }
  expect(new Set(all.flatMap(a => a.child_id ? [a.child_id] : [])).size).toBe(5);
});

it("orders group ties by descending UUID and paginates without repeats", async () => {
  const first = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past" });
  expect(first.groups).toHaveLength(25);
  expect(first.next_cursor).toBeTruthy();
  const second = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", cursor: first.next_cursor });
  expect(second.groups).toHaveLength(2);
  expect(second.next_cursor).toBeUndefined();
  expect([...first.groups, ...second.groups].map(g => g.parent_id)).toEqual(
    Array.from({ length: 27 }, (_, i) => `00000000-0000-4000-8000-${String(29 - i).padStart(12, "0")}`),
  );
});

it("enforces the 20 attempt cap and eight-cursor continuation vector", async () => {
  const page = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", attempt_limit: 999 });
  for (const g of page.groups) {
    expect(identities(g.attempts)).toEqual([...plans(13, 31), "code:1"]);
    expect(g.older_cursor).toBeTruthy();
  }
  const cursors = page.groups.slice(0, 8).map(g => g.older_cursor!);
  const older = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", attempt_limit: 20, attempt_cursor: cursors });
  expect(older.groups.map(g => g.parent_id)).toEqual(page.groups.slice(0, 8).map(g => g.parent_id));
  for (const g of older.groups) {
    expect(identities(g.attempts)).toEqual(plans(1, 12));
    expect(g.older_cursor).toBeUndefined();
  }
  await expect(crossChecksApi.listCrossCheckHistory({
    lifecycle: "past", attempt_cursor: page.groups.slice(0, 9).map(g => g.older_cursor!),
  })).rejects.toMatchObject({ status: 400 });
});

it("filters stages and outcomes while preserving active context and safe Code counts", async () => {
  const plan = await crossChecksApi.listCrossCheckHistory({ stage: "plan", attempt_limit: 2 });
  expect(plan.groups).toHaveLength(2);
  expect(identities(plan.groups[0].attempts)).toEqual(plans(30, 31));
  expect(plan.groups[0].active_attempts).toBe(3);
  expect(plan.groups[0].active_hidden).toBe(2);
  const older = await crossChecksApi.listCrossCheckHistory({
    stage: "plan", attempt_limit: 2, attempt_cursor: plan.groups.map(g => g.older_cursor!),
  });
  expect(older.groups.map(g => identities(g.attempts))).toEqual([plans(28, 29), plans(28, 29)]);
  const code = await crossChecksApi.listCrossCheckHistory({ stage: "code" });
  expect(code.groups.map(g => identities(g.attempts))).toEqual([["code:1"], ["code:1"]]);
  expect(code.groups[0].active_hidden).toBe(2);
  const failed = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", outcome: "didnt_finish" });
  expect(failed.groups).toHaveLength(25);
  for (const g of failed.groups) {
    expect(identities(g.attempts)).toEqual(plans(22, 31));
    expect(g.attempts.every(a => a.child_id === null && a.child_status === null && a.execution === "couldnt_start")).toBe(true);
  }
  for (const [outcome, review, count] of [["passed", "approved", 0], ["raised_issues", "changes_requested", 2]] as const) {
    const result = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", stage: "code", outcome });
    expect(result.groups).toHaveLength(outcome === "passed" ? 14 : 13);
    for (const g of result.groups) {
      expect(identities(g.attempts)).toEqual(["code:1"]);
      expect(g.attempts[0]).toMatchObject({ result: "completed", execution: "finished", child_status: "completed", review, findings_count: count });
      expect(g.attempts[0]).not.toHaveProperty("findings");
    }
  }
  const replaced = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", outcome: "replaced" });
  expect(replaced.groups).toHaveLength(25);
  expect(replaced.groups.map(g => identities(g.attempts))).toEqual(Array.from({ length: 25 }, () => ["plan:3"]));
});

it("classifies a Plan verdict recorded before custody loss by its verdict, not as replaced", async () => {
  const replaced = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", stage: "plan", outcome: "replaced" });
  expect(replaced.groups).toHaveLength(25);
  expect(replaced.groups.map(g => identities(g.attempts))).toEqual(Array.from({ length: 25 }, () => ["plan:3"]));
  expect(replaced.groups[0].attempts[0]).toMatchObject({ review: "approved", execution: "replaced", historical: true });
  const passed = await crossChecksApi.listCrossCheckHistory({ lifecycle: "past", stage: "plan", outcome: "passed" });
  expect(passed.groups).toHaveLength(25);
  for (const g of passed.groups) {
    expect(identities(g.attempts)).toEqual(["plan:4"]);
    expect(g.attempts[0]).toMatchObject({
      review: "approved", execution: "finished", historical: true,
      decided_at: "2026-10-30T10:30:00Z", interrupted_at: "2026-10-30T10:45:00Z",
    });
  }
});
