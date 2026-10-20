import type { MaintenanceExclusion, MaintenanceWriterExclusion } from "../src/maintenance-exclusion.js";

/** A writer exclusion that always holds, for coordinator tests that exercise later stages.
 *  `entered` and `proofs` let a test assert the producer used it. */
export function passingExclusion(): Pick<MaintenanceWriterExclusion, "run"> & { entered: number; proofs: number } {
  const fake = {
    entered: 0, proofs: 0,
    run: async <T>(work: (exclusion: MaintenanceExclusion) => Promise<T>) => {
      fake.entered++;
      return { entered: true as const, value: await work({ open: () => true, prove: async () => { fake.proofs++; } }) };
    },
  };
  return fake;
}
