import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SanitizedPrDescriptionFields, WorkerClient } from "../src/client.js";
import {
  COMPLETION_END,
  COMPLETION_START,
  FORGE_BODY_MAX_CHARS,
  REGION_END,
  REGION_START,
  closingDirectiveFor,
  codeSpan,
  composeBody,
  parseOwnedBlocks,
  renderBody,
  renderCompletionBlock,
  renderRegion,
  type KindSection,
  type OwnedBlocks,
} from "../src/pr-description.js";
import { RUN_KINDS, type ClaimConfig, type ClaimResponse, type RawPrDescriptionFields, type RunKind } from "../src/protocol.js";
import { mrDescription } from "../src/runner.js";
import { guardCriticalMrSection, selfImproveMrSection } from "../src/self-improve.js";
import { makeClaim, nullLogger } from "./helpers.js";

// PRD #1798 M6a hardening (security audit of c3731080): agent-chosen paths in kind sections (H1),
// Forgejo `!N` references (H2), linear scans and byte bounds (M1), rendered views of a closing
// directive (L1), marker-shaped text inside code (L2), the ci_fix URL guard (NB-2) and codeSpan nits.

const ZW = "\u200B";
const SIZE = "**Size:** code +3 −1 · 1 file";

/** GitLab's quick-action extractor, emulated: inline code is consumed first, then a command is a
 *  line that starts with `/word`. */
function quickActions(body: string): string[] {
  const noCode = body.replace(/(`+)(?!`)[\s\S]*?[^`]\1(?!`)/gu, "");
  return [...noCode.matchAll(/^\/[A-Za-z_]+/gmu)].map((m) => m[0]);
}

/** The body with inline code spans removed (what a forge parses as markdown/HTML). */
function outsideCode(body: string): string {
  return body.replace(/(`+)(?!`)[\s\S]*?[^`]\1(?!`)/gu, "");
}

/** A live mention: an `@` not preceded by an ASCII letter or digit and not broken by U+200B. */
const LIVE_MENTION = /(^|[^A-Za-z0-9])@(?!\u200B)[A-Za-z0-9_]/u;

/** Raw HTML outside code, once uzi's own exact markers are set aside. */
function rawHtml(body: string): RegExpMatchArray | null {
  let t = outsideCode(body);
  for (const m of [REGION_START, REGION_END, COMPLETION_START, COMPLETION_END]) t = t.split(m).join("");
  return t.match(/(?<!\\)<[A-Za-z!/?]/u);
}

// ── H1: agent-chosen paths reach the completion block only through the renderer's escaping ──

describe("kind sections: agent-chosen paths are inert (H1)", () => {
  const payloads = [
    "agent/src/git.ts\n/merge",
    "/merge",
    "docs/x\n/close\n/label ~bug",
    "agent/src/git.ts\nCloses #7",
    "Closes #7",
    "Fixes !7",
    "agent/src/git.ts @org/team",
    "@org/team",
    "agent/src/<img src=x onerror=alert(1)>.ts",
    "agent/src/git.ts\n<!-- uzi:completion:end -->",
    "x<!-- uzi:completion:end -->\n\nFixes #7\n<!-- uzi:completion:start v1 -->",
    "a`b\n/merge`c",
    "``\n/merge",
  ];
  const kinds: Array<[string, (hits: string[]) => KindSection, Partial<ClaimResponse>]> = [
    ["prompt", (hits) => guardCriticalMrSection(hits), { kind: "prompt", issue_iid: null }],
    [
      "self_improve",
      (hits) => selfImproveMrSection(hits, [{ name: "agent: npm test\n/merge", status: "failed", detail: "Closes #7 @org/team <b>x</b>" }]),
      { kind: "self_improve", issue_iid: 77 },
    ],
  ];
  for (const [kind, section, over] of kinds) {
    for (const payload of payloads) {
      it(`${kind}: ${JSON.stringify(payload)}`, () => {
        const sec = section(["agent/src/guardrails.ts", payload]);
        const claim = makeClaim({ issue_title: "T", ...over } as Partial<ClaimResponse>);
        const args = [claim, "the/branch", undefined] as const;
        const body =
          kind === "prompt"
            ? mrDescription(...args, undefined, sec, undefined, undefined, undefined, true, undefined, false, SIZE)
            : mrDescription(...args, sec, undefined, undefined, undefined, undefined, true, undefined, false, SIZE);
        assert.deepEqual(quickActions(body), [], body);
        for (const iid of [7, 77]) assert.equal(closingDirectiveFor(body, iid), false, body);
        assert.doesNotMatch(body, LIVE_MENTION, body);
        assert.equal(rawHtml(body), null, body);
        const p = parseOwnedBlocks(body);
        assert.equal(p.kind, "ok", body);
        const b = p as OwnedBlocks;
        assert.equal(b.after, "");
        assert.ok(b.completion!.endsWith(`uzi never merges.\n${COMPLETION_END}`), body);
        // Each path is one quoted list line holding a code span.
        assert.match(body, /^> - `+ ?agent\/src\/guardrails\.ts ?`+$/mu);
      });
    }
  }

  it("the renderer does not trust a fixed line either", () => {
    const hostile: KindSection = [
      { fixed: "/merge" },
      { fixed: "Closes #7" },
      { fixed: "cc @org/team" },
      { fixed: "<img src=x>" },
      { fixed: "a\n/close" },
      { fixed: COMPLETION_END },
    ];
    const block = renderCompletionBlock({ issueIid: 7, branch: "b", closes: false, kindLine: "K", kindSections: [hostile] });
    assert.deepEqual(quickActions(block), []);
    assert.equal(closingDirectiveFor(block, 7), false, block);
    assert.doesNotMatch(block, LIVE_MENTION);
    assert.equal(rawHtml(block), null, block);
    assert.equal(parseOwnedBlocks(block).kind, "ok");
    // The leading `/` is backslash-escaped: the line reads exactly `\/merge`, not a template literal.
    assert.equal(renderCompletionBlock({ branch: "b", closes: false, kindLine: "K", kindSections: [[{ fixed: "/merge" }]] }).split("\n")[3], "\\/merge");
  });

  it("an empty section renders nothing, a check result is escaped", () => {
    const empty = renderCompletionBlock({ branch: "b", closes: false, kindLine: "K", kindSections: [[], undefined] });
    assert.ok(empty.includes("K\n\n---\nOpened by uzi"), empty);
    const sec = selfImproveMrSection([], [{ name: "x_y *z*", status: "passed", detail: "exit 0" }]);
    const block = renderCompletionBlock({ branch: "b", closes: false, kindLine: "K", kindSections: [sec] });
    assert.ok(block.includes("- ✅ x\\_y \\*z\\* — passed (exit 0)"), block);
  });
});

// ── H2: Forgejo `!N` references ──

describe("closingDirectiveFor: Forgejo `!N` references (H2)", () => {
  it("reads `!N` and `path!N` after a keyword as closing", () => {
    for (const text of ["Fixes !7", "closes: !7", "Resolves issue !7", "Fixes o/r!7", "Fix**es** !7", "Closes #1, !7"]) {
      assert.equal(closingDirectiveFor(text, 7, "o/r"), true, text);
    }
    assert.equal(closingDirectiveFor("Fixes other/repo!7", 7, "o/r"), false);
    assert.equal(closingDirectiveFor("See !7", 7), false);
    assert.equal(closingDirectiveFor(`F${ZW}ixes !7`, 7), false);
  });

  it("codeSpan and escapeInline-backed text break a keyword a `!N` follows", () => {
    assert.equal(codeSpan("Closes !7"), `\`C${ZW}loses !7\``);
    assert.equal(closingDirectiveFor(codeSpan("Closes !7"), 7), false);
  });
});

// ── L1: rendered views ──

describe("closingDirectiveFor: rendered views (L1)", () => {
  it("catches directives hidden by entities, tags, comments and format characters", () => {
    for (const [text, iid] of [
      ["Fix<b></b>es #7", 7],
      ["Fi&#120;es #7", 7],
      ["Fi&#x78;es #7", 7],
      ["Fixes <b>#7</b>", 7],
      ["Fixes<!-- --> #7", 7],
      ["Fixes <span>o/r#7</span>", 7],
      ["Fixes #&#55;", 7],
      ["Fixes&nbsp;#5", 5],
      ["Fixes&#32;#5", 5],
      ["Clo<!-- -->ses #5", 5],
      ["Fixes\u2060 #5", 5],
      ["Fi\u00ADxes #5", 5],
      ["Fi&shy;xes #5", 5],
      ["Fixes &amp;#35;5", 5],
      ["Fixes [#5](https://example.com)", 5],
      ["Fixes&num;5", 5],
    ] as Array<[string, number]>) {
      assert.equal(closingDirectiveFor(text, iid, "o/r"), true, text);
    }
  });

  it("keeps the api's U+200B breaker (a sanitized field stays non-closing)", () => {
    for (const text of [`F${ZW}ixes #5`, `F${ZW}ixes &lt;T&gt; #5`, `C${ZW}loses &#35;5`, `F${ZW}ix<b></b>es #5`]) {
      assert.equal(closingDirectiveFor(text, 5), false, text);
    }
  });

  it("fails closed on an issue number that is not a non-negative safe integer", () => {
    for (const iid of [-1, 1.5, Number.NaN, Number.POSITIVE_INFINITY, 2 ** 53]) {
      assert.equal(closingDirectiveFor("nothing here", iid), true, String(iid));
    }
    assert.equal(closingDirectiveFor("nothing here", 0), false);
  });

  it("fails closed on text over the forge cap, and on entities nested past the pass bound", () => {
    assert.equal(closingDirectiveFor("a".repeat(FORGE_BODY_MAX_CHARS + 1), 7), true);
    assert.equal(closingDirectiveFor("a".repeat(FORGE_BODY_MAX_CHARS), 7), false);
    assert.equal(closingDirectiveFor(`&${"amp;".repeat(80)}#35;`, 7), true);
  });
});

// ── M1: linear scans ──

describe("scans stay linear on adversarial input (M1)", () => {
  // A ratio test, not a fixed budget: each shape is timed at N and at 8N characters (best of three,
  // to shed scheduler noise), and 8x the input must take under 32x the time. Linear code measured
  // 5-21x here (cache and GC effects grow with the input, strongest at small sizes, which is why a
  // 4x/8x ratio flaked at 7.6x); quadratic code is 64x. A 20 ms floor keeps a few-millisecond base
  // from turning jitter into a ratio, a pair that misses the ratio is timed once more before it
  // fails (one noisy pair is not a regression; a quadratic scan misses both), and an absolute
  // ceiling (checked at N first, so a quadratic regression fails in seconds rather than running the
  // 8N input for minutes) catches a scan that is slow at every size.
  const N = 16 * 1024;
  const SCALE = 8;
  const MAX_RATIO = 32;
  const FLOOR_MS = 20;
  const CEILING_MS = 2_000;
  const fillTo = (unit: string, n: number) => unit.repeat(Math.ceil(n / unit.length)).slice(0, n);
  const best = (f: () => unknown) => {
    let min = Infinity;
    for (let i = 0; i < 3; i++) {
      const t = performance.now();
      f();
      min = Math.min(min, performance.now() - t);
      if (min > CEILING_MS) break;
    }
    return min;
  };
  const assertLinear = (shape: (n: number) => string, scan: (s: string) => unknown) => {
    const small = shape(N);
    const large = shape(SCALE * N);
    const head = JSON.stringify(small.slice(0, 24));
    const pair = () => {
      const t1 = best(() => scan(small));
      assert.ok(t1 < CEILING_MS / SCALE, `${head}…: ${t1.toFixed(1)} ms at ${small.length}`);
      const t8 = best(() => scan(large));
      const label = `${head}…: ${t1.toFixed(1)} ms at ${small.length}, ${t8.toFixed(1)} ms at ${large.length}`;
      assert.ok(t8 < CEILING_MS, label);
      return { ok: t8 < MAX_RATIO * Math.max(t1, FLOOR_MS), label };
    };
    const first = pair();
    if (first.ok) return;
    const again = pair();
    assert.ok(again.ok, `${first.label}; retried: ${again.label}`);
  };

  it("closingDirectiveFor: 8x the input takes under 32x the time, for each shape", () => {
    const units = [
      "fixes:https://",
      "fix-",
      "fix.",
      // Keyword-glued path chains: a keyword at every segment, each starting a path reference that
      // runs on over the following segments (quadratic before the segment count was bounded).
      "fix-/",
      "fix-a/",
      "fix./",
      "close.a/",
      "fix-fix/",
      "fixes a/",
      "fixes #1 ",
      "fixes #1, ",
      "fixes https://a/",
      "![",
      "[",
      "[a](",
      "[a][",
      "<!--",
      "<!",
      "-",
      "--!",
      '<a b="',
      "<a b='x' c=\"y\" d=z ",
      "<details ",
      "</b ",
      "&amp;",
      "&#",
      "&",
      "`",
      "*_",
      "<https://",
    ];
    const shapes: Array<(n: number) => string> = [
      ...units.map((u) => (n: number) => fillTo(u, n)),
      (n) => `${fillTo("fix-", n)}*`,
      (n) => `${fillTo("fix.", n)}_`,
      (n) => `fixes${" ".repeat(n)}`,
      (n) => `fixes:${"\u2003".repeat(n)}x`,
      // A reference list whose items are themselves keyword + reference: every inner keyword starts
      // its own walk over the rest of the list (quadratic before the continuation memo).
      (n) => `Fixes ${fillTo("fix#1 ", n)}`,
      (n) => `Fixes ${fillTo("fix!1 ", n)}`,
      (n) => `Fixes ${fillTo("fix#1,", n)}`,
    ];
    for (const shape of shapes) assertLinear(shape, (s) => closingDirectiveFor(s, 7, "o/r"));
  });

  it("a deep path reference still resolves: up to 22 segments (GitLab's 20 ancestor groups)", () => {
    const deep = ["top", ...Array.from({ length: 20 }, (_, i) => `sub-${i}.x`), "repo"].join("/");
    assert.equal(deep.split("/").length, 22);
    for (const [text, repo] of [
      [`Fixes ${deep}#7`, deep],
      [`Fixes ${deep}#7`, undefined],
      [`Fixes ${deep}!7`, deep],
      [`Closes #1, ${deep}#7`, deep],
      [`fix-${deep}#7`, undefined],
    ] as Array<[string, string | undefined]>) {
      assert.equal(closingDirectiveFor(text, 7, repo), true, text);
    }
    assert.equal(closingDirectiveFor(`Fixes ${deep}#8`, 7, deep), false);
  });

  it("an inner keyword of a reference list still reads its own first reference", () => {
    // `fix#7` is a path reference (`fix`, not o/r) to the outer walk, a bare `#7` to its own keyword.
    assert.equal(closingDirectiveFor("Fixes #1 fix#1 fix#1 fix#7", 7, "o/r"), true);
    assert.equal(closingDirectiveFor("Fixes #1 fix#1 fix#1 fix#8", 7, "o/r"), false);
    assert.equal(closingDirectiveFor("Fixes #1 fix!1 fix#1, fix!7", 7, "o/r"), true);
  });

  it("parseOwnedBlocks: 8x the input takes under 32x the time, for each shape", () => {
    const units = [
      "<!-- uzi:description:start v1 ",
      "<!-- uzi:completion:end ",
      "<!--",
      "<!-- uzi:description:start",
      "`",
      "``x",
      "\\`",
      "```\n",
      "```a\n",
      "> ```\n",
      "~~~\nx\n",
      "\n",
      `\`\`\`\n${REGION_START}\n`,
    ];
    const shapes: Array<(n: number) => string> = [
      ...units.map((u) => (n: number) => fillTo(u, n)),
      (n) => `${fillTo("`a", n)}\n${REGION_START}\n${REGION_END}`,
    ];
    for (const shape of shapes) assertLinear(shape, (s) => parseOwnedBlocks(s));
  });

  it("parseOwnedBlocks refuses a body over the forge cap as malformed", () => {
    const body = `${REGION_START}\n${REGION_END}\n${"a".repeat(FORGE_BODY_MAX_CHARS)}`;
    assert.deepEqual(parseOwnedBlocks(body), { kind: "malformed", reason: "oversize" });
  });
});

// ── L2: marker-shaped text inside code ──

describe("parseOwnedBlocks ignores markers inside code (L2)", () => {
  const REGION = `${REGION_START}\n${SIZE}\n${REGION_END}`;
  const COMPLETION = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: false });
  const indent = (t: string) => t.split("\n").map((l) => (l ? `  ${l}` : l)).join("\n");

  // Copies that are NOT uzi's exact markers alone on a line at column 0 (indented, `>`-quoted or
  // mid-line). A column-0 whole-line copy in a fence now reads malformed (see the fail-closed
  // describe below): uzi never writes a marker inside code, and the fence model is not CommonMark.
  for (const [name, copy] of [
    ["an indented copy in a ``` fence", "```\n" + indent(REGION + "\n\n" + COMPLETION) + "\n```"],
    ["an indented copy in a ~~~~ fence", "~~~~md\n" + indent(REGION + "\n" + COMPLETION) + "\n~~~~"],
    ["a quoted fence", "> ```\n> " + REGION_START + "\n> " + COMPLETION_END + "\n> ```"],
    ["inline code spans", "Our markers are `" + REGION_START + "` and ``" + COMPLETION_END + "``."],
  ] as Array<[string, string]>) {
    it(`${name} holding copies of both markers parses ok, human text preserved`, () => {
      const before = `Human notes.\n\n${copy}\n\n`;
      const after = `\n\nMore human text:\n\n${copy}\n`;
      const body = `${before}${REGION}\n\n${COMPLETION}${after}`;
      const p = parseOwnedBlocks(body);
      assert.equal(p.kind, "ok", name);
      const b = p as OwnedBlocks;
      assert.equal(b.before, before);
      assert.equal(b.region, REGION);
      assert.equal(b.between, "\n\n");
      assert.equal(b.completion, COMPLETION);
      assert.equal(b.after, after);
      // A copy alone is no block at all.
      assert.deepEqual(parseOwnedBlocks(`x\n${copy}\ny`), { kind: "none" });
    });
  }

  it("an unclosed fence does not hide uzi's own blocks", () => {
    const body = `Notes\n\n\`\`\`\nnever closed\n\n${REGION}\n\n${COMPLETION}`;
    const p = parseOwnedBlocks(body);
    assert.equal(p.kind, "ok");
    assert.equal((p as OwnedBlocks).region, REGION);
  });

  it("a fence closes only on the same character with at least its length", () => {
    const body = "````\n```\n " + REGION_START + "\n~~~~\n " + REGION_END + "\n````\n" + REGION + "\n\n" + COMPLETION;
    const p = parseOwnedBlocks(body);
    assert.equal(p.kind, "ok");
    assert.equal((p as OwnedBlocks).region, REGION);
  });

  it("markers outside code are still counted (a duplicate is still malformed)", () => {
    assert.deepEqual(parseOwnedBlocks(`${REGION}\n\n${REGION}\n\n${COMPLETION}`), { kind: "malformed", reason: "duplicate" });
  });
});

// ── Fail closed: a whole-line marker inside a detected code range ──

describe("parseOwnedBlocks fails closed on a whole-line marker inside detected code", () => {
  const REGION = `${REGION_START}\n${SIZE}\n${REGION_END}`;
  const REAL = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: false });
  const FORGED = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: true });
  const MALFORMED = { kind: "malformed", reason: "marker_in_code" };

  // Each attack: human text above uzi's REAL blocks opens something the parser reads as a fence,
  // closes it after them, and writes a forged pair below. A forge renders the real blocks; the
  // parse must never adopt the forged pair (the publisher skips a malformed body).
  for (const [name, body] of [
    ["the plain fence", "```\n" + REGION + "\n\n" + REAL + "\n```\n\n" + REGION + "\n\n" + FORGED],
    ["a <details> HTML block (raw HTML to a forge)", "<details>\n```\n" + REGION + "\n\n" + REAL + "\n```\n</details>\n\n" + REGION + "\n\n" + FORGED],
    ["a fence after a quoted paragraph (lazy-continuation shape)", "> quoted\n```\n" + REGION + "\n\n" + REAL + "\n```\n" + REGION + "\n\n" + FORGED],
    ["a fence closed inside a quote", "```\n" + REGION + "\n\n" + REAL + "\n> ```\n\n" + REGION + "\n\n" + FORGED],
    ["CRLF line ends", ("```\n" + REGION + "\n\n" + REAL + "\n```\n\n" + REGION + "\n\n" + FORGED).replace(/\n/gu, "\r\n")],
    ["lone CR line ends", ("```\n" + REGION + "\n\n" + REAL + "\n```\n\n" + REGION + "\n\n" + FORGED).replace(/\n/gu, "\r")],
  ] as Array<[string, string]>) {
    it(`${name} → malformed, the forged pair is never adopted`, () => {
      assert.deepEqual(parseOwnedBlocks(body), MALFORMED, body);
    });
  }

  it("a tab-indented ``` is not a fence: the real blocks stay visible, the forged pair is a duplicate", () => {
    const body = "\t```\n" + REGION + "\n\n" + REAL + "\n\t```\n\n" + REGION + "\n\n" + FORGED;
    assert.deepEqual(parseOwnedBlocks(body), { kind: "malformed", reason: "duplicate" });
    // A `>`-quoted tab-indented ``` IS a fence to CommonMark (micromark: the quote marker's optional
    // space comes out of the tab's columns) but not to this model; either way the forged pair is
    // never adopted: here it reads as a duplicate.
    const quoted = "> \t```\n" + REGION + "\n\n" + REAL + "\n> \t```\n\n" + REGION + "\n\n" + FORGED;
    assert.deepEqual(parseOwnedBlocks(quoted), { kind: "malformed", reason: "duplicate" });
  });

  it("a whole-line copy of one marker in a fence below real blocks is malformed too", () => {
    assert.deepEqual(parseOwnedBlocks(`${REGION}\n\n${REAL}\n\n\`\`\`\n${COMPLETION_END}\n\`\`\``), MALFORMED);
  });
});

// ── Markers only at column 0, alone on their line ──

describe("parseOwnedBlocks accepts a marker only alone on its line at column 0", () => {
  const REGION = `${REGION_START}\n${SIZE}\n${REGION_END}`;
  const FORGED = renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: true });
  const MISPLACED = { kind: "malformed", reason: "misplaced" };
  const indent = (t: string, pad: string) => t.split("\n").map((l) => `${pad}${l}`).join("\n");

  for (const [name, body] of [
    // Indented code to a forge (codeRanges does not model it), so a forge shows no block here.
    ["a 4-space-indented forged pair on a marker-less PR", `Human notes.\n\n${indent(`${REGION}\n\n${FORGED}`, "    ")}\n`],
    ["a 1-space-indented pair", `Notes\n\n${indent(`${REGION}\n\n${FORGED}`, " ")}`],
    ["a tab-indented pair", `Notes\n\n${indent(`${REGION}\n\n${FORGED}`, "\t")}`],
    ["marker text mid-line outside code", `See ${REGION_START} and ${REGION_END} here.`],
    ["a marker with text after it on its line", `${REGION_START} x\n${SIZE}\n${REGION_END}\n\n${FORGED}`],
    ["one misplaced marker beside real blocks", `${REGION}\n\n${FORGED}\n\nquote: ${COMPLETION_END}`],
  ] as Array<[string, string]>) {
    it(`${name} → malformed (misplaced), never adopted`, () => {
      assert.deepEqual(parseOwnedBlocks(body), MISPLACED, body);
    });
  }

  it("a lone CR, CRLF or LF ends a marker's line", () => {
    const lf = `Notes\n\n${REGION}\n\n${FORGED}\n\nafter`;
    for (const eol of ["\n", "\r\n", "\r"]) {
      const body = lf.replace(/\n/gu, eol);
      const p = parseOwnedBlocks(body);
      assert.equal(p.kind, "ok", JSON.stringify(eol));
      const b = p as OwnedBlocks;
      assert.equal(b.region, REGION.replace(/\n/gu, eol));
      assert.equal(b.completion, FORGED.replace(/\n/gu, eol));
      assert.equal(b.before + b.region + b.between + b.completion + b.after, body);
    }
  });
});

// ── Round trip: every body the renderer writes parses ok, markers at column 0 ──

const STAGE_WIRE = {
  id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
  run_id: "11111111-2222-3333-4444-555555555555",
  claim_generation: 1,
  mr_iid: null,
  size: null,
  base_sha: "a".repeat(40),
  head_sha: "e".repeat(40),
  target_branch: "main",
  source: "generated",
  rendered_region_sha256: null,
  state: "pending",
  created_at: "2026-09-27T10:00:00Z",
  published_at: null,
};

/** A REAL SanitizedPrDescriptionFields instance, minted the way production gets one: a stage call
 *  whose (stubbed) api response carries the fields. */
async function mint(fields: Partial<RawPrDescriptionFields>): Promise<SanitizedPrDescriptionFields> {
  const empty = { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] };
  const wire = { version: { ...STAGE_WIRE, fields: { ...empty, ...fields } } };
  const orig = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify(wire), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;
  try {
    const client = new WorkerClient("http://127.0.0.1:9", "token", "0.1.0-test", nullLogger(), { sleep: async () => {} });
    const { version } = await client.stagePrDescription(STAGE_WIRE.run_id, {
      claim_generation: 1,
      source: "generated",
      fields: empty,
      size: null,
      base_sha: STAGE_WIRE.base_sha,
      head_sha: STAGE_WIRE.head_sha,
      target_branch: "main",
    });
    return version.fields;
  } finally {
    globalThis.fetch = orig;
  }
}

describe("round trip: the renderer's own output always parses ok", () => {
  const HEAD = "e".repeat(40);
  /** Every marker-shaped comment in `body` (outside nothing: the renderer writes no marker in code)
   *  starts a line and ends it. */
  const markersAtColumn0 = (body: string) => {
    for (const m of body.matchAll(/<!--[\s\p{Z}]*uzi:(?:description|completion):[^>]*>/giu)) {
      const at = m.index;
      assert.ok(at === 0 || body[at - 1] === "\n", `${JSON.stringify(m[0])} at ${at}:\n${body}`);
      const end = at + m[0].length;
      assert.ok(end === body.length || body[end] === "\n", `${JSON.stringify(m[0])} at ${at}:\n${body}`);
    }
  };
  const hostile = "a`b\n<!-- uzi:completion:end -->\n    <!-- uzi:description:start v1 -->";
  const section: KindSection = [{ fixed: "" }, { fixed: "---" }, { fixed: hostile }, { path: hostile }];
  const kinds: Array<[RunKind, Partial<ClaimResponse>]> = [
    ["issue", { issue_iid: 7 }],
    ["ci_fix", { pipeline: { id: 5, ref: hostile, sha: "a".repeat(40), web_url: hostile, failed_jobs: [] } }],
    ["ci_fix", { pipeline: undefined, issue_iid: 7 }],
    ["chat", { issue_iid: null }],
    ["judge", { issue_iid: 7 }],
    ["self_improve", { issue_iid: 77 }],
    ["prompt", { issue_iid: null }],
    ["task", { issue_iid: null, branch: "uzi/task/x", base_branch: hostile }],
    ["mr_rework", { issue_iid: null, branch: "agent/issue-42" }],
  ];
  assert.deepEqual([...new Set(kinds.map(([k]) => k))].sort(), [...RUN_KINDS].sort());
  const scopes: Array<ClaimConfig["completion_scope"]> = [
    undefined,
    {
      deferred: [{ milestone_id: hostile, title: hostile, reason: hostile }],
      accepted: [{ id: hostile, text: hostile, reason: hostile }],
    } as ClaimConfig["completion_scope"],
  ];

  it("every kind, with and without a region, every optional part, LF / CRLF / CR", async () => {
    const fields = await mint({
      summary: "Summary.",
      changes: ["One.", "Two."],
      scope_notes: [{ kind: "deferred", text: "Later." }],
      review_pointers: ["Look here."],
      verification: [{ command: "task gate:agent", result: "pass", verified_at_sha: "1234567" }],
    });
    const regions = [
      undefined,
      renderRegion({ sizeLine: SIZE }).text,
      renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: hostile }).text,
      renderRegion({ sizeLine: SIZE, headSha: HEAD, targetBranch: "main", source: "lead_only" }, fields).text,
      renderRegion({ headSha: HEAD, targetBranch: "main" }).text,
    ];
    assert.ok(regions[3]!.includes("### What changed"), regions[3]);
    for (const [kind, over] of kinds) {
      for (const completionScope of scopes) {
        for (const region of regions) {
          const claim = makeClaim({ kind, issue_title: "T", ...over } as Partial<ClaimResponse>);
          const completion = renderCompletionBlock({
            issueIid: claim.issue_iid,
            branch: hostile,
            // The banner and the kind line are the callers' fixed wording (not escaped by contract);
            // every run-derived value goes through a slot the renderer escapes.
            kindLine: kind === "issue" ? undefined : `Kind ${kind}.`,
            kindSections: [section, selfImproveMrSection([hostile], [{ name: hostile, status: "failed", detail: hostile }])],
            closes: true,
            completionScope,
            scopeCapped: { completedCount: 1, total: 2 },
            repoAgents: true,
            gatesUnverified: [hostile],
            gatesDiscoveryTruncated: true,
            bridged: true,
            banner: "> ⚠️ **Completion unverified.** x",
            staleness: { describedSha: "1".repeat(40), headSha: HEAD },
          });
          const bodies = [
            renderBody(region, completion),
            mrDescription(
              claim,
              hostile,
              { source: "repo", agents: ["a"] },
              section,
              section,
              [hostile],
              true,
              undefined,
              true,
              completionScope,
              true,
              SIZE,
              {
                headSha: HEAD,
                targetBranch: hostile,
                banner: "> ⚠️ **Completion unverified.** x",
                staleness: { describedSha: "1".repeat(40), headSha: HEAD },
              },
            ),
          ];
          for (const body of bodies) {
            markersAtColumn0(body);
            for (const eol of ["\n", "\r\n", "\r"]) {
              const b = body.replace(/\n/gu, eol);
              const p = parseOwnedBlocks(b);
              assert.equal(p.kind, "ok", `${kind} ${JSON.stringify(eol)}:\n${body}`);
              const o = p as OwnedBlocks;
              assert.equal(o.before, "");
              assert.equal(o.after, "");
              assert.ok(o.completion!.startsWith(COMPLETION_START) && o.completion!.endsWith(COMPLETION_END));
            }
          }
          if (region) {
            const o = parseOwnedBlocks(renderBody(region, completion)) as OwnedBlocks;
            assert.equal(o.region, region);
            assert.equal(o.completion, completion);
            // A body a human framed: the blocks are found and every other byte is kept.
            const framed = `Human intro.\n\n${region}\n\nbot note\n\n${completion}\n\nTrailer`;
            const f = parseOwnedBlocks(framed) as OwnedBlocks;
            assert.equal(composeBody(f, { region, completion }), framed);
          }
        }
      }
    }
  });
});

// ── NB-2: the ci_fix pipeline URL guard ──

describe("ci_fix pipeline URL (NB-2)", () => {
  const bodyFor = (url: string) =>
    mrDescription(
      makeClaim({
        kind: "ci_fix",
        issue_title: "T",
        pipeline: { id: 5, ref: "main", sha: "a".repeat(40), web_url: url, failed_jobs: [] },
      } as Partial<ClaimResponse>),
      "ci-fix/pipeline-5",
    );

  it("a plain http(s) URL stays a bare autolink", () => {
    assert.match(bodyFor("https://gitlab.example/g/p/-/pipelines/5"), /on `main`: https:\/\/gitlab\.example\/g\/p\/-\/pipelines\/5$/mu);
  });

  it("anything else renders as an inert code span", () => {
    for (const [url, span] of [
      ["javascript:alert(1)", "`javascript:alert(1)`"],
      ["ftp://x/y", "`ftp://x/y`"],
      ["https://e/x)[a](javascript:y)", "`https://e/x)[a](javascript:y)`"],
      ["https://e/<img src=x>", "`https://e/<img src=x>`"],
      ["https://e/p\n/merge", "`https://e/p /merge`"],
      ["https://e/@org", `\`https://e/@${ZW}org\``],
      ["https://e/Fixes#7", `\`https://e/F${ZW}ixes#7\``],
    ]) {
      const body = bodyFor(url!);
      assert.ok(body.includes(`: ${span}\n`), `${url}:\n${body}`);
      assert.deepEqual(quickActions(body), []);
      assert.equal(rawHtml(body), null, body);
      assert.equal(closingDirectiveFor(body, 7), false, body);
      assert.doesNotMatch(outsideCode(body), /\]\(javascript:/u);
    }
  });
});

// ── Nits: codeSpan ──

describe("codeSpan nits", () => {
  it("neutralises `<!--`, so a marker-shaped value is never read as a marker", () => {
    assert.equal(codeSpan(COMPLETION_END), `\`<${ZW}!-- uzi:completion:end -->\``);
    const block = renderCompletionBlock({ branch: COMPLETION_END, closes: false, issueIid: 7 });
    assert.equal(parseOwnedBlocks(block).kind, "ok");
  });

  it("an empty value renders as (empty), never as a bare pair of backticks", () => {
    for (const s of ["", "  ", "\n\t", "\u200E"]) assert.equal(codeSpan(s), "(empty)", JSON.stringify(s));
  });
});
