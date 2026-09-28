import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mrTitle, mrCompletionBlock } from "../src/runner.js";
import { makeClaim, nullLogger } from "./helpers.js";
import type { ClaimConfig, PrDescriptionSize } from "../src/protocol.js";
import { PrDescriptionPublisher } from "../src/pr-description-publisher.js";
import { parseOwnedBlocks } from "../src/pr-description.js";

const ZERO = { added: 0, deleted: 0 };
const SIZE: PrDescriptionSize = { unavailable: false, files: 1, code: { added: 1, deleted: 0 }, tests: ZERO, docs: ZERO, config: ZERO, generated: ZERO, vendored: ZERO };

// PRD #1227 M2/M3 — the agent half of owner completion decisions. mrTitle / mrCompletionBlock render
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

/** mrCompletionBlock with the issue-arm defaults and an explicit renderCloses + completion_scope, so a
 *  test controls exactly the two axes under test without a wall of positional undefineds. */
function render(
  scope: ClaimConfig["completion_scope"],
  renderCloses: boolean,
  iid = 1,
): string {
  const claim = makeClaim({ issue_iid: iid, issue_title: "Do the thing" });
  return mrCompletionBlock(claim, BRANCH, undefined, undefined, undefined, undefined, undefined, undefined, renderCloses, scope);
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

describe("mrCompletionBlock — owner PARTIAL (PRD #1227 M2)", () => {
  it("renders a partial-delivery body listing each deferred milestone + reason and NO Closes", () => {
    const body = render({ deferred: DEFERRED }, true);
    assert.match(body, /Implements part of #1 \(partial delivery — owner scope decision/);
    assert.match(body, /Partial delivery — owner scope decision \(PRD #1227\)/);
    // Each deferred milestone is named as `milestone_id` — title: reason.
    assert.match(body, /> - `m3` — Third milestone: deprioritized/);
  });

  // MUTATION-MINDED (PRD M2): a partial NEVER closes, even when renderCloses would otherwise be true.
  // Restoring unconditional closing language (dropping the renderer's issueArm partial arms in
  // agent/src/pr-description.ts, which never write Closes) must FAIL this.
  it("emits NO `Closes #<iid>` for a partial body even when renderCloses=true", () => {
    const body = render({ deferred: DEFERRED }, true);
    assert.doesNotMatch(body, /Closes #/, "an owner partial must never close the issue, regardless of renderCloses");
    assert.doesNotMatch(body, /Closes #1/);
  });

  it("a partial takes precedence over the #634 scopeCapped count body", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    const body = mrCompletionBlock(
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

describe("mrCompletionBlock — owner ACCEPT (PRD #1227 M3)", () => {
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

describe("mrCompletionBlock — no completion_scope renders the plain completion block", () => {
  it("renders the exact issue completion block (Closes present when renderCloses is true)", async () => {
    // PRD #1798 M6: the completion block ends in the maintainer-approved footer (2026-09-27),
    // byte-pinned, and it is pinned through the LIVE path too: the body a new MR is created with is
    // the publisher's initialBody over this block (runner.ts phasePublish).
    const expected = [
      "<!-- uzi:completion:start v1 -->",
      "Related to #1.",
      "",
      "Closes #1",
      "",
      "---",
      "Opened by uzi from `agent/issue-1`. A human reviews and merges; uzi never merges.",
      "<!-- uzi:completion:end -->",
    ].join("\n");
    assert.strictEqual(render(undefined, true), expected);
    // An empty completion_scope (both arrays absent) must render identically to no completion_scope.
    assert.strictEqual(render({}, true), expected);
    assert.strictEqual(render({ deferred: [], accepted: [] }, true), expected);

    const publisher = new PrDescriptionPublisher({
      forge: {
        getMergeRequest: async () => {
          throw new Error("not read");
        },
        updateMergeRequestDescription: async () => {
          throw new Error("not written");
        },
      },
      api: {
        stagePrDescription: async () => {
          throw new Error("no api");
        },
        bindPrDescription: async () => {
          throw new Error("no api");
        },
        lookupPrDescription: async () => {
          throw new Error("no api");
        },
        ackPrDescription: async () => {
          throw new Error("no api");
        },
      },
      pass: null,
      log: nullLogger(),
      emit: () => {},
    });
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    const pub = await publisher.prepare(
      {
        runId: claim.run_id,
        claimGeneration: 1,
        claim,
        repoUrl: "https://gitlab.example.test/o/r",
        pat: "pat",
        mode: "own",
        completionCloses: true,
        facts: async () => ({ baseSha: "b".repeat(40), size: { line: "**Size:** code +1 −0 · 1 file", size: SIZE } }),
        context: async () => {
          throw new Error("no editor pass");
        },
        completion: () => render(undefined, true),
      },
      { headSha: "a".repeat(40), targetBranch: "main" },
    );
    const created = pub.initialBody(render(undefined, true));
    assert.ok(created.endsWith(`\n\n${expected}`), created);
    const parsed = parseOwnedBlocks(created);
    assert.equal(parsed.kind === "ok" && parsed.completion, expected);
    assert.equal(parsed.kind === "ok" && parsed.after, "", "the footer closes the body");
  });
});
