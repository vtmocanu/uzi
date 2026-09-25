// @vitest-environment jsdom
//
// App-wide danger banner (PRD #1484 M5). Rendered standalone (no provider), so the shared
// hook's fallback self-fetches the mocked GET /api/admin/health — the same pattern
// AdminHealth.test.tsx uses. The banner is selected by its SCOPED handle (role="alert" +
// aria-label="Instance health"), never a bare getByRole("alert"): RateLimitAnnouncer and
// UpdateEscalationBanner own their own roles.
//
// Covered: renders only on danger; hides the Snooze button while episode_id is null; snoozes
// through the endpoint (click → POST called → banner hides); returns on a new episode_id; and
// RETURNS after the 1 h snooze lapses on a still-danger, still-open episode (D2 — the snooze
// honours 1 h, not the whole episode), driven by the injectable `now` clock.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { HealthDangerBanner } from "./HealthDangerBanner";
import { api, type HealthDoc } from "../lib/api";
import { healthySilentDoc, incidentDoc } from "../mocks/data/health";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn(), snoozeAdminHealth: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderBanner(now?: () => number) {
  return render(
    <MemoryRouter>
      <HealthDangerBanner now={now} />
    </MemoryRouter>,
  );
}

const banner = () => screen.queryByRole("alert", { name: "Instance health" });
// Re-drive the shared fallback poll (usePollWhileVisible fires tick on visibilitychange when
// the tab is visible) after swapping what getAdminHealth resolves, without fake timers.
function repoll() {
  fireEvent(document, new Event("visibilitychange"));
}

describe("HealthDangerBanner", () => {
  it("does not render when the status is not danger", async () => {
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    renderBanner();
    // Give the fallback fetch a chance to resolve, then confirm nothing showed.
    await waitFor(() => expect(mockApi.getAdminHealth).toHaveBeenCalled());
    expect(banner()).toBeNull();
  });

  it("renders on danger with the verdict and a Snooze button when an episode is open", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc());
    renderBanner();
    await waitFor(() => expect(banner()).not.toBeNull());
    // Verdict line derived from counts.danger (3 danger checks in the incident fixture).
    expect(screen.getByText("uzi cannot run work: 3 blocking checks")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Snooze 1 h" })).toBeTruthy();
    // PRD #1648 D6: the danger mark is the decorative SVG shape, not a font glyph.
    const el = banner();
    if (!el) throw new Error("banner not rendered");
    expect(el.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
    expect(el.textContent).not.toMatch(/[\u25cf\u25b2\u25c6\u25cb]/);
  });

  it("hides the Snooze button while episode_id is null (a danger doc with no open episode)", async () => {
    const noEpisode: HealthDoc = { ...incidentDoc(), episode_id: null };
    mockApi.getAdminHealth.mockResolvedValue(noEpisode);
    renderBanner();
    // The banner still shows on danger; only the Snooze action is gated on an open episode.
    await waitFor(() => expect(banner()).not.toBeNull());
    expect(screen.queryByRole("button", { name: "Snooze 1 h" })).toBeNull();
  });

  it("snoozes through the endpoint: click → POST called → banner hides", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc());
    mockApi.snoozeAdminHealth.mockResolvedValue({
      episode_id: "b1f0c2ep",
      snoozed_until: new Date(Date.now() + 3600_000).toISOString(),
    });
    renderBanner();
    await waitFor(() => expect(banner()).not.toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" }));

    await waitFor(() => expect(mockApi.snoozeAdminHealth).toHaveBeenCalledTimes(1));
    // Optimistically hidden right away (keyed to the current episode).
    await waitFor(() => expect(banner()).toBeNull());
  });

  it("returns on a new episode_id (snooze is keyed to the episode)", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc()); // episode A (b1f0c2ep)
    mockApi.snoozeAdminHealth.mockResolvedValue({
      episode_id: "b1f0c2ep",
      snoozed_until: new Date(Date.now() + 3600_000).toISOString(),
    });
    renderBanner();
    await waitFor(() => expect(banner()).not.toBeNull());

    // Snooze episode A → banner hides.
    fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" }));
    await waitFor(() => expect(banner()).toBeNull());

    // A NEW episode arrives (different episode_id, snoozed_until null): the poll re-fetches
    // and the banner returns, because the snooze was keyed to episode A.
    const episodeB: HealthDoc = { ...incidentDoc(), episode_id: "episodeB", snoozed_until: null };
    mockApi.getAdminHealth.mockResolvedValue(episodeB);
    repoll();
    await waitFor(() => expect(banner()).not.toBeNull());
    // And its Snooze button is back for the new episode.
    expect(screen.getByRole("button", { name: "Snooze 1 h" })).toBeTruthy();
  });

  it("returns after the 1 h snooze lapses on a still-danger, still-open episode", async () => {
    // A clock the test controls, so the 1 h lapse needs no real waiting. The component reads
    // it for both the server-snooze comparison and the optimistic snoozed-until (now + 1 h).
    let clock = Date.parse("2026-09-20T00:00:00Z");
    const now = () => clock;
    const snoozedUntil = new Date(clock + 3600_000).toISOString(); // base + 1 h — the server's D2 value

    // Episode open (b1f0c2ep), no server snooze yet. The endpoint confirms snoozed_until =
    // now + 1 h, exactly what the server stores (D2).
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc());
    mockApi.snoozeAdminHealth.mockResolvedValue({ episode_id: "b1f0c2ep", snoozed_until: snoozedUntil });
    renderBanner(now);
    await waitFor(() => expect(banner()).not.toBeNull());

    // Snooze → optimistic hide for 1 h.
    fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" }));
    await waitFor(() => expect(banner()).toBeNull());

    // The next poll carries the server-confirmed snooze (snoozed_until = base + 1 h). The
    // episode is unchanged and still danger, so the banner stays hidden within the hour. A
    // FRESH doc object each poll, so React re-renders and re-reads the clock (an identical
    // object reference would bail the render).
    const snoozedDoc: HealthDoc = { ...incidentDoc(), snoozed_until: snoozedUntil };
    mockApi.getAdminHealth.mockResolvedValue(snoozedDoc);
    repoll();
    await waitFor(() => expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(2));
    expect(banner()).toBeNull();

    // Advance the clock past the 1 h snooze. Same open episode, still danger, same server
    // snoozed_until (now in the past) — both snoozes have lapsed, so the banner RETURNS.
    // (Against the old episode-keyed boolean hide this assertion fails: that hid the whole
    // episode regardless of elapsed time.)
    clock += 3600_000 + 1000;
    mockApi.getAdminHealth.mockResolvedValue({ ...incidentDoc(), snoozed_until: snoozedUntil });
    repoll();
    await waitFor(() => expect(banner()).not.toBeNull());
    expect(screen.getByRole("button", { name: "Snooze 1 h" })).toBeTruthy();
  });

  // A FAILED snooze POST must not leave the banner hidden. The optimistic hide is rolled back
  // in the rejection path, so a still-danger banner returns at once rather than staying hidden
  // for the full hour with nothing persisted server-side. Fails without the rollback: the
  // optimistic now+1h deadline hides it and no poll can restore it (effectiveSnoozedUntil never
  // drops), so the banner never comes back.
  it("rolls back the optimistic snooze and keeps the banner when the POST fails", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc());
    mockApi.snoozeAdminHealth.mockRejectedValue(new Error("network"));
    renderBanner();
    await waitFor(() => expect(banner()).not.toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" }));
    await waitFor(() => expect(mockApi.snoozeAdminHealth).toHaveBeenCalled());
    // The POST rejected, so the optimistic hide is cleared and the still-danger banner stays.
    await waitFor(() => expect(banner()).not.toBeNull());
    expect(screen.getByRole("button", { name: "Snooze 1 h" })).toBeTruthy();
  });

  // The hide is time-based, so it must lapse on a TIMER, not only on the next poll. If polling
  // stalls across the deadline (useHealthPoll keeps the last-good doc and calls no setState), an
  // expiry timer is the only thing that can bring the banner back at the hour. Regression for a
  // banner that stayed hidden past its snooze during a polling outage. Fails without the timer:
  // every poll here resolves the SAME object reference, so setDoc bails and drives no re-render.
  it("re-renders and returns when the snooze deadline passes even if no further poll updates the doc", async () => {
    vi.useFakeTimers();
    try {
      const base = Date.parse("2026-09-20T00:00:00Z");
      vi.setSystemTime(base);
      const now = () => Date.now(); // fake-timer clock: setTimeout and now() advance together
      const snoozedUntil = new Date(base + 3600_000).toISOString(); // server snooze, base + 1 h (D2)
      mockApi.getAdminHealth.mockResolvedValue({ ...incidentDoc(), snoozed_until: snoozedUntil });

      render(
        <MemoryRouter>
          <HealthDangerBanner now={now} />
        </MemoryRouter>,
      );
      // Flush the initial fallback fetch; the active server snooze hides the banner.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1);
      });
      expect(banner()).toBeNull();

      // Advance past the 1 h deadline WITHOUT delivering a new doc (the mock keeps resolving the
      // same reference, so the 10 s poll re-renders nothing). Only the expiry timer can return it.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3600_000 + 1000);
      });
      expect(banner()).not.toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
