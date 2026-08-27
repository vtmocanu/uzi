// RepoMultiSelect — a small multi-select repo picker (PRD #589 M6).
//
// Enabling a default on N repos is a client-side fan-out (one enableCatalogSchedule
// call per repo, matching the CLI), so the Default-jobs guardrail bar needs a compact
// way to pick several repos at once. There was no reusable picker, so this is it: a
// native <details> disclosure (keyboard-accessible, no click-away wiring) over a
// checkbox list, with the current selection shown as removable chips.

import type { Repo } from "../lib/api";
import { cx } from "./ui";
import { XIcon } from "./icons";

export function RepoMultiSelect({
  repos,
  selected,
  onChange,
  // Repos that cannot be picked here (e.g. already materialized for this slug), shown
  // disabled with a reason, so the caller can offer "only repos not yet enabled".
  disabledIds = [],
  disabledReason,
  label = "Repos",
  id = "repo-multiselect",
}: {
  repos: Repo[];
  selected: string[];
  onChange: (ids: string[]) => void;
  disabledIds?: string[];
  disabledReason?: string;
  label?: string;
  id?: string;
}) {
  const toggle = (repoId: string) => {
    onChange(
      selected.includes(repoId) ? selected.filter((r) => r !== repoId) : [...selected, repoId],
    );
  };
  const selectedRepos = selected
    .map((rid) => repos.find((r) => r.id === rid))
    .filter((r): r is Repo => !!r);

  return (
    <div className="space-y-2">
      <details className="group rounded-lg border border-edge bg-raised">
        <summary
          className="flex cursor-pointer list-none items-center justify-between gap-2 px-3 py-2 text-sm text-fg outline-hidden focus-visible:ring-2 focus-visible:ring-brand/60"
          aria-label={`${label} — ${selected.length} selected`}
        >
          <span className="font-medium text-muted">{label}</span>
          <span className="text-[12px] text-faint">
            {selected.length === 0 ? "none selected" : `${selected.length} selected`}
          </span>
        </summary>
        <div className="max-h-56 overflow-y-auto border-t border-edge p-1.5">
          {repos.length === 0 ? (
            <p className="px-2 py-2 text-[12.5px] text-faint">No repos available.</p>
          ) : (
            <ul className="space-y-0.5">
              {repos.map((r) => {
                const isDisabled = disabledIds.includes(r.id);
                const checked = selected.includes(r.id);
                return (
                  <li key={r.id}>
                    <label
                      className={cx(
                        "flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm",
                        isDisabled
                          ? "cursor-not-allowed opacity-55"
                          : "cursor-pointer hover:bg-surface",
                      )}
                      title={isDisabled ? disabledReason : undefined}
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        disabled={isDisabled}
                        onChange={() => toggle(r.id)}
                        aria-label={r.path_with_namespace}
                        className="h-4 w-4 shrink-0 accent-brand"
                      />
                      <span className="truncate font-mono text-[12.5px] text-fg">
                        {r.path_with_namespace}
                      </span>
                      {isDisabled && disabledReason && (
                        <span className="ml-auto shrink-0 text-[11px] text-faint">{disabledReason}</span>
                      )}
                    </label>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </details>

      {selectedRepos.length > 0 && (
        <div className="flex flex-wrap gap-1.5" data-testid={`${id}-chips`}>
          {selectedRepos.map((r) => (
            <span
              key={r.id}
              className="inline-flex items-center gap-1 rounded-md border border-brand/40 bg-brand/10 px-2 py-0.5 font-mono text-[11px] text-brand"
            >
              {r.path_with_namespace}
              <button
                type="button"
                aria-label={`Remove ${r.path_with_namespace}`}
                onClick={() => toggle(r.id)}
                className="text-brand/70 hover:text-brand"
              >
                <XIcon />
              </button>
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
