// Real layout cannot be established in jsdom: the by-user table must scroll inside the card at
// phone widths, and fit the card without scrolling at a 1280px desktop viewport (issue #4227).
import "../index.css";
import { afterEach, expect, it } from "vitest";
import { page } from "vitest/browser";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewUsageCard } from "./OverviewUsageCard";
import type { UsageWindow } from "../lib/usageByUser";
import type { AdminUsage, SelfUsage } from "../lib/api";
import { mockApi } from "../mocks/mockApi";
import { OUTCOME_SCOPE_NOTE, USAGE_SCOPE_NOTE } from "./usageParts";

afterEach(cleanup);

function Harness({ self, admin }: { self: SelfUsage; admin: AdminUsage }) {
  const [window, onWindowChange] = useState<UsageWindow>("lifetime");
  return <OverviewUsageCard self={self} admin={admin} window={window} onWindowChange={onWindowChange} viewerId={admin.users[0].user_id} />;
}

async function mount(width: number, wrapperWidth?: number) {
  await page.viewport(width, 900);
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  // Cost notes on the biggest user and the Factory row, and the two figures the old column clipped.
  admin.users[0].subscription_run_count = 270; admin.users[0].unreported_run_count = 46;
  admin.factory.lifetime_subscription_run_count = 270; admin.factory.lifetime_unreported_run_count = 48;
  admin.users.forEach((u) => { u.harness_runs = { claude: 20, codex: 3 }; });
  admin.users[0].harness_runs = { claude: 61, codex: 39 };
  admin.users[1].harness_runs = { claude: 100, codex: 0 };
  const card = <MemoryRouter><Harness self={self} admin={admin} /></MemoryRouter>;
  render(wrapperWidth ? <div style={{ width: wrapperWidth }}>{card}</div> : <div className="p-4">{card}</div>);
  fireEvent.click(screen.getByRole("button", { name: "All users" }));
  return screen.getByTestId("by-user-scroll");
}

it("scrolls the by-user table inside its wrapper at 375px without overflowing the page", async () => {
  const scroll = await mount(375);
  expect(getComputedStyle(scroll).overflowX).toBe("auto");
  expect(scroll.scrollWidth).toBeGreaterThan(scroll.clientWidth);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(document.documentElement.clientWidth);
});

// The card's outer width in the real app at a 1280px viewport: 1280 minus the sidebar (AppShell.tsx,
// `w-60` = 240px), the main padding (`sm:px-6` on both sides = 48px) and a ~15px scrollbar = 977px.
// Its inner width, minus ui.tsx Card's `p-5` (40px) and 1px border on each side (2px), is the
// 935px the table has to fit.
it("fits the whole by-user table in the card at a 1280px viewport, with every cell's content inside its cell", async () => {
  const scroll = await mount(1280, 1280 - 240 - 48 - 15);
  expect(scroll.clientWidth).toBeCloseTo(935, 0);
  // (a) no horizontal scrolling at this width.
  expect(scroll.scrollWidth).toBeLessThanOrEqual(scroll.clientWidth);

  const region = scroll.getBoundingClientRect();
  const header = screen.getByRole("columnheader", { name: "Cost share · Claude / Codex" });
  const figures = [screen.getByText("61 / 39"), screen.getByText("100 / 0")];
  // (b) nothing is clipped: each element's content fits itself, the scroll region's visible area
  // and its own cell.
  for (const el of [header, ...figures]) {
    const r = el.getBoundingClientRect();
    const cell = el.closest("td, th")!.getBoundingClientRect();
    expect(el.scrollWidth, el.textContent!).toBeLessThanOrEqual(el.clientWidth);
    expect(r.left, el.textContent!).toBeGreaterThanOrEqual(region.left);
    expect(r.right, el.textContent!).toBeLessThanOrEqual(region.right);
    expect(r.left, el.textContent!).toBeGreaterThanOrEqual(cell.left);
    expect(r.right, el.textContent!).toBeLessThanOrEqual(cell.right);
  }

  // (c) the header text starts where the bar cells' content starts. Measure the rendered header
  // text, not the cell box: a header and its cells share one column box whatever their text-align,
  // so a box measurement would pass with the header right-aligned again.
  const textRange = document.createRange();
  textRange.selectNodeContents(header);
  const headerTextLeft = textRange.getBoundingClientRect().left;
  const barCells = screen.getAllByRole("row").slice(1).map((r) => r.lastElementChild!);
  expect(barCells.length).toBeGreaterThan(2);
  for (const cell of barCells) {
    const contentLeft = cell.firstElementChild!.firstElementChild!.getBoundingClientRect().left;
    expect(Math.abs(contentLeft - headerTextLeft)).toBeLessThanOrEqual(1);
  }
});

// The two notes live in info tips: opened by keyboard focus, each must read as sentence text
// (the Usage title is uppercase and tracked), wrap, and stay fully visible on screen.
async function openTip(name: string) {
  const trigger = screen.getByRole("button", { name });
  trigger.focus();
  const tip = document.getElementById(trigger.getAttribute("aria-describedby")!)!;
  await expect.poll(() => getComputedStyle(tip).opacity).toBe("1");
  return tip;
}

// Inside the viewport, wrapping rather than overflowing, and on top at points just inside its rounded corners (Chromium hit-tests the radius): a clip by an
// ancestor scroller or a cover by a sibling would put another element there.
function expectFullyVisible(tip: HTMLElement) {
  const t = tip.getBoundingClientRect();
  expect(t.width).toBeGreaterThan(0);
  expect(t.left).toBeGreaterThanOrEqual(0);
  expect(t.top).toBeGreaterThanOrEqual(0);
  expect(t.right).toBeLessThanOrEqual(document.documentElement.clientWidth);
  expect(t.bottom).toBeLessThanOrEqual(window.innerHeight);
  expect(tip.scrollWidth).toBeLessThanOrEqual(tip.clientWidth);
  for (const [x, y] of [[t.left + 10, t.top + 10], [t.right - 10, t.top + 10], [t.left + 10, t.bottom - 10], [t.right - 10, t.bottom - 10]]) {
    const hit = document.elementFromPoint(x, y);
    expect(tip.contains(hit), `${x},${y} ${hit?.outerHTML.slice(0, 160)} tip=${JSON.stringify(t)}`).toBe(true);
  }
}

function expectSentenceText(tip: HTMLElement) {
  const style = getComputedStyle(tip);
  expect(style.textTransform).toBe("none");
  expect(style.letterSpacing).toBe("normal");
}

it.each([1280, 400])("opens the scope note on focus, in sentence case and inside its card at %ipx", async (width) => {
  await mount(width);
  const tip = await openTip("About usage figures");
  expect(tip.textContent).toBe(USAGE_SCOPE_NOTE);
  expectSentenceText(tip);
  expectFullyVisible(tip);
});

// Issue #4577: the outcomes caption sits in the outcomes block and must also stay inside its card.
it.each([1280, 400])("opens the outcome scope note on focus, in sentence case and inside its card at %ipx", async (width) => {
  await mount(width);
  const tip = await openTip("About run outcomes");
  expect(tip.textContent).toBe(OUTCOME_SCOPE_NOTE);
  expectSentenceText(tip);
  expectFullyVisible(tip);
});

// The fail rate's denominator is in a tip on the rate. The table's horizontal scroller also clips
// vertically, so the Factory footer's tip (last row) must escape it and stay fully visible, in its
// bold row without inheriting the weight.
it.each([1280, 375])("opens the Factory fail rate's tip inside the table's scroller at %ipx", async (width) => {
  const scroll = await mount(width, width === 1280 ? 1280 - 240 - 48 - 15 : undefined);
  const rows = screen.getAllByRole("row");
  const factory = rows[rows.length - 1];
  const rate = within(factory).getByRole("button", { name: /%$/ });
  rate.focus();
  const tip = document.getElementById(rate.getAttribute("aria-describedby")!)!;
  await expect.poll(() => getComputedStyle(tip).opacity).toBe("1");
  expect(tip.textContent).toMatch(/^[\d,]+ failed of [\d,]+ finished runs$/);
  expect(getComputedStyle(tip).fontWeight).toBe("400");
  expect(scroll.contains(tip)).toBe(true);
  expectFullyVisible(tip);
});
