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

  // PRD #65 R8: the worker parses forge_type off the claim (the server emits it on
  // every claim; "gitlab" for a GitLab connection). Absent ⇒ gitlab on an old api;
  // present here, so the parse is pinned across the language boundary.
  assert.equal(claim.repo.forge_type, "gitlab");

  // PRD #19's autopilot flag rides the same claim shape (post-landing merge): the
  // worker must still parse the skills fields alongside it.
  assert.equal(claim.auto_approve, true);

  // PRD #35: the usage-limit park fields ride the same claim, top-level alongside
  // auto_approve and re-read from the runs row on every claim so a resumed run keeps
  // them. plan_md is asserted here for the first time — the server always sent it,
  // but it was undeclared in protocol.ts and unread while nothing needed it; the
  // resume-skips-the-gate path (Decision 6b) consumes plan_md and plan_approved
  // together, so the parse of both is now load-bearing.
  //
  // The golden carries these as `true` rather than at their zero values for two
  // reasons, NEITHER of which is "otherwise a dropped field would pass":
  //
  //   1. it matches the precedent auto_approve already set in this fixture, and
  //   2. a non-default value is what distinguishes "actually wired" from "present
  //      and always zero" for a future producer built from the real claim path
  //      rather than from the hand-built struct.
  //
  // An earlier version of this comment claimed a `false` golden would let a producer
  // that DROPPED the field pass here. That is false and was corrected rather than
  // left: this file imports `node:assert/strict`, so `assert.equal` IS
  // `strictEqual`, and `undefined === false` is false — the assertion throws at
  // either golden value.
  //
  // What actually gates a dropped field is stated in this file's header and is worth
  // re-stating because it is not this assertion: removing a member from
  // `ClaimResponse` fails `npm run typecheck`, not this test, because
  // agent/tsconfig.json includes `test` in the program. The Go half is gated
  // separately, by a byte-compare against MarshalIndent output.
  assert.equal(claim.wait_on_limit, true);
  assert.equal(claim.plan_approved, true);
  assert.equal(claim.plan_md, "# Plan\n");

  // Config caps ride the claim (no worker-side hardcoded drift).
  assert.equal(claim.config?.skill_max_bytes, 65536);
  assert.equal(claim.config?.skills_max_per_run, 32);

  // Tool provisioning fields (PRD #18 M3): the resolved tier-1 package list and
  // the repo devbox opt-in flag ride the same config.
  assert.deepEqual(claim.config?.tool_packages, ["kubectl@1.31", "jq"]);
  assert.equal(claim.config?.repo_devbox_opt_in, false);

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
