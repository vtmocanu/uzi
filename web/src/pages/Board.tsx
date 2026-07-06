// Kanban board of PRD-labeled issues. Columns are GitLab labels; moves are
// forge-first (the server writes the label, then returns the authoritative
// card — a failed move snaps back because nothing moved optimistically).
// Column identity follows multica's status-color language
// (packages/views/issues/components/status-icon.tsx / status-heading.tsx):
// every column gets a stable accent dot, Open is neutral, Closed is muted and
// not a drop target. Cards are content-first (multica board-card.tsx): title,
// meta, badges. Live behavior (latest_run badges, 10s visibility-gated polling,
// auto-move toasts, the attention strip, in-app issue links) is PRD #12 M2/M3.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, Link, useNavigate } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  type Board as BoardData,
  type Card as CardData,
  type RunListItem,
} from "../lib/api";
import { startRunGate, type StartRunGate } from "../lib/runStream";
import {
  canOpenRunView,
  hasActiveRun,
  isAwaitingApproval,
  retryHint,
  runBadge,
} from "../lib/runBadge";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { visibleColumns } from "../lib/boardColumns";
import { prefs } from "../lib/prefs";
import { Alert, Badge, Button, Card, cx, Field, Input, PageHeader, SectionTitle, Skeleton, Textarea } from "../components/ui";
import { FixCiButton, PipelineBadge } from "../components/PipelineBadge";
import { ExternalLinkIcon, PlusIcon, XIcon } from "../components/icons";
import { useAuth } from "../auth/AuthContext";

const OPEN_KEY = "";
const CLOSED_KEY = "__closed__";

// Stable accents for working columns, cycled by position.
const COLUMN_ACCENTS = ["bg-info", "bg-brand", "bg-warn", "bg-ok", "bg-danger"];

// columnKeyForCard maps a card to the key of the column it renders in.
function columnKeyForCard(card: CardData): string {
  if (card.closed) return CLOSED_KEY;
  return card.column;
}

// columnLabel is the human name of the column a card sits in (for toasts).
function columnLabel(card: CardData): string {
  if (card.closed) return "Closed";
  if (card.column === "") return "Open";
  return card.column;
}

export function Board() {
  const { id: repoId = "" } = useParams();
  const navigate = useNavigate();
  const { prdlessEnabled, prdlessLabel } = useAuth();
  const [board, setBoard] = useState<BoardData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [syncing, setSyncing] = useState(false);
  const [dragIid, setDragIid] = useState<number | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const [editingColumns, setEditingColumns] = useState(false);
  const [creatingIssue, setCreatingIssue] = useState(false);
  const [starting, setStarting] = useState<number | null>(null);
  const [prdlessBusy, setPrdlessBusy] = useState<number | null>(null);

  // Hide-empty-columns toggle, persisted per repo. Initialised lazily from prefs;
  // re-read on repoId change because the route swaps :id without remounting the
  // component (the lazy init only ran for the first repo). See boardColumns.ts —
  // the actual hide decision is derived at render, never stored per column.
  const hideEmptyKey = `uzi.board.${repoId}.hideEmpty`;
  const [hideEmpty, setHideEmpty] = useState(() => prefs.get(hideEmptyKey, false));
  useEffect(() => {
    setHideEmpty(prefs.get(`uzi.board.${repoId}.hideEmpty`, false));
  }, [repoId]);
  const toggleHideEmpty = () => {
    setHideEmpty((v) => {
      const next = !v;
      prefs.set(hideEmptyKey, next);
      return next;
    });
  };

  // Start-run preconditions, refreshed alongside the board: whether the user has a
  // worker and an Anthropic token. Whether an issue already has an active run now
  // comes from the card's own latest_run (no separate listRuns fan-in).
  const [hasWorker, setHasWorker] = useState(false);
  const [hasToken, setHasToken] = useState(false);
  // The viewer's runs on this repo blocked on their approval — drives the
  // attention strip above the columns.
  const [awaitingRuns, setAwaitingRuns] = useState<RunListItem[]>([]);

  // Toasts announce auto-moves the poll observes ("#42 → Human Review").
  const [toasts, setToasts] = useState<{ id: number; text: string }[]>([]);
  const toastSeq = useRef(0);
  // Pending toast-removal timers, cleared on unmount so a dismissal never fires
  // setState on an unmounted component.
  const toastTimers = useRef<ReturnType<typeof setTimeout>[]>([]);
  const pushToast = useCallback((text: string) => {
    const id = (toastSeq.current += 1);
    setToasts((t) => [...t, { id, text }]);
    const timer = setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 6000);
    toastTimers.current.push(timer);
  }, []);

  // iids the operator moved by hand within the last poll window. The column change
  // is already applied locally, so the background poll must NOT re-announce it as
  // an auto-move — this closes the false-toast window between a drag's server
  // commit and move()'s local setBoard (reviewer SF2). Released after one poll
  // interval; a genuine later auto-move on the same card still toasts.
  const suppressToastIids = useRef<Set<number>>(new Set());
  const suppressTimers = useRef<ReturnType<typeof setTimeout>[]>([]);
  useEffect(
    () => () => {
      toastTimers.current.forEach(clearTimeout);
      suppressTimers.current.forEach(clearTimeout);
    },
    [],
  );

  // boardRef mirrors the rendered board so the background poll can diff a fresh
  // payload against what the user is looking at. Manual drags are already applied
  // here, so they never toast — only server-driven moves do.
  const boardRef = useRef<BoardData | null>(null);
  useEffect(() => {
    boardRef.current = board;
  }, [board]);

  const load = useCallback(async () => {
    setError("");
    try {
      const { board } = await api.getBoard(repoId);
      setBoard(board);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load board");
    } finally {
      setLoading(false);
    }
  }, [repoId]);

  const loadPreconditions = useCallback(async () => {
    try {
      const [{ workers }, { secrets }, { runs }] = await Promise.all([
        api.listWorkers(),
        api.listSecrets(),
        api.listRuns({ repoId }),
      ]);
      setHasWorker(workers.length > 0);
      setHasToken(secrets.some((s) => s.kind === "anthropic_token"));
      setAwaitingRuns(runs.filter((r) => isAwaitingApproval(r.status)));
    } catch {
      // Non-fatal: the board still renders; Start-run stays gated conservatively.
    }
  }, [repoId]);

  // poll is the background refresh: re-read the board, toast on any column change
  // the user did not make, and refresh preconditions. Errors are swallowed (keep the
  // last good board; the next tick retries) — only the foreground load surfaces them.
  const poll = useCallback(async () => {
    try {
      const { board: fresh } = await api.getBoard(repoId);
      const prev = boardRef.current;
      if (prev) {
        for (const card of fresh.cards) {
          const before = prev.cards.find((c) => c.iid === card.iid);
          if (
            before &&
            columnKeyForCard(before) !== columnKeyForCard(card) &&
            !suppressToastIids.current.has(card.iid)
          ) {
            pushToast(`#${card.iid} → ${columnLabel(card)}`);
          }
        }
      }
      setBoard(fresh);
    } catch {
      // keep the last good board
    }
  }, [repoId, pushToast]);

  useEffect(() => {
    load();
    loadPreconditions();
  }, [load, loadPreconditions]);

  // Liveness: poll every ~10s while visible, paused when the tab is hidden, so
  // auto-moves and badge changes appear without a manual Refresh. Becoming visible
  // again triggers an immediate catch-up. No WebSocket (out of scope). The shared
  // hook stashes this callback in a ref, so the inline arrow is safe.
  usePollWhileVisible(() => {
    poll();
    loadPreconditions();
  }, 10000);

  const startRun = async (card: CardData) => {
    setError("");
    setStarting(card.iid);
    try {
      const { run } = await api.createRun(repoId, card.iid);
      navigate(`/runs/${run.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not start run");
      setStarting(null);
      loadPreconditions();
    }
  };

  // Fix CI (PRD #6): queue a plan-gated ci_fix run for a failed pipeline's ref.
  // The server enforces the "no active fix / branch free" preconditions and returns
  // a 409 the ApiError surfaces; on success we open the new run.
  const [fixingRef, setFixingRef] = useState<string | null>(null);
  const fixCi = async (ref: string) => {
    setError("");
    setFixingRef(ref);
    try {
      const { run } = await api.createCIFixRun(repoId, ref);
      navigate(`/runs/${run.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not start CI fix");
      setFixingRef(null);
    }
  };

  const refresh = async () => {
    setSyncing(true);
    setError("");
    try {
      const { board } = await api.syncRepo(repoId);
      setBoard(board);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Refresh failed");
    } finally {
      setSyncing(false);
    }
  };

  const move = async (toKey: string, iid: number) => {
    if (toKey === CLOSED_KEY) return; // Closed is not a drop target in the MVP.
    setError("");
    // Suppress the auto-move toast for this card: a poll landing between the
    // server commit below and the local setBoard would otherwise diff the new
    // column against the stale baseline and false-toast a move the user just made.
    suppressToastIids.current.add(iid);
    try {
      const to = toKey === OPEN_KEY ? "open" : toKey;
      const { card } = await api.moveIssue(repoId, iid, to);
      // Forge-first: the server applied the label change and returned the
      // authoritative card. Replace it in place (no optimistic move, so a
      // failure leaves the card where it was — a natural snap-back).
      setBoard((prev) =>
        prev ? { ...prev, cards: prev.cards.map((c) => (c.iid === card.iid ? card : c)) } : prev,
      );
      // Hold the suppression for one poll interval so an already-in-flight poll
      // still sees the moved column as its baseline, then release it.
      const timer = setTimeout(() => suppressToastIids.current.delete(iid), 11000);
      suppressTimers.current.push(timer);
    } catch (err) {
      suppressToastIids.current.delete(iid); // the move failed — nothing to suppress
      setError(err instanceof ApiError ? err.message : "Move failed");
    }
  };

  // PRDLESS toggle (PRD #22 M4): apply/remove the escape-hatch label on a card.
  // Forge-first like move — replace the card with the server's authoritative copy
  // on success, no optimistic update. The label change never moves a column, so
  // unlike move() it needs no auto-move-toast suppression.
  const togglePrdless = async (card: CardData) => {
    setError("");
    setPrdlessBusy(card.iid);
    try {
      const applying = !card.labels.includes(prdlessLabel);
      const { card: updated } = await api.setIssuePrdless(repoId, card.iid, applying);
      setBoard((prev) =>
        prev ? { ...prev, cards: prev.cards.map((c) => (c.iid === updated.iid ? updated : c)) } : prev,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not update the label");
    } finally {
      setPrdlessBusy(null);
    }
  };

  const columns = useMemo(() => {
    const cols: { key: string; label: string; droppable: boolean; accent: string }[] = [
      { key: OPEN_KEY, label: "Open", droppable: true, accent: "bg-faint" },
    ];
    (board?.columns ?? []).forEach((c, i) => {
      cols.push({
        key: c.label_name,
        label: c.label_name,
        droppable: true,
        accent: COLUMN_ACCENTS[i % COLUMN_ACCENTS.length],
      });
    });
    cols.push({ key: CLOSED_KEY, label: "Closed", droppable: false, accent: "bg-edge-strong" });
    return cols;
  }, [board]);

  const cardsByColumn = useMemo(() => {
    const map = new Map<string, CardData[]>();
    for (const c of board?.cards ?? []) {
      const key = columnKeyForCard(c);
      const arr = map.get(key) ?? [];
      arr.push(c);
      map.set(key, arr);
    }
    return map;
  }, [board]);

  if (loading) {
    return (
      <div className="space-y-5">
        <Skeleton className="h-8 w-64" />
        <div className="flex gap-4">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-72 w-72 shrink-0" />
          ))}
        </div>
      </div>
    );
  }

  // Columns to render: hide-empty is derived here from the freshly-polled cards,
  // never stored, so an auto-move that populates a hidden column reveals it on the
  // next poll. A live drag reveals every lane so they stay drop targets, which
  // drives hiddenCount to 0 — the toolbar hint (gated on hiddenCount > 0) simply
  // disappears mid-drag rather than reading "0 hidden".
  const visible = visibleColumns(
    columns,
    (key) => cardsByColumn.get(key)?.length ?? 0,
    hideEmpty,
    dragIid != null,
  );
  const hiddenCount = columns.length - visible.length;

  return (
    <div className="space-y-5">
      <PageHeader
        backTo="/repos"
        backLabel="Boards"
        titleNode={
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight">{board?.path_with_namespace ?? "Board"}</h1>
            {board?.pipeline && <PipelineBadge pipeline={board.pipeline} />}
            {board?.pipeline && (
              <FixCiButton
                pipeline={board.pipeline}
                busy={fixingRef === board.pipeline.ref}
                onClick={() => fixCi(board.pipeline!.ref)}
              />
            )}
          </div>
        }
        description="Columns are GitLab labels. Cards move automatically as their runs progress; you can still drag a card to change its label on the forge. Only PRD-labeled issues appear here."
        actions={
          <>
            <label className="flex cursor-pointer select-none items-center gap-1.5 py-1.5 text-xs text-muted">
              <input
                type="checkbox"
                checked={hideEmpty}
                onChange={toggleHideEmpty}
                className="h-3.5 w-3.5 rounded border-edge accent-brand"
              />
              Hide empty
              {hiddenCount > 0 && <span className="text-muted">({hiddenCount} hidden)</span>}
            </label>
            <Button size="sm" onClick={() => setCreatingIssue((v) => !v)}>
              {creatingIssue ? (
                <>
                  <XIcon /> Close
                </>
              ) : (
                <>
                  <PlusIcon /> Create issue
                </>
              )}
            </Button>
            <Button variant="secondary" size="sm" onClick={() => setEditingColumns((v) => !v)}>
              {editingColumns ? "Close settings" : "Columns"}
            </Button>
            <Button variant="secondary" size="sm" disabled={syncing} onClick={refresh}>
              {syncing ? "Refreshing…" : "Refresh"}
            </Button>
          </>
        }
      />

      {error && <Alert message={error} />}

      {awaitingRuns.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-warn/40 bg-warn/10 px-4 py-2.5 text-sm text-warn">
          <span className="font-medium">
            {awaitingRuns.length} run{awaitingRuns.length > 1 ? "s" : ""} awaiting your approval
          </span>
          {awaitingRuns.map((r) => (
            <Link
              key={r.id}
              to={`/runs/${r.id}`}
              className="rounded-md border border-warn/40 px-1.5 py-0.5 text-warn transition-colors hover:bg-warn/20"
            >
              #{r.issue_iid} →
            </Link>
          ))}
        </div>
      )}

      {creatingIssue && (
        <CreateIssueForm
          repoId={repoId}
          onCreated={() => {
            setCreatingIssue(false);
            load();
            loadPreconditions();
          }}
          onError={setError}
        />
      )}

      {editingColumns && board && (
        <ColumnSettings
          board={board}
          onSaved={(b) => {
            setBoard(b);
            setEditingColumns(false);
          }}
          onError={setError}
        />
      )}

      <div className="flex items-start gap-4 overflow-x-auto pb-4">
        {visible.map((col) => {
          const cards = cardsByColumn.get(col.key) ?? [];
          const isTarget = dropTarget === col.key && col.droppable;
          const closedCol = col.key === CLOSED_KEY;
          // An empty lane only visible because a drag is in progress: dim it so it
          // reads as a transient drop target, not a real column.
          const dragRevealed = hideEmpty && dragIid != null && cards.length === 0;
          return (
            <div
              key={col.key || "open"}
              onDragOver={(e) => {
                if (!col.droppable) return;
                e.preventDefault();
                setDropTarget(col.key);
              }}
              onDragLeave={() => setDropTarget((t) => (t === col.key ? null : t))}
              onDrop={(e) => {
                e.preventDefault();
                setDropTarget(null);
                const iid = Number(e.dataTransfer.getData("text/plain"));
                if (iid) move(col.key, iid);
              }}
              className={cx(
                "flex w-72 shrink-0 flex-col rounded-xl border p-2.5 transition-colors",
                dragRevealed && "opacity-60",
                isTarget
                  ? "border-brand/70 bg-brand/5 ring-1 ring-brand/40"
                  : closedCol
                    ? "border-dashed border-edge bg-transparent"
                    : "border-edge bg-surface/60",
              )}
            >
              <div className="mb-2.5 flex items-center gap-2 px-1">
                <span aria-hidden="true" className={cx("h-2 w-2 rounded-full", col.accent)} />
                <span className={cx("text-sm font-semibold", closedCol ? "text-faint" : "text-fg")}>
                  {col.label}
                </span>
                <span className="ml-auto rounded-md bg-raised px-1.5 py-0.5 text-[11px] tabular-nums text-faint">
                  {cards.length}
                </span>
              </div>
              <div className="flex flex-col gap-2">
                {cards.map((card) => (
                  <IssueCard
                    key={card.iid}
                    card={card}
                    repoId={repoId}
                    projectWebUrl={board?.web_url}
                    gate={startRunGate({
                      hasPrdLink: card.has_prd_link,
                      prdlessBypass: prdlessEnabled && card.labels.includes(prdlessLabel),
                      closed: card.closed,
                      hasWorker,
                      hasToken,
                      activeRunExists: hasActiveRun(card.latest_run),
                    })}
                    starting={starting === card.iid}
                    onStart={() => startRun(card)}
                    fixCiBusy={card.pipeline != null && fixingRef === card.pipeline.ref}
                    onFixCi={() => card.pipeline && fixCi(card.pipeline.ref)}
                    prdlessEnabled={prdlessEnabled}
                    prdlessLabel={prdlessLabel}
                    prdlessBusy={prdlessBusy === card.iid}
                    onTogglePrdless={() => togglePrdless(card)}
                    onDragStart={(e) => {
                      e.dataTransfer.setData("text/plain", String(card.iid));
                      setDragIid(card.iid);
                    }}
                    onDragEnd={() => setDragIid(null)}
                    dimmed={dragIid === card.iid}
                  />
                ))}
                {cards.length === 0 && (
                  <p className="rounded-lg border border-dashed border-edge py-6 text-center text-xs text-faint">
                    {col.droppable ? "Drop a card here" : "Nothing closed yet"}
                  </p>
                )}
              </div>
            </div>
          );
        })}
      </div>

      {toasts.length > 0 && (
        <div
          role="status"
          aria-live="polite"
          className="pointer-events-none fixed bottom-4 right-4 z-50 flex flex-col gap-2"
        >
          {toasts.map((t) => (
            <div
              key={t.id}
              className="rounded-lg border border-brand/40 bg-surface px-4 py-2 text-sm text-fg shadow-xl"
            >
              {t.text}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function IssueCard({
  card,
  repoId,
  projectWebUrl,
  gate,
  starting,
  onStart,
  fixCiBusy,
  onFixCi,
  prdlessEnabled,
  prdlessLabel,
  prdlessBusy,
  onTogglePrdless,
  onDragStart,
  onDragEnd,
  dimmed,
}: {
  card: CardData;
  repoId: string;
  projectWebUrl?: string;
  gate: StartRunGate;
  starting: boolean;
  onStart: () => void;
  fixCiBusy: boolean;
  onFixCi: () => void;
  prdlessEnabled: boolean;
  prdlessLabel: string;
  prdlessBusy: boolean;
  onTogglePrdless: () => void;
  onDragStart: (e: React.DragEvent) => void;
  onDragEnd: () => void;
  dimmed: boolean;
}) {
  const prdlessApplied = card.labels.includes(prdlessLabel);
  // Closed cards are not movable (move-to-Closed is unsupported; close/reopen
  // stays on the forge), so they are not draggable.
  const draggable = !card.closed;
  const run = card.latest_run;
  const badge = run ? runBadge(run, Date.now()) : null;
  const hint = run ? retryHint(run.run_count) : null;
  // awaiting_approval is the loudest card state: a human is the blocker while a
  // worker is held busy. Give the whole card a warn ring so it can't be missed.
  const loud = isAwaitingApproval(run?.status ?? "");
  const mrHref =
    badge?.kind === "mr" && isHttpsUrl(projectWebUrl)
      ? `${projectWebUrl}/-/merge_requests/${badge.mrIid}`
      : null;
  return (
    <div
      draggable={draggable}
      onDragStart={draggable ? onDragStart : undefined}
      onDragEnd={draggable ? onDragEnd : undefined}
      className={cx(
        "group rounded-lg border bg-raised/80 p-3 text-sm transition-colors",
        loud ? "border-warn/60 ring-2 ring-warn/40" : "border-edge",
        draggable ? "cursor-grab hover:border-edge-strong active:cursor-grabbing" : "cursor-default",
        dimmed && "opacity-40",
      )}
    >
      <div className="flex items-start justify-between gap-2">
        {/* In-app issue view. draggable={false}: a native <a> is draggable and
            would hijack the card's HTML5 drag payload. */}
        <Link
          to={`/repos/${repoId}/issues/${card.iid}`}
          draggable={false}
          className="font-medium leading-snug text-fg hover:text-brand-hover"
        >
          {card.title}
        </Link>
        <div className="flex shrink-0 items-center gap-1.5">
          {isHttpsUrl(card.web_url) && (
            <a
              href={card.web_url}
              target="_blank"
              rel="noreferrer"
              draggable={false}
              aria-label="Open on GitLab"
              title="Open on GitLab"
              className="text-faint transition-colors hover:text-brand"
            >
              <ExternalLinkIcon />
            </a>
          )}
          <span className="font-mono text-xs text-faint">#{card.iid}</span>
        </div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {badge &&
          (badge.kind === "mr" ? (
            mrHref ? (
              <a
                href={mrHref}
                target="_blank"
                rel="noreferrer"
                draggable={false}
                title="Open the merge request on GitLab"
                className="inline-flex items-center rounded-md border border-ok/40 bg-ok/10 px-1.5 py-0.5 text-[11px] font-medium text-ok transition-colors hover:bg-ok/20"
              >
                !{badge.mrIid}
              </a>
            ) : (
              <span className="inline-flex items-center rounded-md border border-ok/40 bg-ok/10 px-1.5 py-0.5 text-[11px] font-medium text-ok">
                !{badge.mrIid}
              </span>
            )
          ) : (
            <span className={badge.pulse ? "animate-pulse" : ""}>
              <Badge tone={badge.tone} title={badge.title}>
                {badge.label}
              </Badge>
            </span>
          ))}
        {hint && (
          <span className="text-[11px] text-faint" title="Number of runs on this issue">
            {hint}
          </span>
        )}
        {!card.has_prd_link &&
          (prdlessEnabled && prdlessApplied ? (
            <Badge tone="brand" title="PRD-link gate bypassed by label">
              {prdlessLabel}
            </Badge>
          ) : (
            <Badge tone="warning" title="Description has no link to a prds/*.md file; excluded from agent pickup">
              no PRD link
            </Badge>
          ))}
        {card.conflict && (
          <Badge tone="danger" title="Issue carries multiple column labels; shown in the highest column until the next move">
            conflict
          </Badge>
        )}
        {card.pipeline && <PipelineBadge pipeline={card.pipeline} />}
        {card.pipeline && <FixCiButton pipeline={card.pipeline} busy={fixCiBusy} onClick={onFixCi} />}
      </div>
      {(run || card.author) && (
        <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
          {run?.status === "running" && run.worker_name && <span>{run.worker_name}</span>}
          {run && !run.is_mine && run.owner_name && <span>started by {run.owner_name}</span>}
          {run && canOpenRunView(run) && (
            <Link
              to={`/runs/${run.id}`}
              draggable={false}
              className="text-brand transition-colors hover:text-brand-hover"
            >
              view run
            </Link>
          )}
          {card.author && <span>{card.author}</span>}
        </div>
      )}
      {!card.closed && (
        <div className="mt-2.5">
          <Button
            variant={gate.enabled ? "primary" : "secondary"}
            size="sm"
            disabled={!gate.enabled || starting}
            title={gate.enabled ? "Queue an agent run for this issue" : gate.reason}
            onClick={onStart}
            className="w-full"
          >
            {starting ? "Starting…" : gate.enabled ? "Start run" : "Start run (gated)"}
          </Button>
        </div>
      )}
      {/* Show when applying is meaningful (no PRD link) or the label is already
          applied (so it can be removed); hide the no-op case — has a PRD link and
          no label (S2). */}
      {prdlessEnabled && !card.closed && (prdlessApplied || !card.has_prd_link) && (
        <button
          type="button"
          draggable={false}
          disabled={prdlessBusy}
          onClick={onTogglePrdless}
          title={
            prdlessApplied
              ? `Remove the ${prdlessLabel} label and re-apply the PRD-link requirement`
              : `Apply the ${prdlessLabel} label so a run can start without a PRD link`
          }
          className="mt-1.5 w-full rounded-md border border-edge px-2 py-1 text-[11px] text-muted transition-colors hover:border-brand/60 hover:text-fg disabled:opacity-50"
        >
          {prdlessBusy ? "…" : prdlessApplied ? `Remove ${prdlessLabel}` : `Mark ${prdlessLabel}`}
        </button>
      )}
    </div>
  );
}

// CreateIssueForm opens a PRD-shaped issue on the forge. The description carries a
// prds/*.md link slot so the server's has_prd_link check passes and the card is
// immediately run-able.
function CreateIssueForm({
  repoId,
  onCreated,
  onError,
}: {
  repoId: string;
  onCreated: () => void;
  onError: (m: string) => void;
}) {
  const { prdLabel } = useAuth();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    onError("");
    setSaving(true);
    try {
      await api.createIssue(repoId, title.trim(), description);
      onCreated();
    } catch (err) {
      onError(err instanceof ApiError ? err.message : "Could not create the issue");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="max-w-2xl space-y-3">
      <SectionTitle>Create a PRD issue</SectionTitle>
      <p className="text-xs text-faint">
        Opened on GitLab with the <span className="font-medium text-muted">{prdLabel}</span> label.
        Link a <code className="rounded bg-raised px-1 py-0.5 text-muted">prds/*.md</code> file in the
        description so a run can be started from it.
      </p>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Title">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Issue title" />
        </Field>
        <Field label="Description">
          <Textarea
            rows={5}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={"What to build…\n\nSee prds/N-feature.md"}
          />
        </Field>
        <Button type="submit" disabled={saving || title.trim() === ""}>
          {saving ? "Creating…" : "Create issue"}
        </Button>
      </form>
    </Card>
  );
}

function ColumnSettings({
  board,
  onSaved,
  onError,
}: {
  board: BoardData;
  onSaved: (b: BoardData) => void;
  onError: (m: string) => void;
}) {
  const { prdLabel, autopilotLabel, prdlessLabel } = useAuth();
  const [names, setNames] = useState<string[]>(board.columns.map((c) => c.label_name));
  const [newName, setNewName] = useState("");
  const [saving, setSaving] = useState(false);

  // Suggest labels seen on cards that are not already columns and not the
  // configured PRD/autopilot/PRDLESS labels (those are workflow markers, never
  // columns).
  const suggestions = useMemo(() => {
    const seen = new Set<string>();
    for (const c of board.cards) for (const l of c.labels) seen.add(l);
    return [...seen]
      .filter((l) => l !== prdLabel && l !== autopilotLabel && l !== prdlessLabel && !names.includes(l))
      .sort();
  }, [board.cards, names, prdLabel, autopilotLabel, prdlessLabel]);

  const add = (name: string) => {
    const n = name.trim();
    if (n && !names.includes(n)) setNames([...names, n]);
    setNewName("");
  };

  const removeAt = (i: number) => setNames(names.filter((_, idx) => idx !== i));
  const swap = (i: number, j: number) => {
    if (j < 0 || j >= names.length) return;
    const next = [...names];
    [next[i], next[j]] = [next[j], next[i]];
    setNames(next);
  };

  const save = async () => {
    setSaving(true);
    try {
      const { board: updated } = await api.configureColumns(
        board.repo_id,
        names.map((label_name) => ({ label_name })),
      );
      onSaved(updated);
    } catch (err) {
      onError(err instanceof ApiError ? err.message : "Could not save columns");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="max-w-2xl">
      <SectionTitle>Columns</SectionTitle>
      <p className="mt-1 text-xs text-faint">
        Each column is a label. Order is left-to-right. New names are created as labels on the forge.
      </p>
      <ul className="mt-3 space-y-2">
        {names.map((name, i) => (
          <li key={name} className="flex items-center gap-2">
            <span className="flex flex-1 items-center gap-2 rounded-md border border-edge bg-raised px-3 py-1.5 text-sm">
              <span
                aria-hidden="true"
                className={cx("h-2 w-2 rounded-full", COLUMN_ACCENTS[i % COLUMN_ACCENTS.length])}
              />
              {name}
            </span>
            <Button variant="ghost" size="sm" onClick={() => swap(i, i - 1)} disabled={i === 0} aria-label={`Move ${name} up`}>
              ↑
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => swap(i, i + 1)}
              disabled={i === names.length - 1}
              aria-label={`Move ${name} down`}
            >
              ↓
            </Button>
            <Button variant="danger" size="sm" onClick={() => removeAt(i)}>
              Remove
            </Button>
          </li>
        ))}
        {names.length === 0 && <li className="text-xs text-faint">No columns.</li>}
      </ul>

      <div className="mt-4 flex gap-2">
        <Input
          placeholder="New column label"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add(newName);
            }
          }}
        />
        <Button variant="secondary" onClick={() => add(newName)} disabled={!newName.trim()}>
          Add
        </Button>
      </div>

      {suggestions.length > 0 && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <span className="text-xs text-faint">Suggestions:</span>
          {suggestions.map((s) => (
            <button
              key={s}
              onClick={() => add(s)}
              className="rounded-md border border-edge bg-raised px-2 py-0.5 text-xs text-muted transition-colors hover:border-brand/60 hover:text-fg"
            >
              + {s}
            </button>
          ))}
        </div>
      )}

      <div className="mt-4">
        <Button onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save columns"}
        </Button>
      </div>
    </Card>
  );
}
