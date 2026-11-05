import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewNext } from "./OverviewNext";
import { api } from "../lib/api";
import { healthySilentDoc } from "../mocks/data";
import { aRun, aUser, aWorker, authValue, emptySelf } from "./overviewFixtures";
import { useAuth } from "../auth/AuthContext";

// PRD #3878 M3: the preview's cards stack and never scroll sideways on a phone. Layout
// needs a real engine, so this runs in Chromium; jsdom cannot see an overflow.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listRepos: vi.fn(),
      listAgentTemplates: vi.fn(),
      listSecrets: vi.fn(),
      listConnections: vi.fn(),
      getUsage: vi.fn(),
      getAdminUsage: vi.fn(),
      adminListRuns: vi.fn(),
      getRecoveryHolds: vi.fn(),
      getAdminHealth: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const LONG_TITLE = "An exceedingly long issue title that keeps going well past the width of a four hundred pixel phone screen";
const LONG_EMAIL = "someone.with.a.very.long.local.part.indeed@subdomain.a-rather-long-company-domain.example.com";

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, is_admin: true }));
  mockApi.listRuns.mockResolvedValue({
    runs: [
      aRun({ id: "plan", status: "awaiting_approval", issue_title: LONG_TITLE, repo_path: "an-organisation/a-repository-with-a-long-name" }),
      aRun({ id: "ask", status: "awaiting_input", issue_title: LONG_TITLE }),
      aRun({ id: "park", status: "paused", issue_title: LONG_TITLE }),
      aRun({
        id: "pr",
        status: "completed",
        mr_iid: 4,
        mr_state: "opened",
        mr_web_url: "https://github.com/o/r/pull/4",
        forge_type: "github",
        issue_title: LONG_TITLE,
        finished_at: "2026-07-05T12:00:00Z",
      }),
      aRun({ id: "mine", status: "running", issue_title: LONG_TITLE, iteration_count: 12 }),
    ],
  });
  mockApi.adminListRuns.mockResolvedValue({
    runs: [
      aRun({ id: "x1", status: "running", issue_title: LONG_TITLE, owner_email: LONG_EMAIL }),
      aRun({ id: "x2", status: "awaiting_approval", issue_title: LONG_TITLE, owner_email: LONG_EMAIL }),
    ],
  });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "online" })] });
  mockApi.listRepos.mockResolvedValue({ repos: [{ enabled: true } as never] });
  mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [{ kind: "anthropic_token" } as never] });
  mockApi.listConnections.mockResolvedValue({ connections: [{} as never] });
  mockApi.getUsage.mockResolvedValue(emptySelf());
  mockApi.getAdminUsage.mockResolvedValue({ factory: emptySelf(), users: [], earliest_run: null });
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("OverviewNext at 400px", () => {
  it("stacks its cards inside the viewport with no horizontal scroll", async () => {
    await page.viewport(400, 900);
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );

    // Everything the test measures has rendered once the last group's rows are in.
    await screen.findByText("Other users (admin)");
    await screen.findByRole("list", { name: "Other users' runs" });
    await document.fonts.ready;
    expect(window.innerWidth).toBe(400);

    const needs = screen.getByRole("region", { name: "Needs your decision" });
    const runs = screen.getByRole("heading", { name: "Runs" }).closest("div.rounded-xl");
    expect(runs).not.toBeNull();
    const cards = [needs, runs as HTMLElement];
    const rects = cards.map((card) => card.getBoundingClientRect());

    // Stacked: one column, same left edge, each below the previous one.
    for (const r of rects) {
      expect.soft(r.left, "card left edge").toBe(rects[0].left);
      expect.soft(r.right, "card right edge").toBeLessThanOrEqual(window.innerWidth);
      expect.soft(r.width, "card width").toBeGreaterThan(0);
    }
    expect.soft(rects[1].top, "Runs sits below the decision card").toBeGreaterThanOrEqual(rects[0].bottom);

    // No row or owner label pokes out of its card.
    const card = runs as HTMLElement;
    const cardRight = card.getBoundingClientRect().right;
    for (const el of card.querySelectorAll<HTMLElement>("li, li *")) {
      expect.soft(el.getBoundingClientRect().right, `${el.tagName} ${el.textContent?.slice(0, 30)}`).toBeLessThanOrEqual(
        cardRight + 1,
      );
    }
    expect.soft(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
      window.innerWidth,
    );
  });
});

describe("OverviewNext capacity rail", () => {
  function mountPreview() {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          id: "w1",
          name: "a-worker-with-an-exceedingly-long-name-that-must-truncate-rather-than-push-the-rail-sideways",
          status: "online",
          active_runs: 1,
          max_concurrent_runs: 2,
          stats_cpu_pct: 34,
          stats_mem_bytes: 2147483648,
          stats_mem_limit_bytes: 4294967296,
          stats_source: "cgroup",
        }),
        aWorker({ id: "w2", name: "quiet", status: "offline" }),
        aWorker({
          id: "w3",
          name: "busy-worker",
          status: "online",
          draining_since: "2026-07-05T12:00:00Z",
          manual_cordoned_at: "2026-07-05T12:00:00Z",
          effective_cordoned: true,
          active_runs: 2,
          max_concurrent_runs: 4,
          active_cross_checks: 1,
          max_cross_check_slots: 2,
        }),
        // The mock's three-pill row (online, draining, 1/2 runs): its name is the one the
        // name min-width keeps readable at 320px.
        aWorker({
          id: "w4",
          name: "base.l-highmem-b54d",
          status: "online",
          draining_since: "2026-07-05T12:00:00Z",
          active_runs: 1,
          max_concurrent_runs: 2,
        }),
      ],
    });
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
  }

  it("opens and closes a worker disclosure to reveal its stat line", async () => {
    await page.viewport(1280, 900);
    mountPreview();
    await screen.findByText("Capacity");
    const stat = await screen.findByText(/cpu 34%/);
    const details = stat.closest("details") as HTMLDetailsElement;
    expect(details.open).toBe(false);
    expect(stat.checkVisibility(), "closed: stat line hidden").toBe(false);
    (details.querySelector("summary") as HTMLElement).click();
    expect(details.open).toBe(true);
    expect(stat.checkVisibility(), "open: stat line visible").toBe(true);
    (details.querySelector("summary") as HTMLElement).click();
    expect(details.open).toBe(false);
  });

  it("sits beside the runs widget on desktop", async () => {
    await page.viewport(1440, 900);
    mountPreview();
    await screen.findByText("Capacity");
    await document.fonts.ready;
    const runs = (screen.getByRole("heading", { name: "Runs" }).closest("div.rounded-xl") as HTMLElement).getBoundingClientRect();
    const rail = (screen.getByRole("heading", { name: /Capacity/ }).closest("div.rounded-xl") as HTMLElement).getBoundingClientRect();
    expect(rail.left).toBeGreaterThanOrEqual(runs.right);
  });

  it("stacks below the runs widget at 400px with no horizontal overflow", async () => {
    await page.viewport(400, 900);
    mountPreview();
    await screen.findByText("Capacity");
    await document.fonts.ready;
    expect(window.innerWidth).toBe(400);
    const runs = (screen.getByRole("heading", { name: "Runs" }).closest("div.rounded-xl") as HTMLElement).getBoundingClientRect();
    const railCard = screen.getByRole("heading", { name: /Capacity/ }).closest("div.rounded-xl") as HTMLElement;
    const rail = railCard.getBoundingClientRect();
    expect(rail.top, "rail below the runs widget").toBeGreaterThanOrEqual(runs.bottom);
    expect(rail.right).toBeLessThanOrEqual(window.innerWidth);
    for (const el of railCard.querySelectorAll<HTMLElement>("li, li *")) {
      expect.soft(el.getBoundingClientRect().right, `${el.tagName}`).toBeLessThanOrEqual(rail.right + 1);
    }
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(window.innerWidth);
    expectRailSummariesFit(railCard);
  });

  it("keeps worker names readable and rows inside the rail at 320px", async () => {
    await page.viewport(320, 800);
    mountPreview();
    await screen.findByText("Capacity");
    await document.fonts.ready;
    expect(window.innerWidth).toBe(320);
    const railCard = screen.getByRole("heading", { name: /Capacity/ }).closest("div.rounded-xl") as HTMLElement;
    expectRailSummariesFit(railCard);
  });
});

describe("OverviewNext held work card", () => {
  function mountWithHolds() {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: { open_holds: 11, admission_counted_holds: 8, custody_hold_limit: 8, decision_needed: 2, blocked_runs: 2 },
      holds: [],
    });
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
  }
  const cardOf = (name: RegExp) => screen.getByRole("heading", { name, level: 2 }).closest("div.rounded-xl") as HTMLElement;

  it("stacks below Capacity at 400px with no horizontal overflow", async () => {
    await page.viewport(400, 900);
    mountWithHolds();
    await screen.findByText("Capacity");
    await screen.findByText("custody slots used");
    await document.fonts.ready;
    expect(window.innerWidth).toBe(400);
    const capacity = cardOf(/Capacity/).getBoundingClientRect();
    const heldCard = cardOf(/Held work/);
    const held = heldCard.getBoundingClientRect();
    expect(held.left, "same column as Capacity").toBe(capacity.left);
    expect(held.top, "below Capacity").toBeGreaterThanOrEqual(capacity.bottom);
    expect(held.right).toBeLessThanOrEqual(window.innerWidth);
    for (const el of heldCard.querySelectorAll<HTMLElement>("*")) {
      expect.soft(el.getBoundingClientRect().right, `${el.tagName} ${el.textContent?.slice(0, 30)}`).toBeLessThanOrEqual(
        held.right + 1,
      );
    }
    expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
      window.innerWidth,
    );
  });

  it("sits beside Capacity between lg and xl", async () => {
    await page.viewport(1100, 900);
    mountWithHolds();
    await screen.findByText("Capacity");
    await screen.findByText("custody slots used");
    await document.fonts.ready;
    const capacity = cardOf(/Capacity/).getBoundingClientRect();
    const held = cardOf(/Held work/).getBoundingClientRect();
    expect(held.left, "Held work to the right of Capacity").toBeGreaterThanOrEqual(capacity.right);
    expect(held.right).toBeLessThanOrEqual(window.innerWidth);
  });
});

// Every worker summary stays inside its box and its name keeps a readable width, however
// many badges the worker shows.
function expectRailSummariesFit(railCard: HTMLElement) {
  const summaries = railCard.querySelectorAll<HTMLElement>("li summary");
  expect(summaries.length).toBe(4);
  for (const s of summaries) {
    expect.soft(s.scrollWidth, `summary ${s.textContent?.slice(0, 30)} overflow`).toBeLessThanOrEqual(s.clientWidth + 1);
    const name = s.querySelector<HTMLElement>("[data-worker-name]") as HTMLElement;
    expect.soft(name.getBoundingClientRect().width, `name ${name.textContent?.slice(0, 20)} width`).toBeGreaterThanOrEqual(80);
  }
  expect.soft(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
    document.documentElement.clientWidth,
  );
}
