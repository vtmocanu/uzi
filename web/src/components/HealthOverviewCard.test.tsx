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
import { healthySilentDoc, incidentDoc, ownerOnlyDoc, ownerOnlyWaitingOwnerId, ownerOnlyWaitingRunId } from "../mocks/data/health";

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
  it("shows both Codex pricing reasons and operator docs as a nonblocking warning", async () => {
    const doc = healthySilentDoc();
    expect(doc.checks).toHaveLength(19);
    const pricing = doc.checks.find((c) => c.id === "pricing.codex")!;
    pricing.severity = "warn";
    pricing.summary = "2 Codex models lack usable pricing";
    pricing.evidence = [
      { label: "gpt-5.5", value: "3 runs, no price" },
      { label: "gpt-5.6-sol", value: "2 runs, promotional price expired" },
    ];
    doc.status = "warn";
    doc.counts = { ok: 18, warn: 1, danger: 0, unknown: 0, na: 0 };
    mockApi.getAdminHealth.mockResolvedValue(doc);
    renderCard();
    const card = await screen.findByRole("status", { name: "System health" });
    expect(within(card).getByText("Codex price coverage")).toBeTruthy();
    for (const reason of ["gpt-5.5: 3 runs, no price", "gpt-5.6-sol: 2 runs, promotional price expired"]) {
      expect(within(card).getByText((_, el) => el?.tagName === "SPAN" && el.textContent === reason)).toBeTruthy();
    }
    expect(within(card).getByRole("link", { name: "Docs: admin-health" }).getAttribute("href")).toBe("/docs/admin-health");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(doc.blocking).toBe(false);
  });

  it("keeps owner danger evidence visible without an instance-wide blocker claim", async () => {
    const doc = ownerOnlyDoc();
    mockApi.getAdminHealth.mockResolvedValue(doc);
    renderCard();
    const card = await screen.findByRole("alert", { name: "System health" });
    expect(within(card).getByText("System health: 3 checks need attention; no instance-wide blocker detected")).toBeTruthy();
    expect(within(card).getByText(doc.checks.find((c) => c.id === "fleet.capacity")!.summary)).toBeTruthy();
    const items = within(card).getAllByRole("listitem");
    expect(items).toHaveLength(3);
    for (const item of items.slice(0, 2)) {
      expect(within(item).getAllByText("Waiting run:")).toHaveLength(2);
      expect(within(item).getByText((_, el) =>
        el?.tagName === "SPAN" && el.textContent === `Waiting run: run ${ownerOnlyWaitingRunId}; owner ${ownerOnlyWaitingOwnerId}; waited 36m; stored reason no online worker can run this — it needs a capability none of your workers has; provision a ca`,
      )).toBeTruthy();
      expect(within(item).getByText("Danger")).toBeTruthy();
      if (items.indexOf(item) === 0) expect(within(item).getByText("Owners affected:")).toBeTruthy();
      expect(item.textContent).toContain("22930000-0000-0000-0000-000000000005");
    }
    expect(within(card).getByRole("link", { name: "Open health" })).toBeTruthy();
  });

  it("collapses to one quiet line when every check passes", async () => {
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc()); // 19 checks, all ok
    renderCard();
    // A healthy card is a status region (not an alert), scoped by its label.
    const card = await screen.findByRole("status", { name: "System health" });
    expect(within(card).getByText("System health: all 19 checks passing")).toBeTruthy();
    // No verdict/attention items in the quiet form.
    expect(within(card).queryByText(/uzi cannot run work/)).toBeNull();
    expect(within(card).getByRole("link", { name: "Open health" })).toBeTruthy();
    // PRD #1648 D6: the OK severity badge (word + decorative SVG shape), no font glyph.
    expect(within(card).getByText("OK")).toBeTruthy();
    expect(card.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
    expect(card.textContent).not.toMatch(/[\u25cf\u25b2\u25c6\u25cb]/);
  });

  it("shows the verdict and the top three attention items when checks need attention", async () => {
    mockApi.getAdminHealth.mockResolvedValue(incidentDoc()); // 3 danger + 1 warn = 4 attention
    renderCard();
    // Danger → alert role (CustodyBoardAlert's conditional role), scoped by label.
    const card = await screen.findByRole("alert", { name: "System health" });
    expect(within(card).getByText("System health: uzi cannot run work: 1 blocking check")).toBeTruthy();
    // The three worst (danger) checks are listed by title; the 4th (warn) folds into "and 1 more".
    expect(within(card).getByText("Worker image roll")).toBeTruthy();
    expect(within(card).getByText("Worker capacity")).toBeTruthy();
    expect(within(card).getByText("Runs waiting for a worker")).toBeTruthy();
    expect(within(card).getByText("and 1 more")).toBeTruthy();
    // PRD #1648 D6: severity by word on the shared badge (headline + three danger items).
    expect(within(card).getAllByText("Danger").length).toBe(4);
    expect(card.textContent).not.toMatch(/[\u25cf\u25b2\u25c6\u25cb]/);
  });
});
