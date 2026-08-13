// Schedules — the /schedules list page (PRD #241 M5, mock §1).
//
// An owner-scoped table of what each schedule targets, when it next/last fires, its
// options, and a per-row enable toggle (PATCH { enabled }). Recurring rows persist;
// a fired `once` row shows as terminal; a status='error' (parked) row is called out
// distinctly. The "New schedule" button and the per-row ✎ open the shared modal.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  ApiError,
  type LastFire,
  type LastFireSkip,
  type Schedule,
  type ScheduleSkipReason,
} from "../lib/api";
import { relativeFromNow, ScheduleModal } from "../components/ScheduleModal";
import { humanizeCron } from "../lib/schedulePresets";
import { scheduleSkipReasonLabel } from "../lib/scheduleSkipReasons";
import {
  Alert,
  Badge,
  type BadgeTone,
  Button,
  ListSkeleton,
  PageHeader,
  Toggle,
  cx,
} from "../components/ui";
import { ClockIcon, PencilIcon, PlayIcon, PlusIcon } from "../components/icons";

// The COLSPAN of the schedules table, so the expandable "Last fire" detail row
// stretches the full width (Target · When · Next run · Last run · Options · On).
const COLS = 6;

// Skip-reason badge tone, mirroring the mock's semantics: the actionable skips a
// schedule owner can fix (no PRD link, not eligible, a transient fetch failure)
// read amber; the benign, self-resolving ones (already running, body too large)
// read neutral. Exhaustive so a new union member is a tsc error, not a default.
const SKIP_REASON_TONES: Record<ScheduleSkipReason, BadgeTone> = {
  no_prd_link: "warning",
  not_eligible: "warning",
  already_running: "neutral",
  description_too_large: "neutral",
  fetch_failed: "warning",
};

export function Schedules() {
  const [schedules, setSchedules] = useState<Schedule[] | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [busyId, setBusyId] = useState<string>("");

  const load = useCallback(async () => {
    try {
      const rows = await api.listSchedules();
      setSchedules(rows);
      setError("");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load schedules");
      setSchedules([]);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const toggleEnabled = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    // Optimistic flip so the switch responds immediately.
    setSchedules((rows) =>
      rows ? rows.map((r) => (r.id === s.id ? { ...r, enabled: !r.enabled } : r)) : rows,
    );
    try {
      const updated = await api.updateSchedule(s.id, { enabled: !s.enabled });
      setSchedules((rows) => (rows ? rows.map((r) => (r.id === s.id ? updated : r)) : rows));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not update the schedule");
      // Revert on failure.
      setSchedules((rows) =>
        rows ? rows.map((r) => (r.id === s.id ? { ...r, enabled: s.enabled } : r)) : rows,
      );
    } finally {
      setBusyId("");
    }
  };

  const runNow = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      const { created } = await api.runScheduleNow(s.id);
      setNotice(
        created > 0
          ? `Started ${created} run${created === 1 ? "" : "s"} from this schedule.`
          : "Nothing fired — a matching run is already active (skipped by dedup).",
      );
      load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not run the schedule now");
    } finally {
      setBusyId("");
    }
  };

  const onSaved = () => {
    setCreating(false);
    setEditing(null);
    load();
  };

  const enabledCount = schedules?.filter((s) => s.enabled).length ?? 0;
  const activeCount = schedules?.filter((s) => s.enabled && s.status === "active").length ?? 0;
  const total = schedules?.length ?? 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Schedules"
        description={
          total === 0
            ? "Time-driven runs: one-time or recurring, against a pinned issue, a label sweep, or an ad-hoc prompt."
            : `${total} schedule${total === 1 ? "" : "s"} · ${activeCount} active · ${enabledCount} enabled`
        }
        actions={
          <Button onClick={() => setCreating(true)}>
            <PlusIcon /> New schedule
          </Button>
        }
      />

      {error && <Alert message={error} />}
      {notice && <Alert message={notice} tone="info" />}

      {schedules === null ? (
        <ListSkeleton rows={4} />
      ) : schedules.length === 0 ? (
        <div className="rounded-xl border border-dashed border-edge p-10 text-center">
          <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-raised text-lg text-muted">
            <ClockIcon />
          </div>
          <h3 className="text-sm font-medium text-fg">No schedules yet</h3>
          <p className="mx-auto mt-1 max-w-sm text-sm text-faint">
            Schedule a run to fire once at a future moment or on a recurring cadence — the dark factory can
            work off-hours.
          </p>
          <div className="mt-4">
            <Button onClick={() => setCreating(true)}>
              <PlusIcon /> New schedule
            </Button>
          </div>
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-edge bg-surface">
          <table className="w-full text-left text-sm">
            <thead>
              <tr className="border-b border-edge text-[12.5px] text-muted">
                <th className="px-4 py-3 font-medium">Target</th>
                <th className="px-4 py-3 font-medium">When</th>
                <th className="px-4 py-3 font-medium">Next run</th>
                <th className="px-4 py-3 font-medium">Last run</th>
                <th className="px-4 py-3 font-medium">Options</th>
                <th className="px-4 py-3 text-right font-medium">On</th>
              </tr>
            </thead>
            <tbody>
              {schedules.map((s) => (
                <ScheduleRow
                  key={s.id}
                  s={s}
                  busy={busyId === s.id}
                  onToggle={() => toggleEnabled(s)}
                  onRunNow={() => runNow(s)}
                  onEdit={() => setEditing(s)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Legend />

      {creating && <ScheduleModal onClose={() => setCreating(false)} onSaved={onSaved} />}
      {editing && (
        <ScheduleModal editing={editing} onClose={() => setEditing(null)} onSaved={onSaved} />
      )}
    </div>
  );
}

function ScheduleRow({
  s,
  busy,
  onToggle,
  onRunNow,
  onEdit,
}: {
  s: Schedule;
  busy: boolean;
  onToggle: () => void;
  onRunNow: () => void;
  onEdit: () => void;
}) {
  const off = !s.enabled;
  const fired = s.timing === "once" && s.status === "fired";
  const parked = s.status === "error";
  const [expanded, setExpanded] = useState(false);
  return (
    <>
    <tr className={cx("border-t border-edge align-middle", off && "opacity-60")}>
      {/* Target */}
      <td className="px-4 py-3">
        <div className="flex items-center gap-2 font-medium text-fg">
          <span>{targetTitle(s)}</span>
          {s.target === "sweep" && (
            <Badge tone="info" dot>
              sweep
            </Badge>
          )}
          {s.target === "prompt" && (
            <Badge tone="ok" dot>
              prompt
            </Badge>
          )}
          {s.timing === "once" && (
            <Badge tone="brand" dot>
              once
            </Badge>
          )}
          {parked && (
            <Badge tone="danger" dot>
              parked
            </Badge>
          )}
        </div>
        <div className="mt-0.5 font-mono text-[12px] text-faint">
          {s.repo_path || "repo unavailable"}
          {s.target === "prompt" && " · no issue"}
        </div>
      </td>

      {/* When */}
      <td className="px-4 py-3">
        {s.timing === "recurring" ? (
          <>
            <div className="font-mono text-[12.5px] text-fg">{s.cron_expr}</div>
            <div className="mt-0.5 text-[12px] text-muted">
              {humanizeCron(s.cron_expr)} · {s.timezone}
            </div>
          </>
        ) : (
          <>
            <div className="font-mono text-[12.5px] text-fg">{s.run_at ? formatStamp(s.run_at) : "—"}</div>
            <div className="mt-0.5 text-[12px] text-muted">One time · {s.timezone}</div>
          </>
        )}
      </td>

      {/* Next run */}
      <td className="px-4 py-3">
        {parked ? (
          <Badge tone="danger">error</Badge>
        ) : off ? (
          <Badge tone="neutral">paused</Badge>
        ) : fired ? (
          <Badge tone="neutral">fired</Badge>
        ) : s.next_fire_at ? (
          <div className="text-[12.5px] text-muted">
            {formatStamp(s.next_fire_at)}
            <div className="text-[11px] text-faint">{relativeFromNow(s.next_fire_at)}</div>
          </div>
        ) : (
          <span className="text-faint">—</span>
        )}
      </td>

      {/* Last run */}
      <td className="px-4 py-3">
        {s.last_fire ? (
          <LastRunOutcome
            fire={s.last_fire}
            expanded={expanded}
            onToggle={() => setExpanded((v) => !v)}
          />
        ) : s.last_fired_at ? (
          <div className="text-[12.5px] text-muted">{formatStamp(s.last_fired_at)}</div>
        ) : (
          <span className="text-faint">— never fired</span>
        )}
      </td>

      {/* Options */}
      <td className="px-4 py-3">
        <div className="flex flex-wrap gap-1">
          {s.wait_on_limit && <OptionChip>wait-on-limit</OptionChip>}
          {s.auto_approve && <OptionChip>auto-approve</OptionChip>}
          {!s.wait_on_limit && !s.auto_approve && <span className="text-[12px] text-faint">defaults</span>}
        </div>
      </td>

      {/* On + actions */}
      <td className="px-4 py-3">
        <div className="flex items-center justify-end gap-1.5">
          <Button
            variant="ghost"
            size="sm"
            title="Run now"
            aria-label="Run now"
            disabled={busy}
            onClick={onRunNow}
          >
            <PlayIcon />
          </Button>
          <Button variant="ghost" size="sm" title="Edit" aria-label="Edit" onClick={onEdit}>
            <PencilIcon />
          </Button>
          <Toggle
            checked={s.enabled}
            onChange={onToggle}
            disabled={busy}
            label={s.enabled ? "Disable schedule" : "Enable schedule"}
          />
        </div>
      </td>
    </tr>
    {s.last_fire && expanded && (
      <tr className="border-t border-edge">
        <td colSpan={COLS} className="bg-raised/30 px-4 pb-4 pt-0">
          <LastFireDetail s={s} fire={s.last_fire} />
        </td>
      </tr>
    )}
    </>
  );
}

// LastRunOutcome is the enriched "Last run" cell for a schedule that has a
// persisted fire (PRD #308 M4, mock §1): an outcome badge, a muted "{stamp} ·
// matched N" line, and a disclosure that toggles the "Last fire" detail row.
function LastRunOutcome({
  fire,
  expanded,
  onToggle,
}: {
  fire: LastFire;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="inline-flex">
        <OutcomeBadge fire={fire} />
      </span>
      <span className="text-[11px] text-faint tabular-nums">
        {formatStamp(fire.fired_at)} · matched {fire.matched}
      </span>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        className="inline-flex items-center gap-1 text-[11px] font-medium text-info hover:underline"
      >
        Last fire
        <span aria-hidden="true" className={cx("transition-transform", expanded && "rotate-180")}>
          ▾
        </span>
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
// fire timestamp + a status badge, a matched/started/skipped/max-issues tally, one
// row per started run (linking to the run) and per skipped candidate (with its
// typed reason), and the actionable cap hint when a capped fire started nothing.
function LastFireDetail({ s, fire }: { s: Schedule; fire: LastFire }) {
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
        <Tally n={fire.matched} label="matched" tone="mut" />
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
              <span className="min-w-[46px] shrink-0 pt-0.5 font-mono text-[12.5px] text-fg">
                {r.issue_iid != null ? `#${r.issue_iid}` : "prompt"}
              </span>
              <div className="min-w-0 flex-1">
                {r.title && <div className="text-[12.5px] text-muted">{r.title}</div>}
              </div>
              <Link
                to={`/runs/${r.run_id}`}
                className="inline-flex shrink-0 items-center gap-1 rounded-md border border-ok/35 bg-ok/[0.08] px-2 py-0.5 font-mono text-[11.5px] text-ok hover:bg-ok/15"
              >
                ▶ run {r.run_id.slice(0, 4)}…
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
            reaches the candidates behind them, or add <code className="rounded bg-raised px-1 text-fg">PRDLESS</code>{" "}
            / a PRD link.
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

// SkipRow renders one skipped candidate: its issue ref (or a prompt marker), its
// title when present, and a tone-coded badge carrying the human reason label.
function SkipRow({ skip }: { skip: LastFireSkip }) {
  return (
    <div className="flex items-start gap-3 rounded-lg border border-edge bg-raised/50 px-3 py-2">
      <span className="min-w-[46px] shrink-0 pt-0.5 font-mono text-[12.5px] text-fg">
        {skip.issue_iid != null ? `#${skip.issue_iid}` : "prompt"}
      </span>
      <div className="min-w-0 flex-1">
        {skip.title && <div className="text-[12.5px] text-muted">{skip.title}</div>}
      </div>
      <span className="shrink-0">
        <Badge tone={SKIP_REASON_TONES[skip.reason]}>{scheduleSkipReasonLabel(skip.reason)}</Badge>
      </span>
    </div>
  );
}

function OptionChip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted">
      {children}
    </span>
  );
}

function Legend() {
  return (
    <div className="flex flex-wrap gap-x-5 gap-y-2 text-[12px] text-muted">
      <span className="flex items-center gap-1.5">
        <Badge tone="brand" dot>
          once
        </Badge>
        fires a single time, then goes terminal
      </span>
      <span className="flex items-center gap-1.5">
        <Badge tone="info" dot>
          sweep
        </Badge>
        fires on issues matching a label (default: PRD)
      </span>
      <span className="flex items-center gap-1.5">
        <Badge tone="ok" dot>
          prompt
        </Badge>
        issue-less run, opens an MR
      </span>
    </div>
  );
}

// targetTitle renders a schedule's human target line for the first column.
function targetTitle(s: Schedule): string {
  switch (s.target) {
    case "issue":
      return s.issue_iid != null ? `#${s.issue_iid}` : "Pinned issue";
    case "sweep":
      return s.labels && s.labels.length > 0
        ? `Sweep · label ${s.labels.join(", ")}`
        : "Sweep eligible PRD issues";
    case "prompt":
      return s.prompt ? `Prompt: ${truncate(s.prompt, 42)}` : "Prompt";
  }
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

function formatStamp(iso: string): string {
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
