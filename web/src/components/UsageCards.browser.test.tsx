// Real layout and utility focus checks cannot be established in jsdom.
import "../index.css";
import { afterEach, expect, it } from "vitest";
import { page } from "vitest/browser";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { UsageCard, type UsageWindow } from "./UsageCards";
import type { AdminUsage, SelfUsage } from "../lib/api";
import { mockApi } from "../mocks/mockApi";

afterEach(cleanup);

function Harness({ self, admin }: { self: SelfUsage; admin: AdminUsage }) {
  const [window, onWindowChange] = useState<UsageWindow>("last_7_days");
  return <UsageCard self={self} admin={admin} window={window} onWindowChange={onWindowChange} />;
}

async function mount(older = false) {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  // Different optional rows exercise the subgrid instead of two identical columns.
  self.outcomes.last_7_days.needs_landing = 0;
  self.outcomes.last_7_days.fail_origins = {};
  const owner = admin.users.find((u) => u.user_id === admin.factory.outcomes.lifetime.last_failed_user_id)!;
  owner.email = "a.very.long.owner.name.that.must.truncate@example.com";
  admin.factory.outcomes.lifetime.last_failed_origin = "agent_failure";
  if (older) delete admin.users[0].last7_run_count;
  render(<div className="p-4"><MemoryRouter><Harness self={self} admin={admin} /></MemoryRouter></div>);
  return {
    personal: screen.getByRole("region", { name: "Your usage" }),
    factory: screen.getByRole("region", { name: "Factory usage" }),
    button: screen.getByRole("button", { name: "Last 7 days" }),
    owner: screen.getByTitle(owner.email),
    cause: screen.getByRole("region", { name: "Factory usage" }).querySelector('[title="agent failure"]') as HTMLElement,
  };
}

it("aligns all seven column rows at desktop width despite missing optional lines", async () => {
  await page.viewport(1000, 900);
  const { personal, factory, owner, cause } = await mount();
  expect(getComputedStyle(personal).gridTemplateRows).not.toBe("none");
  expect(personal.children).toHaveLength(7);
  expect(factory.children).toHaveLength(7);
  for (let i = 0; i < 7; i++) {
    expect(Math.abs(personal.children[i].getBoundingClientRect().top - factory.children[i].getBoundingClientRect().top)).toBeLessThan(1);
  }
  expect(owner.clientWidth).toBeLessThan(owner.scrollWidth);
  expect(cause.clientWidth).toBeGreaterThanOrEqual(cause.scrollWidth);
  const link = factory.querySelector('a[href^="/runs/"]') as HTMLElement;
  expect(link.clientWidth).toBeGreaterThanOrEqual(link.scrollWidth);
});

it("switches the per-user table and retains its scrollable nine-column layout", async () => {
  await page.viewport(375, 900);
  await mount();
  const users = within(screen.getByRole("region", { name: "Per-user usage, last 7 days" }));
  const table = users.getByRole("table");
  const recent = table.textContent;
  expect(users.getAllByRole("columnheader")).toHaveLength(9);
  const scroll = table.parentElement!;
  expect(getComputedStyle(scroll).overflowX).toBe("auto");
  expect(table.getBoundingClientRect().width).toBeGreaterThanOrEqual(680);
  expect(scroll.scrollWidth).toBeGreaterThan(scroll.clientWidth);
  const names = () => users.getAllByRole("row").slice(1, -1).map((row) => row.querySelector("td")!.textContent);
  const recentNames = names();
  fireEvent.click(screen.getByRole("button", { name: "All time" }));
  expect(screen.getByRole("heading", { name: "Per user · all time" })).toBeTruthy();
  expect(users.getByRole("table").textContent).not.toBe(recent);
  expect(names()).not.toEqual(recentNames);
  expect(users.getAllByRole("columnheader")).toHaveLength(9);
  fireEvent.click(screen.getByRole("button", { name: "Last 7 days" }));
  expect(users.getByRole("table").textContent).toBe(recent);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(375);
});

it("lets an older API use lifetime while seven-day rows remain unavailable", async () => {
  await mount(true);
  const users = within(screen.getByRole("region", { name: "Per-user usage, last 7 days" }));
  expect(users.getByText(/Seven-day per-user usage is unavailable/)).toBeTruthy();
  expect(users.queryByRole("table")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "All time" }));
  expect(users.getByRole("table")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Last 7 days" }));
  expect(users.queryByRole("table")).toBeNull();
  expect(users.getByText(/Upgrade the API/)).toBeTruthy();
});

it("stacks at 375px, uses 44px mobile targets and retains its own keyboard outline", async () => {
  await page.viewport(375, 900);
  const { personal, factory, button } = await mount();
  expect(factory.getBoundingClientRect().top).toBeGreaterThan(personal.getBoundingClientRect().bottom);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(375);
  expect(button.getBoundingClientRect().height).toBeGreaterThanOrEqual(44);
  // Disable the global base fallback: the component's utility must supply the ring.
  const reset = document.createElement("style");
  reset.textContent = "@layer base { button:focus-visible { outline: none; } }";
  document.head.append(reset);
  try {
    button.focus();
    expect(document.activeElement).toBe(button);
    expect(button.matches(":focus-visible")).toBe(true);
    const style = getComputedStyle(button);
    expect(style.outlineStyle).not.toBe("none");
    expect(style.outlineWidth).toBe("2px");
    expect(style.outlineOffset).toBe("2px");
  } finally { reset.remove(); }
});
