import { describe, expect, it } from "vitest";
import { FRESH_RESUME_CUTOFF_TEXT, freshResumeEligible } from "./freshResumeText";

const base = { status: "recovery_wait", kind: "issue", recovery_wait_cause: "worker_requeue_exhausted", recovery_wait_detail: null, recovery_retry_not_before: null };

describe("freshResumeEligible", () => {
  it.each([
    "worker_requeue_exhausted", "retained_recovery_exhausted", "retained_recovery_adoption_failed", "retained_continuation_unwitnessed",
  ])("accepts %s", (cause) => {
    expect(freshResumeEligible({ ...base, recovery_wait_cause: cause })).toBe(true);
  });

  it.each([
    ["codex_idle_stall", { recovery_wait_cause: "codex_idle_stall" }],
    ["worker_memory_pressure", { recovery_wait_cause: "worker_memory_pressure" }],
    ["secret remediation", { recovery_wait_cause: "retained_recovery_adoption_failed", recovery_wait_detail: "secret_remediation_active" }],
    ["source end declined", { recovery_wait_cause: "retained_recovery_adoption_failed", recovery_wait_detail: "source_end_declined" }],
    ["retry stamp", { recovery_retry_not_before: "2030-01-01T00:00:00Z" }],
    ["task kind", { kind: "task" }],
    ["not recovery_wait", { status: "paused" }],
    ["no cause", { recovery_wait_cause: null }],
    ["claimed codex run", { harness: "codex", claim_generation: 1 }],
    ["codex policy hold", { hold_reason: "codex_policy_hold" }],
    ["reserve pause", { pause_mode: "reserve" }],
  ])("refuses %s", (_name, over) => {
    expect(freshResumeEligible({ ...base, ...over })).toBe(false);
  });

  it("accepts an unclaimed codex run", () => {
    expect(freshResumeEligible({ ...base, harness: "codex", claim_generation: 0 })).toBe(true);
  });

  it("keeps the cutoff copy the Go clients carry", () => {
    expect(FRESH_RESUME_CUTOFF_TEXT[3]).toContain("until its new hold is released or the run ends");
    expect(FRESH_RESUME_CUTOFF_TEXT[4]).toContain("an owner with only two workers whose new worker is lost");
  });
});
