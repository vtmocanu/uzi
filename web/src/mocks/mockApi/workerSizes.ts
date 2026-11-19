import type { AdminWorkerSize, AdminWorkerSizes, AdminWorkerSizeWrite, HostedConfigSize, WorkerSizeAdvisory, WorkerSizeSpec, WorkerSizeWarning } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { ABSOLUTE_MAX, BUILTIN_WORKER_SIZES } from "../../lib/workerSizes";
import { delay, mockScenario, requireAdmin } from "./shared";

// Admin worker sizes (PRD #4213): GET /admin/worker-sizes, PUT /admin/worker-sizes/{name} and
// POST /admin/worker-sizes/{name}/reset. The rows are module state, seeded from the builtin
// sizes, and they are the ONE size list the mock serves: hostedConfig and
// provisionHostedWorker (workers.ts) read it too, so an edit here shows up in the provision
// form the next time it mounts, as it does against the real api.
//
// Ceilings mirror a chart with 4 CPU / 32768 MiB / 50 GiB disk and DinD; the editor is
// editable (read-only under ?mock=sizes-readonly); one quota_unavailable advisory shows how a
// missing quota reads (never as fits). Field errors and warnings use the api's exact text.

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
      max_sizes: 16,
    });
  },
  updateAdminWorkerSize: async (name: string, spec: WorkerSizeSpec): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    refuseIfReadOnly();
    const row = find(name);
    return delay(write(row, pick(spec), row.builtin && (!row.shipped || !same(row.shipped, spec))));
  },
  resetAdminWorkerSize: async (name: string): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    refuseIfReadOnly();
    const row = find(name);
    if (!row.builtin || !row.shipped) throw new ApiError(400, "only builtin worker sizes can be reset");
    return delay(write(row, row.shipped, false));
  },
};
