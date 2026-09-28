// PRD #1798 D3/M7: the agent half of the cross-language size-line golden
// (fixtures/pr-size-line/cases.json). The web (DeliveredCard.prSizeLine) and the CLI
// (run_render.go prSizeBody) render the line from the STORED size; the agent renders it from the
// diff's numstat (renderSizeLine). This drives the agent's renderer from each case's synthetic
// `numstat` entries and pins its output, bold stripped, to the same expected line, so the three
// renderers cannot drift apart silently.
//
// Where the two are defined to disagree the case carries `agent_expected`: the agent prints any
// bucket a file landed in, so a binary-only bucket reads `+0 −0`, while the stored size has only
// line counts and the read surfaces omit a `+0 −0` bucket. That divergence is the agent-side
// expectation, documented in fixtures/pr-size-line/README.md.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import {
  classifyPath,
  computeSize,
  renderSizeLine,
  sizeTotals,
  SIZE_UNAVAILABLE,
  type NumstatEntry,
  type PathAttributes,
  type SizeBucket,
} from "../src/pr-size.js";

type Bucket = { added: number; deleted: number };
type FixtureEntry = NumstatEntry & { attrs?: PathAttributes };
interface FixtureCase {
  name: string;
  size: { unavailable: boolean; files: number } & Record<SizeBucket, Bucket>;
  numstat?: FixtureEntry[];
  expected: string | null;
  agent_expected?: string;
}

const FIXTURE = fileURLToPath(new URL("../../fixtures/pr-size-line/cases.json", import.meta.url));
const cases = JSON.parse(fs.readFileSync(FIXTURE, "utf8")) as FixtureCase[];
const BUCKETS: readonly SizeBucket[] = ["code", "tests", "docs", "config", "generated", "vendored"];

function unbold(line: string | null): string | null {
  return line === null ? null : line.replaceAll("**", "");
}

function attrsOf(entries: readonly FixtureEntry[]): Map<string, PathAttributes> {
  return new Map(entries.map((e) => [e.path, e.attrs ?? {}]));
}

describe("pr-size fixture parity (agent renderSizeLine vs the stored-size read surfaces)", () => {
  const driven = cases.filter((c) => c.numstat !== undefined);

  it("drives at least the agreeing, the binary-divergent and the empty cases", () => {
    assert.ok(driven.some((c) => c.agent_expected === undefined && c.expected !== null));
    assert.ok(driven.some((c) => c.agent_expected !== undefined));
    assert.ok(driven.some((c) => c.expected === null));
  });

  for (const c of driven) {
    const entries = c.numstat!;
    it(`${c.name}: the numstat and the stored size describe the same diff`, () => {
      const attrs = attrsOf(entries);
      assert.equal(c.size.unavailable, false);
      assert.equal(c.size.files, entries.length, "files");
      const sums = new Map<SizeBucket, Bucket>(BUCKETS.map((b) => [b, { added: 0, deleted: 0 }]));
      for (const e of entries) {
        const s = sums.get(classifyPath(e.path, attrs.get(e.path)))!;
        s.added += e.added;
        s.deleted += e.deleted;
      }
      for (const b of BUCKETS) assert.deepEqual(c.size[b], sums.get(b), b);
    });

    it(`${c.name}: the agent's structured size (sizeTotals) is the stored size exactly`, () => {
      assert.deepEqual(sizeTotals(entries, attrsOf(entries)), c.size);
    });

    it(`${c.name}: agent line${c.agent_expected !== undefined ? " (documented divergence)" : ""}`, () => {
      const got = unbold(renderSizeLine(entries, attrsOf(entries)));
      assert.equal(got, c.agent_expected ?? c.expected);
    });
  }

  it("the binary divergence is exactly the agent's extra `+0 −0` bucket(s)", () => {
    const parts = (line: string) => line.replace(/^Size: /, "").split(" · ");
    for (const c of driven.filter((d) => d.agent_expected !== undefined)) {
      const agent = parts(c.agent_expected!);
      const read = parts(c.expected!);
      // Every part the read surfaces print, the agent prints too, in the same order...
      assert.deepEqual(agent.filter((p) => read.includes(p)), read, c.name);
      // ...and what only the agent prints is a zero-line bucket, nothing else.
      const onlyAgent = agent.filter((p) => !read.includes(p));
      assert.ok(onlyAgent.length > 0, c.name);
      for (const p of onlyAgent) assert.match(p, /^[a-z]+ \+0 −0$/, `${c.name}: ${p}`);
    }
  });

  it("the agent's unavailable size is the stored unavailable size exactly", async () => {
    const c = cases.find((d) => d.size.unavailable)!;
    const failing = {
      sizeMergeBase: async () => {
        throw new Error("no merge base");
      },
      diffNumstatZ: async () => "",
      checkAttrZ: async () => new Map<string, PathAttributes>(),
    };
    const got = await computeSize(failing, "/bare", "main", "c".repeat(40));
    assert.equal(unbold(got.line), c.expected);
    assert.deepEqual(got.size, c.size);
  });

  it("the unavailable line agrees", () => {
    const c = cases.find((d) => d.size.unavailable);
    assert.ok(c, "fixture has an unavailable case");
    assert.equal(unbold(SIZE_UNAVAILABLE), c.expected);
  });
});
