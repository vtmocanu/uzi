import { describe, it, expect } from "vitest";
import { WORKER_SIZES } from "./workerSizes";

// The drift gate for the size mirror (PRD #58 Decision 7).
//
// api/internal/workersize is the authority on which size names are legal; this test
// pins WORKER_SIZES to the golden that registry generates, so the mirror cannot drift
// silently. Drift is not cosmetic in either direction: a size the web offers but the
// api dropped is a 400 the user cannot avoid, and a size the api gained but the web
// omits is a preset nobody can pick.
//
// Reaching into the api tree from a web test mirrors agent/test/claim-skills-contract
// .test.ts, which reads api/internal/workersvc/testdata the same way, and the same
// caveat applies: this catches DEV-TIME drift, never deployment skew.
//
// The `?raw` import, not node:fs, and that is load-bearing. web/Dockerfile copies
// web/ and docs/ and nothing else, so a shipped source file that imported this golden
// would build locally and break the image. `?raw` matches vite/client's ambient
// wildcard module declaration, so tsc never resolves the path — `npm run build`
// inside the image type-checks this file clean with api/ absent, while vitest (which
// runs from a full checkout) resolves it for real and the assertion has teeth.
// node:fs would need @types/node, which this browser project deliberately lacks.
import goldenRaw from "../../../api/internal/hostedsvc/testdata/hosted_sizes.json?raw";

const GOLDEN = "api/internal/hostedsvc/testdata/hosted_sizes.json";

describe("WORKER_SIZES mirrors the api's size registry", () => {
  it("matches the cross-module golden, name for name and in order", () => {
    const golden = JSON.parse(goldenRaw) as { sizes: string[] };
    expect(
      [...WORKER_SIZES],
      `WORKER_SIZES (web/src/lib/workerSizes.ts) drifted from ${GOLDEN}.\n` +
        "The golden is generated from api/internal/workersize, which is the authority: " +
        "if the registry deliberately changed, update the constant here to match it " +
        "(and check the size select still reads sensibly). If the constant is what " +
        "changed, revert it — the web cannot add a size the api will not accept.",
    ).toEqual(golden.sizes);
  });

  it("carries no quantities — the controller is the authority on those (M6)", () => {
    // Guards the deferral, not a value: the moment someone adds "s = 1 CPU, 2Gi" to
    // the constant, they have invented a number no artifact in this repo has chosen,
    // and M3's real preset table will contradict it silently. The golden is names-only
    // for the same reason.
    for (const size of WORKER_SIZES) {
      expect(size).toMatch(/^[a-z]+$/);
    }
  });
});
