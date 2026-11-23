// One cell of the Models grid (PRD #3263): a family's control for one row, its saved
// prediction, an optional note, reason, Not used reason and field error, each wired to the
// control through aria-describedby. The prediction line is the grid's signature: it always says
// what is SAVED ("Saved effective value: …"), and a draft that differs from it marks the cell
// Unsaved with a brand rail and border rather than pretending to predict the unsaved edit.
//
// A cell the draft routing leaves unused (the Codex judge model while the judge runs on Claude)
// is DIMMED, never disabled: its control drops its fill and takes a dashed border (the value
// stays at AA contrast) until it is hovered or holds focus, it keeps its stored and draft values,
// and it is validated and saved like any other.

import { useLayoutEffect, useRef, type ReactNode } from "react";
import type { ModelsField } from "../../lib/modelsDraft";
import { cx } from "../ui";
import { harnessName } from "./modelsLabels";

export interface ModelsCellIds {
  control: string;
  describedBy: string;
}

export function ModelsCell({
  field,
  idBase,
  dirty,
  prediction,
  reason,
  note,
  notUsed,
  storedWarning,
  error,
  wide = false,
  children,
}: {
  field: ModelsField;
  idBase: string;
  dirty: boolean;
  /** The saved prediction, or null when it is unavailable. */
  prediction: string | null;
  reason?: string | null;
  /** Neutral context for the control (what a routing choice does, where a toggle lives). */
  note?: ReactNode;
  /** Why the draft leaves this cell unused; dims the control and is read as its description. */
  notUsed?: string | null;
  /** A problem with the SAVED value (a legacy id the validator now refuses): information only,
   *  so it never marks the control invalid or blocks Save. */
  storedWarning?: string | null;
  error?: string | null;
  /** Spans both family columns (a row with one control). */
  wide?: boolean;
  children: (ids: ModelsCellIds) => ReactNode;
}) {
  const control = `${idBase}-control`;
  const predictionId = `${idBase}-saved`;
  const reasonId = `${idBase}-reason`;
  const errorId = `${idBase}-error`;
  const noteId = `${idBase}-note`;
  const notUsedId = `${idBase}-not-used`;
  const storedId = `${idBase}-stored-warning`;
  const describedBy = [predictionId, notUsed ? notUsedId : "", note ? noteId : "", reason ? reasonId : "", storedWarning ? storedId : "", error ? errorId : ""]
    .filter(Boolean)
    .join(" ");
  const family = field.family ? harnessName(field.family) : null;
  const root = useRef<HTMLDivElement>(null);
  // An errored cell marks its controls aria-invalid. ModelSelect and EffortSelect are shared
  // pickers with no aria-invalid prop, so the cell sets it on its own select/input; it runs
  // after every render because the custom model ID box mounts and unmounts.
  useLayoutEffect(() => {
    root.current?.querySelectorAll("select, input").forEach((el) => {
      if (error) el.setAttribute("aria-invalid", "true");
      else el.removeAttribute("aria-invalid");
    });
  });
  return (
    <div
      ref={root}
      data-field={field.key}
      data-dirty={dirty ? "true" : undefined}
      data-dimmed={notUsed ? "true" : undefined}
      className={cx(
        // scroll-mb keeps a keyboard-focused control clear of the sticky Save bar.
        "min-w-0 space-y-1.5 border-l-2 pl-3 transition-colors motion-reduce:transition-none [&_input]:scroll-mb-40 [&_select]:scroll-mb-40",
        // Brand orange means Unsaved here, so focus takes the foreground tone instead (border and
        // keyboard ring), and a focused unsaved control never reads as one signal. fg is the
        // text colour, so the ring keeps well over 3:1 against the page in every theme.
        "[&_input:focus]:border-fg/60 [&_select:focus]:border-fg/60 [&_input:focus-visible]:ring-fg [&_select:focus-visible]:ring-fg",
        // An error outranks the Unsaved accent: one rail colour per cell, never two competing.
        error
          ? "border-danger [&_input]:border-danger/70 [&_select]:border-danger/70"
          : dirty
            ? "border-brand [&_input]:border-brand/70 [&_select]:border-brand/70"
            : "border-transparent",
        wide && "@[49rem]:col-span-2",
      )}
    >
      {/* The accessible name is "<Row> — <Family>"; the visible part is the family alone, and
          only where the cells stack and the column headers are hidden (a card under 49rem). */}
      <label htmlFor={control} className={cx("block text-xs font-medium text-muted", family ? "@[49rem]:sr-only" : "sr-only")}>
        {family ? (
          <>
            <span className="sr-only">{field.row} {"—"} </span>
            {family}
          </>
        ) : (
          field.row
        )}
      </label>
      {notUsed ? (
        // Dormant, not faded: the control loses its fill and its border turns dashed, while the
        // value stays readable in text-muted (>= 4.5:1 on the card in every theme; an opacity
        // fade took it to 2.86:1 on the light themes). Hover or focus restores the full control,
        // so a keyboard user editing a dormant choice sees it at full emphasis.
        <div
          data-dim=""
          className={cx(
            "[&_input]:border-dashed [&_input]:bg-transparent [&_input]:text-muted [&_select]:border-dashed [&_select]:bg-transparent [&_select]:text-muted",
            "[&:focus-within_input]:border-solid [&:focus-within_input]:bg-raised [&:focus-within_input]:text-fg [&:focus-within_select]:border-solid [&:focus-within_select]:bg-raised [&:focus-within_select]:text-fg",
            "[&:hover_input]:border-solid [&:hover_input]:bg-raised [&:hover_input]:text-fg [&:hover_select]:border-solid [&:hover_select]:bg-raised [&:hover_select]:text-fg",
          )}
        >
          {children({ control, describedBy })}
        </div>
      ) : (
        children({ control, describedBy })
      )}
      <p id={predictionId} className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-faint">
        <span>
          Saved effective value:{" "}
          <span className={prediction ? "text-muted" : "italic"}>{prediction ?? "Unavailable"}</span>
        </span>
        {dirty && (
          <span className="rounded border border-brand/40 bg-brand/10 px-1 text-[11px] font-medium text-brand">
            <span className="sr-only">, </span>Unsaved
          </span>
        )}
      </p>
      {notUsed && (
        <p id={notUsedId} className="text-xs text-muted">
          {notUsed}
        </p>
      )}
      {note && (
        <p id={noteId} className="text-xs text-faint">
          {note}
        </p>
      )}
      {reason && (
        <p id={reasonId} className="text-xs text-warn">
          {reason}
        </p>
      )}
      {storedWarning && (
        <p id={storedId} className="text-xs text-warn">
          {storedWarning}
        </p>
      )}
      {error && (
        <p id={errorId} className="text-xs font-medium text-danger">
          {error}
        </p>
      )}
    </div>
  );
}
