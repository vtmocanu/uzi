import { useMemo, useState } from "react";
import { api, type Board as BoardData } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { chipLabels } from "../../lib/labelChips";
import { insertionEdgeFor } from "../../lib/boardOrder";
import { Button, Card, cx, Input, SectionTitle } from "../../components/ui";
import { GripVerticalIcon } from "../../components/icons";
import { useAuth } from "../../auth/AuthContext";
import { COLUMN_ACCENTS } from "./shared";

export function ColumnSettings({
  board,
  onSaved,
  onError,
}: {
  board: BoardData;
  onSaved: (b: BoardData) => void;
  onError: (m: string) => void;
}) {
  const { autopilotLabel } = useAuth();
  const [names, setNames] = useState<string[]>(board.columns.map((c) => c.label_name));
  const [newName, setNewName] = useState("");
  const [saving, setSaving] = useState(false);
  // DnD reorder state (mirrors the board cards' idiom). Visuals are driven from
  // this state, never from reading e.dataTransfer during onDragOver (the drag
  // data store is protected during dragover).
  const [dragName, setDragName] = useState<string | null>(null);
  const [insertion, setInsertion] = useState<{ name: string; edge: "top" | "bottom" } | null>(null);

  // Suggest labels seen on cards that are not already columns and not the configured
  // autopilot label (a workflow marker, never a column). Same predicate the cards'
  // chips use (PRD #102 M4), but excluding `names` — the UNSAVED edit state — rather
  // than board.columns, so a label just added above stops being offered before Save.
  const suggestions = useMemo(() => {
    const seen = new Set<string>();
    for (const c of board.cards) for (const l of c.labels) seen.add(l);
    return chipLabels([...seen], {
      autopilotLabel,
      columnLabels: names,
    }).sort();
  }, [board.cards, names, autopilotLabel]);

  const add = (name: string) => {
    const n = name.trim();
    if (n && !names.includes(n)) setNames([...names, n]);
    setNewName("");
  };

  const removeAt = (i: number) => setNames(names.filter((_, idx) => idx !== i));
  // THE single order-computing path for reorder: array-move `from` -> `to` over
  // `names`. A no-op (moveTo(i, i)) leaves the array unchanged.
  const moveTo = (from: number, to: number) => {
    if (from === to) return;
    const next = [...names];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);
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
      onError(errorMessage(err, "Could not save columns"));
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
          <li
            key={name}
            draggable
            onDragStart={(e) => {
              setDragName(name);
              e.dataTransfer.setData("text/plain", name);
              e.dataTransfer.effectAllowed = "move";
            }}
            onDragOver={(e) => {
              e.preventDefault();
              const r = e.currentTarget.getBoundingClientRect();
              setInsertion({ name, edge: insertionEdgeFor(r.top, r.height, e.clientY) });
            }}
            onDrop={(e) => {
              e.preventDefault();
              const from = names.indexOf(e.dataTransfer.getData("text/plain"));
              const r = e.currentTarget.getBoundingClientRect();
              // Recompute the edge at drop time (like the cards' onCardDrop) so a
              // drop with no preceding dragOver still resolves.
              const edge = insertionEdgeFor(r.top, r.height, e.clientY);
              // `to` is the insertion index in the ORIGINAL array. Dragging
              // downward (from < to) removes the source first, shifting every
              // later index down by one, so decrement to compensate.
              let to = i + (edge === "bottom" ? 1 : 0);
              if (from < to) to -= 1;
              if (from >= 0) moveTo(from, to);
              setDragName(null);
              setInsertion(null);
            }}
            onDragEnd={() => {
              setDragName(null);
              setInsertion(null);
            }}
            className={cx(
              "flex items-center gap-2 cursor-grab active:cursor-grabbing",
              name === dragName && "opacity-40",
              insertion?.name === name &&
                insertion.edge === "top" &&
                "shadow-[inset_0_2px_0_0_rgb(var(--brand))]",
              insertion?.name === name &&
                insertion.edge === "bottom" &&
                "shadow-[inset_0_-2px_0_0_rgb(var(--brand))]",
            )}
          >
            <span
              aria-hidden="true"
              className="flex items-center text-faint hover:text-fg cursor-grab active:cursor-grabbing"
            >
              <GripVerticalIcon />
            </span>
            <span className="flex flex-1 items-center gap-2 rounded-md border border-edge bg-raised px-3 py-1.5 text-sm">
              <span
                aria-hidden="true"
                className={cx("h-2 w-2 rounded-full", COLUMN_ACCENTS[i % COLUMN_ACCENTS.length])}
              />
              {name}
            </span>
            <Button variant="danger" size="sm" draggable={false} onClick={() => removeAt(i)}>
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
