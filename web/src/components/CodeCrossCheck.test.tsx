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
  expect(screen.getByText("Checker cost: Subscription · subscription usage")).toBeTruthy();
  expect(container.textContent).not.toContain("$0");
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
