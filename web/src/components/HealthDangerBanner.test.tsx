// @vitest-environment jsdom
//
// App-wide danger banner (PRD #1484 M5). Rendered standalone (no provider), so the shared
// hook's fallback self-fetches the mocked GET /api/admin/health — the same pattern
// AdminHealth.test.tsx uses. The banner is selected by its SCOPED handle (role="alert" +
// aria-label="Instance health"), never a bare getByRole("alert"): RateLimitAnnouncer and
// UpdateEscalationBanner own their own roles.
//
// Covered: renders only on danger; hides the Snooze button while episode_id is null; snoozes
// through the endpoint (click → POST called → banner hides); returns on a new episode_id.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

function renderBanner() {
  return render(
    <MemoryRouter>
      <HealthDangerBanner />
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
});
