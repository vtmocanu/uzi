// Pure, framework-free pipeline-badge logic (PRD #6): the taxonomy that collapses
// each forge's full pipeline-status set to the handful of tones the UI renders,
// plus the display label. Kept out of the components so the mapping is unit-tested
// in isolation (pipelineBadge.test.ts), the same split runBadge.ts uses.

import type { BadgeTone } from "./runBadge";

// PipelineTone collapses a forge's pipeline status set to five UI tones:
//  - passed    → success
//  - failed    → a genuine build failure
//  - running   → anything still in flight
//  - attention → a human must act for it to proceed (GitLab manual)
//  - neutral   → cancelled / skipped, and any status we do not recognize (defensive)
export type PipelineTone = "passed" | "failed" | "running" | "attention" | "neutral";

// ONE merged map across ALL THREE forges (PRD #65 D2/R5, #238 D8): the pipeline
// badge is forge-BLIND — it is never keyed off forge_type. The collision claim holds
// because every string more than one forge shares means the same thing, so one map
// is sound. The audit, shown not asserted (#238 N10):
//   - `success`/`failure`/`cancelled`/`skipped` — GitHub Actions spells these exactly
//     as Forgejo Actions (two-L `cancelled`, GitHub matches Forgejo not GitLab), same
//     meaning, so the existing Forgejo keys already cover GitHub. ✓
//   - `pending` — GitHub's commit-status API uses it for in-flight, same as the
//     existing →running entry. ✓
//   - `waiting` is the ONE genuine cross-forge collision (#238 D8): Forgejo Actions
//     `waiting` means in-flight (→running), while GitHub's run `waiting` means blocked
//     on a deployment-protection gate — a HUMAN must approve (nearer `action_required`
//     →attention). The map is forge-blind and the driver stores the raw string, so the
//     two cannot be told apart here. KNOWINGLY kept →running: the tone is a nuance, not
//     a failure/pass, so no fix run is mis-triggered and no red build renders benign —
//     the properties TestMirrorsWebPipelineBadge actually guards. Escape hatch if this
//     confuses in practice: the GitHub driver translates its own `waiting` before
//     storing (not the default).
// Two Forgejo-only traps this map exists to close (R5 — either one renders a red build
// benign otherwise):
//   - Forgejo spells it `cancelled` (two Ls); GitLab `canceled` (one). BOTH keys are
//     present, or a Forgejo/GitHub-cancelled build falls through `?? "neutral"` by
//     accident (harmless here, but it must be deliberate, not luck).
//   - Forgejo Actions reports a failure as `failure`, not GitLab's `failed`; a
//     commit-status failure as `error`. WITHOUT these keys a failed Forgejo build
//     has no `failed` entry and renders neutral/benign — the whole point of R5.
// Two GitHub-only traps (#238 D8): GitHub says `in_progress`, not `running`; `queued`,
// not `pending`. Both must be present or an in-flight GitHub build renders neutral.
// Forgejo has two status enums (the PRD's "two enums, not one"): Actions run status
// (unknown|waiting|running|success|failure|cancelled|skipped|blocked) and
// CommitStatusState (pending|success|error|failure|warning|skipped). GitHub Actions
// folds two SEQUENTIAL fields into one stored string (#238 D8): a run/job `status`
// (queued|in_progress|requested|waiting|pending) until completion, then a `conclusion`
// (success|failure|cancelled|skipped|timed_out|action_required|neutral|stale|
// startup_failure). All are folded in below so uzi never mis-reads whichever the
// driver surfaces.
const PIPELINE_TONES: Record<string, PipelineTone> = {
  // shared, same meaning on both/all forges
  success: "passed",
  running: "running",
  pending: "running",
  skipped: "neutral",
  // GitLab
  failed: "failed",
  created: "running",
  waiting_for_resource: "running",
  preparing: "running",
  scheduled: "running",
  manual: "attention",
  canceled: "neutral",
  // Forgejo Actions run status (also covers GitHub's shared strings — see audit above)
  failure: "failed", // R5: must map to failed, or a red build renders benign
  cancelled: "neutral", // R5: two-L spelling (Forgejo AND GitHub), distinct from GitLab's `canceled`
  waiting: "running", // #238 D8: GitHub deployment-gate `waiting` is knowingly shown running, not attention
  blocked: "running",
  unknown: "neutral",
  // Forgejo CommitStatusState extras
  error: "failed", // an errored status is a failure, never benign
  warning: "attention",
  // GitHub Actions run/job status (in-flight — traps: `in_progress` not `running`,
  // `queued` not `pending`)
  queued: "running",
  in_progress: "running",
  requested: "running",
  // GitHub Actions conclusion (terminal)
  timed_out: "failed",
  startup_failure: "failed",
  action_required: "attention", // a human must approve a gate/first-run — attention, not a code failure
  neutral: "neutral",
  stale: "neutral",
};

// pipelineTone maps a raw forge pipeline status to its UI tone. An unknown status
// is neutral, never a crash — a forge could add a status uzi has not seen.
export function pipelineTone(status: string): PipelineTone {
  return PIPELINE_TONES[status] ?? "neutral";
}

// TONE_TO_BADGE bridges the pipeline tone to the shared ui.tsx Badge tone (PRD #6
// "reuses Badge tones") plus whether the badge pulses (a still-running pipeline is
// the only live one).
const TONE_TO_BADGE: Record<PipelineTone, { tone: BadgeTone; pulse: boolean }> = {
  passed: { tone: "ok", pulse: false },
  failed: { tone: "danger", pulse: false },
  running: { tone: "info", pulse: true },
  attention: { tone: "warning", pulse: false },
  neutral: { tone: "neutral", pulse: false },
};

export interface PipelineBadgeView {
  label: string;
  tone: BadgeTone;
  pulse: boolean;
}

// pipelineBadge maps a raw pipeline status to its badge view: the shared Badge
// tone, whether it pulses, and a human label. The label is the humanized raw
// status (so "skipped" reads "skipped", not a lossy tone name) except GitLab's
// "success", which reads the friendlier "passed" the rest of the product uses.
export function pipelineBadge(status: string): PipelineBadgeView {
  const { tone, pulse } = TONE_TO_BADGE[pipelineTone(status)];
  const label = status === "success" ? "passed" : status.replace(/_/g, " ");
  return { label, tone, pulse };
}

// pipelineTitle is the badge's hover tooltip: the exact forge status plus how
// stale the cached value is (the sync is poll-based, so a badge can lag up to a
// poll interval). nowMs is passed in (not read from Date.now) for deterministic
// tests.
export function pipelineTitle(status: string, syncedAt: string, nowMs: number): string {
  return `Pipeline ${status} · synced ${syncedAgo(syncedAt, nowMs)}`;
}

function syncedAgo(syncedAt: string, nowMs: number): string {
  const ms = nowMs - Date.parse(syncedAt);
  if (!Number.isFinite(ms)) return "just now";
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
