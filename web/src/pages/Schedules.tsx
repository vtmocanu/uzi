// Schedules — the /schedules list page (PRD #241 M5, mock §1).
//
// An owner-scoped table of what each schedule targets, when it next/last fires, its
// options, and a per-row enable toggle (PATCH { enabled }). Recurring rows persist;
// a fired `once` row shows as terminal; a status='error' (parked) row is called out
// distinctly. The "New schedule" button and the per-row ✎ open the shared modal.

import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  type CatalogEntry,
  type Repo,
  type Schedule,
  type ScheduleCatalog,
} from "../lib/api";
import { relativeFromNow, ScheduleModal } from "../components/ScheduleModal";
import { DefaultJobs } from "../components/DefaultJobs";
import { AddAnotherRepo, ScheduleGroupRow, ScheduleSubRow } from "../components/ScheduleGroupRow";
import { LastRunOutcome, LastFireDetail, formatStamp } from "../components/LastRun";
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
import {
  ClockIcon,
  CopyIcon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  TrashIcon,
} from "../components/icons";

type Tab = "defaults" | "mine";

// The tab order, for the APG roving-tabindex arrow-key handler.
const TAB_ORDER: Tab[] = ["defaults", "mine"];
const tabId = (t: Tab) => `sched-tab-${t}`;
const panelId = (t: Tab) => `sched-panel-${t}`;

// The COLSPAN of the schedules table, so the expandable "Last fire" detail row
// stretches the full width (Target · When · Next run · Last run · Options · On).
const COLS = 6;

// Why an issue-target schedule cannot be replicated onto another repo: the issue
// number is repo-relative, so the same iid points at a different (or missing) issue
// elsewhere. The API now rejects add-repo for issue targets with a 422, so the
// "Add another repo" affordance is disabled with this tooltip rather than left to
// no-op (PRD #636 follow-up, issue #638 P1c).
const ISSUE_NO_MULTI_REPO = "Issue schedules can't span repos - issue numbers are repo-relative";

// TAB_CLASS mirrors AdminShell's tab strip (issue #204 overflow contract) so the two
// in-page tabs read identically to the app's other tabbed surfaces.
const TAB_BASE =
  "-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors";
const TAB_ACTIVE = "border-brand text-fg";
const TAB_INACTIVE = "border-transparent text-muted hover:border-edge-strong hover:text-fg";

export function Schedules() {
  const [tab, setTab] = useState<Tab>("defaults");
  const [schedules, setSchedules] = useState<Schedule[] | null>(null);
  const [catalog, setCatalog] = useState<ScheduleCatalog | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [busyId, setBusyId] = useState<string>("");
  // Which sibling groups are expanded in My schedules (keyed by sibling_group_id), the
  // same Set<string> disclosure pattern DefaultJobs uses for its catalog slugs. Lives in
  // the parent so "add another repo" can auto-expand the group its new sibling landed in.
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());
  // Session-scoped clone provenance: the DTO carries no "cloned from" field (a clone
  // bakes its source in with catalog_slug=null), so this shows the source name for a
  // clone made THIS session. Persistent provenance would need a backend field (seam).
  const [clonedFrom, setClonedFrom] = useState<Record<string, string>>({});
  // Refs to the tab buttons so the arrow-key handler can move focus with selection
  // (APG tabs, automatic activation): Left/Right wrap, Home/End jump to the ends.
  const tabRefs = useRef<Partial<Record<Tab, HTMLButtonElement | null>>>({});
  const onTabKeyDown = (e: React.KeyboardEvent) => {
    const idx = TAB_ORDER.indexOf(tab);
    let next = idx;
    if (e.key === "ArrowRight") next = (idx + 1) % TAB_ORDER.length;
    else if (e.key === "ArrowLeft") next = (idx - 1 + TAB_ORDER.length) % TAB_ORDER.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = TAB_ORDER.length - 1;
    else return;
    e.preventDefault();
    const target = TAB_ORDER[next];
    setTab(target);
    tabRefs.current[target]?.focus();
  };

  const load = useCallback(async () => {
    try {
      const [rows, cat, repoList] = await Promise.all([
        api.listSchedules(),
        api.listScheduleCatalog(),
        api.listRepos().then((r) => r.repos),
      ]);
      setSchedules(rows);
      setCatalog(cat);
      setRepos(repoList);
      setError("");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load schedules");
      setSchedules((cur) => cur ?? []);
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
      // Keep the catalog view's enablement pause-flags in sync.
      load();
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

  // Enable a catalog default on a set of repos — one call per repo (client fan-out,
  // matching the CLI), reporting a partial failure rather than swallowing it.
  const enableDefault = async (entry: CatalogEntry, repoIds: string[]) => {
    if (repoIds.length === 0) return;
    // In-flight guard (matches every other mutating action here): a double-click would
    // otherwise fan out duplicate enableCatalogSchedule calls. busyId keys off the entry
    // slug (distinct from any schedule id), which disables this entry's Enable buttons.
    if (busyId) return;
    setBusyId(entry.slug);
    setError("");
    setNotice("");
    try {
      const results = await Promise.allSettled(
        repoIds.map((rid) => api.enableCatalogSchedule(rid, entry.slug)),
      );
      const ok = results.filter((r) => r.status === "fulfilled").length;
      const failed = results.length - ok;
      if (failed > 0) {
        setError(`Enabled “${entry.name}” on ${ok} of ${repoIds.length} repos; ${failed} failed.`);
      } else {
        setNotice(
          `Enabled “${entry.name}” on ${ok} repo${ok === 1 ? "" : "s"} → ${ok} schedule${ok === 1 ? "" : "s"}.`,
        );
      }
      await load();
    } finally {
      setBusyId("");
    }
  };

  const resetDefault = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      await api.resetSchedule(s.id);
      setNotice("Reset to the catalog default.");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not reset the schedule");
    } finally {
      setBusyId("");
    }
  };

  // Clone a schedule (default or user) into an editable user copy, then open the edit
  // modal on it so the owner can immediately edit the now-unlocked prompt.
  const cloneSchedule = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      const clone = await api.cloneSchedule(s.id);
      const label = s.origin === "default" && s.catalog_slug ? catalogName(catalog, s.catalog_slug) : "a schedule";
      setClonedFrom((m) => ({ ...m, [clone.id]: label }));
      await load();
      setEditing(clone);
      setTab("mine");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not clone the schedule");
    } finally {
      setBusyId("");
    }
  };

  const removeSchedule = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    try {
      await api.deleteSchedule(s.id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not remove the schedule");
    } finally {
      setBusyId("");
    }
  };

  const toggleGroupExpand = (groupId: string) =>
    setExpandedGroups((s) => {
      const next = new Set(s);
      if (next.has(groupId)) next.delete(groupId);
      else next.add(groupId);
      return next;
    });

  // Add another repo to a custom schedule (PRD #636 M3): replicate the source sibling's
  // config onto a new repo via the M1 add-repo endpoint. On success, refresh and
  // auto-expand the (now ≥2-member) group so the new sibling is visible. A 409 (the repo
  // already carries a sibling in this group) is friendly and non-fatal, not an error.
  const addRepo = async (source: Schedule, repoId: string) => {
    if (busyId) return;
    setBusyId(source.id);
    setError("");
    setNotice("");
    try {
      const sibling = await api.addScheduleRepo(source.id, repoId);
      setNotice("Added on another repo.");
      await load();
      if (sibling.sibling_group_id) {
        setExpandedGroups((s) => new Set(s).add(sibling.sibling_group_id as string));
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setNotice("That schedule is already on that repo.");
      } else {
        setError(err instanceof ApiError ? err.message : "Could not add the repo");
      }
    } finally {
      setBusyId("");
    }
  };

  const onSaved = () => {
    setCreating(false);
    setEditing(null);
    load();
  };

  // My schedules holds the owner-authored (and cloned) rows; catalog defaults live in
  // the Default jobs tab, so they are filtered out here to avoid showing twice.
  const mine = (schedules ?? []).filter((s) => s.origin === "user");
  const enabledCount = mine.filter((s) => s.enabled).length;
  const activeCount = mine.filter((s) => s.enabled && s.status === "active").length;
  const total = mine.length;
  const enabledDefaults = (schedules ?? []).filter((s) => s.origin === "default" && s.enabled).length;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Schedules"
        description="Time-driven runs: enable a shipped default job, or author your own against a pinned issue, a label sweep, or an ad-hoc prompt."
        actions={
          <Button onClick={() => setCreating(true)}>
            <PlusIcon /> New schedule
          </Button>
        }
      />

      {/* Two tabs, same table shape (PRD #589 M6): shipped defaults vs the owner's own. */}
      <div className="flex gap-1 overflow-x-auto border-b border-edge" role="tablist" aria-label="Schedules">
        <button
          type="button"
          role="tab"
          id={tabId("defaults")}
          aria-selected={tab === "defaults"}
          aria-controls={panelId("defaults")}
          tabIndex={tab === "defaults" ? 0 : -1}
          ref={(el) => {
            tabRefs.current.defaults = el;
          }}
          onKeyDown={onTabKeyDown}
          onClick={() => setTab("defaults")}
          className={cx(TAB_BASE, tab === "defaults" ? TAB_ACTIVE : TAB_INACTIVE)}
        >
          Default jobs{enabledDefaults > 0 ? ` · ${enabledDefaults}` : ""}
        </button>
        <button
          type="button"
          role="tab"
          id={tabId("mine")}
          aria-selected={tab === "mine"}
          aria-controls={panelId("mine")}
          tabIndex={tab === "mine" ? 0 : -1}
          ref={(el) => {
            tabRefs.current.mine = el;
          }}
          onKeyDown={onTabKeyDown}
          onClick={() => setTab("mine")}
          className={cx(TAB_BASE, tab === "mine" ? TAB_ACTIVE : TAB_INACTIVE)}
        >
          My schedules{total > 0 ? ` · ${total}` : ""}
        </button>
      </div>

      {/* One tabpanel per tab, each labelled by its tab; the inactive one is unmounted,
          so its id/aria-controls link is live only for the shown panel (APG tabs). */}
      {schedules === null || catalog === null ? (
        <ListSkeleton rows={4} />
      ) : tab === "defaults" ? (
        <div role="tabpanel" id={panelId("defaults")} aria-labelledby={tabId("defaults")}>
          <DefaultJobs
            catalog={catalog}
            schedules={schedules}
            repos={repos}
            busyId={busyId}
            onEnable={enableDefault}
            onTogglePause={toggleEnabled}
            onRunNow={runNow}
            onReset={resetDefault}
            onClone={cloneSchedule}
            onRemove={removeSchedule}
            onEdit={setEditing}
            notice={notice}
            error={error}
          />
        </div>
      ) : (
        <div
          className="space-y-6"
          role="tabpanel"
          id={panelId("mine")}
          aria-labelledby={tabId("mine")}
        >
          {error && <Alert message={error} />}
          {notice && <Alert message={notice} tone="info" />}
          <p className="text-sm text-muted">
            {total === 0
              ? "Your own schedules — none yet."
              : `${total} schedule${total === 1 ? "" : "s"} · ${activeCount} active · ${enabledCount} enabled`}
          </p>

          {mine.length === 0 ? (
            <div className="rounded-xl border border-dashed border-edge p-10 text-center">
              <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-raised text-lg text-muted">
                <ClockIcon />
              </div>
              <h3 className="text-sm font-medium text-fg">No schedules of your own</h3>
              <p className="mx-auto mt-1 max-w-sm text-sm text-faint">
                Author a run to fire once at a future moment or on a recurring cadence, or enable a
                shipped default from the Default jobs tab. The dark factory can work off-hours.
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
                  {groupMine(mine).map((row) =>
                    row.kind === "single" ? (
                      <ScheduleRow
                        key={row.schedule.id}
                        s={row.schedule}
                        busy={busyId === row.schedule.id}
                        addBusy={busyId !== ""}
                        clonedFrom={clonedFrom[row.schedule.id]}
                        repos={repos}
                        onToggle={() => toggleEnabled(row.schedule)}
                        onRunNow={() => runNow(row.schedule)}
                        onEdit={() => setEditing(row.schedule)}
                        onClone={() => cloneSchedule(row.schedule)}
                        onAddRepo={(repoId) => addRepo(row.schedule, repoId)}
                      />
                    ) : (
                      <MyScheduleGroup
                        key={row.groupId}
                        groupId={row.groupId}
                        members={row.members}
                        repos={repos}
                        busyId={busyId}
                        expanded={expandedGroups.has(row.groupId)}
                        onToggleExpand={() => toggleGroupExpand(row.groupId)}
                        onToggle={toggleEnabled}
                        onRunNow={runNow}
                        onEdit={setEditing}
                        onRemove={removeSchedule}
                        onAddRepo={addRepo}
                      />
                    ),
                  )}
                </tbody>
              </table>
            </div>
          )}

          <Legend />
        </div>
      )}

      {creating && <ScheduleModal onClose={() => setCreating(false)} onSaved={onSaved} />}
      {editing && (
        <ScheduleModal
          editing={editing}
          onClose={() => setEditing(null)}
          onSaved={onSaved}
          onCloneToEdit={(s) => {
            setEditing(null);
            cloneSchedule(s);
          }}
        />
      )}
    </div>
  );
}

// catalogName resolves a slug to its display name for the "cloned from" label.
function catalogName(catalog: ScheduleCatalog | null, slug: string): string {
  return catalog?.entries.find((e) => e.slug === slug)?.name ?? "a default";
}

function ScheduleRow({
  s,
  busy,
  addBusy,
  clonedFrom,
  repos,
  onToggle,
  onRunNow,
  onEdit,
  onClone,
  onAddRepo,
}: {
  s: Schedule;
  busy: boolean;
  // A GLOBAL busy signal (any row's op in flight), used only for the add-repo
  // affordance so it matches the group's global gating: addRepo() no-ops while
  // busyId is set, so the picker must be disabled the whole time, not just for
  // this row's own op (issue #638 P2a). Run-now/clone/edit/toggle stay per-row `busy`.
  addBusy: boolean;
  // The source name when this row was cloned this session (session-scoped label).
  clonedFrom?: string;
  // The owner's repos, for the "add another repo" picker (excludes this row's own repo).
  repos: Repo[];
  onToggle: () => void;
  onRunNow: () => void;
  onEdit: () => void;
  onClone: () => void;
  // Adds a sibling on repoId, promoting this standalone row into a group (PRD #636 M3).
  onAddRepo: (repoId: string) => void;
}) {
  const off = !s.enabled;
  const fired = s.timing === "once" && s.status === "fired";
  const parked = s.status === "error";
  const [expanded, setExpanded] = useState(false);
  const [addingRepo, setAddingRepo] = useState(false);
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
          {clonedFrom && (
            <Badge tone="neutral" title={`Cloned from ${clonedFrom}`}>
              cloned from {clonedFrom}
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
            panelId={`last-fire-${s.id}`}
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
          <Button
            variant="ghost"
            size="sm"
            title="Clone"
            aria-label="Clone schedule"
            disabled={busy}
            onClick={onClone}
          >
            <CopyIcon />
          </Button>
          {/* Add another repo — replicates this config onto a new repo as a sibling,
              promoting this standalone row into an expandable group (PRD #636 M3).
              Disabled for issue targets (repo-relative iid, issue #638 P1c) and while
              ANY row's op is in flight (global gate matching the group, #638 P2a).
              aria-label stays stable so it's findable by role/name; the title carries
              the issue-target reason when that's why it's disabled. */}
          <Button
            variant="ghost"
            size="sm"
            title={s.target === "issue" ? ISSUE_NO_MULTI_REPO : "Add another repo"}
            // When blocked for an issue target the button is disabled, so its title
            // tooltip is unreachable by keyboard/SR/touch — carry the reason in the
            // accessible name instead so assistive tech announces why (issue #638 P1c).
            aria-label={s.target === "issue" ? "Add another repo (unavailable for issue schedules)" : "Add another repo"}
            aria-expanded={addingRepo}
            aria-controls={`add-repo-${s.id}`}
            disabled={addBusy || s.target === "issue"}
            onClick={() => setAddingRepo((v) => !v)}
          >
            <PlusIcon />
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
    {addingRepo && (
      <tr className="border-t border-edge">
        {/* id pairs with the toggle's aria-controls (a11y review fix); conditionally
            rendered, so the reference exists only while the picker is open. */}
        <td id={`add-repo-${s.id}`} colSpan={COLS} className="bg-raised/30 px-4 pb-4 pt-2">
          <AddAnotherRepo
            name={targetTitle(s)}
            repos={repos}
            taken={new Set([s.repo_id])}
            busy={addBusy}
            disabledReason={s.target === "issue" ? ISSUE_NO_MULTI_REPO : undefined}
            onAddRepo={(repoId) => {
              onAddRepo(repoId);
              setAddingRepo(false);
            }}
          />
        </td>
      </tr>
    )}
    {s.last_fire && expanded && (
      <tr className="border-t border-edge">
        {/* The id pairs with the disclosure's aria-controls (review-wave fix 4).
            Conditionally rendered, so the reference only exists while expanded —
            which is when aria-controls has anything to say. */}
        <td id={`last-fire-${s.id}`} colSpan={COLS} className="bg-raised/30 px-4 pb-4 pt-0">
          <LastFireDetail s={s} fire={s.last_fire} />
        </td>
      </tr>
    )}
    </>
  );
}

// A My-schedules render unit: a standalone row, or an expandable sibling group.
type MineRow =
  | { kind: "single"; schedule: Schedule }
  | { kind: "group"; groupId: string; members: Schedule[] };

// groupMine groups the owner's rows by sibling_group_id per PRD #636 Decision 3, WITHOUT
// ever collapsing the null-group rows together (the naive groupBy(sibling_group_id) bug
// that would render every standalone schedule as one bogus group):
//   - sibling_group_id === null           → always a standalone row;
//   - a non-null id with exactly ONE live → also standalone (never a one-child group);
//     the load-bearing view collapse for delete / partial-failure / repo-disconnect;
//   - a non-null id with ≥2 live members   → one expandable group, emitted at the
//     position of its first member so input order is preserved.
function groupMine(mine: Schedule[]): MineRow[] {
  const buckets = new Map<string, Schedule[]>();
  for (const s of mine) {
    if (!s.sibling_group_id) continue;
    const b = buckets.get(s.sibling_group_id);
    if (b) b.push(s);
    else buckets.set(s.sibling_group_id, [s]);
  }
  const emitted = new Set<string>();
  const rows: MineRow[] = [];
  for (const s of mine) {
    const gid = s.sibling_group_id;
    if (!gid) {
      rows.push({ kind: "single", schedule: s });
      continue;
    }
    const members = buckets.get(gid);
    if (!members || members.length < 2) {
      // A non-null id with a single live member is a standalone row, never a group.
      rows.push({ kind: "single", schedule: s });
      continue;
    }
    if (emitted.has(gid)) continue;
    emitted.add(gid);
    rows.push({ kind: "group", groupId: gid, members });
  }
  return rows;
}

// MyScheduleGroup renders ≥2 sibling rows sharing a sibling_group_id as one expandable
// summary (name + repo-count + toggle, via the neutral ScheduleGroupRow) over per-repo
// sub-rows. Siblings are independent (Decision 1): every sub-row control targets only
// its own row.
function MyScheduleGroup({
  groupId,
  members,
  repos,
  busyId,
  expanded,
  onToggleExpand,
  onToggle,
  onRunNow,
  onEdit,
  onRemove,
  onAddRepo,
}: {
  groupId: string;
  members: Schedule[];
  repos: Repo[];
  busyId: string;
  expanded: boolean;
  onToggleExpand: () => void;
  onToggle: (s: Schedule) => void;
  onRunNow: (s: Schedule) => void;
  onEdit: (s: Schedule) => void;
  onRemove: (s: Schedule) => void;
  onAddRepo: (source: Schedule, repoId: string) => void;
}) {
  // The siblings share a job config (copied at add-repo time), so the summary name is the
  // head member's target title; per-repo cadence/state lives in the sub-rows.
  const head = members[0];
  const name = targetTitle(head);
  const taken = new Set(members.map((m) => m.repo_id));
  return (
    <ScheduleGroupRow
      name={name}
      cols={COLS}
      expanded={expanded}
      onToggleExpand={onToggleExpand}
      disclosureId={`sibling-repos-${groupId}`}
      expandLabelName={name}
      repoCount={members.length}
    >
      {/* PRD #636 M3 (line ~106): the custom group summary carries name + repo-count +
          expand toggle ONLY — no type pill. The type is already spelled out in the name
          (targetTitle prefixes "Prompt:" / "Sweep ·"), and siblings may have diverged, so
          per-repo target/cadence/state live in the sub-rows below, not the summary. */}
      {members.map((s) => (
        <MyScheduleSubRow
          key={s.id}
          s={s}
          busy={busyId === s.id}
          onToggle={() => onToggle(s)}
          onRunNow={() => onRunNow(s)}
          onEdit={() => onEdit(s)}
          onRemove={() => onRemove(s)}
        />
      ))}
      {/* Disabled for an issue-target group (repo-relative iid, issue #638 P1c). In
          practice such a group can only exist from before this fix, but the control
          must still be gated. */}
      <AddAnotherRepo
        name={name}
        repos={repos}
        taken={taken}
        busy={busyId !== ""}
        disabledReason={head.target === "issue" ? ISSUE_NO_MULTI_REPO : undefined}
        onAddRepo={(repoId) => onAddRepo(head, repoId)}
      />
    </ScheduleGroupRow>
  );
}

// MyScheduleSubRow is one per-repo sibling line under a group summary: the neutral
// ScheduleSubRow shell plus this tab's editable per-row controls (run-now, edit,
// remove, pause/resume). No lock, no reset — this is a user-owned row.
function MyScheduleSubRow({
  s,
  busy,
  onToggle,
  onRunNow,
  onEdit,
  onRemove,
}: {
  s: Schedule;
  busy: boolean;
  onToggle: () => void;
  onRunNow: () => void;
  onEdit: () => void;
  onRemove: () => void;
}) {
  const repoLabel = s.repo_path || "repo unavailable";
  const nextFire = s.next_fires[0] ?? s.next_fire_at;
  return (
    <ScheduleSubRow
      repoLabel={repoLabel}
      enabled={s.enabled}
      cronExpr={s.cron_expr}
      nextFire={nextFire}
      // Per-repo target pill: the group summary no longer carries the type (PRD #636 M3),
      // and a sibling can diverge (it is an independently editable row), so surface each
      // row's own target here rather than dropping it silently.
      badges={
        <>
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
          {s.target === "self_improve" && (
            <Badge tone="ok" dot>
              self-improve
            </Badge>
          )}
          {s.target === "issue" && (
            <Badge tone="brand" dot>
              issue
            </Badge>
          )}
        </>
      }
      actions={
        <>
          <Button variant="ghost" size="sm" title="Run now" aria-label={`Run now on ${repoLabel}`} disabled={busy} onClick={onRunNow}>
            <PlayIcon />
          </Button>
          <Button variant="ghost" size="sm" title="Edit" aria-label={`Edit on ${repoLabel}`} onClick={onEdit}>
            <PencilIcon />
          </Button>
          <Button variant="ghost" size="sm" title="Remove" aria-label={`Remove on ${repoLabel}`} disabled={busy} onClick={onRemove}>
            <TrashIcon />
          </Button>
          <Toggle
            checked={s.enabled}
            onChange={onToggle}
            disabled={busy}
            label={s.enabled ? `Pause on ${repoLabel}` : `Resume on ${repoLabel}`}
          />
        </>
      }
    />
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
    case "self_improve":
      return "Self-improvement";
  }
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}
