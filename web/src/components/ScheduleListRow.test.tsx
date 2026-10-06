// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleListRow } from "./ScheduleListRow";
import { mockApi } from "../mocks/mockApi";
import type { Schedule } from "../lib/api";

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
afterEach(cleanup);
function row(s: Schedule) {
  return render(<MemoryRouter><table><tbody><ScheduleListRow s={s} name="Sweep"
    busy={false} addBusy={false} pauseNote={null} repos={[]} onToggle={vi.fn()}
    onRunNow={vi.fn()} onEdit={vi.fn()} onReset={vi.fn()} onClone={vi.fn()}
    onRemove={vi.fn()} onAddRepo={vi.fn()} /></tbody></table></MemoryRouter>);
}
describe("schedule capacity options", () => {
  it.each(["user", "default"] as const)("shows gated and ungated batch options for %s rows", async (origin) => {
    const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
    const s = { ...template, origin, capacity_limit: 4, capacity_room_needed: 2, max_issues: 3 };
    const view = row(s);
    const limit = screen.getByText("limit 4 · room 2");
    expect(limit.className).toContain("text-brand");
    expect(screen.getByText("3 at a time")).toBeTruthy();
    view.unmount();
    row({ ...s, capacity_limit: null, capacity_room_needed: null, max_issues: null });
    expect(screen.getByText("no limit").className).toContain("text-muted");
    expect(screen.getByText("unlimited at a time")).toBeTruthy();
  });
  it("describes null N as available room and shows a blocked last fire", async () => {
    const s = await mockApi.getSchedule("sch-capacity-blocked");
    row({ ...s, max_issues: null });
    expect(screen.getByText("available room at a time")).toBeTruthy();
    expect(screen.getByText("Waiting for room")).toBeTruthy();
    expect(screen.getByText(/space for 1 more run; needs 2/)).toBeTruthy();
  });
});

 it.each(["user", "default"] as const)("shows removes label only for enabled removal on %s rows", async (origin) => {
   const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
   const view = row({ ...template, origin, remove_label_on_dispatch: true });
   expect(screen.getByText("removes label")).toBeTruthy();
   view.unmount();
   row({ ...template, origin, remove_label_on_dispatch: false });
   expect(screen.queryByText("removes label")).toBeNull();
 });
