// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { CheckerActivitySummary } from "./CheckerActivitySummary";
import { RunEventRow } from "./RunEvent";
import { mockRuns } from "../mocks/data/runs";
import type { RunMessage } from "../lib/apiTypes";

afterEach(cleanup);
const msg = (seq: number, payload: unknown): RunMessage => ({ seq, kind: "status", agent: "cross-checker", agent_instance: null, agent_label: null, payload, created_at: "2026-07-04T00:00:00.000Z" });
const counts = { ok: 4, denied: 0, oversize: 0, limit: 0, error: 0 };
const zero = { ok: 0, denied: 0, oversize: 0, limit: 0, error: 0 };
const messages = [msg(1, { event: "check_activity_start", v: 1, cap: 200, generation: 1 }),
  msg(2, { event: "check_activity_summary", generation: 1, reads: counts, searches: zero, dropped: 0, complete: true })];
const checker = { ...mockRuns[0], kind: "cross_check" as const, status: "completed" as const, claim_generation: 1, first_started_at: "2026-01-01T00:00:00Z" };

it("renders the capture state for a cross_check run", () => {
  const r = render(<CheckerActivitySummary run={checker} messages={messages} />);
  expect(r.getByTestId("checker-activity-summary").textContent).toContain("4 reads, 0 searches, 0 dropped, complete");
});
it("renders nothing for a non-cross_check run", () => {
  const r = render(<CheckerActivitySummary run={{ ...checker, kind: "issue" }} messages={messages} />);
  expect(r.container.textContent).toBe("");
});
it("renders activity status rows as inert text", () => {
  const hostile = "<img src=x onerror=x>\u202E\u001b\nevil.go";
  const r = render(<RunEventRow msg={msg(3, { event: "check_activity", op: "read", outcome: "ok", path: hostile })} live={false} />);
  expect(r.container.querySelector("img")).toBeNull();
  expect(r.container.textContent).toContain("read <img src=x onerror=x>evil.go · ok");
  expect(r.container.textContent).not.toContain("\u202E");
  expect(r.container.textContent).not.toContain("\u001b");
});
it("keeps the generic fallback for an unknown status event", () => {
  const r = render(<RunEventRow msg={msg(4, { event: "check_activity_v2" })} live={false} />);
  expect(r.container.textContent).toContain("status: check_activity_v2");
});
