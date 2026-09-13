// Durable run recovery display logic (PRD #1296 M5, D6/D7). Pure functions that map
// the owner-scoped RecoveryArchiveSummary onto what the run page's "Recovery archives"
// section renders — kept out of the component so each rule is unit-testable and the two
// consumers (the panel and its tests) share one source of truth.
//
// NOTE the untrusted-text rule (D6): the summary's reason / source_sha /
// attempted_head_sha / prerequisite_shas are worker-authored and MUST be rendered as
// escaped plain text through stripUnsafeChars, never through <Markdown>. That escaping
// happens at the render site; nothing here relaxes it.

import type { RecoveryArchive, RecoveryArchiveSummary } from "./apiTypes";
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
