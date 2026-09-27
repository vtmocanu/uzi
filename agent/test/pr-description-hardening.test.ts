import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  COMPLETION_END,
  COMPLETION_START,
  FORGE_BODY_MAX_CHARS,
  REGION_END,
  REGION_START,
  closingDirectiveFor,
  codeSpan,
  parseOwnedBlocks,
  renderCompletionBlock,
  type KindSection,
  type OwnedBlocks,
} from "../src/pr-description.js";
import type { ClaimResponse } from "../src/protocol.js";
import { mrDescription } from "../src/runner.js";
import { guardCriticalMrSection, selfImproveMrSection } from "../src/self-improve.js";
import { makeClaim } from "./helpers.js";

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
  const N = 256 * 1024;
  const fill = (unit: string) => unit.repeat(Math.ceil(N / unit.length)).slice(0, N);
  const time = (f: () => unknown) => {
    const t = performance.now();
    f();
    return performance.now() - t;
  };

  it("closingDirectiveFor: 256K characters of each shape in under 500 ms", () => {
    const shapes = [
      fill("fixes:https://"),
      fill("fix-"),
      fill("fix."),
      `${fill("fix-")}*`,
      `${fill("fix.")}_`,
      `fixes${" ".repeat(N)}`,
      `fixes:${"\u2003".repeat(N)}x`,
      fill("fixes a/"),
      fill("fixes #1 "),
      fill("fixes #1, "),
      fill("fixes https://a/"),
      fill("!["),
      fill("["),
      fill("[a]("),
      fill("[a]["),
      fill("<!--"),
      fill("<!"),
      fill("-"),
      fill("--!"),
      fill('<a b="'),
      fill("<a b='x' c=\"y\" d=z "),
      fill("<details "),
      fill("</b "),
      fill("&amp;"),
      fill("&#"),
      fill("&"),
      fill("`"),
      fill("*_"),
      fill("<https://"),
    ];
    for (const s of shapes) {
      const ms = time(() => closingDirectiveFor(s, 7, "o/r"));
      assert.ok(ms < 500, `${JSON.stringify(s.slice(0, 24))}…: ${ms.toFixed(0)} ms`);
    }
  });

  it("parseOwnedBlocks: 256K characters of each shape in under 500 ms", () => {
    const shapes = [
      fill("<!-- uzi:description:start v1 "),
      fill("<!-- uzi:completion:end "),
      fill("<!--"),
      fill("<!-- uzi:description:start"),
      fill("`"),
      fill("``x"),
      fill("\\`"),
      fill("```\n"),
      fill("```a\n"),
      fill("> ```\n"),
      fill("~~~\nx\n"),
      fill("\n"),
      `${fill("`a")}\n${REGION_START}\n${REGION_END}`,
    ];
    for (const s of shapes) {
      const ms = time(() => parseOwnedBlocks(s));
      assert.ok(ms < 500, `${JSON.stringify(s.slice(0, 24))}…: ${ms.toFixed(0)} ms`);
    }
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

  for (const [name, copy] of [
    ["a ``` fence", "```\n" + REGION + "\n\n" + COMPLETION + "\n```"],
    ["a ~~~~ fence", "~~~~md\n" + REGION + "\n" + COMPLETION + "\n~~~~"],
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
    const body = "````\n```\n" + REGION_START + "\n~~~~\n" + REGION_END + "\n````\n" + REGION + "\n\n" + COMPLETION;
    const p = parseOwnedBlocks(body);
    assert.equal(p.kind, "ok");
    assert.equal((p as OwnedBlocks).region, REGION);
  });

  it("markers outside code are still counted (a duplicate is still malformed)", () => {
    assert.deepEqual(parseOwnedBlocks(`${REGION}\n\n${REGION}\n\n${COMPLETION}`), { kind: "malformed", reason: "duplicate" });
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
