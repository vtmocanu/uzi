import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { ClaimConfig, ClaimResponse } from "../src/protocol.js";
import { flagCIConfigPaths } from "../src/ci-config-guard.js";

// The worker side of the PRD #6 ci_fix wire contract. It parses the SAME golden
// file the Go producer test pins (api/internal/workersvc/ci_fix_test.go →
// TestCIFixClaimWireContract), so the ci_fix-specific fields (kind, null
// issue_iid, pipeline snapshot) can never drift into two lenient fakes. Typing the
// parsed value as ClaimResponse also makes `npm run typecheck` fail if a field this
// test reads is missing from protocol.ts.
const fixture = join(
  import.meta.dirname,
  "..",
  "..",
  "api",
  "internal",
  "workersvc",
  "testdata",
  "claim_ci_fix_wire.json",
);

test("ci_fix claim wire contract: worker parses the server's ci_fix shape", () => {
  const claim = JSON.parse(readFileSync(fixture, "utf8")) as ClaimResponse;

  // A ci_fix run: kind set, no issue.
  assert.equal(claim.kind, "ci_fix");
  assert.equal(claim.issue_iid, null);

  // PRD #246: the trust flag rides every repo block byte-for-byte (no omitempty on
  // the Go side), false here since this fixture's repo did not opt in — the
  // non-default counterpart to the `true` pinned in claim-skills-contract.test.ts.
  assert.equal(claim.repo.claudemd_enabled, false);

  // The failed-pipeline snapshot the agent diagnoses.
  assert.ok(claim.pipeline, "ci_fix claim must carry a pipeline snapshot");
  assert.equal(claim.pipeline!.id, 4200);
  assert.equal(claim.pipeline!.ref, "main");
  assert.equal(typeof claim.pipeline!.sha, "string");
  assert.equal(typeof claim.pipeline!.web_url, "string");

  // Failed jobs carry identity + a log tail (untrusted evidence).
  assert.ok(Array.isArray(claim.pipeline!.failed_jobs));
  assert.equal(claim.pipeline!.failed_jobs.length, 1);
  const job = claim.pipeline!.failed_jobs[0]!;
  assert.equal(job.name, "unit");
  assert.equal(job.stage, "test");
  assert.equal(typeof job.log_tail, "string");

  // PRD #209: an ordinary (worker-planned) run carries plan_source "agent" — the
  // DEFAULT and the value that leaves the session/approval discriminator unchanged.
  // Pinned here as the non-seeded counterpart to the seeded golden in
  // claim-skills-contract.test.ts.
  assert.equal(claim.plan_source, "agent");

  // PRD #400 M4b: the diff-review target rides every claim, top-level. This ci_fix
  // golden is not a review claim, so the field is null (the wire carries it
  // byte-for-byte); typing the parse as ClaimResponse fails `npm run typecheck` if it is
  // dropped from protocol.ts.
  assert.equal(claim.review_target_run_id, null);
});

// PRD #71 M5 cross-side contract: the server produces the guard's protected-path
// set as `ClaimConfig.CIConfigPaths` with the json tag `ci_config_paths` (see
// api/internal/workersvc/claim.go), and the worker's pre-push guard reads exactly
// that field (`claim.config.ci_config_paths`) and drives flagCIConfigPaths with it.
// A drift in the wire name between the two sides would silently fail the guard OPEN
// (an empty path set flags nothing), so both halves are pinned here.
test("ci_fix guard contract: config.ci_config_paths reaches the worker guard by its exact wire name", () => {
  // 1. The Go producer's json tag is exactly `ci_config_paths` (omitempty). Pin the
  //    literal string against the Go source so a rename on either side is caught.
  const claimGo = readFileSync(
    join(import.meta.dirname, "..", "..", "api", "internal", "workersvc", "claim.go"),
    "utf8",
  );
  assert.match(
    claimGo,
    /CIConfigPaths \[\]string `json:"ci_config_paths,omitempty"`/,
    "the Go ClaimConfig must carry the `ci_config_paths` json tag the worker reads",
  );

  // 2. The TS field name is enforced by the type: this literal only typechecks if
  //    protocol.ts declares `ci_config_paths` on ClaimConfig. This is the shape the
  //    server sends and the runner reads at push time (claim.config.ci_config_paths).
  const config: ClaimConfig = {
    ci_config_paths: [".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", "ci/pipeline.yml"],
  };

  // 3. Run the guard classifier over a representative changed-file set with the
  //    server-supplied paths — the CI file (incl. the project's configured path)
  //    is flagged, the code file is not. This is the classification the fail-closed
  //    push guard branches on.
  const changed = ["src/app.ts", ".gitlab-ci.yml", "ci/pipeline.yml"];
  const flagged = flagCIConfigPaths(changed, config.ci_config_paths ?? []);
  assert.deepEqual(flagged, [".gitlab-ci.yml", "ci/pipeline.yml"]);
});
