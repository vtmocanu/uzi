// Schedules — the /schedules list page (PRD #241 M5, mock §1).
//
// An owner-scoped table of what each schedule targets, when it next/last fires, its
// options, and a per-row enable toggle (PATCH { enabled }). Recurring rows persist;
// a fired `once` row shows as terminal; a status='error' (parked) row is called out
// distinctly. The "New schedule" button and the per-row ✎ open the shared modal.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Schedule } from "../lib/api";
import { relativeFromNow, ScheduleModal } from "../components/ScheduleModal";
import { humanizeCron } from "../lib/schedulePresets";
import {
  Alert,
  Badge,
  Button,
  ListSkeleton,
  PageHeader,
  Toggle,
  cx,
} from "../components/ui";
import { ClockIcon, PencilIcon, PlayIcon, PlusIcon } from "../components/icons";

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
  return (
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
        {s.last_fired_at ? (
          <div className="text-[12.5px] text-muted">{formatStamp(s.last_fired_at)}</div>
        ) : (
          <span className="text-faint">—</span>
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
