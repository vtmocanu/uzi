// Durable run recovery display logic (PRD #1296 M5, D6/D7). Pure functions that map
// the owner-scoped RecoveryArchiveSummary onto what the run page's "Recovery archives"
// section renders — kept out of the component so each rule is unit-testable and the two
// consumers (the panel and its tests) share one source of truth.
//
// NOTE the untrusted-text rule (D6): the summary's reason / source_sha /
// attempted_head_sha / prerequisite_shas are worker-authored and MUST be rendered as
// escaped plain text through stripUnsafeChars, never through <Markdown>. That escaping
// happens at the render site; nothing here relaxes it.

import type {
  RecoveryArchive,
  RecoveryArchiveSummary,
  RecoveryCustodyAggregate,
  RecoveryCustodyHold,
} from "./apiTypes";
import { isTerminalRun } from "./runStatus";
import type { BadgeTone } from "../components/ui";

// RecoverySectionKind is the top-level shape the section renders, derived from the
// aggregate summary so the section is truthful with ZERO captures. `null` means render
// nothing at all — an ordinary run with no recovery relevance.
export type RecoverySectionKind =
  // One or more retained capture rows to render (each with its own per-capture state).
  | "captures"
  // A custody hold is open but no capture row exists yet: the archive is still being
  // pinned/prepared at the protected boundary.
  | "preparing"
  // Recovery was never armed for this run (no custody hold ever existed) — the honest
  // legacy/unsupported case, surfaced only on a run that actually failed to finalize.
  | "unsupported"
  // Recovery was armed but produced no downloadable archive and holds nothing open
  // (custody released with no committed output, or the source became unavailable).
  | "unavailable";

// recoverySectionKind decides whether — and as what — the run page surfaces the
// recovery section. It is deliberately NOT gated on the failed-card conditional or on
// archives.length: it reads the aggregate summary plus the run's status.
//
//   - Any retained capture  → "captures" (shown in any run status, incl. a nonterminal
//     pre-partial capture, D8), so a real archive is never hidden.
//   - A terminal run with an open hold and no capture yet → "preparing".
//   - A run that FAILED to finalize with neither → "unsupported" (legacy: no hold ever)
//     or "unavailable" (supported, but nothing was captured).
//   - Anything else (a healthy/running/completed run whose claim-time hold is just a
//     reservation) → null: nothing to say, so nothing is rendered.
export function recoverySectionKind(
  summary: RecoveryArchiveSummary,
  runStatus: string,
): RecoverySectionKind | null {
  if (summary.archives.length > 0) return "captures";
  if (isTerminalRun(runStatus) && summary.has_open_hold) return "preparing";
  if (runStatus === "failed") return summary.supported ? "unavailable" : "unsupported";
  return null;
}

// CaptureView is the resolved per-capture display: its human label, badge tone, a
// one-line status description, and whether its bytes can be downloaded now. Download is
// enabled ONLY in the `available` state (D7: never render a usable download before ready).
export interface CaptureView {
  label: string;
  tone: BadgeTone;
  description: string;
  downloadable: boolean;
}

// captureView maps a capture's lifecycle state onto its display. An unknown state (a
// server that grew a new state before this client did) is treated as not-downloadable
// and labelled honestly rather than mis-rendered as available.
export function captureView(state: string): CaptureView {
  switch (state) {
    case "preparing":
      return {
        label: "Preparing",
        tone: "info",
        description: "Capturing the committed history at the protected boundary.",
        downloadable: false,
      };
    case "uploading":
      return {
        label: "Uploading",
        tone: "info",
        description: "Storing the encrypted archive. Download unlocks once it is complete.",
        downloadable: false,
      };
    case "available":
      return {
        label: "Available",
        tone: "ok",
        description: "The original committed history is ready to download.",
        downloadable: true,
      };
    case "needs_action":
      return {
        label: "Needs action",
        tone: "warning",
        description:
          "The archive could not be completed. Recovery is retained, but not yet downloadable.",
        downloadable: false,
      };
    case "expired":
      return {
        label: "Expired",
        tone: "neutral",
        description: "The download window has passed and the bytes were purged.",
        downloadable: false,
      };
    case "discarded":
      return {
        label: "Discarded",
        tone: "neutral",
        description: "This archive was explicitly discarded and its bytes were deleted.",
        downloadable: false,
      };
    default:
      return {
        label: state || "Unknown",
        tone: "neutral",
        description: "This archive is not available for download.",
        downloadable: false,
      };
  }
}

// formatArchiveSize renders a capture's byte_size for display. Absent (the manifest is
// not bound yet) renders an em dash. Binary units (KiB/MiB) match the PRD's size limits.
export function formatArchiveSize(bytes: number | null | undefined): string {
  if (bytes == null || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const kib = bytes / 1024;
  if (kib < 1024) return `${kib.toFixed(kib < 10 ? 1 : 0)} KiB`;
  const mib = kib / 1024;
  if (mib < 1024) return `${mib.toFixed(mib < 10 ? 1 : 0)} MiB`;
  const gib = mib / 1024;
  return `${gib.toFixed(gib < 10 ? 1 : 0)} GiB`;
}

// shortSha trims a full 40-hex SHA to its leading 12 for a compact display, leaving a
// shorter or non-hex value untouched. The caller still routes the result through
// stripUnsafeChars — this only shortens, it does not sanitize.
export function shortSha(sha: string): string {
  return sha.length > 12 ? sha.slice(0, 12) : sha;
}

// sortedArchives returns the run's captures newest-first (created_at desc), so the most
// recent recovery attempt leads. A stable copy — it never mutates the input array.
export function sortedArchives(summary: RecoveryArchiveSummary): RecoveryArchive[] {
  return [...summary.archives].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

// ── Owner custody surface (PRD #1349 M6, D6/D7/D8/D9) ─────────────────────────
// Pure decision logic for the board alert and the Workers per-hold resolution surface.
// The server derives each hold's `attention` (active | capturing | archive_ready |
// needs_action | source_only | released | discarded) and the owner-wide aggregate; this
// file only maps those onto presentation. It NEVER re-derives admission, severity, or the
// decision-needed count from raw hold rows — those are server-authoritative (D6/D10).

// CustodyAlertView is the board alert's resolved presentation, or `null` to render nothing.
export interface CustodyAlertView {
  // danger = the admission limit is reached and new claims are blocked (D8 escalation);
  // warning = attention is required but claims still flow.
  tone: "warning" | "danger";
  atLimit: boolean;
  // "6 / 8 custody slots used" — the safety-slot line.
  slotsLabel: string;
  slotsUsed: number;
  slotsLimit: number;
  decisionNeeded: number;
  blockedRuns: number;
  // recovery_wait_count is DERIVED CLIENT-SIDE by the caller (counting the owner's runs in
  // the recovery_wait status) and threaded in for diagnosis only — it never triggers the
  // alert on its own and carries no lifetime cap (PRD #1349 scope-out).
  recoveryWaitCount: number;
  headline: string;
}

// custodyAlertView decides whether the board alert renders and, if so, how loud it is.
//
// SELF-HIDES (returns null) unless attention is required OR claims are blocked (D8): there
// is at least one open hold AND (a hold needs an owner decision, a run is blocked, or the
// admission limit is reached). A fleet carrying only healthy active protection is NOT an
// incident (D6), so the alert stays hidden — it is not "another permanent alarm card".
//
// ESCALATES to `danger` styling the moment open_holds reaches custody_hold_limit: at the
// limit every further code-run claim for the owner stops, which is the incident this whole
// PRD exists to surface. Below the limit it is `warning`.
export function custodyAlertView(
  agg: RecoveryCustodyAggregate,
  recoveryWaitCount: number,
): CustodyAlertView | null {
  const atLimit =
    agg.custody_hold_limit > 0 && agg.open_holds >= agg.custody_hold_limit;
  const show =
    agg.open_holds > 0 &&
    (agg.decision_needed > 0 || agg.blocked_runs > 0 || atLimit);
  if (!show) return null;
  return {
    tone: atLimit ? "danger" : "warning",
    atLimit,
    slotsLabel: `${agg.open_holds} / ${agg.custody_hold_limit} custody slots used`,
    slotsUsed: agg.open_holds,
    slotsLimit: agg.custody_hold_limit,
    decisionNeeded: agg.decision_needed,
    blockedRuns: agg.blocked_runs,
    recoveryWaitCount,
    headline: atLimit
      ? "Held work is blocking new runs"
      : "Held work needs your attention",
  };
}

// The owner verbs the per-worker hold surface offers directly (D8). They never share
// ambiguous "Discard" copy:
//   - export  → export the recovery archive. On this surface it LINKS to the run's Recovery
//               archives section rather than minting a second download control, so the
//               D7 secret-review warning always sits above the actual download. The run view
//               also owns the third verb, "Delete archive" (artifact cleanup).
//   - discard → discard the exact custody hold / worker-local source (the strongest verb).
//               Actionable inline here because the hold discard keys on (run_id, hold_id),
//               both present on the hold DTO — this is the milestone's core new capability:
//               dispositioning a capture-less hold that no run-view archive action can reach.
export type CustodyHoldAction = "export" | "discard";

// CustodyGroup buckets a hold for presentation so healthy protection is visually separated
// from work that needs a decision (D6/D8). The order here is the display order.
export type CustodyGroup =
  | "attention" // needs an owner decision: needs_action | source_only (or unknown)
  | "archive_ready" // archive available, releasing automatically — Export only
  | "capturing" // capture in progress — pending, no action
  | "active" // healthy active protection — no action
  | "resolved"; // released / discarded — artifact context only

// CustodyHoldView is one hold's resolved presentation.
export interface CustodyHoldView {
  group: CustodyGroup;
  tone: BadgeTone;
  // The short badge label ("Active protection", "Decision required", …).
  stateLabel: string;
  // One plain-language line describing what this hold is and what will happen.
  summary: string;
  // archive_ready holds self-resolve through the reconciler (D8/D9): the row reads
  // "releasing automatically" and is NOT presented as a decision.
  autoReleasing: boolean;
  // Whether this hold counts as needing an owner decision (mirrors the server's aggregate
  // decision_needed classification, D10): pending/actionable + source-only, never healthy
  // active protection or archive-ready/release-pending.
  needsDecision: boolean;
  // Ordered action set for this hold. The strongest verb (discard) only appears where the
  // worker-local source may be the only copy (no available archive).
  actions: CustodyHoldAction[];
}

// custodyHoldView maps one hold onto its presentation, from the server-derived `attention`
// and `has_available_capture`. An UNKNOWN attention is treated as needing a decision
// (fail toward the owner seeing it), never silently hidden or auto-actioned.
export function custodyHoldView(hold: RecoveryCustodyHold): CustodyHoldView {
  const hasArchive = hold.has_available_capture;
  switch (hold.attention) {
    case "active":
      return {
        group: "active",
        tone: "brand",
        stateLabel: "Active protection",
        summary:
          "Protecting this run's committed work while it is in flight. No action needed.",
        autoReleasing: false,
        needsDecision: false,
        actions: [],
      };
    case "capturing":
      // Transient and automatic (like archive_ready): the owner takes no action, so it is
      // NOT counted as a decision — it self-resolves once the capture completes.
      return {
        group: "capturing",
        tone: "info",
        stateLabel: "Capturing",
        summary:
          "Archiving the committed history now. This resolves on its own once the archive is ready.",
        autoReleasing: false,
        needsDecision: false,
        actions: [],
      };
    case "archive_ready":
      return {
        group: "archive_ready",
        tone: "ok",
        stateLabel: "Archive ready",
        summary:
          "Recovery is available. Custody is releasing automatically — export the archive if you want a copy.",
        autoReleasing: true,
        needsDecision: false,
        actions: ["export"],
      };
    case "needs_action":
      return {
        group: "attention",
        tone: "warning",
        stateLabel: "Needs attention",
        summary:
          "The archive could not be completed, so the worker-local source may be the only copy of this work.",
        autoReleasing: false,
        needsDecision: true,
        actions: hasArchive ? ["export", "discard"] : ["discard"],
      };
    case "source_only":
      return {
        group: "attention",
        tone: "warning",
        stateLabel: "Decision required",
        summary:
          "No server archive exists. The worker-local source may be the only copy — export is not possible, so choose whether to discard it.",
        autoReleasing: false,
        needsDecision: true,
        actions: ["discard"],
      };
    case "released":
    case "discarded":
      return {
        group: "resolved",
        tone: "neutral",
        stateLabel: hold.attention === "released" ? "Released" : "Discarded",
        summary:
          hold.attention === "released"
            ? "Custody released. Any surviving archive stays exportable on the run."
            : "This held source was discarded. Its worker-local copy may be gone permanently.",
        autoReleasing: false,
        needsDecision: false,
        // Released custody no longer protects a last copy; a surviving archive is exportable
        // (and deletable) on the run view, which owns the D7 secret-review warning.
        actions: hasArchive ? ["export"] : [],
      };
    default:
      // A server that grew a new attention before this client did: treat as a decision
      // rather than mis-render it as safe.
      return {
        group: "attention",
        tone: "warning",
        stateLabel: hold.attention || "Unknown",
        summary:
          "This hold is retaining unpublished work. Review it before deciding what to do.",
        autoReleasing: false,
        needsDecision: true,
        actions: hasArchive ? ["export", "discard"] : ["discard"],
      };
  }
}

// A worker's holds, grouped for the per-worker resolution surface.
export interface WorkerHoldGroup {
  workerId: string;
  // The bounded, server-derived display name (D7) — may be absent for an older worker.
  workerName?: string;
  holds: RecoveryCustodyHold[];
  // How many of this worker's holds need an owner decision (drives the per-worker ordering
  // so the worker with the most to resolve leads).
  decisionCount: number;
}

const GROUP_ORDER: Record<CustodyGroup, number> = {
  attention: 0,
  archive_ready: 1,
  capturing: 2,
  active: 3,
  resolved: 4,
};

// groupHoldsByWorker folds a flat hold list into per-worker groups for the Workers surface
// (D8). Workers are ordered by how much needs resolving (most decisions first, then name);
// within a worker, holds are ordered by group priority then generation. Never mutates input.
export function groupHoldsByWorker(holds: RecoveryCustodyHold[]): WorkerHoldGroup[] {
  const byWorker = new Map<string, WorkerHoldGroup>();
  for (const hold of holds) {
    let g = byWorker.get(hold.worker_id);
    if (!g) {
      g = {
        workerId: hold.worker_id,
        workerName: hold.worker_name,
        holds: [],
        decisionCount: 0,
      };
      byWorker.set(hold.worker_id, g);
    }
    g.holds.push(hold);
    if (custodyHoldView(hold).needsDecision) g.decisionCount++;
  }
  const groups = [...byWorker.values()];
  for (const g of groups) {
    g.holds.sort((a, b) => {
      const ga = GROUP_ORDER[custodyHoldView(a).group];
      const gb = GROUP_ORDER[custodyHoldView(b).group];
      if (ga !== gb) return ga - gb;
      return a.generation - b.generation;
    });
  }
  groups.sort((a, b) => {
    if (a.decisionCount !== b.decisionCount) return b.decisionCount - a.decisionCount;
    return (a.workerName ?? a.workerId).localeCompare(b.workerName ?? b.workerId);
  });
  return groups;
}
