import { describe, it, expect } from "vitest";
import { buildIndex, searchIndex, stripDocBody, MIN_QUERY_LENGTH, type IndexedDoc } from "./docsearch";
import type { Doc } from "./docs";

// The snippet window is ~160 chars plus at most two `…` bracket characters.
const SNIPPET_MAX = 162;

// Synthetic corpus so these tests are decoupled from the real docs/ content
// (pages can be rewritten without touching them). `summarize` is not re-run
// here — the summary is supplied directly, mirroring what docs.ts stores.
function doc(slug: string, title: string, body: string, summary = ""): Doc {
  return { slug, meta: { title, order: null, audience: "user" }, body, summary };
}

function index(...docs: Doc[]): IndexedDoc[] {
  return buildIndex(docs);
}

function slugs(index: IndexedDoc[], query: string): string[] {
  return searchIndex(index, query).map((r) => r.doc.slug);
}

describe("stripDocBody", () => {
  it("keeps code-fence content as text but drops the fence delimiters and language tag", () => {
    const { plainBody } = stripDocBody("# Run\n\n```sh\ndocker compose --profile agent up\n```\n");
    expect(plainBody).toContain("docker compose --profile agent up");
    expect(plainBody).not.toContain("```");
    expect(plainBody).not.toContain("sh\n");
  });

  it("keeps table cell text and drops pipes and the alignment row", () => {
    const { plainBody } = stripDocBody("| Name | Role |\n| --- | --- |\n| lead | plans |\n");
    expect(plainBody).toBe("Name Role lead plans");
  });

  it("reduces links/images to their text and drops emphasis/backtick markers", () => {
    const { plainBody } = stripDocBody("See the **[join token](./x.md)** and `UZI_WORKER_TOKEN`.\n");
    expect(plainBody).toBe("See the join token and UZI_WORKER_TOKEN.");
  });

  it("drops word-boundary emphasis underscores but keeps intra-word ones (identifiers)", () => {
    const { plainBody } = stripDocBody("an _emphasized_ word next to snake_case_id\n");
    expect(plainBody).toBe("an emphasized word next to snake_case_id");
  });

  it("collects heading text (markers dropped) and retains it in the body", () => {
    const { plainBody, headings } = stripDocBody("# Worker setup\n\ntext\n\n## Generate a join token\n");
    expect(headings).toEqual(["Worker setup", "Generate a join token"]);
    expect(plainBody).toContain("Worker setup");
    expect(plainBody).toContain("Generate a join token");
  });

  it("does not treat a `#` inside a code fence as a heading", () => {
    const { headings, plainBody } = stripDocBody("# Real\n\n```sh\n# just a shell comment\necho hi\n```\n");
    expect(headings).toEqual(["Real"]);
    expect(plainBody).toContain("# just a shell comment");
  });
});

describe("searchIndex — short-query guard", () => {
  const idx = index(doc("a", "Alpha", "some body text"));
  it("returns nothing for a query shorter than MIN_QUERY_LENGTH", () => {
    expect(MIN_QUERY_LENGTH).toBe(2);
    expect(searchIndex(idx, "a")).toEqual([]);
    expect(searchIndex(idx, " ")).toEqual([]);
    expect(searchIndex(idx, "")).toEqual([]);
  });
});

describe("searchIndex — matching is plain substring, not regex", () => {
  it("matches regex-metacharacter tokens literally (.env, --profile)", () => {
    const idx = index(
      doc("cfg", "Config", "copy .env.example to .env and set the vars"),
      doc("run", "Run", "docker compose --profile agent up starts a worker"),
    );
    expect(slugs(idx, ".env")).toEqual(["cfg"]);
    expect(slugs(idx, "--profile")).toEqual(["run"]);
  });

  it("does not let a `.` token match every character (would if it were a regex)", () => {
    const idx = index(doc("nodot", "No dot", "a body with no full stop"));
    expect(searchIndex(idx, ".")).toEqual([]); // 1-char, guarded anyway
    expect(slugs(idx, "..")).toEqual([]); // ".." is not a substring here
  });
});

describe("searchIndex — multi-token AND", () => {
  const idx = index(
    doc("both", "Both", "the worker claims a queued run"),
    doc("one", "One", "the worker sits idle"),
  );
  it("requires every token to appear somewhere in the doc", () => {
    expect(slugs(idx, "worker queued")).toEqual(["both"]);
    expect(slugs(idx, "worker missingword")).toEqual([]);
  });
});

describe("searchIndex — ranking tiers", () => {
  it("orders title match over heading over body, then by occurrences, then slug", () => {
    const idx = index(
      doc("title-hit", "The token page", "unrelated prose"),
      doc("heading-hit", "Setup", "## The token section\n\nunrelated prose"),
      doc("body-hit", "Setup", "you paste the token once and never again"),
      doc("body-hit-more", "Setup", "the token, the token, the token appears thrice"),
    );
    // title(3) > heading(2) > body(1); within body, more occurrences first.
    expect(slugs(idx, "token")).toEqual(["title-hit", "heading-hit", "body-hit-more", "body-hit"]);
  });

  it("uses slug as a stable tiebreak within a tier", () => {
    const idx = index(
      doc("zeta", "Zeta", "the term appears once"),
      doc("alpha", "Alpha", "the term appears once"),
    );
    expect(slugs(idx, "term")).toEqual(["alpha", "zeta"]);
  });
});

describe("searchIndex — snippet windowing", () => {
  it("centers a bounded window on the first body match with … on cut sides", () => {
    const body = `${"lorem ipsum ".repeat(20)}the NEEDLE is here ${"dolor sit ".repeat(20)}`;
    const [result] = searchIndex(index(doc("d", "Doc", body)), "needle");
    expect(result.snippet.length).toBeLessThanOrEqual(SNIPPET_MAX);
    expect(result.snippet.startsWith("…")).toBe(true);
    expect(result.snippet.endsWith("…")).toBe(true);
    expect(result.snippet.toLowerCase()).toContain("needle");
  });

  it("marks the matched token within the snippet (snippet-relative ranges)", () => {
    const [result] = searchIndex(index(doc("d", "Doc", "an alpha beta gamma body")), "beta");
    const marked = result.ranges.map(([s, e]) => result.snippet.slice(s, e).toLowerCase());
    expect(marked).toEqual(["beta"]);
  });
});

describe("searchIndex — overlapping-token range merging", () => {
  it("collapses overlapping tokens (work + worker) into one non-overlapping range", () => {
    const [result] = searchIndex(index(doc("d", "Doc", "the worker toils quietly")), "work worker");
    // Ranges must be sorted and non-overlapping.
    for (let i = 1; i < result.ranges.length; i++) {
      expect(result.ranges[i][0]).toBeGreaterThanOrEqual(result.ranges[i - 1][1]);
    }
    // "work" is subsumed by the "worker" occurrence — one range covers "worker".
    const texts = result.ranges.map(([s, e]) => result.snippet.slice(s, e).toLowerCase());
    expect(texts).toEqual(["worker"]);
  });
});

describe("searchIndex — mixed title-token + body-token snippet", () => {
  it("windows on the body token and does not mark the title-only token", () => {
    const idx = index(doc("autopilot", "Autopilot", "queued runs are claimed by the worker process"));
    const [result] = searchIndex(idx, "autopilot worker");
    // "autopilot" matches only the title, "worker" matches the body.
    expect(result.doc.slug).toBe("autopilot");
    expect(result.snippet.toLowerCase()).toContain("worker");
    const marked = result.ranges.map(([s, e]) => result.snippet.slice(s, e).toLowerCase());
    expect(marked).toEqual(["worker"]); // the title-only token has no body range
  });

  it("falls back to the doc summary for a title-only match (no body hit)", () => {
    const idx = index(doc("board", "Board", "no matching term in this body", "The kanban board summary"));
    const [result] = searchIndex(idx, "board");
    expect(result.snippet).toBe("The kanban board summary");
    // "board" is highlighted where it appears in the summary.
    expect(result.ranges.map(([s, e]) => result.snippet.slice(s, e).toLowerCase())).toEqual(["board"]);
  });
});
