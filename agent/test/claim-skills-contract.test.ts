import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { ClaimResponse } from "../src/protocol.js";

// The worker side of the PRD #16 wire contract. It parses the SAME golden file the
// Go producer test pins (api/internal/workersvc/claim_wire_contract_test.go), so
// the two sides can never drift into two lenient fakes. Reaching into the api tree
// mirrors the existing lead-builtin cross-check in agents.test.ts. Typing the
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
  "claim_skills_wire.json",
);

test("claim wire contract: worker parses the server's skill shape", () => {
  const claim = JSON.parse(readFileSync(fixture, "utf8")) as ClaimResponse;

  // Repo opt-in flag.
  assert.equal(claim.repo.skills_enabled, true);

  // PRD #19's autopilot flag rides the same claim shape (post-landing merge): the
  // worker must still parse the skills fields alongside it.
  assert.equal(claim.auto_approve, true);

  // Config caps ride the claim (no worker-side hardcoded drift).
  assert.equal(claim.config?.skill_max_bytes, 65536);
  assert.equal(claim.config?.skills_max_per_run, 32);

  // The per-run skill union: name + description + body per entry.
  assert.ok(Array.isArray(claim.skills));
  assert.deepEqual(
    claim.skills!.map((s) => s.name),
    ["ci-cd-norms", "team-kb"],
  );
  for (const s of claim.skills!) {
    assert.equal(typeof s.name, "string");
    assert.equal(typeof s.description, "string");
    assert.equal(typeof s.body, "string");
  }

  // Dropped-skill log: name + reason code.
  assert.ok(Array.isArray(claim.skills_dropped));
  assert.deepEqual(claim.skills_dropped, [{ name: "ci-cd-norms", reason: "shadowed" }]);

  // Per-template scoping: coder carries its allocation, reviewer an explicit empty
  // list (never undefined — the worker always passes an explicit skills list).
  const coder = claim.agents.find((a) => a.name === "coder");
  const reviewer = claim.agents.find((a) => a.name === "reviewer");
  assert.deepEqual(coder?.skills, ["ci-cd-norms", "team-kb"]);
  assert.deepEqual(reviewer?.skills, []);
});
