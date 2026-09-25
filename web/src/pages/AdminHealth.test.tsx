// @vitest-environment jsdom
//
// Health tab component tests (PRD #1484 M4, triage-first layout PRD #1648 M3/M4). They mount
// the real AdminHealth page (which wraps itself in AdminShell) and feed it fixtures through
// the mocked api client — the same pattern CustodyBoardAlert.test.tsx uses. Covered: the
// header line (Copy diagnostics), the attention card in its danger / warn / unknown forms or
// the all-clear line, the All checks inventory (disclosure, chips, counts, poll survival,
// in-page jumps with reduced motion), na on a no-hosted-workers fixture, and the fleet table.
// Severity is asserted by ACCESSIBLE NAME / TEXT (the badge word, the shape's label), never a
// colour class; the tab pip's worded aria-label is asserted; and a hostile worker name /
// blocking reason rendered into a `title=` attribute is asserted on the attribute.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AdminHealth } from "./AdminHealth";
import { api, type AdminWorker, type HealthDoc } from "../lib/api";
import {
  degradedDoc,
  healthySilentDoc,
  incidentDoc,
  incidentFleetWorkers,
  noHostedWorkersDoc,
  unknownDoc,
} from "../mocks/data/health";

// The page reads GET /api/admin/health (via the shared useAdminHealth hook, used by both the
// page and AdminShell's tab pip) and GET /api/admin/workers. Mock only those two.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn(), adminListWorkers: vi.fn() } };
});
const mockApi = vi.mocked(api);

const originalMatchMedia = window.matchMedia;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.matchMedia = originalMatchMedia;
});

async function renderHealth(doc: HealthDoc, fleet: AdminWorker[] = []) {
  mockApi.getAdminHealth.mockResolvedValue(doc);
  mockApi.adminListWorkers.mockResolvedValue({ workers: fleet });
  const utils = render(
    <MemoryRouter initialEntries={["/admin/health"]}>
      <AdminHealth />
    </MemoryRouter>,
  );
  // The Copy diagnostics button renders only once the doc resolves, so waiting on it ensures
  // no assertion races the load (and it is unambiguous — one per page).
  await waitFor(() => expect(screen.getByText("Copy diagnostics")).toBeTruthy());
  return utils;
}

// An attention item (or null) for a check id (the id has a dot, so an attribute selector).
function attentionItem(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector(`[id="c-${id}"]`);
}

// The ids of the attention items, in DOM order.
function attentionIds(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll('[id^="c-"]')).map((el) => el.id.slice(2));
}

// The attention band: the header that holds the verdict heading.
function band(title: string): HTMLElement {
  const header = screen.getByRole("heading", { name: title }).closest("header");
  if (!header) throw new Error(`no band for ${title}`);
  return header;
}

function groupButton(name: string): HTMLElement {
  return screen.getByRole("button", { name });
}

// Recompute a doc's tally + status after a test reshapes its checks (mirrors the fixture's).
function retally(doc: HealthDoc): HealthDoc {
  const counts = { ok: 0, warn: 0, danger: 0, unknown: 0, na: 0 };
  for (const c of doc.checks) counts[c.severity as keyof typeof counts]++;
  const status = counts.danger > 0 ? "danger" : counts.warn + counts.unknown > 0 ? "warn" : "ok";
  return { ...doc, counts, status };
}

function withSeverity(doc: HealthDoc, over: Record<string, string>): HealthDoc {
  return retally({ ...doc, checks: doc.checks.map((c) => (over[c.id] ? { ...c, severity: over[c.id] } : c)) });
}

function stubReducedMotion(reduce: boolean) {
  window.matchMedia = ((query: string) => ({
    matches: reduce && query.includes("prefers-reduced-motion: reduce"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
}

describe("AdminHealth — attention card and all-clear line (M3)", () => {
  it("all clear (silent): one quiet line, no attention card, no tab pip", async () => {
    const { container } = await renderHealth(healthySilentDoc());
    expect(screen.getByText("All systems normal.")).toBeTruthy();
    expect(screen.getByText("Nothing needs attention.")).toBeTruthy();
    expect(screen.getByText("refreshes every 10 s")).toBeTruthy();
    // No attention band (no alert/status region, no verdict heading) and no attention items.
    expect(container.querySelector('[role="alert"], [role="status"]')).toBeNull();
    expect(screen.queryByRole("heading", { name: /blocking|warning/ })).toBeNull();
    expect(attentionIds(container)).toEqual([]);
    // Every check's shape carries its word; all 14 read OK, none Warn/Danger.
    expect(screen.getAllByText("OK").length).toBe(14);
    expect(screen.queryByText(/^(Warn|Danger|Unknown)$/)).toBeNull();
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });

  it("warn (degraded): a warn status band and both warn items expanded with their action", async () => {
    const { container } = await renderHealth(degradedDoc());
    // Count-aware warn headline, derived from counts (2 warn, 0 unknown → "2 warnings").
    const b = band("2 warnings, nothing is blocked");
    expect(b.getAttribute("role")).toBe("status");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(within(b).getByText("Work is still flowing. Worst first, each with what to do.")).toBeTruthy();
    expect(within(b).getByText("2 Warn")).toBeTruthy();
    // Both warn items are fully expanded (no <details>, nothing to click): the action is there.
    expect(container.querySelector("details")).toBeNull();
    expect(attentionIds(container)).toEqual(["forge.ciwatch", "schedules.paused"]);
    const ci = attentionItem(container, "forge.ciwatch")!;
    expect(within(ci).getByText("What to do.")).toBeTruthy();
    expect(within(ci).getByText(/Raise CI_WATCH_MAX_REFS/)).toBeTruthy();
    expect(within(ci).getByText("27 of 20 watched")).toBeTruthy();
    const paused = attentionItem(container, "schedules.paused")!;
    expect(within(paused).getByText(/will not run until they unpause/)).toBeTruthy();
    // Tab pip: present, warning-worded accessible name, count 2. Awaited because AdminShell's
    // own useAdminHealth resolves independently of the page's.
    const pip = await screen.findByLabelText("2 health checks need attention: 2 warning");
    expect(within(pip).getByText("2")).toBeTruthy();
  });

  it("danger (incident): alert band, derived sub line, worst first, Fleet link only on fleet.* items", async () => {
    const { container } = await renderHealth(incidentDoc(), incidentFleetWorkers());
    // Count-aware danger headline (3 danger; the 1 warn is not blocking and is excluded).
    const b = band("uzi cannot run work: 3 blocking checks");
    expect(b.getAttribute("role")).toBe("alert");
    // K = 4 attention − 3 danger = 1.
    expect(
      within(b).getByText("Plus 1 more that need attention without blocking work. Worst first, each with what to do."),
    ).toBeTruthy();
    expect(within(b).getByText("3 Danger")).toBeTruthy();
    expect(within(b).getByText("1 Warn")).toBeTruthy();
    expect(within(b).queryByText(/OK/)).toBeNull();
    expect(attentionIds(container)).toEqual(["fleet.roll", "fleet.capacity", "queue.waiting", "forge.ciwatch"]);
    // The Fleet link sits on the two fleet.* items only, and its target exists.
    const fleetLinks = screen.getAllByRole("link", { name: /See the affected workers in Fleet/ });
    expect(fleetLinks.length).toBe(2);
    expect(within(attentionItem(container, "fleet.roll")!).getByRole("link", { name: /affected workers/ })).toBeTruthy();
    expect(within(attentionItem(container, "fleet.capacity")!).getByRole("link", { name: /affected workers/ })).toBeTruthy();
    expect(within(attentionItem(container, "queue.waiting")!).queryByRole("link", { name: /affected workers/ })).toBeNull();
    expect(fleetLinks[0].getAttribute("href")).toBe("#fleet");
    const fleet = document.getElementById("fleet")!;
    expect(fleet).toBeTruthy();
    // Activating it scrolls the Fleet card into view (smooth by default).
    fleet.scrollIntoView = vi.fn();
    fireEvent.click(fleetLinks[0]);
    expect(fleet.scrollIntoView).toHaveBeenCalledWith({ block: "start", behavior: "smooth" });
    // The command has a Copy button; the footer carries the check id and its docs link.
    const roll = attentionItem(container, "fleet.roll")!;
    expect(within(roll).getByRole("button", { name: "Copy" })).toBeTruthy();
    expect(within(roll).getByRole("link", { name: "Docs: worker-upgrades" })).toBeTruthy();
    // Tab pip worded as danger, count 4 (3 danger + 1 warn). Awaited for the same reason.
    const pip = await screen.findByLabelText("4 health checks need attention: 3 danger, 1 warning");
    expect(within(pip).getByText("4")).toBeTruthy();
    // The cross-user fleet table is populated across two owners, with the blocking cause.
    expect((await screen.findAllByText("user.a@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("user.c@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("worker: ImagePullBackOff")).length).toBe(4);
  });

  it("orders danger before unknown before warn and counts K over every non-danger item", async () => {
    // slack.socket (after the warn forge.ciwatch in registry order) turned unknown.
    const { container } = await renderHealth(withSeverity(incidentDoc(), { "slack.socket": "unknown" }));
    expect(attentionIds(container)).toEqual(["fleet.roll", "fleet.capacity", "queue.waiting", "slack.socket", "forge.ciwatch"]);
    const b = band("uzi cannot run work: 3 blocking checks");
    expect(within(b).getByText(/^Plus 2 more that need attention/)).toBeTruthy();
    expect(within(b).getAllByText(/^\d+ (Danger|Unknown|Warn)$/).map((el) => el.textContent)).toEqual([
      "3 Danger",
      "1 Unknown",
      "1 Warn",
    ]);
  });

  it("danger with nothing else: the sub line drops the 'Plus K more' clause", async () => {
    await renderHealth(withSeverity(incidentDoc(), { "forge.ciwatch": "ok" }));
    const b = band("uzi cannot run work: 3 blocking checks");
    expect(within(b).getByText("Worst first, each with what to do.")).toBeTruthy();
    expect(within(b).queryByText(/Plus/)).toBeNull();
  });

  it("unknown: Unknown items render expanded in a warn band (a stale signal is never green)", async () => {
    const { container } = await renderHealth(unknownDoc());
    // Overall ranks unknown as warn: the verdict is the warn one, not danger. The headline
    // folds unknowns into the warn count (4 unknown, 0 warn → "4 warnings"); the pip names
    // them separately.
    const b = band("4 warnings, nothing is blocked");
    expect(b.getAttribute("role")).toBe("status");
    expect(within(b).getByText("4 Unknown")).toBeTruthy();
    expect(attentionIds(container).sort()).toEqual(["controller.report", "fleet.capacity", "fleet.roll", "queue.waiting"]);
    const roll = attentionItem(container, "fleet.roll")!;
    expect(within(roll).getByText("Unknown")).toBeTruthy();
    expect(within(roll).getByText(/Check that the worker controller is running/)).toBeTruthy();
    expect(attentionItem(container, "db")).toBeNull();
    expect(await screen.findByLabelText("4 health checks need attention: 4 unknown")).toBeTruthy();
  });

  it("Copy diagnostics copies the whole document as JSON and confirms", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const doc = degradedDoc();
    await renderHealth(doc);
    expect(screen.getByText(/^Checked /)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    expect(writeText).toHaveBeenCalledWith(JSON.stringify(doc, null, 2));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
  });
});

describe("AdminHealth — All checks inventory (M4)", () => {
  it("counts: header and per-group status, na excluded from every denominator", async () => {
    await renderHealth(healthySilentDoc());
    expect(screen.getByText("14 of 14 passing")).toBeTruthy();
    for (const t of ["all 3 passing", "all 2 passing", "all 3 passing", "all 2 passing", "all 4 passing"]) {
      expect(screen.getAllByText(t).length).toBeGreaterThan(0);
    }
    expect(screen.getAllByText(/^all \d passing$/).length).toBe(5);
  });

  it("incident: attention groups read 'K of N need attention'", async () => {
    await renderHealth(incidentDoc());
    expect(screen.getByText("10 of 14 passing")).toBeTruthy();
    expect(screen.getByText("2 of 3 need attention")).toBeTruthy();
    expect(screen.getAllByText("1 of 2 need attention").length).toBe(2); // queue, integrations
    expect(screen.getByText("all 3 passing")).toBeTruthy(); // control
  });

  it("groups are collapsed by default; toggling sets aria-expanded and shows summaries", async () => {
    await renderHealth(healthySilentDoc());
    for (const name of ["Workers and capacity", "Queue", "Control plane", "Integrations", "Housekeeping"]) {
      expect(groupButton(name).getAttribute("aria-expanded")).toBe("false");
    }
    expect(screen.queryByText("Database reachable (2ms); schema at head.")).toBeNull();

    const control = groupButton("Control plane");
    fireEvent.click(control);
    expect(control.getAttribute("aria-expanded")).toBe("true");
    const region = document.getElementById(control.getAttribute("aria-controls")!)!;
    expect(within(region).getByText("Database reachable (2ms); schema at head.")).toBeTruthy();
    expect(within(region).getByText("db")).toBeTruthy(); // the check id
    expect(within(region).getAllByText("OK").length).toBe(3);

    fireEvent.click(control);
    expect(control.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("Database reachable (2ms); schema at head.")).toBeNull();
  });

  it("an opened group stays open when the 10 s poll delivers a new document", async () => {
    await renderHealth(healthySilentDoc());
    fireEvent.click(groupButton("Control plane"));
    // The poll: a fresh document object (now degraded), fetched on the poller's tick
    // (usePollWhileVisible re-runs on visibilitychange as well as on its interval).
    mockApi.getAdminHealth.mockResolvedValue(degradedDoc());
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(await screen.findByRole("heading", { name: "2 warnings, nothing is blocked" })).toBeTruthy();
    expect(groupButton("Control plane").getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("Database reachable (2ms); schema at head.")).toBeTruthy();
    expect(groupButton("Queue").getAttribute("aria-expanded")).toBe("false");
  });

  it("an attention chip links to its item and scrolls it into view, smooth by default", async () => {
    const { container } = await renderHealth(incidentDoc());
    const chip = screen.getByRole("link", { name: "Danger Worker image roll" });
    expect(chip.getAttribute("href")).toBe("#c-fleet.roll");
    const target = attentionItem(container, "fleet.roll")!;
    target.scrollIntoView = vi.fn();
    fireEvent.click(chip);
    expect(target.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "smooth" });
    // Focus follows the jump, so the next Tab continues from the item.
    expect(document.activeElement).toBe(target);
    // The warn chip is a link too; passing checks are plain text, not links.
    expect(screen.getByRole("link", { name: "Warn CI watch capacity" }).getAttribute("href")).toBe("#c-forge.ciwatch");
    expect(screen.queryByRole("link", { name: /Database/ })).toBeNull();
  });

  it("the chip jump respects reduced motion", async () => {
    stubReducedMotion(true);
    const { container } = await renderHealth(incidentDoc());
    const target = attentionItem(container, "queue.waiting")!;
    target.scrollIntoView = vi.fn();
    fireEvent.click(screen.getByRole("link", { name: "Danger Runs waiting for a worker" }));
    expect(target.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "auto" });
  });

  it("na (no hosted workers): N/A chips with their word, 'not applicable' in the counts, still all clear", async () => {
    await renderHealth(noHostedWorkersDoc());
    expect(screen.getByText("All systems normal.")).toBeTruthy();
    // fleet.roll, controller.report and slack.socket are N/A (never green, never gone).
    expect(screen.getAllByText("N/A").length).toBe(3);
    expect(screen.getByText("11 of 11 passing, 3 not applicable")).toBeTruthy();
    expect(screen.getAllByText("all 2 passing, 1 not applicable").length).toBe(2); // workers, control
    expect(screen.getByText("all 1 passing, 1 not applicable")).toBeTruthy(); // integrations
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });

  it("a group whose checks are all N/A reads 'not applicable'", async () => {
    await renderHealth(withSeverity(noHostedWorkersDoc(), { "forge.ciwatch": "na" }));
    expect(screen.getByText("not applicable")).toBeTruthy();
    expect(screen.getByText("10 of 10 passing, 4 not applicable")).toBeTruthy();
  });

  it("no interactive element is nested inside another", async () => {
    const { container } = await renderHealth(incidentDoc(), incidentFleetWorkers());
    for (const button of Array.from(container.querySelectorAll("button"))) {
      expect(button.querySelector("a, button, input, select, textarea, [href]")).toBeNull();
    }
    for (const link of Array.from(container.querySelectorAll("a"))) {
      expect(link.closest("button")).toBeNull();
      expect(link.querySelector("a, button")).toBeNull();
    }
  });
});

describe("AdminHealth — untrusted strings in attributes", () => {
  it("strips a hostile worker name and blocking reason before they reach a title= attribute", async () => {
    // Control chars are BUILT at runtime from char codes, never written as raw bytes or
    // \u escapes in this source (a raw NUL makes the file binary; the sourceBytes gate
    // forbids it). RLO is a RIGHT-TO-LEFT OVERRIDE (U+202E), NUL is U+0000, BEL is U+0007 —
    // all of which stripUnsafeChars removes. React escapes the visible text; the title is
    // the sink a rendered-text sweep would miss, so it must be stripped too (issue #124).
    const RLO = String.fromCharCode(0x202e);
    const NUL = String.fromCharCode(0);
    const BEL = String.fromCharCode(7);
    const hostile: AdminWorker = {
      ...incidentFleetWorkers()[0],
      id: "w-hostile",
      name: `${RLO}evil${NUL}name`,
      upgrade_status: "upgrade_failed",
      upgrade_blocking_container: `work${NUL}er`,
      upgrade_blocking_reason: `Pull${RLO}BackOff${BEL}`,
      owner_email: "user.a@uzi.local",
    };
    await renderHealth(incidentDoc(), [hostile]);

    // The worker-name cell's title carries the STRIPPED name (no U+202E, no NUL). Awaited
    // because the fleet table loads on its own fetch, after the health document. toBe with
    // the fully-stripped value is strictly stronger than a "does not contain" check.
    const nameCell = await screen.findByTitle("evilname");
    expect(nameCell.getAttribute("title")).toBe("evilname");

    // The blocking cell's title is stripped too (the hostile reason matches no k8s enum, so
    // likelyCause yields null and the title falls back to the stripped container:reason).
    const blockingCell = screen.getByTitle("worker: PullBackOff");
    expect(blockingCell.getAttribute("title")).toBe("worker: PullBackOff");
  });
});
