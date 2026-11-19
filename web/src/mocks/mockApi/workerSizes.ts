import type { AdminWorkerSize, AdminWorkerSizes, AdminWorkerSizeWrite, HostedConfigSize, WorkerSizeAdvisory, WorkerSizeSpec, WorkerSizeWarning } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { BUILTIN_WORKER_SIZES } from "../../lib/workerSizes";
import { delay, requireAdmin } from "./shared";

// Admin worker sizes (PRD #4213): GET /admin/worker-sizes, PUT /admin/worker-sizes/{name} and
// POST /admin/worker-sizes/{name}/reset. The rows are module state, seeded from the builtin
// sizes, and they are the ONE size list the mock serves: hostedConfig and
// provisionHostedWorker (workers.ts) read it too, so an edit here shows up in the provision
// form the next time it mounts, as it does against the real api.
//
// Ceilings mirror a chart with 4 CPU / 32768 MiB / 50 GiB disk and DinD; the editor is
// editable; one quota_unavailable advisory shows how a missing quota reads (never as fits).

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

// The same checks the api's workersize.Validate makes against these ceilings, in its units.
function validate(spec: WorkerSizeSpec): Record<string, string> {
  const fields: Record<string, string> = {};
  for (const f of FIELDS) {
    if (!Number.isInteger(spec[f]) || spec[f] <= 0) fields[f] = "must be a positive whole number";
  }
  if (spec.cpu_request_millicores > spec.cpu_limit_millicores) fields.cpu_request_millicores = "must not exceed the CPU limit";
  if (spec.memory_request_mib > spec.memory_limit_mib) fields.memory_request_mib = "must not exceed the memory limit";
  const over = (f: "cpu_limit_millicores" | "memory_limit_mib" | "data_gib" | "dind_data_gib", unit: string) => {
    const c = CEILINGS[f];
    if (c && spec[f] > c.value) fields[f] = `must be at most ${c.value} ${unit} (${c.chart_path})`;
  };
  over("cpu_limit_millicores", "millicores");
  over("memory_limit_mib", "MiB");
  over("data_gib", "GiB");
  over("dind_data_gib", "GiB");
  return fields;
}

function warnings(prev: WorkerSizeSpec, next: WorkerSizeSpec): WorkerSizeWarning[] {
  const out: WorkerSizeWarning[] = [];
  if (next.data_gib < prev.data_gib) out.push({ kind: "disk_lowered", field: "data_gib", message: `data disk lowered from ${prev.data_gib} GiB to ${next.data_gib} GiB: existing claims keep their size; only new claims use the lower value` });
  if (next.dind_data_gib < prev.dind_data_gib) out.push({ kind: "disk_lowered", field: "dind_data_gib", message: `DinD data disk lowered from ${prev.dind_data_gib} GiB to ${next.dind_data_gib} GiB: existing claims keep their size; only new claims use the lower value` });
  return out;
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
      editable: true,
      read_only_reason: null,
      read_only_code: null,
      advisories: advisories(),
      max_sizes: 16,
    });
  },
  updateAdminWorkerSize: async (name: string, spec: WorkerSizeSpec): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    const row = find(name);
    return delay(write(row, pick(spec), row.builtin && (!row.shipped || !same(row.shipped, spec))));
  },
  resetAdminWorkerSize: async (name: string): Promise<AdminWorkerSizeWrite> => {
    requireAdmin();
    const row = find(name);
    if (!row.builtin || !row.shipped) throw new ApiError(400, "only builtin worker sizes can be reset");
    return delay(write(row, row.shipped, false));
  },
};
