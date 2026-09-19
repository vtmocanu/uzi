import {
  type AdminUsage,
  type AgentSelectionInput,
  type CredentialEpoch,
  type CredentialOverride,
  type RecoveryCustodyHold,
  type RecoveryCustodyHolds,
  type Run,
  type RunPriority,
  type RunInputKind,
  type SelfUsage,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { isCredentialSwitchRefusedLane } from "../../lib/credentialOverride";
import { isTerminalRun } from "../../lib/runStatus";
import {
  LIVE_RUN_ID,
  mockAdminRateLimits,
  mockMyRateLimitsByUser,
  mockMyTokenRateLimits,
  mockOtherRunOwners,
  mockRunInputs,
  mockSecrets,
  runListItem,
} from "../data";
import { ensureLive, handleInput, startNewRun } from "../engine";
import { getRun, nextRunId, patchRun, state } from "../store";
import { delay, mockScenario, requireSession } from "./shared";
import { LEAD_NAME_RE, templates } from "./agents";

function listRunsFor(): Run[] {
  return [...state.runs.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

// ── Custody recovery mock (PRD #1349 M6) ──────────────────────────────────────
// The `?mock=custody` scenario seeds a full spread of owner custody holds — every
// server-derived attention across several workers — so the board alert and the Workers
// resolution surface can be seen and driven offline. Any other scenario reports zero open
// holds, so the alert self-hides and the existing demos stay clean.
//
// Discards are persisted in module-level sets so a hold/archive stays gone across the 10s
// polls (the surface re-fetches, and a mutation that reappeared would read as a no-op).
const discardedHolds = new Set<string>();
const discardedCaptures = new Set<string>();

// Hostile worker name (a ZWSP, an RTL override and an HTML-injection payload, all as \u
// ESCAPES — never raw bytes, so this source file carries no invisible characters) so the
// surface's stripUnsafeChars + React escaping are visible offline. It renders as sanitized,
// contiguous text with no <script> element.
const HOSTILE_WORKER_NAME = "ci\u200brunner\u202e<script>alert(1)</script>";

function seedCustodyHolds(): RecoveryCustodyHold[] {
  const t = (minsAgo: number) => new Date(Date.now() - minsAgo * 60_000).toISOString();
  const mk = (o: Partial<RecoveryCustodyHold> & Pick<RecoveryCustodyHold, "id" | "run_id" | "worker_id" | "attention">): RecoveryCustodyHold => ({
    generation: 1,
    state: o.attention === "released" || o.attention === "discarded" ? "released" : "open",
    has_available_capture: false,
    created_at: t(180),
    updated_at: t(20),
    ...o,
  });
  return [
    // Worker A — base(M): the full lifecycle on one worker.
    mk({ id: "hold-a4", run_id: "run-a4", worker_id: "wkr-base-m", worker_name: "base (M)", generation: 4, attention: "active" }),
    mk({ id: "hold-a3", run_id: "run-a3", worker_id: "wkr-base-m", worker_name: "base (M)", generation: 3, attention: "capturing", capture_state: "uploading" }),
    mk({ id: "hold-a2", run_id: "run-a2", worker_id: "wkr-base-m", worker_name: "base (M)", generation: 2, attention: "archive_ready", has_available_capture: true, capture_state: "available" }),
    mk({ id: "hold-a1", run_id: "run-a1", worker_id: "wkr-base-m", worker_name: "base (M)", generation: 1, attention: "source_only" }),
    // Worker B — jvm-worker: an actionable capture failure + a source-only hold.
    mk({ id: "hold-b2", run_id: "run-b2", worker_id: "wkr-jvm", worker_name: "jvm-worker", generation: 2, attention: "needs_action", capture_state: "needs_action" }),
    mk({ id: "hold-b1", run_id: "run-b1", worker_id: "wkr-jvm", worker_name: "jvm-worker", generation: 1, attention: "source_only" }),
    // Worker C — hostile name: a source-only decision + healthy protection.
    mk({ id: "hold-c2", run_id: "run-c2", worker_id: "wkr-ext", worker_name: HOSTILE_WORKER_NAME, generation: 2, attention: "active" }),
    mk({ id: "hold-c1", run_id: "run-c1", worker_id: "wkr-ext", worker_name: HOSTILE_WORKER_NAME, generation: 1, attention: "source_only" }),
    // A resolved hold whose archive survives — released, exportable on the run.
    mk({ id: "hold-r1", run_id: "run-r1", worker_id: "wkr-base-m", worker_name: "base (M)", generation: 7, attention: "released", has_available_capture: true, capture_state: "available", released_at: t(5) }),
  ];
}

function currentCustodyHolds(): RecoveryCustodyHold[] {
  if (mockScenario() !== "custody") return [];
  return seedCustodyHolds().filter((h) => !discardedHolds.has(h.id));
}

function custodyResponse(): RecoveryCustodyHolds {
  const holds = currentCustodyHolds();
  const open = holds.filter((h) => h.state === "open");
  const decisionNeeded = open.filter(
    (h) => h.attention === "source_only" || h.attention === "needs_action",
  ).length;
  return {
    aggregate: {
      open_holds: open.length,
      custody_hold_limit: 8,
      decision_needed: decisionNeeded,
      // Two queued code runs are wedged behind the owner limit in this scenario.
      blocked_runs: open.length >= 8 ? 2 : 0,
    },
    holds,
  };
}

// ── Per-run credential override / switch demo (PRD #1247 M7) ──────────────────
// Four ?mock= scenarios overlay the credential fields onto ONE seeded run each, so the
// override / pending-switch / epoch-history surfaces AND the set-token warning are
// browsable offline; any other scenario leaves every run clean (the override badges
// self-hide on a null override). A run whose token was actually switched THIS session
// (setRunCredential below) is recorded here so the live mutation wins over the seed —
// which is what lets the "lingering-stamp" case prove no stale switch shows after apply.
const credentialSwitched = new Set<string>();

// One applied epoch, so the history list renders with a token label + reason + time.
const appliedEpochs = (): CredentialEpoch[] => [
  {
    claim_generation: 1,
    secret_id: "sec-default",
    label: "default",
    select_reason: "default",
    applied_at: new Date(Date.now() - 90 * 60_000).toISOString(),
  },
  {
    claim_generation: 2,
    secret_id: "sec-console-key",
    label: "console-key",
    select_reason: "override_pinned",
    applied_at: new Date(Date.now() - 20 * 60_000).toISOString(),
  },
];

function credentialOverlay(run: Run): Run {
  // A live set-token this session wins over the seeded scenario state (D-Step-A: the
  // server suppresses a stale/applied switch, so the DTO reads null once applied).
  if (credentialSwitched.has(run.id)) return run;
  switch (mockScenario()) {
    case "parked-with-alternative":
      // A limit_wait run whose `auto` override could move it to a fresh pooled token.
      if (run.id === "run-limit-wait")
        return {
          ...run,
          credential_override: { mode: "auto", label: null },
          credential_switch: null,
          credential_epochs: appliedEpochs().slice(0, 1),
        };
      return run;
    case "switch-on-running":
      // A running run with a switch REQUESTED but not yet applied — the pending badge.
      if (run.id === LIVE_RUN_ID)
        return {
          ...run,
          credential_override: { mode: "pinned", label: "console-key" },
          credential_switch: "requested",
          credential_epochs: appliedEpochs().slice(0, 1),
        };
      return run;
    case "released-awaiting-reclaim":
      // The held worker released its claim; the run awaits reclaim on the new token.
      if (run.id === LIVE_RUN_ID)
        return {
          ...run,
          credential_override: { mode: "pinned", label: "console-key" },
          credential_switch: "released",
          credential_epochs: appliedEpochs().slice(0, 1),
        };
      return run;
    case "warning":
      // The set-token path returns a D6 warning (below); seed an override so the surface
      // has something to show alongside it.
      if (run.id === LIVE_RUN_ID)
        return {
          ...run,
          credential_override: { mode: "auto", label: null },
          credential_switch: null,
          credential_epochs: appliedEpochs(),
        };
      return run;
    case "lingering-stamp":
      // The switch APPLIED: the override + epoch history stay, but credential_switch is
      // null — proving the UI shows NO stale pending badge once the DTO reports null.
      if (run.id === LIVE_RUN_ID)
        return {
          ...run,
          credential_override: { mode: "pinned", label: "console-key" },
          credential_switch: null,
          credential_epochs: appliedEpochs(),
        };
      return run;
    default:
      return run;
  }
}

// overrideToRead maps the set-token WRITE body ({mode, secret_id?}) to the READ-side
// {mode, label} the run DTO carries; inherit clears it to null. A pinned token's label
// is resolved from the seeded secrets (null when the id is unknown, mirroring a deleted
// token's snapshot).
function overrideToRead(body: { mode: string; secret_id?: string }): CredentialOverride | null {
  if (body.mode === "inherit") return null;
  if (body.mode === "pinned")
    return { mode: "pinned", label: mockSecrets.find((s) => s.id === body.secret_id)?.label ?? null };
  return { mode: body.mode, label: null };
}

export const runsApi = {
  // ── Runs ────────────────────────────────────────────────────────────────────
  createRun: async (
    repoId: string,
    issueIid: number,
    force?: boolean,
    // PRD #1247 M7: mirror the real client's optional override arg. The mock stamps the
    // read-side {mode,label} onto the created run so a demo start-with-token is visible.
    credentialOverride?: { mode: string; secret_id?: string },
  ) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === issueIid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.issue_iid === issueIid && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "a run is already in progress for this issue");
    // issue #856: a completed prior run that still owns an open MR refuses a fresh
    // run (coded issue_has_open_mr), unless the caller passes force to override.
    if (!force) {
      const openMR = [...state.runs.values()].find(
        (r) =>
          r.repo_id === repoId &&
          r.issue_iid === issueIid &&
          r.kind === "issue" &&
          r.status === "completed" &&
          r.mr_iid != null &&
          r.mr_state === "opened",
      );
      if (openMR) {
        throw new ApiError(
          409,
          `issue #${issueIid} already has open MR !${openMR.mr_iid} — merge or close it, or leave review comments on the MR to iterate, before starting a new run`,
          { code: "issue_has_open_mr", mr_iid: openMR.mr_iid },
        );
      }
    }
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "issue",
      issue_iid: issueIid,
      issue_title: card.title,
      issue_description: "See the linked PRD.",
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
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
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
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
      recovery_retry_not_before: null,
      forge_park_count: 0,
      forge_park_max: 0,
      // PRD #1247 M7: stamp the read-side override the caller chose (null = inherit).
      credential_override: credentialOverride ? overrideToRead(credentialOverride) : null,
      credential_switch: null,
      credential_epochs: [],
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  createCIFixRun: async (repoId: string, ref: string) => {
    if (!state.boards.get(repoId)) throw new ApiError(404, "repo not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.kind === "ci_fix" && r.pipeline_ref === ref && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "an active CI-fix run already exists for this ref");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "ci_fix",
      issue_iid: null,
      issue_title: `Fix CI: ${ref} pipeline`,
      issue_description: `Diagnose and fix the failed pipeline for \`${ref}\`.`,
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
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
      pipeline_ref: ref,
      pipeline_web_url: `https://gitlab.example.com/myorg/uzi/-/pipelines/4242`,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
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
      recovery_retry_not_before: null,
      forge_park_count: 0,
      forge_park_max: 0,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  listRuns: async (params?: {
    repoId?: string;
    issueIid?: number;
    // Mirrors the real client's passive-poll flag (#331); the mock does no real
    // fetch, so the marker has no effect here beyond keeping the types compatible.
    passive?: boolean;
  }) =>
    delay({
      runs: listRunsFor()
        // Chat conversations ride runs but have their own page (PRD #39), and judge
        // is a repo-less meta-run — both are excluded here exactly as the real
        // ListRunsForUser excludes them (`kind NOT IN ('chat','judge')`, PRD #239 D4).
        .filter((r) => r.kind !== "chat" && r.kind !== "judge")
        // Caller-scoped, like the real ListRunsForUser: other demo users' runs
        // (mockOtherRunOwners) belong to the admin all-users list only.
        .filter((r) => !(r.id in mockOtherRunOwners))
        .filter((r) => (params?.repoId ? r.repo_id === params.repoId : true))
        .filter((r) => (params?.issueIid != null ? r.issue_iid === params.issueIid : true))
        .map((r) => runListItem(r)),
    }),
  // PRD #40: token/cost usage. Static demo figures — enough to populate the
  // dashboard's "Your usage" and (admin) factory cards + per-user table.
  getUsage: async () =>
    delay<SelfUsage>({
      lifetime: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 },
      last_7_days: { input_tokens: 280_000, cache_read_tokens: 2_800_000, cache_creation_tokens: 40_000, output_tokens: 120_000, cost_usd: 4.55 },
      run_count: 23,
      // PRD #1293 failed-run outcomes. Internally consistent: finished === completed +
      // cancelled + plan_rejected + failed, and sum(fail_origins) === failed. finished is a
      // different population from run_count (D2) — infra failures spend no tokens, so it sits
      // a little above the 23 usage-bearing runs. Mirrors the u-vlad row below ("you").
      // needs_landing (issue #1418) is the sub-cut of failed whose committed work is
      // human-landable (here the workflow-scope + push-secret failures); needs_landing <= failed.
      outcomes: {
        lifetime: {
          finished: 30, completed: 22, cancelled: 2, plan_rejected: 1, failed: 5, needs_landing: 2,
          fail_origins: { agent_failure: 1, run_timeout: 1, workflow_scope_missing: 1, push_secret_blocked: 1, unknown: 1 },
        },
        last_7_days: {
          finished: 8, completed: 6, cancelled: 1, plan_rejected: 0, failed: 1, needs_landing: 0,
          fail_origins: { agent_failure: 1 },
        },
      },
    }),
  getAdminUsage: async () =>
    delay<AdminUsage>({
      factory: {
        lifetime: { input_tokens: 5_400_000, cache_read_tokens: 53_900_000, cache_creation_tokens: 900_000, output_tokens: 2_400_000, cost_usd: 88.15 },
        last_7_days: { input_tokens: 900_000, cache_read_tokens: 9_100_000, cache_creation_tokens: 120_000, output_tokens: 410_000, cost_usd: 14.9 },
        run_count: 79,
        // Factory lifetime outcomes are the sum of the four per-user rows below (finished 38 +
        // 30 + 21 + 8 = 97, failed 5 + 5 + 2 + 1 = 13, needs_landing 1 + 2 + 0 + 0 = 3); the
        // fail_origins likewise sum the per-user maps. last_7_days is a smaller window.
        outcomes: {
          lifetime: {
            finished: 97, completed: 77, cancelled: 5, plan_rejected: 2, failed: 13, needs_landing: 3,
            fail_origins: { agent_failure: 4, run_timeout: 3, worker_lost: 1, finalize_base_align_conflict: 1, workflow_scope_missing: 1, push_secret_blocked: 1, unknown: 2 },
          },
          last_7_days: {
            finished: 25, completed: 20, cancelled: 1, plan_rejected: 1, failed: 3, needs_landing: 0,
            fail_origins: { agent_failure: 2, run_timeout: 1 },
          },
        },
      },
      users: [
        { user_id: "u-maria", email: "maria@example.com", usage: { input_tokens: 2_490_000, cache_read_tokens: 22_400_000, cache_creation_tokens: 400_000, output_tokens: 1_020_000, cost_usd: 37.83 }, run_count: 31, outcomes: { finished: 38, completed: 30, cancelled: 2, plan_rejected: 1, failed: 5, needs_landing: 1, fail_origins: { agent_failure: 2, run_timeout: 1, finalize_base_align_conflict: 1, unknown: 1 } } },
        { user_id: "u-vlad", email: "vlad@example.com", usage: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 }, run_count: 23, outcomes: { finished: 30, completed: 22, cancelled: 2, plan_rejected: 1, failed: 5, needs_landing: 2, fail_origins: { agent_failure: 1, run_timeout: 1, workflow_scope_missing: 1, push_secret_blocked: 1, unknown: 1 } } },
        { user_id: "u-andrei", email: "andrei@example.com", usage: { input_tokens: 1_010_000, cache_read_tokens: 13_600_000, cache_creation_tokens: 210_000, output_tokens: 550_000, cost_usd: 19.71 }, run_count: 19, outcomes: { finished: 21, completed: 18, cancelled: 1, plan_rejected: 0, failed: 2, needs_landing: 0, fail_origins: { agent_failure: 1, worker_lost: 1 } } },
        { user_id: "u-dana", email: "dana@example.com", usage: { input_tokens: 290_000, cache_read_tokens: 3_500_000, cache_creation_tokens: 50_000, output_tokens: 120_000, cost_usd: 4.21 }, run_count: 6, outcomes: { finished: 8, completed: 7, cancelled: 0, plan_rejected: 0, failed: 1, needs_landing: 0, fail_origins: { run_timeout: 1 } } },
      ],
      earliest_run: "2026-05-12T09:00:00Z",
    }),
  // ── Claude rate limits (PRD #53) ───────────────────────────────────────────
  // The caller's own reading follows the persona (a demo login as a seeded
  // non-admin shows danger / unavailable / no-token); the admin table covers every
  // row state. Percentages only — no token material ever appears here.
  getMyRateLimits: async () => {
    const me = requireSession();
    return delay({ tokens: mockMyRateLimitsByUser[me.id] ?? mockMyTokenRateLimits }, 60);
  },
  getAdminRateLimits: async () => delay({ users: mockAdminRateLimits.map((u) => ({ ...u })) }, 60),
  getRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (id === LIVE_RUN_ID) ensureLive(id);
    // Mirror the server's run-detail read (PRD #37 M4-fix): own_agents is resolved
    // here from the owner's templates (lead stripped), so the plan gate's "My agent
    // templates" card has chips in mock mode without a separate fetch.
    const own_agents = templates
      .filter((t) => !LEAD_NAME_RE.test(t.name))
      .map((t) => ({ name: t.name, description: t.description }));
    // PRD #1247 M7: overlay the active credential-override demo scenario onto the run
    // (a no-op for the default/unknown scenario, and for a run switched this session).
    return delay({ run: { ...credentialOverlay(run), own_agents } }, 60);
  },
  // PRD #35: flip this run's usage-limit opt-in. Mirrors the server's guard — the
  // same NEGATIVE predicate the cancel path uses — so a terminal run is refused and
  // `limit_wait` is admitted for free.
  //
  // 🔴 IT MUST NOT TOUCH `status`. A parked run stays parked with its clock intact;
  // this changes what happens at the NEXT limit. A mock that helpfully un-parked the
  // run would teach the demo (and anyone testing against it) the one wrong thing
  // about this control.
  setRunWaitOnLimit: async (id: string, enabled: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (isTerminalRun(run.status)) throw new ApiError(409, "this run has already finished");
    patchRun(id, { wait_on_limit: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #1247 M7: switch (or set) a run's Anthropic credential. Mirrors the server's
  // contract closely enough to drive the demo and hold the UI to it: a missing run 404s;
  // a terminal run 409s; a REFUSED lane (task_review / chat / judge / self_improve) 409s
  // — the same lanes the UI hides the control for, so a stray call still fails. On success
  // it writes the read-side override, marks the switch applied (credential_switch null,
  // like Step A once applied), records the id so the seed no longer overlays it, and
  // returns the updated run plus an OPTIONAL D6 warning for the `warning` scenario and for
  // an `auto` choice with no eligible pooled token.
  setRunCredential: async (id: string, body: { mode: string; secret_id?: string }) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (isTerminalRun(run.status)) throw new ApiError(409, "run has already finished");
    if (isCredentialSwitchRefusedLane(run))
      throw new ApiError(409, "this run's lane does not support switching its Anthropic token");
    credentialSwitched.add(id);
    patchRun(id, {
      credential_override: overrideToRead(body),
      credential_switch: null,
      updated_at: new Date().toISOString(),
    });
    const updated = { ...getRun(id)! };
    const pooledEligible = mockSecrets.some((s) => s.kind === "anthropic_token" && s.auto_eligible);
    const warning =
      mockScenario() === "warning"
        ? "The chosen token was set, but a limit report suggests it may be throttled soon."
        : body.mode === "auto" && !pooledEligible
          ? "No token is currently eligible in your auto pool; the run will hold until one frees up."
          : "";
    return delay(warning ? { run: updated, warning } : { run: updated }, 120);
  },

  // PRD #841: set (or clear) a run's per-run MR-review-rework override. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and — unlike
  // setRunWaitOnLimit — NO terminal-status guard (D2), because the watcher acts after
  // the run completes, so the toggle stays live on a completed run whose MR is still
  // open. `null` clears back to inherit.
  setRunMrRework: async (id: string, enabled: boolean | null) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    patchRun(id, { mr_rework_enabled: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #1202: start an ON-DEMAND MR-review-rework run for a run's open MR. Mirrors the
  // server's shape: 404 on an unknown run (the demo caller owns every non-other-user run,
  // so ownership is not modelled separately). On success it fabricates a NEW `mr_rework`
  // run DERIVED from the source run — a fresh id, folding onto the source's existing
  // branch + MR (like the automatic watcher's run), issue-LESS (CreateAutoMRReworkRun
  // leaves issue_iid NULL), trigger_source "manual", queued — with the guidance folded
  // into issue_description. It carries none of the source's automatic-loop guard readings
  // (a fresh manual run has spent zero automatic cycles). Persisted to the store so the
  // fabricated run is retrievable, then echoed back under { run }.
  startRunRework: async (id: string, guidance?: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    const now = new Date().toISOString();
    const g = (guidance ?? "").trim();
    const newRun: Run = {
      ...run,
      id: nextRunId(),
      kind: "mr_rework",
      trigger_source: "manual",
      status: "queued",
      issue_iid: null,
      issue_web_url: null,
      issue_description: g ? `On-demand MR rework.\n\nGuidance:\n${g}` : "On-demand MR rework.",
      requeue_count: 0,
      iteration_count: 0,
      // A fresh manual rework has no automatic-loop guard readings of its own.
      mr_rework_auto_cycles: null,
      mr_rework_auto_cap: null,
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      stop_kind: null,
      stop_reason: null,
      failure_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      plan_md: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(newRun.id, newRun);
    return delay({ run: { ...newRun } }, 80);
  },

  // ── Owner completion decisions (PRD #1227 M4) ─────────────────────────────────
  // Demo-mode stubs for the three owner/admin decisions on a completion-BLOCKED run. These do
  // NOT replicate every server validation arm — just enough to satisfy the api key-set parity
  // guard and drive the demo: the demo caller owns every non-other-user run so a missing run is
  // the only 404; a 409 on a run that is not completion-blocked; and — for partial/accept — a
  // 409 when the fenced contract_revision no longer matches, plus a new revision N+1 with the
  // deferred/accepted history appended. `continue` just un-parks the hold; it DROPS the guidance
  // arg (the mock has nowhere to record it), which also keeps the method assignable to realApi's
  // wider type with no unused arg.
  continueCompletionDecision: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.completion_phase !== "blocked")
      throw new ApiError(409, "run is not completion-blocked");
    patchRun(id, {
      status: "queued",
      completion_phase: "reworking",
      hold_reason: null,
      hold_context: null,
      updated_at: new Date().toISOString(),
    });
    return delay({ run: { ...getRun(id)! } }, 80);
  },
  partialCompletionDecision: async (
    id: string,
    keep: string[],
    reason: string,
    contractRevision: number,
  ) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.completion_phase !== "blocked")
      throw new ApiError(409, "run is not completion-blocked");
    if ((run.completion_revision ?? 0) !== contractRevision)
      throw new ApiError(409, "the contract revision has changed; re-read and re-decide");
    const keepSet = new Set(keep);
    const alreadyDeferred = new Set((run.completion_deferred ?? []).map((d) => d.milestone_id));
    const newRev = (run.completion_revision ?? 0) + 1;
    const removed = (run.milestones ?? [])
      .filter((m) => !alreadyDeferred.has(m.id) && !keepSet.has(m.id))
      .map((m) => ({ milestone_id: m.id, reason, revision: newRev }));
    if (removed.length === 0)
      throw new ApiError(400, "a partial decision must remove at least one milestone");
    patchRun(id, {
      status: "queued",
      completion_phase: "reworking",
      hold_reason: null,
      hold_context: null,
      completion_revision: newRev,
      completion_deferred: [...(run.completion_deferred ?? []), ...removed],
      updated_at: new Date().toISOString(),
    });
    return delay({ run: { ...getRun(id)! } }, 80);
  },
  acceptCompletionDecision: async (
    id: string,
    criteria: string[],
    reason: string,
    contractRevision: number,
  ) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.completion_phase !== "blocked")
      throw new ApiError(409, "run is not completion-blocked");
    if ((run.completion_revision ?? 0) !== contractRevision)
      throw new ApiError(409, "the contract revision has changed; re-read and re-decide");
    const newRev = (run.completion_revision ?? 0) + 1;
    const wanted = new Set(criteria);
    const added = (run.milestones ?? [])
      .filter((m) => wanted.has(`${m.id}.c1`))
      .map((m) => ({
        id: `${m.id}.c1`,
        milestone_id: m.id,
        text: m.title,
        reason,
        revision: newRev,
      }));
    if (added.length === 0) throw new ApiError(400, "no known criterion to accept");
    patchRun(id, {
      status: "queued",
      completion_phase: "reworking",
      hold_reason: null,
      hold_context: null,
      completion_revision: newRev,
      completion_accepted: [...(run.completion_accepted ?? []), ...added],
      updated_at: new Date().toISOString(),
    });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // Issue #754: resume an auto-lane run parked at `pool_wait` right now. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and
  // pool_wait-ONLY — a 409 ("run is not waiting for a pooled token") on any other
  // status, including a run already resumed to `queued` (so a second click 409s, the
  // idempotent-ish contract the panel's inline "no longer waiting" note relies on).
  // On success the run moves to `queued`, which is what un-parks it.
  resumeRunNow: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "pool_wait")
      throw new ApiError(409, "run is not waiting for a pooled token");
    patchRun(id, { status: "queued", updated_at: new Date().toISOString() });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #1190: request a pause / withdraw one. Route through the engine's handleInput so
  // the mock enforces the SAME contract the web is built against (the 409s for a non-
  // running run and an unpausable kind, the boundary rule, the running → paused park).
  pauseRun: async (id: string, mode: "milestone" | "now") => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const rejection = handleInput(id, "pause", mode);
    if (rejection) throw new ApiError(rejection.status, rejection.message);
    return delay({ server_side: false }, 120);
  },
  cancelPause: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const rejection = handleInput(id, "pause_cancel", "");
    if (rejection) throw new ApiError(rejection.status, rejection.message);
    return delay({ server_side: false }, 120);
  },

  // PRD #1190 (D14): resume a `paused` run via the widened /resume-now endpoint. Mirrors
  // ResumePausedRun — paused → queued (the mock has no budget clock to bank, but the
  // transition is what un-parks it). Owner-scoped; a 409 naming the status on any run that
  // is neither paused nor pool_wait, so the panel's inline "no longer paused" note is
  // reachable when a second click races the refetch. Also accepts a pool_wait run, since
  // the real endpoint dispatches on status.
  resumeRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "paused" && run.status !== "pool_wait")
      throw new ApiError(409, `run is ${run.status}`);
    patchRun(id, { status: "queued", updated_at: new Date().toISOString() });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #320 M6: bump this run to the front of the queue, or clear that override.
  // Mirrors the server: owner-scoped (the demo caller owns every non-other-user run)
  // and QUEUED-ONLY (409 on a non-queued run, exactly like the real endpoint). Clearing
  // the override returns the run to its NATURAL class — "background" for the kinds that
  // demote (judge/self_improve), "normal" otherwise — since the mock has no live rank
  // machinery; the "restored" grace state is a seed, not something undo produces here.
  expediteRun: async (id: string, expedite: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "queued") throw new ApiError(409, "run is not queued");
    const natural: RunPriority =
      run.kind === "self_improve" || run.kind === "judge" ? "background" : "normal";
    patchRun(id, { priority: expedite ? "expedited" : natural });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  getRunMessages: async (id: string, afterSeq = 0) => {
    const log = state.messages.get(id);
    if (!log) throw new ApiError(404, "run not found");
    return delay({ messages: log.filter((m) => m.seq > afterSeq).map((m) => ({ ...m })) }, 60);
  },
  // PRD #95 steer queue (M2 seeds demo data across delivery states so M3's
  // SteerQueueCard renders every chip). A run with no sample inputs returns an empty
  // queue; a missing run 404s (which the card treats as "no queue", never an error).
  getRunInputs: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const inputs = (mockRunInputs[id] ?? []).map((i) => ({ ...i }));
    return delay({ inputs }, 60);
  },
  // PRD #1296 M5 (D6/D7): the owner-scoped recovery aggregate. Demo mode shows a
  // realistic AVAILABLE archive for a failed run so the "Recovery archives" section
  // renders its committed-history + secret-review copy and the enabled download; every
  // other run reports the honest supported-but-empty aggregate (which renders nothing
  // unless the run actually failed). A missing run 404s, which the section treats as
  // "no recovery data".
  getRunArchives: async (id: string) => {
    const r = getRun(id);
    if (!r) throw new ApiError(404, "run not found");
    const capId = `${id}-cap1`;
    if (r.status === "failed" && !discardedCaptures.has(capId)) {
      return delay(
        {
          supported: true,
          legacy: false,
          has_open_hold: true,
          counts: {
            preparing: 0,
            uploading: 0,
            available: 1,
            needs_action: 0,
            expired: 0,
            discarded: 0,
          },
          archives: [
            {
              id: capId,
              run_id: id,
              state: "available",
              source_sha: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
              attempted_head_sha: "f0e1d2c3b4a5968778695a4b3c2d1e0f01234567",
              byte_size: 4718592,
              checksum: "sha256:3f786850e387550fdab836ed7e6dc881de23001b",
              prerequisite_shas: ["9988776655443322110099887766554433221100"],
              created_at: new Date(Date.now() - 3_600_000).toISOString(),
              expires_at: new Date(Date.now() + 6 * 86_400_000).toISOString(),
            },
          ],
        },
        60,
      );
    }
    return delay(
      {
        supported: true,
        legacy: false,
        has_open_hold: false,
        counts: {
          preparing: 0,
          uploading: 0,
          available: 0,
          needs_action: 0,
          expired: 0,
          discarded: 0,
        },
        archives: [],
      },
      60,
    );
  },
  // PRD #1349 M6 (D7/D8): the owner-wide custody hold listing + aggregate the board alert and
  // the Workers resolution surface read. Rich under ?mock=custody; empty otherwise so the
  // alert self-hides and the other demos stay clean.
  getRecoveryHolds: async () => delay(custodyResponse(), 80),
  // PRD #1296 / #1349 M6 (D7/D9): delete one recovery ARCHIVE artifact. Artifact cleanup
  // only — it does NOT disposition the parent hold. Persisted so the capture stays gone on
  // the run view's reload.
  discardRunArchive: async (runId: string, captureId: string) => {
    if (!getRun(runId)) throw new ApiError(404, "run not found");
    discardedCaptures.add(captureId);
    return delay({ discarded: true }, 80);
  },
  // PRD #1349 M5/M6 (D7/D9): discard one exact custody hold — the possible-only-copy source
  // disposition. Removes it from the owner listing so the surface reflects the discard on the
  // next poll. Mirrors the server: an unknown hold in the custody scenario 404s.
  discardHold: async (runId: string, holdId: string) => {
    const exists = currentCustodyHolds().some((h) => h.id === holdId && h.run_id === runId);
    if (!exists) throw new ApiError(404, "hold not found");
    discardedHolds.add(holdId);
    return delay({ discarded: true }, 80);
  },
  submitRunInput: async (
    id: string,
    kind: RunInputKind,
    body = "",
    selection?: AgentSelectionInput,
    overrideCapabilities?: boolean,
  ) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    // PRD #88: the engine returns the refusals the real api answers with (a 409 for an
    // answer to a question that has moved on, a 400 for a malformed body) rather than
    // resolving 200 over a no-op. A mock that swallows a refusal is how a surface ends up
    // built against a laxer contract than the one that ships.
    const rejection = handleInput(id, kind, body);
    if (rejection) throw new ApiError(rejection.status, rejection.message);
    // PRD #37: mirror the selection onto the run row so the mock's read-only
    // post-approval view has something to show.
    if (kind === "approve_plan" && selection) {
      patchRun(id, { agent_source: selection.source, agent_exclusions: selection.exclusions });
    }
    // PRD #84 M4 4c/4d: the "run without the capability" override clears the run's inferred
    // required_capabilities before approving, mirroring the server (the false-positive
    // correction). required_tools/size_class are display-only and untouched.
    if (kind === "approve_plan" && overrideCapabilities) {
      patchRun(id, { required_capabilities: [] });
    }
    return delay({ server_side: false }, 150);
  },

  adminListRuns: async () =>
    delay({
      runs: listRunsFor()
        .filter((r) => r.kind !== "chat")
        .filter((r) => !["completed", "failed", "cancelled"].includes(r.status))
        // Owner attribution: the mock's owner column is mockOtherRunOwners; every
        // other run belongs to the session admin. Before this map existed, EVERY
        // row here was stamped with the session email — the demo factory list was
        // 100% "mine", the exact duplication amendment 2 removes.
        .map((r) => runListItem(r, mockOtherRunOwners[r.id] ?? requireSession().email)),
    }),
};
