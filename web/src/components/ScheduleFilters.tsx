// ScheduleFilters — the filter bar above the Schedules tab's list (PRD #1645 D5, D11).
//
// A single-select chip group (All · From catalog · Mine · Paused) whose counts describe
// what each chip would show over ALL schedules, a Repo select offered only when the
// schedules span two or more repos, and a removable "Job: <name>" chip while a job filter
// is applied. Stateless: the page owns the filter state so it survives a tab switch.

import type { SourceFilter } from "../lib/scheduleList";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { Select, cx } from "./ui";

// The "All" chip's id: the page moves focus to it after Clear filters.
export const FILTER_ALL_ID = "schedule-filter-all";

const SOURCES: { key: SourceFilter; label: string }[] = [
  { key: "all", label: "All" },
  { key: "catalog", label: "From catalog" },
  { key: "mine", label: "Mine" },
  { key: "paused", label: "Paused" },
];

export function ScheduleFilters({
  source,
  counts,
  onSource,
  repos,
  repoId,
  onRepo,
  jobName,
  onClearJob,
}: {
  source: SourceFilter;
  counts: Record<SourceFilter, number>;
  onSource: (s: SourceFilter) => void;
  // The distinct repos the schedules span; the select renders only for two or more.
  repos: { id: string; path: string }[];
  repoId: string | null;
  onRepo: (id: string | null) => void;
  // The applied Job filter's display name, or null when no job filter is applied.
  jobName: string | null;
  onClearJob: () => void;
}) {
  const demo = useDemoMode();
  return (
    <div className="flex flex-wrap items-center gap-2">
      <div role="group" aria-label="Show schedules" className="flex flex-wrap gap-1.5">
        {SOURCES.map(({ key, label }) => {
          const on = source === key;
          return (
            <button
              key={key}
              id={key === "all" ? FILTER_ALL_ID : undefined}
              type="button"
              aria-pressed={on}
              onClick={() => onSource(key)}
              className={cx(
                "inline-flex h-7 items-center gap-1.5 rounded-full border px-3 text-xs transition-colors",
                on
                  ? "border-brand/50 bg-brand/10 font-medium text-fg"
                  : "border-edge text-muted hover:border-edge-strong hover:text-fg",
              )}
            >
              {label}
              <span className={cx("tabular-nums", on ? "text-brand" : "text-faint")}>{counts[key]}</span>
            </button>
          );
        })}
      </div>
      {jobName !== null && (
        <span className="inline-flex h-7 items-center gap-1 rounded-full border border-brand/50 bg-brand/10 pl-3 pr-1 text-xs text-fg">
          Job: {jobName}
          <button
            type="button"
            onClick={onClearJob}
            aria-label={`Remove the Job: ${jobName} filter`}
            className="inline-flex h-5 w-5 items-center justify-center rounded-full text-muted hover:bg-raised hover:text-fg"
          >
            <span aria-hidden="true">×</span>
          </button>
        </span>
      )}
      {repos.length >= 2 && (
        <Select
          aria-label="Repo"
          value={repoId ?? ""}
          onChange={(e) => onRepo(e.target.value === "" ? null : e.target.value)}
          className="w-full md:ml-auto md:w-auto"
        >
          <option value="">All repos</option>
          {repos.map((r) => (
            <option key={r.id} value={r.id}>
              {maskRepoPath(r.path, demo)}
            </option>
          ))}
        </Select>
      )}
    </div>
  );
}
