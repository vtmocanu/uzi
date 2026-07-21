// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Notifications } from "./Notifications";
import { api, type Notification } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Mock only the network + auth; keep the real notifications helpers so the
// title/body convention and the change event run for real.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listNotifications: vi.fn(), markNotificationRead: vi.fn(), unreadNotificationCount: vi.fn() },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function aNotif(over: Partial<Notification> = {}): Notification {
  return {
    id: "ntf-1",
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues" },
    run_id: "run-done",
    review_id: null,
    read_at: null,
    created_at: "2026-07-05T12:00:00Z",
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "u1", is_admin: false } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Notifications inbox (PRD #46 M2)", () => {
  it("renders own notifications with the payload title/body and a run deep link", async () => {
    // A NON-judge kind, deliberately (PRD #98 M5). This assertion used to ride a
    // `judge_review` row, and after the retarget that made it a test of the judge path
    // wearing the generic path's name — the exact confusion Decision 4 warns about.
    mockApi.listNotifications.mockResolvedValue({
      notifications: [aNotif({ kind: "run_failed", payload: { title: "Run failed", body: "exit 1" } })],
      unread: 1,
      total: 1,
    });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Run failed")).toBeTruthy());
    expect(screen.getByText("exit 1")).toBeTruthy();
    const link = screen.getByText("· Open run").closest("a");
    expect(link?.getAttribute("href")).toBe("/runs/run-done");
    // Own view was requested (no all-scope), paged from the first offset.
    expect(mockApi.listNotifications).toHaveBeenCalledWith({ limit: 30, offset: 0 });
  });

  it("falls back to a humanized kind when the payload has no title", async () => {
    mockApi.listNotifications.mockResolvedValue({
      notifications: [aNotif({ payload: {} })],
      unread: 1,
      total: 1,
    });
    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("judge review")).toBeTruthy());
  });

  it("marks a notification read and drops it from the unread state", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [aNotif()], unread: 1, total: 1 });
    mockApi.markNotificationRead.mockResolvedValue({
      notification: aNotif({ read_at: "2026-07-05T13:00:00Z" }),
    });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Mark read")).toBeTruthy());
    fireEvent.click(screen.getByText("Mark read"));

    await waitFor(() => expect(mockApi.markNotificationRead).toHaveBeenCalledWith("ntf-1"));
    // The row flips to "read" and the mark-read control is gone.
    await waitFor(() => expect(screen.getByText("read")).toBeTruthy());
    expect(screen.queryByText("Mark read")).toBeNull();
  });

  it("hides the admin scope toggle for a non-admin", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [], unread: 0, total: 0 });
    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.listNotifications).toHaveBeenCalled());
    expect(screen.queryByText("All users")).toBeNull();
  });

  it("admin can switch to the all-users view, which shows the owner and requests all=1", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { id: "admin", is_admin: true } } as unknown as ReturnType<typeof useAuth>);
    mockApi.listNotifications
      .mockResolvedValueOnce({ notifications: [], unread: 0, total: 0 }) // initial own view
      .mockResolvedValueOnce({
        notifications: [aNotif({ owner: { id: "u-mira", email: "mira@uzi.local", display_name: "Mira Ionescu" } })],
        unread: 0,
        total: 1,
      });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("All users")).toBeTruthy());
    fireEvent.click(screen.getByText("All users"));

    await waitFor(() => expect(screen.getByText("Mira Ionescu")).toBeTruthy());
    expect(mockApi.listNotifications).toHaveBeenLastCalledWith({ all: true, limit: 30, offset: 0 });
    // Another user's row offers no mark-read control (own-row only).
    expect(screen.queryByText("Mark read")).toBeNull();
  });

  // PRD #98 Remaining Work: the inbox was the ONLY judge-text renderer with no escaping pin.
  // The property already HELD — React escapes it — but nothing asserted it, and the other two
  // renderers both carry the pin (RunView.test.tsx "renders review free text as escaped text,
  // never HTML"; Judge.test.tsx "renders markup in the preview as literal characters").
  //
  // Why the inbox needed it MORE than the others rather than less: `payload.body` carries
  // `reviewSummaryPreview(sub.SummaryMd)` — scrubbed and length-capped at ingest, but
  // untrusted judge free text by the producer's own description — and M5 made this a
  // first-class judge surface. The pre-flag that produced the other two pins was explicitly
  // about a future author reaching for a markdown renderer; this was the one place that would
  // not have failed.
  //
  // Same payload as the other two on purpose: one string, three renderers, so a reader can
  // see they are asserting the same property.
  it("renders judge free text as escaped text, never HTML", async () => {
    mockApi.listNotifications.mockResolvedValue({
      notifications: [
        aNotif({
          payload: { title: "Run review ready", body: "<img src=x onerror=alert(1)> **not bold**" },
        }),
      ],
      unread: 1,
      total: 1,
    });

    const { container } = render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    // Wait on the TITLE, not on the text under test: the row must be rendered before either
    // assertion means anything, and awaiting the escaped body would put the text query first
    // again. A bare `expect(container.querySelector("img")).toBeNull()` with no await ahead of
    // it passes VACUOUSLY — nothing has rendered yet, so of course there is no <img>.
    await screen.findByText("Run review ready");
    // THE SECURITY PROPERTY, ASSERTED FIRST so it is the first red. The markup never became a
    // real element (no dangerouslySetInnerHTML / markdown parsing on judge output).
    expect(container.querySelector("img")).toBeNull();
    // ...and it is present as literal characters. Both halves of the payload are required:
    // raw HTML breaks the first, a markdown renderer breaks the second.
    expect(screen.getByText(/<img src=x onerror=alert\(1\)> \*\*not bold\*\*/)).toBeTruthy();
  });

  // The GROUPED path renders the same NotificationRow, but M5 is what put judge text behind
  // an expander, so the surface is asserted where it is actually reachable rather than
  // inferred from the component being shared.
  it("escapes judge free text inside an expanded group too", async () => {
    const evil = "<img src=x onerror=alert(1)> **not bold**";
    mockApi.listNotifications.mockResolvedValue({
      notifications: [
        aNotif({ id: "j1", payload: { title: "Run review ready", body: evil }, created_at: "2026-07-20T12:00:00Z" }),
        aNotif({ id: "j2", payload: { title: "Run review ready", body: evil }, created_at: "2026-07-20T11:59:00Z" }),
      ],
      unread: 2,
      total: 2,
    });

    const { container } = render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("2 reviews ready")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { expanded: false }));

    // Same ordering rule as above, and the same reason for awaiting the title instead.
    expect((await screen.findAllByText("Run review ready")).length).toBe(2);
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getAllByText(/<img src=x onerror=alert\(1\)> \*\*not bold\*\*/).length).toBe(2);
  });

  it("deep-links a judge review into the Judge workbench, anchored to its run", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [aNotif()], unread: 1, total: 1 });
    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Run review ready")).toBeTruthy());
    const link = screen.getByText("· Open in Judge").closest("a");
    expect(link?.getAttribute("href")).toBe("/judge?run=run-done");
    // ...and the generic run link is not also rendered.
    expect(screen.queryByText("· Open run")).toBeNull();
  });

  it("collapses consecutive judge reviews into one expandable header (Decision 5)", async () => {
    // DISTINCT run ids per row, which is what makes the href assertions below
    // discriminating rather than decorative — see the comment at those assertions.
    const rows = [
      aNotif({ id: "j1", run_id: "run-one", payload: { title: "review one", body: "" }, created_at: "2026-07-20T12:00:00Z" }),
      aNotif({ id: "j2", run_id: "run-two", payload: { title: "review two", body: "" }, created_at: "2026-07-20T11:59:00Z" }),
      aNotif({ id: "j3", run_id: "run-three", payload: { title: "review three", body: "" }, created_at: "2026-07-20T11:58:00Z" }),
    ];
    mockApi.listNotifications.mockResolvedValue({ notifications: rows, unread: 3, total: 3 });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("3 reviews ready")).toBeTruthy());
    // Collapsed: the individual rows are not on screen...
    expect(screen.queryByText("review one")).toBeNull();
    // ...and no to-triage number is shown, because no AppShell supplied one. A rendered 0
    // here would be the page asserting "nothing to triage" on its own authority.
    expect(screen.queryByText(/to triage/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { expanded: false }));

    // Expanded shows exactly what the ungrouped inbox showed — same rows, same controls.
    await waitFor(() => expect(screen.getByText("review one")).toBeTruthy());
    expect(screen.getByText("review two")).toBeTruthy();
    expect(screen.getByText("review three")).toBeTruthy();
    expect(screen.getAllByText("Mark read")).toHaveLength(3);

    // THE ANCHORED DEEP-LINK'S RENDER PATH, asserted where it is actually taken.
    // `notificationLink` is pinned as a pure function (lib/notifications.test.ts) and the
    // ungrouped DOM path is pinned by "deep-links a judge review …" above — but judge pings
    // arrive in BURSTS and get grouped, so the anchored row a user really clicks is one
    // inside this expander, and nothing here asserted an href. (The expander does render the
    // per-notification anchor: it renders the same NotificationRow. The claim that the
    // anchored path was "reachable only from an ungrouped row" was false — this is the
    // assertion that keeps it that way.)
    //
    // The three run ids DIFFER on purpose. With one shared id, folding every member's anchor
    // to the group's first row would satisfy a per-row assertion unchanged; distinct ids are
    // what make these three lines discriminate.
    const rowLink = (title: string) =>
      within(screen.getByText(title).closest("li")!).getByText("· Open in Judge").closest("a");
    expect(rowLink("review one")?.getAttribute("href")).toBe("/judge?run=run-one");
    expect(rowLink("review two")?.getAttribute("href")).toBe("/judge?run=run-two");
    expect(rowLink("review three")?.getAttribute("href")).toBe("/judge?run=run-three");
    // …and the HEADER deliberately carries NO anchor: it spans several runs, so anchoring it
    // to any one of them would misrepresent what was clicked. Header → `/judge` (todo bucket),
    // row → `/judge?run={id}` (all bucket): the two are different claims and this pins both.
    expect(screen.getByText("Open Judge").closest("a")?.getAttribute("href")).toBe("/judge");
  });

  it("pages the inbox with Load more (offset paging), then hides the button", async () => {
    // Non-judge rows, so paging is measured on its own. Thirty consecutive judge_review
    // rows would collapse into ONE group header (PRD #98 M5, Decision 5) and every
    // per-row assertion here would be asserting the grouping instead of the paging.
    const page1 = Array.from({ length: 30 }, (_, i) =>
      aNotif({ id: `p1-${i}`, kind: "run_failed", payload: { title: `row ${i}`, body: "" } }),
    );
    const page2 = [
      aNotif({ id: "p2-0", kind: "run_failed", payload: { title: "second page row", body: "" } }),
    ];
    mockApi.listNotifications
      .mockResolvedValueOnce({ notifications: page1, unread: 0, total: 31 })
      .mockResolvedValueOnce({ notifications: page2, unread: 0, total: 31 });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("row 0")).toBeTruthy());
    fireEvent.click(screen.getByText(/Load more/));

    await waitFor(() => expect(screen.getByText("second page row")).toBeTruthy());
    // The next page was fetched at offset = the count already shown.
    expect(mockApi.listNotifications).toHaveBeenLastCalledWith({ limit: 30, offset: 30 });
    // All 31 rows are now loaded, so the button is gone.
    expect(screen.queryByText(/Load more/)).toBeNull();
  });
});
