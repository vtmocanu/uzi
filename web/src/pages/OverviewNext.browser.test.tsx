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
import { healthySilentDoc, overviewNextDoc } from "../mocks/data";
import { overviewNextOwnerWorkers } from "../mocks/data/overviewNext";
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
      listUsers: vi.fn(),
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
  mockApi.listUsers.mockResolvedValue({ users: [] });
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

// #4082: a run chip carries its cross-check state as an inner tag: "cross-checking" in
// Planning (plan) and Building (code), and in Plan review why the cross-check sent the plan to
// its owner. The tag wraps with the chip and never pokes out of it, down to a phone.
describe("OverviewNext issue work cross-check tags", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.dataset.font = "plex";
  });
  afterEach(() => {
    delete document.documentElement.dataset.font;
  });

  const OTHER = "lena@uzi.local";

  function mountCrossCheckLine() {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "own-plan", status: "running", is_planning: true, issue_iid: 4040, cross_check_active: "plan" }),
        aRun({ id: "own-plan-quiet", status: "running", is_planning: true, issue_iid: 4041, cross_check_active: null }),
        aRun({ id: "own-review", status: "awaiting_approval", issue_iid: 3881, plan_cross_check_required: true, plan_cross_check_gate_reason: "revise" }),
        aRun({ id: "own-build", status: "running", issue_iid: 3543, cross_check_active: "code" }),
        aRun({ id: "own-build-quiet", status: "running", issue_iid: 3251, cross_check_active: null }),
      ],
    });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "x-plan", status: "running", is_planning: true, issue_iid: 97, cross_check_active: "plan", owner_email: OTHER }),
        aRun({ id: "x-review", status: "awaiting_approval", issue_iid: 88, plan_cross_check_required: true, plan_cross_check_gate_reason: "worker_unsupported", owner_email: OTHER }),
        aRun({ id: "x-blocked", status: "awaiting_approval", issue_iid: 77, plan_cross_check_required: true, plan_cross_check_gate_reason: "block", owner_email: OTHER }),
        aRun({ id: "x-build", status: "running", issue_iid: 91, cross_check_active: "code", owner_email: OTHER }),
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

  // The chips of one station, keyed by their "#N" reference.
  function chips(card: HTMLElement, stage: string): Map<string, HTMLElement> {
    const station = card.querySelector(`[data-stage="${stage}"]`) as HTMLElement;
    const out = new Map<string, HTMLElement>();
    for (const el of station.querySelectorAll<HTMLElement>("span")) {
      const ref = /^#\d+/.exec(el.textContent ?? "")?.[0];
      // A chip is the outermost span whose text starts with its reference.
      if (ref && !out.has(ref) && el.firstChild?.nodeType === Node.TEXT_NODE) out.set(ref, el);
    }
    return out;
  }

  // The chip's tag text (its inner span), or null when it carries none.
  const tagOf = (chip: HTMLElement | undefined) => chip?.querySelector("span")?.textContent ?? null;

  function expectTagsInsideChips(card: HTMLElement) {
    for (const stage of ["planning", "review", "building"]) {
      for (const [ref, chip] of chips(card, stage)) {
        const tag = chip.querySelector("span");
        if (!tag) continue;
        const c = chip.getBoundingClientRect();
        const t = tag.getBoundingClientRect();
        expect.soft(t.width, `${ref} tag has width`).toBeGreaterThan(0);
        expect.soft(t.left, `${ref} tag left`).toBeGreaterThanOrEqual(c.left - 0.5);
        expect.soft(t.right, `${ref} tag right`).toBeLessThanOrEqual(c.right + 0.5);
        expect.soft(t.top, `${ref} tag top`).toBeGreaterThanOrEqual(c.top - 0.5);
        expect.soft(t.bottom, `${ref} tag bottom`).toBeLessThanOrEqual(c.bottom + 0.5);
      }
    }
  }

  async function ready() {
    const card = await screen.findByRole("region", { name: "Issue work" });
    const toggle = screen.getByRole("group", { name: "Issue work scope" });
    await expect.poll(() => (within(toggle).getByRole("button", { name: "All users" }) as HTMLButtonElement).disabled).toBe(false);
    await document.fonts.ready;
    return { card, toggle };
  }

  for (const width of [1280, 400]) {
    it(`tags the chips in Planning, Plan review and Building, inside their boxes, at ${width}px`, async () => {
      await page.viewport(width, 900);
      mountCrossCheckLine();
      const { card, toggle } = await ready();
      expect(window.innerWidth).toBe(width);

      const planning = chips(card, "planning");
      const review = chips(card, "review");
      const building = chips(card, "building");
      expect(tagOf(planning.get("#4040"))).toBe("cross-checking");
      expect(tagOf(planning.get("#4041")), "no live check, no tag").toBeNull();
      expect(tagOf(review.get("#3881"))).toBe("cross-check: revise");
      expect(tagOf(building.get("#3543"))).toBe("cross-checking");
      expect(tagOf(building.get("#3251")), "no live check, no tag").toBeNull();
      // The chip reads as one phrase, and its tooltip says which check.
      expect(planning.get("#4040")!.textContent).toBe("#4040 cross-checking");
      expect(planning.get("#4040")!.title).toBe("Plan cross-check in progress");
      expect(building.get("#3543")!.title).toMatch(/^Code cross-check in progress/);
      // You scope: other users' chips are not on the line.
      expect(planning.has("#97")).toBe(false);
      expectTagsInsideChips(card);

      // All users: another user's chips carry the same tags, with the owner first in the tooltip.
      await userEvent.click(within(toggle).getByRole("button", { name: "All users" }));
      const allPlanning = chips(card, "planning");
      const allReview = chips(card, "review");
      const allBuilding = chips(card, "building");
      expect(tagOf(allPlanning.get("#97"))).toBe("cross-checking");
      expect(tagOf(allReview.get("#88"))).toBe("cross-check: unavailable");
      expect(tagOf(allBuilding.get("#91"))).toBe("cross-checking");
      expect(tagOf(allPlanning.get("#4040")), "yours keep theirs").toBe("cross-checking");
      expect(tagOf(allReview.get("#3881"))).toBe("cross-check: revise");
      expect(tagOf(allReview.get("#77"))).toBe("cross-check: blocked");
      expect(allReview.get("#88")!.title).toMatch(/ · Unavailable/);
      expect(allReview.get("#88")!.title.startsWith("Unavailable")).toBe(false);
      expectTagsInsideChips(card);

      if (width === 400) {
        const cardRight = card.getBoundingClientRect().right;
        for (const el of card.querySelectorAll<HTMLElement>("*")) {
          expect.soft(el.getBoundingClientRect().right, `${el.tagName} ${el.textContent?.slice(0, 24)}`).toBeLessThanOrEqual(cardRight + 1);
        }
      }
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
    });
  }

  // A long tag ("cross-check: unavailable") in a narrow side-by-side station breaks inside its
  // pill rather than running past the station into the next one's chips (#4082 review).
  for (const width of [768, 1024, 1280]) {
    it(`keeps every chip inside its station at ${width}px, All users`, async () => {
      await page.viewport(width, 900);
      mountCrossCheckLine();
      const { card, toggle } = await ready();
      expect(window.innerWidth).toBe(width);
      await userEvent.click(within(toggle).getByRole("button", { name: "All users" }));
      expect(tagOf(chips(card, "review").get("#88"))).toBe("cross-check: unavailable");

      for (const stage of ["planning", "review", "building"]) {
        const s = (card.querySelector(`[data-stage="${stage}"]`) as HTMLElement).getBoundingClientRect();
        for (const [ref, chip] of chips(card, stage)) {
          const c = chip.getBoundingClientRect();
          expect.soft(c.left, `${width}px ${stage} ${ref} chip left`).toBeGreaterThanOrEqual(s.left - 1);
          expect.soft(c.right, `${width}px ${stage} ${ref} chip right`).toBeLessThanOrEqual(s.right + 1);
        }
      }
      expectTagsInsideChips(card);
    });
  }

  // The tag is 11px text, so it needs WCAG AA's 4.5:1 against what is actually painted under
  // it: the page, the card, the station (raised when pressed) and the chip's own tint, stacked.
  // The canvas does the compositing, so any CSS colour form (oklab, color-mix) resolves.
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 1;
  const ctx = canvas.getContext("2d", { willReadFrequently: true })!;
  function paint(color: string, under: number[]): number[] {
    ctx.globalCompositeOperation = "copy";
    ctx.fillStyle = `rgb(${under.join(",")})`;
    ctx.fillRect(0, 0, 1, 1);
    ctx.globalCompositeOperation = "source-over";
    ctx.fillStyle = "transparent";
    ctx.fillStyle = color;
    ctx.fillRect(0, 0, 1, 1);
    return Array.from(ctx.getImageData(0, 0, 1, 1).data.slice(0, 3));
  }
  function backdrop(el: HTMLElement): number[] {
    const chain: Element[] = [];
    for (let n: Element | null = el; n; n = n.parentElement) chain.unshift(n);
    return chain.reduce((under, n) => paint(getComputedStyle(n).backgroundColor, under), [255, 255, 255]);
  }
  const luminance = (rgb: number[]) => {
    const [r, g, b] = rgb.map((v) => {
      const c = v / 255;
      return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  function tagContrast(chip: HTMLElement | undefined): number {
    const tag = chip!.querySelector("span") as HTMLElement;
    const bg = backdrop(tag);
    const fg = paint(getComputedStyle(tag).color, bg);
    const [hi, lo] = [luminance(fg), luminance(bg)].sort((a, b) => b - a);
    return (hi + 0.05) / (lo + 0.05);
  }

  for (const theme of ["ember", "dawn"]) {
    it(`keeps every tag at AA contrast on its chip in ${theme}, with a station pressed too`, async () => {
      document.documentElement.dataset.theme = theme;
      try {
        await page.viewport(1280, 900);
        mountCrossCheckLine();
        const { card, toggle } = await ready();
        await userEvent.click(within(toggle).getByRole("button", { name: "All users" }));
        const cases: [string, string][] = [
          ["planning", "#4040"],
          ["building", "#3543"],
          ["review", "#3881"],
          ["review", "#88"],
          ["review", "#77"],
        ];
        for (const [stage, ref] of cases) {
          const chip = chips(card, stage).get(ref);
          expect(tagOf(chip), `${ref} carries a tag`).not.toBeNull();
          expect.soft(tagContrast(chip), `${theme} ${stage} ${ref}`).toBeGreaterThanOrEqual(4.5);
        }
        // Pressing a station raises its fill under the chips; the tags must hold there too.
        for (const stage of ["planning", "building", "review"]) {
          const station = card.querySelector(`[data-stage="${stage}"]`) as HTMLElement;
          await userEvent.click(station);
          expect(station.getAttribute("aria-pressed")).toBe("true");
          for (const [s, ref] of cases.filter(([s]) => s === stage)) {
            expect.soft(tagContrast(chips(card, s).get(ref)), `${theme} ${s} ${ref} pressed`).toBeGreaterThanOrEqual(4.5);
          }
        }
      } finally {
        delete document.documentElement.dataset.theme;
      }
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
        // Several problem pills at once: full slots, a dind volume at 91%, lost memory
        // authority and a failed upgrade.
        aWorker({
          id: "w5",
          name: "base.l-highmem-7c2e",
          kind: "hosted",
          status: "online",
          active_runs: 2,
          max_concurrent_runs: 2,
          stats_disk_dind_bytes: 45_600_000_000,
          stats_disk_dind_total_bytes: 50_000_000_000,
          memory_authority_lost_at: "2026-07-05T11:30:00Z",
          upgrade_status: "upgrade_failed",
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
    const stat = await screen.findByRole("progressbar", { name: "CPU", hidden: true });
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
    expectRailSummariesFit(railCard, 5);
    expectFifthWorkerPills(railCard);
  });

  it("keeps worker names readable and rows inside the rail at 320px", async () => {
    await page.viewport(320, 800);
    mountPreview();
    await screen.findByText("Capacity");
    await document.fonts.ready;
    expect(window.innerWidth).toBe(320);
    const railCard = screen.getByRole("heading", { name: /Capacity/ }).closest("div.rounded-xl") as HTMLElement;
    expectRailSummariesFit(railCard, 5);
    expectFifthWorkerPills(railCard);
  });
});

// PRD #4078: the mock's owner fleet (every problem state at once) in the real rail. Problem
// badges stay in the collapsed summary, and the expanded rows hold the gauges, runs on the
// worker, held-work link and footer without pushing the page sideways.
describe("OverviewNext capacity rail worker rows", () => {
  const FLEET = overviewNextOwnerWorkers();

  function mountFleet() {
    mockApi.listWorkers.mockResolvedValue({ workers: FLEET });
    // The viewer's own run that the disk-danger worker reports, so its row joins to a link.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-mr-rework", status: "running", issue_iid: 77, issue_title: "Rework the cost cap" })],
    });
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
  }

  const railOf = () => screen.getByRole("heading", { name: /Capacity/ }).closest("div.rounded-xl") as HTMLElement;
  const rowOf = (rail: HTMLElement, name: string) => {
    const label = within(rail).getByText(name);
    return label.closest("details") as HTMLDetailsElement;
  };

  for (const width of [1280, 400]) {
    it(`shows disk and memory-authority badges with every row closed at ${width}px`, async () => {
      await page.viewport(width, 900);
      mountFleet();
      await screen.findByText("Capacity");
      await document.fonts.ready;
      expect(window.innerWidth).toBe(width);
      const rail = railOf();
      for (const d of rail.querySelectorAll("details")) expect((d as HTMLDetailsElement).open).toBe(false);

      const disk = within(rail).getByText("disk 91%");
      expect(disk.checkVisibility(), "disk badge visible while closed").toBe(true);
      expect(disk.closest("summary")).toBe(rowOf(rail, "base.l-highmem-7c2e").querySelector("summary"));
      const memory = within(rail).getByText("memory authority lost");
      expect(memory.checkVisibility(), "memory authority badge visible while closed").toBe(true);
      expect(memory.closest("summary")).toBe(rowOf(rail, "ephemeral-4b2d9e70").querySelector("summary"));

      expectRailSummariesFit(rail, FLEET.length);
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
    });
  }

  for (const width of [1280, 400]) {
    it(`opens the disk-danger, offline, leased and rolling rows into gauges, runs, notes, held-work link and footer at ${width}px`, async () => {
      await page.viewport(width, 900);
      expect(window.innerWidth).toBe(width);
      mountFleet();
      await screen.findByText("Capacity");
      await document.fonts.ready;
      const rail = railOf();

      const danger = rowOf(rail, "base.l-highmem-7c2e");
      const offline = rowOf(rail, "ci-runner-offline");
      const leased = rowOf(rail, "ephemeral-9e17a0d3");
      const rolling = rowOf(rail, "base.l-highmem-b54d");
      for (const row of [danger, offline, leased, rolling]) {
        (row.querySelector("summary") as HTMLElement).click();
        expect(row.open).toBe(true);
      }

      expect(within(leased).getByText(/Kept warm for same-branch follow-up work until/).checkVisibility()).toBe(true);
      expect(within(rolling).getByText(/Takes no new runs\./).checkVisibility()).toBe(true);
      expect(within(rolling).getByText(/Rolls to v0\.86\.1-rc\.2 once fully idle/).checkVisibility()).toBe(true);

      const cpu = within(danger).getByRole("progressbar", { name: "CPU" });
      expect(cpu.checkVisibility()).toBe(true);
      expect(within(danger).getByText("On this worker").checkVisibility()).toBe(true);
      const run = within(danger).getByRole("link", { name: /#77 Rework the cost cap/ });
      expect(run.getAttribute("href")).toBe("/runs/run-mr-rework");
      expect(within(danger).getByText("run 5e44c9d7").checkVisibility(), "unjoined run shows its short id").toBe(true);
      expect(within(danger).getByRole("link", { name: /Review held work/ }).checkVisibility()).toBe(true);
      expect(within(danger).getByText(/^v0\.87\.0-rc\.4 · up /).checkVisibility()).toBe(true);

      expect(within(offline).getByText("Last reported").checkVisibility()).toBe(true);
      expect(within(offline).getByText(/last seen/).checkVisibility()).toBe(true);

      const railRight = rail.getBoundingClientRect().right;
      for (const el of rail.querySelectorAll<HTMLElement>("li, li *")) {
        expect.soft(el.getBoundingClientRect().right, `${el.tagName} ${el.textContent?.slice(0, 30)}`).toBeLessThanOrEqual(railRight + 1);
      }
      expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
    });
  }
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

// The several-problem-pills worker (the 5th in mountPreview) shows every pill in its summary.
function expectFifthWorkerPills(railCard: HTMLElement) {
  const summary = railCard.querySelectorAll<HTMLElement>("li summary")[4];
  for (const text of ["2/2 runs", "disk 91%", "memory authority lost", "upgrade failed"]) {
    expect(within(summary).getByText(text).checkVisibility(), `${text} visible`).toBe(true);
  }
}

// Every worker summary stays inside its box and its name keeps a readable width, however
// many badges the worker shows.
function expectRailSummariesFit(railCard: HTMLElement, count: number) {
  const summaries = railCard.querySelectorAll<HTMLElement>("li summary");
  expect(summaries.length).toBe(count);
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
        // The item's first child is its one-line row (an awaiting_input item adds an excerpt line
        // under it, #4040; PRs never do). The title keeps a readable share of the line.
        const line = el.children[0] as HTMLElement;
        const title = line.children[1] as HTMLElement;
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
        expect.soft(l.right, "link inside its ref cell").toBeLessThanOrEqual((line.children[0] as HTMLElement).getBoundingClientRect().right + 0.5);
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

  // PRD #4040: expanded awaiting_input items quote long pending questions on a second line that
  // truncates to one line inside the card, never pushing the page sideways on a phone.
  it("keeps an expanded question group with long excerpts inside the viewport at 400px", async () => {
    await page.viewport(400, 900);
    const LONG_QUESTION =
      "The joint-cap fixture does not finish within the lifted deadline on this worker. Lower the caps to the 2x Chromium floor, or keep them and record the measurement as not obtained?";
    const { card } = await mountCard(
      [0, 1, 2].map((i) =>
        aRun({
          id: `ask-${i}`,
          status: "awaiting_input",
          issue_iid: 3540 + i,
          issue_title: LONG_TITLE,
          pending_question: LONG_QUESTION,
          status_since: `2026-07-05T1${i}:00:00Z`,
        }),
      ),
    );
    expect(window.innerWidth).toBe(400);
    const button = within(card).getByRole("button", { name: "Open runs" });
    const list = document.getElementById(button.getAttribute("aria-controls") ?? "") as HTMLElement;
    expect(list.querySelectorAll("[data-excerpt]")).toHaveLength(0);
    button.focus();
    await userEvent.keyboard("{Enter}");
    expect(button.getAttribute("aria-expanded")).toBe("true");

    const excerpts = Array.from(list.querySelectorAll("[data-excerpt]")) as HTMLElement[];
    expect(excerpts).toHaveLength(3);
    const cardRight = card.getBoundingClientRect().right;
    const lineHeight = parseFloat(getComputedStyle(excerpts[0]).lineHeight);
    for (const el of excerpts) {
      const r = el.getBoundingClientRect();
      expect.soft(r.right, "excerpt inside the card").toBeLessThanOrEqual(cardRight + 1);
      expect.soft(r.height, "excerpt is one line").toBeLessThanOrEqual(lineHeight + 1);
      expect.soft(el.scrollWidth, "the long excerpt truncates").toBeGreaterThan(el.clientWidth);
      expect.soft(el.title, "full excerpt on hover").toBe(LONG_QUESTION);
    }
    for (const li of Array.from(list.children) as HTMLElement[]) {
      expect.soft(li.getBoundingClientRect().right, "item inside the card").toBeLessThanOrEqual(cardRight + 1);
      expect.soft(li.scrollWidth, "item does not overflow").toBeLessThanOrEqual(li.clientWidth + 1);
    }
    expect(card.scrollWidth, "card does not overflow").toBeLessThanOrEqual(card.clientWidth + 1);
    expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(window.innerWidth);
  });
});

// #4042: the admin People chip's popover stays inside the viewport (below the chip on desktop,
// a bottom sheet on a phone, short phones included), Escape hands focus back to the chip, and
// its Usage by user action can be driven from the keyboard to the usage card's By user section.
describe("OverviewNext People chip", () => {
  const VIEWER = aUser.email;
  const person = (id: string, email: string, display_name: string | null, last_login: string | null) => ({
    ...aUser,
    id,
    email,
    display_name,
    last_login,
    created_at: "2026-06-01T00:00:00Z",
  });
  const hoursAgo = (h: number) => new Date(Date.now() - h * 3600_000).toISOString();

  beforeEach(() => {
    localStorage.clear();
    document.documentElement.dataset.font = "plex";
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "o1", status: "running", issue_title: LONG_TITLE, owner_email: "priya@uzi.local" }),
        aRun({ id: "o2", status: "awaiting_approval", issue_title: LONG_TITLE, owner_email: "sam@uzi.local" }),
        aRun({ id: "o3", status: "queued", issue_title: LONG_TITLE, owner_email: "priya@uzi.local" }),
        aRun({ id: "o4", status: "limit_wait", issue_title: LONG_TITLE, owner_email: "jordan@uzi.local" }),
        aRun({ id: "o5", status: "awaiting_input", issue_title: LONG_TITLE, owner_email: LONG_EMAIL }),
        aRun({ id: "o6", status: "running", issue_title: LONG_TITLE, owner_email: "lena@uzi.local" }),
      ],
    });
    mockApi.listUsers.mockResolvedValue({
      users: [
        person("u1", VIEWER, "Admin", hoursAgo(0.1)),
        person("u-priya", "priya@uzi.local", "Priya Desai", hoursAgo(1)),
        person("u-sam", "sam@uzi.local", "Sam Okafor", hoursAgo(3)),
        person("u-jordan", "jordan@uzi.local", "Jordan Lee", hoursAgo(30)),
        person("u-lena", "lena@uzi.local", "Lena Fischer", hoursAgo(0.5)),
        person("u-long", LONG_EMAIL, null, hoursAgo(5)),
        person("u-elif", "elif@uzi.local", "Elif Kaya", hoursAgo(2)),
        person("u-hana", "hana@uzi.local", "Hana Ito", hoursAgo(24 * 9)),
        person("u-mateo", "mateo@uzi.local", "Mateo Silva", null),
      ],
    });
    const self = emptySelf();
    mockApi.getAdminUsage.mockResolvedValue({
      factory: self,
      users: ["u1", "u-priya", "u-sam"].map((id, i) => ({
        user_id: id,
        email: `${id}@uzi.local`,
        usage: self.lifetime,
        run_count: 4,
        outcomes: self.outcomes.lifetime,
        subscription_run_count: 0,
        unreported_run_count: 0,
        last_7_days: self.last_7_days,
        last7_run_count: [3, 1, 0][i],
        last7_outcomes: self.outcomes.last_7_days,
      })),
      earliest_run: "2026-06-01T00:00:00Z",
    });
  });
  afterEach(() => {
    delete document.documentElement.dataset.font;
  });

  async function mountChip() {
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
    // The chip renders once the admin list has loaded; wait for the users too so the
    // popover holds its full content (names, signed-in group, idle line) before measuring.
    const chip = await screen.findByRole("button", { name: /^People: \d+ active now of \d+$/ });
    // The usage data loads separately from the admin lists. Wait for the Usage card, so the
    // footer action reads "Usage by user →" (not unavailable) and the card can be queried.
    await screen.findByRole("group", { name: "Usage scope" });
    await document.fonts.ready;
    return chip;
  }

  const inViewport = (r: DOMRect, what: string) => {
    expect.soft(r.left, `${what} left`).toBeGreaterThanOrEqual(0);
    expect.soft(r.top, `${what} top`).toBeGreaterThanOrEqual(0);
    expect.soft(r.right, `${what} right`).toBeLessThanOrEqual(window.innerWidth);
    expect.soft(r.bottom, `${what} bottom`).toBeLessThanOrEqual(window.innerHeight);
  };

  for (const [width, height] of [
    [1280, 900],
    [400, 900],
  ] as const) {
    it(`opens inside the viewport and closes on Escape back to the chip at ${width}x${height}`, async () => {
      await page.viewport(width, height);
      const chip = await mountChip();
      expect(window.innerWidth).toBe(width);
      await userEvent.click(chip);
      const dialog = await screen.findByRole("dialog", { name: "People using uzi" });
      expect(chip.getAttribute("aria-expanded")).toBe("true");
      // The popover holds every group the fixture feeds it.
      expect(within(dialog).getByText("Priya Desai")).toBeTruthy();
      expect(within(dialog).getByText("Elif Kaya")).toBeTruthy();
      expect(within(dialog).getByText(/never signed in/)).toBeTruthy();
      inViewport(dialog.getBoundingClientRect(), "dialog");
      if (width === 1280) {
        // Desktop: below the chip, right edges aligned.
        const c = chip.getBoundingClientRect();
        const d = dialog.getBoundingClientRect();
        expect.soft(d.top, "below the chip").toBeGreaterThanOrEqual(c.bottom);
        expect.soft(Math.abs(d.right - c.right), "right edge aligned to the chip").toBeLessThanOrEqual(1);
      }
      // No row pokes out of the dialog.
      const dRight = dialog.getBoundingClientRect().right;
      for (const li of dialog.querySelectorAll<HTMLElement>("li")) {
        expect.soft(li.getBoundingClientRect().right, li.textContent?.slice(0, 30)).toBeLessThanOrEqual(dRight + 1);
      }
      expect(document.documentElement.scrollWidth, "no horizontal scroll").toBeLessThanOrEqual(
        document.documentElement.clientWidth,
      );

      await userEvent.keyboard("{Escape}");
      expect(screen.queryByRole("dialog", { name: "People using uzi" })).toBeNull();
      expect(chip.getAttribute("aria-expanded")).toBe("false");
      expect(document.activeElement).toBe(chip);
    });
  }

  // A busy viewer's pill ("3 running · 2 queued · 2 waiting · waits on them") once squeezed
  // their own name to a single letter; the name keeps a readable width and the pill wraps.
  for (const width of [1280, 400]) {
    it(`keeps a busy viewer's name readable beside a long pill at ${width}px`, async () => {
      mockApi.listRuns.mockResolvedValue({
        runs: [
          ...["r1", "r2", "r3"].map((id) => aRun({ id, status: "running" })),
          ...["q1", "q2"].map((id) => aRun({ id, status: "queued" })),
          ...["l1", "l2"].map((id) => aRun({ id, status: "limit_wait" })),
          aRun({ id: "a1", status: "awaiting_input" }),
        ],
      });
      await page.viewport(width, 900);
      const chip = await mountChip();
      await userEvent.click(chip);
      const dialog = await screen.findByRole("dialog", { name: "People using uzi" });
      const row = within(dialog).getByText("(you)").closest("li") as HTMLElement;
      const [, name, pill] = Array.from(row.children) as HTMLElement[];
      expect(pill.textContent).toBe("3 running · 2 queued · 2 waiting · waits on them");
      expect.soft(name.getBoundingClientRect().width, "name keeps a readable width").toBeGreaterThanOrEqual(120);
      expect.soft(name.scrollWidth, "the viewer's short name shows whole").toBeLessThanOrEqual(name.clientWidth + 1);
      expect.soft(pill.getBoundingClientRect().right, "pill inside the dialog").toBeLessThanOrEqual(
        dialog.getBoundingClientRect().right,
      );
      expect.soft(pill.getBoundingClientRect().left, "pill right of the name").toBeGreaterThanOrEqual(
        name.getBoundingClientRect().right,
      );
    });
  }

  it("fits a short phone (400x600) with the Usage by user action on screen", async () => {
    await page.viewport(400, 600);
    const chip = await mountChip();
    await userEvent.click(chip);
    const dialog = await screen.findByRole("dialog", { name: "People using uzi" });
    inViewport(dialog.getBoundingClientRect(), "dialog");
    const action = within(dialog).getByRole("button", { name: "Usage by user →" });
    inViewport(action.getBoundingClientRect(), "Usage by user");
    expect(document.documentElement.scrollWidth, "no horizontal scroll").toBeLessThanOrEqual(
      document.documentElement.clientWidth,
    );
  });

  it("keeps the Usage by user action pinned on screen when the sheet scrolls (400x400)", async () => {
    await page.viewport(400, 400);
    const chip = await mountChip();
    await userEvent.click(chip);
    const dialog = await screen.findByRole("dialog", { name: "People using uzi" });
    inViewport(dialog.getBoundingClientRect(), "dialog");
    // The content must overflow, or this case proves nothing about the sticky footer.
    expect(dialog.scrollHeight, "sheet scrolls").toBeGreaterThan(dialog.clientHeight);
    const action = within(dialog).getByRole("button", { name: "Usage by user →" });
    inViewport(action.getBoundingClientRect(), "Usage by user");
  });

  it("drives Usage by user from the keyboard: Enter opens, Tab reaches the action, Enter lands on By user", async () => {
    await page.viewport(1280, 900);
    const chip = await mountChip();
    const usageScope = screen.getByRole("group", { name: "Usage scope" });
    const allUsers = within(usageScope).getByRole("button", { name: "All users" });
    expect(allUsers.getAttribute("aria-pressed")).toBe("false");

    chip.focus();
    await userEvent.keyboard("{Enter}");
    const dialog = await screen.findByRole("dialog", { name: "People using uzi" });
    expect(chip.getAttribute("aria-expanded")).toBe("true");
    await userEvent.tab();
    const action = within(dialog).getByRole("button", { name: "Usage by user →" });
    expect(document.activeElement).toBe(action);
    await userEvent.keyboard("{Enter}");

    expect(chip.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog", { name: "People using uzi" })).toBeNull();
    expect(within(screen.getByRole("group", { name: "Usage scope" })).getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    await expect.poll(() => document.activeElement?.id).toBe("overview-usage-by-user");
  });
});

describe("OverviewNext health chip popover", () => {
  function mountChip() {
    mockApi.getAdminHealth.mockResolvedValue(overviewNextDoc());
    render(
      <MemoryRouter initialEntries={["/overview/next"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <OverviewNext />
        </main>
      </MemoryRouter>,
    );
    return screen.findByRole("button", { name: /System health/ });
  }

  it("opens on click, closes on Escape and on a click elsewhere, and returns focus to the chip", async () => {
    await page.viewport(1280, 900);
    const chip = await mountChip();
    await userEvent.click(chip);
    const dialog = await screen.findByRole("dialog", { name: "System health" });
    expect(dialog.checkVisibility(), "panel visible").toBe(true);
    expect(chip.getAttribute("aria-expanded")).toBe("true");

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "System health" })).toBeNull();
    expect(document.activeElement).toBe(chip);

    await userEvent.click(chip);
    await screen.findByRole("dialog", { name: "System health" });
    // Click on empty page: the greeting heading is inert text (not focusable) above the panel,
    // so focus falls to <body> and the chip takes it back a frame later.
    const heading = screen.getByRole("heading", { level: 1 });
    expect(heading.tabIndex, "click target is not focusable").toBe(-1);
    await userEvent.click(heading);
    expect(screen.queryByRole("dialog", { name: "System health" })).toBeNull();
    await expect.poll(() => document.activeElement).toBe(chip);
  });

  it("stays inside the viewport and below the chip at sm and wider", async () => {
    for (const width of [640, 768, 1024, 1280, 1440]) {
      cleanup();
      await page.viewport(width, 900);
      const chip = await mountChip();
      await userEvent.click(chip);
      const dialog = await screen.findByRole("dialog", { name: "System health" });
      await document.fonts.ready;
      const r = dialog.getBoundingClientRect();
      const visibleWidth = document.documentElement.clientWidth;
      expect.soft(r.left, `left edge at ${width}px`).toBeGreaterThanOrEqual(15);
      expect.soft(r.right, `right edge at ${width}px`).toBeLessThanOrEqual(visibleWidth - 15);
      const c = chip.getBoundingClientRect();
      expect.soft(r.top, `below the chip at ${width}px`).toBeGreaterThanOrEqual(c.bottom);
      // Attached to the chip: right-aligned when that fits, otherwise left-aligned when that fits.
      if (c.right - r.width >= 16) {
        expect.soft(Math.abs(r.right - c.right), `right-aligned to the chip at ${width}px`).toBeLessThanOrEqual(1);
      } else if (c.left + r.width <= visibleWidth - 16) {
        expect.soft(Math.abs(r.left - c.left), `left-aligned to the chip at ${width}px`).toBeLessThanOrEqual(1);
      }
    }
  });

  it("never covers a fixed sidebar left of the content column", async () => {
    for (const width of [1280, 1440, 1600]) {
      cleanup();
      await page.viewport(width, 900);
      mockApi.getAdminHealth.mockResolvedValue(overviewNextDoc());
      // The shell's lg sidebar is a fixed 240px aside beside <main> (AppShell lg:pl-60).
      render(
        <MemoryRouter initialEntries={["/overview/next"]}>
          <div style={{ paddingLeft: 240 }}>
            <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
              <OverviewNext />
            </main>
          </div>
        </MemoryRouter>,
      );
      const chip = await screen.findByRole("button", { name: /System health/ });
      await userEvent.click(chip);
      const dialog = await screen.findByRole("dialog", { name: "System health" });
      await document.fonts.ready;
      const mainLeft = document.querySelector("main")!.getBoundingClientRect().left;
      const r = dialog.getBoundingClientRect();
      expect.soft(r.left, `clear of the sidebar at ${width}px`).toBeGreaterThanOrEqual(mainLeft);
      expect.soft(r.right, `inside the viewport at ${width}px`).toBeLessThanOrEqual(document.documentElement.clientWidth - 15);
    }
  });

  it("re-places the open panel on resize, switching to the bottom sheet below sm", async () => {
    await page.viewport(1280, 900);
    const chip = await mountChip();
    await userEvent.click(chip);
    const dialog = await screen.findByRole("dialog", { name: "System health" });
    await page.viewport(700, 900);
    // At 700 the panel (640 wide) can only sit in [16, 44]: poll until it has been re-placed there.
    await expect
      .poll(() => {
        const r = dialog.getBoundingClientRect();
        return r.left >= 15 && r.right <= document.documentElement.clientWidth - 15;
      })
      .toBe(true);
    await page.viewport(500, 900);
    await expect
      .poll(() => Math.abs(dialog.getBoundingClientRect().left - 16))
      .toBeLessThanOrEqual(1);
    expect(dialog.style.left, "no inline placement on the bottom sheet").toBe("");
  });

  it("stays inside the viewport as a bottom sheet at 375px", async () => {
    await page.viewport(375, 900);
    const chip = await mountChip();
    await userEvent.click(chip);
    const dialog = await screen.findByRole("dialog", { name: "System health" });
    await document.fonts.ready;
    expect(window.innerWidth).toBe(375);
    const r = dialog.getBoundingClientRect();
    // Bottom sheet with 1rem (16px) insets on the sides and bottom, fully inside the viewport.
    expect.soft(Math.abs(r.left - 16), "left inset").toBeLessThanOrEqual(1);
    expect.soft(Math.abs(window.innerWidth - r.right - 16), "right inset").toBeLessThanOrEqual(1);
    expect.soft(Math.abs(window.innerHeight - r.bottom - 16), "bottom inset").toBeLessThanOrEqual(1);
    expect.soft(r.top, "top inside the viewport").toBeGreaterThanOrEqual(0);
    expect(document.documentElement.scrollWidth, "document stays inside the viewport").toBeLessThanOrEqual(
      window.innerWidth,
    );
  });
});
