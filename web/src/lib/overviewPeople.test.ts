import { describe, expect, it } from "vitest";
import type { AdminUsage, RunStatus, RunSummaryItem, User } from "./api";
import { mergeOverviewRows, overviewCounts } from "./overview";
import {
  ADMIN_RUNS_CAP,
  executingPeople,
  idleText,
  peopleModel,
  peopleRows,
  pillText,
  pillTone,
  runPart,
} from "./overviewPeople";

const NOW = Date.parse("2026-11-08T12:00:00Z");
const HOUR = 3600 * 1000;
const DAY = 24 * HOUR;
const ME = "me@x.io";

let seq = 0;
function run(over: Partial<RunSummaryItem> & { status: RunStatus }): RunSummaryItem {
  return { id: `r-${++seq}`, health: "ok", ...over } as RunSummaryItem;
}

function user(email: string, over: Partial<User> = {}): User {
  return { id: `u-${email}`, email, display_name: null, is_admin: false, is_active: true, last_login: null, ...over } as User;
}

function ago(ms: number): string {
  return new Date(NOW - ms).toISOString();
}

function model(over: Partial<Parameters<typeof peopleModel>[0]> = {}) {
  return peopleModel({
    ownRuns: [],
    adminRuns: { runs: [], failed: false, lastOkAt: null },
    users: [],
    usersFailed: false,
    viewer: { email: ME },
    adminUsage: null,
    usageTargetAvailable: true,
    now: NOW,
    demo: false,
    ...over,
  });
}

function admin(runs: RunSummaryItem[]) {
  return { runs, failed: false, lastOkAt: NOW };
}

function pillFor(runs: RunSummaryItem[]): string {
  const m = model({ adminRuns: admin(runs.map((r) => ({ ...r, owner_email: "a@x.io" }))) });
  if (m.active.state !== "ok") throw new Error("not ok");
  return m.active.people.map((p) => p.pill).join("|");
}

describe("pill text", () => {
  it("lists running and queued", () => {
    expect(pillFor([run({ status: "running" }), run({ status: "queued" })])).toBe("1 running · 1 queued");
  });

  it("adds waits on them for a run parked for its owner", () => {
    expect(pillFor([run({ status: "running" }), run({ status: "queued" }), run({ status: "paused" })])).toBe(
      "1 running · 1 queued · waits on them",
    );
  });

  it("calls an automatic park waiting", () => {
    expect(pillFor([run({ status: "limit_wait" })])).toBe("1 waiting");
  });

  it("says waits on them for plan approval, a question or a follow-up alone", () => {
    for (const status of ["awaiting_approval", "awaiting_input", "awaiting_followup"] as const) {
      expect(pillFor([run({ status })])).toBe("waits on them");
    }
  });

  it("counts a revising plan as running", () => {
    expect(pillFor([run({ status: "awaiting_approval", is_revising: true })])).toBe("1 running");
  });

  it("counts an account cooldown once, as waiting", () => {
    expect(pillFor([run({ status: "running", health: "waiting_account_cooldown" })])).toBe("1 waiting");
  });

  it("orders all four parts and derives the tone", () => {
    const p = { running: 2, queued: 1, waiting: 3, owner: 1 };
    expect(pillText(p)).toBe("2 running · 1 queued · 3 waiting · waits on them");
    expect(pillTone(p)).toBe("warn");
    expect(pillTone({ ...p, owner: 0 })).toBe("ok");
    expect(pillTone({ running: 0, queued: 1, waiting: 0, owner: 0 })).toBe("neutral");
  });

  it("ignores terminal runs and treats an unknown status as the owner's", () => {
    expect(runPart(run({ status: "completed" }))).toBeNull();
    expect(runPart(run({ status: "future_state" as RunStatus }))).toBe("owner");
  });
});

describe("peopleRows", () => {
  it("keeps a viewer-owned admin row the personal list lacks, and prefers the personal row on a duplicate id", () => {
    const own = run({ id: "a", status: "queued" });
    const dup = run({ id: "a", status: "running", owner_email: "them@x.io" });
    const mine = run({ id: "b", status: "running", owner_email: ME });
    const rows = peopleRows([own], [dup, mine], ME);
    expect(rows.map((r) => [r.run.id, r.owner])).toEqual([
      ["a", ME],
      ["b", ME],
    ]);
    expect(rows[0].run.status).toBe("queued");
  });

  it("makes the viewer active, once, when their run is only in the admin rows", () => {
    const m = model({ adminRuns: admin([run({ status: "running", owner_email: ME })]), users: [user(ME)] });
    if (m.active.state !== "ok") throw new Error("not ok");
    expect(m.active.people).toHaveLength(1);
    expect(m.active.people[0]).toMatchObject({ key: ME, you: true, pill: "1 running" });
  });
});

describe("executingPeople", () => {
  it("never exceeds the executing count of the merged rows, including a viewer run only in the admin response", () => {
    const own = [run({ status: "queued" })];
    const adminRows = [
      run({ status: "running", owner_email: ME }),
      run({ status: "running", owner_email: ME }),
      run({ status: "running", owner_email: "a@x.io" }),
      run({ status: "running", owner_email: "b@x.io", health: "waiting_account_cooldown" }),
      run({ status: "completed", owner_email: "c@x.io" }),
      // Running in its owner's pill, but not executing in the sentence.
      run({ status: "awaiting_approval", is_revising: true, owner_email: "d@x.io" }),
    ];
    const merged = mergeOverviewRows(own, adminRows, ME);
    const people = executingPeople(merged, ME);
    expect(people).toBeLessThanOrEqual(overviewCounts(merged).executing);
    // mergeOverviewRows drops the viewer's admin-only rows, so neither side counts them.
    expect(overviewCounts(merged).executing).toBe(1);
    expect(people).toBe(1);
  });

  it("counts a personal row (no owner_email) as the viewer's", () => {
    const rows = [run({ status: "running" }), run({ status: "running" }), run({ status: "running", owner_email: ME })];
    expect(executingPeople(rows, ME)).toBe(1);
  });
});

describe("active people", () => {
  it("reports 4 owners of 9 accounts with the viewer first, one recent sign-in and an idle summary", () => {
    const users = [
      user(ME, { display_name: "Zed You" }),
      user("a@x.io", { display_name: "Alice Smith" }),
      user("b@x.io", { display_name: "Bob Jones" }),
      user("c@x.io", { display_name: "Cara Lee" }),
      user("d@x.io", { last_login: ago(2 * HOUR) }),
      user("e@x.io", { last_login: ago(3 * DAY + HOUR) }),
      user("f@x.io", { last_login: ago(10 * DAY) }),
      user("g@x.io", { last_login: null }),
      user("h@x.io", { is_active: false }),
      user("i@x.io"),
    ];
    const m = model({
      users,
      adminRuns: admin([
        run({ status: "running", owner_email: ME }),
        run({ status: "running", owner_email: "a@x.io" }),
        run({ status: "queued", owner_email: "b@x.io" }),
        run({ status: "paused", owner_email: "c@x.io" }),
      ]),
    });
    if (m.active.state !== "ok") throw new Error("not ok");
    expect(m.active.count).toBe(4);
    expect(m.active.people.map((p) => p.name)).toEqual(["Zed You", "Alice Smith", "Bob Jones", "Cara Lee"]);
    expect(m.active.people[0].you).toBe(true);
    expect(m.active.people[3].pill).toBe("waits on them");
    expect(m.total).toBe(9);
    expect(m.signedIn).toEqual([{ key: "d@x.io", name: "d@x.io", initials: "D", you: false, ago: "2h ago" }]);
    expect(m.idle).toEqual({ count: 2, minDays: 3, maxDays: 10, never: 2 });
    expect(idleText(m.idle!)).toBe("2 people, last seen 3 to 10 days ago · 2 never signed in");
  });

  it("counts a deactivated owner and an owner without a user row in count and total", () => {
    const m = model({
      users: [user("a@x.io"), user("gone@x.io", { is_active: false })],
      adminRuns: admin([
        run({ status: "running", owner_email: "gone@x.io" }),
        run({ status: "running", owner_email: "ghost@x.io" }),
      ]),
    });
    if (m.active.state !== "ok") throw new Error("not ok");
    expect(m.active.count).toBe(2);
    expect(m.total).toBe(3);
    expect(m.active.people.find((p) => p.key === "gone@x.io")?.deactivated).toBe(true);
    expect(m.active.people.find((p) => p.key === "ghost@x.io")?.deactivated).toBe(false);
    expect(m.active.count).toBeLessThanOrEqual(m.total!);
  });

  it("marks the list as at-least from exactly 500 rows", () => {
    const rows = (n: number) => Array.from({ length: n }, () => run({ status: "completed", owner_email: "a@x.io" }));
    const at = model({ adminRuns: admin(rows(ADMIN_RUNS_CAP)) });
    const under = model({ adminRuns: admin(rows(ADMIN_RUNS_CAP - 1)) });
    expect(at.active).toMatchObject({ state: "ok", atLeast: true });
    expect(under.active).toMatchObject({ state: "ok", atLeast: false });
  });

  it("never shows stale people when the admin list failed, and loads while it is pending", () => {
    const stale = [run({ status: "running", owner_email: "a@x.io" })];
    expect(model({ adminRuns: { runs: null, failed: true, lastOkAt: null } }).active).toEqual({ state: "unavailable" });
    expect(model({ adminRuns: { runs: stale, failed: true, lastOkAt: NOW } }).active).toEqual({ state: "unavailable" });
    expect(model({ adminRuns: { runs: null, failed: false, lastOkAt: null } }).active).toEqual({ state: "loading" });
  });

  it("drops total, signed-in and idle when the user list failed", () => {
    const m = model({ users: [user("a@x.io")], usersFailed: true });
    expect(m.total).toBeNull();
    expect(m.signedIn).toBeNull();
    expect(m.idle).toBeNull();
    expect(model({ users: null }).total).toBeNull();
  });

  it("drops signed-in and idle when the active list is not ok", () => {
    const m = model({ users: [user("a@x.io"), user("b@x.io")], adminRuns: { runs: null, failed: true, lastOkAt: null } });
    expect(m.total).toBe(2);
    expect(m.signedIn).toBeNull();
    expect(m.idle).toBeNull();
  });
});

describe("signed in and idle", () => {
  it("splits on 24 hours and sorts the recent ones first", () => {
    const m = model({
      users: [
        user("in@x.io", { last_login: ago(23 * HOUR + 59 * 60 * 1000) }),
        user("out@x.io", { last_login: ago(24 * HOUR + 60 * 1000) }),
        user("edge@x.io", { last_login: ago(24 * HOUR) }),
        user("newer@x.io", { last_login: ago(HOUR) }),
        user("future@x.io", { last_login: new Date(NOW + HOUR).toISOString() }),
      ],
    });
    expect(m.signedIn?.map((s) => s.key)).toEqual(["future@x.io", "newer@x.io", "in@x.io"]);
    expect(m.idle).toEqual({ count: 2, minDays: 1, maxDays: 1, never: 0 });
  });

  it("marks the viewer in the signed-in group", () => {
    const m = model({ users: [user(ME, { last_login: ago(HOUR) }), user("a@x.io", { last_login: ago(HOUR) })] });
    expect(m.signedIn?.find((s) => s.key === ME)?.you).toBe(true);
    expect(m.signedIn?.find((s) => s.key === "a@x.io")?.you).toBe(false);
  });

  it("words the idle range and keeps never-signed-in out of it", () => {
    expect(idleText({ count: 1, minDays: 1, maxDays: 1, never: 0 })).toBe("1 person, last seen 1 day ago");
    expect(idleText({ count: 2, minDays: 4, maxDays: 4, never: 0 })).toBe("2 people, last seen 4 days ago");
    expect(idleText({ count: 2, minDays: 2, maxDays: 9, never: 0 })).toBe("2 people, last seen 2 to 9 days ago");
    expect(idleText({ count: 2, minDays: null, maxDays: null, never: 0 })).toBe(
      "2 people, last seen more than a day ago",
    );
    expect(idleText({ count: 0, minDays: null, maxDays: null, never: 3 })).toBe("3 never signed in");
    expect(idleText({ count: 0, minDays: null, maxDays: null, never: 0 })).toBeNull();
  });

  it("counts an unparseable last login as idle without a range", () => {
    const m = model({ users: [user("a@x.io", { last_login: "not a date" }), user("b@x.io", { last_login: ago(5 * DAY) })] });
    expect(m.idle).toEqual({ count: 2, minDays: 5, maxDays: 5, never: 0 });
  });
});

describe("week and usage action", () => {
  const usage = (rows: Array<number | undefined>) =>
    ({
      factory: {},
      earliest_run: null,
      users: rows.map((n, i) => ({ user_id: `u${i}`, email: `u${i}@x.io`, last7_run_count: n })),
    }) as unknown as AdminUsage;

  it("counts users with runs in the last 7 days", () => {
    expect(model({ adminUsage: usage([3, 0, 1, 0]) }).week).toBe(2);
  });

  it("is null without usage or when a row lacks the count", () => {
    expect(model({ adminUsage: null }).week).toBeNull();
    expect(model({ adminUsage: undefined }).week).toBeNull();
    expect(model({ adminUsage: usage([1, undefined]) }).week).toBeNull();
  });

  it("is available only with usage and a target", () => {
    expect(model({ adminUsage: usage([1]), usageTargetAvailable: true }).usageAction).toBe("available");
    expect(model({ adminUsage: usage([1]), usageTargetAvailable: false }).usageAction).toBe("unavailable");
    expect(model({ adminUsage: null, usageTargetAvailable: true }).usageAction).toBe("unavailable");
  });
});

describe("names", () => {
  const one = (u: User, demo = false) => {
    const m = model({ users: [u], demo, adminRuns: admin([run({ status: "running", owner_email: u.email })]) });
    if (m.active.state !== "ok") throw new Error("not ok");
    return m.active.people[0];
  };

  it("masks the display name and the email in demo mode", () => {
    expect(one(user("robin.diaz@x.io", { display_name: "Robin Diaz" }), true).name).toBe("Robin");
    const p = one(user("robin.diaz@x.io"), true);
    expect(p.name).toBe("Robin");
    expect(p.name).not.toContain("diaz");
  });

  it("strips bidi and zero-width characters, falling back to Unknown when nothing is left", () => {
    const bidi = String.fromCharCode(0x202e);
    const zw = String.fromCharCode(0x200b);
    expect(one(user("a@x.io", { display_name: `Ann${bidi}${zw} Lee` })).name).toBe("Ann Lee");
    expect(one(user("b@x.io", { display_name: `${bidi}${zw}` })).name).toBe("Unknown");
    const owned = model({ adminRuns: admin([run({ status: "running", owner_email: `${bidi}${zw}` })]) });
    if (owned.active.state !== "ok") throw new Error("not ok");
    expect(owned.active.people[0].name).toBe("Unknown");
  });

  it("derives initials from two tokens, an email local part or one name", () => {
    expect(one(user("a@x.io", { display_name: "Ann Lee" })).initials).toBe("AL");
    expect(one(user("jo.park@x.io")).initials).toBe("JP");
    expect(one(user("zed@x.io")).initials).toBe("Z");
    expect(one(user("q@x.io", { display_name: "Maria" })).initials).toBe("M");
  });
});
