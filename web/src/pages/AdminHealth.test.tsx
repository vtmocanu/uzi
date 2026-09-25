// @vitest-environment jsdom
//
// Health tab component tests (PRD #1484 M4). They mount the real AdminHealth page (which
// wraps itself in AdminShell) and feed it fixtures through the mocked api client — the same
// pattern CustodyBoardAlert.test.tsx uses. The five health STATES are covered: healthy
// (all-ok), warn (degraded), danger (incident), unknown, and na on a no-hosted-workers
// fixture. Severity is asserted by ACCESSIBLE NAME / TEXT (the pill's word), never a colour
// class; the tab pip's presence and its worded aria-label are asserted; and a hostile worker
// name / blocking reason rendered into a `title=` attribute is asserted on the attribute.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
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

// The <details> for a check id (the id has a dot, so an attribute selector, not a class one).
function checkDetails(container: HTMLElement, id: string): HTMLDetailsElement {
  const el = container.querySelector(`[id="c-${id}"]`);
  if (!(el instanceof HTMLElement)) throw new Error(`no check row for ${id}`);
  return el as HTMLDetailsElement;
}

describe("AdminHealth — the five states", () => {
  it("healthy (all-ok): verdict, an OK tally, no attention list, every group passing, no tab pip", async () => {
    const { container } = await renderHealth(healthySilentDoc());
    expect(screen.getByRole("heading", { name: "All systems normal" })).toBeTruthy();
    // The OK tally pill (count 14) is present; no warn/danger/unknown pills anywhere.
    expect(screen.getAllByText("OK").length).toBeGreaterThan(0);
    expect(screen.queryByText("Warn")).toBeNull();
    expect(screen.queryByText("Danger")).toBeNull();
    // Every group header reads "all passing".
    expect(screen.getAllByText("all passing").length).toBe(5);
    // No tab pip when nothing needs attention.
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
    // A danger/unknown-only expansion rule: with all ok, nothing is force-open.
    expect(checkDetails(container, "db").open).toBe(false);
  });

  it("warn (degraded): verdict, Warn pills, a needs-attention list, and a warning tab pip with the count", async () => {
    await renderHealth(degradedDoc());
    // Count-aware warn headline, derived from counts (2 warn, 0 unknown → "2 warnings").
    // Asserting the real count text: a regression to a constant headline, or a wrong count,
    // fails here.
    expect(screen.getByRole("heading", { name: "2 warnings, nothing is blocked" })).toBeTruthy();
    // Severity asserted by TEXT (the pill word), not a colour class.
    expect(screen.getAllByText("Warn").length).toBeGreaterThan(0);
    // Needs-attention list: the two warn checks link to their check by title.
    expect(screen.getByRole("link", { name: "CI watch capacity" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Paused schedules" })).toBeTruthy();
    // Tab pip: present, warning-worded accessible name, count 2 (non-ok/non-na count). Awaited
    // because AdminShell's own useAdminHealth resolves independently of the page's.
    const pip = await screen.findByLabelText("2 health checks need attention: 2 warning");
    expect(within(pip).getByText("2")).toBeTruthy();
  });

  it("danger (incident): verdict, Danger pills, a danger tab pip, and the fleet table populated with blocking cause", async () => {
    const { container } = await renderHealth(incidentDoc(), incidentFleetWorkers());
    // Count-aware danger headline, derived from counts.danger (3 danger checks; the 1 warn is
    // not blocking and is excluded). A wrong count or a warn-inclusive count fails here.
    expect(screen.getByRole("heading", { name: "uzi cannot run work: 3 blocking checks" })).toBeTruthy();
    expect(screen.getAllByText("Danger").length).toBeGreaterThan(0);
    // Tab pip worded as danger, count 4 (3 danger + 1 warn). Awaited for the same reason.
    const pip = await screen.findByLabelText("4 health checks need attention: 3 danger, 1 warning");
    expect(within(pip).getByText("4")).toBeTruthy();
    // fleet.roll is danger, so its row renders EXPANDED.
    expect(checkDetails(container, "fleet.roll").open).toBe(true);
    // The cross-user fleet table is populated across two owners, with the blocking cause.
    expect((await screen.findAllByText("user.a@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("user.c@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("worker: ImagePullBackOff")).length).toBe(4);
  });

  it("unknown: an Unknown pill renders and the unknown check is expanded (a stale signal is never green)", async () => {
    const { container } = await renderHealth(unknownDoc());
    expect(screen.getAllByText("Unknown").length).toBeGreaterThan(0);
    // controller.report is unknown → expanded; a green check stays collapsed.
    expect(checkDetails(container, "controller.report").open).toBe(true);
    expect(checkDetails(container, "db").open).toBe(false);
    // Overall ranks unknown as warn: the verdict is the warn one, not danger. Unknowns fold
    // into the warn count (4 unknown, 0 warn → "4 warnings"), matching the pip's aria wording.
    expect(screen.getByRole("heading", { name: "4 warnings, nothing is blocked" })).toBeTruthy();
  });

  it("na (no hosted workers): the hosted checks read N/A and the overall verdict is still ok", async () => {
    await renderHealth(noHostedWorkersDoc());
    expect(screen.getByRole("heading", { name: "All systems normal" })).toBeTruthy();
    // fleet.roll, controller.report and slack.socket are N/A (never green, never gone).
    expect(screen.getAllByText("N/A").length).toBeGreaterThanOrEqual(3);
    // A check that cannot apply still carries a signal, but does not count as attention.
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
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
