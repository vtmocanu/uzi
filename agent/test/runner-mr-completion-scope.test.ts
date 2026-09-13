import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mrTitle, mrDescription } from "../src/runner.js";
import { makeClaim } from "./helpers.js";
import type { ClaimConfig } from "../src/protocol.js";

// PRD #1227 M2/M3 — the agent half of owner completion decisions. mrTitle / mrDescription render
// the run's owner decisions (claim.config.completion_scope): `deferred` drives a `[partial]`,
// non-closing partial-delivery body (scope_reduced), and `accepted` drives an accept warning block.
// These are DIRECT-CALL unit tests on the exported renderers, so the byte-level assertions the PRD
// requires (a partial NEVER closes; a mutation restoring closing language must fail) are provable
// without driving a whole RunRunner.execute(); the create-then-verify wiring is covered end to end in
// runner-completion-permit.test.ts.

const BRANCH = "agent/issue-1";

// completion_scope with ONE owner-deferred milestone (PRD #1227 D2 shape).
const DEFERRED: NonNullable<ClaimConfig["completion_scope"]>["deferred"] = [
  { milestone_id: "m3", title: "Third milestone", reason: "deprioritized" },
];
// completion_scope with ONE owner-accepted unmet criterion (PRD #1227 D3 shape).
const ACCEPTED: NonNullable<ClaimConfig["completion_scope"]>["accepted"] = [
  { id: "m2.c1", milestone_id: "m2", text: "Second milestone", reason: "acceptable as-is" },
];

/** mrDescription with the issue-arm defaults and an explicit renderCloses + completion_scope, so a
 *  test controls exactly the two axes under test without a wall of positional undefineds. */
function render(
  scope: ClaimConfig["completion_scope"],
  renderCloses: boolean,
  iid = 1,
): string {
  const claim = makeClaim({ issue_iid: iid, issue_title: "Do the thing" });
  return mrDescription(claim, BRANCH, undefined, undefined, undefined, undefined, undefined, undefined, renderCloses, scope);
}

describe("mrTitle — owner completion decisions (PRD #1227 M2)", () => {
  it("prefixes [partial] for an owner PARTIAL (deferred non-empty)", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    assert.strictEqual(mrTitle(claim, undefined, { deferred: DEFERRED }), "[partial] Do the thing");
  });

  it("does NOT prefix [partial] for an accept-ONLY run (accepted non-empty, deferred empty) — it closes the issue", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    assert.strictEqual(mrTitle(claim, undefined, { accepted: ACCEPTED }), "Do the thing");
  });

  it("is byte-identical to today when there is no completion_scope", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    assert.strictEqual(mrTitle(claim), "Do the thing");
    assert.strictEqual(mrTitle(claim, undefined, undefined), "Do the thing");
  });
});

describe("mrDescription — owner PARTIAL (PRD #1227 M2)", () => {
  it("renders a partial-delivery body listing each deferred milestone + reason and NO Closes", () => {
    const body = render({ deferred: DEFERRED }, true);
    assert.match(body, /Implements part of #1 \(partial delivery — owner scope decision/);
    assert.match(body, /Partial delivery — owner scope decision \(PRD #1227\)/);
    // Each deferred milestone is named as `milestone_id` — title: reason.
    assert.match(body, /> - `m3` — Third milestone: deprioritized/);
  });

  // MUTATION-MINDED (PRD M2): a partial NEVER closes, even when renderCloses would otherwise be true.
  // Restoring unconditional closing language (dropping the effectiveCloses guard) must FAIL this.
  it("emits NO `Closes #<iid>` for a partial body even when renderCloses=true", () => {
    const body = render({ deferred: DEFERRED }, true);
    assert.doesNotMatch(body, /Closes #/, "an owner partial must never close the issue, regardless of renderCloses");
    assert.doesNotMatch(body, /Closes #1/);
  });

  it("a partial takes precedence over the #634 scopeCapped count body", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    const body = mrDescription(
      claim,
      BRANCH,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      { completedCount: 2, total: 5 }, // scopeCapped ALSO set
      true,
      { deferred: DEFERRED },
    );
    // The owner-partial body wins; the operator-directive count body is not rendered.
    assert.match(body, /owner scope decision/);
    assert.doesNotMatch(body, /operator scope directive/);
    assert.doesNotMatch(body, /Closes #/);
  });
});

describe("mrDescription — owner ACCEPT (PRD #1227 M3)", () => {
  it("accept-only: closes the issue AND appends a warning block naming id + text + reason", () => {
    const body = render({ accepted: ACCEPTED }, true);
    assert.match(body, /Closes #1/, "an accept-only run still closes the issue");
    assert.match(body, /Accepted unmet criteria — owner decision \(PRD #1227\)/);
    assert.match(body, /> - `m2\.c1` — Second milestone: acceptable as-is/);
  });

  it("both partial AND accept: no Closes, plus the accept warning block", () => {
    const body = render({ deferred: DEFERRED, accepted: ACCEPTED }, true);
    assert.doesNotMatch(body, /Closes #/, "a partial run never closes even with accepted criteria");
    assert.match(body, /owner scope decision/, "the partial body is rendered");
    assert.match(body, /> - `m3` — Third milestone: deprioritized/, "the deferred milestone is listed");
    assert.match(body, /Accepted unmet criteria — owner decision/, "the accept warning block is present");
    assert.match(body, /> - `m2\.c1` — Second milestone: acceptable as-is/);
  });
});

describe("mrDescription — no completion_scope is byte-identical to today", () => {
  it("renders the exact legacy issue body (Closes present when renderCloses is true)", () => {
    const expected = [
      "Implements issue #1.",
      "",
      "Closes #1",
      "",
      "---",
      "Opened automatically by the uzi agent from branch `agent/issue-1`. Please review and merge manually — the agent never merges.",
    ].join("\n");
    assert.strictEqual(render(undefined, true), expected);
    // An empty completion_scope (both arrays absent) must render identically to no completion_scope.
    assert.strictEqual(render({}, true), expected);
    assert.strictEqual(render({ deferred: [], accepted: [] }, true), expected);
  });
});
