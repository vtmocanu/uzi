import { cx } from "../../components/ui";

// IssuesFilter is the board toolbar's "Issues" popover (PRD #764): a simple
// `uzi`-only / all toggle. By default the board renders only `uzi`-labelled (runnable)
// issues; ticking "Show all other issues" reveals every open issue alongside them.
// Lives in its own file under pages/board/ (PRD #1007 M3), like the other board
// clusters, since it is board-toolbar-only. The `show_all` state is persisted per-user
// via board_prefs.
//
// Open/close, Escape and outside-click are owned by Board() (the `open`/`popRef`
// props) so the same effect can also drive other toolbar state if needed and so the
// listeners attach once.
export function IssuesFilter({
  open,
  onToggleOpen,
  popRef,
  triggerRef,
  uziLabel,
  showAll,
  showAllCount,
  onToggleShowAll,
}: {
  open: boolean;
  onToggleOpen: () => void;
  popRef: React.RefObject<HTMLDivElement | null>;
  triggerRef: React.RefObject<HTMLButtonElement | null>;
  uziLabel: string;
  showAll: boolean;
  showAllCount: number;
  onToggleShowAll: () => void;
}) {
  return (
    <div className="relative" ref={popRef}>
      <button
        ref={triggerRef}
        type="button"
        onClick={onToggleOpen}
        aria-haspopup="true"
        aria-expanded={open}
        // Brand border when all issues are shown, so the toolbar shows at a glance that
        // the board is widened beyond the runnable `uzi` set.
        className={cx(
          "inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs transition-colors",
          showAll ? "border-brand/60 text-fg" : "border-edge text-muted hover:text-fg",
        )}
      >
        <span className="text-muted">Issues:</span>
        {/* Names the CONFIGURED `uzi` label — uzi_label is renameable, so a literal
            would be wrong on any instance that changed it. */}
        <span className="font-medium text-fg">{showAll ? "all" : uziLabel}</span>
        <span aria-hidden="true" className="text-[9px] text-faint">
          ▾
        </span>
      </button>
      {open && (
        <div
          role="group"
          aria-label="Board issue labels"
          className="absolute right-0 z-20 mt-1 w-72 rounded-lg border border-edge-strong bg-surface p-1.5 shadow-2xl"
        >
          <div className="px-2 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-faint">
            Show issues labelled
          </div>
          {/* The `uzi` row: pinned, checked and disabled — the runnable set is always
              shown (PRD #764). */}
          <div className="flex items-center gap-2 rounded-md px-2 py-1 text-xs text-muted">
            <input
              type="checkbox"
              checked
              disabled
              aria-label={`${uziLabel} (always shown)`}
              className="h-3.5 w-3.5 rounded border-edge accent-brand"
            />
            {uziLabel}
            <span className="ml-auto text-[11px] text-faint">always shown</span>
          </div>
          <hr className="my-1.5 border-edge" />
          {/* The escape hatch: reveal every other open issue alongside the runnable set. */}
          <label className="flex cursor-pointer select-none items-center gap-2 rounded-md px-2 py-1 text-xs text-fg hover:bg-raised">
            <input
              type="checkbox"
              checked={showAll}
              onChange={onToggleShowAll}
              className="h-3.5 w-3.5 rounded border-edge accent-brand"
            />
            Show all other issues
            <span className="ml-auto font-mono text-[11px] text-faint">{showAllCount}</span>
          </label>
        </div>
      )}
    </div>
  );
}
