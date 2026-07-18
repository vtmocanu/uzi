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

// ONE merged map across BOTH forges (PRD #65 D2/R5): the pipeline badge is
// forge-BLIND — it is never keyed off forge_type. The collision claim holds because
// every string the two forges share (`success`, `running`, `skipped`, `pending`)
// means the same thing, so one map is sound. Two Forgejo-only traps this map exists
// to close (R5 — either one renders a red build benign otherwise):
//   - Forgejo spells it `cancelled` (two Ls); GitLab `canceled` (one). BOTH keys are
//     present, or a Forgejo-cancelled build falls through `?? "neutral"` by accident
//     (harmless here, but it must be deliberate, not luck).
//   - Forgejo Actions reports a failure as `failure`, not GitLab's `failed`; a
//     commit-status failure as `error`. WITHOUT these keys a failed Forgejo build
//     has no `failed` entry and renders neutral/benign — the whole point of R5.
// Forgejo has two status enums (the PRD's "two enums, not one"): Actions run status
// (unknown|waiting|running|success|failure|cancelled|skipped|blocked) and
// CommitStatusState (pending|success|error|failure|warning|skipped). Both are folded
// in below so uzi never mis-reads whichever the driver surfaces.
const PIPELINE_TONES: Record<string, PipelineTone> = {
  // shared, same meaning on both forges
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
  // Forgejo Actions run status
  failure: "failed", // R5: must map to failed, or a red build renders benign
  cancelled: "neutral", // R5: two-L spelling, distinct from GitLab's `canceled`
  waiting: "running",
  blocked: "running",
  unknown: "neutral",
  // Forgejo CommitStatusState extras
  error: "failed", // an errored status is a failure, never benign
  warning: "attention",
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
