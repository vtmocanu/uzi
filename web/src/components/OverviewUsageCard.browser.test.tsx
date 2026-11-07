// Real layout cannot be established in jsdom: the by-user table must scroll inside the card.
import "../index.css";
import { afterEach, expect, it } from "vitest";
import { page } from "vitest/browser";
import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewUsageCard } from "./OverviewUsageCard";
import type { UsageWindow } from "./UsageCards";
import type { AdminUsage, SelfUsage } from "../lib/api";
import { mockApi } from "../mocks/mockApi";

afterEach(cleanup);

function Harness({ self, admin }: { self: SelfUsage; admin: AdminUsage }) {
  const [window, onWindowChange] = useState<UsageWindow>("lifetime");
  return <OverviewUsageCard self={self} admin={admin} window={window} onWindowChange={onWindowChange} viewerId={admin.users[0].user_id} />;
}

it("scrolls the by-user table inside its wrapper at 400px without overflowing the page", async () => {
  await page.viewport(400, 900);
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  // Short emails and no cost notes keep the content narrower than the 640px floor, so only
  // the table's min-width can make it that wide.
  admin.users.forEach((u, i) => {
    u.email = `u${i}@x.io`;
    u.subscription_run_count = 0; u.unreported_run_count = 0;
    u.last7_subscription_run_count = 0; u.last7_unreported_run_count = 0;
  });
  admin.factory.lifetime_subscription_run_count = 0; admin.factory.lifetime_unreported_run_count = 0;
  render(<div className="p-4"><MemoryRouter><Harness self={self} admin={admin} /></MemoryRouter></div>);
  fireEvent.click(screen.getByRole("button", { name: "Factory · all users" }));
  const scroll = screen.getByTestId("by-user-scroll");
  expect(getComputedStyle(scroll).overflowX).toBe("auto");
  expect(scroll.querySelector("table")!.getBoundingClientRect().width).toBeCloseTo(640, 0);
  expect(scroll.scrollWidth).toBeGreaterThan(scroll.clientWidth);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(document.documentElement.clientWidth);
});
