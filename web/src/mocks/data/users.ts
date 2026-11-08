import type {
  User,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";

// ── Users ────────────────────────────────────────────────────────────────────

export const mockAdmin: User = {
  id: "u-admin",
  email: "vlad@uzi.local",
  display_name: "Robin Diaz",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  plan_cross_check_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  ephemeral_docker_enabled: false,
  wait_on_limit: true,
  notify_early_limit_reset: true,
  // PRD #3522: weekly usage reserve off; pause-running at its server default.
  weekly_reserve_pct: null,
  weekly_reserve_pause_running: true,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "auto",
  created_at: daysAgo(41),
  last_login: minsAgo(7),
};

export const mockUsers: User[] = [
  mockAdmin,
  {
    id: "u-mira",
    email: "mira@uzi.local",
    display_name: "Alex Rivera",
    is_admin: false,
    is_active: true,
    autopilot_enabled: true,
    plan_cross_check_enabled: true,
    judge_enabled: true,
    ci_autofix_enabled: false,
    attribution_enabled: true,
    ephemeral_workers_enabled: false,
    ephemeral_docker_enabled: false,
    // PRD #35: the demo's one opted-IN user, and it is the autopilot user on purpose.
    // An autopilot run has no start affordance at all, so this default is the ONLY
    // way its opt-in can ever be expressed — pairing the two is what makes the
    // setting's reason for existing visible in the fixture rather than only in prose.
    wait_on_limit: true,
    notify_early_limit_reset: true,
    weekly_reserve_pct: null,
    weekly_reserve_pause_running: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "auto",
    created_at: daysAgo(33),
    last_login: minsAgo(95),
  },
  {
    id: "u-andrei",
    email: "andrei@uzi.local",
    display_name: "Sam Chen",
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
    weekly_reserve_pct: null,
    weekly_reserve_pause_running: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "auto",
    created_at: daysAgo(20),
    last_login: daysAgo(1),
  },
  {
    id: "u-dan",
    email: "dan@uzi.local",
    display_name: null,
    is_admin: false,
    is_active: false,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    attribution_enabled: true,
    ephemeral_workers_enabled: false,
    ephemeral_docker_enabled: false,
    wait_on_limit: false,
    notify_early_limit_reset: false,
    weekly_reserve_pct: null,
    weekly_reserve_pause_running: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "auto",
    created_at: daysAgo(18),
    last_login: daysAgo(12),
  },
  // radu + mihai exist so a demo login reaches the WARN and STALE own-reading
  // states of the rate-limit meters (PRD #53); see mockMyRateLimitsByUser.
  {
    id: "u-radu",
    email: "radu@uzi.local",
    display_name: "Jordan Kim",
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
    weekly_reserve_pct: null,
    weekly_reserve_pause_running: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "auto",
    created_at: daysAgo(15),
    last_login: minsAgo(20),
  },
  {
    id: "u-mihai",
    email: "mihai@uzi.local",
    display_name: "Priya Nair",
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
    weekly_reserve_pct: null,
    weekly_reserve_pause_running: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    judge_anthropic_bind_mode: "auto",
    created_at: daysAgo(25),
    last_login: daysAgo(1),
  },
];

// ?mock=overview-next (issue #4042): user rows for the scenario's other-run owners
// (overviewNextOtherRunOwners) plus two people with no runs, so the People chip's popover
// shows display names for every active person, a signed-in-within-24-hours row and an idle
// line with a never-signed-in count. Appended to the roster only under that scenario (see
// mockApi/shared.ts), so every other mock test and scenario keeps the roster above.
function scenarioUser(id: string, email: string, displayName: string, lastLogin: string | null, createdDays: number): User {
  return {
    ...mockUsers[1],
    id,
    email,
    display_name: displayName,
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    plan_cross_check_enabled: false,
    judge_enabled: false,
    created_at: daysAgo(createdDays),
    last_login: lastLogin,
  };
}

export const overviewNextUsers: User[] = [
  scenarioUser("u-ov-priya", "priya@uzi.local", "Priya Desai", minsAgo(50), 30),
  scenarioUser("u-ov-sam", "sam@uzi.local", "Sam Okafor", minsAgo(14), 28),
  scenarioUser("u-ov-jordan", "jordan@uzi.local", "Jordan Lee", minsAgo(180), 22),
  scenarioUser("u-ov-lena", "lena@uzi.local", "Lena Fischer", minsAgo(8), 16),
  scenarioUser("u-ov-omar", "omar@uzi.local", "Omar Haddad", daysAgo(2), 12),
  scenarioUser("u-ov-ines", "ines@uzi.local", "Ines Duarte", minsAgo(4), 9),
  // No runs: one signed in two hours ago, one invited but never signed in, one last seen
  // nine days ago (the idle line then reads a range with Priya Nair's one day).
  scenarioUser("u-ov-elif", "elif@uzi.local", "Elif Kaya", minsAgo(120), 11),
  scenarioUser("u-ov-mateo", "mateo@uzi.local", "Mateo Silva", null, 3),
  scenarioUser("u-ov-hana", "hana@uzi.local", "Hana Ito", daysAgo(9), 40),
];
