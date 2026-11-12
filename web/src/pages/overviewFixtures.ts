// Shared fixtures for the Overview page tests (Dashboard.overviewData.test.tsx and
// OverviewNext.test.tsx). Mirrors the wire shapes Dashboard.test.tsx builds locally.

import { vi } from "vitest";
import type { useAuth } from "../auth/AuthContext";
import type { RunListItem, Worker } from "../lib/api";

export const aUser = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  ephemeral_docker_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default" as const,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

export function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    harness_decision: null,
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    kind: "issue",
    issue_iid: 7,
    issue_title: "Live run row",
    issue_description: "",
    harness: "claude", // PRD #1429 M1: harness joined RunDTO (NOT NULL, default claude).
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    plan_cross_check_required: false,
    worker_id: "w1",
    branch: null,
    model: null,
    override_subagent_model: false,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    recovery_wait_cause: null,
    codex_account_action: null,
    codex_secret_id: null,
    codex_secret_label: null,
    codex_select_reason: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    provider_park_count: 0,
    provider_park_streak_started_at: null,
    can_stop_at_provider_hold: false,
    forge_park_max: 0,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "vtmocanu/uzi",
    worker_name: "laptop",
    // PRD #98 M4: unjudged by default, so existing assertions are unchanged.
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  };
}

export function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    hibernation_state: "none",
    hibernation_reason: null,
    hibernated_at: null,
    wake_requested_at: null,
    wake_error: null,
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    active_cross_checks: 0,
    max_cross_check_slots: null,
    template_declared: null,
    template_reported: null,
    version: null,
    upgrade_status: "unknown",
    upgrade_detail: null,
    upgrade_target: "",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: null,
    created_at: "2026-07-05T12:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    stats_disk_dind_bytes: null,
    stats_disk_dind_total_bytes: null,
    stats_disk_dind_inodes: null,
    stats_disk_dind_total_inodes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    ...over,
  };
}

const zeros = () => ({ input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 0, cost_status: "" as const });
// PRD #1293: zero outcomes keep the failed-runs block hidden (finished===0), so these
// usage fixtures leave the existing "nothing yet" / card assertions unchanged.
const zeroOutcomes = () => ({ finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, base_diverged: 0, failed: 0, needs_landing: 0, last_failed_at: null, last_failed_run_id: null, last_failed_origin: null, last_failed_user_id: null, completed_since_last_failure: null, fail_origins: {} });
const zeroOutcomeWindows = () => ({ lifetime: zeroOutcomes(), last_7_days: zeroOutcomes() });
export function emptySelf() {
  // PRD #1429 M1 (D7): the per-window subscription/unreported run counts (0 for empty usage).
  return {
    lifetime: zeros(),
    last_7_days: zeros(),
    run_count: 0,
    outcomes: zeroOutcomeWindows(),
    lifetime_subscription_run_count: 0,
    lifetime_unreported_run_count: 0,
    last7_subscription_run_count: 0,
    last7_unreported_run_count: 0,
  };
}

// The useAuth() value for a signed-in user; tests pass it to vi.mocked(useAuth).mockReturnValue.
export function authValue(user: typeof aUser): ReturnType<typeof useAuth> {
  return {
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance: {
      mode: "dark",
      light_theme: "hall",
      dark_theme: "ember",
      typeface: "system",
      overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
      defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
    },
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
    updateUser: vi.fn(),
    serverUnreachable: false,
    retry: vi.fn(),
  };
}
