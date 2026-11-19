import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type AdminWorkerSize,
  type AdminWorkerSizes,
  type WorkerSizeAdvisory,
  type WorkerSizeCeilings,
  type WorkerSizeSpec,
  type WorkerSizeWarning,
} from "../../lib/api";
import { ApiError, errorMessage } from "../../lib/apiError";
import { Alert, Badge, Button, Card, Input, SectionTitle, Skeleton, type BadgeTone } from "../../components/ui";
import {
  ABSOLUTE_MAX,
  cpuCores,
  memoryGiB,
  parseCores,
  parseDiskGiB,
  parseMemoryGiB,
  sizeLabel,
  type ParsedQuantity,
} from "../../lib/workerSizes";

// WorkerSizesCard is the Admin → Workers editor for hosted worker sizes (PRD #4213): what
// each size buys, in human units (CPU cores, GiB), edited per row and saved as the api's
// integers. The api is authoritative: it validates against the chart ceilings, answers 400
// with per-field errors, and 409 while the controller cannot honour an edit (the editor is
// then read-only and says why). This unit edits and resets existing sizes; adding and
// removing sizes come later, so rows are keyed by name and actions are per row.
//
// The list itself is owned by AdminWorkers, which reads it once and re-reads it after every
// successful write and after a 409 (onChanged), so the default-size selectors on the same page pick up an
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

type WriteResult = { name: string; verb: "saved" | "reset to shipped values"; warnings: WorkerSizeWarning[] };

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

export function WorkerSizesCard({
  data,
  loadError,
  onChanged,
}: {
  /** GET /admin/worker-sizes, null until the first read succeeds. */
  data: AdminWorkerSizes | null;
  /** The last read's error, "" when it succeeded. */
  loadError: string;
  /** Re-read the list and lift it to the page; called after every successful write, on a 409 and by Retry. */
  onChanged: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<WriteResult | null>(null);
  // The row whose Reset is waiting for Confirm / Keep.
  const [confirming, setConfirming] = useState<string | null>(null);
  // A 409 on a write: the controller stopped advertising support after the list was read.
  // It holds only until the next list arrives (the 409 itself asks for one), which is then
  // the authority on whether sizes are editable.
  const [conflict, setConflict] = useState<{ reason: string; against: AdminWorkerSizes | null } | null>(null);
  const conflictReason = conflict && conflict.against === data ? conflict.reason : null;

  // Focus management: Edit focuses the form's first input; Save, Cancel, Escape, Reset and
  // Keep hand focus back to that row's Edit button (looked up by size name, since the
  // button re-mounts after the form closes or the list is re-read).
  const editButtons = useRef(new Map<string, HTMLButtonElement>());
  const firstInput = useRef<HTMLInputElement>(null);
  const confirmButton = useRef<HTMLButtonElement>(null);
  // Set alongside the state change that renders the target; focused once it is mounted.
  const readOnlyBanner = useRef<HTMLDivElement>(null);
  const focusTarget = useRef<{ kind: "edit" | "form" | "confirm" | "banner"; name: string } | null>(null);
  const setFocusTarget = (t: { kind: "edit" | "form" | "confirm" | "banner"; name: string }) => {
    focusTarget.current = t;
  };
  useEffect(() => {
    const t = focusTarget.current;
    if (!t) return;
    const el =
      t.kind === "banner" ? readOnlyBanner.current
      : t.kind === "form" ? firstInput.current
      : t.kind === "confirm" ? confirmButton.current
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

  const startEdit = (row: AdminWorkerSize) => {
    setEditing(row.name);
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

  // Parse the whole draft; a field that cannot be an api integer, or is above a bound the
  // list already names, reports why locally, in the editor's units.
  const parsed = (() => {
    if (!draft) return null;
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
  })();

  const diskLowered =
    !!editingRow &&
    !!parsed &&
    ((parsed.values.data_gib !== undefined && parsed.values.data_gib < editingRow.data_gib) ||
      (parsed.values.dind_data_gib !== undefined && parsed.values.dind_data_gib < editingRow.dind_data_gib));

  // Map a refused write onto the editor: 409 → read-only until the re-read it triggers
  // lands; 400 fields → the inputs (each server message already names its chart path).
  const handleWriteError = (err: unknown, fallback: string, mapFields: boolean) => {
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

  const askReset = (row: AdminWorkerSize) => {
    if (locked) return;
    setConfirming(row.name);
    setFocusTarget({ kind: "confirm", name: row.name });
  };
  const keepRow = (name: string) => {
    setConfirming(null);
    setFocusTarget({ kind: "edit", name });
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

  const advisoriesBySize = data.sizes
    .map((s) => ({ size: s.name, items: data.advisories.filter((a) => a.size === s.name) }))
    .filter((g) => g.items.length > 0);
  const orphanAdvisories = data.advisories.filter((a) => !data.sizes.some((s) => s.name === a.size));
  if (orphanAdvisories.length > 0) advisoriesBySize.push({ size: "", items: orphanAdvisories });

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
      {error && <Alert message={error} />}
      {result && <Alert tone="success" message={`Size ${sizeLabel(result.name)} ${result.verb}.`} />}
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
                const isConfirming = confirming === row.name && canReset;
                const resetLowers =
                  !!row.shipped && (row.shipped.data_gib < row.data_gib || row.shipped.dind_data_gib < row.dind_data_gib);
                return (
                  <tr key={row.name} className={isEditing || isConfirming ? "bg-raised/40" : undefined}>
                    <th scope="row" className="px-1.5 py-2 align-top font-medium text-fg sm:px-3">
                      <div className="flex flex-col items-start gap-1 sm:flex-row sm:flex-wrap sm:items-center sm:gap-1.5">
                        <span className="whitespace-nowrap">{label}</span>
                        {row.builtin && <Badge>Builtin</Badge>}
                        {row.customized && <Badge tone="brand">Customized</Badge>}
                      </div>
                      {canReset && (
                        <p className="mt-1 w-[8rem] text-xs font-normal text-faint sm:w-auto sm:max-w-[16rem]">
                          Shipped: {specSummary(row.shipped!)}
                        </p>
                      )}
                      {isConfirming ? (
                        <div
                          role="group"
                          aria-label={`Confirm reset of size ${label}`}
                          className="mt-2 w-[8rem] space-y-2 font-normal sm:w-auto sm:max-w-[16rem]"
                          onKeyDown={(e) => {
                            if (e.key === "Escape" && !busy) {
                              e.stopPropagation();
                              keepRow(row.name);
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
                            <Button size="sm" variant="ghost" disabled={busy} onClick={() => keepRow(row.name)}>
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
                              <Button size="sm" variant="ghost" aria-label={`Reset size ${label} to shipped values`} disabled={busy} onClick={() => askReset(row)}>
                                Reset
                              </Button>
                            )}
                          </div>
                        )
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
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
            {FIELDS.map((f, i) => {
              const id = `worker-size-${editingRow.name}-${f.key}`;
              const err = fieldErrors[f.key];
              return (
                <div key={f.key} className="min-w-0 space-y-1">
                  <label htmlFor={id} className="block text-xs font-medium text-muted">
                    {f.label} ({f.unit})
                  </label>
                  <Input
                    ref={i === 0 ? firstInput : undefined}
                    id={id}
                    type="text"
                    inputMode={f.unit === "GiB" && f.key.startsWith("d") ? "numeric" : "decimal"}
                    aria-label={`${editLabel} ${f.label} (${f.unit})`}
                    aria-invalid={err ? true : undefined}
                    aria-describedby={err ? `${id}-error` : undefined}
                    value={draft[f.key]}
                    disabled={locked}
                    onChange={(e) => {
                      setDraft({ ...draft, [f.key]: e.target.value });
                      if (err) setFieldErrors((prev) => withoutKey(prev, f.key));
                    }}
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
        <Link to="/admin/health" className="text-info hover:underline">
          Admin health
        </Link>
        .
      </p>
    </Card>
  );
}
