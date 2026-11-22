import type { AdminWorkerSize, AdminWorkerSizes, AdminWorkerSizeWrite, HostedConfigSize, WorkerSizeAdvisory, WorkerSizeRefusal, WorkerSizeSpec, WorkerSizeWarning } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { ABSOLUTE_MAX, BUILTIN_WORKER_SIZES, MAX_WORKER_SIZES, SIZE_NAME_PATTERN } from "../../lib/workerSizes";
import { delay, mockScenario, requireAdmin } from "./shared";
import { mockStoredSizeSettings } from "./settings";
import { workers } from "./workers";

// Admin worker sizes (PRD #4213): GET /admin/worker-sizes, PUT /admin/worker-sizes/{name} (edit,
// or create an absent name, M3), DELETE /admin/worker-sizes/{name} (M3) and
// POST /admin/worker-sizes/{name}/reset. The rows are module state, seeded from the builtin
// sizes, and they are the ONE size list the mock serves: hostedConfig and
// provisionHostedWorker (workers.ts) read it too, so an edit here shows up in the provision
// form the next time it mounts, as it does against the real api.
//
// Ceilings mirror a chart with 4 CPU / 32768 MiB / 50 GiB disk and DinD; the editor is
// editable (read-only under ?mock=sizes-readonly); one quota_unavailable advisory shows how a
// missing quota reads (never as fits); one size_refused refusal shows a refused size in its
// row. Field errors and warnings use the api's exact text.

const FIELDS = ["cpu_request_millicores", "cpu_limit_millicores", "memory_request_mib", "memory_limit_mib", "data_gib", "dind_data_gib"] as const;

const CEILINGS: AdminWorkerSizes["ceilings"] = {
  cpu_limit_millicores: { value: 4000, chart_path: "workers.limitRange.max.cpu" },
  memory_limit_mib: { value: 32768, chart_path: "workers.limitRange.max.memory" },
  data_gib: { value: 50, chart_path: "workers.limitRange.maxPVCStorage" },
  dind_data_gib: { value: 50, chart_path: "workers.docker.limitRange.maxPVCStorage" },
  dind_overridden: false,
  enabled_tiers: ["restricted", "docker"],
};

const pick = (s: WorkerSizeSpec): WorkerSizeSpec => ({
  cpu_request_millicores: s.cpu_request_millicores,
  cpu_limit_millicores: s.cpu_limit_millicores,
  memory_request_mib: s.memory_request_mib,
  memory_limit_mib: s.memory_limit_mib,
  data_gib: s.data_gib,
  dind_data_gib: s.dind_data_gib,
});

const seed = (): AdminWorkerSize[] =>
  BUILTIN_WORKER_SIZES.map((s, i) => ({
    name: s.name,
    ...pick(s),
    display_order: i,
    builtin: true,
    customized: false,
    shipped: pick(s),
    updated_at: "2026-11-01T09:00:00Z",
  }));

let rows: AdminWorkerSize[] = seed();

/** Test seam: back to the seeded builtins. */
export function resetMockWorkerSizes(): void {
  rows = seed();
}

/** The size list hostedConfig ships, in display order. */
export function mockHostedConfigSizes(): HostedConfigSize[] {
  return rows.map((r) => ({ name: r.name, ...pick(r) }));
}

/** A size's current integers, for a worker's hosted_size_spec; undefined for an unknown name. */
export function mockSizeSpec(name: string): WorkerSizeSpec | undefined {
  const row = rows.find((r) => r.name === name);
  return row ? pick(row) : undefined;
}

function advisories(): WorkerSizeAdvisory[] {
  // The restricted tier's quota is not readable in this demo: the api says so explicitly.
  return [{
    size: "l-highmem",
    tier: "restricted",
    kind: "quota_unavailable",
    field: "",
    message: "quota advisory unavailable for the restricted tier: its quota values (workers.quota.*) are not available to the api",
  }];
}

// One worker the controller would not run at its size, as the api reports it: the demo's
// cordoned M worker. Served only while its size exists, as the api does.
const REFUSALS: WorkerSizeRefusal[] = [{
  size: "m",
  worker_id: "w-cordon-eu",
  worker_name: "base.m-9f3c",
  kind: "size_refused",
  reason: "cpu limit 4000 millicores exceeds the controller's 3000 millicores ceiling",
  reported_at: "2026-11-18T14:20:00Z",
}];

const byteOrder = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

// The api's per-size cap on refusal details; the totals count every refusal before it.
const MAX_REFUSALS_PER_SIZE = 3;

// The api's shape for the sizes the card can show: listed sizes only (the api may also return
// unlisted sizes, which have no row), sorted by size, worker name, then id in code-unit order (Go's
// byte order for ASCII names), at most three details per size, and per size the count before that cap.
function refusals(): { refusals: WorkerSizeRefusal[]; refusal_totals: Record<string, number> } {
  const listed = REFUSALS.filter((r) => rows.some((row) => row.name === r.size)).sort(
    (a, b) => byteOrder(a.size, b.size) || byteOrder(a.worker_name, b.worker_name) || byteOrder(a.worker_id, b.worker_id),
  );
  const refusal_totals: Record<string, number> = {};
  const kept: WorkerSizeRefusal[] = [];
  for (const r of listed) {
    const n = refusal_totals[r.size] ?? 0;
    refusal_totals[r.size] = n + 1;
    if (n < MAX_REFUSALS_PER_SIZE) kept.push(r);
  }
  return { refusals: kept, refusal_totals };
}

// Each field's unit, and the api's absolute cap per unit (api/internal/workersize/builtin),
// whatever the chart allows.
const UNIT: Record<(typeof FIELDS)[number], "millicores" | "MiB" | "GiB"> = {
  cpu_request_millicores: "millicores",
  cpu_limit_millicores: "millicores",
  memory_request_mib: "MiB",
  memory_limit_mib: "MiB",
  data_gib: "GiB",
  dind_data_gib: "GiB",
};
const CAP = { millicores: ABSOLUTE_MAX.millicores, MiB: ABSOLUTE_MAX.memoryMiB, GiB: ABSOLUTE_MAX.diskGiB } as const;

// The same checks, in the same order and with the same text, as the api's
// workersize.Validate (api/internal/workersize/validate.go) against these ceilings.
function validate(spec: WorkerSizeSpec): Record<string, string> {
  const fields: Record<string, string> = {};
  const ok: Record<string, boolean> = {};
  for (const f of FIELDS) {
    const v = spec[f];
    const unit = UNIT[f];
    if (!(v > 0)) fields[f] = `must be greater than 0 ${unit}`;
    else if (v > CAP[unit]) fields[f] = `${v} ${unit} exceeds the absolute maximum of ${CAP[unit]} ${unit}`;
    else ok[f] = true;
  }
  if (ok.cpu_request_millicores && ok.cpu_limit_millicores && spec.cpu_request_millicores > spec.cpu_limit_millicores) {
    fields.cpu_request_millicores = `cpu request ${spec.cpu_request_millicores} millicores exceeds the cpu limit of ${spec.cpu_limit_millicores} millicores`;
  }
  if (ok.memory_request_mib && ok.memory_limit_mib && spec.memory_request_mib > spec.memory_limit_mib) {
    fields.memory_request_mib = `memory request ${spec.memory_request_mib} MiB exceeds the memory limit of ${spec.memory_limit_mib} MiB`;
  }
  const ceiling = (f: "cpu_limit_millicores" | "memory_limit_mib" | "data_gib" | "dind_data_gib", what: string) => {
    const c = CEILINGS[f];
    if (ok[f] && c && spec[f] > c.value) fields[f] = `${what} ${spec[f]} ${UNIT[f]} exceeds the ${c.value} ${UNIT[f]} ceiling (${c.chart_path})`;
  };
  ceiling("cpu_limit_millicores", "cpu limit");
  ceiling("memory_limit_mib", "memory limit");
  ceiling("data_gib", "data disk");
  ceiling("dind_data_gib", "dind data disk");
  return fields;
}

function warnings(prev: WorkerSizeSpec, next: WorkerSizeSpec): WorkerSizeWarning[] {
  const out: WorkerSizeWarning[] = [];
  const lowered = (field: "data_gib" | "dind_data_gib", what: string) => {
    if (next[field] < prev[field]) {
      out.push({ kind: "disk_lowered", field, message: `${what} lowered from ${prev[field]} GiB to ${next[field]} GiB: existing claims keep their size; only new claims use the lower value` });
    }
  };
  lowered("data_gib", "data disk");
  lowered("dind_data_gib", "dind data disk");
  return out;
}

// `?mock=sizes-readonly`: the controller does not advertise admin worker sizes, so the list
// is read-only with the api's reason and every write is refused 409, as the api does.
const READ_ONLY_CODE = "controller_unsupported";
const READ_ONLY_REASON = "The controller does not support admin worker sizes yet (upgrade it); edits would be ignored.";
const readOnly = () => mockScenario() === "sizes-readonly";

function refuseIfReadOnly(): void {
  if (!readOnly()) return;
  const error = `worker sizes are read-only: ${READ_ONLY_REASON}`;
  throw new ApiError(409, error, { error, reason: READ_ONLY_CODE, read_only_reason: READ_ONLY_REASON });
}

function find(name: string): AdminWorkerSize {
  const row = rows.find((r) => r.name === name);
  if (!row) throw new ApiError(404, "worker size not found");
  return row;
}

function write(row: AdminWorkerSize, spec: WorkerSizeSpec, customized: boolean): AdminWorkerSizeWrite {
  const fields = validate(spec);
  if (Object.keys(fields).length > 0) throw new ApiError(400, "the size has values that cannot be saved", { error: "the size has values that cannot be saved", fields });
  const next: AdminWorkerSize = { ...row, ...pick(spec), customized, updated_at: new Date().toISOString() };
  const warns = warnings(row, spec);
  rows = rows.map((r) => (r.name === row.name ? next : r));
  return { size: { ...next }, warnings: warns, advisories: advisories().filter((a) => a.size === row.name) };
}

const same = (a: WorkerSizeSpec, b: WorkerSizeSpec) => FIELDS.every((f) => a[f] === b[f]);

// The api's text for a name nothing could ever be stored under (handler AdminPutWorkerSize).
const INVALID_NAME = "invalid worker size name: use a lowercase letter then lowercase letters, digits or hyphens, at most 16 characters";

// PUT, as the api serves it: the gate, then the name, then an edit of an existing row or the
// create arm (validate, then the 16-row cap, then insert after the highest display_order).
function put(name: string, spec: WorkerSizeSpec): AdminWorkerSizeWrite {
  refuseIfReadOnly();
  if (!SIZE_NAME_PATTERN.test(name)) throw new ApiError(400, INVALID_NAME);
  const row = rows.find((r) => r.name === name);
  if (row) return write(row, spec, row.builtin && (!row.shipped || !same(row.shipped, spec)));
  const fields = validate(spec);
  if (Object.keys(fields).length > 0) throw new ApiError(400, "the size has values that cannot be saved", { error: "the size has values that cannot be saved", fields });
  if (rows.length >= MAX_WORKER_SIZES) {
    const error = `at most ${MAX_WORKER_SIZES} worker sizes`;
    throw new ApiError(409, error, { error, reason: "max_sizes" });
  }
  const created: AdminWorkerSize = {
    name,
    ...spec,
    display_order: Math.max(0, ...rows.map((r) => r.display_order)) + 1,
    builtin: false,
    customized: false,
    shipped: null,
    updated_at: new Date().toISOString(),
  };
  rows = [...rows, created];
  return { size: { ...created }, warnings: [], advisories: [] };
}

// DELETE's guard, in the api's order and text (store.DeleteWorkerSizeGuarded): absent → 404,
// builtin → 409, then a worker using it and a stored default-size setting naming it → 409.
function remove(name: string): { deleted: string } {
  refuseIfReadOnly();
  if (!SIZE_NAME_PATTERN.test(name)) throw new ApiError(404, "worker size not found");
  const row = find(name);
  if (row.builtin) {
    const error = "builtin sizes cannot be removed; reset it instead";
    throw new ApiError(409, error, { error, reason: "builtin", used_by: { workers: 0, settings: [] } });
  }
  const using = workers.filter((w) => w.hosted_size === name).length;
  const settings = mockStoredSizeSettings(name);
  if (using > 0 || settings.length > 0) {
    const parts: string[] = [];
    if (using > 0) parts.push(`${using} worker(s) still use it`);
    if (settings.length > 0) parts.push(`it is the stored value of ${settings.join(", ")}`);
    const error = `size is in use: ${parts.join("; ")}`;
    throw new ApiError(409, error, { error, reason: "in_use", used_by: { workers: using, settings } });
  }
  rows = rows.filter((r) => r.name !== name);
  return { deleted: name };
}

export const workerSizesApi = {
  getAdminWorkerSizes: async (): Promise<AdminWorkerSizes> => {
    requireAdmin();
    return delay({
      sizes: rows.map((r) => ({ ...r })),
      ceilings: CEILINGS,
      editable: !readOnly(),
      read_only_reason: readOnly() ? READ_ONLY_REASON : null,
      read_only_code: readOnly() ? READ_ONLY_CODE : null,
      advisories: advisories(),
      max_sizes: MAX_WORKER_SIZES,
      ...refusals(),
    });
  },
  // Both verbs are the api's one PUT. Without a precondition an existing name is edited and an
  // absent one created (update); the create verb sends If-None-Match: * and so is create-only.
  updateAdminWorkerSize: async (name: string, spec: WorkerSizeSpec): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    return delay(put(name, pick(spec)));
  },
  createAdminWorkerSize: async (name: string, spec: WorkerSizeSpec): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    refuseIfReadOnly();
    if (!SIZE_NAME_PATTERN.test(name)) throw new ApiError(400, INVALID_NAME);
    if (rows.some((r) => r.name === name)) {
      const error = `worker size ${name} already exists`;
      throw new ApiError(412, error, { error, reason: "size_exists" });
    }
    return delay(put(name, pick(spec)));
  },
  deleteAdminWorkerSize: async (name: string): Promise<{ deleted: string }> => {
    requireAdmin();
    return delay(remove(name));
  },
  resetAdminWorkerSize: async (name: string): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    refuseIfReadOnly();
    const row = find(name);
    if (!row.builtin || !row.shipped) throw new ApiError(400, "only builtin worker sizes can be reset");
    return delay(write(row, row.shipped, false));
  },
};
