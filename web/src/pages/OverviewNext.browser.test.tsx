/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cdp, page, userEvent } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
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

describe("OverviewNext issue work line", () => {
  function mountLine() {
    // Two live stations (Planning and Building) so their flowing segments render.
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "q", status: "queued", issue_iid: 1, issue_title: LONG_TITLE }),
        aRun({ id: "p", status: "running", is_planning: true, issue_iid: 2 }),
        aRun({ id: "g", status: "awaiting_approval", issue_iid: 3 }),
        aRun({ id: "b", status: "running", issue_iid: 4 }),
        aRun({ id: "a", status: "awaiting_input", issue_iid: 5 }),
        aRun({ id: "k", status: "limit_wait", issue_iid: 6 }),
        aRun({ id: "o", kind: "ci_fix", status: "running", issue_iid: null }),
        aRun({ id: "pr", status: "completed", mr_iid: 4, mr_state: "opened", finished_at: "2026-07-05T12:00:00Z" }),
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
  const stations = () =>
    ["queued", "planning", "review", "building", "pr"].map(
      (s) => screen.getByRole("region", { name: "Issue work" }).querySelector(`[data-stage="${s}"]`) as HTMLElement,
    );

  it("draws the stations side by side on desktop", async () => {
    await page.viewport(1280, 900);
    mountLine();
    await screen.findByRole("region", { name: "Issue work" });
    await document.fonts.ready;
    const rects = stations().map((el) => el.getBoundingClientRect());
    for (let i = 1; i < rects.length; i++) {
      expect.soft(rects[i].top, `station ${i} top`).toBe(rects[0].top);
      expect.soft(rects[i].left, `station ${i} is right of the previous`).toBeGreaterThanOrEqual(rects[i - 1].right - 1);
    }
    const bays = ["asking", "parked", "other"].map(
      (s) => (screen.getByRole("region", { name: "Issue work" }).querySelector(`[data-stage="${s}"]`) as HTMLElement).getBoundingClientRect(),
    );
    for (const b of bays) expect.soft(b.top, "bays share a row").toBe(bays[0].top);
    expect(bays[0].top).toBeGreaterThanOrEqual(Math.max(...rects.map((r) => r.bottom)));
  });

  for (const width of [400, 320]) {
    it(`stacks the line vertically at ${width}px with no horizontal overflow`, async () => {
      await page.viewport(width, 900);
      mountLine();
      const card = await screen.findByRole("region", { name: "Issue work" });
      await document.fonts.ready;
      expect(window.innerWidth).toBe(width);
      const rects = stations().map((el) => el.getBoundingClientRect());
      for (let i = 1; i < rects.length; i++) {
        expect.soft(rects[i].left, `station ${i} left edge`).toBe(rects[0].left);
        expect.soft(rects[i].top, `station ${i} below the previous`).toBeGreaterThanOrEqual(rects[i - 1].bottom - 1);
      }
      const cardRight = card.getBoundingClientRect().right;
      for (const el of card.querySelectorAll<HTMLElement>("*")) {
        expect.soft(el.getBoundingClientRect().right, `${el.tagName} ${el.textContent?.slice(0, 24)}`).toBeLessThanOrEqual(
          cardRight + 1,
        );
      }
      const bays = ["asking", "parked", "other"].map(
        (s) => (card.querySelector(`[data-stage="${s}"]`) as HTMLElement).getBoundingClientRect(),
      );
      for (let i = 1; i < bays.length; i++) expect.soft(bays[i].top, `bay ${i} below the previous`).toBeGreaterThanOrEqual(bays[i - 1].bottom - 1);
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
    });
  }

  it("stops the flowing segments under prefers-reduced-motion", async () => {
    // The stylesheet is the contract: a media rule for reduced motion must set animation
    // none on .issue-line-flow. (Walks every sheet; a cross-origin sheet cannot be read.)
    const found: string[] = [];
    const walk = (rules: CSSRuleList, reduced: boolean) => {
      for (const rule of Array.from(rules)) {
        if (rule instanceof CSSMediaRule) {
          const text = rule.conditionText || rule.media.mediaText;
          walk(rule.cssRules, reduced || /prefers-reduced-motion:\s*reduce/.test(text));
        } else if (rule instanceof CSSStyleRule && reduced && rule.selectorText.includes(".issue-line-flow")) {
          if (rule.style.animationName === "none" || rule.style.animation.includes("none")) found.push(rule.selectorText);
        } else if ("cssRules" in rule && (rule as CSSGroupingRule).cssRules) {
          walk((rule as CSSGroupingRule).cssRules, reduced);
        }
      }
    };
    for (const sheet of Array.from(document.styleSheets)) {
      let rules: CSSRuleList;
      try {
        rules = sheet.cssRules;
      } catch {
        continue;
      }
      walk(rules, false);
    }
    expect(found.length).toBeGreaterThan(0);

    // Source order decides which same-specificity rule wins, so also check the computed
    // style under real reduced-motion emulation.
    await page.viewport(1280, 900);
    mountLine();
    const card = await screen.findByRole("region", { name: "Issue work" });
    const live = card.querySelector('[data-segment="flow"]') as HTMLElement;
    expect(live).not.toBeNull();
    expect(getComputedStyle(live).animationName, "animates without the preference").toBe("issue-line-flow");
    const session = cdp();
    try {
      await session.send("Emulation.setEmulatedMedia", {
        features: [{ name: "prefers-reduced-motion", value: "reduce" }],
      });
      expect(getComputedStyle(live).animationName).toBe("none");
    } finally {
      await session.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "" }] });
    }
  });

  it("hides the connecting segments when the line stacks, keeping the vertical rail", async () => {
    await page.viewport(400, 900);
    mountLine();
    const card = await screen.findByRole("region", { name: "Issue work" });
    const segments = Array.from(card.querySelectorAll<HTMLElement>("[data-segment]"));
    expect(segments.length).toBeGreaterThan(0);
    for (const seg of segments) expect(getComputedStyle(seg).display).toBe("none");
    // The rail pieces join into one rail from the first node's centre to the last node's centre.
    const rails = Array.from(card.querySelectorAll<HTMLElement>("[data-rail]"))
      .filter((el) => getComputedStyle(el).display !== "none")
      .map((el) => el.getBoundingClientRect())
      .sort((a, b) => a.top - b.top);
    const nodes = Array.from(card.querySelectorAll<HTMLElement>("[data-node]")).map((el) => {
      const r = el.getBoundingClientRect();
      return r.top + r.height / 2;
    });
    expect(rails).toHaveLength(nodes.length);
    expect(rails[0].top).toBeCloseTo(nodes[0], 0);
    let reach = rails[0].bottom;
    for (const r of rails.slice(1)) {
      expect(r.top - reach).toBeLessThanOrEqual(1);
      reach = Math.max(reach, r.bottom);
    }
    expect(reach).toBeCloseTo(nodes[nodes.length - 1], 0);
  });

  it("filters the Runs card from the keyboard: Tab to a station, Enter picks it, Space lets go", async () => {
    await page.viewport(1280, 900);
    mountLine();
    const card = await screen.findByRole("region", { name: "Issue work" });
    const at = (stage: string) => card.querySelector(`[data-stage="${stage}"]`) as HTMLElement;
    const yours = () => screen.getByRole("list", { name: "Your runs" }).querySelectorAll("li").length;
    const all = yours();
    expect(all).toBeGreaterThan(1);
    at("queued").focus();
    await userEvent.tab();
    await userEvent.tab();
    expect(document.activeElement).toBe(at("review"));
    await userEvent.keyboard("{Enter}");
    expect(at("review").getAttribute("aria-pressed")).toBe("true");
    expect(card.querySelectorAll('[data-stage][aria-pressed="true"]')).toHaveLength(1);
    expect(yours()).toBe(1);
    expect(screen.getByRole("link", { name: /All runs/ }).previousElementSibling?.textContent).toBe("Plan review");
    await userEvent.keyboard(" ");
    expect(at("review").getAttribute("aria-pressed")).toBe("false");
    expect(yours()).toBe(all);
  });

  it("filters the Runs card from the keyboard on a bay: Enter picks Parked, Space lets go", async () => {
    await page.viewport(1280, 900);
    mountLine();
    const card = await screen.findByRole("region", { name: "Issue work" });
    const bay = card.querySelector('[data-stage="parked"]') as HTMLElement;
    const yours = () => screen.getByRole("list", { name: "Your runs" }).querySelectorAll("li").length;
    const all = yours();
    expect(all).toBeGreaterThan(1);
    bay.focus();
    expect(document.activeElement).toBe(bay);
    await userEvent.keyboard("{Enter}");
    expect(bay.getAttribute("aria-pressed")).toBe("true");
    expect(card.querySelectorAll('[data-stage][aria-pressed="true"]')).toHaveLength(1);
    expect(yours()).toBeGreaterThan(0);
    expect(yours()).toBeLessThan(all);
    expect(screen.getByRole("link", { name: /All runs/ }).previousElementSibling?.textContent).toBe("Parked");
    await userEvent.keyboard(" ");
    expect(bay.getAttribute("aria-pressed")).toBe("false");
    expect(yours()).toBe(all);
  });

  it("animates a live segment and leaves a static one still", async () => {
    await page.viewport(1280, 900);
    mountLine();
    const card = await screen.findByRole("region", { name: "Issue work" });
    const live = card.querySelector('[data-segment="flow"]') as HTMLElement;
    expect(live).not.toBeNull();
    expect(getComputedStyle(live).animationName).toBe("issue-line-flow");
    const still = card.querySelector('[data-segment="static"]') as HTMLElement;
    expect(getComputedStyle(still).animationName).toBe("none");
  });
});

// #4052: the admin's You / All users switch is the Issue work header's right-most item and
// holds still while the scope or a pressed station changes the rest of the card.
describe("OverviewNext issue work scope switch", () => {
  // IBM Plex has real 400/500/600 faces with different advances, so a weight change between
  // the selected and unselected option would move the switch; the system fallback may not.
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.dataset.font = "plex";
  });
  afterEach(() => {
    delete document.documentElement.dataset.font;
  });

  function mountAdminLine() {
    // The viewer's own runs on several stations; the admin list (beforeEach) adds two other
    // users' runs, so All users is enabled and changes the counts.
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "q", status: "queued", issue_iid: 1 }),
        aRun({ id: "g", status: "awaiting_approval", issue_iid: 3 }),
        aRun({ id: "b", status: "running", issue_iid: 4 }),
        aRun({ id: "k", status: "limit_wait", issue_iid: 6 }),
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

  async function header() {
    const card = await screen.findByRole("region", { name: "Issue work" });
    const toggle = screen.getByRole("group", { name: "Issue work scope" });
    // The admin list has loaded once All users can be picked.
    // (The usage card has its own All users option, so the button is looked up in the switch.)
    await expect.poll(() => (within(toggle).getByRole("button", { name: "All users" }) as HTMLButtonElement).disabled).toBe(false);
    await document.fonts.ready;
    // fonts.ready also resolves after a failed load; without Plex the steady-width check is moot.
    // (document.fonts.check is true when no face matches at all, so look the faces up.)
    const loaded = (weight: string) =>
      [...document.fonts].some((f) => f.family.includes("IBM Plex Sans") && f.weight === weight && f.status === "loaded");
    for (const weight of ["400", "500", "600"]) expect(loaded(weight), `IBM Plex Sans ${weight} loaded`).toBe(true);
    const title = screen.getByRole("heading", { name: "Issue work", level: 2 });
    const note = title.parentElement!.children[1] as HTMLElement;
    expect(note.textContent).toMatch(/on the line/);
    return { card, toggle, title, note };
  }

  const centreY = (r: DOMRect) => r.top + r.height / 2;

  for (const width of [1280, 400]) {
    it(`puts the switch at the header's far right, inside the card, at ${width}px`, async () => {
      await page.viewport(width, 900);
      mountAdminLine();
      const { card, toggle, title, note } = await header();
      expect(window.innerWidth).toBe(width);
      const t = toggle.getBoundingClientRect();
      const c = card.getBoundingClientRect();
      expect.soft(t.right, "right of the title").toBeGreaterThanOrEqual(title.getBoundingClientRect().right);
      expect.soft(t.right, "right of the note").toBeGreaterThanOrEqual(note.getBoundingClientRect().right - 0.5);
      expect.soft(t.left, "inside the card (left)").toBeGreaterThanOrEqual(c.left);
      expect.soft(t.right, "inside the card (right)").toBeLessThanOrEqual(c.right);
      // Far right: flush with the card's content box (its right edge minus the padding).
      const contentRight = c.right - parseFloat(getComputedStyle(card).paddingRight) - parseFloat(getComputedStyle(card).borderRightWidth);
      expect.soft(Math.abs(t.right - contentRight), "flush with the card's content edge").toBeLessThanOrEqual(1);
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
        window.innerWidth,
      );
    });
  }

  it("keeps title, note and switch on one row at 1280px", async () => {
    await page.viewport(1280, 900);
    mountAdminLine();
    const { toggle, title, note } = await header();
    const t = toggle.getBoundingClientRect();
    const h = title.getBoundingClientRect();
    const n = note.getBoundingClientRect();
    // One row: each item's vertical centre lies inside the others' boxes.
    for (const [name, r] of [["title", h], ["note", n]] as const) {
      expect.soft(centreY(r), `${name} centre within the switch row`).toBeGreaterThanOrEqual(t.top);
      expect.soft(centreY(r), `${name} centre within the switch row`).toBeLessThanOrEqual(t.bottom);
    }
    expect.soft(centreY(t), "switch centre on the title row").toBeGreaterThanOrEqual(h.top);
    expect.soft(centreY(t), "switch centre on the title row").toBeLessThanOrEqual(h.bottom);
    expect(n.left, "note between title and switch").toBeGreaterThanOrEqual(h.right);
    expect(n.right).toBeLessThanOrEqual(t.left + 0.5);
  });

  it("keeps the switch on the title row and wraps the note below at 400px", async () => {
    await page.viewport(400, 900);
    mountAdminLine();
    const { toggle, title, note } = await header();
    const t = toggle.getBoundingClientRect();
    const h = title.getBoundingClientRect();
    const n = note.getBoundingClientRect();
    expect.soft(centreY(t), "switch centre on the title row").toBeGreaterThanOrEqual(h.top);
    expect.soft(centreY(t), "switch centre on the title row").toBeLessThanOrEqual(h.bottom);
    expect.soft(centreY(h), "title centre within the switch row").toBeGreaterThanOrEqual(t.top);
    expect.soft(centreY(h), "title centre within the switch row").toBeLessThanOrEqual(t.bottom);
    expect.soft(n.top, "note below the title").toBeGreaterThanOrEqual(h.bottom);
    expect.soft(n.top, "note below the switch").toBeGreaterThanOrEqual(t.bottom);
    expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
      window.innerWidth,
    );
  });

  for (const width of [1280, 400]) {
    it(`holds the switch still across You, All users and a pressed station at ${width}px`, async () => {
      await page.viewport(width, 900);
      mountAdminLine();
      const { card, toggle, note } = await header();
      // Page coordinates: pressing a station can scroll it into view on a phone, which moves
      // every viewport rect without moving the switch on the page.
      const rect = () => {
        const r = toggle.getBoundingClientRect();
        return { left: r.left + window.scrollX, top: r.top + window.scrollY, width: r.width, height: r.height };
      };
      const you = rect();
      const youNote = note.textContent;

      const allUsers = within(toggle).getByRole("button", { name: "All users" });
      await userEvent.click(allUsers);
      expect(allUsers.getAttribute("aria-pressed")).toBe("true");
      expect(note.textContent, "the note follows the scope").not.toBe(youNote);
      const all = rect();

      const building = card.querySelector('[data-stage="building"]') as HTMLElement;
      await userEvent.click(building);
      expect(building.getAttribute("aria-pressed")).toBe("true");
      const pressed = rect();

      for (const [name, r] of [["All users", all], ["station pressed", pressed]] as const) {
        for (const k of ["left", "top", "width", "height"] as const) {
          expect.soft(r[k], `${name} ${k}`).toBeCloseTo(you[k], 1);
        }
      }
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
        window.innerWidth,
      );
    });
  }
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

// #4041: the decision card's rows sit in two columns on wide screens only while there are 2 to
// 4 of them, always stack on a phone, and an expanded PR group never pushes the page sideways.
describe("OverviewNext decision card layout", () => {
  const prs = (n: number) =>
    Array.from({ length: n }, (_, i) =>
      aRun({
        id: `pr-${i}`,
        status: "completed",
        mr_iid: 100 + i,
        mr_state: "opened",
        mr_web_url: `https://github.com/o/r/pull/${100 + i}`,
        forge_type: "github",
        repo_path: "an-organisation/a-repository-with-a-long-name",
        issue_title: LONG_TITLE,
        finished_at: `2026-07-05T1${i}:00:00Z`,
      }),
    );

  async function mountCard(runs: ReturnType<typeof aRun>[]) {
    mockApi.listRuns.mockResolvedValue({ runs });
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
    const card = await screen.findByRole("region", { name: "Needs your decision" });
    await document.fonts.ready;
    const rows = () => Array.from((card.querySelector("[data-decision-rows]") as HTMLElement).children) as HTMLElement[];
    const lefts = () => new Set(rows().map((r) => Math.round(r.getBoundingClientRect().left)));
    return { card, rows, lefts };
  }

  const threeRows = () => [
    aRun({ id: "plan", status: "awaiting_approval", issue_title: LONG_TITLE }),
    aRun({ id: "ask", status: "awaiting_input", issue_title: LONG_TITLE }),
    ...prs(2),
  ];

  it("puts three rows in two columns at 1280px", async () => {
    await page.viewport(1280, 900);
    const { rows, lefts } = await mountCard(threeRows());
    expect(rows()).toHaveLength(3);
    expect(lefts().size).toBe(2);
  });

  it("stacks three rows in one column at 400px", async () => {
    await page.viewport(400, 900);
    const { rows, lefts } = await mountCard(threeRows());
    expect(rows()).toHaveLength(3);
    expect(lefts().size).toBe(1);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(window.innerWidth);
  });

  // Kinds group, so five rows (held, plan, question, park, PRs) is the most the card can show;
  // five is the "more than four" case.
  it("keeps five rows in one column at 1280px", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: { open_holds: 1, admission_counted_holds: 1, custody_hold_limit: 8, decision_needed: 1, blocked_runs: 0 },
      holds: [
        {
          id: "hold-h",
          run_id: "held-run",
          generation: 1,
          state: "open",
          attention: "source_only",
          worker_id: "w1",
          inventory_guarded: false,
          has_available_capture: false,
          created_at: "2026-07-05T10:00:00Z",
          updated_at: "2026-07-05T10:00:00Z",
        } as never,
      ],
    });
    await page.viewport(1280, 900);
    const { rows, lefts } = await mountCard([
      aRun({ id: "plan", status: "awaiting_approval", issue_title: LONG_TITLE }),
      aRun({ id: "ask", status: "awaiting_input", issue_title: LONG_TITLE }),
      aRun({ id: "park", status: "paused", issue_title: LONG_TITLE }),
      ...prs(3),
    ]);
    // The held row joins once the holds response lands.
    await expect.poll(() => rows().length).toBe(5);
    expect(lefts().size).toBe(1);
  });

  it("keeps a lone row in one column spanning the list at 1280px", async () => {
    await page.viewport(1280, 900);
    const { card, rows } = await mountCard([aRun({ id: "plan", status: "awaiting_approval", issue_title: LONG_TITLE })]);
    expect(rows()).toHaveLength(1);
    const list = (card.querySelector("[data-decision-rows]") as HTMLElement).getBoundingClientRect();
    const row = rows()[0].getBoundingClientRect();
    expect(row.left).toBeCloseTo(list.left, 0);
    expect(row.width).toBeCloseTo(list.width, 0);
  });

  // Expanding keeps every item on one line, holds the neighbouring row still (and, in a
  // two-column column at 1280px, the toggle too), puts the revealed items next in Tab order, and
  // never pushes the page sideways, at 1280px and stacked at 400px.
  for (const width of [1280, 400]) {
    it(`expands the PR group from the keyboard at ${width}px: one line per item, toggle holds still`, async () => {
      await page.viewport(width, 900);
      const { card, rows, lefts } = await mountCard([
        aRun({ id: "plan", status: "awaiting_approval", issue_title: LONG_TITLE }),
        aRun({ id: "ask", status: "awaiting_input", issue_title: LONG_TITLE }),
        ...prs(8),
      ]);
      expect(lefts().size).toBe(width === 1280 ? 2 : 1);
      const button = within(card).getByRole("button", { name: "Review PRs" });
      const list = document.getElementById(button.getAttribute("aria-controls") ?? "") as HTMLElement;
      expect(list).not.toBeNull();
      const neighbour = within(card).getByRole("link", { name: "Answer" });
      const pageTop = (el: HTMLElement) => el.getBoundingClientRect().top + window.scrollY;
      const before = { toggle: pageTop(button), neighbour: pageTop(neighbour) };

      button.focus();
      await userEvent.keyboard("{Enter}");
      expect(button.getAttribute("aria-expanded")).toBe("true");
      expect(within(list).getAllByRole("link", { name: "Review PR" })).toHaveLength(8);
      if (width === 1280) {
        expect.soft(pageTop(button), "toggle holds still").toBeCloseTo(before.toggle, 0);
      } else {
        // Stacked, the toggle sits under the body by design, so it follows the list down.
        expect.soft(button.getBoundingClientRect().top, "toggle under the list").toBeGreaterThanOrEqual(list.getBoundingClientRect().bottom);
      }
      expect.soft(pageTop(neighbour), "neighbouring row holds still").toBeCloseTo(before.neighbour, 0);

      // WCAG 2.4.3: the toggle precedes what it reveals, so the next Tab lands inside the list.
      await userEvent.tab();
      expect(list.contains(document.activeElement)).toBe(true);

      const items = Array.from(list.children) as HTMLElement[];
      expect(items).toHaveLength(8);
      const cardRight = card.getBoundingClientRect().right;
      for (const el of items) {
        const r = el.getBoundingClientRect();
        expect.soft(r.height, `one line: ${el.textContent?.slice(0, 30)}`).toBeLessThanOrEqual(24);
        expect.soft(r.right, el.textContent?.slice(0, 30)).toBeLessThanOrEqual(cardRight + 1);
        // The title keeps a readable share of the line.
        const title = el.children[1] as HTMLElement;
        expect.soft(title.getBoundingClientRect().width, "title width").toBeGreaterThanOrEqual(width === 1280 ? 100 : 50);
        // A long repo path truncates on its own; the PR number always shows whole, inside the link.
        const num = el.querySelector("[data-pr-num]") as HTMLElement;
        const link = num.closest("a") as HTMLElement;
        const n = num.getBoundingClientRect();
        const l = link.getBoundingClientRect();
        expect.soft(num.textContent, "number span").toMatch(/ #1\d\d$/);
        expect.soft(num.scrollWidth, `number not clipped: ${link.textContent}`).toBeLessThanOrEqual(num.clientWidth);
        expect.soft(n.width, "number has width").toBeGreaterThan(0);
        expect.soft(n.right, "number inside the link").toBeLessThanOrEqual(l.right + 0.5);
        expect.soft(n.left, "number inside the link").toBeGreaterThanOrEqual(l.left - 0.5);
        // The ref cell clips its overflow, so the link itself must fit inside it.
        expect.soft(l.right, "link inside its ref cell").toBeLessThanOrEqual((el.children[0] as HTMLElement).getBoundingClientRect().right + 0.5);
        if (width === 1280) {
          const repo = el.querySelector("[data-pr-repo]") as HTMLElement;
          expect.soft(repo.scrollWidth, "the long repo path is the part that truncates").toBeGreaterThan(repo.clientWidth);
          expect.soft(repo.title, "full repo on hover").toBe("an-organisation/a-repository-with-a-long-name");
        }
      }
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
      expect(rows()).toHaveLength(3);

      button.focus();
      await userEvent.keyboard(" ");
      expect(button.getAttribute("aria-expanded")).toBe("false");
      expect(button.textContent).toBe("Review PRs");
    });
  }
});
