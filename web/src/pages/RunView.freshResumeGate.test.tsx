// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RecoveryWaitPanel } from "./RunView";
import type { Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";

vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, getFreshResumePreview: vi.fn(), startFreshResume: vi.fn() } };
});
afterEach(cleanup);

function held(over: Partial<Run>): Run {
  return { ...mockRuns[0], id: "held", kind: "issue", status: "recovery_wait",
    recovery_wait_cause: "worker_requeue_exhausted", recovery_wait_detail: null,
    recovery_retry_not_before: null, outcome_pending: undefined, ...over } as Run;
}
const offer = () => screen.queryByRole("button", { name: /Fresh resume on another worker/ });

it.each([
  ["worker_requeue_exhausted", {}],
  ["retained_recovery_exhausted", { recovery_wait_cause: "retained_recovery_exhausted" }],
  ["adoption failed", { recovery_wait_cause: "retained_recovery_adoption_failed" }],
] as const)("offers fresh resume on an eligible %s hold", (_n, over) => {
  render(<RecoveryWaitPanel run={held(over as Partial<Run>)} ownerProof />);
  expect(offer()).not.toBeNull();
});

it.each([
  ["secret remediation", { recovery_wait_cause: "retained_recovery_adoption_failed", recovery_wait_detail: "secret_remediation_active" }],
  ["codex_idle_stall", { recovery_wait_cause: "codex_idle_stall" }],
  ["source_end_declined", { recovery_wait_cause: "retained_recovery_adoption_failed", recovery_wait_detail: "source_end_declined" }],
  ["retry stamp", { recovery_wait_cause: "retained_recovery_exhausted", recovery_retry_not_before: "2099-01-01T00:00:00Z" }],
  ["claimed codex run", { harness: "codex", claim_generation: 1 }],
  ["codex policy hold", { hold_reason: "codex_policy_hold" }],
  ["reserve pause", { pause_mode: "reserve" }],
] as const)("hides fresh resume for %s", (_n, over) => {
  render(<RecoveryWaitPanel run={held(over as Partial<Run>)} ownerProof />);
  expect(offer()).toBeNull();
});

it("hides fresh resume without owner proof", () => {
  render(<RecoveryWaitPanel run={held({})} ownerProof={false} />);
  expect(offer()).toBeNull();
});
