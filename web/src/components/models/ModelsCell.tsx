// One cell of the Models grid (PRD #3263): a family's control for one row, its saved
// prediction, an optional reason, and an optional field error, each wired to the control
// through aria-describedby. The prediction line is the grid's signature: it always says what
// is SAVED ("Saved effective value: …"), and a draft that differs from it marks the cell
// Unsaved with a brand rail and border rather than pretending to predict the unsaved edit.

import type { ReactNode } from "react";
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
  error?: string | null;
  /** Spans both family columns (a row with one control). */
  wide?: boolean;
  children: (ids: ModelsCellIds) => ReactNode;
}) {
  const control = `${idBase}-control`;
  const predictionId = `${idBase}-saved`;
  const reasonId = `${idBase}-reason`;
  const errorId = `${idBase}-error`;
  const describedBy = [predictionId, reason ? reasonId : "", error ? errorId : ""].filter(Boolean).join(" ");
  const family = field.family ? harnessName(field.family) : null;
  return (
    <div
      data-field={field.key}
      data-dirty={dirty ? "true" : undefined}
      className={cx(
        // scroll-mb keeps a keyboard-focused control clear of the sticky Save bar.
        "min-w-0 space-y-1.5 border-l-2 pl-3 transition-colors motion-reduce:transition-none [&_input]:scroll-mb-40 [&_select]:scroll-mb-40",
        // An error outranks the Unsaved accent: one rail colour per cell, never two competing.
        error
          ? "border-danger [&_input]:border-danger/70 [&_select]:border-danger/70"
          : dirty
            ? "border-brand [&_input]:border-brand/70 [&_select]:border-brand/70"
            : "border-transparent",
        wide && "sm:col-span-2",
      )}
    >
      {/* The accessible name is "<Row> — <Family>"; the visible part is the family alone, and
          only below sm, where the column headers are hidden and the cells stack. */}
      <label htmlFor={control} className={cx("block text-xs font-medium text-muted", family ? "sm:sr-only" : "sr-only")}>
        {family ? (
          <>
            <span className="sr-only">{field.row} {"—"} </span>
            {family}
          </>
        ) : (
          field.row
        )}
      </label>
      {children({ control, describedBy })}
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
      {reason && (
        <p id={reasonId} className="text-xs text-warn">
          {reason}
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
