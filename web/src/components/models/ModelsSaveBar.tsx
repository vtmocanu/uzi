// The Models tab's sticky Save bar (PRD #3263): the count of changed values and which rows
// they are, the save outcome, Discard and Save. It is sticky, not fixed, so at the end of the
// page it sits in flow below the last row and never covers it.

import type { ReactNode } from "react";
import { Alert, Button, cx } from "../ui";

export type SaveOutcome =
  | { kind: "saved"; count: number }
  | { kind: "saved_no_prediction" }
  | { kind: "invalid"; count: number; unmatched: string[] }
  | { kind: "not_saved"; message: string }
  | { kind: "unknown" }
  | { kind: "saved_unreadable" }
  | { kind: "reloaded"; stillDirty: number; adopted?: boolean }
  | { kind: "reload_failed"; message: string }
  | { kind: "discarded_unconfirmed" };

function plural(n: number, one: string, many: string): string {
  return n === 1 ? `1 ${one}` : `${n} ${many}`;
}

function OutcomeAlert({ outcome, onReload, reloading, uncertain }: {
  outcome: SaveOutcome;
  onReload: () => void;
  reloading: boolean;
  /** An earlier save is still unconfirmed: keep Reload reachable even after a failed retry. */
  uncertain: boolean;
}) {
  const reload = (
    <Button type="button" variant="secondary" size="sm" disabled={reloading} onClick={onReload}>
      {reloading ? "Reloading…" : "Reload saved settings"}
    </Button>
  );
  const withReload = (alert: ReactNode) => (
    <div className="flex flex-wrap items-start gap-2">
      <div className="min-w-0 flex-1">{alert}</div>
      {reload}
    </div>
  );
  const reloadIfUncertain = (alert: ReactNode) => (uncertain ? withReload(alert) : alert);
  // A failed retry says nothing about the earlier, unconfirmed save: keep saying so.
  const earlier = uncertain ? " An earlier save is still unconfirmed; reload to see what is stored." : "";
  switch (outcome.kind) {
    case "saved":
      return <Alert tone="success" message={`Saved ${plural(outcome.count, "change", "changes")}. Your next runs use ${outcome.count === 1 ? "it" : "them"}.`} />;
    case "saved_no_prediction":
      return withReload(
        <Alert
          tone="info"
          message="Saved. Every change committed, but the new effective values could not be read, so they show as unavailable."
        />,
      );
    case "invalid":
      return reloadIfUncertain(
        <div role="alert" className="space-y-1 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <p>
            {plural(outcome.count, "value needs", "values need")} attention. Nothing was saved; your edits are still
            here.{earlier}
          </p>
          {outcome.unmatched.map((m) => (
            <p key={m} className="text-xs">
              {m}
            </p>
          ))}
        </div>,
      );
    case "not_saved":
      return reloadIfUncertain(
        <Alert message={`The save failed and nothing was changed: ${outcome.message}. Your edits are still here.${earlier}`} />,
      );
    case "unknown":
      return withReload(
        <Alert
          tone="warning"
          message="The server could not confirm whether this save was applied. Your edits are still here. Reload the saved settings to see what is stored before saving again."
        />,
      );
    case "saved_unreadable":
      return withReload(
        <Alert
          tone="warning"
          message="Your changes were saved, but reading them back failed. Reload the saved settings to see the stored values."
        />,
      );
    case "reloaded":
      return (
        <Alert
          tone="info"
          message={
            outcome.adopted
              ? "Reloaded the saved settings. The page now shows what is stored."
              : outcome.stillDirty === 0
              ? "Reloaded the saved settings. They match your edits."
              : `Reloaded the saved settings. ${plural(outcome.stillDirty, "edit differs", "edits differ")} from what is stored.`
          }
        />
      );
    case "discarded_unconfirmed":
      return withReload(
        <Alert
          tone="warning"
          message="Your edits were discarded here, but an earlier save may still have been applied. Reload the saved settings to see what is stored."
        />,
      );
    case "reload_failed":
      return withReload(<Alert message={`Could not reload the saved settings: ${outcome.message}`} />);
  }
}

export function ModelsSaveBar({
  dirtyRows,
  saving,
  busy,
  blockedCount,
  outcome,
  reloading,
  uncertain = false,
  onDiscard,
  onReload,
}: {
  /** The accessible name of every changed value, in grid order. */
  dirtyRows: string[];
  saving: boolean;
  /** A save or a reload is in flight: Save and Discard wait for it. */
  busy: boolean;
  /** Values with a local problem that must be fixed before saving: an "Other" picker with an
   *  empty box (dirty or not) or a changed value its own validator refuses. */
  blockedCount: number;
  outcome: SaveOutcome | null;
  reloading: boolean;
  /** An earlier save may have committed without confirmation (see UnsavedModelsGuard). */
  uncertain?: boolean;
  onDiscard: () => void;
  onReload: () => void;
}) {
  const count = dirtyRows.length;
  const blocked = blockedCount > 0;
  return (
    <section
      aria-label="Save model changes"
      data-testid="models-save-bar"
      className={cx(
        "sticky bottom-0 z-10 space-y-3 rounded-xl border bg-surface/95 px-4 py-3 shadow-lg backdrop-blur supports-[backdrop-filter]:bg-surface/85",
        count > 0 ? "border-brand/50" : "border-edge",
      )}
    >
      {outcome && <OutcomeAlert outcome={outcome} onReload={onReload} reloading={reloading} uncertain={uncertain} />}
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:gap-3">
        <p role="status" aria-live="polite" data-testid="models-save-status" className="min-w-0 text-sm sm:flex-1">
          {saving ? (
            <span className="text-fg">Saving {plural(count, "change", "changes")}{"…"}</span>
          ) : count === 0 && blocked ? (
            // Nothing differs from what is saved, but a cell still blocks Save. Only an "Other"
            // picker with an empty box can (a validator warning needs a changed value, so count
            // is then above 0), so say what to do rather than "No unsaved changes".
            <span className="font-semibold text-fg">Fix {plural(blockedCount, "value", "values")} before saving</span>
          ) : count === 0 ? (
            <span className="text-muted">No unsaved changes</span>
          ) : (
            <>
              <span className="font-semibold text-fg">{plural(count, "unsaved change", "unsaved changes")}</span>
              {/* Which values changed; below sm the count alone keeps the bar one line tall. */}
              <span className="text-muted max-sm:sr-only">: {dirtyRows.join(", ")}</span>
              {/* Why Save is disabled, kept visible at every width. */}
              {blocked && <span className="font-medium text-danger"> {"·"} fix {blockedCount} before saving</span>}
            </>
          )}
        </p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" disabled={busy || (count === 0 && !blocked)} onClick={onDiscard}>
            Discard
          </Button>
          <Button type="submit" disabled={busy || count === 0 || blocked}>
            {saving ? "Saving…" : "Save changes"}
          </Button>
        </div>
      </div>
    </section>
  );
}
