// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CodeCrossCheck } from "./CodeCrossCheck";
import { mockRuns } from "../mocks/data/runs";
import type { CodeCrossCheckSummary } from "../lib/apiTypes";
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
  expect(screen.getByText(`Family: ${label} · Model: Unreported · Effort: Unreported`)).toBeTruthy();
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
it("shows missing outcome honestly", () => {
  render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_required: true, code_cross_check_summary: undefined }} /></MemoryRouter>);
  expect(screen.getByText("Outcome unavailable")).toBeTruthy();
});
it("shows incomplete reason and superseded evidence warning", () => {
  view({ outcome: "failed", reason_class: "snapshot_failed" });
  expect(screen.getByText("Incomplete")).toBeTruthy();
  expect(screen.getByText("Reason: Snapshot failed")).toBeTruthy();
  cleanup();
  view({ interrupted_at: "2026-10-09T00:00:00Z" });
  expect(screen.getByText("Superseded · Earlier attempt evidence")).toBeTruthy();
  expect(screen.getByText(/they are not current findings/)).toBeTruthy();
});
it("renders hostile prose and paths safely, strict IDs, validated child and subscription cost", () => {
  const { container } = view({ checker_run_id: "12345678-abcd-abcd-abcd-123456789abc",
    usage: { input_tokens: 1, output_tokens: 1, cache_read_tokens: 0, cache_creation_tokens: 0, cost_usd: 0, cost_status: "subscription" },
    findings: [{ id: "KEEP_id-1", path: "<img src=x>\u202E\u0007", line: 2, severity: "major",
      title: "**Finding title**", detail: "<script>alert(1)</script> [bad](javascript:alert(1))" }] });
  expect(screen.getByText(/KEEP_id-1/)).toBeTruthy();
  expect(screen.getByText("Path: <img src=x>:2")).toBeTruthy();
  expect(container.querySelector("script, img")).toBeNull();
  expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  expect(screen.getByRole("link", { name: "Checker run" }).getAttribute("href")).toBe("/runs/12345678-abcd-abcd-abcd-123456789abc");
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
  expect(screen.getByText("Pending report")).toBeTruthy();
  expect(screen.queryByText(/1 addressed,/)).toBeNull();
  cleanup();
  view({ findings: persistedFindings, dispositions: persistedDispositions, finalized_at: "2026-10-11T00:00:00Z", interrupted_at: "2026-10-11T01:00:00Z" });
  expect(screen.getByText("Superseded · Earlier attempt evidence")).toBeTruthy();
  expect(screen.getByText("Fixed first")).toBeTruthy();
  expect(screen.queryByText(/1 addressed,/)).toBeNull();
});
it.each([undefined, null])("does not turn legacy dispositions %s into not reported", (dispositions) => {
  view({ findings: persistedFindings, dispositions, finalized_at: "2026-10-11T00:00:00Z" });
  expect(screen.getByText("Dispositions unavailable")).toBeTruthy();
  expect(screen.queryByText(/Disposition:/)).toBeNull();
  expect(screen.queryByText(/not reported/)).toBeNull();
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
  expect(screen.getAllByRole("article").length).toBeGreaterThan(0);
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
  expect(screen.getAllByText("Disposition: addressed")).toHaveLength(20);
});
it("renders no opt-out evidence", () => {
  const { container } = render(<MemoryRouter><CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_required: false, code_cross_check_summary: undefined }} /></MemoryRouter>);
  expect(container.textContent).toBe("");
});
it("rejects forged links and IDs and discloses field and item truncation", () => {
  view({ checker_run_id: "../settings", findings: Array.from({ length: 21 }, (_, i) => ({
    id: i === 0 ? "bad\n" : "f" + i, path: "界".repeat(900), title: "a".repeat(4000), detail: "b".repeat(4000), severity: "minor"
  })) });
  expect(screen.queryByRole("link", { name: "Checker run" })).toBeNull();
  expect(screen.getByText(/ID unavailable/)).toBeTruthy();
  expect(screen.getByText("Findings truncated for display.")).toBeTruthy();
  expect(screen.queryByText(/Finding: f20/)).toBeNull();
});
it("shows custom guidance with the first 12 hex chars of its digest", () => {
  const { container } = view({ custom_guidance_applied: true, guidance_digest: "0123456789abcdef".repeat(4) });
  expect(screen.getByText(/Custom guidance applied/)).toBeTruthy();
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
