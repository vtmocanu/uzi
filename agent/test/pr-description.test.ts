import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SanitizedPrDescriptionFields, WorkerClient } from "../src/client.js";
import {
  BODY_CAP_CHARS,
  COMPLETION_END,
  COMPLETION_START,
  REGION_CAP_BYTES,
  REGION_END,
  REGION_START,
  STALENESS_END,
  STALENESS_START,
  capBody,
  closingDirectiveFor,
  codeSpan,
  composeBody,
  escapeInline,
  parseOwnedBlocks,
  regionSha256,
  renderBody,
  renderCompletionBlock,
  renderRegion,
  type OwnedBlocks,
} from "../src/pr-description.js";
import type { ClaimConfig, ClaimResponse, PrDescriptionStageRequest, RawPrDescriptionFields, RunKind } from "../src/protocol.js";
import { mrDescription } from "../src/runner.js";
import { makeClaim, nullLogger } from "./helpers.js";

// PRD #1798 M6a: the pure renderer of uzi's two owned PR-description blocks (pr-description.ts) and
// mrDescription on top of it.

const ZW = "\u200B";
const HEAD = "e4020cc0123456789abcdef0123456789abcdef0";
const SIZE = "**Size:** code +3 \u22121 \u00b7 1 file";

// ── Minting REAL SanitizedPrDescriptionFields instances ────────────────────────────────────
// Only WorkerClient's response decoders construct the class, so a test gets an instance the way
// production does: a stage call whose (stubbed) api response carries the fields.

const STAGE: PrDescriptionStageRequest = {
  claim_generation: 1,
  source: "generated",
  fields: { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] },
  size: null,
  base_sha: "a".repeat(40),
  head_sha: HEAD,
  target_branch: "main",
};

async function mint(fields: Partial<RawPrDescriptionFields>): Promise<SanitizedPrDescriptionFields> {
  const wire = {
    version: {
      id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
      run_id: "11111111-2222-3333-4444-555555555555",
      claim_generation: 1,
      mr_iid: null,
      fields: { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [], ...fields },
      size: null,
      base_sha: "a".repeat(40),
      head_sha: HEAD,
      target_branch: "main",
      source: "generated",
      rendered_region_sha256: null,
      state: "pending",
      created_at: "2026-09-27T10:00:00Z",
      published_at: null,
    },
  };
  const orig = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify(wire), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;
  try {
    const client = new WorkerClient("http://127.0.0.1:9", "token", "0.1.0-test", nullLogger(), { sleep: async () => {} });
    const { version } = await client.stagePrDescription(wire.version.run_id, STAGE);
    return version.fields;
  } finally {
    globalThis.fetch = orig;
  }
}

const FULL: Partial<RawPrDescriptionFields> = {
  summary: "A human can now mark a finding done from the Findings page.",
  changes: ["Web: Mark done on each Findings row.", "CLI: uzi findings resolve."],
  scope_notes: [{ kind: "added", text: "Done also works by disposition ID." }],
  review_pointers: ["The disposition rules are reused from the judge."],
  verification: [
    { command: "task gate:api", result: "pass", verified_at_sha: "e4020cc" },
    { command: "task gate:web", result: "fail", verified_at_sha: "e4020cc" },
  ],
};

// ── renderRegion ────────────────────────────────────────────────────────────────────────────

describe("renderRegion (D5, D8, D12)", () => {
  it("renders the full layout from a real instance, fields verbatim", async () => {
    const f = await mint(FULL);
    const r = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main", source: "generated" }, f);
    assert.equal(r.withFields, true);
    assert.equal(
      r.text,
      [
        REGION_START,
        "A human can now mark a finding done from the Findings page.",
        "",
        SIZE,
        "",
        "### What changed",
        "- Web: Mark done on each Findings row.",
        "- CLI: uzi findings resolve.",
        "",
        "### Verification",
        "Reported by the agent at `e4020cc`:",
        "- passed: task gate:api",
        "- failed: task gate:web",
        "",
        "### Scope and review notes",
        "- Added: Done also works by disposition ID.",
        "- Review: The disposition rules are reused from the judge.",
        "",
        "Describes `e4020cc` against `main`.",
        REGION_END,
      ].join("\n"),
    );
  });

  it("shows both SHAs when a check was reported at a different commit than the head (D5)", async () => {
    const f = await mint({ verification: [{ command: "npm test", result: "pass", verified_at_sha: "abcdef1" }] });
    const r = renderRegion({ headSha: HEAD, targetBranch: "main" }, f);
    assert.match(r.text, /Reported by the agent at `abcdef1` \(the PR head is `e4020cc`\):\n- passed: npm test/);
  });

  it("rung 2 (lead_only) says the summary was not checked against the diff (D8)", async () => {
    const f = await mint({ summary: "The lead's summary." });
    const r = renderRegion({ sizeLine: SIZE, source: "lead_only" }, f);
    // The note follows the summary as its own paragraph, never under "### Verification" (which a
    // lead_only version with no reported checks does not render at all).
    assert.match(r.text, /^The lead's summary\.\n\n_Summary written by the agent, not checked against the diff\._\n\n\*\*Size/m);
    assert.doesNotMatch(r.text, /### Verification/);
    const gen = renderRegion({ sizeLine: SIZE, source: "generated" }, f);
    assert.doesNotMatch(gen.text, /not checked against the diff/);
  });

  it("rung 3 / no fields: size line and provenance only", () => {
    const r = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "release/1.x" });
    assert.deepEqual(r, {
      text: `${REGION_START}\n${SIZE}\n\nDescribes \`e4020cc\` against \`release/1.x\`.\n${REGION_END}`,
      withFields: false,
      fallback: "no_fields",
    });
    // Unknown head or target: no provenance line.
    assert.equal(renderRegion({ sizeLine: SIZE }).text, `${REGION_START}\n${SIZE}\n${REGION_END}`);
  });

  it("deterministic_only never renders fields", async () => {
    const f = await mint(FULL);
    const r = renderRegion({ sizeLine: SIZE, source: "deterministic_only" }, f);
    assert.equal(r.fallback, "deterministic_only");
    assert.equal(r.text, `${REGION_START}\n${SIZE}\n${REGION_END}`);
  });

  it("refuses anything but a real instance: Object.assign and structuredClone fall back to size-only", async () => {
    const s = await mint(FULL);
    const raw: RawPrDescriptionFields = {
      summary: "Closes #1 @everyone <!-- uzi:description:end -->",
      changes: [],
      scope_notes: [],
      review_pointers: [],
      verification: [],
    };
    const sizeOnly = `${REGION_START}\n${SIZE}\n${REGION_END}`;
    // Both compile into the sanitized slot with no cast (client.ts documents why); only the runtime
    // is() check stops them.
    const assigned = Object.assign({}, s, raw);
    const cloned = structuredClone(s);
    for (const forged of [assigned, cloned]) {
      const r = renderRegion({ sizeLine: SIZE }, forged);
      assert.deepEqual(r, { text: sizeOnly, withFields: false, fallback: "not_sanitized" });
    }
    const anyTyped = JSON.parse(JSON.stringify(s)) as SanitizedPrDescriptionFields;
    assert.equal(renderRegion({ sizeLine: SIZE }, anyTyped).fallback, "not_sanitized");
    // The genuine instance renders its fields.
    assert.equal(renderRegion({ sizeLine: SIZE }, s).withFields, true);
  });

  it("a region over 6 KiB falls back to the size line and provenance (D15 region cap)", async () => {
    const big = await mint({ summary: "x".repeat(600), changes: Array.from({ length: 5 }, () => "y".repeat(200)), review_pointers: ["z".repeat(200)], verification: Array.from({ length: 20 }, (_, i) => ({ command: `c${i}`.padEnd(200, "c"), result: "pass" as const, verified_at_sha: "abcdef1" })) });
    const r = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main" }, big);
    assert.equal(r.fallback, "region_cap");
    assert.equal(r.text, `${REGION_START}\n${SIZE}\n\nDescribes \`e4020cc\` against \`main\`.\n${REGION_END}`);
    const small = await mint({ summary: "x".repeat(600) });
    const ok = renderRegion({ sizeLine: SIZE }, small);
    assert.equal(ok.withFields, true);
    assert.ok(Buffer.byteLength(ok.text) <= REGION_CAP_BYTES);
  });
});

describe("regionSha256", () => {
  it("hashes the exact region text with CRLF normalised to LF", () => {
    const lf = `${REGION_START}\n${SIZE}\n${REGION_END}`;
    const crlf = lf.replace(/\n/g, "\r\n");
    assert.match(regionSha256(lf), /^[0-9a-f]{64}$/);
    assert.equal(regionSha256(crlf), regionSha256(lf));
    assert.notEqual(regionSha256(`${lf} `), regionSha256(lf));
  });
});

// ── parseOwnedBlocks / composeBody / capBody ───────────────────────────────────────────────

const REGION = `${REGION_START}\n${SIZE}\n${REGION_END}`;
const COMPLETION = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: true });

describe("parseOwnedBlocks (D10)", () => {
  it("none: a legacy body with no uzi marker", () => {
    assert.deepEqual(parseOwnedBlocks("Related to #7.\n\nCloses #7"), { kind: "none" });
  });

  it("ok: splits around both blocks so the parts concatenate back to the body", () => {
    const body = `Intro by a human.\n\n${REGION}\n\nBot summary\n\n${COMPLETION}\n\nTrailer`;
    const p = parseOwnedBlocks(body);
    assert.equal(p.kind, "ok");
    const b = p as OwnedBlocks;
    assert.equal(b.before, "Intro by a human.\n\n");
    assert.equal(b.region, REGION);
    assert.equal(b.between, "\n\nBot summary\n\n");
    assert.equal(b.completion, COMPLETION);
    assert.equal(b.after, "\n\nTrailer");
    assert.equal(b.before + b.region + b.between + b.completion + b.after, body);
  });

  it("ok with the completion block missing (region only), and with the region missing", () => {
    const onlyRegion = parseOwnedBlocks(`${REGION}\n\nnotes`);
    assert.equal(onlyRegion.kind, "ok");
    assert.equal((onlyRegion as OwnedBlocks).completion, undefined);
    assert.equal((onlyRegion as OwnedBlocks).after, "\n\nnotes");
    const onlyCompletion = parseOwnedBlocks(`x\n${COMPLETION}\ny`);
    assert.equal(onlyCompletion.kind, "ok");
    assert.equal((onlyCompletion as OwnedBlocks).region, undefined);
    assert.equal((onlyCompletion as OwnedBlocks).completion, COMPLETION);
  });

  it("malformed: duplicate, unbalanced, unknown version, reversed or nested blocks", () => {
    const cases: Array<[string, string]> = [
      [`${REGION}\n${REGION}\n${COMPLETION}`, "duplicate"],
      [`${COMPLETION}\n${COMPLETION_START}`, "duplicate"],
      [`${REGION_START}\nx\n${COMPLETION}`, "unbalanced"],
      [`x\n${COMPLETION_END}`, "unbalanced"],
      [`${REGION_END}\nx\n${REGION_START}`, "unbalanced"],
      [`${COMPLETION}\n${REGION}`, "unbalanced"],
      [`${REGION_START}\n${COMPLETION}\n${REGION_END}`, "unbalanced"],
      [`<!-- uzi:description:start v2 -->\nx\n${REGION_END}`, "unknown_version"],
      [`<!-- uzi:completion:start -->\nx\n${COMPLETION_END}`, "unknown_version"],
      [`<!--uzi:description:start v1-->\nx\n${REGION_END}`, "unknown_version"],
    ];
    for (const [body, reason] of cases) {
      assert.deepEqual(parseOwnedBlocks(body), { kind: "malformed", reason }, body);
    }
  });
});

describe("composeBody (D10)", () => {
  it("replaces only the owned blocks and keeps every other byte, CRLF and odd spacing included", () => {
    const before = "## Human notes\r\n\r\nKeep  this\u00a0exactly.\r\n\r\n";
    const between = "\n\n<!-- a review bot's own marker -->\n**CodeRabbit** summary — kept\n\n";
    const after = "\n\n---\nfooter a maintainer added  \n";
    const body = before + REGION + between + COMPLETION + after;
    const parsed = parseOwnedBlocks(body);
    assert.equal(parsed.kind, "ok");
    const newRegion = `${REGION_START}\nnew\n${REGION_END}`;
    const newCompletion = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: false });
    const out = composeBody(parsed as OwnedBlocks, { region: newRegion, completion: newCompletion });
    assert.equal(out, before + newRegion + between + newCompletion + after);
    // Replacing only the completion block keeps the region too.
    assert.equal(composeBody(parsed as OwnedBlocks, { completion: newCompletion }), before + REGION + between + newCompletion + after);
  });

  it("refuses to insert a block the body does not carry", () => {
    const parsed = parseOwnedBlocks(`x\n${COMPLETION}`) as OwnedBlocks;
    assert.equal(composeBody(parsed, { region: REGION }), undefined);
    const noCompletion = parseOwnedBlocks(`${REGION}\nx`) as OwnedBlocks;
    assert.equal(composeBody(noCompletion, { completion: COMPLETION }), undefined);
  });
});

describe("capBody (D15)", () => {
  it("drops the region to the size-only region when the body would exceed 65,536 characters", async () => {
    const f = await mint(FULL);
    const full = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main" }, f).text;
    const sizeOnly = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main" }).text;
    const preserved = "p".repeat(BODY_CAP_CHARS - COMPLETION.length - 400);
    const parts = parseOwnedBlocks(`${preserved}${REGION}\n\n${COMPLETION}`) as OwnedBlocks;
    const compose = (r: string) => composeBody(parts, { region: r });
    const capped = capBody(compose, full, sizeOnly);
    assert.equal(capped.capped, true);
    assert.equal(capped.body, `${preserved}${sizeOnly}\n\n${COMPLETION}`, "preserved text and completion block never truncated");
    // Under the cap: the full region stays.
    const shortParts = parseOwnedBlocks(`hi\n${REGION}\n\n${COMPLETION}`) as OwnedBlocks;
    const fine = capBody((r) => composeBody(shortParts, { region: r }), full, sizeOnly);
    assert.deepEqual(fine, { body: `hi\n${full}\n\n${COMPLETION}`, capped: false });
    // Preserved text alone over the cap: never truncated (the body stays over).
    const huge = parseOwnedBlocks(`${"q".repeat(BODY_CAP_CHARS)}${REGION}\n${COMPLETION}`) as OwnedBlocks;
    const over = capBody((r) => composeBody(huge, { region: r }), full, sizeOnly);
    assert.equal(over.capped, true);
    assert.ok(over.body!.startsWith("q".repeat(BODY_CAP_CHARS)));
  });
});

// ── Completion block ───────────────────────────────────────────────────────────────────────

describe("renderCompletionBlock (D10, D12, D14)", () => {
  it("renders the staleness line inside its own inner marker, only when the heads differ", () => {
    const block = renderCompletionBlock({
      issueIid: 7,
      branch: "agent/issue-7",
      closes: false,
      staleness: { describedSha: "1111111aaaa", headSha: "2222222bbbb" },
    });
    assert.ok(
      block.includes(
        `${STALENESS_START}\nThis description may be outdated: it describes \`1111111\`; the PR head is \`2222222\`.\n${STALENESS_END}`,
      ),
      block,
    );
    assert.ok(block.indexOf(STALENESS_START) > block.indexOf(COMPLETION_START));
    assert.ok(block.indexOf(STALENESS_END) < block.indexOf(COMPLETION_END));
    const current = renderCompletionBlock({
      issueIid: 7,
      branch: "agent/issue-7",
      closes: false,
      staleness: { describedSha: HEAD, headSha: HEAD.slice(0, 7) },
    });
    assert.ok(!current.includes(STALENESS_START));
  });

  it("puts the unverified banner inside the block, first", () => {
    const block = renderCompletionBlock({ issueIid: 7, branch: "b", closes: false, banner: "> BANNER" });
    assert.ok(block.startsWith(`${COMPLETION_START}\n> BANNER\n\nRelated to #7.`), block);
  });

  it("a partial (owner or operator) never renders Closes, whatever closes says", () => {
    const deferred: NonNullable<ClaimConfig["completion_scope"]>["deferred"] = [
      { milestone_id: "m3", title: "Third", reason: "later" },
    ];
    for (const block of [
      renderCompletionBlock({ issueIid: 7, branch: "b", closes: true, completionScope: { deferred } }),
      renderCompletionBlock({ issueIid: 7, branch: "b", closes: true, scopeCapped: { completedCount: 1, total: 2 } }),
      renderCompletionBlock({ issueIid: 7, branch: "b", closes: true, completionScope: { deferred }, scopeCapped: { completedCount: 1 } }),
    ]) {
      assert.doesNotMatch(block, /Closes #/);
      assert.equal(closingDirectiveFor(block, 7), false, block);
    }
    assert.match(renderCompletionBlock({ issueIid: 7, branch: "b", closes: true }), /\nCloses #7\n/);
  });
});

describe("mrDescription over the renderer: every PR-producing kind renders", () => {
  const kinds: Array<[RunKind, Partial<ClaimResponse>, RegExp]> = [
    ["issue", { issue_iid: 7 }, /Related to #7\.\n\nCloses #7/],
    [
      "ci_fix",
      { pipeline: { id: 5, ref: "main", sha: "a".repeat(40), web_url: "https://gitlab.example/g/p/-/pipelines/5", failed_jobs: [] } },
      /^CI fix for the failing pipeline on `main`: https:\/\/gitlab\.example\/g\/p\/-\/pipelines\/5$/m,
    ],
    ["self_improve", { issue_iid: 77 }, /Tracking issue: #77 \(a stable container this PR never closes\)\./],
    ["prompt", { issue_iid: null }, /^Ad-hoc scheduled prompt run \(PRD #241\)/m],
    ["task", { issue_iid: null, branch: "uzi/task/x", base_branch: "dev" }, /^Handoff task \(PRD #400\) on `the\/branch` \(branched from `dev`\)/m],
    ["mr_rework", { issue_iid: null, branch: "agent/issue-42" }, /^Automated MR rework \(PRD #700\) addressing review feedback on `the\/branch`/m],
  ];
  for (const [kind, over, line] of kinds) {
    it(kind, () => {
      const claim = makeClaim({ kind, issue_title: "T", ...over } as Partial<ClaimResponse>);
      const body = mrDescription(claim, "the/branch", undefined, undefined, undefined, undefined, undefined, undefined, true, undefined, false, SIZE, {
        headSha: HEAD,
        targetBranch: "main",
      });
      const p = parseOwnedBlocks(body);
      assert.equal(p.kind, "ok", body);
      const b = p as OwnedBlocks;
      assert.equal(b.before, "");
      assert.equal(b.region, `${REGION_START}\n${SIZE}\n\nDescribes \`e4020cc\` against \`main\`.\n${REGION_END}`);
      assert.equal(b.between, "\n\n");
      assert.equal(b.after, "");
      assert.match(b.completion!, line);
      assert.ok(b.completion!.endsWith("---\nOpened by uzi from `the/branch`. A human reviews and merges; uzi never merges.\n" + COMPLETION_END));
      if (kind !== "issue") {
        assert.doesNotMatch(body, /Closes #/, `${kind} never closes`);
        assert.ok(!body.includes("#null"), body);
      }
    });
  }

  it("the agents line is one short line without the agent list; kind sections render after it", () => {
    const claim = makeClaim({ kind: "self_improve", issue_iid: 77 });
    const section = [{ fixed: "" }, { fixed: "---" }, { fixed: "### Self-improvement run" }, { fixed: "" }, { fixed: "evidence" }];
    const body = mrDescription(claim, "b", { source: "repo", agents: ["lead", "coder"] }, section);
    assert.ok(body.includes("Internally reviewed by the repository's own agents, not uzi's built-in reviewer."));
    assert.ok(!body.includes("coder"));
    assert.ok(body.includes("reviewer.\n\n---\n### Self-improvement run\n\nevidence\n\n---\nOpened by uzi"), body);
  });

  it("the banner passed through opts renders inside the completion block", () => {
    const claim = makeClaim({ issue_iid: 7 });
    const body = mrDescription(claim, "b", undefined, undefined, undefined, undefined, undefined, undefined, false, undefined, false, undefined, { banner: "> ⚠️ **Completion unverified.** x" });
    assert.ok(body.startsWith(`${COMPLETION_START}\n> ⚠️ **Completion unverified.** x\n\nRelated to #7.`), body);
  });
});

// ── closingDirectiveFor: the port of the api's prDescClosing ───────────────────────────────

describe("closingDirectiveFor (port of api pr_description_sanitize.go prDescClosing)", () => {
  // TestSanitizePrDescriptionClosingKeywords' table: every keyword x every reference x colon.
  const keywords = [
    "Close", "Closes", "closed", "Closing", "Fix", "Fixes", "fixed", "Fixing",
    "Resolve", "Resolves", "resolved", "Resolving", "Implement", "Implements", "implemented", "Implementing",
    "CLOSES", "fIxEs",
  ];
  const refs: Array<[string, boolean]> = [
    // [reference, resolves to issue 12 of repo "o/r"]
    ["#12", true], ["issue #12", true], ["issues #12", true], ["Issue #12", true],
    ["owner/repo#12", false], ["group/sub/project#12", false], ["project#12", false], ["o/r#12", true], ["r#12", true],
    ["https://gitlab.example.com/g/p/-/issues/12", false],
    ["https://github.com/o/r/issues/12", true],
    ["https://codeberg.org/o/r/issues/12", true],
    ["https://gitlab.example.com/o/r/-/work_items/12", true],
  ];
  it("catches every keyword/reference form of the api table (repo unknown ⇒ every form)", () => {
    for (const kw of keywords) {
      for (const [ref, ours] of refs) {
        for (const colon of ["", ":"]) {
          const text = `This PR ${kw}${colon} ${ref} today.`;
          assert.equal(closingDirectiveFor(text, 12), true, text);
          assert.equal(closingDirectiveFor(text, 12, "o/r"), ours, `${text} (repo o/r)`);
          assert.equal(closingDirectiveFor(text, 13), false, `${text} (other issue)`);
          // The api's breaker (U+200B after the keyword's first letter) neutralises every form.
          const broken = `This PR ${kw[0]}${ZW}${kw.slice(1)}${colon} ${ref} today.`;
          assert.equal(closingDirectiveFor(broken, 12), false, broken);
        }
      }
    }
  });

  it("catches the api's extra, emphasis and Unicode-space forms", () => {
    for (const text of [
      "**Fixes** #12", "Fixes\n#12", "closes GH-12", "fix [#12](https://github.com/o/r/issues/12)", "Fixes &#35;12",
      "Fixes &num;12", "_Fixes_ #12", "__Fixes__ #12", "Fix**es** #12", "Fixes:#12", "Fixes#12", "Fixes\u2003#12",
      "Fixes\u3000#12", "Fixes\u00a0#12", "Fixes [#12]", "~~Fixes~~ #12", "`Fixes` #12", "**Fixes:** #12", "a_Fixes #12",
      "Closes PROJ-12", "Fixes\\_ #12",
    ]) {
      assert.equal(closingDirectiveFor(text, 12), true, text);
    }
  });

  it("follows GitLab reference lists", () => {
    assert.equal(closingDirectiveFor("Resolves:   #7, #8 and #9", 9), true);
    assert.equal(closingDirectiveFor("Closes #1, #2, issue #3", 3), true);
    assert.equal(closingDirectiveFor("Closes #1 and o/r#4", 4, "o/r"), true);
    assert.equal(closingDirectiveFor("Closes #1 and then see #4", 4), false);
  });

  it("leaves plain references and keyword-free text alone", () => {
    for (const text of [
      "See #12 for context.", "This fixes a crash in the parser.", "Related to #12.",
      "Implements part of #12 (partial delivery — owner scope decision; this MR does NOT close the issue).",
      "Work on issue #12", "prefixes #12", "Fixed-width #12",
    ]) {
      assert.equal(closingDirectiveFor(text, 12), false, text);
    }
  });
});

// ── Property: no rendered region or completion block carries a closing directive ─────────────

/** A seeded PRNG (mulberry32), so the property run is deterministic. */
function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// The api sanitizer's step 3, as far as closing directives go: a U+200B after the first letter of
// every keyword a reference follows, in the raw, markers-removed and markers-blanked views
// (neutralizeClosingDirectives), and a leading `#` escaped. The oracle regex is the api's.
const API_CLOSING = /\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b[\s\p{Z}]*:?[\s\p{Z}]*(?:issues?[\s\p{Z}]*)?(?:#\d+|gh-\d+|[\w.-]+(?:\/[\w.-]+)*#\d+|[A-Za-z][A-Za-z0-9_]+-\d+|https?:\/\/[^\s<>()]*?\/(?:issues|work_items)\/\d+)/giu;
function apiShaped(s: string): string {
  let t = s.replace(/\s+/g, " ").trim();
  if (t.startsWith("#")) t = `\\${t}`;
  const at = new Set<number>();
  const markers = "*_~`[]\\";
  const removed: number[] = [];
  let rv = "";
  let bv = "";
  for (let i = 0; i < t.length; i++) {
    if (markers.includes(t[i]!)) {
      bv += " ";
      continue;
    }
    bv += t[i];
    rv += t[i];
    removed.push(i);
  }
  for (const m of t.matchAll(API_CLOSING)) at.add(m.index);
  for (const m of rv.matchAll(API_CLOSING)) at.add(removed[m.index]!);
  for (const m of bv.matchAll(API_CLOSING)) at.add(m.index);
  let out = "";
  for (let i = 0; i < t.length; i++) out += t[i] + (at.has(i) ? ZW : "");
  return out;
}

describe("property: rendered blocks never carry a closing directive", () => {
  const WORDS = [
    "Closes", "fixes", "Resolved", "implementing", "closing:", "**Fixes**", "_fix_", "`closes`", "issue", "issues",
    "#7", "#12", "o/r#7", "gh-7", "PROJ-7", "https://github.com/o/r/issues/7", "and", ",", "the", "parser", "@alice", "[#7]", "~~",
  ];
  it("sanitizer-shaped fields (U+200B breakers) render into a region no forge reads as closing", async () => {
    const rnd = prng(1798);
    const pick = () => WORDS[Math.floor(rnd() * WORDS.length)]!;
    const phrase = () => Array.from({ length: 1 + Math.floor(rnd() * 6) }, pick).join(" ");
    for (let i = 0; i < 150; i++) {
      const f = await mint({
        summary: apiShaped(phrase()),
        changes: Array.from({ length: Math.floor(rnd() * 4) }, () => apiShaped(phrase())),
        scope_notes: [{ kind: "deferred", text: apiShaped(phrase()) }],
        review_pointers: [apiShaped(phrase())],
        verification: [{ command: apiShaped(phrase()), result: "pass", verified_at_sha: "abcdef1" }],
      });
      const r = renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main", source: rnd() < 0.5 ? "lead_only" : "generated" }, f);
      assert.equal(r.withFields, true);
      for (const n of [7, 12]) {
        assert.equal(closingDirectiveFor(r.text, n), false, `iteration ${i} closes #${n}:\n${r.text}`);
      }
    }
  });

  it("positive control: the same fields WITHOUT the breakers are caught", async () => {
    const f = await mint({ summary: "This work fixes o/r#7 today." });
    assert.equal(closingDirectiveFor(renderRegion({ sizeLine: SIZE }, f).text, 7, "o/r"), true);
  });

  it("deterministic interpolations are escaped: milestone titles, reasons, criteria, branches, gate dirs", () => {
    const hostile = ["Closes #1", "Fixes: o/r#1", "_Resolves_ #1", "@user please", "/merge", "fixes", "implements https://github.com/o/r/issues/1"];
    const rnd = prng(46);
    for (let i = 0; i < 100; i++) {
      const h = () => hostile[Math.floor(rnd() * hostile.length)]!;
      const deferred = [{ milestone_id: h(), title: h(), reason: h() }];
      const accepted = [{ id: h(), milestone_id: "m1", text: h(), reason: h() }];
      const branch = `x/${h().replace(/\s+/g, "")}`;
      const variants = [
        renderCompletionBlock({ issueIid: 1, branch, closes: true, completionScope: { deferred, accepted }, gatesUnverified: [h(), "#1"] }),
        renderCompletionBlock({ issueIid: 1, branch, closes: false, completionScope: { accepted }, gatesUnverified: [h()], banner: "> held" }),
        renderCompletionBlock({ issueIid: 1, branch, closes: true, scopeCapped: { completedCount: 1 } }),
        renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: branch }).text,
      ];
      for (const text of variants) {
        assert.equal(closingDirectiveFor(text, 1, "o/r"), false, text);
        assert.doesNotMatch(text, /(^|[^A-Za-z0-9])@(?!\u200B)user/u, text);
      }
    }
    // A closing completion still carries exactly its own `Closes #1`.
    assert.equal(closingDirectiveFor(renderCompletionBlock({ issueIid: 1, branch: "b", closes: true }), 1), true);
  });

  it("escapeInline / codeSpan neutralise keywords, mentions, markdown and a leading slash", () => {
    assert.equal(escapeInline("/merge"), "\\/merge");
    assert.equal(escapeInline("Closes #1"), `C${ZW}loses #1`);
    assert.equal(escapeInline("ping @user"), `ping @${ZW}user`);
    assert.equal(escapeInline("a@b.c"), "a@b.c");
    assert.equal(escapeInline("# Title\n\n- item"), "\\# Title - item");
    assert.equal(escapeInline("[x](http://e) <b>*y*</b>"), "\\[x\\](http://e) \\<b\\>\\*y\\*\\</b\\>");
    assert.equal(escapeInline("1. first"), "1\\. first");
    assert.equal(escapeInline("Third milestone"), "Third milestone");
    assert.equal(codeSpan("fix/login"), "`fix/login`", "a keyword with no reference stays copyable");
    assert.equal(codeSpan("x/Closes#1"), `\`x/C${ZW}loses#1\``);
    assert.equal(codeSpan("a`b"), "``a`b``");
    assert.equal(codeSpan("`a"), "`` `a ``");
  });
});

describe("renderBody", () => {
  it("region, blank line, completion block; no region ⇒ the completion block alone", () => {
    assert.equal(renderBody(REGION, COMPLETION), `${REGION}\n\n${COMPLETION}`);
    assert.equal(renderBody(undefined, COMPLETION), COMPLETION);
  });
});
