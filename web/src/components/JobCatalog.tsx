// JobCatalog — the Job catalog tab of the Schedules page (PRD #1645 D7; replaces the
// PRD #589 Default jobs table).
//
// One card per shipped catalog entry, for discovery: what the job does, its catalog
// default cadence, its options, where it already runs and an "Enable on…" picker scoped
// to that entry (EnableJobDialog). No per-repo controls live here: once enabled, a job is
// operated from the Schedules tab, which the card's status link opens filtered to it.

import type { CatalogEntry, Repo, Schedule, ScheduleCatalog } from "../lib/api";
import { humanizeCron } from "../lib/schedulePresets";
import { jobEnablement } from "../lib/scheduleList";
import { EnableJobDialog, type EnableResult } from "./EnableJobDialog";
import { Badge } from "./ui";
import { LockIcon } from "./icons";

// The DOM id of a catalog entry's card, so the Schedules tab's "from catalog" link can move
// focus to it (PRD #1645 D4).
export const catalogEntryId = (slug: string) => `catalog-entry-${slug}`;

export function JobCatalog({
  catalog,
  schedules,
  repos,
  busy,
  openSlug,
  onOpenSlug,
  onEnable,
  onShowJob,
}: {
  catalog: ScheduleCatalog;
  // All the owner's schedules; the origin-default rows are where each entry runs.
  schedules: Schedule[];
  repos: Repo[];
  // A page mutation is in flight: every card's Enable waits.
  busy: boolean;
  // The entry whose enable dialog is open (at most one), owned by the page so the
  // Schedules tab's "Enable on another repo" can open it.
  openSlug: string | null;
  onOpenSlug: (slug: string | null) => void;
  onEnable: (entry: CatalogEntry, repoIds: string[]) => Promise<EnableResult | null>;
  // Switch to the Schedules tab filtered to this job (D5 Job filter).
  onShowJob: (slug: string) => void;
}) {
  if (catalog.entries.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-edge p-10 text-center">
        <p className="mx-auto max-w-sm text-sm text-muted">
          No shipped jobs are available on this instance yet. Create your own with New schedule.
        </p>
      </div>
    );
  }
  return (
    <div className="space-y-4">
      <p className="max-w-2xl text-sm text-muted">
        Shipped jobs. Their prompt and labels are sealed and improve with each release; you pick
        the repos and own the cadence.
      </p>
      <ul className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
        {catalog.entries.map((entry) => (
          <JobCard
            key={entry.slug}
            entry={entry}
            schedules={schedules}
            repos={repos}
            busy={busy}
            open={openSlug === entry.slug}
            onOpenChange={(open) => onOpenSlug(open ? entry.slug : null)}
            onEnable={(ids) => onEnable(entry, ids)}
            onShowJob={() => onShowJob(entry.slug)}
          />
        ))}
      </ul>
    </div>
  );
}

function JobCard({
  entry,
  schedules,
  repos,
  busy,
  open,
  onOpenChange,
  onEnable,
  onShowJob,
}: {
  entry: CatalogEntry;
  schedules: Schedule[];
  repos: Repo[];
  busy: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onEnable: (repoIds: string[]) => Promise<EnableResult | null>;
  onShowJob: () => void;
}) {
  const { repoIds, pausedRepos } = jobEnablement(schedules, entry.slug);
  const n = repoIds.size;
  const assigned = entry.selector_kind === "assigned";
  return (
    <li
      id={catalogEntryId(entry.slug)}
      tabIndex={-1}
      className="flex flex-col rounded-xl border border-edge bg-surface p-4 outline-hidden focus-visible:ring-2 focus-visible:ring-brand/60"
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <h3 className="text-sm font-semibold text-fg">{entry.name}</h3>
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
      </div>
      <p className="mt-1.5 text-[12.5px] leading-relaxed text-muted">{entry.description}</p>

      <p className="mt-3 text-[12.5px] text-fg">
        {humanizeCron(entry.cron)}
        <span className="text-muted"> · {entry.timezone}</span>
        <span className="text-faint"> · catalog default</span>
      </p>

      <div className="mb-3 mt-2 flex flex-wrap gap-1">
        {entry.model ? <Chip>model {entry.model}</Chip> : <Chip>inherit model</Chip>}
        {entry.target === "sweep" && assigned && <Chip>assigned to uzi</Chip>}
        {entry.target === "sweep" && !assigned && entry.labels?.map((l) => <Chip key={l}>label {l}</Chip>)}
        {entry.target === "sweep" && entry.max_issues > 0 && <Chip>max {entry.max_issues}</Chip>}
      </div>

      {/* The footer sits at the card's bottom edge so a row of cards lines up. */}
      <div className="mt-auto flex flex-wrap items-center justify-between gap-2 border-t border-edge pt-3">
        {n === 0 ? (
          <span className="text-[12.5px] text-faint">Not enabled</span>
        ) : (
          <button
            type="button"
            onClick={onShowJob}
            className="rounded text-left text-[12.5px] text-brand underline-offset-2 hover:underline"
          >
            Enabled on {n} repo{n === 1 ? "" : "s"}
            {pausedRepos > 0 && `, ${pausedRepos} paused`}
          </button>
        )}
        <EnableJobDialog
          entry={entry}
          repos={repos}
          enabledRepoIds={repoIds}
          busy={busy}
          open={open}
          onOpenChange={onOpenChange}
          onEnable={onEnable}
        />
      </div>
    </li>
  );
}

function Chip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted">
      {children}
    </span>
  );
}
