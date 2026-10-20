import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { ClaimResponse } from "../src/protocol.js";
import { validateCIRepairClaim } from "../src/ci-evidence.js";
import { DEFAULT_CI_CONFIG_PATHS, flagCIConfigPathsRaw } from "../src/ci-config-guard.js";

// Issue #2865 cross-side contract: the default-branch CI repair claim. The worker parses the SAME
// golden file the Go producer pins (api/internal/workersvc/testdata/claim_ci_repair_wire.json), so
// the `ci_config_guard` wire name can never drift into two lenient fakes. A drift would silently
// fail the guard OPEN (an absent flag turns the backstop off).
const apiDir = join(import.meta.dirname, "..", "..", "api", "internal", "workersvc");

test("ci repair claim wire contract: the worker reads config.ci_config_guard and ci_config_paths", () => {
  const claim = JSON.parse(readFileSync(join(apiDir, "testdata", "claim_ci_repair_wire.json"), "utf8")) as ClaimResponse;
  assert.equal(claim.config?.ci_config_guard, true);
  assert.ok((claim.config?.ci_config_paths ?? []).length > 0);
  // The worker floor covers everything the server sends for this lane, so a missing server list
  // can never narrow the guard.
  for (const glob of claim.config?.ci_config_paths ?? []) {
    assert.ok(DEFAULT_CI_CONFIG_PATHS.includes(glob), `${glob} is in the worker repair floor`);
  }
  const flagged = flagCIConfigPathsRaw(
    ["src/app.ts", ".forgejo/workflows/x.yml", ".github/workflows/ci.yml"],
    [...DEFAULT_CI_CONFIG_PATHS, ...(claim.config?.ci_config_paths ?? [])],
  );
  assert.deepEqual(flagged, [".forgejo/workflows/x.yml", ".github/workflows/ci.yml"]);
});

test("ci repair claim wire contract: the Go producer's json tag is exactly ci_config_guard", () => {
  const claimGo = readFileSync(join(apiDir, "claim.go"), "utf8");
  assert.match(claimGo, /CIConfigGuard bool `json:"ci_config_guard,omitempty"`/);
});

test("artifact-bearing Go claim preserves canonical aliases and unknown remainder", () => {
  const claim = JSON.parse(readFileSync(join(apiDir, "testdata", "claim_ci_artifacts_wire.json"), "utf8")) as ClaimResponse;
  validateCIRepairClaim({ ...claim, config: claim.config ?? undefined }, claim.claim_generation);
  assert.equal(claim.repo.forge_type, "github");
  assert.equal(claim.config?.ci_config_guard, true);
  assert.equal(claim.job, undefined);
  const repair = claim.ci_repair!;
  assert.equal(repair.input_files.length, 1);
  const kept = repair.manifest.entries.filter(e => e.status === "kept");
  assert.equal(kept.length, 2);
  for (const entry of kept) assert.equal(entry.manifest_id, repair.input_files[0]!.manifest_id);
  assert.equal(repair.manifest.uninspected, true);
  assert.equal(repair.manifest.omitted, undefined);
});
