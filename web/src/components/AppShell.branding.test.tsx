// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell, __resetBrandingForTests } from "./AppShell";
import { api, type Branding } from "../lib/api";
import { DEFAULT_TITLE } from "../lib/brandTitle";
import { useAuth } from "../auth/AuthContext";
import { mockBuildInfo } from "../mocks/data";

// Instance-branding chrome (PRD #685 M3a): the custom app mark across all four
// surfaces (desktop aside, mobile drawer, signed-out PublicShell, mobile signed-in
// top bar) and the durable MIT © Vlad Mocanu credit.
//
// The app mark is asserted on the HTML <img> handle (data-testid="app-logo-img" /
// alt="app logo"), NEVER role="img": the inline FactoryIcon renders through an
// <svg role="img">, so a role query would collide with it. Custom-mode tests read
// img.getAttribute("src"); the default test asserts the SAME query returns nothing.
//
// The module-memoised branding fetch (`brandingPromise` in AppShell) is reset
// between tests via the exported `__resetBrandingForTests` seam — unlike
// buildInfoPromise, which its own test file leaves cold-per-file because it needs
// only one value; here each test drives a DIFFERENT branding, so the memo must be
// cleared or the first test's value would pin the whole file.

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
    unreadNotificationCount: vi.fn().mockResolvedValue({ unread: 0 }),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.4.2" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    listFindings: vi
      .fn()
      .mockResolvedValue({ bucket: "to_file", repo: "", run: "", open_count: 0, findings: [] }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMySettings: vi.fn().mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    }),
    version: vi.fn(),
    branding: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const DEFAULT_BRANDING: Branding = {
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_present: false,
  app_logo_keep_name: true,
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: false,
  brand_logo_present: false,
};

function brandingWith(overrides: Partial<Branding>): Branding {
  return { ...DEFAULT_BRANDING, ...overrides };
}

function signIn() {
  vi.mocked(useAuth).mockReturnValue({ user, logout: vi.fn() } as never);
}

function signOut() {
  vi.mocked(useAuth).mockReturnValue({ user: null, logout: vi.fn() } as never);
}

beforeEach(() => {
  __resetBrandingForTests();
  signIn();
  mockApi.version.mockResolvedValue(mockBuildInfo);
  mockApi.branding.mockResolvedValue(DEFAULT_BRANDING);
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

function renderShell(initial = "/dashboard") {
  return render(
    <MemoryRouter initialEntries={[initial]}>
      <AppShell>
        <div />
      </AppShell>
    </MemoryRouter>,
  );
}

// The name/subtitle marker. Unique to the app mark's name span (the email
// "admin@uzi.local" also contains "uzi", so match the subtitle instead), and a
// substring regex because PublicShell/mobile render it as "· uzinele întunecate".
const NAME_RE = /uzinele întunecate/;

describe("AppShell branding — durable credit (D3)", () => {
  // FIRST in the file, deliberately: buildInfoPromise has no reset seam, so this is
  // the only test guaranteed a cold build memo. It leaves version pending to prove
  // the credit renders while BOTH branding and build are unresolved.
  it("renders the license credit while branding and build are still in flight", () => {
    mockApi.branding.mockReturnValue(new Promise<Branding>(() => {}));
    mockApi.version.mockReturnValue(new Promise(() => {}));
    renderShell();
    expect(screen.getAllByTestId("license-credit").length).toBeGreaterThan(0);
    expect(screen.getAllByTestId("license-credit")[0].textContent).toBe("MIT © Vlad Mocanu");
  });

  it("still renders the credit under full white-label settings (signed-in)", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "custom", app_logo_present: true, app_logo_keep_name: false }),
    );
    renderShell();
    await screen.findAllByTestId("app-logo-img");
    expect(screen.getAllByTestId("license-credit").length).toBeGreaterThan(0);
  });

  it("renders the credit on the signed-out shell too", async () => {
    signOut();
    renderShell("/");
    expect((await screen.findByTestId("license-credit")).textContent).toBe("MIT © Vlad Mocanu");
  });

  it("hides the credit when the sidebar is collapsed", () => {
    window.localStorage.setItem("uzi.sidebar.collapsed", "true");
    renderShell();
    expect(screen.queryAllByTestId("license-credit")).toHaveLength(0);
  });
});

describe("AppShell branding — app mark", () => {
  it("default mode: no app-logo <img> on any surface, FactoryIcon + literals render", async () => {
    renderShell();
    fireEvent.click(screen.getByLabelText("Open navigation")); // co-mount the drawer
    // Literals render on every mark (name kept in default mode).
    expect((await screen.findAllByText(NAME_RE)).length).toBeGreaterThan(0);
    await waitFor(() => expect(mockApi.branding).toHaveBeenCalled());
    expect(screen.queryAllByTestId("app-logo-img")).toHaveLength(0);
    // The trusted inline FactoryIcon fallback renders instead — assert THAT mark, not any
    // svg on the page (nav icons make a bare querySelector("svg") tautological).
    expect(screen.queryAllByTestId("app-mark-fallback").length).toBeGreaterThan(0);
  });

  it("custom + present: renders <img src='/api/branding/logo/app'>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "custom", app_logo_present: true }));
    renderShell();
    const imgs = await screen.findAllByTestId("app-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("src")).toBe("/api/branding/logo/app");
  });

  it("custom + not present: renders the inline FactoryIcon, no app-logo <img>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "custom", app_logo_present: false }));
    renderShell();
    await waitFor(() => expect(mockApi.branding).toHaveBeenCalled());
    expect(screen.queryAllByTestId("app-logo-img")).toHaveLength(0);
    // The trusted inline FactoryIcon fallback renders instead of a /brand-default.svg <img>.
    expect(screen.queryAllByTestId("app-mark-fallback").length).toBeGreaterThan(0);
  });

  it("preset mode (metaminds): renders <img src='/brand-presets/metaminds.svg'>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "preset", app_logo_preset: "metaminds" }));
    renderShell();
    const imgs = await screen.findAllByTestId("app-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("src")).toBe("/brand-presets/metaminds.svg");
  });

  it("preset mode, unknown slug: renders the inline FactoryIcon, no app-logo <img>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "preset", app_logo_preset: "nope" }));
    renderShell();
    await waitFor(() => expect(mockApi.branding).toHaveBeenCalled());
    expect(screen.queryAllByTestId("app-logo-img")).toHaveLength(0);
    // Assert the FactoryIcon fallback mark specifically, not any svg on the page.
    expect(screen.queryAllByTestId("app-mark-fallback").length).toBeGreaterThan(0);
  });
});

describe("AppShell branding — white-label hides the name on all four surfaces", () => {
  it("signed-in: name absent on desktop aside, mobile drawer, and mobile top bar", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "custom", app_logo_present: true, app_logo_keep_name: false }),
    );
    renderShell();
    fireEvent.click(screen.getByLabelText("Open navigation")); // co-mount the drawer
    // Wait for the custom fetch to settle (marks flip to <img>), then assert no name.
    await screen.findAllByTestId("app-logo-img");
    expect(screen.queryByText(NAME_RE)).toBeNull();
  });

  it("signed-out PublicShell: name absent", async () => {
    signOut();
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "custom", app_logo_present: true, app_logo_keep_name: false }),
    );
    renderShell("/");
    await screen.findByTestId("app-logo-img");
    expect(screen.queryByText(NAME_RE)).toBeNull();
  });

  it("custom + keep_name: the name IS kept beside the custom mark", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "custom", app_logo_present: true, app_logo_keep_name: true }),
    );
    renderShell();
    await screen.findAllByTestId("app-logo-img");
    expect(screen.getAllByText(NAME_RE).length).toBeGreaterThan(0);
  });

  it("preset + keep_name off: full white-label, the name is hidden (D4)", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "preset", app_logo_preset: "metaminds", app_logo_keep_name: false }),
    );
    renderShell();
    fireEvent.click(screen.getByLabelText("Open navigation")); // co-mount the drawer
    await screen.findAllByTestId("app-logo-img");
    expect(screen.queryByText(NAME_RE)).toBeNull();
  });

  it("preset + keep_name on: co-brand, the name IS kept (D4)", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ app_logo_mode: "preset", app_logo_preset: "metaminds", app_logo_keep_name: true }),
    );
    renderShell();
    await screen.findAllByTestId("app-logo-img");
    expect(screen.getAllByText(NAME_RE).length).toBeGreaterThan(0);
  });
});

// The POWERED BY brand block (PRD #685 M3b). Property assertions on named channels:
// the "POWERED BY" label text, the company string, and the brand <img> handle
// (data-testid="brand-logo-img") with its `src` — never a pixel snapshot. Each case
// drives a DIFFERENT branding through the reset memo (beforeEach clears it).
const POWERED_BY_RE = /powered by/;

describe("AppShell branding — POWERED BY block", () => {
  it("text mode: renders the POWERED BY label and the company string", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "text", brand_company: "Acme, Inc." }));
    renderShell();
    expect((await screen.findAllByText(POWERED_BY_RE)).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Acme, Inc.").length).toBeGreaterThan(0);
    // Text mode never renders a brand <img>.
    expect(screen.queryAllByTestId("brand-logo-img")).toHaveLength(0);
  });

  it("logo mode, no asset: falls back to the preset <img src='/brand-default.svg'>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_logo_present: false }));
    renderShell();
    const imgs = await screen.findAllByTestId("brand-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("src")).toBe("/brand-default.svg");
  });

  it("logo mode, asset present: renders <img src='/api/branding/logo/brand'>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_logo_present: true }));
    renderShell();
    const imgs = await screen.findAllByTestId("brand-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("src")).toBe("/api/branding/logo/brand");
  });

  it("XSS control: an uploaded brand logo renders as <img>, never inline <svg>", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_logo_present: true }));
    renderShell();
    const imgs = await screen.findAllByTestId("brand-logo-img");
    for (const img of imgs) {
      // The brand channel is the HTML <img>, not inlined SVG bytes.
      expect(img.tagName).toBe("IMG");
      // No inline <svg> injected into the brand block alongside the <img>.
      expect(img.parentElement?.querySelector("svg")).toBeNull();
    }
  });

  it("placement below (default): shows the POWERED BY label", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ brand_mode: "logo", brand_logo_present: true, brand_placement: "below" }),
    );
    renderShell();
    await screen.findAllByTestId("brand-logo-img");
    expect(screen.getAllByText(POWERED_BY_RE).length).toBeGreaterThan(0);
  });

  it("placement topright: hides the POWERED BY label", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ brand_mode: "logo", brand_logo_present: true, brand_placement: "topright" }),
    );
    renderShell();
    await screen.findAllByTestId("brand-logo-img");
    expect(screen.queryByText(POWERED_BY_RE)).toBeNull();
  });

  it("text mode ignores a stale topright placement and renders below (with the label)", async () => {
    // Top-right is logo-only (D6); a topright left over from a prior logo config
    // must not strand a bare company label in the header row — text always renders
    // the below block, matching the admin live preview.
    mockApi.branding.mockResolvedValue(
      brandingWith({ brand_mode: "text", brand_company: "Acme, Inc.", brand_placement: "topright" }),
    );
    renderShell();
    expect((await screen.findAllByText(POWERED_BY_RE)).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Acme, Inc.").length).toBeGreaterThan(0);
    expect(screen.queryAllByTestId("brand-logo-img")).toHaveLength(0);
  });

  it("plaque on: adds the light plaque background; off: does not", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_plaque: true }));
    const { container } = renderShell();
    await screen.findAllByTestId("brand-logo-img");
    expect(container.querySelector('[class*="f6f6f8"]')).not.toBeNull();
    cleanup();
    __resetBrandingForTests();
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_plaque: false }));
    const { container: c2 } = renderShell();
    await screen.findAllByTestId("brand-logo-img");
    expect(c2.querySelector('[class*="f6f6f8"]')).toBeNull();
  });

  it("none / default: no brand <img> and no POWERED BY label", async () => {
    renderShell();
    await waitFor(() => expect(mockApi.branding).toHaveBeenCalled());
    expect(screen.queryAllByTestId("brand-logo-img")).toHaveLength(0);
    expect(screen.queryByText(POWERED_BY_RE)).toBeNull();
  });

  it("collapsed rail: the POWERED BY block does not render", async () => {
    window.localStorage.setItem("uzi.sidebar.collapsed", "true");
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "logo", brand_logo_present: true }));
    renderShell();
    await waitFor(() => expect(mockApi.branding).toHaveBeenCalled());
    expect(screen.queryAllByTestId("brand-logo-img")).toHaveLength(0);
    expect(screen.queryByText(POWERED_BY_RE)).toBeNull();
  });

  // Layout via CLASS PROXIES (jsdom has no layout engine): the M4 below-block restyle
  // (D6/D8) is a single right-aligned line with no separator. Each assertion pairs a
  // NEGATIVE (the dropped `border-b`) with a POSITIVE (the one-row `justify-end`) so
  // neither goes vacuous, scoped to the below block via the "powered by" label's
  // container — NOT a component-wide border-b query (the header/mobile bars keep it).
  it("text-mode below block: no border-b separator, one right-aligned row", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "text", brand_company: "Acme, Inc." }));
    renderShell();
    const label = (await screen.findAllByText(POWERED_BY_RE))[0];
    const row = label.parentElement as HTMLElement;
    expect(row.className).not.toMatch(/border-b/);
    expect(row.className).toMatch(/justify-end|text-right/);
  });

  it("logo-mode below block: no border-b separator, one right-aligned row", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({ brand_mode: "logo", brand_logo_present: true, brand_placement: "below" }),
    );
    renderShell();
    const label = (await screen.findAllByText(POWERED_BY_RE))[0];
    const row = label.parentElement as HTMLElement;
    expect(row.className).not.toMatch(/border-b/);
    expect(row.className).toMatch(/justify-end|text-right/);
  });

  // Issue #828: the header divider belongs on the OUTER wrapper that bounds the
  // wordmark Link AND the below "powered by" row, so both share one bottom border —
  // NOT on the inner PoweredBy row. Assert the wrapper (row.parentElement) has
  // border-b while the inner row still does not.
  it("text-mode below block: the outer header wrapper carries the single border-b, not the row", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ brand_mode: "text", brand_company: "Acme, Inc." }));
    renderShell();
    const label = (await screen.findAllByText(POWERED_BY_RE))[0];
    const row = label.parentElement as HTMLElement;
    const outer = row.parentElement as HTMLElement;
    expect(row.className).not.toMatch(/border-b/);
    expect(outer.className).toMatch(/border-b/);
  });
});

// The M4 one-row footer (D-follow-up): the version badge (left) and the durable
// license credit (right) share a single `justify-between` row when expanded, and the
// credit is ungated — it renders during the /api/version load and on its failure.
describe("AppShell branding — one-row footer", () => {
  it("expanded: the license credit sits in a justify-between row with the version badge", async () => {
    renderShell();
    const credit = await screen.findByTestId("license-credit");
    const row = credit.parentElement as HTMLElement;
    expect(row.className).toMatch(/justify-between/);
    // The version badge (BuildInfoPopover) is the left-hand sibling in the same row.
    expect(row.textContent).toContain("MIT © Vlad Mocanu");
  });

  it("credit still renders (in the row) before the version resolves", async () => {
    mockApi.version.mockReturnValue(new Promise(() => {}));
    renderShell();
    const credit = await screen.findByTestId("license-credit");
    const row = credit.parentElement as HTMLElement;
    expect(row.className).toMatch(/justify-between/);
  });

  // Issue #828: the top divider belongs on the full-width footer ROW, not on the
  // half-width BuildInfoPopover host inside it. Pair the POSITIVE (border-t now on
  // the row) with the retained justify-between so neither assertion goes vacuous.
  it("expanded: the footer row carries the full-width border-t divider", async () => {
    renderShell();
    const credit = await screen.findByTestId("license-credit");
    const row = credit.parentElement as HTMLElement;
    expect(row.className).toMatch(/border-t/);
    expect(row.className).toMatch(/justify-between/);
  });
});

// White-label browser-tab <title> (issue #688 M2): AppShell drives document.title
// from brandTabTitle(branding) in an effect placed before the guest early return, so
// it applies signed-out too. Reset the title between cases so one test's white-label
// company cannot leak into the next.
describe("AppShell branding — browser-tab title (issue #688)", () => {
  afterEach(() => {
    document.title = DEFAULT_TITLE;
  });

  it("default branding, signed in: title is the static default", async () => {
    renderShell();
    await waitFor(() => expect(document.title).toBe(DEFAULT_TITLE));
  });

  it("full white-label, signed in: title becomes the brand company", async () => {
    mockApi.branding.mockResolvedValue(
      brandingWith({
        app_logo_mode: "custom",
        app_logo_present: true,
        app_logo_keep_name: false,
        brand_company: "Acme, Inc.",
      }),
    );
    renderShell();
    await waitFor(() => expect(document.title).toBe("Acme, Inc."));
  });

  it("full white-label, SIGNED OUT: title still becomes the brand company (applies to guests)", async () => {
    signOut();
    mockApi.branding.mockResolvedValue(
      brandingWith({
        app_logo_mode: "custom",
        app_logo_present: true,
        app_logo_keep_name: false,
        brand_company: "Acme, Inc.",
      }),
    );
    renderShell("/");
    await waitFor(() => expect(document.title).toBe("Acme, Inc."));
  });
});

// Preset-mark accessible name (a11y, carried from M2): in preset mode the mark is the
// only brand identity in a full white-label, so the <img> alt is the preset LABEL;
// custom mode keeps the generic "app logo".
describe("AppShell branding — app-mark alt text", () => {
  it("preset mode (metaminds): the app-logo <img> alt is the preset label", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "preset", app_logo_preset: "metaminds" }));
    renderShell();
    const imgs = await screen.findAllByTestId("app-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("alt")).toBe("Metaminds");
  });

  it("custom mode: the app-logo <img> keeps alt='app logo'", async () => {
    mockApi.branding.mockResolvedValue(brandingWith({ app_logo_mode: "custom", app_logo_present: true }));
    renderShell();
    const imgs = await screen.findAllByTestId("app-logo-img");
    expect(imgs.length).toBeGreaterThan(0);
    for (const img of imgs) expect(img.getAttribute("alt")).toBe("app logo");
  });
});
