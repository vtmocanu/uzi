// One cell of the Models grid (PRD #3263): a family's control for one row, its saved
// prediction, an optional note, reason, Not used reason and field error, each wired to the
// control through aria-describedby. The prediction always says what is SAVED ("Saved effective
// value: …") to assistive tech; on screen it is a muted line shown only where it adds something
// the control does not already show (#4671): an Unsaved cell, Unavailable, or a value the select
// hides. A draft that differs from the saved value marks the cell Unsaved with a brand rail and
// border rather than pretending to predict the unsaved edit. A clean cell holding a saved
// override offers Reset to default, which edits the draft only.
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
  predictionVisible = true,
  onReset,
  children,
}: {
  field: ModelsField;
  idBase: string;
  dirty: boolean;
  /** The saved prediction, or null when it is unavailable. */
  prediction: string | null;
  /** False when the control already shows everything the saved prediction says (a clean cell
   *  only; a dirty or Unavailable cell always shows its line). */
  predictionVisible?: boolean;
  /** Present on a clean cell whose saved value is not Default: resets the draft to Default. */
  onReset?: () => void;
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
  // Reset removes its own button, so focus moves to the cell's control once the draft re-renders.
  const focusAfterReset = useRef(false);
  // An errored cell marks its controls aria-invalid. ModelSelect and EffortSelect are shared
  // pickers with no aria-invalid prop, so the cell sets it on its own select/input; it runs
  // after every render because the custom model ID box mounts and unmounts.
  useLayoutEffect(() => {
    root.current?.querySelectorAll("select, input").forEach((el) => {
      if (error) el.setAttribute("aria-invalid", "true");
      else el.removeAttribute("aria-invalid");
    });
    if (focusAfterReset.current) {
      focusAfterReset.current = false;
      document.getElementById(control)?.focus();
    }
  });
  const lineShown = dirty || prediction === null || predictionVisible;
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
      {/* Assistive tech always gets ONE text run, "Saved effective value: <prediction>[, Unsaved]":
          computed descriptions trim each text node, so a sentence split across spans reads
          "Savedeffective value:…". The visible line (when shown) and markers are aria-hidden
          duplicates. */}
      <p id={predictionId} className="flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5 text-xs text-faint">
        <span className="sr-only">
          {`Saved effective value: ${prediction ?? "Unavailable"}${dirty ? ", Unsaved" : ""}`}
        </span>
        {lineShown && (
          <>
            {dirty && <span aria-hidden="true">Saved</span>}
            <span aria-hidden="true">{"\u2192"}</span>
            <span aria-hidden="true" data-prediction="" className={cx("min-w-0 max-w-full [overflow-wrap:anywhere]", prediction ? "text-muted" : "italic")}>
              {prediction ?? "Unavailable"}
            </span>
            {dirty && (
              <span aria-hidden="true" className="rounded border border-brand/40 bg-brand/10 px-1 text-[11px] font-medium text-brand">
                Unsaved
              </span>
            )}
          </>
        )}
      </p>
      {onReset && !dirty && (
        <button
          type="button"
          className="text-xs text-muted underline decoration-edge-strong underline-offset-2 hover:text-fg"
          // The visible words come first, so a speech-input user can say what is on screen.
          aria-label={`Reset to default (${field.family ? `${field.row} — ${family}` : field.row})`}
          onClick={() => {
            focusAfterReset.current = true;
            onReset();
          }}
        >
          Reset to default
        </button>
      )}
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
