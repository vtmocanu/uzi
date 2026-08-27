// ScheduleGroupRow — the neutral, catalog-agnostic grouped summary + sub-row shell
// extracted from DefaultJobs' CatalogRow/SubRow (PRD #636 M3, Solution overview).
//
// Both the Default-jobs tab and the custom My-schedules tab render through this one
// component, so a custom sibling group looks like a default group minus the default-only
// chrome. The shell owns ONLY the neutral structure — the summary <tr> (a name, a
// repo-count, an expand/collapse toggle) and the expandable sub-rows region — and takes
// every catalog-vs-custom difference as a slot:
//
//   - the Default-jobs variant fills targetBadges with its 🔒 lock + type pill + a
//     "customized" badge, keeps its When/Next/Last/Options cells and its Enable button,
//     and renders its per-repo SubRows + "enable another repo" as children;
//   - the custom My-schedules variant passes a plain name, empty catalog cells, and
//     renders editable per-repo sub-rows + "add another repo" as children.
//
// Nothing here imports a CatalogEntry or hard-codes the DEFAULT chip / lock / a single
// cadence — siblings may have diverged, so per-repo cadence/target/next/last/options/on
// live in the SUB-ROWS, never the summary.

import { useState, type ReactNode } from "react";
import type { Repo } from "../lib/api";
import { humanizeCron } from "../lib/schedulePresets";
import { relativeFromNow } from "./ScheduleModal";
import { Badge, Button, cx } from "./ui";
import { ChevronDownIcon, PlusIcon } from "./icons";

export function ScheduleGroupRow({
  name,
  targetBadges,
  description,
  whenCell,
  nextCell,
  lastCell,
  optionsCell,
  leadingActions,
  repoCount,
  expanded,
  onToggleExpand,
  disclosureId,
  expandLabelName,
  cols,
  children,
}: {
  // The schedule/job name — the only text the neutral summary carries by itself.
  name: string;
  // Variant chrome shown after the name inside the Target flex container (default:
  // lock + type pill + customized badge; custom: none).
  targetBadges?: ReactNode;
  // The muted description line under the Target cell; omitted when absent.
  description?: ReactNode;
  // The catalog cells — kept for the default variant, empty for the custom variant
  // (siblings may have diverged, so the summary shows no single cadence/target).
  whenCell?: ReactNode;
  nextCell?: ReactNode;
  lastCell?: ReactNode;
  optionsCell?: ReactNode;
  // Actions rendered before the expand toggle in the "On" cell (default: Enable).
  leadingActions?: ReactNode;
  // The live member count; the expand toggle renders only when > 0.
  repoCount: number;
  expanded: boolean;
  onToggleExpand: () => void;
  // The id the expand toggle's aria-controls points at (unique per group).
  disclosureId: string;
  // Woven into the toggle's aria-label ("Show/Hide repos for <name>").
  expandLabelName: string;
  // The schedules table's colspan, so the expanded sub-rows row spans full width.
  cols: number;
  // The expanded region: per-repo sub-rows + the add/enable-another-repo affordance.
  children?: ReactNode;
}) {
  return (
    <>
      <tr className="border-t border-edge align-top">
        {/* Target — name + variant badges in one flex-wrap container, description after */}
        <td className="px-4 py-3">
          <div className="flex flex-wrap items-center gap-2 font-medium text-fg">
            <span>{name}</span>
            {targetBadges}
          </div>
          {description != null && (
            <div className="mt-0.5 max-w-md text-[12px] text-muted">{description}</div>
          )}
        </td>

        <td className="px-4 py-3">{whenCell}</td>
        <td className="px-4 py-3">{nextCell}</td>
        <td className="px-4 py-3">{lastCell}</td>
        <td className="px-4 py-3">{optionsCell}</td>

        {/* On + actions: variant leading actions, then the neutral expand toggle */}
        <td className="px-4 py-3">
          <div className="flex items-center justify-end gap-1.5">
            {leadingActions}
            {repoCount > 0 && (
              <button
                type="button"
                onClick={onToggleExpand}
                aria-expanded={expanded}
                aria-controls={disclosureId}
                aria-label={`${expanded ? "Hide" : "Show"} repos for ${expandLabelName}`}
                className="inline-flex items-center gap-1 rounded-md px-2 py-1 text-[12px] font-medium text-muted transition-colors hover:text-fg"
              >
                {repoCount} repo{repoCount === 1 ? "" : "s"}
                <ChevronDownIcon className={cx("h-3.5 w-3.5 transition-transform", expanded && "rotate-180")} />
              </button>
            )}
          </div>
        </td>
      </tr>

      {repoCount > 0 && expanded && (
        <tr className="border-t border-edge">
          {/* id pairs with the expand toggle's aria-controls; conditionally rendered, so
              the reference exists only while expanded — matching ScheduleRow's disclosure. */}
          <td id={disclosureId} colSpan={cols} className="bg-raised/30 px-4 pb-4 pt-1">
            <div className="space-y-2">{children}</div>
          </td>
        </tr>
      )}
    </>
  );
}

// ScheduleSubRow — one neutral per-repo production line under a group summary. The
// left block (repo label + badges + cron/next line) is shared; the variant supplies its
// own badges, an optional leading action (default's prominent Reset), and its action
// cluster (the ghost buttons + pause/resume toggle).
export function ScheduleSubRow({
  repoLabel,
  enabled,
  badges,
  cronExpr,
  nextFire,
  leadingAction,
  actions,
}: {
  // The repo path (or id fallback) shown mono in the sub-row head.
  repoLabel: string;
  enabled: boolean;
  // Variant badges before the neutral "paused" badge (default: customized).
  badges?: ReactNode;
  cronExpr: string;
  // The next fire instant for the enabled row; falsy hides the "next …" span.
  nextFire?: string | null;
  // An action rendered between the info block and the action cluster (default: Reset).
  leadingAction?: ReactNode;
  // The right-aligned action cluster (run-now / edit / remove / toggle …).
  actions: ReactNode;
}) {
  return (
    <div
      className={cx(
        "flex flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border border-edge bg-surface px-3 py-2.5",
        !enabled && "opacity-70",
      )}
    >
      <div className="min-w-[160px] flex-1">
        <div className="flex items-center gap-2">
          <span className="font-mono text-[12.5px] text-fg">{repoLabel}</span>
          {badges}
          {!enabled && <Badge tone="neutral">paused</Badge>}
        </div>
        <div className="mt-0.5 font-mono text-[11.5px] text-faint">
          {cronExpr} · {humanizeCron(cronExpr)}
          {enabled && nextFire && <span> · next {relativeFromNow(nextFire)}</span>}
        </div>
      </div>

      {leadingAction}

      <div className="flex items-center gap-1.5">{actions}</div>
    </div>
  );
}

// AddAnotherRepo — the custom-variant analog of DefaultJobs' EnableAnotherRepo. Offers
// ONLY owned repos not already carrying a sibling in this group (a duplicate would 409 on
// the server's partial unique index), and calls onAddRepo with the chosen repo id.
export function AddAnotherRepo({
  name,
  repos,
  taken,
  busy,
  disabledReason,
  onAddRepo,
}: {
  // Used only to disambiguate the picker's accessible label.
  name: string;
  repos: Repo[];
  // Repo ids already in the group (or the standalone row's own repo) — excluded.
  taken: Set<string>;
  busy: boolean;
  // When set, the whole picker (the <select> AND the Add button) is disabled and this
  // string is surfaced as a tooltip + short note — the affordance stays visible so the
  // reason is discoverable. Used to gate issue-target schedules, which can't span repos
  // (issue #638 P1c). Undefined = normally enabled.
  disabledReason?: string;
  onAddRepo: (repoId: string) => void;
}) {
  const available = repos.filter((r) => !taken.has(r.id));
  const [repoId, setRepoId] = useState("");
  // Blocked (e.g. an issue-target schedule that can't span repos, issue #638 P1c) is
  // checked BEFORE the empty-available case so a blocked group whose owner has no other
  // repos still surfaces the reason rather than a misleading "every repo" note. Rendered as
  // a plain note, not a disabled picker: a title tooltip is unreachable on a disabled
  // control, so there is no picker to attach a dead tooltip to.
  if (disabledReason != null) {
    return <p className="px-1 text-[12px] text-faint">{disabledReason}</p>;
  }
  if (available.length === 0) {
    return <p className="px-1 text-[12px] text-faint">Running on every available repo.</p>;
  }
  return (
    <div className="flex flex-wrap items-center gap-2 px-1 pt-1">
      <span className="text-[12px] text-muted">Add another repo:</span>
      <select
        aria-label={`Add ${name} on another repo`}
        value={repoId}
        onChange={(e) => setRepoId(e.target.value)}
        className="rounded-md border border-edge bg-raised px-2 py-1 font-mono text-[12px] text-fg outline-hidden focus:border-brand/70 disabled:opacity-50"
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
            onAddRepo(repoId);
            setRepoId("");
          }
        }}
      >
        <PlusIcon /> Add
      </Button>
    </div>
  );
}
