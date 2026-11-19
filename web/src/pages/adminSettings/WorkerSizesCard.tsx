import { useState, type FormEvent } from "react";
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
// successful write (onChanged), so the default-size selectors on the same page pick up an
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

const HEAD = "px-2 py-2 font-medium sm:px-3";

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

export function WorkerSizesCard({
  data,
  loadError,
  onChanged,
}: {
  /** GET /admin/worker-sizes, null until the first read succeeds. */
  data: AdminWorkerSizes | null;
  /** The last read's error, "" when it succeeded. */
  loadError: string;
  /** Re-read the list and lift it to the page; called after every successful write and by Retry. */
  onChanged: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<WriteResult | null>(null);
  // A 409 on a write: the controller stopped advertising support after the list was read.
  const [conflictReason, setConflictReason] = useState<string | null>(null);

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

  const startEdit = (row: AdminWorkerSize) => {
    setEditing(row.name);
    setDraft(draftOf(row));
    setFieldErrors({});
    setError("");
    setResult(null);
  };
  const cancelEdit = () => {
    setEditing(null);
    setDraft(null);
    setFieldErrors({});
    setError("");
  };

  // Parse the whole draft; a field that cannot be an api integer reports why, locally.
  const parsed = (() => {
    if (!draft) return null;
    const values: Partial<WorkerSizeSpec> = {};
    const errors: Record<string, string> = {};
    for (const f of FIELDS) {
      const p = f.parse(draft[f.key]);
      if (p.ok) values[f.key] = p.value;
      else errors[f.key] = p.error;
    }
    if (values.cpu_request_millicores !== undefined && values.cpu_limit_millicores !== undefined && values.cpu_request_millicores > values.cpu_limit_millicores) {
      errors.cpu_request_millicores = "The request cannot be above the limit";
    }
    if (values.memory_request_mib !== undefined && values.memory_limit_mib !== undefined && values.memory_request_mib > values.memory_limit_mib) {
      errors.memory_request_mib = "The request cannot be above the limit";
    }
    return { values, errors };
  })();

  const diskLowered =
    !!editingRow &&
    !!parsed &&
    ((parsed.values.data_gib !== undefined && parsed.values.data_gib < editingRow.data_gib) ||
      (parsed.values.dind_data_gib !== undefined && parsed.values.dind_data_gib < editingRow.dind_data_gib));

  // Map a refused write onto the editor: 409 → read-only, 400 fields → the inputs.
  const handleWriteError = (err: unknown, fallback: string, mapFields: boolean) => {
    if (err instanceof ApiError && err.status === 409) {
      const body = err.body as { read_only_reason?: string } | null;
      setConflictReason(body?.read_only_reason || err.message);
      return;
    }
    if (err instanceof ApiError && err.status === 400) {
      const fields = (err.body as { fields?: Record<string, string> } | null)?.fields ?? {};
      const known = Object.entries(fields).filter(([k]) => mapFields && FIELD_KEYS.has(k));
      const other = Object.entries(fields).filter(([k]) => !(mapFields && FIELD_KEYS.has(k)));
      if (known.length > 0) setFieldErrors(Object.fromEntries(known));
      const detail = other.map(([k, v]) => `${FIELDS.find((f) => f.key === k)?.label ?? k}: ${v}`).join("; ");
      setError(detail ? `${err.message}. ${detail}` : err.message);
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
      setEditing(null);
      setDraft(null);
      setResult({ name: editingRow.name, verb: "saved", warnings: resp.warnings });
      await onChanged();
    } catch (err) {
      handleWriteError(err, `Failed to save size ${sizeLabel(editingRow.name)}`, true);
    } finally {
      setBusy(false);
    }
  };

  const reset = async (row: AdminWorkerSize) => {
    if (locked) return;
    setError("");
    setResult(null);
    setFieldErrors({});
    setBusy(true);
    try {
      const resp = await api.resetAdminWorkerSize(row.name);
      if (editing === row.name) cancelEdit();
      setResult({ name: row.name, verb: "reset to shipped values", warnings: resp.warnings });
      await onChanged();
    } catch (err) {
      // A reset has no inputs to map onto, so every field error is spelled out in the alert.
      handleWriteError(err, `Failed to reset size ${sizeLabel(row.name)}`, false);
    } finally {
      setBusy(false);
    }
  };

  const advisoriesBySize = data.sizes
    .map((s) => ({ size: s.name, items: data.advisories.filter((a) => a.size === s.name) }))
    .filter((g) => g.items.length > 0);
  const orphanAdvisories = data.advisories.filter((a) => !data.sizes.some((s) => s.name === a.size));
  if (orphanAdvisories.length > 0) advisoriesBySize.push({ size: "", items: orphanAdvisories });

  const tiers = data.ceilings.enabled_tiers;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Worker sizes</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          What each hosted worker size buys. The request is reserved on a node; the limit is the most a worker can use.
          Saving a CPU or memory change rolls every idle worker of that size once onto the new values, and a busy one
          when it goes idle. A disk-only change rolls nothing: claims grow in place.
        </p>
        <p className="mt-2 text-sm text-muted">{ceilingsLine(data.ceilings)}</p>
        {tiers && tiers.length > 0 && (
          <p className="mt-1 text-xs text-faint">Checked against the enabled worker tiers: {tiers.join(", ")}.</p>
        )}
        {data.ceilings.dind_overridden && (
          <p className="mt-1 text-xs text-faint">
            The DinD data disk is fixed for every size by <code className="font-mono text-fg">workers.docker.dindDataSize</code>, so
            the DinD value here is not used while that override is set.
          </p>
        )}
      </div>

      {readOnly && <Alert tone="warning" message={`Sizes are read-only. ${readOnlyReason}`} />}
      {loadError && <Alert message={loadError} />}
      {error && <Alert message={error} />}
      {result && <Alert tone="success" message={`Size ${sizeLabel(result.name)} ${result.verb}.`} />}
      {result?.warnings.map((w, i) => <Alert key={`${w.field}-${i}`} tone="warning" message={w.message} />)}

      <div className="space-y-2">
      <p className="text-xs text-muted">Request / limit. CPU in cores; memory and disks in GiB.</p>
      <div className="overflow-x-auto rounded-lg border border-edge">
        <table className="w-full text-left text-sm">

          <thead className="border-b border-edge text-muted">
            <tr>
              <th scope="col" className={HEAD}>Size</th>
              <th scope="col" className={HEAD}>CPU <span className="sr-only">(cores, request / limit)</span></th>
              <th scope="col" className={HEAD}>Memory <span className="sr-only">(GiB, request / limit)</span></th>
              <th scope="col" className={HEAD}>Data <span className="sr-only">disk (GiB)</span></th>
              <th scope="col" className={HEAD}>DinD <span className="sr-only">data disk (GiB)</span></th>
              <th scope="col" className={HEAD}><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-edge">
            {data.sizes.map((row) => {
              const label = sizeLabel(row.name);
              const isEditing = editing === row.name && draft !== null;
              const canReset = row.builtin && row.customized && row.shipped !== null;
              return [
                <tr key={row.name} className={isEditing ? "bg-raised/40" : undefined}>
                  <th scope="row" className="px-2 py-2 align-top sm:px-3 font-medium text-fg">
                    <div className="flex flex-col items-start gap-1 sm:flex-row sm:flex-wrap sm:items-center sm:gap-1.5">
                      <span>{label}</span>
                      {row.builtin && <Badge>Builtin</Badge>}
                      {row.customized && <Badge tone="brand">Customized</Badge>}
                    </div>
                    {canReset && (
                      <p className="mt-1 max-w-[16rem] text-xs font-normal text-faint">Shipped: {specSummary(row.shipped!)}</p>
                    )}
                  </th>
                  <td className="whitespace-nowrap px-2 py-2 align-top tabular-nums sm:px-3">
                    <span className="text-muted">{cpuCores(row.cpu_request_millicores)}</span>
                    <span className="text-faint"> / </span>
                    <span className="text-fg">{cpuCores(row.cpu_limit_millicores)}</span>
                  </td>
                  <td className="whitespace-nowrap px-2 py-2 align-top tabular-nums sm:px-3">
                    <span className="text-muted">{memoryGiB(row.memory_request_mib)}</span>
                    <span className="text-faint"> / </span>
                    <span className="text-fg">{memoryGiB(row.memory_limit_mib)}</span>
                  </td>
                  <td className="px-2 py-2 align-top sm:px-3 tabular-nums text-fg">{row.data_gib}</td>
                  <td className="px-2 py-2 align-top sm:px-3 tabular-nums text-fg">{row.dind_data_gib}</td>
                  <td className="px-2 py-2 align-top sm:px-3">
                    <div className="flex justify-end gap-2">
                      {!isEditing && (
                        <Button size="sm" variant="secondary" aria-label={`Edit size ${label}`} disabled={locked} onClick={() => startEdit(row)}>
                          Edit
                        </Button>
                      )}
                      {canReset && (
                        <Button size="sm" variant="ghost" aria-label={`Reset size ${label} to shipped values`} disabled={locked} onClick={() => void reset(row)}>
                          Reset
                        </Button>
                      )}
                    </div>
                  </td>
                </tr>,
                isEditing && (
                  <tr key={`${row.name}-edit`} className="bg-raised/40">
                    <td colSpan={6} className="px-2 pb-4 pt-1 sm:px-3">
                      <form onSubmit={save} noValidate aria-label={`Edit size ${label}`} className="space-y-3">
                        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
                          {FIELDS.map((f) => {
                            const id = `worker-size-${row.name}-${f.key}`;
                            const err = fieldErrors[f.key];
                            return (
                              <div key={f.key} className="space-y-1">
                                <label htmlFor={id} className="block text-xs font-medium text-muted">
                                  {f.label} ({f.unit})
                                </label>
                                <Input
                                  id={id}
                                  type="text"
                                  inputMode={f.unit === "GiB" && f.key.startsWith("d") ? "numeric" : "decimal"}
                                  aria-label={`${label} ${f.label} (${f.unit})`}
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
                                  <p id={`${id}-error`} className="text-xs text-danger">
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
                            {busy ? "Saving…" : `Save size ${label}`}
                          </Button>
                          <Button type="button" size="sm" variant="ghost" onClick={cancelEdit} disabled={busy}>
                            Cancel
                          </Button>
                        </div>
                      </form>
                    </td>
                  </tr>
                ),
              ];
            })}
          </tbody>
        </table>
      </div>
      </div>

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
