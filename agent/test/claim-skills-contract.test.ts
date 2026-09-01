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

// A second golden that genuinely OMITS review_comments: the ci_fix claim wire, which
// the Go producer (api/internal/workersvc/ci_fix_test.go) pins byte-for-byte to
// json.MarshalIndent of a real ci_fix claim whose ReviewComments is nil. The nil +
// `omitempty` tag drops the key entirely — so this file, not a hand-built copy, is
// what carries the omitempty contract across the language boundary.
const ciFixFixture = join(
  import.meta.dirname,
  "..",
  "..",
  "api",
  "internal",
  "workersvc",
  "testdata",
  "claim_ci_fix_wire.json",
);

test("claim wire contract: worker parses the server's skill shape", () => {
  const claim = JSON.parse(readFileSync(fixture, "utf8")) as ClaimResponse;

  // Repo opt-in flags (PRD #16 skills, PRD #246 claudemd). Both trust flags ride the
  // repo block byte-for-byte (no omitempty on the Go side), so the worker's parse of
  // each is pinned across the language boundary.
  assert.equal(claim.repo.skills_enabled, true);
  assert.equal(claim.repo.claudemd_enabled, true);

  // PRD #65 R8: the worker parses forge_type off the claim (the server emits it on
  // every claim; "gitlab" for a GitLab connection). Absent ⇒ gitlab on an old api;
  // present here, so the parse is pinned across the language boundary.
  assert.equal(claim.repo.forge_type, "gitlab");

  // PRD #381: the bounded, bot-filtered snapshot of the issue's human comments rides
  // the claim next to issue_description. The golden carries one comment with truncated
  // set, so the worker's parse of the whole snapshot (comment fields + truncated flag)
  // is pinned across the language boundary; typing the parse as ClaimResponse also makes
  // `npm run typecheck` fail if issue_comments is dropped from protocol.ts.
  assert.equal(claim.issue_comments?.truncated, true);
  assert.deepEqual(claim.issue_comments?.comments, [
    {
      author_username: "carol",
      author_forge_user_id: 42,
      created_at: "2026-07-04T09:00:00Z",
      body: "please guard on Valid",
    },
  ]);

  // PRD #700 M2: the bot-self-filtered snapshot of an MR's review comments rides an
  // mr_rework claim. The golden carries one inline comment with truncated set, so the
  // worker's parse of the whole snapshot (comment fields incl. the anchors + monotonic
  // id, plus the truncated flag) is pinned across the language boundary; typing the
  // parse as ClaimResponse also makes `npm run typecheck` fail if review_comments is
  // dropped from protocol.ts.
  assert.equal(claim.review_comments?.truncated, true);
  assert.deepEqual(claim.review_comments?.comments, [
    {
      id: 5001,
      author_username: "coderabbit",
      author_forge_user_id: 43,
      created_at: "2026-07-04T10:00:00Z",
      body: "guard nil here",
      path: "api/x.go",
      line: 42,
      reply_id: "5001",
      resolve_id: "PRRT_thread1",
      head_sha: "headsha999",
      review_state: "inline",
    },
  ]);

  // The omitempty contract: a claim WITHOUT an mr_rework review snapshot omits the
  // key entirely (the Go side tags review_comments `omitempty`). Assert against a
  // GOLDEN that genuinely omits it — the ci_fix wire, byte-pinned to a real ci_fix
  // claim (ReviewComments nil). This is the non-vacuous key-presence check on the
  // parse side: if the Go side ever stopped omitting the field, that golden would
  // gain a `review_comments` key and this assertion would break. (The prior version
  // copied `claim` and `delete`d the key from the copy, which only exercised the JS
  // `delete` operator and passed regardless of the Go contract.)
  const withoutReview = JSON.parse(
    readFileSync(ciFixFixture, "utf8"),
  ) as Record<string, unknown>;
  assert.equal("review_comments" in withoutReview, false);
  assert.equal(
    (withoutReview as Partial<ClaimResponse>).review_comments,
    undefined,
  );

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
  // PRD #400 M2: the task-run MR gate + source ref ride the same claim, top-level
  // alongside auto_approve. Meaningful only for a task run, but the wire always
  // carries them; pinned across the language boundary so typing the parse as
  // ClaimResponse fails `npm run typecheck` if either is dropped from protocol.ts.
  assert.equal(claim.open_mr, true);
  // issue #552 M3: stop_pending re-delivers the durable stop_kind='stopped' fact so a
  // graceful stop survives a worker crash. Pinned across the language boundary; typing the
  // parse as ClaimResponse also makes `npm run typecheck` fail if it is dropped from
  // protocol.ts. The golden models an interactive, non-terminal, stopped run, so it is true.
  assert.equal(claim.stop_pending, true);
  assert.equal(claim.base_branch, "develop");
  // PRD #400 M4b: the diff-review target rides the same claim, top-level alongside
  // base_branch. This golden is not a review claim, so the field is null (the wire
  // carries it byte-for-byte); pinned across the language boundary so typing the parse
  // as ClaimResponse fails `npm run typecheck` if it is dropped from protocol.ts.
  assert.equal(claim.review_target_run_id, null);
  assert.equal(claim.plan_approved, true);
  assert.equal(claim.plan_md, "# Plan\n");
  // PRD #209: the plan_source discriminator rides the same claim, top-level alongside
  // plan_approved. This golden models a SEEDED run — approved with no approve_plan input
  // — so the pair the worker's D4 discriminator reads (plan_approved + plan_source) is
  // pinned across the language boundary. Typing the parse as ClaimResponse also makes
  // `npm run typecheck` fail if plan_source is dropped from protocol.ts.
  assert.equal(claim.plan_source, "seeded");
  // PRD #209 M4: the staleness-guard pair the runner reads after checkout rides the same
  // seeded claim — the commit the plan was written against, and whether a divergence should
  // fail the run. Pinned across the language boundary; typing the parse as ClaimResponse
  // also makes `npm run typecheck` fail if either field is dropped from protocol.ts.
  assert.equal(claim.planned_base_commit, "abc123def4567890abc123def4567890abc12345");
  assert.equal(claim.require_base_match, true);

  // Config caps ride the claim (no worker-side hardcoded drift).
  assert.equal(claim.config?.skill_max_bytes, 65536);
  assert.equal(claim.config?.skills_max_per_run, 32);

  // issue #916: the owner's AI-attribution flag rides the config as an always-present
  // bool (default true = today's behavior; worker suppresses the trailer when false).
  assert.equal(claim.config?.attribution_enabled, true);

  // Tool provisioning fields (PRD #18 M3): the resolved tier-1 package list and
  // the repo devbox opt-in flag ride the same config.
  assert.deepEqual(claim.config?.tool_packages, ["kubectl@1.31", "jq"]);
  assert.equal(claim.config?.repo_devbox_opt_in, false);

  // PRD #123 M1b: the Decision 6 denylist base names ride the same config so the worker
  // can filter tier-2 (repo devbox.json) packages by base name. Pinned across the
  // language boundary; typing the parse as ClaimResponse also makes `npm run typecheck`
  // fail if denied_tool_packages is dropped from protocol.ts.
  assert.deepEqual(claim.config?.denied_tool_packages, ["glab", "vault"]);

  // The per-run skill union: name + description + body per entry.
  assert.ok(Array.isArray(claim.skills));
  assert.deepEqual(
    claim.skills!.map((s) => s.name),
    ["team-runbook", "team-kb"],
  );
  for (const s of claim.skills!) {
    assert.equal(typeof s.name, "string");
    assert.equal(typeof s.description, "string");
    assert.equal(typeof s.body, "string");
  }

  // Dropped-skill log: name + reason code.
  assert.ok(Array.isArray(claim.skills_dropped));
  assert.deepEqual(claim.skills_dropped, [{ name: "team-runbook", reason: "shadowed" }]);

  // Per-template scoping: coder carries its allocation, reviewer an explicit empty
  // list (never undefined — the worker always passes an explicit skills list).
  const coder = claim.agents.find((a) => a.name === "coder");
  const reviewer = claim.agents.find((a) => a.name === "reviewer");
  assert.deepEqual(coder?.skills, ["team-runbook", "team-kb"]);
  assert.deepEqual(reviewer?.skills, []);
});
