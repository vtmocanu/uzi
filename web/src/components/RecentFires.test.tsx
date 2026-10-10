// @vitest-environment jsdom
//
// Issue #2519: a schedule row shows its recent fires that did something. The cell's badge
// and stamp come from recent_fires[0] (last_fire may be a newer capacity-blocked tick), the
// disclosure reads "Recent fires", and the panel lists the fires newest first: the newest
// open, older ones collapsed behind headers. An absent, null or empty recent_fires keeps
// today's last_fire row.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleListRow } from "./ScheduleListRow";
import { formatStamp } from "./LastRun";
import { mockApi } from "../mocks/mockApi";
import type { LastFire, Schedule } from "../lib/api";

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
afterEach(cleanup);

function row(s: Schedule) {
  return render(<MemoryRouter><table><tbody><ScheduleListRow s={s} name="Sweep"
    busy={false} addBusy={false} pauseNote={null} repos={[]} onToggle={vi.fn()}
    onRunNow={vi.fn()} onEdit={vi.fn()} onReset={vi.fn()} onClone={vi.fn()}
    onRemove={vi.fn()} onAddRepo={vi.fn()} /></tbody></table></MemoryRouter>);
}

const BLOCKED_AT = "2026-10-10T12:04:00Z";
const blocked: LastFire = { fired_at: BLOCKED_AT, matched: 0, capped: false, started: [], skips: [],
  capacity: { in_flight: 4, limit: 4, room_needed: 1, room: 0, blocked: true } };
const newest: LastFire = { fired_at: "2026-10-10T11:34:00Z", matched: 1, capped: false, skips: [],
  started: [{ issue_iid: 1011, run_id: "run-newest", title: "Newest started issue" }] };
const middle: LastFire = { fired_at: "2026-10-10T09:34:00Z", matched: 2, capped: false, started: [],
  skips: [
    { issue_iid: 1007, title: "Middle skipped issue", reason: "already_running" },
    { issue_iid: 1008, title: "Another skipped issue", reason: "fetch_failed" },
  ] };
const oldest: LastFire = { fired_at: "2026-10-09T10:00:00Z", matched: 1, capped: false, skips: [],
  started: [{ issue_iid: 1003, run_id: "run-oldest", title: "Oldest started issue" }] };

async function sweep(over: Partial<Schedule>): Promise<Schedule> {
  const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
  return { ...template, last_fired_at: BLOCKED_AT, last_fire: blocked, recent_fires: [newest, middle, oldest], ...over };
}

const disclosure = (name: string) => screen.getByRole("button", { name });

describe("schedule row with recent fires (issue #2519)", () => {
  it("shows recent_fires[0] in the cell while last_fire is a blocked tick, plus the waiting line", async () => {
    row(await sweep({}));
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(screen.getByText(`${formatStamp(newest.fired_at)} · examined 1`)).toBeTruthy();
    expect(screen.getByText(`Waiting for room at ${formatStamp(BLOCKED_AT)}`)).toBeTruthy();
    // The cell no longer shows the blocked tick's own badge (it is present when it is shown, below).
    expect(screen.queryByText("Waiting for room")).toBeNull();
    expect(disclosure("Recent fires").getAttribute("aria-expanded")).toBe("false");
  });

  it("omits the waiting line when last_fire was not blocked", async () => {
    row(await sweep({ last_fire: newest }));
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(disclosure("Recent fires")).toBeTruthy();
    expect(screen.queryByText(/Waiting for room/)).toBeNull();
  });

  it.each([
    ["empty", []],
    ["null", null],
    ["absent", undefined],
  ] as const)("keeps today's last_fire row when recent_fires is %s", async (_, recent) => {
    const s = await sweep({ recent_fires: recent as LastFire[] | null | undefined });
    if (recent === undefined) delete s.recent_fires;
    row(s);
    // Today's blocked cell: its own badge and capacity line, no extra waiting line.
    expect(screen.getByText("Waiting for room")).toBeTruthy();
    expect(screen.getByText(/space for 0 more runs; needs 1/)).toBeTruthy();
    expect(screen.queryByText(/Waiting for room at/)).toBeNull();
    expect(screen.queryByText("1 started")).toBeNull();
    fireEvent.click(disclosure("Last fire"));
    expect(disclosure("Last fire").getAttribute("aria-controls")).toBe(`last-fire-${s.id}`);
    // The panel is today's single LastFireDetail, titled "Last fire".
    const panel = document.getElementById(`last-fire-${s.id}`)!;
    expect(within(panel).getByText("Last fire")).toBeTruthy();
    expect(within(panel).getByText(/4 unfinished runs, limit 4/)).toBeTruthy();
    expect(within(panel).queryByText("Latest fire")).toBeNull();
  });

  it("renders the newest fire open, older fires collapsed, and expands an older one", async () => {
    const s = await sweep({});
    row(s);
    fireEvent.click(disclosure("Recent fires"));
    expect(disclosure("Recent fires").getAttribute("aria-controls")).toBe(`last-fire-${s.id}`);
    const panel = document.getElementById(`last-fire-${s.id}`)!;

    // The blocked note names the tick and its in-flight/limit/room numbers.
    const note = within(panel).getByRole("note");
    expect(note.textContent).toContain(formatStamp(BLOCKED_AT));
    expect(note.textContent).toContain("4 unfinished runs, limit 4 · space for 0 more runs; needs 1");

    // Newest: open through LastFireDetail.
    expect(within(panel).getByText("Latest fire")).toBeTruthy();
    expect(within(panel).getByText("Newest started issue")).toBeTruthy();

    // Older: one collapsed header each, detail not rendered.
    const list = within(panel).getByRole("list", { name: "Earlier fires" });
    const headers = within(list).getAllByRole("button");
    expect(headers).toHaveLength(2);
    expect(headers[0].textContent).toContain(formatStamp(middle.fired_at));
    expect(headers[0].textContent).toContain("0 started · 2 skipped");
    expect(headers[0].textContent).toContain("examined 2 · started 0 · skipped 2");
    expect(headers[1].textContent).toContain(formatStamp(oldest.fired_at));
    expect(headers[1].textContent).toContain("examined 1 · started 1 · skipped 0");
    for (const h of headers) {
      expect(h.getAttribute("aria-expanded")).toBe("false");
      expect(h.hasAttribute("aria-controls")).toBe(false);
    }
    expect(within(panel).queryByText("Middle skipped issue")).toBeNull();
    expect(within(panel).queryByText("Oldest started issue")).toBeNull();
    expect(within(panel).queryByText("Earlier fire")).toBeNull();

    fireEvent.click(headers[0]);
    expect(headers[0].getAttribute("aria-expanded")).toBe("true");
    const detail = document.getElementById(headers[0].getAttribute("aria-controls")!)!;
    expect(within(detail).getByText("Earlier fire")).toBeTruthy();
    expect(within(detail).getByText("Middle skipped issue")).toBeTruthy();
    // Expanding one leaves the other collapsed.
    expect(headers[1].getAttribute("aria-expanded")).toBe("false");
    expect(within(panel).queryByText("Oldest started issue")).toBeNull();
    // A second click collapses it again.
    fireEvent.click(headers[0]);
    expect(within(panel).queryByText("Middle skipped issue")).toBeNull();
  });

  it("omits the blocked note when last_fire was not blocked", async () => {
    const s = await sweep({ last_fire: newest });
    row(s);
    fireEvent.click(disclosure("Recent fires"));
    const panel = document.getElementById(`last-fire-${s.id}`)!;
    expect(within(panel).getByText("Latest fire")).toBeTruthy();
    expect(within(panel).queryByRole("note")).toBeNull();
  });

  it("renders the row and panel from recent_fires when last_fire is null", async () => {
    const s = await sweep({ last_fire: null });
    row(s);
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(screen.queryByText(/Waiting for room/)).toBeNull();
    fireEvent.click(disclosure("Recent fires"));
    const panel = document.getElementById(`last-fire-${s.id}`)!;
    expect(within(panel).getByText("Newest started issue")).toBeTruthy();
    expect(within(within(panel).getByRole("list", { name: "Earlier fires" })).getAllByRole("button")).toHaveLength(2);
  });
});

describe("recent fires mock (issue #2519)", () => {
  it("carries a blocked last_fire and three recent fires, newest first", async () => {
    const s = await mockApi.getSchedule("sch-recent-fires");
    expect(s.last_fire?.capacity?.blocked).toBe(true);
    const fires = s.recent_fires ?? [];
    expect(fires.map((f) => [f.started.length, f.skips.length])).toEqual([[1, 0], [0, 3], [2, 0]]);
    const times = fires.map((f) => Date.parse(f.fired_at));
    expect([...times].sort((a, b) => b - a)).toEqual(times);
  });
});
