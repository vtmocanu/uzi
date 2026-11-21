// PRD 4416 M2: a RecoveryCoordinator publishes only after resolving the exact-value scan needles of its run, and a
// coordinator built without a resolver withholds every publish (fail closed). Tests that do not exercise the scan
// spread this to say "the run holds no secret variables".
import type { NeedleResolution } from "../src/run-env-scan.js";

export const NO_SCAN_NEEDLES: { scanNeedlesFor: () => Promise<NeedleResolution> } = {
  scanNeedlesFor: async () => ({ kind: "needles", needles: [] }),
};
