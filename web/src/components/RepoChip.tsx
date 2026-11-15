// The Repos chip for the new Overview header (/dashboard) (issue #4334, admins only; the page decides
// whether to mount it). A compact "N repos active of M" trigger with a badge stack opens a
// popover listing which repos have listed active work, which ran in the last week and how
// many are idle. All figures come from the pure repoActivityModel; this component only draws
// it. The shared chip hook handles focus and dismissal; this chip keeps its own desktop
// left-alignment decision for a wrapped header.

import { useLayoutEffect, useState } from "react";
import { Link } from "react-router-dom";
import { repoIdleText, type ActiveRepo, type RepoActivityModel } from "../lib/overviewRepos";
import { cx } from "./ui";
import { ChipPopoverPanel } from "./ChipPopoverPanel";
import { useChipPopover } from "./useChipPopover";

const INFO_TEXT =
  "Active counts repos with a listed non-terminal run: chat, judge and cross-check runs are not listed, and the list stops at 500 runs. Last run includes every run kind.";

const TONE_CLASS = {
  ok: "bg-ok/15 text-ok",
  warn: "bg-warn/15 text-warn",
  neutral: "border border-edge bg-surface text-muted",
} as const;

// Square where the People chip's avatar is round, so the two chips read as different kinds.
function Badge({ text, small = false }: { text: string; small?: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "inline-flex shrink-0 items-center justify-center rounded-[5px] border border-edge bg-raised font-semibold text-fg",
        small ? "h-5 w-5 text-[9px] ring-1 ring-surface" : "h-6 w-6 text-[10px]",
      )}
    >
      {text}
    </span>
  );
}

function GroupLabel({ children }: { children: string }) {
  return <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-faint">{children}</p>;
}

type Active = RepoActivityModel["active"];

// The count wording always uses the plural "repos", including for 1.
function chipText(active: Active, total: number | null) {
  if (active.state === "unavailable") return { main: "Repos active unavailable", of: null };
  if (active.state === "loading") return { main: "", of: null };
  return {
    main: `${active.atLeast ? "at least " : ""}${active.count} repos active`,
    of: total != null ? `of ${total}` : null,
  };
}

function nameText(active: Active, total: number | null): string {
  if (active.state === "unavailable") return "Repos: active now unavailable";
  if (active.state === "loading") return "";
  const lead = active.atLeast ? "at least " : "";
  return `Repos: ${lead}${active.count} active now${total != null ? ` of ${total}` : ""}`;
}

function headerText(active: Active, total: number | null): string {
  if (active.state === "unavailable") return "Repos active unavailable";
  if (active.state === "loading") return "";
  const lead = active.atLeast ? "At least " : "";
  if (total != null) return `${lead}${active.count} of ${total} repos active now`;
  return `${lead}${active.count} repos active now`;
}

export function RepoChip({ model }: { model: RepoActivityModel }) {
  const { open, setOpen, hostRef, panelRef, triggerRef, panelId } = useChipPopover();
  const [alignLeft, setAlignLeft] = useState(false);

  // Decided on open: a right-aligned panel needs the trigger's right edge to leave room for the
  // panel width plus a 16px margin; otherwise align to the trigger's left edge.
  useLayoutEffect(() => {
    if (!open || !triggerRef.current) return;
    const panelWidth = Math.min(360, window.innerWidth - 32);
    setAlignLeft(triggerRef.current.getBoundingClientRect().right < panelWidth + 16);
  }, [open, triggerRef]);

  const { active, total, recent, idle, listFailed } = model;
  if (active.state === "loading") return null;

  const text = chipText(active, total);
  const repos: ActiveRepo[] = active.state === "ok" ? active.repos : [];
  const stack = repos.slice(0, 2);
  const extra = repos.length - stack.length;
  const idleLine = idle ? repoIdleText(idle) : null;

  return (
    <div ref={hostRef} className="relative inline-block min-w-0 max-w-full">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={nameText(active, total)}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
      >
        {stack.length > 0 && (
          <span aria-hidden="true" className="flex items-center -space-x-0.5">
            {stack.map((r) => (
              <Badge key={r.key} text={r.badge} small />
            ))}
            {extra > 0 && (
              <span className="inline-flex h-5 min-w-5 items-center justify-center rounded-[5px] border border-edge bg-raised px-1 text-[9px] font-semibold text-muted ring-1 ring-surface">
                +{extra}
              </span>
            )}
          </span>
        )}
        <span className="min-w-0 truncate">
          {active.state === "unavailable" ? (
            <span className="text-muted">{text.main}</span>
          ) : (
            <>
              <b className="font-semibold">{text.main}</b>
              {text.of && <span className="text-muted"> {text.of}</span>}
            </>
          )}
        </span>
      </button>
      {open && (
        <ChipPopoverPanel
          id={panelId}
          panelRef={panelRef}
          label="Repos in uzi"
          alignment={alignLeft ? "left" : "right"}
        >
          <div className="space-y-3 p-3">
            <div className="flex items-start justify-between gap-2">
              <h3 className="text-sm font-semibold text-fg">{headerText(active, total)}</h3>
              {/* Drawn, not a glyph: the bundled fonts lack U+24D8. */}
              <span
                className="inline-flex h-4 w-4 shrink-0 cursor-help items-center justify-center rounded-full border border-edge text-[10px] font-semibold leading-none text-faint"
                title={INFO_TEXT}
                aria-hidden="true"
              >
                i
              </span>
              <span className="sr-only">{INFO_TEXT}</span>
            </div>

            <div>
              <GroupLabel>Active now</GroupLabel>
              {active.state === "unavailable" ? (
                <p className="text-sm text-muted">The admin run list could not be read.</p>
              ) : repos.length === 0 ? (
                <p className="text-sm text-muted">No repo has listed active work.</p>
              ) : (
                <ul className="space-y-1.5">
                  {repos.map((r) => (
                    // The path keeps at least 8rem; a long pill wraps inside the rest instead of
                    // squeezing the path to nothing.
                    <li key={r.key} className="grid grid-cols-[auto_minmax(8rem,1fr)_auto] items-center gap-2 text-sm">
                      <Badge text={r.badge} />
                      <span className="truncate font-mono text-xs text-fg" title={r.path}>
                        {r.path}
                      </span>
                      <span className={cx("rounded-xl px-2 py-0.5 text-right text-xs", TONE_CLASS[r.tone])}>{r.pill}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {listFailed && <p className="text-sm text-muted">The repo list could not be read.</p>}
            {!listFailed && active.state === "ok" && active.atLeast && total !== null && (
              <p className="text-sm text-muted">
                Recent and idle repos are hidden: the run list stops at 500 runs.
              </p>
            )}

            {recent != null && recent.length > 0 && (
              <div>
                <GroupLabel>Run within 7 days, nothing active</GroupLabel>
                <ul className="space-y-1.5">
                  {recent.map((r) => (
                    <li key={r.key} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
                      <Badge text={r.badge} />
                      <span className="truncate font-mono text-xs text-fg" title={r.path}>
                        {r.path}
                      </span>
                      <span className="text-xs text-muted">{r.ago}</span>
                    </li>
                  ))}
                </ul>
              </div>
            )}

            {idleLine != null && (
              <div>
                <GroupLabel>Idle</GroupLabel>
                <p className="text-sm text-muted">{idleLine}</p>
              </div>
            )}
          </div>

          <div className="sticky bottom-0 flex items-center justify-end border-t border-edge bg-raised px-3 py-2 text-xs">
            <Link to="/repos" className="font-medium text-brand hover:text-brand-hover">
              Your repos
            </Link>
          </div>
        </ChipPopoverPanel>
      )}
    </div>
  );
}
