import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mrTitle, mrCompletionBlock } from "../src/runner.js";
import { makeClaim } from "./helpers.js";
import type { ClaimConfig } from "../src/protocol.js";

// #1801: a "non-closing" MR body must not be a closing directive on ANY supported forge. GitLab's
// default issue closing pattern also closes on Implement(s|ed|ing) and Closing/Fixing/Resolving, so
// the old `Implements issue #N.` line closed the issue on merge even when the completion interlock
// (ADR 1225) had deliberately withheld `Closes #N`. The title matters too: GitLab's default merge
// and squash commit messages carry the MR title, and those commits are scanned on the default branch.
//
// GITLAB_DEFAULT_CLOSING is a SUBSET of GitLab's documented default pattern (docs.gitlab.com,
// "Default closing pattern"): the same keywords, optional colon and optional `issue(s)`, with
// %{issue_ref} narrowed to the reference forms uzi's renderers can emit (`#N`, `group/project#N`,
// and issue / work-item URLs). It omits the external-tracker key alternative (`ABC-123`) and
// reference-list chaining, so it proves these fixed strings are not closing directives; it is not
// a general closing-directive detector (that is PRD #1798's whole-body scan). It models the
// DEFAULT only; self-managed GitLab may configure its own pattern.
const ISSUE_REF = String.raw`(?:[A-Za-z0-9_.\-]+(?:\/[A-Za-z0-9_.\-]+)+)?#\d+|https?:\/\/\S+\/(?:issues|work_items)\/\d+`;
const GITLAB_DEFAULT_CLOSING = new RegExp(
  String.raw`\b((?:[Cc]los(?:e[sd]?|ing)|\b[Ff]ix(?:e[sd]|ing)?|\b[Rr]esolv(?:e[sd]?|ing)|\b[Ii]mplement(?:s|ed|ing)?)(:?) +(?:(?:issues? +)?(?:${ISSUE_REF})))`,
);

const BRANCH = "agent/issue-7";
const DEFERRED: NonNullable<ClaimConfig["completion_scope"]>["deferred"] = [
  { milestone_id: "m3", title: "Third milestone", reason: "deprioritized" },
];
const ACCEPTED: NonNullable<ClaimConfig["completion_scope"]>["accepted"] = [
  { id: "m2.c1", milestone_id: "m2", text: "Second milestone", reason: "acceptable as-is" },
];

function body(opts: {
  renderCloses: boolean;
  scope?: ClaimConfig["completion_scope"];
  scopeCapped?: { completedCount: number; total?: number };
}): string {
  const claim = makeClaim({ issue_iid: 7, issue_title: "Do the thing" });
  return mrCompletionBlock(
    claim,
    BRANCH,
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
    opts.scopeCapped,
    opts.renderCloses,
    opts.scope,
  );
}

describe("GitLab default closing pattern oracle (#1801)", () => {
  it("matches the forms GitLab documents, including the old uzi line", () => {
    for (const s of [
      "Implements issue #7.",
      "Implements #7",
      "Closes #7",
      "Closing #7",
      "fixes group/project#7",
      "Resolved: #7",
      "Resolve issue #7",
      "Closes https://gitlab.example.com/group/project/-/issues/7",
      "Implements https://gitlab.example.com/group/project/-/work_items/7",
    ]) {
      assert.match(s, GITLAB_DEFAULT_CLOSING, s);
    }
  });

  it("does not match the replacement wording", () => {
    for (const s of ["Related to #7.", "Work on issue #7", "Implements part of #7 (partial delivery)"]) {
      assert.doesNotMatch(s, GITLAB_DEFAULT_CLOSING, s);
    }
  });
});

describe("mrCompletionBlock: non-closing bodies carry no GitLab closing directive (#1801)", () => {
  const cases: Array<[string, Parameters<typeof body>[0]]> = [
    // An interlocked run creates its MR with renderCloses=false, and the held / completion-unverified
    // reconcile re-renders exactly this body (the unverified banner is prepended by the runner).
    ["interlocked creation and held/unverified reconcile", { renderCloses: false }],
    ["interlocked, with owner-accepted criteria", { renderCloses: false, scope: { accepted: ACCEPTED } }],
    ["owner partial (PRD #1227)", { renderCloses: true, scope: { deferred: DEFERRED } }],
    ["operator scope cap (PRD #634)", { renderCloses: true, scopeCapped: { completedCount: 2, total: 3 } }],
  ];
  for (const [name, opts] of cases) {
    it(name, () => {
      const b = body(opts);
      assert.doesNotMatch(b, GITLAB_DEFAULT_CLOSING, `non-closing body closes on GitLab:\n${b}`);
    });
  }

  it("positive control: a verified completion still carries exactly one closing directive, `Closes #7`", () => {
    const b = body({ renderCloses: true });
    const hits = [...b.matchAll(new RegExp(GITLAB_DEFAULT_CLOSING.source, "g"))].map((m) => m[0]);
    assert.deepStrictEqual(hits, ["Closes #7"]);
  });
});

describe("mrTitle: empty-title fallback is not a GitLab closing directive (#1801)", () => {
  it("renders `Work on issue #N`, with and without the [partial] prefix", () => {
    const claim = makeClaim({ issue_iid: 7, issue_title: "" });
    const plain = mrTitle(claim);
    const partial = mrTitle(claim, undefined, { deferred: DEFERRED });
    assert.strictEqual(plain, "Work on issue #7");
    assert.strictEqual(partial, "[partial] Work on issue #7");
    assert.doesNotMatch(plain, GITLAB_DEFAULT_CLOSING);
    assert.doesNotMatch(partial, GITLAB_DEFAULT_CLOSING);
  });
});
