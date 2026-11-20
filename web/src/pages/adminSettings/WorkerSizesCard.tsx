import { useEffect, useRef, useState, type FormEvent, type Ref } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type AdminWorkerSize,
  type AdminWorkerSizes,
  type WorkerSizeAdvisory,
  type WorkerSizeCeilings,
  type WorkerSizeSpec,
  type WorkerSizeDeleteRefusal,
  type WorkerSizeRefusal,
  type WorkerSizeWarning,
} from "../../lib/api";
import { ApiError, errorMessage } from "../../lib/apiError";
import { Alert, Badge, Button, Card, Input, SectionTitle, Skeleton, type BadgeTone } from "../../components/ui";
import {
  ABSOLUTE_MAX,
  cpuCores,
  memoryGiB,
  newSizeNameError,
  parseCores,
  parseDiskGiB,
  parseMemoryGiB,
  sizeLabel,
  sizeOptionLabel,
  type ParsedQuantity,
} from "../../lib/workerSizes";

// WorkerSizesCard is the Admin → Workers editor for hosted worker sizes (PRD #4213): what
// each size buys, in human units (CPU cores, GiB), edited per row and saved as the api's
// integers. The api is authoritative: it validates against the chart ceilings, answers 400
// with per-field errors, and 409 while the controller cannot honour an edit (the editor is
// then read-only and says why). Builtin sizes are edited and reset; admin-added sizes (M3)
// are added below the table and edited or removed per row. A removal the api refuses
// because something still uses the size says what, in the words of this page.
//
// The list itself is owned by AdminWorkers, which reads it once and re-reads it after every
// successful write and after the refusals that ask for one (onChanged; not an in-use or
// builtin removal refusal), so the default-size selectors on the same page pick up an
// edited quantity at once.

type FieldKey = keyof WorkerSizeSpec;

type FieldDef = {
  key: FieldKey;
  label: string;
  unit: "cores" | "GiB";
  parse: (text: string) => ParsedQuantity;
  show: (n: number) => string;
};

const FIELDS: readonly FieldDef[] = [
  { key: "cpu_request_millicores", label: "CPU request", unit: "cores", parse: parseCores, show: cpuCores },
  { key: "cpu_limit_millicores", label: "CPU limit", unit: "cores", parse: parseCores, show: cpuCores },
  { key: "memory_request_mib", label: "Memory request", unit: "GiB", parse: parseMemoryGiB, show: memoryGiB },
  { key: "memory_limit_mib", label: "Memory limit", unit: "GiB", parse: parseMemoryGiB, show: memoryGiB },
  { key: "data_gib", label: "Data disk", unit: "GiB", parse: parseDiskGiB, show: String },
  { key: "dind_data_gib", label: "DinD data disk", unit: "GiB", parse: parseDiskGiB, show: String },
];

const HEAD = "px-1.5 py-2 font-medium sm:px-3";

const FIELD_KEYS = new Set<string>(FIELDS.map((f) => f.key));

type Draft = Record<FieldKey, string>;

const EMPTY_DRAFT = Object.fromEntries(FIELDS.map((f) => [f.key, ""])) as Draft;

function withoutKey(m: Record<string, string>, key: string): Record<string, string> {
  const next = { ...m };
  delete next[key];
  return next;
}

const draftOf = (s: WorkerSizeSpec): Draft =>
  Object.fromEntries(FIELDS.map((f) => [f.key, f.show(s[f.key])])) as Draft;

// One line naming a spec in human units, used for what a reset restores.
function specSummary(s: WorkerSizeSpec): string {
  return (
    `${cpuCores(s.cpu_request_millicores)} / ${cpuCores(s.cpu_limit_millicores)} CPU, ` +
    `${memoryGiB(s.memory_request_mib)} / ${memoryGiB(s.memory_limit_mib)} GiB memory, ` +
    `${s.data_gib} GiB data, ${s.dind_data_gib} GiB DinD`
  );
}

const ADVISORY_LABEL: Record<WorkerSizeAdvisory["kind"], string> = {
  // Never "fits" or "OK": a quota the api cannot read is an unknown, and says so.
  quota_unavailable: "Quota check unavailable",
  quota_zero: "Quota is zero",
  quota_exceeded: "Exceeds quota",
};

const ADVISORY_TONE: Record<WorkerSizeAdvisory["kind"], BadgeTone> = {
  quota_unavailable: "neutral",
  quota_zero: "danger",
  quota_exceeded: "warning",
};

// A refused size in plain words: who refused it, before the controller's own reason.
const REFUSAL_KIND: Record<WorkerSizeRefusal["kind"], string> = {
  size_refused: "controller refused this size",
  pvc_rejected: "cluster rejected a volume claim",
};

// The workers a row names before "and N more" (the rest are on Admin health).
const REFUSALS_SHOWN = 3;

const HEALTH_ROUTE = "/admin/health";

// The workers the cluster would not run at this size: a danger badge in the row's badges,
// then up to three workers, each linking to Admin health where its report lives.
function SizeRefusals({ label, items }: { label: string; items: WorkerSizeRefusal[] }) {
  const shown = items.slice(0, REFUSALS_SHOWN);
  const more = items.length - shown.length;
  return (
    <ul aria-label={`Workers refused at size ${label}`} className="mt-2 w-[8rem] space-y-1.5 text-xs font-normal sm:w-auto sm:max-w-[16rem]">
      {shown.map((r, i) => {
        const who = r.worker_name || r.worker_id;
        return (
          <li key={`${r.worker_id}-${r.kind}-${i}`} className="[overflow-wrap:anywhere]">
            {/* The accessible name starts with the visible worker name (label in name). */}
            <Link to={HEALTH_ROUTE} aria-label={`${who} in Admin health`} className="font-medium text-info hover:underline">
              {who}
            </Link>
            <span className="text-fg">: {REFUSAL_KIND[r.kind] ?? r.kind}</span>
            {r.reason && <span className="block text-muted">{r.reason}</span>}
          </li>
        );
      })}
      {more > 0 && (
        <li>
          <Link to={HEALTH_ROUTE} className="text-info hover:underline">
            and {more} more {more === 1 ? "worker" : "workers"} in Admin health
          </Link>
        </li>
      )}
    </ul>
  );
}

function ceilingsLine(c: WorkerSizeCeilings): string {
  const parts: string[] = [];
  if (c.cpu_limit_millicores) parts.push(`${cpuCores(c.cpu_limit_millicores.value)} CPU`);
  if (c.memory_limit_mib) parts.push(`${memoryGiB(c.memory_limit_mib.value)} GiB memory`);
  if (c.data_gib) parts.push(`${c.data_gib.value} GiB disk`);
  if (c.dind_data_gib) parts.push(`${c.dind_data_gib.value} GiB DinD disk`);
  if (parts.length === 0) return "No chart limits apply to sizes beyond the api's absolute caps.";
  const paths = [c.cpu_limit_millicores, c.memory_limit_mib, c.data_gib, c.dind_data_gib]
    .map((x) => x?.chart_path)
    .filter((p): p is string => !!p);
  const unique = [...new Set(paths)];
  return `Limits: up to ${parts.join(" / ")} (from chart values ${unique.join(", ")}).`;
}

type WriteResult = { name: string; verb: "saved" | "reset to shipped values" | "added" | "removed"; warnings: WorkerSizeWarning[] };

// The default-size settings a removal refusal can name, as this page labels them.
const SETTING_USE: Record<string, string> = {
  hosted_default_size: "new workers",
  ephemeral_default_size: "ephemeral workers",
};

// A refused removal in the page's words: what still uses the size, and that it stays.
function removalRefusal(label: string, body: WorkerSizeDeleteRefusal | null, fallback: string): string {
  if (body?.reason === "builtin") return `${label} is a builtin size and cannot be removed. Reset it to shipped values instead.`;
  if (body?.reason !== "in_use") return fallback;
  const parts = [`${label} is still in use, so it was not removed.`];
  const n = body.used_by?.workers ?? 0;
  if (n > 0) parts.push(`Still used by ${n} ${n === 1 ? "worker" : "workers"}.`);
  const uses = (body.used_by?.settings ?? []).map((k) => SETTING_USE[k] ?? k);
  if (uses.length > 0) parts.push(`Set as the default size for ${uses.join(" and ")}; choose another default first.`);
  return parts.join(" ");
}

// "At most 4 cores (workers.limitRange.max.cpu)": a bound in the editor's own units.
const coresWord = (millicores: number) => `${cpuCores(millicores)} ${millicores === 1000 ? "core" : "cores"}`;
const ABSOLUTE_NOTE = "the api's absolute maximum";

// The most a field may hold before the api refuses it, in the editor's units: the chart
// ceiling that binds it (limits and disks only; a request is bounded by its limit), else
// the api's absolute cap. Checked locally so a refusal names the bound before saving.
function fieldBound(key: FieldKey, c: WorkerSizeCeilings): { max: number; message: string }[] {
  const at = (max: number, text: string, path: string) => ({ max, message: `At most ${text} (${path})` });
  const out: { max: number; message: string }[] = [];
  switch (key) {
    case "cpu_request_millicores":
    case "cpu_limit_millicores":
      out.push(at(ABSOLUTE_MAX.millicores, coresWord(ABSOLUTE_MAX.millicores), ABSOLUTE_NOTE));
      if (key === "cpu_limit_millicores" && c.cpu_limit_millicores) {
        out.push(at(c.cpu_limit_millicores.value, coresWord(c.cpu_limit_millicores.value), c.cpu_limit_millicores.chart_path));
      }
      break;
    case "memory_request_mib":
    case "memory_limit_mib":
      out.push(at(ABSOLUTE_MAX.memoryMiB, `${memoryGiB(ABSOLUTE_MAX.memoryMiB)} GiB`, ABSOLUTE_NOTE));
      if (key === "memory_limit_mib" && c.memory_limit_mib) {
        out.push(at(c.memory_limit_mib.value, `${memoryGiB(c.memory_limit_mib.value)} GiB`, c.memory_limit_mib.chart_path));
      }
      break;
    case "data_gib":
    case "dind_data_gib": {
      out.push(at(ABSOLUTE_MAX.diskGiB, `${ABSOLUTE_MAX.diskGiB} GiB`, ABSOLUTE_NOTE));
      const ceiling = c[key];
      if (ceiling) out.push(at(ceiling.value, `${ceiling.value} GiB`, ceiling.chart_path));
      break;
    }
  }
  return out;
}

// Parse a whole draft; a field that cannot be an api integer, or is above a bound the list
// already names, reports why locally, in the editor's units.
function parseDraft(draft: Draft, ceilings: WorkerSizeCeilings): { values: Partial<WorkerSizeSpec>; errors: Record<string, string> } {
  const values: Partial<WorkerSizeSpec> = {};
  const errors: Record<string, string> = {};
  for (const f of FIELDS) {
    const p = f.parse(draft[f.key]);
    if (!p.ok) {
      errors[f.key] = p.error;
      continue;
    }
    values[f.key] = p.value;
    // The tightest bound names the real limit (a chart ceiling is below the absolute cap).
    const over = fieldBound(f.key, ceilings)
      .sort((x, y) => x.max - y.max)
      .find((bound) => p.value > bound.max);
    if (over) errors[f.key] = over.message;
  }
  if (!errors.cpu_request_millicores && !errors.cpu_limit_millicores && values.cpu_request_millicores! > values.cpu_limit_millicores!) {
    errors.cpu_request_millicores = "The request cannot be above the limit";
  }
  if (!errors.memory_request_mib && !errors.memory_limit_mib && values.memory_request_mib! > values.memory_limit_mib!) {
    errors.memory_request_mib = "The request cannot be above the limit";
  }
  return { values, errors };
}

// The six inputs, shared by the edit and the add forms. Each input's accessible name starts
// with `labelPrefix` (the size's label, or "New size"), so two forms never read alike.
function SizeFields({
  idPrefix,
  labelPrefix,
  draft,
  errors,
  locked,
  firstInputRef,
  onChange,
}: {
  idPrefix: string;
  labelPrefix: string;
  draft: Draft;
  errors: Record<string, string>;
  locked: boolean;
  firstInputRef?: Ref<HTMLInputElement>;
  onChange: (key: FieldKey, value: string) => void;
}) {
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
      {FIELDS.map((f, i) => {
        const id = `${idPrefix}-${f.key}`;
        const err = errors[f.key];
        return (
          <div key={f.key} className="min-w-0 space-y-1">
            <label htmlFor={id} className="block text-xs font-medium text-muted">
              {f.label} ({f.unit})
            </label>
            <Input
              ref={i === 0 ? firstInputRef : undefined}
              id={id}
              type="text"
              inputMode={f.unit === "GiB" && f.key.startsWith("d") ? "numeric" : "decimal"}
              aria-label={`${labelPrefix} ${f.label} (${f.unit})`}
              aria-invalid={err ? true : undefined}
              aria-describedby={err ? `${id}-error` : undefined}
              value={draft[f.key]}
              disabled={locked}
              onChange={(e) => onChange(f.key, e.target.value)}
              className="px-2.5 py-1.5 tabular-nums disabled:opacity-60 aria-invalid:border-danger/70"
            />
            {err && (
              <p id={`${id}-error`} className="text-xs text-danger [overflow-wrap:anywhere]">
                {err}
              </p>
            )}
          </div>
        );
      })}
    </div>
  );
}

export function WorkerSizesCard({
  data,
  loadError,
  onChanged,
}: {
  /** GET /admin/worker-sizes, null until the first read succeeds. */
  data: AdminWorkerSizes | null;
  /** The last read's error, "" when it succeeded. */
  loadError: string;
  /**
   * Re-read the list and lift it to the page; called after every successful write, on a
   * read-only or max_sizes 409, on a 404 removal and by Retry. A removal refused as in use
   * or builtin does not re-read.
   */
  onChanged: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<WriteResult | null>(null);
  // The row whose Reset or Remove is waiting for Confirm / Keep.
  const [confirming, setConfirming] = useState<{ name: string; action: "reset" | "remove" } | null>(null);
  // The add form (M3): open or not, the typed name and the six fields. Opening it closes the
  // edit form and the reverse, so one fieldErrors map serves whichever is open.
  const [adding, setAdding] = useState(false);
  const [addName, setAddName] = useState("");
  const [addDraft, setAddDraft] = useState<Draft>(EMPTY_DRAFT);
  // A 409 on a write: the controller stopped advertising support after the list was read.
  // It holds only until the next list arrives (the 409 itself asks for one), which is then
  // the authority on whether sizes are editable.
  const [conflict, setConflict] = useState<{ reason: string; against: AdminWorkerSizes | null } | null>(null);
  const conflictReason = conflict && conflict.against === data ? conflict.reason : null;

  // Focus management: Edit focuses the form's first input; Save, Cancel, Escape, a Reset
  // (confirmed or kept) and a refused removal hand focus back to that row's Edit button;
  // Keep or Escape on a removal returns to that row's Remove button (both looked up by
  // size name, since the buttons re-mount after the form closes or the list is re-read).
  const editButtons = useRef(new Map<string, HTMLButtonElement>());
  const firstInput = useRef<HTMLInputElement>(null);
  const confirmButton = useRef<HTMLButtonElement>(null);
  // Set alongside the state change that renders the target; focused once it is mounted.
  // Add opens onto the name input, Cancel or Escape returns to the Add button, and a
  // successful Add lands on the new row's Edit; a removal lands on its success message,
  // since the row it came from is gone.
  const readOnlyBanner = useRef<HTMLDivElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const addNameInput = useRef<HTMLInputElement>(null);
  const resultRegion = useRef<HTMLDivElement>(null);
  const errorRegion = useRef<HTMLDivElement>(null);
  const capNote = useRef<HTMLParagraphElement>(null);
  const removeButtons = useRef(new Map<string, HTMLButtonElement>());
  // A refused removal, shown inline under that row's actions (and nowhere else). It lasts
  // until another Add, Edit, Remove or Reset starts, or until the next re-read of the list
  // arrives: a new list may no longer be what the refusal explained. An earlier write's
  // unawaited re-read that lands after a later refusal clears that refusal too (accepted:
  // the list it shows is then newer than the explanation).
  const [rowRefusal, setRowRefusal] = useState<{ name: string; message: string } | null>(null);
  const [refusalData, setRefusalData] = useState(data);
  if (data !== refusalData) {
    setRefusalData(data);
    setRowRefusal(null);
  }
  // "field": `name` is the id of the first invalid input; "cap": the 16-of-16 note.
  type FocusKind = "edit" | "form" | "confirm" | "banner" | "add" | "addForm" | "result" | "error" | "cap" | "field" | "remove";
  const focusTarget = useRef<{ kind: FocusKind; name: string } | null>(null);
  const setFocusTarget = (t: { kind: FocusKind; name: string }) => {
    focusTarget.current = t;
  };
  useEffect(() => {
    const t = focusTarget.current;
    if (!t) return;
    const el =
      t.kind === "banner" ? readOnlyBanner.current
      : t.kind === "form" ? firstInput.current
      : t.kind === "confirm" ? confirmButton.current
      : t.kind === "add" ? addButton.current
      : t.kind === "addForm" ? addNameInput.current
      : t.kind === "result" ? (resultRegion.current ?? errorRegion.current)
      : t.kind === "error" ? errorRegion.current
      : t.kind === "cap" ? (capNote.current ?? errorRegion.current)
      : t.kind === "field" ? document.getElementById(t.name)
      : t.kind === "remove" ? (removeButtons.current.get(t.name) ?? null)
      : (editButtons.current.get(t.name) ?? null);
    // A button disabled while a write is in flight cannot take focus; try again once enabled.
    if (el && !(el instanceof HTMLButtonElement && el.disabled)) {
      el.focus();
      focusTarget.current = null;
    }
  });

  if (!data) {
    return (
      <Card className="space-y-5">
        <SectionTitle>Worker sizes</SectionTitle>
        {loadError ? (
          <>
            <Alert message={loadError} />
            <Button variant="secondary" onClick={() => void onChanged()}>
              Retry loading worker sizes
            </Button>
          </>
        ) : (
          <Skeleton className="h-24 w-full" />
        )}
      </Card>
    );
  }

  const readOnlyReason = conflictReason ?? (data.editable ? null : (data.read_only_reason ?? "The controller cannot apply size edits right now."));
  const readOnly = readOnlyReason !== null;
  const locked = readOnly || busy;
  const editingRow = data.sizes.find((s) => s.name === editing) ?? null;
  const ceilings = data.ceilings;
  const maxSizes = data.max_sizes || 16;
  const atCap = data.sizes.length >= maxSizes;

  const closeAdd = () => {
    setAdding(false);
    setAddName("");
    setAddDraft(EMPTY_DRAFT);
    setFieldErrors({});
  };

  const startEdit = (row: AdminWorkerSize) => {
    if (adding) closeAdd();
    setEditing(row.name);
    setRowRefusal(null);
    setDraft(draftOf(row));
    setFieldErrors({});
    setError("");
    setResult(null);
    setConfirming(null);
    setFocusTarget({ kind: "form", name: row.name });
  };
  const closeEdit = () => {
    setEditing(null);
    setDraft(null);
    setFieldErrors({});
  };
  const cancelEdit = () => {
    if (editing) setFocusTarget({ kind: "edit", name: editing });
    closeEdit();
    setError("");
  };

  const startAdd = () => {
    if (locked || atCap) return;
    closeEdit();
    setRowRefusal(null);
    setAdding(true);
    setAddName("");
    setAddDraft(EMPTY_DRAFT);
    setFieldErrors({});
    setError("");
    setResult(null);
    setConfirming(null);
    setFocusTarget({ kind: "addForm", name: "" });
  };
  const cancelAdd = () => {
    closeAdd();
    setError("");
    setFocusTarget({ kind: "add", name: "" });
  };

  const parsed = draft ? parseDraft(draft, ceilings) : null;
  const addParsed = adding ? parseDraft(addDraft, ceilings) : null;
  const typedName = addName.trim();
  const addNameError = adding ? newSizeNameError(typedName, data.sizes.map((s) => s.name)) : null;

  const diskLowered =
    !!editingRow &&
    !!parsed &&
    ((parsed.values.data_gib !== undefined && parsed.values.data_gib < editingRow.data_gib) ||
      (parsed.values.dind_data_gib !== undefined && parsed.values.dind_data_gib < editingRow.dind_data_gib));

  // Map a refused write onto the editor: 409 → read-only until the re-read it triggers
  // lands; 400 fields → the inputs (each server message already names its chart path).
  const handleWriteError = (err: unknown, fallback: string, mapFields: boolean) => {
    if (err instanceof ApiError && err.status === 409 && (err.body as { reason?: string } | null)?.reason === "max_sizes") {
      // Another admin filled the table meanwhile; the re-read hides Add. Close the form so it
      // does not linger (and resurface after a later Remove), and park focus on the cap note,
      // which renders as soon as the form closes. The error message takes focus only when
      // the note is not rendered (the card went read-only meanwhile).
      closeAdd();
      setError(`At most ${maxSizes} sizes. Remove an added size to add another.`);
      setFocusTarget({ kind: "cap", name: "" });
      void onChanged();
      return;
    }
    if (err instanceof ApiError && err.status === 409) {
      const body = err.body as { read_only_reason?: string } | null;
      setConflict({ reason: body?.read_only_reason || err.message, against: data });
      // The open editor locks, so focus would fall to body; the banner explains why.
      setFocusTarget({ kind: "banner", name: "" });
      void onChanged();
      return;
    }
    if (err instanceof ApiError && err.status === 400) {
      const fields = (err.body as { fields?: Record<string, string> } | null)?.fields ?? {};
      const known = Object.entries(fields).filter(([k]) => mapFields && FIELD_KEYS.has(k));
      const other = Object.entries(fields).filter(([k]) => !(mapFields && FIELD_KEYS.has(k)));
      if (known.length > 0) setFieldErrors(Object.fromEntries(known));
      const detail = other.map(([k, v]) => `${FIELDS.find((f) => f.key === k)?.label ?? k}: ${v}`).join("; ");
      if (known.length > 0) setError(["Fix the highlighted fields and save again.", detail].filter(Boolean).join(" "));
      else setError(detail ? `${err.message}. ${detail}` : err.message);
      return;
    }
    setError(errorMessage(err, fallback));
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!editingRow || !parsed || locked) return;
    setError("");
    setResult(null);
    if (Object.keys(parsed.errors).length > 0) {
      setFieldErrors(parsed.errors);
      const first = FIELDS.find((f) => parsed.errors[f.key]);
      if (first) setFocusTarget({ kind: "field", name: `worker-size-${editingRow.name}-${first.key}` });
      return;
    }
    setFieldErrors({});
    setBusy(true);
    try {
      const resp = await api.updateAdminWorkerSize(editingRow.name, parsed.values as WorkerSizeSpec);
      closeEdit();
      setResult({ name: editingRow.name, verb: "saved", warnings: resp.warnings });
      setFocusTarget({ kind: "edit", name: editingRow.name });
      await onChanged();
    } catch (err) {
      handleWriteError(err, `Failed to save size ${sizeLabel(editingRow.name)}`, true);
    } finally {
      setBusy(false);
    }
  };

  const askConfirm = (row: AdminWorkerSize, action: "reset" | "remove") => {
    if (locked) return;
    setRowRefusal(null);
    setConfirming({ name: row.name, action });
    setFocusTarget({ kind: "confirm", name: row.name });
  };
  // Keep / Escape: a removal goes back to the Remove button that opened it; a reset goes
  // to that row's Edit button, as a confirmed reset does.
  const keepRow = (name: string, action: "reset" | "remove") => {
    setConfirming(null);
    setFocusTarget({ kind: action === "remove" ? "remove" : "edit", name });
  };

  const reset = async (row: AdminWorkerSize) => {
    if (locked) return;
    setError("");
    setResult(null);
    setFieldErrors({});
    setBusy(true);
    try {
      const resp = await api.resetAdminWorkerSize(row.name);
      if (editing === row.name) closeEdit();
      setResult({ name: row.name, verb: "reset to shipped values", warnings: resp.warnings });
      setFocusTarget({ kind: "edit", name: row.name });
      await onChanged();
    } catch (err) {
      // A reset has no inputs to map onto, so every field error is spelled out in the alert.
      handleWriteError(err, `Failed to reset size ${sizeLabel(row.name)}`, false);
    } finally {
      setConfirming(null);
      setBusy(false);
    }
  };

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (!addParsed || locked) return;
    setError("");
    setResult(null);
    const errors: Record<string, string> = { ...addParsed.errors, ...(addNameError ? { name: addNameError } : {}) };
    if (Object.keys(errors).length > 0) {
      setFieldErrors(errors);
      const firstField = FIELDS.find((f) => errors[f.key]);
      const firstId = errors.name ? "add-worker-size-name" : firstField ? `add-worker-size-${firstField.key}` : "";
      if (firstId) setFocusTarget({ kind: "field", name: firstId });
      return;
    }
    setFieldErrors({});
    setBusy(true);
    const name = typedName;
    try {
      const resp = await api.createAdminWorkerSize(name, addParsed.values as WorkerSizeSpec);
      closeAdd();
      setResult({ name, verb: "added", warnings: resp.warnings });
      // The new row's Edit mounts once the re-read lands; the focus effect waits for it.
      setFocusTarget({ kind: "edit", name });
      await onChanged();
    } catch (err) {
      if (err instanceof ApiError && err.status === 400 && err.message.startsWith("invalid worker size name")) {
        setFieldErrors({ name: "Start with a lowercase letter, then use only lowercase letters, digits or hyphens (at most 16 characters)" });
      } else {
        handleWriteError(err, `Failed to add size ${sizeLabel(name)}`, true);
      }
    } finally {
      setBusy(false);
    }
  };

  const remove = async (row: AdminWorkerSize) => {
    if (locked) return;
    const label = sizeLabel(row.name);
    setError("");
    setRowRefusal(null);
    setResult(null);
    setBusy(true);
    try {
      await api.deleteAdminWorkerSize(row.name);
      if (editing === row.name) closeEdit();
      setResult({ name: row.name, verb: "removed", warnings: [] });
      setFocusTarget({ kind: "result", name: "" });
      await onChanged();
    } catch (err) {
      const body = err instanceof ApiError ? (err.body as WorkerSizeDeleteRefusal | null) : null;
      if (err instanceof ApiError && err.status === 409 && (body?.reason === "in_use" || body?.reason === "builtin")) {
        // Under the row it came from, not above the table; focus stays on that row's Edit.
        setRowRefusal({ name: row.name, message: removalRefusal(label, body, err.message) });
        setFocusTarget({ kind: "edit", name: row.name });
      } else if (err instanceof ApiError && err.status === 404) {
        // Already gone (another admin removed it): the re-read drops the row, so focus
        // moves to the message.
        setError(`Size ${label} was already removed.`);
        setFocusTarget({ kind: "result", name: "" });
        void onChanged();
      } else {
        handleWriteError(err, `Failed to remove size ${label}`, false);
      }
    } finally {
      setConfirming(null);
      setBusy(false);
    }
  };

  const advisoriesBySize = data.sizes
    .map((s) => ({ size: s.name, items: data.advisories.filter((a) => a.size === s.name) }))
    .filter((g) => g.items.length > 0);
  const orphanAdvisories = data.advisories.filter((a) => !data.sizes.some((s) => s.name === a.size));
  if (orphanAdvisories.length > 0) advisoriesBySize.push({ size: "", items: orphanAdvisories });

  // Refusals per size; an older api sends none, and one for a size no longer listed has no row.
  const refusalsBySize = new Map<string, WorkerSizeRefusal[]>();
  for (const r of data.refusals ?? []) refusalsBySize.set(r.size, [...(refusalsBySize.get(r.size) ?? []), r]);

  const tiers = ceilings.enabled_tiers;
  const editLabel = editingRow ? sizeLabel(editingRow.name) : "";

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Worker sizes</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          What each hosted worker size buys. The request is reserved on a node; the limit is the most a worker can use.
          Saving a CPU or memory change rolls every idle worker of that size once onto the new values, and a busy one
          when it goes idle. A disk-only change rolls nothing: claims grow in place.
        </p>
        <p className="mt-2 text-sm text-muted">{ceilingsLine(ceilings)}</p>
        {tiers && tiers.length > 0 && (
          <p className="mt-1 text-xs text-faint">Checked against the enabled worker tiers: {tiers.join(", ")}.</p>
        )}
        {ceilings.dind_overridden && (
          <p className="mt-1 text-xs text-faint">
            The DinD data disk is fixed for every size by <code className="font-mono text-fg">workers.docker.dindDataSize</code>, so
            the DinD value here is not used while that override is set.
          </p>
        )}
      </div>

      {readOnly && (
        <div ref={readOnlyBanner} tabIndex={-1} className="outline-none">
          <Alert tone="warning" message={`Sizes are read-only. ${readOnlyReason}`} />
        </div>
      )}
      {loadError && <Alert message={loadError} />}
      {error && (
        <div ref={errorRegion} tabIndex={-1} className="outline-none">
          <Alert message={error} />
        </div>
      )}
      {result && (
        <div ref={resultRegion} tabIndex={-1} className="outline-none">
          <Alert tone="success" message={`Size ${sizeLabel(result.name)} ${result.verb}.`} />
        </div>
      )}
      {result?.warnings.map((w, i) => <Alert key={`${w.field}-${i}`} tone="warning" message={w.message} />)}

      <div className="space-y-2">
        <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
          <p className="text-xs text-muted">Request / limit. CPU in cores; memory and disks in GiB.</p>
          {readOnly && <span className="text-xs text-faint">Read-only</span>}
        </div>
        {/* Actions live in the Size column, the one a narrow screen always shows, so the
            table may scroll sideways without hiding what a row can do. */}
        <div className="overflow-x-auto rounded-lg border border-edge">
          <table className="w-full text-left text-xs sm:text-sm">
            <thead className="border-b border-edge text-muted">
              <tr>
                <th scope="col" className={HEAD}>Size</th>
                <th scope="col" className={HEAD}>CPU <span className="sr-only">(cores, request / limit)</span></th>
                <th scope="col" className={HEAD}>Memory <span className="sr-only">(GiB, request / limit)</span></th>
                <th scope="col" className={HEAD}>Data <span className="sr-only">disk (GiB)</span></th>
                <th scope="col" className={HEAD}>DinD <span className="sr-only">data disk (GiB)</span></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-edge">
              {data.sizes.map((row) => {
                const label = sizeLabel(row.name);
                const isEditing = editing === row.name && draft !== null;
                const canReset = row.builtin && row.customized && row.shipped !== null;
                // Only an admin-added size can be removed; a builtin is reset instead.
                const canRemove = !row.builtin;
                const confirmAction = confirming?.name === row.name ? confirming.action : null;
                const isConfirming = (confirmAction === "reset" && canReset) || (confirmAction === "remove" && canRemove);
                const refused = refusalsBySize.get(row.name) ?? [];
                const resetLowers =
                  !!row.shipped && (row.shipped.data_gib < row.data_gib || row.shipped.dind_data_gib < row.dind_data_gib);
                return (
                  <tr key={row.name} className={isEditing || isConfirming ? "bg-raised/40" : undefined}>
                    <th scope="row" className="px-1.5 py-2 align-top font-medium text-fg sm:px-3">
                      <div className="flex flex-col items-start gap-1 sm:flex-row sm:flex-wrap sm:items-center sm:gap-1.5">
                        <span className="whitespace-nowrap">{label}</span>
                        {row.builtin && <Badge>Builtin</Badge>}
                        {row.customized && <Badge tone="brand">Customized</Badge>}
                        {refused.length > 0 && <Badge tone="danger">Refused</Badge>}
                      </div>
                      {refused.length > 0 && <SizeRefusals label={label} items={refused} />}
                      {canReset && (
                        <p className="mt-1 w-[8rem] text-xs font-normal text-faint sm:w-auto sm:max-w-[16rem]">
                          Shipped: {specSummary(row.shipped!)}
                        </p>
                      )}
                      {isConfirming && confirmAction === "remove" ? (
                        <div
                          role="group"
                          aria-label={`Confirm removal of size ${label}`}
                          className="mt-2 w-[8rem] space-y-2 font-normal sm:w-auto sm:max-w-[16rem]"
                          onKeyDown={(e) => {
                            if (e.key === "Escape" && !busy) {
                              e.stopPropagation();
                              keepRow(row.name, "remove");
                            }
                          }}
                        >
                          <p className="text-xs text-fg">Remove size {label}? Workers can no longer be provisioned with it.</p>
                          <div className="flex gap-2">
                            <Button ref={confirmButton} size="sm" variant="danger" disabled={locked} onClick={() => void remove(row)}>
                              {busy ? "Removing…" : "Remove"}
                            </Button>
                            <Button size="sm" variant="ghost" disabled={busy} onClick={() => keepRow(row.name, "remove")}>
                              Keep
                            </Button>
                          </div>
                        </div>
                      ) : isConfirming ? (
                        <div
                          role="group"
                          aria-label={`Confirm reset of size ${label}`}
                          className="mt-2 w-[8rem] space-y-2 font-normal sm:w-auto sm:max-w-[16rem]"
                          onKeyDown={(e) => {
                            if (e.key === "Escape" && !busy) {
                              e.stopPropagation();
                              keepRow(row.name, "reset");
                            }
                          }}
                        >
                          <p className="text-xs text-fg">
                            Reset {label} to shipped values?
                            {resetLowers && " Existing workers keep their larger disk; only new workers get the smaller size."}
                          </p>
                          <div className="flex gap-2">
                            <Button ref={confirmButton} size="sm" variant="secondary" disabled={locked} onClick={() => void reset(row)}>
                              {busy ? "Resetting…" : "Confirm"}
                            </Button>
                            <Button size="sm" variant="ghost" disabled={busy} onClick={() => keepRow(row.name, "reset")}>
                              Keep
                            </Button>
                          </div>
                        </div>
                      ) : (
                        !readOnly &&
                        !isEditing && (
                          <div className="mt-2 flex gap-2 font-normal">
                            <Button
                              ref={(el) => {
                                if (el) editButtons.current.set(row.name, el);
                                else editButtons.current.delete(row.name);
                              }}
                              size="sm"
                              variant="secondary"
                              aria-label={`Edit size ${label}`}
                              disabled={busy}
                              onClick={() => startEdit(row)}
                            >
                              Edit
                            </Button>
                            {canReset && (
                              <Button size="sm" variant="ghost" aria-label={`Reset size ${label} to shipped values`} disabled={busy} onClick={() => askConfirm(row, "reset")}>
                                Reset
                              </Button>
                            )}
                            {canRemove && (
                              <Button
                                ref={(el) => {
                                  if (el) removeButtons.current.set(row.name, el);
                                  else removeButtons.current.delete(row.name);
                                }}
                                size="sm"
                                variant="ghost"
                                aria-label={`Remove size ${label}`}
                                disabled={busy}
                                onClick={() => askConfirm(row, "remove")}
                              >
                                Remove
                              </Button>
                            )}
                          </div>
                        )
                      )}
                      {rowRefusal?.name === row.name && (
                        <div className="mt-2 w-[8rem] font-normal sm:w-auto sm:max-w-[16rem]">
                          <Alert message={rowRefusal.message} />
                        </div>
                      )}
                    </th>
                    <td className="whitespace-nowrap px-1.5 py-2 align-top tabular-nums sm:px-3">
                      <span className="text-muted">{cpuCores(row.cpu_request_millicores)}</span>
                      <span className="text-faint"> / </span>
                      <span className="text-fg">{cpuCores(row.cpu_limit_millicores)}</span>
                    </td>
                    <td className="whitespace-nowrap px-1.5 py-2 align-top tabular-nums sm:px-3">
                      <span className="text-muted">{memoryGiB(row.memory_request_mib)}</span>
                      <span className="text-faint"> / </span>
                      <span className="text-fg">{memoryGiB(row.memory_limit_mib)}</span>
                    </td>
                    <td className="px-1.5 py-2 align-top tabular-nums text-fg sm:px-3">{row.data_gib}</td>
                    <td className="px-1.5 py-2 align-top tabular-nums text-fg sm:px-3">{row.dind_data_gib}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>

      {/* The edit form sits below the table, outside its sideways scroll, so every input
          is on screen at any width. */}
      {editingRow && draft && (
        <form
          onSubmit={save}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !busy) {
              e.preventDefault();
              cancelEdit();
            }
          }}
          noValidate
          aria-label={`Edit size ${editLabel}`}
          className="space-y-3 rounded-lg border border-edge bg-raised/40 p-3 sm:p-4"
        >
          <h3 className="text-sm font-medium text-fg">Edit size {editLabel}</h3>
          <SizeFields
            idPrefix={`worker-size-${editingRow.name}`}
            labelPrefix={editLabel}
            draft={draft}
            errors={fieldErrors}
            locked={locked}
            firstInputRef={firstInput}
            onChange={(key, value) => {
              setDraft({ ...draft, [key]: value });
              if (fieldErrors[key]) setFieldErrors((prev) => withoutKey(prev, key));
            }}
          />
          {diskLowered && (
            <Alert tone="warning" message="Existing workers keep their larger disk; only new workers get the smaller size." />
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="submit" size="sm" disabled={locked}>
              {busy ? "Saving…" : `Save size ${editLabel}`}
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={cancelEdit} disabled={busy}>
              Cancel
            </Button>
          </div>
        </form>
      )}

      {/* Adding a size (M3): one quiet line under the table until asked for, then a form in
          the edit form's shape. Hidden while read-only, like every other write. */}
      {!readOnly && !adding && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
          {!atCap && (
            <Button ref={addButton} size="sm" variant="secondary" disabled={busy} onClick={startAdd}>
              Add size
            </Button>
          )}
          <p ref={capNote} tabIndex={-1} className="text-xs text-muted outline-none">
            {data.sizes.length} of {maxSizes} sizes.
            {atCap && ` At most ${maxSizes} sizes: remove an added size to add another.`}
          </p>
        </div>
      )}
      {adding && !atCap && addParsed && (
        <form
          onSubmit={add}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !busy) {
              e.preventDefault();
              cancelAdd();
            }
          }}
          noValidate
          aria-label="Add a size"
          className="space-y-3 rounded-lg border border-edge bg-raised/40 p-3 sm:p-4"
        >
          <h3 className="text-sm font-medium text-fg">Add a size</h3>
          <div className="max-w-xs space-y-1">
            <label htmlFor="add-worker-size-name" className="block text-xs font-medium text-muted">
              Name
            </label>
            <Input
              ref={addNameInput}
              id="add-worker-size-name"
              type="text"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              maxLength={32}
              aria-label="New size name"
              aria-invalid={fieldErrors.name ? true : undefined}
              aria-describedby={fieldErrors.name ? "add-worker-size-name-error add-worker-size-name-help" : "add-worker-size-name-help"}
              value={addName}
              disabled={locked}
              onChange={(e) => {
                setAddName(e.target.value);
                if (fieldErrors.name) setFieldErrors((prev) => withoutKey(prev, "name"));
              }}
              className="px-2.5 py-1.5 disabled:opacity-60 aria-invalid:border-danger/70"
            />
            {fieldErrors.name && (
              <p id="add-worker-size-name-error" className="text-xs text-danger [overflow-wrap:anywhere]">
                {fieldErrors.name}
              </p>
            )}
            <p id="add-worker-size-name-help" className="text-xs text-faint">
              A lowercase letter, then lowercase letters, digits or hyphens; at most 16 characters.
            </p>
          </div>
          <SizeFields
            idPrefix="add-worker-size"
            labelPrefix="New size"
            draft={addDraft}
            errors={fieldErrors}
            locked={locked}
            onChange={(key, value) => {
              setAddDraft({ ...addDraft, [key]: value });
              if (fieldErrors[key]) setFieldErrors((prev) => withoutKey(prev, key));
            }}
          />
          {/* What the provision form and the default-size selectors will offer, once the
              name and every quantity are valid: the size is checked where it will be read. */}
          {!addNameError && Object.keys(addParsed.errors).length === 0 && (
            <p className="text-xs text-muted">
              Pickers will show: <span className="text-fg">{sizeOptionLabel({ name: typedName, ...(addParsed.values as WorkerSizeSpec) })}</span>
            </p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="submit" size="sm" disabled={locked}>
              {busy ? "Adding…" : addNameError ? "Add size" : `Add size ${sizeLabel(typedName)}`}
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={cancelAdd} disabled={busy}>
              Cancel
            </Button>
          </div>
        </form>
      )}

      {advisoriesBySize.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-sm font-medium text-fg">Quota checks</h3>
          <p className="text-xs text-muted">
            Advisory only: a size that would exceed a tier's ResourceQuota still saves, and an actual rejection shows up in Admin health.
          </p>
          <ul className="space-y-1.5">
            {advisoriesBySize.flatMap((g) =>
              g.items.map((a, i) => (
                <li key={`${g.size}-${a.tier}-${a.kind}-${a.field}-${i}`} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-sm">
                  <Badge tone={ADVISORY_TONE[a.kind] ?? "neutral"}>{ADVISORY_LABEL[a.kind] ?? a.kind}</Badge>
                  <span className="text-fg">
                    {a.size ? sizeLabel(a.size) : "All sizes"}
                    {a.tier ? `, ${a.tier} tier` : ""}
                  </span>
                  <span className="basis-full text-xs text-muted">{a.message}</span>
                </li>
              )),
            )}
          </ul>
        </div>
      )}

      <p className="text-xs text-muted">
        A size the cluster refuses, above a controller ceiling or with a disk claim it rejects, is reported per worker
        on{" "}
        <Link to={HEALTH_ROUTE} className="text-info hover:underline">
          Admin health
        </Link>
        .
      </p>
    </Card>
  );
}
