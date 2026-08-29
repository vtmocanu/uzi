// DefaultJobs — the "Default jobs" tab of the Schedules page (PRD #589 M6).
//
// The builtin schedtmpl catalog rendered in the same table shape as My schedules
// (Target · When · Next run · Last run · Options · On). Each catalog entry is one
// summary row carrying a 🔒 baked-prompt marker (the tab header already says "Default
// jobs", so a per-row DEFAULT chip was redundant and was dropped — PRD #636 M5);
// enabling it on a repo materializes an origin='default' schedule. A default enabled on several repos
// collapses to one summary row that expands into per-repo sub-rows (Layout A) — one
// blueprint, many production lines.
//
// The guardrail: the per-entry Enable affordance renders only when clicking it would do
// something — i.e. at least one repo picked in the shared RepoMultiSelect is not already
// enabled on that job (success criterion 1). Enabling fans out one enableCatalogSchedule
// call per actionable repo (client-side, matching the CLI). A repo already materialized
// for a slug is never offered a fresh enable — its sub-row carries the pause/resume toggle
// instead (re-enabling a paused default is a server no-op).

import { useState } from "react";
import type { CatalogEntry, Repo, Schedule, ScheduleCatalog } from "../lib/api";
import { humanizeCron } from "../lib/schedulePresets";
import { RepoMultiSelect } from "./RepoMultiSelect";
import { ScheduleGroupRow, ScheduleSubRow } from "./ScheduleGroupRow";
import { LastRunOutcome, LastFireDetail, formatStamp } from "./LastRun";
import { SweepLabelWarn } from "./SweepLabelWarn";
import { Alert, Badge, Button, Toggle } from "./ui";
import {
  CopyIcon,
  LockIcon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  RotateCcwIcon,
  TrashIcon,
} from "./icons";

const COLS = 6;

export function DefaultJobs({
  catalog,
  schedules,
  repos,
  busyId,
  onEnable,
  onTogglePause,
  onRunNow,
  onReset,
  onClone,
  onRemove,
  onEdit,
  notice,
  error,
}: {
  catalog: ScheduleCatalog;
  // All the owner's schedules; the default (origin='default') rows are the enablements.
  schedules: Schedule[];
  repos: Repo[];
  busyId: string;
  // Enable an entry on a set of repos (client fan-out). Resolves when all calls settle.
  onEnable: (entry: CatalogEntry, repoIds: string[]) => Promise<void>;
  onTogglePause: (s: Schedule) => void;
  onRunNow: (s: Schedule) => void;
  onReset: (s: Schedule) => void;
  onClone: (s: Schedule) => void;
  onRemove: (s: Schedule) => void;
  onEdit: (s: Schedule) => void;
  notice: string;
  error: string;
}) {
  // The shared repo selection driving every entry's Enable button (guardrail: a row's
  // Enable renders only when at least one selected repo is not already enabled on it).
  const [selectedRepos, setSelectedRepos] = useState<string[]>([]);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  // Sweep-warn targets set after an enable, so a missing selector label surfaces once the
  // enable lands (the warn self-resolves when the label exists or is created).
  const [warnTargets, setWarnTargets] = useState<{ repoId: string; repoPath: string; labels: string[] }[]>([]);

  const defaultRows = schedules.filter((s) => s.origin === "default" && !!s.catalog_slug);
  const rowsFor = (slug: string) => defaultRows.filter((s) => s.catalog_slug === slug);

  const toggleExpand = (slug: string) =>
    setExpanded((s) => {
      const next = new Set(s);
      if (next.has(slug)) next.delete(slug);
      else next.add(slug);
      return next;
    });

  const enable = async (entry: CatalogEntry, repoIds: string[]) => {
    await onEnable(entry, repoIds);
    // Auto-expand so the freshly-enabled repos are visible, and arm the sweep-warn.
    setExpanded((s) => new Set(s).add(entry.slug));
    if (entry.target === "sweep" && entry.labels.length > 0) {
      setWarnTargets(
        repoIds.map((rid) => ({
          repoId: rid,
          repoPath: repos.find((r) => r.id === rid)?.path_with_namespace ?? "",
          labels: entry.labels,
        })),
      );
    }
  };

  return (
    <div className="space-y-4">
      {/* Repo-guardrail bar */}
      <div className="rounded-xl border border-edge bg-surface p-4">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div className="max-w-md">
            <h3 className="text-sm font-medium text-fg">Enable a default on your repos</h3>
            <p className="mt-0.5 text-[12.5px] text-muted">
              Pick the repos to enable on, then Enable a job below. Its prompt and labels are
              shipped and sealed; you own the cadence.
            </p>
          </div>
          <div className="w-full sm:w-72">
            <RepoMultiSelect repos={repos} selected={selectedRepos} onChange={setSelectedRepos} />
            <p className="mt-1.5 text-right text-[12px] text-faint">
              {selectedRepos.length === 0
                ? "Pick at least one repo to enable"
                : `${selectedRepos.length} repo${selectedRepos.length === 1 ? "" : "s"} → ${selectedRepos.length} schedule${selectedRepos.length === 1 ? "" : "s"}`}
            </p>
          </div>
        </div>
      </div>

      {/* Async enable/reset/run-now outcomes go through the shared Alert (role=alert for
          an error, role=status for a notice) so a screen reader announces them — the "My
          schedules" tab already does this; the hand-rolled divs here were silent. */}
      {error && <Alert message={error} />}
      {notice && <Alert message={notice} tone="info" />}

      {/* Any sweep-warns armed by the last enable (self-resolving). */}
      {warnTargets.length > 0 && (
        <div className="space-y-2">
          {warnTargets.map((w) => (
            <SweepLabelWarn
              key={`${w.repoId}-${w.labels.join(",")}`}
              repoId={w.repoId}
              repoPath={w.repoPath}
              labels={w.labels}
            />
          ))}
        </div>
      )}

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
            {catalog.entries.map((entry) => (
              <CatalogRow
                key={entry.slug}
                entry={entry}
                rows={rowsFor(entry.slug)}
                repos={repos}
                selectedRepos={selectedRepos}
                expanded={expanded.has(entry.slug)}
                busyId={busyId}
                onToggleExpand={() => toggleExpand(entry.slug)}
                onEnableSelected={(ids) => enable(entry, ids)}
                onEnableRepo={(repoId) => enable(entry, [repoId])}
                onTogglePause={onTogglePause}
                onRunNow={onRunNow}
                onReset={onReset}
                onClone={onClone}
                onRemove={onRemove}
                onEdit={onEdit}
              />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function CatalogRow({
  entry,
  rows,
  repos,
  selectedRepos,
  expanded,
  busyId,
  onToggleExpand,
  onEnableSelected,
  onEnableRepo,
  onTogglePause,
  onRunNow,
  onReset,
  onClone,
  onRemove,
  onEdit,
}: {
  entry: CatalogEntry;
  rows: Schedule[];
  repos: Repo[];
  selectedRepos: string[];
  expanded: boolean;
  busyId: string;
  onToggleExpand: () => void;
  onEnableSelected: (repoIds: string[]) => void;
  onEnableRepo: (repoId: string) => void;
  onTogglePause: (s: Schedule) => void;
  onRunNow: (s: Schedule) => void;
  onReset: (s: Schedule) => void;
  onClone: (s: Schedule) => void;
  onRemove: (s: Schedule) => void;
  onEdit: (s: Schedule) => void;
}) {
  const enabledCount = rows.length;
  const activeCount = rows.filter((r) => r.enabled).length;
  const anyCustomized = rows.some((r) => r.customized);
  const firedStamps = rows
    .map((r) => r.last_fired_at)
    .filter((d): d is string => !!d)
    .sort();
  const lastFired = firedStamps.length > 0 ? firedStamps[firedStamps.length - 1] : undefined;
  // The materialized repo ids for this slug — never offered a fresh enable.
  const materialized = new Set(rows.map((r) => r.repo_id));
  // The selected repos that would actually get a new enable — a repo already materialized
  // for this slug is a no-op, so the row Enable button only appears (and only fans out) for
  // this actionable subset.
  const actionable = selectedRepos.filter((id) => !materialized.has(id));

  // The Default-jobs variant re-adds its default-only chrome through the neutral shell's
  // slots: the 🔒 lock + type pill + customized badge as targetBadges (name + lock + pill
  // stay on one flex-wrap line, PRD #636 M5), the catalog When/Next/Last/Options cells,
  // and the Enable button as the leading action. The DEFAULT chip stays gone (M5).
  return (
    <ScheduleGroupRow
      name={entry.name}
      cols={COLS}
      expanded={expanded}
      onToggleExpand={onToggleExpand}
      disclosureId={`catalog-repos-${entry.slug}`}
      expandLabelName={entry.name}
      repoCount={enabledCount}
      showDisclosureCount={false}
      description={entry.description}
      targetBadges={
        <>
          {/* A self_improve entry has no baked prompt/labels/guidance, so it carries no
              sealed catalog text and skips the lock marker. */}
          {entry.target !== "self_improve" && (
            <span
              className="inline-flex items-center text-muted"
              title="Baked prompt — shipped and sealed"
              aria-label="Baked prompt, read-only"
            >
              <LockIcon />
            </span>
          )}
          {entry.target === "sweep" ? (
            <Badge tone="info" dot>
              sweep
            </Badge>
          ) : entry.target === "self_improve" ? (
            <Badge tone="ok" dot>
              self-improve
            </Badge>
          ) : (
            <Badge tone="ok" dot>
              prompt
            </Badge>
          )}
          {anyCustomized && <Badge tone="warning">customized</Badge>}
        </>
      }
      whenCell={
        <>
          <div className="font-mono text-[12.5px] text-fg">{entry.cron}</div>
          <div className="mt-0.5 text-[12px] text-muted">
            {humanizeCron(entry.cron)} · {entry.timezone}
          </div>
        </>
      }
      nextCell={
        enabledCount === 0 ? (
          <span className="text-[12.5px] text-faint">not enabled</span>
        ) : (
          <div className="flex flex-col gap-1">
            <Badge tone="ok">{enabledCount} repo{enabledCount === 1 ? "" : "s"}</Badge>
            {activeCount < enabledCount && (
              <span className="text-[11px] text-faint">{enabledCount - activeCount} paused</span>
            )}
          </div>
        )
      }
      lastCell={
        lastFired ? (
          <div className="text-[12.5px] text-muted">{formatStamp(lastFired)}</div>
        ) : (
          <span className="text-faint">— never fired</span>
        )
      }
      optionsCell={
        <div className="flex flex-wrap gap-1">
          {entry.model ? <Chip>model {entry.model}</Chip> : <Chip>inherit model</Chip>}
          {entry.target === "sweep" && entry.labels.map((l) => <Chip key={l}>label {l}</Chip>)}
          {entry.target === "sweep" && entry.max_issues > 0 && <Chip>max {entry.max_issues}</Chip>}
        </div>
      }
      leadingActions={
        actionable.length > 0 ? (
          <Button
            size="sm"
            disabled={busyId !== ""}
            onClick={() => onEnableSelected(actionable)}
            title={`Enable on ${actionable.length} repo${actionable.length === 1 ? "" : "s"}`}
          >
            <PlusIcon /> Enable
          </Button>
        ) : undefined
      }
    >
      {rows.map((s) => (
        <SubRow
          key={s.id}
          s={s}
          entry={entry}
          busy={busyId === s.id}
          onTogglePause={() => onTogglePause(s)}
          onRunNow={() => onRunNow(s)}
          onReset={() => onReset(s)}
          onClone={() => onClone(s)}
          onRemove={() => onRemove(s)}
          onEdit={() => onEdit(s)}
        />
      ))}
      <EnableAnotherRepo
        entry={entry}
        repos={repos}
        materialized={materialized}
        busy={busyId !== ""}
        onEnableRepo={onEnableRepo}
      />
    </ScheduleGroupRow>
  );
}

// SubRow — one per-repo production line under a default's summary (Layout A). A paused
// repo shows its resume toggle (never a fresh enable). Customized rows make Reset loud.
function SubRow({
  s,
  entry,
  busy,
  onTogglePause,
  onRunNow,
  onReset,
  onClone,
  onRemove,
  onEdit,
}: {
  s: Schedule;
  entry: CatalogEntry;
  busy: boolean;
  onTogglePause: () => void;
  onRunNow: () => void;
  onReset: () => void;
  onClone: () => void;
  onRemove: () => void;
  onEdit: () => void;
}) {
  const nextFire = s.next_fires[0] ?? s.next_fire_at;
  const [expanded, setExpanded] = useState(false);
  const panelId = `last-fire-${s.id}`;
  return (
    <ScheduleSubRow
      repoLabel={s.repo_path || s.repo_id}
      enabled={s.enabled}
      cronExpr={s.cron_expr}
      nextFire={nextFire}
      panelId={panelId}
      // Per-repo last-run parity with the standalone row (issue #690): the same three-way
      // fallback (outcome badge / bare stamp / never-fired), and the expandable detail
      // rendered below the flex row only while expanded.
      lastRun={
        s.last_fire ? (
          <LastRunOutcome
            fire={s.last_fire}
            expanded={expanded}
            onToggle={() => setExpanded((v) => !v)}
            panelId={panelId}
          />
        ) : s.last_fired_at ? (
          <div className="text-[12.5px] text-muted">{formatStamp(s.last_fired_at)}</div>
        ) : (
          <span className="text-faint">— never fired</span>
        )
      }
      lastRunDetail={s.last_fire && expanded ? <LastFireDetail s={s} fire={s.last_fire} /> : null}
      badges={s.customized ? <Badge tone="warning">customized</Badge> : null}
      leadingAction={
        // Reset is prominent for a customized row (restores the catalog cadence).
        s.customized ? (
          <Button variant="secondary" size="sm" disabled={busy} onClick={onReset} title="Reset to the catalog default">
            <RotateCcwIcon /> Reset
          </Button>
        ) : null
      }
      actions={
        <>
          <Button variant="ghost" size="sm" title="Run now" aria-label={`Run now on ${s.repo_path}`} disabled={busy} onClick={onRunNow}>
            <PlayIcon />
          </Button>
          <Button variant="ghost" size="sm" title="Edit settings" aria-label={`Edit ${entry.name} on ${s.repo_path}`} onClick={onEdit}>
            <PencilIcon />
          </Button>
          <Button variant="ghost" size="sm" title="Clone to an editable copy" aria-label={`Clone ${entry.name} on ${s.repo_path}`} disabled={busy} onClick={onClone}>
            <CopyIcon />
          </Button>
          {!s.customized && (
            <Button variant="ghost" size="sm" title="Reset to the catalog default" aria-label={`Reset ${entry.name} on ${s.repo_path}`} disabled={busy} onClick={onReset}>
              <RotateCcwIcon />
            </Button>
          )}
          <Button variant="ghost" size="sm" title="Remove" aria-label={`Remove ${entry.name} on ${s.repo_path}`} disabled={busy} onClick={onRemove}>
            <TrashIcon />
          </Button>
          {/* Already materialized: the resume/pause toggle is the affordance, not enable. */}
          <Toggle
            checked={s.enabled}
            onChange={onTogglePause}
            disabled={busy}
            label={s.enabled ? `Pause on ${s.repo_path}` : `Resume on ${s.repo_path}`}
          />
        </>
      }
    />
  );
}

// EnableAnotherRepo offers ONLY repos not yet materialized for this slug (the guardrail:
// a materialized repo gets a resume toggle, not a fresh enable).
function EnableAnotherRepo({
  entry,
  repos,
  materialized,
  busy,
  onEnableRepo,
}: {
  entry: CatalogEntry;
  repos: Repo[];
  materialized: Set<string>;
  busy: boolean;
  onEnableRepo: (repoId: string) => void;
}) {
  const available = repos.filter((r) => !materialized.has(r.id));
  const [repoId, setRepoId] = useState("");
  if (available.length === 0) {
    return (
      <p className="px-1 text-[12px] text-faint">Enabled on every available repo.</p>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-2 px-1 pt-1">
      <span className="text-[12px] text-muted">Enable on another repo:</span>
      <select
        aria-label={`Enable ${entry.name} on another repo`}
        value={repoId}
        onChange={(e) => setRepoId(e.target.value)}
        className="rounded-md border border-edge bg-raised px-2 py-1 font-mono text-[12px] text-fg outline-hidden focus:border-brand/70"
      >
        <option value="">Choose a repo…</option>
        {available.map((r) => (
          <option key={r.id} value={r.id}>
            {r.path_with_namespace}
          </option>
        ))}
      </select>
      <Button
        size="sm"
        variant="secondary"
        disabled={repoId === "" || busy}
        onClick={() => {
          if (repoId) {
            onEnableRepo(repoId);
            setRepoId("");
          }
        }}
      >
        <PlusIcon /> Enable
      </Button>
    </div>
  );
}

function Chip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted">
      {children}
    </span>
  );
}
