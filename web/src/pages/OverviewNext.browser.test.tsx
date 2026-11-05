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
