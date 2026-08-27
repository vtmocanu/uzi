import { describe, it, expect } from "vitest";
import { CAPABILITY_VOCABULARY } from "./capabilityVocabulary";

// The drift gate for the capability-vocabulary mirror (PRD #512 M3).
//
// The authority is Go: api/internal/capability's Vocabulary() is the single source of
// truth for the v1 vocabulary {docker, jvm}, and its golden test writes that list to
// vocabulary.json. This test pins CAPABILITY_VOCABULARY against that same golden across
// the module boundary, exactly as workerSizes.test.ts pins WORKER_SIZES against
// hosted_sizes.json — so if the Go vocabulary changes and its golden is regenerated,
// this test reddens until the web const follows.
//
// The `?raw` import (not node:fs) is load-bearing and mirrors workerSizes.test.ts:
// web/Dockerfile copies only web/ and docs/, so a source file that imported the golden
// by path would break the image build. `?raw` matches vite/client's ambient wildcard
// module declaration, so tsc never resolves the path — `npm run build` type-checks this
// file clean with api/ absent — while vitest resolves it for real from a full checkout
// and the assertion has teeth. The flip side is that tsc cannot validate the path either:
// move or rename the golden and typecheck stays green, only `npm test` (a bare ENOENT that
// takes the suite down) tells you. CI runs the web tests from a full checkout, so the gate
// always fires.
import vocabRaw from "../../../api/internal/capability/testdata/vocabulary.json?raw";

const GOLDEN = "api/internal/capability/testdata/vocabulary.json";

const golden = JSON.parse(vocabRaw) as { vocabulary: string[] };

describe("CAPABILITY_VOCABULARY mirrors the Go capability vocabulary", () => {
  it("matches the cross-module golden, name for name and in order", () => {
    expect(
      [...CAPABILITY_VOCABULARY],
      `CAPABILITY_VOCABULARY (web/src/lib/capabilityVocabulary.ts) drifted from ${GOLDEN}.\n` +
        "The golden is generated from api/internal/capability's Vocabulary(), which is the " +
        "authority: if the vocabulary deliberately changed, update the constant here to match " +
        "it (and check the repo capability picker still reads sensibly). If the constant is " +
        "what changed, revert it — the web cannot offer a capability the server will not accept.",
    ).toEqual(golden.vocabulary);
  });
});
