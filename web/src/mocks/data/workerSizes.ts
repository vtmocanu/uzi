import type { WorkerSizeSpec } from "../../lib/api";
import { BUILTIN_WORKER_SIZES } from "../../lib/workerSizes";

// builtinSizeSpec is a builtin size's six integers without its name, for the worker fixtures
// that carry hosted_size_spec (PRD #4213). The mock admin-sizes API starts from the same seed.
export function builtinSizeSpec(name: string): WorkerSizeSpec {
  const found = BUILTIN_WORKER_SIZES.find((s) => s.name === name);
  if (!found) throw new Error(`no builtin worker size "${name}"`);
  return {
    cpu_request_millicores: found.cpu_request_millicores,
    cpu_limit_millicores: found.cpu_limit_millicores,
    memory_request_mib: found.memory_request_mib,
    memory_limit_mib: found.memory_limit_mib,
    data_gib: found.data_gib,
    dind_data_gib: found.dind_data_gib,
  };
}
