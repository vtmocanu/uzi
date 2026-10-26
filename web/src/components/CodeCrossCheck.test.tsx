// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, fireEvent as domFireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CodeCrossCheck } from "./CodeCrossCheck";
import { mockRuns } from "../mocks/data/runs";
import type { CodeCrossCheckSummary } from "../lib/apiTypes";
// Testing Library's event map does not include the native details toggle event.
const fireEvent = Object.assign(domFireEvent, {
  toggle: (node: Element) => domFireEvent(node, new Event("toggle")),
});
afterEach(cleanup);
const summary: CodeCrossCheckSummary = {
  stage: "code", round: 1, candidate_generation: 1, base_commit: null, head_commit: null,
  candidate_digest: "", checker_run_id: null, checker_harness: "codex", checker_model: null,
  checker_effort: null, outcome: "completed", reason_class: null, findings: [],
  interrupted_at: null, finalized_at: null, deadline_at: "",
};
function view(over: Partial<CodeCrossCheckSummary> = {}) {
  return render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_summary: { ...summary, ...over } }} /></MemoryRouter>);
}
it("renders a named standalone Card with heading, outcome and checker action", () => {
  view({ checker_run_id: "12345678-abcd-abcd-abcd-123456789abc" });
  const region = screen.getByRole("region", { name: "Code cross-check" });
  const heading = screen.getByRole("heading", { name: "Code cross-check · Advisory before publication", level: 2 });
  const shell = region.firstElementChild!;
  for (const token of ["rounded-xl", "border", "border-edge", "bg-surface", "p-5"]) expect(shell.classList.contains(token)).toBe(true);
  for (const token of ["rounded-md", "p-3", "border", "p-5"]) expect(region.classList.contains(token)).toBe(false);
  expect(screen.getAllByText("Completed")).toHaveLength(1);
  expect(screen.getByText("Completed").closest("header")).toBe(heading.closest("header"));
  const link = screen.getByRole("link", { name: "Open checker run" });
  expect(link.closest("header")).toBe(heading.closest("header"));
  expect(link.classList.contains("focus-visible:ring-brand/60")).toBe(true);
  expect(region.querySelector(".animate-pulse")).toBeNull();
});
it("shows current empty evidence with both snapshot SHAs null and unavailable cost", () => {
  view();
  expect(screen.getByText("Completed")).toBeTruthy();
  expect(screen.getByText("No findings recorded.")).toBeTruthy();
  expect(screen.getByText("Checker cost: Unavailable")).toBeTruthy();
  expect(screen.queryByText(/Checked SHA/)).toBeNull();
});
it.each([
  ["codex", "Codex"], ["claude", "Claude"], [null, "Unreported"],
] as const)("displays checker family %s", (checker_harness, label) => {
  view({ checker_harness });
  const family = screen.getByText(`Family: ${label}`);
  expect(screen.getByText("Model: Unreported").closest("p")).toBe(family.closest("p"));
  expect(screen.getByText("Effort: Unreported").closest("p")).toBe(family.closest("p"));
});
it.each([
  ["worker_unsupported", "Worker does not support code cross-check"],
  ["snapshot_failed", "Snapshot failed"],
  ["interrupted", "Interrupted"],
  ["timeout", "Checker timed out"],
  ["model_timeout", "Checker timed out"],
  ["no_local_checker", "No local checker available"],
  ["checker_unavailable", "No local checker available"],
])("labels closed reason %s", (reason_class, label) => {
  view({ outcome: "failed", reason_class });
  expect(screen.getByText(`Reason: ${label}`)).toBeTruthy();
});
it("bounds unknown reasons and renders them as plain text", () => {
  const { container } = view({ reason_class: "<script>unknown</script>" + "x".repeat(300) });
  const reason = screen.getByText(/^Reason:/);
  expect(reason.textContent).toBe("Reason: " + ("<script>unknown</script>" + "x".repeat(300)).slice(0, 200));
  expect(container.querySelector("script")).toBeNull();
});
it("shows an absent required result on a terminal run honestly", () => {
  render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], status: "completed", code_cross_check_required: true, code_cross_check_summary: undefined }} /></MemoryRouter>);
  expect(screen.getByText("Not recorded")).toBeTruthy();
});
it("shows incomplete reason and superseded evidence warning", () => {
  view({ outcome: "failed", reason_class: "snapshot_failed" });
  expect(screen.getByText("Incomplete")).toBeTruthy();
  expect(screen.getByText("Reason: Snapshot failed")).toBeTruthy();
  cleanup();
  view({ interrupted_at: "2026-10-09T00:00:00Z" });
  expect(screen.getByText("Earlier attempt evidence").closest("header")?.textContent).toContain("Superseded");
  expect(screen.getByText(/they are not current findings/)).toBeTruthy();
});
it("renders hostile prose and paths safely, strict IDs, validated child and subscription cost", () => {
  const { container } = view({ checker_run_id: "12345678-abcd-abcd-abcd-123456789abc",
    usage: { input_tokens: 1, output_tokens: 1, cache_read_tokens: 0, cache_creation_tokens: 0, cost_usd: 0, cost_status: "subscription" },
    findings: [{ id: "KEEP_id-1", path: "<img src=x>\u202E\u0007", line: 2, severity: "major",
      title: "**Finding title**", detail: "<script>alert(1)</script> [bad](javascript:alert(1))" }] });
  expect(screen.getByText(/KEEP_id-1/)).toBeTruthy();
  const chip = screen.getByText("<img src=x>");
  expect(chip.tagName).toBe("CODE");
  expect(chip.closest("p")).toBeTruthy();
  expect(chip.querySelector("img, em, strong")).toBeNull();
  expect(container.querySelector("script, img")).toBeNull();
  expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  expect(screen.getByRole("link", { name: "Open checker run" }).getAttribute("href")).toBe("/runs/12345678-abcd-abcd-abcd-123456789abc");
  expect(screen.getByText("Checker cost: No estimate · subscription usage")).toBeTruthy();
  expect(container.textContent).not.toContain("$0");
});
const persistedFindings = [
  { id: "KEEP_id-1", severity: "major", title: "First finding" },
  { id: "f2", severity: "minor", title: "Second finding" },
  { id: "f3", severity: "critical", title: "Third finding" },
];
const persistedDispositions = [
  { finding_id: "f2", disposition: "declined", reason: "**Verified second**" },
  { finding_id: "KEEP_id-1", disposition: "addressed", reason: "**Fixed first**" },
  { finding_id: "f3", disposition: "not_reported", reason: "" },
];
it("associates persisted dispositions by stable identity and counts only finalized active evidence", () => {
  const { container } = view({ findings: persistedFindings, dispositions: persistedDispositions, finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("1 addressed, 1 declined, 1 not reported")).toBeTruthy();
  expect(screen.getByText("Finalized")).toBeTruthy();
  const articles = container.querySelectorAll("article");
  expect(articles[0].textContent).toContain("KEEP_id-1");
  expect(articles[0].textContent).toContain("Disposition: addressed");
  expect(articles[0].querySelector("strong")?.textContent).toBe("Fixed first");
  expect(articles[1].textContent).toContain("Disposition: declined");
  expect(articles[1].querySelector("strong")?.textContent).toBe("Verified second");
  cleanup();
  view({ findings: persistedFindings, dispositions: persistedDispositions });
  expect(screen.getByText("Dispositions pending")).toBeTruthy();
  expect(screen.queryByText(/1 addressed,/)).toBeNull();
  cleanup();
  view({ findings: persistedFindings, dispositions: persistedDispositions, finalized_at: "2026-10-11T00:00:00Z", interrupted_at: "2026-10-11T01:00:00Z" });
  expect(screen.getByText("Earlier attempt evidence").closest("header")?.textContent).toContain("Superseded");
  expect(screen.getByText("Fixed first")).toBeTruthy();
  expect(screen.queryByText(/1 addressed,/)).toBeNull();
});
it.each([undefined, null])("does not turn legacy dispositions %s into not reported", (dispositions) => {
  view({ findings: persistedFindings, dispositions, finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("Dispositions unavailable")).toBeTruthy();
  expect(screen.queryByText(/Disposition:/)).toBeNull();
  expect(screen.queryByText(/^[0-9]+ addressed,.*not reported/)).toBeNull();
});
it.each([
  ["unknown", [persistedFindings[0]], [{ finding_id: "unknown", disposition: "addressed", reason: "hidden reason" }]],
  ["duplicate dispositions", persistedFindings, [...persistedDispositions, persistedDispositions[0]]],
  ["duplicate findings", [persistedFindings[0], persistedFindings[0]], [persistedDispositions[1]]],
  ["forged identity", [{ id: "KEEP_id-1\n" }], [persistedDispositions[1]]],
  ["bad status", persistedFindings, [{ finding_id: "f2", disposition: "accepted", reason: "hidden reason" }]],
  ["missing reason", persistedFindings, [{ finding_id: "f2", disposition: "addressed" }]],
  ["reason bytes", [persistedFindings[1]], [{ finding_id: "f2", disposition: "addressed", reason: "界".repeat(341) + "ab" }]],
  ["reason length", persistedFindings, [{ finding_id: "f2", disposition: "addressed", reason: "x".repeat(1025) }]],
  ["invalid UTF16", persistedFindings, [{ finding_id: "f2", disposition: "addressed", reason: "\ud800" }]],
  ["incomplete finalized", persistedFindings, []],
  ["overlimit findings", Array.from({ length: 21 }, (_, i) => ({ id: "f" + i })), persistedDispositions],
  ["overlimit dispositions", persistedFindings, Array.from({ length: 21 }, () => persistedDispositions[0])],
] as const)("rejects %s as a batch without binding or counts", (_name, findings, dispositions) => {
  view({ findings: [...findings], dispositions: [...dispositions], finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("Dispositions unavailable")).toBeTruthy();
  expect(screen.queryByText(/Disposition:/)).toBeNull();
  expect(screen.queryByText(/addressed,/)).toBeNull();
  expect(screen.queryByText("hidden reason")).toBeNull();
  expect(screen.getAllByRole("article", { hidden: true }).length).toBeGreaterThan(0);
});
it("renders bounded hostile reasons through hardened Markdown and preserves sibling details", () => {
  const hostile = "**Fixed**\n<script>alert(1)</script> [bad](javascript:alert(1))\u001b[31m\u0007\u202e";
  const { container } = view({ findings: persistedFindings, dispositions: [
    { ...persistedDispositions[1], reason: hostile }, persistedDispositions[0], persistedDispositions[2],
  ], finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("1 addressed, 1 declined, 1 not reported")).toBeTruthy();
  expect(screen.getByText("Fixed").tagName).toBe("STRONG");
  expect(screen.getByText("Verified second")).toBeTruthy();
  expect(container.querySelector("script, img")).toBeNull();
  expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  for (const unsafe of ["\u001b", "\u0007", "\u202e"]) expect(container.textContent).not.toContain(unsafe);
});
it("accepts twenty dispositions and a reason at the UTF8 byte boundary", () => {
  const findings = Array.from({ length: 20 }, (_, i) => ({ id: "f" + i }));
  const dispositions = findings.map(f => ({ finding_id: f.id, disposition: "addressed", reason: "界".repeat(341) + "a" }));
  view({ findings, dispositions, finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("20 addressed, 0 declined, 0 not reported")).toBeTruthy();
  expect(screen.getAllByText("addressed")).toHaveLength(20);
});
it("renders no opt-out evidence", () => {
  const { container } = render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_required: false, code_cross_check_summary: undefined }} /></MemoryRouter>);
  expect(container.textContent).toBe("");
});
it("rejects forged links and IDs and discloses field and item truncation", () => {
  view({ checker_run_id: "../settings", findings: Array.from({ length: 21 }, (_, i) => ({
    id: i === 0 ? "bad\n" : "f" + i, path: "界".repeat(900), title: "a".repeat(4000), detail: "b".repeat(4000), severity: "minor"
  })) });
  expect(screen.queryByRole("link", { name: "Open checker run" })).toBeNull();
  expect(screen.getByText(/ID unavailable/)).toBeTruthy();
  expect(screen.getByText("Findings truncated for display.")).toBeTruthy();
  expect(screen.queryByText(/Finding: f20/)).toBeNull();
});
it("shows custom guidance with the first 12 hex chars of its digest", () => {
  const { container } = view({ custom_guidance_applied: true, guidance_digest: "0123456789abcdef".repeat(4) });
  expect(screen.getByText(/Custom guidance applied/).closest("p")).toBe(screen.getByText("Checker cost: Unavailable").closest("p"));
  const code = container.querySelector("code");
  expect(code?.textContent).toBe("0123456789ab");
  expect(code?.className).toContain("font-mono");
});
it.each([
  [{}],
  [{ custom_guidance_applied: false, guidance_digest: "0123456789abcdef".repeat(4) }],
])("shows nothing about guidance when it was not applied (%j)", (over) => {
  const { container } = view(over);
  expect(screen.queryByText(/Custom guidance/)).toBeNull();
  expect(container.querySelector("code")).toBeNull();
});
it("omits a digest that is not hex", () => {
  const { container } = view({ custom_guidance_applied: true, guidance_digest: "<b>not-hex</b>" });
  expect(screen.getByText("Custom guidance applied")).toBeTruthy();
  expect(container.querySelector("code")).toBeNull();
});

it.each([3, 5])("keeps the first three of %s findings outside native disclosure in source order", (count) => {
  const { container } = view({ findings: Array.from({ length: count }, (_, i) => ({
    id: "f" + i, severity: i === 0 ? "__proto__" : "minor", path: "pkg/__init__.py",
    title: "**Title " + i + "**", detail: "Inline `code` [safe](https://example.com) [bad](javascript:alert(1))",
  })) });
  const rows = [...container.querySelectorAll("article")];
  expect(rows.map(row => row.querySelector("code")?.textContent)).toEqual(Array.from({ length: count }, (_, i) => "f" + i));
  expect(rows[0].textContent).toContain("Severity unavailable");
  expect(rows[1].querySelector(".shrink-0")?.textContent).toBe("minor");
  expect(screen.getAllByText("pkg/__init__.py")[0].tagName).toBe("CODE");
  expect(rows[0].querySelector(".judge-prose strong")?.textContent).toBe("Title 0");
  expect(rows[0].querySelector(".judge-prose.text-muted code")?.textContent).toBe("code");
  expect(rows[0].querySelector('a[href="https://example.com"]')).toBeTruthy();
  expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  const details = container.querySelector("details");
  expect(rows.slice(0, 3).every(row => !row.closest("details"))).toBe(true);
  if (count === 3) { expect(details).toBeNull(); return; }
  expect(rows.slice(3).every(row => row.closest("details") === details)).toBe(true);
  const toggle = screen.getByText("Show 2 more findings");
  expect(toggle.tagName).toBe("SUMMARY");
  expect(toggle.className).toContain("focus-visible:ring-2");
  (details as HTMLDetailsElement).open = true;
  fireEvent.toggle(details!);
  expect(screen.getByText("Hide 2 more findings")).toBeTruthy();
  (details as HTMLDetailsElement).open = false;
  fireEvent.toggle(details!);
  expect(screen.getByText("Show 2 more findings")).toBeTruthy();
});
it("discloses item and UTF8-budget truncation outside closed and open disclosure", () => {
  const { container } = view({ findings: Array.from({ length: 21 }, (_, i) => ({
    id: "f" + i, path: "界".repeat(512), title: "界".repeat(512), detail: "界".repeat(2048), severity: "major",
  })) });
  const rows = container.querySelectorAll("article");
  expect(rows).toHaveLength(20);
  // Three complete 9,216-byte rows leave 5,120 bytes: path/title consume 3,072,
  // then detail retains 682 three-byte characters, leaving two bytes unused.
  expect(rows[3].querySelector(".judge-prose.text-muted")?.textContent).toBe("界".repeat(682));
  expect(rows[4].querySelector(".judge-prose.text-muted")?.textContent).toBe("");
  const notice = screen.getByText("Findings truncated for display.");
  expect(notice.closest("details")).toBeNull();
  const details = container.querySelector("details")!;
  expect(details.open).toBe(false);
  details.open = true;
  fireEvent.toggle(details);
  expect(screen.getByText("Hide 17 more findings")).toBeTruthy();
  expect(notice.closest("details")).toBeNull();
});
it.each(["running", "queued", "completed", "failed", "cancelled"] as const)("labels missing required code evidence for %s", status => {
  const terminal = ["completed", "failed", "cancelled"].includes(status);
  render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], status, code_cross_check_required: true, code_cross_check_summary: undefined }} /></MemoryRouter>);
  expect(screen.getByText(terminal ? "Not recorded" : "Pending")).toBeTruthy();
  expect(screen.getByText(terminal ? "This run ended without a code cross-check result." : "Runs after implementation, before the PR is published.")).toBeTruthy();
  if (!terminal) {
    expect(screen.queryByText("Not recorded")).toBeNull();
    expect(screen.queryByText(/This run ended/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Open checker run" })).toBeNull();
    expect(screen.queryByText(/Family:|Model:|Checker cost:|Dispositions/)).toBeNull();
  }
});
it.each(["pending", "completed", "failed", "future"])("keeps outcome %s honest without valid dispositions", outcome => {
  view({ outcome, findings: [{ id: "f1" }], dispositions: [], finalized_at: "recorded" });
  expect(screen.getByText(outcome === "pending" ? "Pending" : outcome === "completed" ? "Completed" : outcome === "failed" ? "Incomplete" : "Outcome unavailable")).toBeTruthy();
  expect(screen.getByText("Dispositions unavailable")).toBeTruthy();
  expect(screen.queryByText(/0 addressed/)).toBeNull();
  if (outcome === "pending") expect(screen.getByText("No result recorded yet.")).toBeTruthy();
});
it("shortens only a validated SHA and preserves its full title in the footer", () => {
  const sha = "abcdef0123456789".repeat(2) + "abcdef01";
  view({ head_commit: sha });
  const chip = screen.getByText(sha.slice(0, 12));
  expect(chip.tagName).toBe("CODE");
  expect(chip.getAttribute("title")).toBe(sha);
  expect(chip.closest("p")?.textContent).toContain("Family: Codex");
  cleanup();
  view({ head_commit: sha + "\\n" });
  expect(screen.queryByText(/Checked SHA/)).toBeNull();
});
it("retains malformed-item and unavailable-batch copy", () => {
  view({ findings: [null, "bad"] as unknown as CodeCrossCheckSummary["findings"] });
  expect(screen.getAllByText("Finding unavailable")).toHaveLength(2);
  cleanup();
  view({ findings: null as unknown as CodeCrossCheckSummary["findings"] });
  expect(screen.getByText("Findings unavailable")).toBeTruthy();
});

it.each(["path", "title", "detail", "items"])("discloses isolated code %s truncation outside either disclosure state", field => {
  const findings = Array.from({ length: field === "items" ? 21 : 5 }, (_, i) => ({
    id: "f" + i, path: "path_" + i, severity: "minor", title: "Title " + i, detail: "Detail " + i,
  }));
  if (field === "path") findings[0].path = "P".repeat(513);
  if (field === "title") findings[0].title = "T".repeat(513);
  if (field === "detail") findings[0].detail = "D".repeat(2049);
  const { container } = view({ findings });
  const notice = screen.getByText("Findings truncated for display.");
  const details = container.querySelector("details")!;
  expect(details.open).toBe(false);
  expect(notice.closest("details")).toBeNull();
  expect(container.querySelectorAll("article")).toHaveLength(field === "items" ? 20 : 5);
  if (field === "items") expect(screen.queryByText("Title 20")).toBeNull();
  details.open = true;
  fireEvent.toggle(details);
  expect(screen.getByText(field === "items" ? "Hide 17 more findings" : "Hide 2 more findings")).toBeTruthy();
  expect(notice.closest("details")).toBeNull();
  expect(container.querySelector(".animate-pulse, .overflow-auto, .overflow-y-auto, .overflow-scroll, .overflow-y-scroll")).toBeNull();
  expect(screen.queryByText(/Checking/)).toBeNull();
});


const coverageAccount = {
  version: 1 as const,
  entries: [
    { path: "src/a.ts", status: "examined" as const, reason: "read implementation", recorded_read: true },
    { path: "src/b.ts", status: "examined" as const, reason: "checked branch", recorded_read: true },
    { path: "src/c.ts", status: "examined" as const, reason: "checked types", recorded_read: false },
    { path: "src/d.ts", status: "examined" as const, reason: "checked errors", recorded_read: true },
    { path: "src/e.ts", status: "examined" as const, reason: "checked inputs", recorded_read: true },
    { path: "src/f.ts", status: "examined" as const, reason: "checked outputs", recorded_read: true },
    { path: "src/g.ts", status: "examined" as const, reason: "checked callers", recorded_read: false },
    { path: "src/h.ts", status: "examined" as const, reason: "checked cleanup", recorded_read: true },
    { path: "src/i.ts", status: "examined" as const, reason: "checked tests", recorded_read: true },
    { path: "src/deleted.ts", status: "unassessed" as const, reason: "deleted path", recorded_read: false },
    { path: "src/k.ts", status: "unassessed" as const, reason: "", recorded_read: false },
    { path: "src/l.ts", status: "unassessed" as const, reason: "not reached", recorded_read: false },
  ],
  inventory_incomplete: false, omitted_paths: 0, omitted_count_exact: true,
  unknown_judgments: 0, invalid_judgments: 0, omitted_judgments: 0, activity_complete: true, reasons: [],
};

it("coverage displays the complete stored twelve-path account with derived counts", () => {
  const { container } = view({ coverage: coverageAccount });
  expect(screen.getByText("12 stored paths: 9 examined, 0 excluded, 3 unassessed")).toBeTruthy();
  expect(screen.getByText("Activity: complete")).toBeTruthy();
  expect(screen.getByText("Advisory review account; not a correctness certification.")).toBeTruthy();
  const rows = container.querySelectorAll('[aria-label="Review coverage account"] li');
  const expected = [
    ["src/a.ts", "examined", "read implementation", "yes"],
    ["src/b.ts", "examined", "checked branch", "yes"],
    ["src/c.ts", "examined", "checked types", "no"],
    ["src/d.ts", "examined", "checked errors", "yes"],
    ["src/e.ts", "examined", "checked inputs", "yes"],
    ["src/f.ts", "examined", "checked outputs", "yes"],
    ["src/g.ts", "examined", "checked callers", "no"],
    ["src/h.ts", "examined", "checked cleanup", "yes"],
    ["src/i.ts", "examined", "checked tests", "yes"],
    ["src/deleted.ts", "unassessed", "deleted path", "no"],
    ["src/k.ts", "unassessed", "not reported", "no"],
    ["src/l.ts", "unassessed", "not reached", "no"],
  ];
  expect(rows).toHaveLength(12);
  expect(container.textContent).not.toContain("\\n");
  expected.forEach(([path, status, reason, read], i) => {
    expect(rows[i].textContent).toBe(path + " · " + status + " · Recorded read: " + read + "Reason: " + reason);
  });
});

it("coverage renders exclusions, judgment counts and partial activity without inventing omitted inventory", () => {
  view({ coverage: { ...coverageAccount, entries: [{ path: "generated.ts", status: "excluded", reason: "generated source", recorded_read: false }],
    inventory_incomplete: true, unknown_judgments: 2, invalid_judgments: 3, omitted_judgments: 4,
    activity_complete: false, reasons: ["activity_incomplete"] } });
  expect(screen.getByText("1 stored paths: 0 examined, 1 excluded, 0 unassessed")).toBeTruthy();
  expect(screen.getByText("Reason: generated source")).toBeTruthy();
  expect(screen.getByText("Judgments: 2 unknown, 3 invalid, 4 omitted")).toBeTruthy();
  expect(screen.getByText("Activity: partial")).toBeTruthy();
  expect(screen.queryByText(/Inventory incomplete/)).toBeNull();
  expect(screen.queryByText(/0 paths omitted/)).toBeNull();
});

it.each([
  [true, 7, "Inventory incomplete: 7 paths omitted"],
  [false, 7, "Inventory incomplete: at least 7 paths omitted; total unknown"],
  [false, 0, "Inventory incomplete: omitted path count unknown"],
] as const)("coverage explains inventory omission exact=%s count=%s", (exact, count, expected) => {
  view({ coverage: { ...coverageAccount, inventory_incomplete: true, omitted_count_exact: exact, omitted_paths: count, reasons: ["inventory_truncated"] } });
  expect(screen.getByText(expected)).toBeTruthy();
});

it.each([undefined, null])("coverage preserves legacy absence %s", coverage => {
  view({ coverage });
  expect(screen.getByText("coverage not reported")).toBeTruthy();
  expect(screen.queryByText(/stored paths:/)).toBeNull();
});

it.each([
  false, [], {}, { ...coverageAccount, version: 2 }, { ...coverageAccount, entries: [null] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], status: "approved" }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], recorded_read: null }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], reason: "" }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], status: "unassessed", recorded_read: true }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], status: "excluded", recorded_read: true }] },
  { ...coverageAccount, entries: [coverageAccount.entries[0], coverageAccount.entries[0]] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], path: "界".repeat(171) }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], reason: "界".repeat(86) }] },
  { ...coverageAccount, entries: [{ ...coverageAccount.entries[0], path: "\ud800" }] },
  { ...coverageAccount, entries: Array.from({ length: 129 }, (_, i) => ({ ...coverageAccount.entries[0], path: String(i) })) },
  { ...coverageAccount, omitted_paths: -1 }, { ...coverageAccount, omitted_paths: 1 },
  { ...coverageAccount, omitted_count_exact: false }, { ...coverageAccount, activity_complete: null },
  { ...coverageAccount, unknown_judgments: 1.5 }, { ...coverageAccount, reasons: [null] },
  { ...coverageAccount, extra: "x".repeat(128 * 1024) },
])("coverage rejects malformed evidence %#", coverage => {
  view({ coverage: coverage as unknown as CodeCrossCheckSummary["coverage"] });
  expect(screen.getByText("Coverage unavailable")).toBeTruthy();
  expect(screen.queryByText(/stored paths:/)).toBeNull();
});

it("coverage shows all 128 stored entries and full bounded fields", () => {
  view({ coverage: { ...coverageAccount, entries: Array.from({ length: 128 }, (_, i) => ({
    path: i === 127 ? "z".repeat(512) : "path-" + i, status: "unassessed", reason: "r".repeat(256), recorded_read: false,
  })) } });
  expect(screen.getByText("128 stored paths: 0 examined, 0 excluded, 128 unassessed")).toBeTruthy();
  expect(screen.getByText("z".repeat(512))).toBeTruthy();
  expect(screen.getAllByText("Reason: " + "r".repeat(256))).toHaveLength(128);
});

it("coverage keeps hostile Markdown HTML ANSI and bidi plain in historical evidence", () => {
  const { container } = view({ interrupted_at: "2026-10-09T00:00:00Z", coverage: {
    ...coverageAccount, entries: [{ path: "<img src=x>**path**\u001b[31m\u202e", status: "excluded",
      reason: "[bad](javascript:alert(1))\u001b]0;hidden\u0007\u202e", recorded_read: false }],
  } });
  expect(screen.getByText("Earlier attempt evidence")).toBeTruthy();
  expect(screen.getByText("<img src=x>**path**")).toBeTruthy();
  expect(screen.getByText("Reason: [bad](javascript:alert(1))")).toBeTruthy();
  const account = container.querySelector('[aria-label="Review coverage account"]')!;
  expect(account.querySelector("img, a, strong, script")).toBeNull();
  for (const code of [27, 7, 0x202e]) expect(account.textContent).not.toContain(String.fromCodePoint(code));
  expect(account.textContent).not.toContain("hidden");
});
