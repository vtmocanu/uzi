import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, within, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { CodeCrossCheckSummary, PlanCrossCheckSummary } from "../lib/apiTypes";
import { mockRuns } from "../mocks/data/runs";
import { PlanCrossCheck, PlanCrossCheckEvent } from "./PlanCrossCheck";
import { CodeCrossCheck } from "./CodeCrossCheck";
import { PlanPanel } from "../pages/runView/PlanPanel";

vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, listSecrets: vi.fn().mockResolvedValue({ secrets: [] }) } };
});

const originalTheme = document.documentElement.getAttribute("data-theme");
afterEach(() => {
  cleanup();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});
const child = "11111111-1111-4111-8111-111111111111";
const path = "pkg/__init__/" + "long_directory_with_underscores/".repeat(10) + "file_name.ts";
const plan: PlanCrossCheckSummary = {
  round: 1, verdict: "revise", reason_class: "revise", historical: false,
  checker_run_id: child, checker_model: "long_model_identifier_".repeat(9),
  checker_effort: "long_effort_metadata_".repeat(9), checker_model_source: "pin",
  checker_effort_source: "worker default", usage: null,
  findings: { summary: "Overview uses `plan_summary`.", items: Array.from({ length: 5 }, (_, i) => ({
    file: path, severity: i < 2 ? "info" : "warning",
    summary: "Title `title_" + i + "`", rationale: "Reason `reason_" + i + "`",
  })) },
};
const codeFindings = Array.from({ length: 5 }, (_, i) => ({
  id: "f" + i, path, line: i + 1, severity: i < 2 ? "minor" : "major",
  title: "Title `title_" + i + "`", detail: "Detail `detail_" + i + "`",
}));
const code: CodeCrossCheckSummary = {
  stage: "code", round: 1, candidate_generation: 1, base_commit: null,
  head_commit: "abcdef0123456789abcdef0123456789abcdef01", candidate_digest: "",
  checker_run_id: child, checker_harness: "codex", checker_model: "long_model_identifier_".repeat(9),
  checker_effort: "long_effort_metadata_".repeat(9), outcome: "completed", reason_class: null,
  interrupted_at: null, finalized_at: "2026-10-13T00:00:00Z", deadline_at: "",
  custom_guidance_applied: true, guidance_digest: "0123456789abcdef".repeat(4),
  findings: codeFindings,
  dispositions: Array.from({ length: 5 }, (_, i) => ({
    finding_id: "f" + i, disposition: i % 2 ? "declined" : "addressed", reason: "Reason `reason_" + i + "`",
  })),
};

function noOverflow(shells: Element[], width: number) {
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  expect(document.body.scrollWidth).toBeLessThanOrEqual(width);
  for (const shell of shells) {
    expect(shell.scrollWidth).toBeLessThanOrEqual(shell.clientWidth);
    expect(shell.getBoundingClientRect().right).toBeLessThanOrEqual(width);
    for (const node of shell.querySelectorAll("li, article, .judge-prose, details")) {
      expect(["auto", "scroll"]).not.toContain(getComputedStyle(node).overflowY);
      expect(["auto", "scroll"]).not.toContain(getComputedStyle(node).overflowX);
    }
  }
}

async function keyboardFocus(target: HTMLElement) {
  // Enter keyboard modality, then focus the exact target rather than matching page text.
  await userEvent.tab();
  target.focus();
  expect(document.activeElement).toBe(target);
  expect(target.matches(":focus-visible")).toBe(true);
  const style = getComputedStyle(target);
  expect(style.boxShadow).not.toBe("none");
  expect(style.boxShadow).toMatch(/2px/);
}

const matrix = [1100, 375].flatMap(width => ["dawn", "ember"].map(theme => ({ width, theme })));
it.each(matrix)("renders and natively expands all card surfaces at $width px in $theme", async ({ width, theme }) => {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  const parked = mockRuns.find(run => run.id === "run-awaiting")!;
  render(<MemoryRouter><main className="space-y-4 p-4">
    <div data-testid="standalone"><PlanCrossCheck run={{ ...mockRuns[0], plan_cross_check_summary: plan }} /></div>
    <CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_summary: code }} />
    <div data-testid="gate"><PlanPanel run={{ ...parked, plan_cross_check_summary: plan }}
      busy={false} canSteer={false} onApprove={vi.fn()} onReject={vi.fn()} /></div>
    <PlanCrossCheckEvent payload={{ verdict: plan.verdict, reason_class: plan.reason_class,
      checker_run_id: child, findings: plan.findings }} />
  </main></MemoryRouter>);
  const standalone = within(screen.getByTestId("standalone")).getByRole("region", { name: "Plan cross-check" });
  const inset = within(screen.getByTestId("gate")).getByRole("region", { name: "Plan cross-check" });
  const codeRegion = screen.getByRole("region", { name: "Code cross-check" });
  const event = screen.getByRole("region", { name: "Plan cross-check event" });
  const regions = [standalone, codeRegion, inset, event];
  const shells = regions.map(region => region === inset ? region : region.firstElementChild!);
  const surface = theme === "dawn" ? "rgb(255, 255, 255)" : "rgb(15, 18, 26)";
  const edge = theme === "dawn" ? "rgb(214, 219, 228)" : "rgb(39, 45, 60)";
  regions.forEach((region, i) => {
    const heading = within(region).getByRole("heading", { level: region === inset ? 3 : 2 });
    expect(heading.closest("header")).toBe(within(region).getByRole("link", { name: "Open checker run" }).closest("header"));
    const style = getComputedStyle(shells[i]);
    expect(style.backgroundColor).toBe(surface);
    expect(style.borderTopColor).toBe(edge);
    expect(style.borderTopStyle).toBe("solid");
    expect(style.borderTopWidth).toBe("1px");
    expect(style.paddingTop).toBe(region === inset ? "16px" : "20px");
    expect(style.borderTopLeftRadius).toBe(region === inset ? "10px" : "14px");
    if (region !== inset) expect(getComputedStyle(region).borderTopWidth).toBe("0px");
    expect(region.querySelector(".animate-pulse")).toBeNull();
    expect(within(region).queryByText(/Checking/)).toBeNull();
    const rows = [...region.querySelectorAll(region === codeRegion ? "article" : "li")];
    expect(rows).toHaveLength(5);
    for (const [index, row] of rows.entries()) {
      expect(row.querySelector(".judge-prose code")?.textContent).toBe("title_" + index);
      expect(row.querySelector(".judge-prose.text-muted code")?.textContent).toBe((region === codeRegion ? "detail_" : "reason_") + index);
      const chip = [...row.querySelectorAll("code")].find(node => node.textContent === path)!;
      expect(chip).toBeTruthy();
      expect(chip.querySelector("em, strong")).toBeNull();
      expect(getComputedStyle(chip).backgroundColor).toBe(theme === "dawn" ? "rgb(235, 238, 243)" : "rgb(23, 27, 38)");
    }
    if (region === codeRegion) expect(rows[0].querySelectorAll(".judge-prose")[2].querySelector("code")?.textContent).toBe("reason_0");
    else expect(within(region).getByText("plan_summary").closest(".judge-prose")).toBeTruthy();
  });
  const gateHeading = within(screen.getByTestId("gate")).getByRole("heading", { name: /Plan awaiting the owner's approval/ });
  expect(inset.getBoundingClientRect().top).toBeGreaterThanOrEqual(gateHeading.parentElement!.parentElement!.getBoundingClientRect().bottom);
  expect(within(codeRegion).getByText("abcdef012345").getAttribute("title")).toBe(code.head_commit);
  expect(within(codeRegion).getByText(/Custom guidance applied/).textContent).toContain("0123456789ab");
  expect(within(codeRegion).getByText("3 addressed, 2 declined, 0 not reported")).toBeTruthy();
  noOverflow(shells, width);
  for (const region of regions) {
    const rows = [...region.querySelectorAll(region === codeRegion ? "article" : "li")];
    for (const row of rows.slice(0, 3)) await expect.element(page.elementLocator(row)).toBeVisible();
    for (const row of rows.slice(3)) await expect.element(page.elementLocator(row)).not.toBeVisible();
    const link = within(region).getByRole("link", { name: "Open checker run" });
    await keyboardFocus(link);
    await userEvent.tab();
    const toggle = within(region).getByText("Show 2 more findings");
    expect(document.activeElement).toBe(toggle);
    expect(getComputedStyle(toggle).boxShadow).not.toBe("none");
    await userEvent.type(page.elementLocator(toggle), "{Enter}", { skipClick: true });
    await waitFor(() => expect(toggle.textContent).toBe("Hide 2 more findings"));
    expect(document.activeElement).toBe(toggle);
    for (const row of rows.slice(3)) await expect.element(page.elementLocator(row)).toBeVisible();
    noOverflow(shells, width);
    await page.elementLocator(toggle).click();
    await waitFor(() => expect(toggle.textContent).toBe("Show 2 more findings"));
    for (const row of rows.slice(3)) await expect.element(page.elementLocator(row)).not.toBeVisible();
    await page.elementLocator(toggle).click();
    await waitFor(() => expect(toggle.textContent).toBe("Hide 2 more findings"));
    for (const row of rows.slice(3)) await expect.element(page.elementLocator(row)).toBeVisible();
  }
  noOverflow(shells, width);
  for (const region of [standalone, codeRegion, inset]) {
    for (const label of [/^Effort:/, /^Checker cost:/]) {
      const value = within(region).getByText(label);
      const separator = value.previousElementSibling!;
      expect(separator.textContent?.trim()).toBe("·");
      expect(Math.abs(separator.getBoundingClientRect().top - value.getBoundingClientRect().top)).toBeLessThan(1);
    }
  }
});

it("keeps truncation notices visibly outside closed and natively opened disclosures", async () => {
  await page.viewport(375, 900);
  document.documentElement.setAttribute("data-theme", "dawn");
  render(<MemoryRouter><main className="space-y-4 p-4">
    <PlanCrossCheck run={{ ...mockRuns[0], plan_cross_check_summary: { ...plan,
      findings: { summary: "S".repeat(16385), items: plan.findings!.items } } }} />
    <CodeCrossCheck run={{ ...mockRuns[0], code_cross_check_summary: { ...code,
      findings: codeFindings.map((item, i) => i === 0 ? { ...item, detail: "D".repeat(2049) } : item) } }} />
  </main></MemoryRouter>);
  const regions = ["Plan cross-check", "Code cross-check"].map(name => screen.getByRole("region", { name }));
  for (const region of regions) {
    const notice = within(region).getByText("Findings truncated for display.");
    expect(notice.closest("details")).toBeNull();
    expect(region.querySelector("details")!.open).toBe(false);
    await expect.element(page.elementLocator(notice)).toBeVisible();
    await page.elementLocator(within(region).getByText("Show 2 more findings")).click();
    await waitFor(() => expect(within(region).getByText("Hide 2 more findings")).toBeTruthy());
    await expect.element(page.elementLocator(notice)).toBeVisible();
  }
  noOverflow(regions.map(region => region.firstElementChild!), 375);
});
