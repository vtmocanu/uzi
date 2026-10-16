import "../../index.css";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { PlanPanel } from "./PlanPanel";
import { mockRuns } from "../../mocks/data/runs";

vi.mock("../../lib/api", async original => {
  const actual = await original<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    setRunCredential: vi.fn() } };
});
afterEach(cleanup);

it.each([375, 1100])("keeps reset confirmation readable and operable at %s px", async width => {
  await page.viewport(width, 900);
  const run = { ...mockRuns[0], status: "awaiting_approval" as const, gate_revision: 4,
    reset_state_digest: "digest", completion_revision: null, plan_md: "A revised plan",
    milestones: [{ id: "m1", title: "Original definition with progress already recorded" }],
    milestones_candidate: [], milestones_completed: ["m1"], milestones_in_progress: [], milestones_agents: [],
    repo_agents: [], own_agents: [], required_capabilities: [], required_tools: [],
    completion_accepted: [], completion_deferred: [],
  };
  render(<main className="min-w-0 p-4"><PlanPanel run={run} busy={false} onApprove={vi.fn()}
    onReject={vi.fn()} onResetApprove={vi.fn().mockResolvedValue(true)} /></main>);
  await userEvent.click(screen.getByRole("button", { name: /^Approve plan/ }));
  const region = screen.getByRole("region", { name: "Confirm milestone reset" });
  expect(within(region).getByText(/Explicit empty replacement/)).toBeTruthy();
  expect(region.scrollWidth).toBeLessThanOrEqual(region.clientWidth);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  expect(getComputedStyle(region).fontSize).toBe("16px");
  const reason = within(region).getByLabelText("Reason for reset");
  await userEvent.click(reason);
  expect(document.activeElement).toBe(reason);
  await userEvent.type(reason, "Replace the scope deliberately");
  await userEvent.click(within(region).getByRole("checkbox"));
  const confirm = within(region).getByRole("button", { name: "Reset progress and approve plan" });
  expect((confirm as HTMLButtonElement).disabled).toBe(false);
  expect(confirm.getBoundingClientRect().width).toBeLessThanOrEqual(region.clientWidth);
  await userEvent.click(within(region).getByRole("button", { name: "Cancel reset" }));
  expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
});
