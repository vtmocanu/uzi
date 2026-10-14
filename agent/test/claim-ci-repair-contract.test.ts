import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { ClaimResponse } from "../src/protocol.js";
import { DEFAULT_CI_REPAIR_CONFIG_PATHS, flagCIConfigPathsRaw } from "../src/ci-config-guard.js";

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
    assert.ok(DEFAULT_CI_REPAIR_CONFIG_PATHS.includes(glob), `${glob} is in the worker repair floor`);
  }
  const flagged = flagCIConfigPathsRaw(
    ["src/app.ts", ".forgejo/workflows/x.yml", ".github/workflows/ci.yml"],
    [...DEFAULT_CI_REPAIR_CONFIG_PATHS, ...(claim.config?.ci_config_paths ?? [])],
  );
  assert.deepEqual(flagged, [".forgejo/workflows/x.yml", ".github/workflows/ci.yml"]);
});

test("ci repair claim wire contract: the Go producer's json tag is exactly ci_config_guard", () => {
  const claimGo = readFileSync(join(apiDir, "claim.go"), "utf8");
  assert.match(claimGo, /CIConfigGuard bool `json:"ci_config_guard,omitempty"`/);
});
