import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { ClaimResponse } from "../src/protocol.js";

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
});
