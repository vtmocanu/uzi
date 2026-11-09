// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CapacityRail } from "./CapacityRail";
import { aRun, aWorker } from "../pages/overviewFixtures";
import type { RunSummaryItem } from "../lib/api";

afterEach(cleanup);

const withStats = { stats_cpu_pct: 34, stats_mem_bytes: 2147483648, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" };

const NOW = Date.parse("2026-07-05T12:00:00Z");

function mount(workers: ReturnType<typeof aWorker>[], runs: RunSummaryItem[] = []) {
  return render(
    <MemoryRouter>
      <CapacityRail workers={workers} runs={runs} now={NOW} />
    </MemoryRouter>,
  );
}

const fleet = () => [
  aWorker({ id: "a", name: "alpha", active_runs: 2, max_concurrent_runs: 2, ...withStats }),
  aWorker({ id: "b", name: "bravo", active_runs: 1, max_concurrent_runs: 2, draining_since: "2026-07-05T12:00:00Z", kind: "hosted", upgrade_status: "upgrading", upgrade_target: "v0.86.1" }),
  aWorker({ id: "c", name: "charlie", status: "offline", max_concurrent_runs: 2 }),
];

describe("CapacityRail", () => {
  it("states the headline, the legend and the exclusions", () => {
    const { container } = mount(fleet());
    expect(screen.getByRole("heading", { name: /Capacity/ })).toBeTruthy();
    expect(container.querySelector("strong")?.textContent).toBe("3 of 4");
    expect(container.textContent).toContain("run slots in use");
    expect(container.textContent).toContain("In use 3");
    expect(container.textContent).toContain("Closed 1");
    expect(container.textContent).toContain("Open 0");
    expect(container.textContent).toContain("Not counted: 1 offline");
    expect(screen.getByRole("link", { name: "Workers →" }).getAttribute("href")).toBe("/workers");
  });

  it("carries the advertised-capacity note and never promises a run", () => {
    const { container } = mount(fleet());
    expect(screen.getByText("Open slots are advertised spare capacity; credentials, cordons and admission still decide claims.")).toBeTruthy();
    expect(container.textContent).not.toMatch(/available|guaranteed|will run/i);
  });

  it("lists a worker with no stats, an offline worker and every other worker", () => {
    mount(fleet());
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("bravo")).toBeTruthy();
    expect(screen.getByText("charlie")).toBeTruthy();
    expect(screen.getAllByText("No stats reported").length).toBe(2);
    expect(screen.getAllByRole("progressbar", { name: "CPU", hidden: true })).toHaveLength(1);
  });

  it("keeps a draining pill neutral and says it takes no new runs, with the roll note", () => {
    const { container } = mount(fleet());
    const pill = screen.getByText("draining");
    expect(pill.className).toContain("bg-neutral-surface");
    expect(container.textContent).toContain("Takes no new runs. Rolls to v0.86.1 once fully idle.");
  });

  it("shows no roll note for a manual cordon alone, but still says it takes no new runs", () => {
    const { container } = mount([aWorker({ manual_cordoned_at: "2026-07-05T12:00:00Z", effective_cordoned: true, max_concurrent_runs: 1 })]);
    expect(container.textContent).toContain("Takes no new runs.");
    expect(container.textContent).not.toContain("Rolls to");
  });

  it("notes no-cap workers and over-cap runs", () => {
    const { container } = mount([aWorker({ id: "x", max_concurrent_runs: null }), aWorker({ id: "y", active_runs: 3, max_concurrent_runs: 2 })]);
    expect(container.textContent).toContain("Not counted: 1 with no advertised slot cap");
    expect(container.textContent).toContain("1 run over advertised caps");
  });

  it("draws one cell per slot, by kind, up to twelve slots", () => {
    const { container } = mount(fleet());
    const count = (k: string) => container.querySelectorAll(`[data-slot="${k}"]`).length;
    expect([count("in-use"), count("closed"), count("open")]).toEqual([3, 1, 0]);
  });

  it("orders cells in use, closed, open and styles closed as hatched and open as outlined", () => {
    const { container } = mount([
      aWorker({ id: "a", name: "alpha", active_runs: 1, max_concurrent_runs: 3 }),
      aWorker({ id: "b", name: "bravo", active_runs: 0, max_concurrent_runs: 2, draining_since: "2026-07-05T12:00:00Z" }),
    ]);
    const cells = Array.from(container.querySelectorAll<HTMLElement>("[data-slot]"));
    expect(cells.map((c) => c.dataset.slot)).toEqual(["in-use", "closed", "closed", "open", "open"]);
    const closed = cells.find((c) => c.dataset.slot === "closed") as HTMLElement;
    const open = cells.find((c) => c.dataset.slot === "open") as HTMLElement;
    expect(closed.className).toContain("repeating-linear-gradient");
    expect(closed.getAttribute("style") ?? "").not.toContain("background");
    expect(closed.className).toContain("border-edge-strong");
    expect(open.className).toContain("border-edge-strong");
  });

  it("draws proportional segments past twelve slots and skips a zero segment", () => {
    const { container } = mount([aWorker({ active_runs: 5, max_concurrent_runs: 20 })]);
    const width = (k: string) => (container.querySelector(`[data-slot="${k}"]`) as HTMLElement).style.width;
    expect([width("in-use"), width("open")]).toEqual(["25%", "75%"]);
    expect(container.querySelector('[data-slot="closed"]')).toBeNull();
  });

  it("sanitizes a hostile worker name", () => {
    mount([aWorker({ name: "evil‮​name" })]);
    expect(screen.getByText("evilname")).toBeTruthy();
  });

  it("shows an empty state with a link when there are no workers", () => {
    mount([]);
    expect(screen.getByText(/No workers yet\./)).toBeTruthy();
    expect(screen.getAllByRole("link").some((l) => l.getAttribute("href") === "/workers")).toBe(true);
  });
});

const BIDI = "\u202e";
const GIB = 1024 ** 3;
const dataDisk = (pct: number) => ({ stats_disk_data_bytes: pct * GIB, stats_disk_data_total_bytes: 100 * GIB });
const badge = (container: HTMLElement, text: RegExp) =>
  Array.from(container.querySelectorAll("summary span")).find((e) => text.test(e.textContent ?? ""));

describe("CapacityRail collapsed-row problem badges", () => {
  it("shows none on a healthy worker", () => {
    const { container } = mount([aWorker({ ...withStats })]);
    const summary = container.querySelector("summary")?.textContent ?? "";
    expect(summary).not.toMatch(/quarantined|memory authority|disk \d+%|upgrade/i);
  });

  it("shows quarantine, memory authority and failed upgrade only when their fields say so", () => {
    const { container } = mount([
      aWorker({
        residue_quarantined_at: "2026-07-05T11:00:00Z",
        residue_quarantine_cause: "unreadable",
        memory_authority_lost_at: "2026-07-05T11:30:00Z",
        upgrade_status: "upgrade_failed",
      }),
    ]);
    const summary = container.querySelector("summary")?.textContent ?? "";
    expect(summary).toContain("quarantined");
    expect(summary).toContain("memory authority lost");
    expect(summary).toContain("upgrade failed");
  });

  const problemBadges = [/quarantined/, /memory authority lost/, /upgrade failed|waiting for capacity/, /disk \d+%/];
  it.each([
    ["residue_quarantined_at", { residue_quarantined_at: "2026-07-05T11:00:00Z", residue_quarantine_cause: "unreadable" }, /quarantined/, 0],
    ["memory_authority_lost_at", { memory_authority_lost_at: "2026-07-05T11:30:00Z" }, /memory authority lost/, 1],
    ["upgrade_failed", { kind: "hosted", upgrade_status: "upgrade_failed" }, /upgrade failed/, 2],
    ["capacity_blocked", { kind: "hosted", upgrade_status: "capacity_blocked" }, /waiting for capacity/, 2],
  ] as const)("shows only the %s badge when only that field is set", (_name, fields, own, group) => {
    const { container } = mount([aWorker({ ...fields })]);
    const summary = container.querySelector("summary")?.textContent ?? "";
    expect(summary).toMatch(own);
    problemBadges.forEach((other, i) => {
      if (i !== group) expect(summary).not.toMatch(other);
    });
  });

  it("does not show the upgrade badge for a merely outdated worker", () => {
    const { container } = mount([aWorker({ upgrade_status: "outdated" })]);
    expect(container.querySelector("summary")?.textContent ?? "").not.toMatch(/outdated|upgrade/i);
  });

  it("flags disk at 85% but not 84%", () => {
    const { container, unmount } = mount([aWorker(dataDisk(84))]);
    expect(badge(container, /^disk \d+%$/)).toBeUndefined();
    unmount();
    const r = mount([aWorker(dataDisk(85))]);
    const b = badge(r.container, /^disk 85%$/) as HTMLElement;
    expect(b).toBeTruthy();
    expect(b.getAttribute("title")).toBe("Disk /data 85% full");
  });

  it("flags an inode-only danger and a disk-only worker with no CPU/memory sample", () => {
    const { container } = mount([
      aWorker({
        id: "i",
        stats_disk_dind_bytes: 10 * GIB,
        stats_disk_dind_total_bytes: 100 * GIB,
        stats_disk_dind_inodes: 96,
        stats_disk_dind_total_inodes: 100,
      }),
      aWorker({ id: "d", ...dataDisk(90), status: "offline" }),
    ]);
    const inode = badge(container, /^disk 96%$/) as HTMLElement;
    expect(inode.getAttribute("title")).toBe("Disk dind 96% full (inodes)");
    const off = badge(container, /^disk 90%$/) as HTMLElement;
    expect(off.getAttribute("title")).toBe("Disk /data 90% full (last known)");
  });

  it("matches the disk gauge percent when stats exist", () => {
    const { container } = mount([aWorker({ ...withStats, ...dataDisk(91) })]);
    const gauge = screen.getByRole("progressbar", { name: "Disk /data", hidden: true });
    expect(badge(container, /^disk \d+%$/)?.textContent).toBe(`disk ${gauge.getAttribute("aria-valuenow")}%`);
  });

  it("matches the dind gauge when inodes, not bytes, are the danger", () => {
    const { container } = mount([
      aWorker({
        ...withStats,
        stats_disk_dind_bytes: 20 * GIB,
        stats_disk_dind_total_bytes: 100 * GIB,
        stats_disk_dind_inodes: 91,
        stats_disk_dind_total_inodes: 100,
      }),
    ]);
    const gauge = screen.getByRole("progressbar", { name: "Disk dind", hidden: true });
    expect(gauge.getAttribute("aria-valuenow")).toBe("91");
    expect(badge(container, /^disk \d+%$/)?.textContent).toBe(`disk ${gauge.getAttribute("aria-valuenow")}%`);
  });
});

describe("CapacityRail expanded row", () => {
  it("shows the gauges for a stats worker and the disk line for a disk-only worker", () => {
    mount([aWorker({ id: "a", ...withStats }), aWorker({ id: "b", ...dataDisk(50) })]);
    expect(screen.getByRole("progressbar", { name: "CPU", hidden: true })).toBeTruthy();
    expect(screen.getByText("No stats reported")).toBeTruthy();
    expect(screen.getByText(/disk 50\/100 GiB/)).toBeTruthy();
  });

  it("notes last-known readings on an offline stats worker", () => {
    const { container } = mount([aWorker({ ...withStats, status: "offline" })]);
    expect(container.textContent).toContain("Last known readings. Not counted in run slots while offline.");
  });

  it("joins reported runs to loaded runs, with fallbacks, and drops pending outcomes", () => {
    const runs = [
      aRun({ id: "run-with-iid-0001", issue_iid: 42, issue_title: "Fix the thing" }),
      aRun({ id: "run-no-iid-00002", issue_iid: null, issue_title: "Task only" }),
      aRun({ id: "run-blank-000003", issue_iid: null, issue_title: "  " }),
    ] as RunSummaryItem[];
    const w = aWorker({
      reported_runs: [
        { run_id: "run-with-iid-0001", phase: "awaiting_followup", claim_generation: 1 },
        { run_id: "run-no-iid-00002", phase: "running", claim_generation: 1 },
        { run_id: "run-blank-000003", phase: "running", claim_generation: 1 },
        { run_id: "unknown-run-id-9999", phase: "running", claim_generation: 1 },
        { run_id: "pending-run-0000", phase: "running", claim_generation: 1, terminal_pending: true },
      ],
    });
    const { container } = mount([w], runs);
    expect(screen.getByText("On this worker")).toBeTruthy();
    const joined = screen.getByRole("link", { name: "#42 Fix the thing" });
    expect(joined.getAttribute("href")).toBe("/runs/run-with-iid-0001");
    expect(screen.getByRole("link", { name: "Task only" }).getAttribute("href")).toBe("/runs/run-no-iid-00002");
    expect(screen.getByRole("link", { name: "run-blan" })).toBeTruthy();
    const unjoined = screen.getByText("run unknown-");
    expect(unjoined.getAttribute("title")).toBe("unknown-run-id-9999");
    expect(container.textContent).toContain("awaiting followup");
    expect(container.textContent).not.toContain("pending-");
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
  });

  it("falls back to the short run id for a blank title even when the issue number is known", () => {
    const runs = [aRun({ id: "run-iid-blank-0042", issue_iid: 42, issue_title: " \t " })] as RunSummaryItem[];
    const { container } = mount([aWorker({ reported_runs: [{ run_id: "run-iid-blank-0042", phase: "running", claim_generation: 1 }] })], runs);
    const link = screen.getByRole("link", { name: "run-iid-" });
    expect(link.textContent).toBe("run-iid-");
    expect(link.getAttribute("href")).toBe("/runs/run-iid-blank-0042");
    expect(container.textContent).not.toContain("#42");
  });

  it("sizes the five largest, largest first, and lists an unsized run after them", () => {
    const sizes = [3, 7, 1, 9, 5, 2, 8];
    const w = aWorker({
      reported_runs: [
        { run_id: "u0-unsized-xx", phase: "running", claim_generation: 1 },
        ...sizes.map((_g, i) => ({ run_id: `s${i}-sized-run-aaaa`, phase: "running", claim_generation: 1 })),
        { run_id: "u1-unsized-yy", phase: "running", claim_generation: 1 },
      ],
      run_disk: sizes.map((g, i) => ({ run_id: `s${i}-sized-run-aaaa`, home_bytes: g * GIB, cache_bytes: 0, truncated: i === 3, sampled_at: "2026-07-05T11:59:00Z" })),
    });
    const { container } = mount([w]);
    const rows = Array.from(container.querySelectorAll("details [data-run-row]")).map((p) => p.textContent ?? "");
    expect(rows).toHaveLength(9);
    const sized = rows.slice(0, 5).map((t) => /running · (.*)$/.exec(t)?.[1]);
    expect(sized).toEqual(["at least 9 GiB", "8 GiB", "7 GiB", "5 GiB", "3 GiB"]);
    // Unsized rows follow in report order: the two smallest sampled runs fell out of the top five.
    const rest = rows.slice(5);
    expect(rest.every((t) => !/GiB/.test(t))).toBe(true);
    expect(rest.map((t) => t.slice(0, 12))).toEqual(["run u0-unsiz", "run s2-sized", "run s5-sized", "run u1-unsiz"]);
  });

  it("leaves the largest-run line out of the rail's gauges", () => {
    const w = aWorker({
      ...withStats,
      run_disk: [{ run_id: "r-disk-0001", home_bytes: 4 * GIB, cache_bytes: 0, truncated: false, sampled_at: "2026-07-05T11:59:00Z" }],
    });
    const { container } = mount([w]);
    expect(screen.getByRole("progressbar", { name: "CPU", hidden: true })).toBeTruthy();
    expect(container.textContent).not.toContain("Largest run");
  });

  it("titles the section Last reported when offline", () => {
    mount([aWorker({ status: "offline", reported_runs: [{ run_id: "abcdefgh1234", phase: "running", claim_generation: 1 }] })]);
    expect(screen.getByText("Last reported")).toBeTruthy();
  });

  it("omits the section with no reported runs", () => {
    mount([aWorker()]);
    expect(screen.queryByText("On this worker")).toBeNull();
  });
});

describe("CapacityRail footer", () => {
  const footer = (container: HTMLElement) => container.querySelector("p.font-mono")?.textContent ?? null;

  it("shows version and uptime for an online worker, never a heartbeat", () => {
    const { container } = mount([
      aWorker({ version: "1.2.3", online_since: "2026-07-05T09:30:00Z", last_heartbeat_at: "2026-07-05T11:59:00Z" }),
    ]);
    expect(footer(container)).toBe("v1.2.3 · up 2h 30m");
  });

  it("shows last seen for an offline worker", () => {
    const { container } = mount([aWorker({ status: "offline", version: "1.0.0", last_heartbeat_at: "2026-07-05T10:00:00Z" })]);
    expect(footer(container)).toContain("v1.0.0 · last seen");
    expect(footer(container)).not.toContain("up ");
  });

  it("says never seen for a null heartbeat and omits an unparseable one", () => {
    const a = mount([aWorker({ status: "offline", last_heartbeat_at: null })]);
    expect(footer(a.container)).toBe("never seen");
    a.unmount();
    const b = mount([aWorker({ status: "offline", last_heartbeat_at: "garbage" })]);
    expect(footer(b.container)).toBeNull();
  });

  it("omits up when online_since is absent, null or unparseable", () => {
    for (const online_since of [undefined, null, "garbage"]) {
      const { container, unmount } = mount([aWorker({ version: "1", online_since })]);
      expect(footer(container)).toBe("v1");
      unmount();
    }
  });

  it("omits the version part for null and bidi-only versions, and renders no footer when empty", () => {
    const a = mount([aWorker({ version: BIDI })]);
    expect(a.container.querySelector("p.font-mono")).toBeNull();
    a.unmount();
    const b = mount([aWorker({ version: null })]);
    expect(b.container.querySelector("p.font-mono")).toBeNull();
  });
});

describe("CapacityRail notes", () => {
  it("explains an ephemeral worker", () => {
    const { container } = mount([aWorker({ ephemeral: true })]);
    expect(container.textContent).toContain("Auto-provisioned on demand for a run that needed a capability no online worker had.");
  });

  it("shows a live lease note and nothing for an expired one", () => {
    const future = new Date(Date.now() + 3600_000).toISOString();
    const past = new Date(Date.now() - 3600_000).toISOString();
    const a = mount([aWorker({ ephemeral_lease_expires_at: future })]);
    expect(a.container.textContent).toMatch(/Kept warm for same-branch follow-up work until .+ \(.+ left\), then removed\./);
    a.unmount();
    const b = mount([aWorker({ ephemeral_lease_expires_at: past })]);
    expect(b.container.textContent).not.toContain("Kept warm");
  });

  it("links held work when custody decisions are needed", () => {
    const { container } = mount([aWorker({ custody_decisions_needed: 2 })]);
    expect(container.textContent).toContain("Retaining unpublished committed work that needs an owner decision.");
    expect(screen.getByRole("link", { name: "Review held work →" }).getAttribute("href")).toBe("/workers?tab=workers#recovery-holds");
  });

  it("gives the restart hint when memory authority is lost", () => {
    const { container } = mount([aWorker({ memory_authority_lost_at: "2026-07-05T11:00:00Z" })]);
    expect(container.textContent).toContain("Restart the worker's container to regain memory authority.");
  });

  it("shows none of them on a plain worker", () => {
    const { container } = mount([aWorker()]);
    expect(container.textContent).not.toMatch(/Retaining|Restart the worker|Kept warm|Auto-provisioned/);
  });
});

describe("CapacityRail untrusted worker text", () => {
  it("renders run id, phase and version inert", () => {
    const evil = `<img src=x onerror=alert(1)>${BIDI}`;
    const { container } = mount([
      aWorker({
        version: evil,
        reported_runs: [{ run_id: `${BIDI}${evil}`, phase: evil, claim_generation: 1 }],
      }),
    ]);
    // Positive pins: the stripped values are still rendered, as text.
    expect(screen.getByText("run <img src")).toBeTruthy();
    expect(screen.getByText("run <img src").getAttribute("title")).toBe("<img src=x onerror=alert(1)>");
    expect(screen.getByText("<img src=x onerror=alert(1)>", { exact: true, selector: "span.font-mono.text-faint" })).toBeTruthy();
    expect(container.querySelector("p.font-mono")?.textContent).toBe("v<img src=x onerror=alert(1)>");
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).not.toContain(BIDI);
    for (const el of Array.from(container.querySelectorAll("*"))) {
      for (const attr of ["title", "href"]) expect(el.getAttribute(attr) ?? "").not.toContain(BIDI);
    }
  });

  it("renders a loaded run's issue title inert", () => {
    const evil = `${BIDI}<img src=x onerror=alert(1)>`;
    const runs = [aRun({ id: "run-evil-000001", issue_iid: 7, issue_title: evil })] as RunSummaryItem[];
    const { container } = mount(
      [aWorker({ reported_runs: [{ run_id: "run-evil-000001", phase: "running", claim_generation: 1 }] })],
      runs,
    );
    const link = screen.getByRole("link", { name: /^#7 / });
    expect(link.textContent).not.toContain(BIDI);
    expect(link.textContent).toContain("<img src=x onerror=alert(1)>");
    // The truncated link carries its full (stripped) label as a tooltip.
    expect(link.getAttribute("title")).toBe(link.textContent);
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).not.toContain(BIDI);
    for (const el of Array.from(container.querySelectorAll("*"))) {
      for (const attr of ["title", "href", "aria-label"]) expect(el.getAttribute(attr) ?? "").not.toContain(BIDI);
    }
  });
});
