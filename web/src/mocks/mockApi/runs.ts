import {
  type AgentSelectionInput,
  type Run,
  type RunPriority,
  type RunInputKind,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { isTerminalRun } from "../../lib/runStatus";
import {
  LIVE_RUN_ID,
  mockAdminRateLimits,
  mockMyRateLimitsByUser,
  mockMyTokenRateLimits,
  mockOtherRunOwners,
  mockRunInputs,
  runListItem,
} from "../data";
import { ensureLive, handleInput, startNewRun } from "../engine";
import { getRun, nextRunId, patchRun, state } from "../store";
import { delay, requireSession } from "./shared";
import { LEAD_NAME_RE, templates } from "./agents";

function listRunsFor(): Run[] {
  return [...state.runs.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

export const runsApi = {
  // ── Runs ────────────────────────────────────────────────────────────────────
  createRun: async (repoId: string, issueIid: number, force?: boolean) => {
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
    delay({
      lifetime: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 },
      last_7_days: { input_tokens: 280_000, cache_read_tokens: 2_800_000, cache_creation_tokens: 40_000, output_tokens: 120_000, cost_usd: 4.55 },
      run_count: 23,
    }),
  getAdminUsage: async () =>
    delay({
      factory: {
        lifetime: { input_tokens: 5_400_000, cache_read_tokens: 53_900_000, cache_creation_tokens: 900_000, output_tokens: 2_400_000, cost_usd: 88.15 },
        last_7_days: { input_tokens: 900_000, cache_read_tokens: 9_100_000, cache_creation_tokens: 120_000, output_tokens: 410_000, cost_usd: 14.9 },
        run_count: 79,
      },
      users: [
        { user_id: "u-maria", email: "maria@example.com", usage: { input_tokens: 2_490_000, cache_read_tokens: 22_400_000, cache_creation_tokens: 400_000, output_tokens: 1_020_000, cost_usd: 37.83 }, run_count: 31 },
        { user_id: "u-vlad", email: "vlad@example.com", usage: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 }, run_count: 23 },
        { user_id: "u-andrei", email: "andrei@example.com", usage: { input_tokens: 1_010_000, cache_read_tokens: 13_600_000, cache_creation_tokens: 210_000, output_tokens: 550_000, cost_usd: 19.71 }, run_count: 19 },
        { user_id: "u-dana", email: "dana@example.com", usage: { input_tokens: 290_000, cache_read_tokens: 3_500_000, cache_creation_tokens: 50_000, output_tokens: 120_000, cost_usd: 4.21 }, run_count: 6 },
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
    return delay({ run: { ...run, own_agents } }, 60);
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
