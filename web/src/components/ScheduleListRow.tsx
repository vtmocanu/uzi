// ScheduleListRow — one schedule on the Schedules tab (PRD #1645 D2, D4, D11).
//
// Every schedule, catalog-derived (origin "default") or user-authored, renders through
// this one row with the same columns and control positions: Target · repo, When, Next
// run, Last run, Options, Actions · On. Every value comes from the ROW itself (its own
// cron_expr, timezone, model, labels, ...), never from the catalog entry: a customized
// default shows the cadence it will actually fire on.
//
// One DOM serves both layouts. From md up it is a table row; below md (D11) the same
// <tr>/<td>s restyle into a stacked card (a two-column grid) so there is no horizontal
// scroll and no duplicate controls: name, badges and the toggle; repo and "from catalog";
// When and Next run; Last run; the action buttons; then the option chips. The toggle is
// rendered in exactly one place per layout (the name line on a card, the last column on a
// table row), so its DOM and focus order always match where it is drawn.

import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from "react";
import type { Repo, Schedule } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { humanizeCron } from "../lib/schedulePresets";
import { nextFireOf } from "../lib/scheduleList";
import { relativeFromNow } from "./ScheduleModal";
import { LastRunOutcome, LastFireDetail, formatStamp } from "./LastRun";
import { AddAnotherRepo } from "./AddAnotherRepo";
import { MoreActionsMenu, type MoreActionsItem } from "./MoreActionsMenu";
import { Badge, Button, Toggle, cx } from "./ui";
import { LockIcon, PencilIcon, PlayIcon, RotateCcwIcon } from "./icons";

// The table's column count, so a detail row spans the full width.
const SCHEDULE_COLS = 6;

// Why an issue-target schedule cannot be replicated onto another repo: the issue number is
// repo-relative, so the same iid points at a different (or missing) issue elsewhere. The
// API rejects add-repo for issue targets with a 422 (PRD #636 follow-up, issue #638 P1c).
const ISSUE_NO_MULTI_REPO = "Issue schedules can't span repos - issue numbers are repo-relative";

// Reset's scope, verbatim from D4 (it restores more than the cadence).
const RESET_TOOLTIP =
  "Restores all editable settings to the catalog defaults and clears your guidance, harness and token overrides. The shipped prompt and labels are unchanged.";

// The element a reveal (D12) scrolls to and focuses: the row's name block.
export const scheduleNameId = (id: string) => `schedule-name-${id}`;

// Mobile (below md) cell classes: each <td> is a grid item of the card; from md up it is a
// normal table cell. `full` spans both card columns.
const TD = "block px-4 py-1.5 md:table-cell md:py-3";
const FULL = "col-span-2";

// Tailwind's md breakpoint (48rem). Without matchMedia (tests, SSR) it reads as the table
// layout.
const MD_UP = "(min-width: 48rem)";
function subscribeMdUp(onChange: () => void) {
  if (typeof window === "undefined" || !window.matchMedia) return () => {};
  const mql = window.matchMedia(MD_UP);
  mql.addEventListener("change", onChange);
  return () => mql.removeEventListener("change", onChange);
}
const readMdUp = () => (typeof window === "undefined" || !window.matchMedia ? true : window.matchMedia(MD_UP).matches);

export function ScheduleListRow({
  s,
  name,
  busy,
  addBusy,
  clonedFrom,
  pauseNote,
  repos,
  onToggle,
  onRunNow,
  onEdit,
  onReset,
  onClone,
  onRemove,
  onAddRepo,
  onShowInCatalog,
  onEnableElsewhere,
}: {
  s: Schedule;
  // The display name (catalog entry name for a default row, target title for a user row).
  name: string;
  // This row's own operation is in flight.
  busy: boolean;
  // ANY row's operation is in flight: add-repo is globally gated (addRepo no-ops while
  // busy), so its picker must be disabled the whole time (issue #638 P2a).
  addBusy: boolean;
  // The source name when this row was cloned this session (session-scoped label).
  clonedFrom?: string;
  // While pause-all is active, the pre-formatted "paused until <stamp>" line; else null.
  pauseNote: string | null;
  // The owner's repos, for the add-another-repo picker.
  repos: Repo[];
  onToggle: () => void;
  onRunNow: () => void;
  onEdit: () => void;
  onReset: () => void;
  onClone: () => void;
  onRemove: () => void;
  onAddRepo: (repoId: string) => void;
  // Switch to the Job catalog tab and focus this default's entry. Absent when the row is
  // not a default or its slug has no catalog entry (nothing to show or enable from).
  onShowInCatalog?: () => void;
  // Switch to the Job catalog tab and open this default's enable dialog (D4 "Enable on
  // another repo"). Absent under the same conditions as onShowInCatalog.
  onEnableElsewhere?: () => void;
}) {
  const demo = useDemoMode();
  const isDefault = s.origin === "default";
  const off = !s.enabled;
  const fired = s.timing === "once" && s.status === "fired";
  const parked = s.status === "error";
  const repoLabel = s.repo_path ? maskRepoPath(s.repo_path, demo) : "repo unavailable";
  const nextFire = nextFireOf(s);
  // A self_improve schedule is always auto-approved (server-forced), so it is not a user
  // option: suppress the chip and let the "defaults" fallback show instead.
  const showApprove = s.auto_approve && s.target !== "self_improve";
  const [expanded, setExpanded] = useState(false);
  const [addingRepo, setAddingRepo] = useState(false);
  const addPanelRef = useRef<HTMLTableCellElement>(null);
  const menuButtonRef = useRef<HTMLButtonElement>(null);
  // The toggle moves between cells when the layout flips (see the header), which
  // unmounts the focused switch and drops focus to <body>. The subscription notes, BEFORE
  // the re-render, whether this row's switch held focus; the layout effect then focuses
  // the remounted one before paint.
  const rowRef = useRef<HTMLTableRowElement>(null);
  const refocusToggle = useRef(false);
  const subscribe = useCallback(
    (onChange: () => void) =>
      subscribeMdUp(() => {
        const active = document.activeElement;
        refocusToggle.current =
          active !== null && active.getAttribute("role") === "switch" && !!rowRef.current?.contains(active);
        onChange();
      }),
    [],
  );
  const mdUp = useSyncExternalStore(subscribe, readMdUp, () => true);
  useLayoutEffect(() => {
    if (!refocusToggle.current) return;
    refocusToggle.current = false;
    rowRef.current?.querySelector<HTMLElement>('[role="switch"]')?.focus();
  }, [mdUp]);

  // Closing the add-repo panel (Cancel or a submit) puts focus back on the More actions
  // button that opened it, rather than letting it fall to <body> with the panel. A
  // successful add then moves it on to the new row (D12); a 409 or error leaves it here.
  const closeAddPanel = () => {
    setAddingRepo(false);
    menuButtonRef.current?.focus();
  };

  // Opening the add-repo panel from the menu moves focus to its repo picker.
  useEffect(() => {
    if (addingRepo) addPanelRef.current?.querySelector<HTMLElement>("select, button")?.focus();
  }, [addingRepo]);

  const on = `${name} on ${repoLabel}`;
  const menuItems: MoreActionsItem[] = [
    { key: "clone", label: "Clone to an editable copy", disabled: busy, onSelect: onClone },
  ];
  if (isDefault) {
    if (onEnableElsewhere) menuItems.push({ key: "enable", label: "Enable on another repo", onSelect: onEnableElsewhere });
  } else {
    menuItems.push(
      s.target === "issue"
        ? { key: "add", label: "Add to another repo", disabled: true, description: ISSUE_NO_MULTI_REPO, onSelect: () => {} }
        : { key: "add", label: "Add to another repo", disabled: addBusy, onSelect: () => setAddingRepo(true) },
    );
  }
  menuItems.push({ key: "remove", label: "Remove", danger: true, disabled: busy, onSelect: onRemove });

  // The row's one toggle, placed by layout (see the header comment and D11).
  const toggle = (
    <Toggle
      checked={s.enabled}
      onChange={onToggle}
      disabled={busy}
      focusableWhenDisabled
      label={s.enabled ? `Pause ${on}` : `Resume ${on}`}
    />
  );

  return (
    <>
      <tr
        ref={rowRef}
        className={cx(
          "grid grid-cols-2 border-t border-edge py-2 align-middle md:table-row md:py-0",
          off && "opacity-60",
        )}
      >
        {/* Target · repo (and, on a phone card, the toggle at the end of the name line) */}
        <td className={cx(TD, FULL)}>
          <div className="flex items-start gap-3">
            <div className="min-w-0 flex-1">
              <div
                id={scheduleNameId(s.id)}
                tabIndex={-1}
                className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded font-medium text-fg outline-hidden focus-visible:ring-2 focus-visible:ring-brand/60"
              >
                <span>{name}</span>
                {isDefault && s.target !== "self_improve" && (
                  <span
                    className="inline-flex items-center text-muted"
                    title="Baked prompt — shipped and sealed"
                    aria-label="Baked prompt, read-only"
                  >
                    <LockIcon />
                  </span>
                )}
                <KindPill s={s} />
                {!isDefault && s.target === "prompt" && s.output_mode && (
                  <Badge tone="neutral">{s.output_mode}</Badge>
                )}
                {!isDefault && s.timing === "once" && (
                  <Badge tone="brand" dot>
                    once
                  </Badge>
                )}
                {parked && (
                  <Badge tone="danger" dot>
                    parked
                  </Badge>
                )}
                {s.customized && <Badge tone="warning">customized</Badge>}
                {clonedFrom && (
                  <Badge tone="neutral" title={`Cloned from ${clonedFrom}`}>
                    cloned from {clonedFrom}
                  </Badge>
                )}
              </div>
              <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-[12px]">
                <span className="break-all font-mono text-faint">
                  {repoLabel}
                  {s.target === "prompt" && " · no issue"}
                </span>
                {isDefault && onShowInCatalog && (
                  <button
                    type="button"
                    onClick={onShowInCatalog}
                    aria-label={`from catalog: ${name}`}
                    className="rounded text-brand underline-offset-2 hover:underline"
                  >
                    from catalog
                  </button>
                )}
              </div>
            </div>
            {!mdUp && toggle}
          </div>
        </td>

        {/* When: the row's OWN cadence and zone, never the catalog's */}
        <td className={TD}>
          <MobileLabel>When</MobileLabel>
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
        <td className={TD}>
          <MobileLabel>Next run</MobileLabel>
          {parked ? (
            <Badge tone="danger">error</Badge>
          ) : off ? (
            <Badge tone="neutral">paused</Badge>
          ) : fired ? (
            <Badge tone="neutral">fired</Badge>
          ) : nextFire ? (
            <div className="text-[12.5px] text-muted">
              {formatStamp(nextFire)}
              {/* Pause-all replaces the relative line where the row would otherwise fire. */}
              {pauseNote ? (
                <div className="text-[11px] text-warn">{pauseNote}</div>
              ) : (
                <div className="text-[11px] text-faint">{relativeFromNow(nextFire)}</div>
              )}
            </div>
          ) : (
            <span className="text-faint">—</span>
          )}
        </td>

        {/* Last run */}
        <td className={cx(TD, FULL)}>
          <MobileLabel>Last run</MobileLabel>
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

        {/* Options: the row's own effective values (after the actions on a phone card) */}
        <td className={cx(TD, FULL, "order-last md:order-none")}>
          <div className="flex flex-wrap gap-1">
            {isDefault ? <DefaultOptions s={s} showApprove={showApprove} /> : <UserOptions s={s} showApprove={showApprove} />}
          </div>
        </td>

        {/* Actions · On */}
        <td className={cx(TD, FULL)}>
          <div className="flex flex-wrap items-center gap-1.5 md:flex-nowrap md:justify-end">
            {/* aria-disabled rather than disabled while busy: a browser blurs a button that
                becomes disabled, which would drop a keyboard user's focus to <body>. */}
            <Button
              variant="ghost"
              size="sm"
              title="Run now"
              aria-label={`Run now: ${on}`}
              aria-disabled={busy || undefined}
              onClick={() => {
                if (!busy) onRunNow();
              }}
              className="aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
            >
              <PlayIcon />
            </Button>
            <Button variant="ghost" size="sm" title="Edit" aria-label={`Edit ${on}`} onClick={onEdit}>
              <PencilIcon />
            </Button>
            {isDefault && s.customized && (
              <button
                type="button"
                title={RESET_TOOLTIP}
                aria-label={`Reset ${on} to catalog defaults`}
                disabled={busy}
                onClick={onReset}
                className="inline-flex h-7 shrink-0 items-center gap-1 rounded-lg border border-warn/40 bg-warn/10 px-2 text-xs font-medium text-warn transition-colors hover:bg-warn/20 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <RotateCcwIcon /> Reset
              </button>
            )}
            <MoreActionsMenu label={`More actions for ${on}`} items={menuItems} buttonRef={menuButtonRef} />
            {mdUp && toggle}
          </div>
        </td>
      </tr>

      {addingRepo && (
        <tr className="block border-t border-edge md:table-row">
          <td
            ref={addPanelRef}
            id={`add-repo-${s.id}`}
            colSpan={SCHEDULE_COLS}
            className="block bg-raised/30 px-4 pb-4 pt-2 md:table-cell"
          >
            <div className="flex flex-wrap items-center gap-2">
              <AddAnotherRepo
                name={name}
                repos={repos}
                taken={new Set([s.repo_id])}
                busy={addBusy}
                onAddRepo={(repoId) => {
                  onAddRepo(repoId);
                  closeAddPanel();
                }}
              />
              <Button variant="ghost" size="sm" onClick={closeAddPanel}>
                Cancel
              </Button>
            </div>
          </td>
        </tr>
      )}
      {s.last_fire && expanded && (
        <tr className="block border-t border-edge md:table-row">
          {/* The id pairs with the disclosure's aria-controls; rendered only while expanded. */}
          <td id={`last-fire-${s.id}`} colSpan={SCHEDULE_COLS} className="block bg-raised/30 px-4 pb-4 pt-0 md:table-cell">
            <LastFireDetail s={s} fire={s.last_fire} />
          </td>
        </tr>
      )}
    </>
  );
}

function MobileLabel({ children }: { children: React.ReactNode }) {
  return <div className="mb-0.5 text-[11px] text-faint md:hidden">{children}</div>;
}

function KindPill({ s }: { s: Schedule }) {
  if (s.target === "sweep")
    return (
      <Badge tone="info" dot>
        sweep
      </Badge>
    );
  if (s.target === "prompt")
    return (
      <Badge tone="ok" dot>
        prompt
      </Badge>
    );
  if (s.target === "self_improve")
    return (
      <Badge tone="ok" dot>
        self-improve
      </Badge>
    );
  return null;
}

// A default row's chips (D4): model or "inherit model", the sealed sweep labels, max
// issues, then the run flags, all read from the row's own effective values.
function DefaultOptions({ s, showApprove }: { s: Schedule; showApprove: boolean }) {
  return (
    <>
      <OptionChip>{s.model ? `model ${s.model}` : "inherit model"}</OptionChip>
      {s.target === "sweep" && s.labels?.map((l) => <OptionChip key={l}>label {l}</OptionChip>)}
      {s.target === "sweep" && s.max_issues != null && s.max_issues > 0 && <OptionChip>max {s.max_issues}</OptionChip>}
      {s.wait_on_limit && <OptionChip>wait-on-limit</OptionChip>}
      {showApprove && <OptionChip>auto-approve</OptionChip>}
      {s.mr_rework_enabled != null && <OptionChip>mr-rework: {s.mr_rework_enabled ? "on" : "off"}</OptionChip>}
    </>
  );
}

// A user row keeps today's chips: only explicit run flags earn one (PRD #841: a null
// mr-rework is inherit and shows nothing), with "defaults" when none apply.
function UserOptions({ s, showApprove }: { s: Schedule; showApprove: boolean }) {
  return (
    <>
      {s.wait_on_limit && <OptionChip>wait-on-limit</OptionChip>}
      {showApprove && <OptionChip>auto-approve</OptionChip>}
      {s.mr_rework_enabled != null && <OptionChip>mr-rework: {s.mr_rework_enabled ? "on" : "off"}</OptionChip>}
      {!s.wait_on_limit && !showApprove && s.mr_rework_enabled == null && (
        <span className="text-[12px] text-faint">defaults</span>
      )}
    </>
  );
}

function OptionChip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted">
      {children}
    </span>
  );
}
