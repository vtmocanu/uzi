import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams, Link, useNavigate } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  isTerminalRun,
  type Board as BoardData,
  type Card as CardData,
} from "../lib/api";
import { startRunGate, type StartRunGate } from "../lib/runStream";
import { Alert, Badge, Button, Card, Field, Input } from "../components/ui";

const OPEN_KEY = "";
const CLOSED_KEY = "__closed__";

// columnKeyForCard maps a card to the key of the column it renders in.
function columnKeyForCard(card: CardData): string {
  if (card.closed) return CLOSED_KEY;
  return card.column;
}

export function Board() {
  const { id: repoId = "" } = useParams();
  const navigate = useNavigate();
  const [board, setBoard] = useState<BoardData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [syncing, setSyncing] = useState(false);
  const [dragIid, setDragIid] = useState<number | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const [editingColumns, setEditingColumns] = useState(false);
  const [creatingIssue, setCreatingIssue] = useState(false);
  const [starting, setStarting] = useState<number | null>(null);

  // Start-run preconditions, refreshed alongside the board: whether the user has
  // a worker and an Anthropic token, and which of this repo's issues already have
  // a non-terminal run.
  const [hasWorker, setHasWorker] = useState(false);
  const [hasToken, setHasToken] = useState(false);
  const [activeIids, setActiveIids] = useState<Set<number>>(new Set());

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
        api.listRuns(),
      ]);
      setHasWorker(workers.length > 0);
      setHasToken(secrets.some((s) => s.kind === "anthropic_token"));
      const active = new Set<number>();
      for (const r of runs) {
        if (r.repo_id === repoId && !isTerminalRun(r.status)) active.add(r.issue_iid);
      }
      setActiveIids(active);
    } catch {
      // Non-fatal: the board still renders; Start-run stays gated conservatively.
    }
  }, [repoId]);

  useEffect(() => {
    load();
    loadPreconditions();
  }, [load, loadPreconditions]);

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
    try {
      const to = toKey === OPEN_KEY ? "open" : toKey;
      const { card } = await api.moveIssue(repoId, iid, to);
      // Forge-first: the server applied the label change and returned the
      // authoritative card. Replace it in place (no optimistic move, so a
      // failure leaves the card where it was — a natural snap-back).
      setBoard((prev) =>
        prev ? { ...prev, cards: prev.cards.map((c) => (c.iid === card.iid ? card : c)) } : prev,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Move failed");
    }
  };

  const columns = useMemo(() => {
    const cols: { key: string; label: string; droppable: boolean }[] = [
      { key: OPEN_KEY, label: "Open", droppable: true },
    ];
    for (const c of board?.columns ?? []) {
      cols.push({ key: c.label_name, label: c.label_name, droppable: true });
    }
    cols.push({ key: CLOSED_KEY, label: "Closed", droppable: false });
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

  if (loading) return <p className="text-slate-500">Loading board…</p>;

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">{board?.path_with_namespace ?? "Board"}</h1>
          <p className="mt-1 text-sm text-slate-400">
            Columns are GitLab labels. Drag a card to change its label on the forge. Only
            PRD-labeled issues appear here.{" "}
            <Link to="/repos" className="text-indigo-400 hover:text-indigo-300">
              Back to repos
            </Link>
          </p>
        </div>
        <div className="flex gap-2">
          <Button onClick={() => setCreatingIssue((v) => !v)}>
            {creatingIssue ? "Close" : "Create issue"}
          </Button>
          <Button variant="ghost" onClick={() => setEditingColumns((v) => !v)}>
            {editingColumns ? "Close settings" : "Columns"}
          </Button>
          <Button variant="ghost" disabled={syncing} onClick={refresh}>
            {syncing ? "Refreshing…" : "Refresh"}
          </Button>
        </div>
      </div>

      {error && <Alert message={error} />}

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

      <div className="flex gap-4 overflow-x-auto pb-4">
        {columns.map((col) => {
          const cards = cardsByColumn.get(col.key) ?? [];
          const isTarget = dropTarget === col.key && col.droppable;
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
              className={`flex w-72 shrink-0 flex-col rounded-xl border p-3 ${
                isTarget ? "border-indigo-500 bg-indigo-950/30" : "border-slate-800 bg-panel/40"
              }`}
            >
              <div className="mb-3 flex items-center justify-between">
                <span className="text-sm font-semibold text-slate-200">{col.label}</span>
                <span className="text-xs text-slate-500">{cards.length}</span>
              </div>
              <div className="flex flex-col gap-2">
                {cards.map((card) => (
                  <IssueCard
                    key={card.iid}
                    card={card}
                    gate={startRunGate({
                      hasPrdLink: card.has_prd_link,
                      closed: card.closed,
                      hasWorker,
                      hasToken,
                      activeRunExists: activeIids.has(card.iid),
                    })}
                    starting={starting === card.iid}
                    onStart={() => startRun(card)}
                    onDragStart={(e) => {
                      e.dataTransfer.setData("text/plain", String(card.iid));
                      setDragIid(card.iid);
                    }}
                    onDragEnd={() => setDragIid(null)}
                    dimmed={dragIid === card.iid}
                  />
                ))}
                {cards.length === 0 && (
                  <p className="py-6 text-center text-xs text-slate-600">Empty</p>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function IssueCard({
  card,
  gate,
  starting,
  onStart,
  onDragStart,
  onDragEnd,
  dimmed,
}: {
  card: CardData;
  gate: StartRunGate;
  starting: boolean;
  onStart: () => void;
  onDragStart: (e: React.DragEvent) => void;
  onDragEnd: () => void;
  dimmed: boolean;
}) {
  // Closed cards are not movable (move-to-Closed is unsupported; close/reopen
  // stays on the forge), so they are not draggable.
  const draggable = !card.closed;
  return (
    <div
      draggable={draggable}
      onDragStart={draggable ? onDragStart : undefined}
      onDragEnd={draggable ? onDragEnd : undefined}
      className={`rounded-lg border border-slate-700 bg-slate-900 p-3 text-sm ${
        draggable ? "cursor-grab active:cursor-grabbing" : "cursor-default"
      } ${dimmed ? "opacity-40" : ""}`}
    >
      <div className="flex items-start justify-between gap-2">
        {isHttpsUrl(card.web_url) ? (
          <a
            href={card.web_url}
            target="_blank"
            rel="noreferrer"
            className="font-medium text-slate-100 hover:text-indigo-300"
          >
            {card.title}
          </a>
        ) : (
          <span className="font-medium text-slate-100">{card.title}</span>
        )}
        <span className="shrink-0 text-xs text-slate-500">#{card.iid}</span>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {!card.has_prd_link && (
          <Badge tone="warning" title="Description has no link to a prds/*.md file; excluded from agent pickup">
            no PRD link
          </Badge>
        )}
        {card.conflict && (
          <Badge tone="danger" title="Issue carries multiple column labels; shown in the highest column until the next move">
            conflict
          </Badge>
        )}
        {card.author && <span className="text-xs text-slate-500">{card.author}</span>}
      </div>
      {!card.closed && (
        <div className="mt-2">
          <Button
            variant={gate.enabled ? "primary" : "ghost"}
            disabled={!gate.enabled || starting}
            title={gate.enabled ? "Queue an agent run for this issue" : gate.reason}
            onClick={onStart}
            className="w-full"
          >
            {starting ? "Starting…" : "Start run"}
          </Button>
          {!gate.enabled && <p className="mt-1 text-[11px] text-slate-500">{gate.reason}</p>}
        </div>
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
    <Card className="space-y-3">
      <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
        Create a PRD issue
      </h2>
      <p className="text-xs text-slate-500">
        Opened on GitLab with the <span className="font-medium">PRD</span> label. Link a{" "}
        <code className="rounded bg-slate-800 px-1 py-0.5 text-slate-200">prds/*.md</code> file in the
        description so a run can be started from it.
      </p>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Title">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Issue title" />
        </Field>
        <Field label="Description">
          <textarea
            className="w-full rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-100 outline-none focus:border-indigo-400"
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
  const [names, setNames] = useState<string[]>(board.columns.map((c) => c.label_name));
  const [newName, setNewName] = useState("");
  const [saving, setSaving] = useState(false);

  // Suggest labels seen on cards that are not already columns (and not "PRD").
  const suggestions = useMemo(() => {
    const seen = new Set<string>();
    for (const c of board.cards) for (const l of c.labels) seen.add(l);
    return [...seen].filter((l) => l !== "PRD" && !names.includes(l)).sort();
  }, [board.cards, names]);

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
    <Card>
      <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">Columns</h2>
      <p className="mt-1 text-xs text-slate-500">
        Each column is a label. Order is left-to-right. New names are created as labels on the forge.
      </p>
      <ul className="mt-3 space-y-2">
        {names.map((name, i) => (
          <li key={name} className="flex items-center gap-2">
            <span className="flex-1 rounded-md border border-slate-700 bg-slate-900 px-3 py-1.5 text-sm">
              {name}
            </span>
            <Button variant="ghost" onClick={() => swap(i, i - 1)} disabled={i === 0}>
              ↑
            </Button>
            <Button variant="ghost" onClick={() => swap(i, i + 1)} disabled={i === names.length - 1}>
              ↓
            </Button>
            <Button variant="danger" onClick={() => removeAt(i)}>
              Remove
            </Button>
          </li>
        ))}
        {names.length === 0 && <li className="text-xs text-slate-600">No columns.</li>}
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
        <Button variant="ghost" onClick={() => add(newName)} disabled={!newName.trim()}>
          Add
        </Button>
      </div>

      {suggestions.length > 0 && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <span className="text-xs text-slate-500">Suggestions:</span>
          {suggestions.map((s) => (
            <button
              key={s}
              onClick={() => add(s)}
              className="rounded-md border border-slate-700 bg-slate-800 px-2 py-0.5 text-xs text-slate-300 hover:border-indigo-500"
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
