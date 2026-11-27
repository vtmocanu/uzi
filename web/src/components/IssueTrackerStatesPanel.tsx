// The State mapping of a repo linked to an issue tracker (PRD #4173 M6), reachable from
// the repo's row on the Repos page and from the board's column settings.
//
// It reads as the board does, left to right turned top to bottom: the open State a new
// issue starts in, one row per board column, then the closed State uzi writes when an
// issue is done. Each row offers only the States the server would accept in that role,
// so most refusals cannot be reached from here: the closed State lists every State but
// only a closable one (resolved and means done) can be picked, and a State another row
// already uses is not offered twice. The server still decides; a refused save shows its
// message verbatim and the choices stay on screen.
//
// A saved State the project no longer offers for its row (renamed, removed or archived in
// the issue tracker since) stays visible as the row's value, marked and with a note, and
// Save stays off until each such row gets a State the project still offers: the panel
// never resends a State it knows is gone, and never drops one without saying so.
//
// State names and column labels are API text: rendered as JSX text through
// stripUnsafeChars. Option values keep the exact names, which the PUT must send back.

import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  type IssueTrackerState,
  type IssueTrackerStateMapping,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { stripUnsafeChars } from "../lib/safeText";
import { Alert, Button, cx, Select, Skeleton } from "./ui";

type Loaded = {
  states: IssueTrackerState[];
  // The columns the rows show, in board order.
  columns: string[];
  // Set when the board could not be read: why, so the panel can say so.
  boardError: string;
};

export function IssueTrackerStatesPanel({
  repoId,
  repoLabel,
  columns: boardColumns,
  onClose,
}: {
  repoId: string;
  // The repo's display path, already masked for demo mode by the caller.
  repoLabel: string;
  // The board's column labels when the caller has the board (the column settings);
  // otherwise the panel reads the board itself.
  columns?: string[];
  onClose: () => void;
}) {
  const ids = useId();
  const headingRef = useRef<HTMLHeadingElement>(null);
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [loadError, setLoadError] = useState("");
  const [openState, setOpenState] = useState("");
  const [closedState, setClosedState] = useState("");
  // A Map, not a plain object: a column named `toString` or `constructor` must not read the
  // inherited Object.prototype member (PR #4757 review).
  const [columnStates, setColumnStates] = useState<Map<string, string>>(() => new Map());
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    headingRef.current?.focus();
  }, []);

  // The labels as a string, so a new array with the same labels is no change.
  const columnsKey = boardColumns ? JSON.stringify(boardColumns) : "";
  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const view = await api.getIssueTrackerStates(repoId);
        let columns: string[] | undefined = columnsKey ? (JSON.parse(columnsKey) as string[]) : undefined;
        let boardError = "";
        if (!columns) {
          try {
            const { board } = await api.getBoard(repoId);
            columns = board.columns.map((c) => c.label_name);
          } catch (err) {
            // A never-enabled repo has no board yet. Keep the saved column rows so a
            // save does not drop them.
            boardError = stripUnsafeChars(errorMessage(err, "The board could not be read"));
            columns = (view.mapping?.columns ?? []).map((c) => c.label_name);
          }
        }
        if (!live) return;
        const byLabel = new Map<string, string>();
        for (const c of view.mapping?.columns ?? []) byLabel.set(c.label_name, c.state);
        setOpenState(view.mapping?.open_state ?? "");
        setClosedState(view.mapping?.closed_state ?? "");
        setColumnStates(byLabel);
        setLoaded({ states: view.states, columns, boardError });
      } catch (err) {
        if (live) setLoadError(stripUnsafeChars(errorMessage(err, "Failed to load the issue tracker States")));
      }
    })();
    return () => {
      live = false;
    };
  }, [repoId, columnsKey]);

  // Which row uses each State, so no State is offered to two rows.
  const usedBy = new Map<string, string>();
  if (openState) usedBy.set(openState, "the open State");
  if (closedState) usedBy.set(closedState, "the closed State");
  for (const label of loaded?.columns ?? []) {
    const st = columnStates.get(label);
    if (st) usedBy.set(st, `column ${stripUnsafeChars(label)}`);
  }
  const takenElsewhere = (name: string, mine: string) => name !== mine && usedBy.has(name);
  const optionText = (st: IssueTrackerState, mine: string) =>
    takenElsewhere(st.name, mine)
      ? `${stripUnsafeChars(st.name)} (used for ${usedBy.get(st.name)})`
      : stripUnsafeChars(st.name);

  const edit = (apply: () => void) => {
    apply();
    setSaved(false);
    setSaveError("");
  };

  const save = async () => {
    if (!loaded) return;
    setSaving(true);
    setSaveError("");
    setSaved(false);
    const mapping: IssueTrackerStateMapping = {
      open_state: openState,
      closed_state: closedState,
      columns: loaded.columns
        .filter((label) => columnStates.get(label))
        .map((label) => ({ label_name: label, state: columnStates.get(label) ?? "" })),
    };
    try {
      await api.putIssueTrackerStates(repoId, mapping);
      setSaved(true);
    } catch (err) {
      setSaveError(stripUnsafeChars(errorMessage(err, "Could not save the State mapping")));
    } finally {
      setSaving(false);
    }
  };

  const openable = loaded?.states.filter((s) => s.openable) ?? [];
  // A saved value the row does not offer: its option is drawn here, disabled.
  const isOffered = (value: string, role: "open" | "closed") =>
    value === "" ||
    (loaded?.states ?? []).some((st) => st.name === value && (role === "open" ? st.openable : st.closable));
  const isProjectState = (value: string) => (loaded?.states ?? []).some((st) => st.name === value);
  const staleOption = (value: string, role: "open" | "closed") =>
    isOffered(value, role) ? null : (
      <option value={value} disabled>
        {stripUnsafeChars(value)}{" "}
        {isProjectState(value) ? "(no longer offered for this row)" : "(no longer a State of the project)"}
      </option>
    );
  const staleNote = (value: string, role: "open" | "closed", alternative: string) =>
    isOffered(value, role)
      ? undefined
      : `The saved State ${isProjectState(value) ? "is not allowed for this row" : "is no longer a State of the project"}. Choose ${alternative}; Save stays off until then.`;
  const anyStale =
    !isOffered(openState, "open") ||
    !isOffered(closedState, "closed") ||
    (loaded?.columns ?? []).some((label) => !isOffered(columnStates.get(label) ?? "", "open"));

  return (
    <section
      aria-labelledby={`${ids}-title`}
      className="max-w-2xl space-y-4 rounded-xl border border-edge bg-surface p-4"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 id={`${ids}-title`} ref={headingRef} tabIndex={-1} className="text-sm font-semibold text-fg outline-hidden">
            Issue tracker States
          </h3>
          <p className="mt-0.5 break-all text-xs text-muted">{repoLabel}</p>
          <p className="mt-0.5 break-all text-xs text-muted">
            Repo id: <code className="select-text">{repoId}</code> (for <code>uzi repo issue-tracker-states</code>)
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={saving}>
          Close
        </Button>
      </div>
      <p className="max-w-prose text-sm text-muted">
        Each board column stands for one State of the linked project, and so do the open and the closed State. uzi
        writes these States when an issue moves. Every row needs a different State.
      </p>

      {loadError && <Alert message={loadError} />}
      {!loaded && !loadError && (
        <div className="space-y-2" aria-hidden="true">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </div>
      )}

      {loaded && (
        <>
          {loaded.boardError && (
            <p className="rounded-lg border border-edge bg-raised/40 px-3 py-2 text-xs text-muted" data-testid="states-board-note">
              The board&apos;s columns could not be read: {loaded.boardError}. A board opens after the repo&apos;s
              first enable; map its columns then. The open and the closed State can be saved now.
            </p>
          )}
          <ol className="relative space-y-3 border-l border-edge pl-4">
            <MappingRow
              id={`${ids}-open`}
              label="Open"
              hint="A new or reopened issue starts here."
              marker="start"
              note={staleNote(openState, "open", "another State")}
            >
              <Select
                id={`${ids}-open`}
                value={openState}
                disabled={saving}
                onChange={(e) => edit(() => setOpenState(e.target.value))}
              >
                <option value="">Choose a State</option>
                {staleOption(openState, "open")}
                {openable.map((st) => (
                  <option key={st.name} value={st.name} disabled={takenElsewhere(st.name, openState)}>
                    {optionText(st, openState)}
                  </option>
                ))}
              </Select>
            </MappingRow>

            {loaded.columns.map((label, i) => {
              const value = columnStates.get(label) ?? "";
              const id = `${ids}-col-${i}`;
              return (
                <MappingRow
                  key={label}
                  id={id}
                  label={stripUnsafeChars(label)}
                  hint="Board column"
                  marker="column"
                  note={
                    value === ""
                      ? "Unmapped — a move into this column is refused until it is mapped."
                      : staleNote(value, "open", "another State or Unmapped")
                  }
                >
                  <Select
                    id={id}
                    value={value}
                    disabled={saving}
                    onChange={(e) => edit(() => setColumnStates((prev) => new Map(prev).set(label, e.target.value)))}
                  >
                    <option value="">Unmapped</option>
                    {staleOption(value, "open")}
                    {openable.map((st) => (
                      <option key={st.name} value={st.name} disabled={takenElsewhere(st.name, value)}>
                        {optionText(st, value)}
                      </option>
                    ))}
                  </Select>
                </MappingRow>
              );
            })}

            <MappingRow
              id={`${ids}-closed`}
              label="Closed"
              hint="uzi writes it when an issue is done. It must mean done, never Won't fix."
              marker="end"
              note={staleNote(closedState, "closed", "another State")}
            >
              <Select
                id={`${ids}-closed`}
                value={closedState}
                disabled={saving}
                onChange={(e) => edit(() => setClosedState(e.target.value))}
              >
                <option value="">Choose a State</option>
                {!loaded.states.some((st) => st.name === closedState) && staleOption(closedState, "closed")}
                {loaded.states.map((st) =>
                  st.closable ? (
                    <option key={st.name} value={st.name} disabled={takenElsewhere(st.name, closedState)}>
                      {optionText(st, closedState)}
                    </option>
                  ) : (
                    <option key={st.name} value={st.name} disabled>
                      {stripUnsafeChars(st.name)} (cannot be the closed State: it does not mean done)
                    </option>
                  ),
                )}
              </Select>
            </MappingRow>
          </ol>

          {saveError && <Alert message={saveError} />}
          {saved && <Alert tone="success" message="State mapping saved." />}
          <div className="flex justify-end">
            <Button onClick={save} disabled={saving || !openState || !closedState || anyStale}>
              {saving ? "Saving…" : "Save States"}
            </Button>
          </div>
        </>
      )}
    </section>
  );
}

function MappingRow({
  id,
  label,
  hint,
  marker,
  note,
  children,
}: {
  id: string;
  label: string;
  hint: string;
  marker: "start" | "column" | "end";
  note?: string;
  children: React.ReactNode;
}) {
  return (
    <li className="relative grid gap-1.5 sm:grid-cols-[minmax(0,12rem)_1fr] sm:items-start sm:gap-4">
      {/* The rail's stop: hollow for the open State, filled for a column, ok-toned for
          the closed State, so the column of dots reads as the issue's way through. */}
      <span
        aria-hidden="true"
        className={cx(
          "absolute -left-[21px] top-2.5 h-2.5 w-2.5 rounded-full border-2",
          marker === "start" && "border-brand bg-surface",
          marker === "column" && "border-edge-strong bg-raised",
          marker === "end" && "border-ok bg-ok",
        )}
      />
      <div className="min-w-0 pt-1.5">
        <label htmlFor={id} className="block break-words text-sm font-medium text-fg">
          {label}
        </label>
        <p className="text-xs text-faint">{hint}</p>
      </div>
      <div className="min-w-0 space-y-1">
        {children}
        {note && <p className="text-xs text-warn">{note}</p>}
      </div>
    </li>
  );
}
