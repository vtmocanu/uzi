// @vitest-environment jsdom
//
// Overview admin health card (PRD #1484 M5). Rendered standalone, so the shared hook's
// fallback self-fetches the mocked GET /api/admin/health (the AdminHealth.test.tsx pattern).
// The card is selected by its SCOPED handle (aria-label="System health"), never a bare
// getByRole("status") — RateLimitAnnouncer owns one.
//
// Covered: quiet one-liner when every check passes; verdict + top-three attention items when
// not.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { HealthOverviewCard } from "./HealthOverviewCard";
import { api } from "../lib/api";
import { healthySilentDoc, incidentDoc } from "../mocks/data/health";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderCard() {
  return render(
    <MemoryRouter>
      <HealthOverviewCard />
    </MemoryRouter>,
  );
}

describe("HealthOverviewCard", () => {
  it("collapses to one quiet line when every check passes", async () => {
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc()); // 14 checks, all ok
    renderCard();
    // A healthy card is a status region (not an alert), scoped by its label.
    const card = await screen.findByRole("status", { name: "System health" });
    expect(within(card).getByText("System health: all 14 checks passing")).toBeTruthy();
    // No verdict/attention items in the quiet form.
    expect(within(card).queryByText(/uzi cannot run work/)).toBeNull();
    expect(within(card).getByRole("link", { name: "Open health" })).toBeTruthy();
  });

  it("shows the verdict and the top three attention items when checks need attention", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc()); // 3 danger + 1 warn = 4 attention
    renderCard();
    // Danger → alert role (CustodyBoardAlert's conditional role), scoped by label.
    const card = await screen.findByRole("alert", { name: "System health" });
    expect(within(card).getByText("System health: uzi cannot run work: 3 blocking checks")).toBeTruthy();
    // The three worst (danger) checks are listed by title; the 4th (warn) folds into "and 1 more".
    expect(within(card).getByText("Worker image roll")).toBeTruthy();
    expect(within(card).getByText("Worker capacity")).toBeTruthy();
    expect(within(card).getByText("Runs waiting for a worker")).toBeTruthy();
    expect(within(card).getByText("and 1 more")).toBeTruthy();
  });
});
