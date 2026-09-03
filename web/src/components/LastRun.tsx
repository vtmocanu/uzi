// LastRun — the shared "Last run" UI for a schedule row (PRD #308 M4, mock §1/§2).
//
// Extracted verbatim from Schedules.tsx so it can be reused without a circular import
// (Schedules.tsx imports DefaultJobs and ScheduleGroupRow, so re-exporting the "last run"
// pieces from Schedules.tsx would cycle). The public surface is LastRunOutcome (the list
// cell), LastFireDetail (the expandable panel), and formatStamp (the single source of
// truth for the schedules' timestamp formatting).

import { Link } from "react-router-dom";
import { Badge, type BadgeTone, cx } from "./ui";
import { ChevronDownIcon } from "./icons";
import { ForgeIssueAnchor } from "./ForgeIssueAnchor";
import { scheduleSkipReasonLabel } from "../lib/scheduleSkipReasons";
import type { LastFire, LastFireSkip, Schedule, ScheduleSkipReason } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Skip-reason badge tone, mirroring the mock's semantics: the actionable skips a
// schedule owner can fix (not eligible, a transient fetch failure) read amber; the
// benign, self-resolving ones (already running, body too large) read neutral.
// Exhaustive so a new union member is a tsc error, not a default.
const SKIP_REASON_TONES: Record<ScheduleSkipReason, BadgeTone> = {
  not_eligible: "warning",
  already_running: "neutral",
  description_too_large: "neutral",
  fetch_failed: "warning",
  // A locked owner vault (self_improve, PRD #590) is benign and self-resolving: the
  // cycle re-fires on schedule once the vault is unlocked, so it reads neutral like the
  // other self-resolving skips.
  vault_locked: "neutral",
  // The concurrent-open-MR cap (self_improve, PRD #686 D10) is benign and self-resolving:
  // the cycle re-fires next cadence once a human merges or closes an outstanding MR.
  self_improve_mr_cap_reached: "neutral",
  // issue #856: a prior completed run's MR is still open, so a fresh run is refused. Benign
  // and self-resolving like already_running — the cadence re-fires once the MR merges/closes.
  open_mr_exists: "neutral",
  // PRD #1093: the owner's user-level "pause all schedules" switch was on when this fire
  // came due. Benign and self-resolving — the cadence advanced as normal and re-fires on
  // resume, nothing replays (the explanatory detail-row copy is added in M4).
  schedules_paused: "neutral",
};

// LastRunOutcome is the enriched "Last run" cell for a schedule that has a
// persisted fire (PRD #308 M4, mock §1): an outcome badge, a muted "{stamp} ·
// examined N" line, and a disclosure that toggles the "Last fire" detail row.
export function LastRunOutcome({
  fire,
  expanded,
  onToggle,
  panelId,
}: {
  fire: LastFire;
  expanded: boolean;
  onToggle: () => void;
  // The detail row's element id, for the disclosure's aria-controls.
  panelId: string;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="inline-flex">
        <OutcomeBadge fire={fire} />
      </span>
      <span className="text-[11px] text-faint tabular-nums">
        {formatStamp(fire.fired_at)} · examined {fire.matched}
      </span>
      {/* A DISCLOSURE, not a link (ux-tweaks item 2): it expands the detail row in
          place, so it must not wear the app's link costume (text-info + underline
          promises navigation). Muted text + a chevron is the vocabulary every other
          expander here uses. The chevron is the SVG icon, not the ▾ font glyph — the
          glyph's metrics drift per font and its rotated open state rendered as thin
          stray lines; the SVG is viewBox-centred and crisp in both themes (the same
          trade ChevronsRightIcon records in icons.tsx). */}
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        // Only reference the panel while it is mounted: the caller renders the detail
        // (and its id={panelId} element) only when expanded, so a collapsed disclosure
        // must not carry a dangling aria-controls to a non-existent id (issue #690 CR).
        aria-controls={expanded ? panelId : undefined}
        className="inline-flex w-fit items-center gap-1 rounded text-[11px] font-medium text-muted transition-colors hover:text-fg"
      >
        Last fire
        <ChevronDownIcon className={cx("h-3 w-3 transition-transform", expanded && "rotate-180")} />
      </button>
    </div>
  );
}

// OutcomeBadge is the one-line verdict shown in the list cell: green when a fire
// started runs, amber when it fired but started nothing (skips only), neutral for
// an empty-label sweep that matched nothing (a legitimate outcome, not an error).
function OutcomeBadge({ fire }: { fire: LastFire }) {
  if (fire.started.length > 0) {
    return (
      <Badge tone="ok" dot>
        {fire.started.length} started
      </Badge>
    );
  }
  if (fire.skips.length > 0) {
    return (
      <Badge tone="warning" dot>
        0 started · {fire.skips.length} skipped
      </Badge>
    );
  }
  return (
    <Badge tone="neutral" dot>
      matched 0
    </Badge>
  );
}

// LastFireDetail is the expandable "Last fire" panel (mock §2): a header with the
// fire timestamp + a status badge, an examined/started/skipped/max-issues tally, one
// row per started run (linking to the run) and per skipped candidate (with its
// typed reason), and the actionable cap hint when a capped fire started nothing.
export function LastFireDetail({ s, fire }: { s: Schedule; fire: LastFire }) {
  const { uziLabel } = useAuth();
  const good = fire.started.length > 0;
  const skippedOnly = fire.started.length === 0 && fire.skips.length > 0;
  // The hint that is the whole point of Goal 2: a capped fire that reached only the
  // oldest candidate(s) and started nothing — raising the cap or labelling the head
  // candidate is the fix. Rendered ONLY under exactly that condition.
  const showHint = fire.capped && fire.skips.length > 0 && fire.started.length === 0;
  return (
    <div
      className={cx(
        "rounded-lg border border-edge bg-surface p-4",
        good ? "border-l-2 border-l-ok/60" : "border-l-2 border-l-warn/60",
      )}
    >
      <div className="mb-3 flex flex-wrap items-baseline gap-2.5">
        <span className="text-[13px] font-semibold text-fg">Last fire</span>
        <span className="font-mono text-[12px] text-faint">
          {formatStamp(fire.fired_at)} · {s.timezone}
        </span>
        <span className="ml-auto">
          {good ? (
            <Badge tone="ok" dot>
              {fire.started.length} started
            </Badge>
          ) : skippedOnly ? (
            <Badge tone="warning" dot>
              started nothing
            </Badge>
          ) : (
            <Badge tone="neutral" dot>
              matched 0
            </Badge>
          )}
        </span>
      </div>

      <div className="mb-3.5 flex flex-wrap gap-x-5 gap-y-2">
        <Tally n={fire.matched} label="examined" tone="mut" />
        <Tally n={fire.started.length} label="started" tone={fire.started.length > 0 ? "ok" : "mut"} />
        <Tally n={fire.skips.length} label="skipped" tone={fire.skips.length > 0 ? "warn" : "mut"} />
        <Tally
          n={s.max_issues == null ? "—" : s.max_issues}
          label="max issues"
          tone="mut"
        />
      </div>

      {(fire.started.length > 0 || fire.skips.length > 0) && (
        <div className="flex flex-col gap-2">
          {fire.started.map((r) => (
            <div
              key={r.run_id}
              className="flex items-start gap-3 rounded-lg border border-edge bg-raised/50 px-3 py-2"
            >
              <IssueRef issueIID={r.issue_iid} webURL={r.web_url} />
              <div className="min-w-0 flex-1">
                {r.title && <div className="text-[12.5px] text-muted">{r.title}</div>}
              </div>
              <Link
                to={`/runs/${r.run_id}`}
                className="inline-flex shrink-0 items-center gap-1 rounded-md border border-ok/35 bg-ok/[0.08] px-2 py-0.5 font-mono text-[11.5px] text-ok hover:bg-ok/15"
              >
                run {r.run_id.slice(0, 8)}…
              </Link>
            </div>
          ))}
          {fire.skips.map((skip, i) => (
            <SkipRow key={`${skip.issue_iid ?? "prompt"}-${i}`} skip={skip} />
          ))}
        </div>
      )}

      {showHint && (
        <div className="mt-3.5 flex items-start gap-2.5 rounded-lg border border-brand/25 bg-brand/[0.06] px-3 py-2.5 text-[12.5px] text-muted">
          <span aria-hidden="true" className="shrink-0 font-bold text-brand">
            →
          </span>
          <div>
            <span className="font-semibold text-fg">Nothing newer was reached.</span> max issues is{" "}
            <span className="font-semibold text-fg">{s.max_issues ?? "—"}</span>, so only the oldest
            candidate{fire.skips.length === 1 ? " was" : "s were"} tried. Raise the cap so the sweep
            reaches the candidates behind them, or add the{" "}
            <code className="rounded bg-raised px-1 text-fg">{uziLabel}</code> label so they become runnable.
          </div>
        </div>
      )}
    </div>
  );
}

// Tally renders one big number + uppercase key in the detail panel's summary row.
function Tally({
  n,
  label,
  tone,
}: {
  n: number | string;
  label: string;
  tone: "ok" | "warn" | "mut";
}) {
  const color = tone === "ok" ? "text-ok" : tone === "warn" ? "text-warn" : "text-muted";
  return (
    <div className="flex flex-col gap-0.5">
      <span className={cx("text-[19px] font-semibold leading-none tabular-nums", color)}>{n}</span>
      <span className="text-[11px] uppercase tracking-wide text-faint">{label}</span>
    </div>
  );
}

// IssueRef renders the fire row's issue ref (PRD #411): `#<iid>` as an external forge
// link when the issue's web_url was snapshotted at fire time and is a valid https URL,
// otherwise a plain `#<iid>`; an issue-less fire (null iid) renders the "prompt" marker.
// The schedule DTO carries no forge_type on fire rows, so the accessible label uses a
// generic "the forge". This span is a SIBLING of the row's run <Link>, so a plain anchor
// is correct here (no nested-anchor, no stopPropagation needed).
function IssueRef({ issueIID, webURL }: { issueIID: number | null; webURL?: string | null }) {
  const cls = "min-w-[46px] shrink-0 pt-0.5 font-mono text-[12.5px] text-fg";
  if (issueIID == null) {
    return <span className={cls}>prompt</span>;
  }
  return (
    <ForgeIssueAnchor
      webUrl={webURL}
      iid={issueIID}
      label={`Open issue #${issueIID} on the forge`}
      className={cx(cls, "hover:text-brand")}
      fallbackClassName={cls}
    />
  );
}

// SkipRow renders one skipped candidate: its issue ref (or a prompt marker), its
// title when present, and a tone-coded badge carrying the human reason label.
function SkipRow({ skip }: { skip: LastFireSkip }) {
  return (
    <div className="flex items-start gap-3 rounded-lg border border-edge bg-raised/50 px-3 py-2">
      <IssueRef issueIID={skip.issue_iid} webURL={skip.web_url} />
      <div className="min-w-0 flex-1">
        {skip.title && <div className="text-[12.5px] text-muted">{skip.title}</div>}
      </div>
      <span className="shrink-0">
        <Badge tone={SKIP_REASON_TONES[skip.reason]}>{scheduleSkipReasonLabel(skip.reason)}</Badge>
      </span>
    </div>
  );
}

export function formatStamp(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return new Intl.DateTimeFormat(undefined, {
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
    month: "short",
    day: "numeric",
  }).format(d);
}
