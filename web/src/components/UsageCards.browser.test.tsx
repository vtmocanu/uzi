// Real layout and utility focus checks cannot be established in jsdom.
import "../index.css";
import { afterEach, expect, it } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { UsageCard } from "./UsageCards";
import { mockApi } from "../mocks/mockApi";

afterEach(cleanup);

async function mount() {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  // Different optional rows exercise the subgrid instead of two identical columns.
  self.outcomes.last_7_days.needs_landing = 0;
  self.outcomes.last_7_days.fail_origins = {};
  const owner = admin.users.find((u) => u.user_id === admin.factory.outcomes.lifetime.last_failed_user_id)!;
  owner.email = "a.very.long.owner.name.that.must.truncate@example.com";
  admin.factory.outcomes.lifetime.last_failed_origin = "agent_failure";
  render(<div className="p-4"><MemoryRouter><UsageCard self={self} admin={admin} window="last_7_days" onWindowChange={() => {}} /></MemoryRouter></div>);
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
