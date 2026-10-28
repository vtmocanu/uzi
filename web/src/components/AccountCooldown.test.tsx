// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Run } from "../lib/apiTypes";
import { accountCooldownText, healthBadge, runBadge } from "../lib/runBadge";
import { RunHealthBadge } from "./RunHealthBadge";
import { RunProgressCell } from "./RunProgressCell";
import { RunProgressCard } from "./RunProgressCard";
import { HealthFlag } from "../pages/RunView";

const now = Date.parse("2026-10-28T12:00:00Z");
const at = "2026-10-28T12:01:37.037Z";
vi.mock("../lib/useNow", () => ({ useNow: () => Date.parse("2026-10-28T12:00:00Z") }));
vi.mock("../lib/useMaintenance", () => ({ useMaintenance: () => ({ status: null }) }));
afterEach(cleanup);

const waiting = (over: Partial<Run> = {}) => ({
  id: "account-owner-run", status: "running", health: "waiting_account_cooldown",
  health_reason: "waiting for account cooldown: stale overload detail", health_since: "2026-10-28T11:00:00Z",
  account_cooldown_class: "rate_limit", account_cooldown_eligible_at: at,
  progress: { state: "waiting", pct: null, milestone_done: 1, milestone_total: 2, active_milestone_id: "m2", phase: "" },
  milestones: [{ id: "m1", title: "API" }, { id: "m2", title: "Web" }], milestones_completed: ["m1"],
  milestones_in_progress: ["m2"], retry_not_before: null, recovery_retry_not_before: null,
  ...over,
} as Run);

it.each(["claimed", "running", "awaiting_approval"] as const)("%s renders typed owner cooldown calmly on detail and shared badges", status => {
  const r = waiting({ status });
  const text = `waiting for account cooldown · rate_limit · earliest eligibility ${at}`;
  expect(accountCooldownText(r, now)).toBe(text);
  expect(healthBadge(r, now)).toMatchObject({ label: text, title: text, tone: "info", pulse: false });
  expect(runBadge(r as never, now)).toMatchObject({ label: text });
  const { container } = render(<><HealthFlag run={r} /><RunHealthBadge run={r} nowMs={now} /></>);
  expect(screen.getAllByText(text).filter(el => !el.classList.contains("sr-only"))).toHaveLength(2);
  expect(container.textContent).not.toMatch(/⚠|stuck for|provider reported/);
  expect(container.querySelector(".animate-pulse")).toBeNull();
  expect(r.id).toBe("account-owner-run");
});

it.each([
  { account_cooldown_class: undefined }, { account_cooldown_class: "unknown" },
  { account_cooldown_eligible_at: undefined }, { account_cooldown_eligible_at: "2026-02-30T12:01:37Z" },
  { account_cooldown_eligible_at: "2026-10-28T12:00:00Z" }, { account_cooldown_eligible_at: "not a date" },
  { account_cooldown_eligible_at: "2026-10-28" }, { account_cooldown_eligible_at: {} },
  { status: "queued" }, { status: "completed" }, { status: "paused" }, { health: "ok" },
] as Partial<Run>[])("unknown, expired, malformed and stale metadata establishes no wait: %j", over => {
  const r = waiting(over);
  expect(accountCooldownText(r, now)).toBeNull();
  const { container } = render(<><HealthFlag run={r} /><RunHealthBadge run={r} nowMs={now} /></>);
  expect(container.textContent).not.toMatch(/waiting for account cooldown|⚠|stuck|provider reported/);
});

it.each(["stalled", "looping", "slow"] as const)("typed advice preserves independent %s health", health => {
  const r = waiting({ health, health_reason: "independent fault", deadline_at: at });
  expect(accountCooldownText(r, now)).toBeNull();
  expect(healthBadge(r, now)).toMatchObject({ tone: "warning", pulse: true });
  const { container } = render(<HealthFlag run={r} />);
  expect(container.textContent).toContain("⚠");
  expect(container.textContent).not.toContain("waiting for account cooldown");
});

it("leaves claimed fault-health gating unchanged and strips stale account reasons on other health", () => {
  expect(healthBadge(waiting({ status: "claimed", health: "stalled" }), now)).toBeNull();
  const badge = healthBadge(waiting({ health: "stalled" }), now);
  expect(badge).toMatchObject({ tone: "warning", title: undefined });
});

it("progress leaves a single visible wait to health and retains milestone counts without live activity", () => {
  const r = waiting({ milestones_live: [{ milestone_id: "m2", lanes: [{ agent: "coder", agent_instance: "lane-1", agent_label: "Old activity", tool: "Bash", detail: "old gate", at: "2026-10-28T11:59:00Z" }] }] });
  const { container } = render(<MemoryRouter><RunHealthBadge run={r} nowMs={now} /><RunProgressCell run={r} now={now} /><RunProgressCard run={r} activity={{ agent: "coder", agent_label: "Old activity", tool: "Bash", detail: "old gate", at: "2026-10-28T11:59:00Z", seq: 1 }} /></MemoryRouter>);
  const visible = [...container.querySelectorAll("*")].filter(el => el.childElementCount === 0 && !el.classList.contains("sr-only") && !el.closest(".sr-only"));
  expect(visible.filter(el => el.textContent?.includes("waiting for account cooldown"))).toHaveLength(1);
  expect(container.textContent).toContain("1 of 2 reported complete");
  expect(container.textContent).not.toMatch(/waits on you|Old activity|old gate|%/);
  expect(container.querySelector(".animate-pulse")).toBeNull();
});

it("foreign progress with redacted fields makes no false cooldown or human-action claim", () => {
  const r = waiting({ account_cooldown_class: undefined, account_cooldown_eligible_at: undefined, health_reason: null });
  const { container } = render(<MemoryRouter><RunHealthBadge run={r} nowMs={now} /><RunProgressCell run={r} now={now} /><RunProgressCard run={r} /></MemoryRouter>);
  expect(container.textContent).toContain("1 of 2 reported complete");
  expect(container.textContent).not.toMatch(/waits on you|waiting for account cooldown|⚠|%/);
});
