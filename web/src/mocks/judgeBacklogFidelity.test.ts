import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import {
  bucketOf,
  filterGroups,
  groupJudgeRecommendations,
  type JudgeBacklogRow,
} from "./mockApi";
import type { JudgeBacklogBucket, JudgeRecommendationGroup } from "../lib/api";

// The mock/server fidelity golden fixture (PRD #98 seam 6). This file is the VITEST HALF;
// the Go half is api/internal/workersvc/judge_backlog_fidelity_test.go. Neither reads the
// other, and neither generates the fixture: each compares its OWN output against the same
// third artifact, so a failure names the side that drifted. A direct Go-vs-JS diff could
// only ever report that they disagree, and it would make `npm test` need a Go toolchain.
//
// toMatchSnapshot() is deliberately NOT used anywhere below. Vitest WRITES a missing
// snapshot on first run and passes, which is exactly the "golden file rots into a snapshot"
// mechanism this fixture exists to prevent, so it is disqualified by construction rather
// than by discipline.
//
// This file is ASCII-ONLY on purpose. Every exotic character is built by code point rather
// than pasted: a pasted glyph corrupts silently in transit, and the corruption reads as a
// passing test. One occurred while this fixture was authored, turning a literal U+0020 into
// U+00A0 inside a probe.
const ELL = String.fromCodePoint(0x2026);
const NBSP = String.fromCodePoint(0x00a0);
const BOM = String.fromCodePoint(0xfeff);
const LSEP = String.fromCodePoint(0x2028);
const SP = String.fromCodePoint(0x0020);

const PREVIEW_MAX = 280;

type FidCase = {
  name: string;
  proves: string;
  do_not_tidy?: string;
  bucket: string;
  rows: JudgeBacklogRow[];
};

// read resolves against THIS FILE, not the process cwd, so the suite does not depend on
// where vitest was invoked from. A missing fixture throws; it must never be skipped over,
// because a skipped fidelity check is indistinguishable from a passing one.
function read(name: string): string {
  const url = new URL(`../../../fixtures/judge-fidelity/${name}`, import.meta.url);
  try {
    return readFileSync(url, "utf8");
  } catch (err) {
    throw new Error(
      `fixture unreadable: ${name}: ${String(err)} -- seam 6 asserts nothing without it, ` +
        `and skipping would look identical to passing`,
    );
  }
}

const cases: FidCase[] = (JSON.parse(read("cases.json")) as { cases: FidCase[] }).cases;
const expected: Record<string, unknown[]> = JSON.parse(read("expected.json")) as Record<string, unknown[]>;

function fatal(msg: string): never {
  throw new Error(msg);
}

function caseNamed(name: string): FidCase {
  const c = cases.find((x) => x.name === name);
  if (!c) {
    fatal(
      `fixture broken: cases.json has no case "${name}" -- the self-check below is what makes ` +
        `that case load-bearing, so removing the case must redden here rather than quietly ` +
        `shrink the fixture`,
    );
  }
  return c;
}

function goldenFor(name: string): JudgeRecommendationGroup[] {
  const g = expected[name];
  if (!g) fatal(`fixture broken: expected.json has no golden output for case "${name}"`);
  return g as JudgeRecommendationGroup[];
}

// rung classifies ONE fixture row on the #94 ladder. It deliberately does NOT call
// bucketOf: a self-check that ran the implementation could be talked into agreeing by a
// mutated implementation, and immunity to that is the whole reason this layer exists. The
// honest cost is that this classifier cannot notice a LADDER change, only a fixture change;
// catching a ladder change is the golden comparison's job.
function rung(r: JudgeBacklogRow): string {
  if (r.disposition_status === "dismissed") return "dismissed";
  if (r.disposition_status === "done") return "done";
  if (r.filed_settled) return "filed";
  return "todo";
}

const ck = (category: string, target: string) => JSON.stringify([category, target]);
const rowKey = (r: JudgeBacklogRow) => ck(r.category, r.target);
const runes = (s: string) => Array.from(s).length;
const units = (s: string) => s.length;

// firstSeen is each coordinate's first row index: the pre-sort group order a stable sort
// must preserve within a tie.
function firstSeen(rows: JudgeBacklogRow[]): Map<string, number> {
  const out = new Map<string, number>();
  rows.forEach((r, i) => {
    if (!out.has(rowKey(r))) out.set(rowKey(r), i);
  });
  return out;
}

// wire is the marshalled form. Comparing parsed JSON rather than objects means an added or
// dropped key is a failure, which is the point: the keys ARE the contract the mock mirrors.
const wire = (v: unknown) => JSON.parse(JSON.stringify(v)) as unknown;

describe("judge backlog fidelity: the mock grouper against the golden", () => {
  it("has at least one case, with a golden for each and no orphans", () => {
    if (cases.length === 0) {
      fatal("fixture broken: cases.json defines no cases -- every assertion below would pass over an empty range");
    }
    const names = new Set<string>();
    for (const c of cases) {
      if (names.has(c.name)) {
        fatal(
          `fixture broken: cases.json defines "${c.name}" twice -- the second would silently ` +
            `win every lookup in the self-check`,
        );
      }
      names.add(c.name);
      if (!(c.name in expected)) {
        fatal(
          `fixture broken: cases.json defines case "${c.name}" with no golden in expected.json ` +
            `-- an ungolden case runs the grouper and asserts nothing`,
        );
      }
    }
    for (const name of Object.keys(expected)) {
      if (!names.has(name)) {
        fatal(
          `fixture broken: expected.json carries golden output for "${name}" but cases.json no ` +
            `longer defines it -- an orphaned golden is never compared against anything and reads as coverage`,
        );
      }
    }
  });

  for (const c of cases) {
    it(`matches expected.json for ${c.name}`, () => {
      const got = wire(filterGroups(groupJudgeRecommendations(c.rows), c.bucket as JudgeBacklogBucket));
      // If the Go half (api/internal/workersvc/judge_backlog_fidelity_test.go) is GREEN and
      // this is RED, the MOCK drifted. That asymmetry is the whole reason the golden is a
      // third artifact owned by neither runtime.
      expect(got, `mock grouper disagrees with fixtures/judge-fidelity/expected.json for ${c.name}`).toEqual(
        expected[c.name],
      );
    });
  }

  it("bucketOf mirrors the server ladder on every rung", () => {
    // Not a fixture assertion: the four rungs as a table, so the extracted ladder is pinned
    // independently of whether any case happens to exercise a given rung.
    expect(bucketOf("dismissed", false)).toBe("dismissed");
    expect(bucketOf("dismissed", true)).toBe("dismissed");
    expect(bucketOf("done", false)).toBe("done");
    expect(bucketOf("done", true)).toBe("done");
    expect(bucketOf(null, true)).toBe("filed");
    expect(bucketOf(null, false)).toBe("todo");
  });
});

describe("judge backlog fidelity: the cases discriminate (input side)", () => {
  it("dedup-across-runs has a coordinate in two different runs", () => {
    const c = caseNamed("dedup-across-runs");
    const runsPerCoord = new Map<string, Set<string>>();
    for (const r of c.rows) {
      if (!runsPerCoord.has(rowKey(r))) runsPerCoord.set(rowKey(r), new Set());
      runsPerCoord.get(rowKey(r))!.add(r.run_id);
    }
    const ok = [...runsPerCoord.values()].some((s) => s.size >= 2);
    if (!ok) {
      fatal(
        "fixture broken: no coordinate in this case occurs in two different runs -- otherwise it " +
          "proves nothing about deduping ACROSS runs, and run_count 1 would satisfy it",
      );
    }
  });

  it("occurrences-exceed-run-count repeats a coordinate inside ONE run", () => {
    const c = caseNamed("occurrences-exceed-run-count");
    const n = new Map<string, number>();
    for (const r of c.rows) {
      const k = `${rowKey(r)}|${r.run_id}`;
      n.set(k, (n.get(k) ?? 0) + 1);
    }
    if (![...n.values()].some((v) => v >= 2)) {
      fatal(
        "fixture broken: no (category, target, run_id) triple appears twice -- otherwise " +
          "occurrences can never outnumber run_count and the SQLSTATE 21000 shape is not in the fixture at all",
      );
    }
  });

  it("partial-settle has a coordinate that is both open and settled", () => {
    const c = caseNamed("partial-settle");
    const open = new Set<string>();
    const settled = new Set<string>();
    for (const r of c.rows) (rung(r) === "todo" ? open : settled).add(rowKey(r));
    if (![...open].some((k) => settled.has(k))) {
      fatal(
        "fixture broken: no coordinate has BOTH an open member and a settled one -- otherwise " +
          "the open_count short-circuit in the rollup is never exercised",
      );
    }
  });

  it("rollup-precedence-pairs covers all six pairs, with no stray todo on the settled ones", () => {
    const c = caseNamed("rollup-precedence-pairs");
    const rungs = new Map<string, Set<string>>();
    for (const r of c.rows) {
      if (!rungs.has(rowKey(r))) rungs.set(rowKey(r), new Set());
      rungs.get(rowKey(r))!.add(rung(r));
    }
    const pairs: [string, string][] = [
      ["dismissed", "done"],
      ["dismissed", "filed"],
      ["dismissed", "todo"],
      ["done", "filed"],
      ["done", "todo"],
      ["filed", "todo"],
    ];
    for (const [a, b] of pairs) {
      const found = [...rungs.values()].some((have) => {
        if (!have.has(a) || !have.has(b)) return false;
        // For a pair that does not involve todo, a stray todo member would push open_count
        // above zero, the rollup would short-circuit, and topRung would never be consulted.
        // The coordinate would look like coverage and be none.
        if (a !== "todo" && b !== "todo" && have.has("todo")) return false;
        return true;
      });
      if (!found) {
        fatal(
          `fixture broken: no coordinate carries members on BOTH the ${a} and ${b} rungs with no ` +
            `stray todo member -- otherwise the ${a}/${b} half of the precedence ladder is never chosen between`,
        );
      }
    }
  });

  it("sort-tie-first-seen-order really contains a tie, and something to order", () => {
    const c = caseNamed("sort-tie-first-seen-order");
    const agg = new Map<string, { runs: Set<string>; open: number }>();
    for (const r of c.rows) {
      if (!agg.has(rowKey(r))) agg.set(rowKey(r), { runs: new Set(), open: 0 });
      const a = agg.get(rowKey(r))!;
      a.runs.add(r.run_id);
      if (rung(r) === "todo") a.open += 1;
    }
    const tally = new Map<string, number>();
    for (const a of agg.values()) {
      const k = `${a.runs.size}/${a.open}`;
      tally.set(k, (tally.get(k) ?? 0) + 1);
    }
    if (![...tally.values()].some((n) => n >= 2)) {
      fatal(
        "fixture broken: no two coordinates tie on (run_count, open_count) -- otherwise the sort " +
          "never has to break a tie and the first-seen ordering guarantee is untested",
      );
    }
    if (tally.size < 2) {
      fatal(
        "fixture broken: every coordinate has the same (run_count, open_count) -- with nothing to " +
          "order, a comparator that always returned 0 would pass",
      );
    }
  });

  it("two cases share a row set, differ in bucket, and expect different group counts", () => {
    const byRows = new Map<string, FidCase[]>();
    for (const c of cases) {
      const k = JSON.stringify(c.rows);
      byRows.set(k, [...(byRows.get(k) ?? []), c]);
    }
    const ok = [...byRows.values()].some((group) => {
      if (group.length < 2) return false;
      const buckets = new Set(group.map((c) => c.bucket));
      const counts = new Set(group.map((c) => goldenFor(c.name).length));
      return buckets.size >= 2 && counts.size >= 2;
    });
    if (!ok) {
      fatal(
        "fixture broken: no two cases share an identical row set while declaring different ?bucket= " +
          "values AND expecting different group counts -- otherwise a filterGroups that ignored the " +
          "bucket entirely would pass every case",
      );
    }
  });

  it("preview-ascii-cut has an over-cap row, on a coordinate that is not the case's first", () => {
    const c = caseNamed("preview-ascii-cut");
    if (!c.rows.some((r) => runes(r.rationale_md) > PREVIEW_MAX)) {
      fatal(
        "fixture broken: no row exceeds the preview cap -- otherwise the truncation branch never " +
          "runs and the case only proves short strings pass through",
      );
    }
    if (![...firstSeen(c.rows).values()].some((i) => i > 0)) {
      fatal(
        "fixture broken: every coordinate's first row is row 0 -- otherwise 'the preview comes from " +
          "the GROUP's first row' is indistinguishable from 'the preview comes from the case's first row'",
      );
    }
    const per = new Map<string, number>();
    for (const r of c.rows) per.set(rowKey(r), (per.get(rowKey(r)) ?? 0) + 1);
    if (![...per.values()].some((n) => n >= 2)) {
      fatal(
        "fixture broken: no coordinate has a second row -- otherwise 'the preview comes from the " +
          "FIRST row' cannot be distinguished from 'from the last row'",
      );
    }
  });

  it("preview-multibyte-cut has a row whose rune and code-unit counts differ, past the cap", () => {
    const c = caseNamed("preview-multibyte-cut");
    const ok = c.rows.some((r) => runes(r.rationale_md) !== units(r.rationale_md) && runes(r.rationale_md) > PREVIEW_MAX);
    if (!ok) {
      fatal(
        "fixture broken: no row has a rune count differing from its UTF-16 code-unit count AND " +
          "exceeding the cap -- otherwise the cut lands where the two counts agree and a code-unit " +
          "implementation is indistinguishable from a rune one",
      );
    }
  });

  it("preview-multibyte-no-cut has a row over the cap in code units and under it in runes", () => {
    const c = caseNamed("preview-multibyte-no-cut");
    const ok = c.rows.some((r) => units(r.rationale_md) > PREVIEW_MAX && runes(r.rationale_md) <= PREVIEW_MAX);
    if (!ok) {
      fatal(
        "fixture broken: no row is over the cap in UTF-16 code units while under it in runes -- this " +
          "is the ONLY shape that separates a rune implementation from a code-unit one, because past " +
          "280 runes both cut and cut identically",
      );
    }
  });

  it("preview-trim-boundary puts each divergent character, and a plain space, at rune 280", () => {
    const c = caseNamed("preview-trim-boundary");
    const want = new Map<string, boolean>([
      [NBSP, false],
      [BOM, false],
      [LSEP, false],
      [SP, false],
    ]);
    for (const r of c.rows) {
      const chars = Array.from(r.rationale_md);
      if (chars.length <= PREVIEW_MAX) {
        fatal(
          `fixture broken: a trim-boundary row is under the cap, so it is never cut and its rune ` +
            `${PREVIEW_MAX} is never at the boundary at all`,
        );
      }
      const at = chars[PREVIEW_MAX - 1];
      if (want.has(at)) want.set(at, true);
    }
    for (const [ch, ok] of want) {
      if (!ok) {
        fatal(
          `fixture broken: no row places U+${ch.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")} ` +
            `at rune ${PREVIEW_MAX} -- that character is in JS's \\s and not in Go's TrimRight cutset, so ` +
            `without it the two trim sets are never asked to disagree (U+0020 is the positive control: it ` +
            `must be trimmed by BOTH, otherwise 'these characters survive' is satisfied by never trimming)`,
        );
      }
    }
  });

  it("sort-stability-13-groups has all four load-bearing clauses", () => {
    const c = caseNamed("sort-stability-13-groups");
    const order: string[] = [];
    const runsPerCoord = new Map<string, Set<string>>();
    for (const r of c.rows) {
      if (!runsPerCoord.has(rowKey(r))) {
        runsPerCoord.set(rowKey(r), new Set());
        order.push(rowKey(r));
      }
      runsPerCoord.get(rowKey(r))!.add(r.run_id);
      if (rung(r) === "todo") {
        fatal(
          "fixture broken: a row in the stability case is open, so open_count can break a tie the " +
            "sort was supposed to leave alone -- every member must be settled",
        );
      }
    }
    if (order.length < 13) {
      fatal(
        `fixture broken: ${order.length} groups, need at least 13 -- Go's pdqsort insertion-sorts ` +
          `below n=12, so an unstable sort produces byte-identical output at any smaller size`,
      );
    }
    const seq = order.map((k) => runsPerCoord.get(k)!.size);
    const distinct = new Map<number, number>();
    for (const v of seq) distinct.set(v, (distinct.get(v) ?? 0) + 1);
    if (distinct.size < 2) {
      fatal(
        "fixture broken: every group has the same run_count -- an all-tied run is already sorted, " +
          "pdqsort short-circuits on it, and the unstable sort goes green at every size",
      );
    }
    for (const [v, n] of distinct) {
      if (n < 2) {
        fatal(
          `fixture broken: run_count ${v} appears on only ${n} group -- with no tie at that value ` +
            `there is no ordering for stability to preserve`,
        );
      }
    }
    if (seq.every((v, i) => i === 0 || v <= seq[i - 1])) {
      fatal(
        "fixture broken: the groups are already in run_count DESC order before the sort runs -- " +
          "pdqsort detects an ordered input and short-circuits, so this case would go green under an " +
          "unstable sort. Interleave them",
      );
    }
  });
});

// The OUTPUT side is what defeats regeneration. Regenerating expected.json from a REGRESSED
// implementation produces a golden that no longer has the properties each case exists to
// demonstrate, and these predicates depend on neither implementation, so no regeneration
// can talk them into agreeing. Honest limit: a regression that happens to preserve every
// declared property below regenerates cleanly. The declared list IS the coverage.
describe("judge backlog fidelity: the golden still describes the discrimination (output side)", () => {
  it("occurrences-exceed-run-count still shows more occurrences than runs", () => {
    const ok = goldenFor("occurrences-exceed-run-count").some((g) => g.occurrences.length > g.run_count);
    if (!ok) {
      fatal(
        "fixture broken: no expected group has more occurrences than run_count -- this case no longer " +
          "describes the shape it is named for, which is what a golden regenerated from a run_count " +
          "that escaped the runsSeen guard would look like",
      );
    }
  });

  it("rollup-precedence-pairs still rolls each pair to the rung it is named for", () => {
    const want: Record<string, string> = {
      "pair-dismissed-done": "dismissed",
      "pair-dismissed-filed": "dismissed",
      "pair-done-filed": "done",
      "pair-dismissed-todo": "todo",
      "pair-done-todo": "todo",
      "pair-filed-todo": "todo",
    };
    const got: Record<string, string> = {};
    for (const g of goldenFor("rollup-precedence-pairs")) got[g.target] = g.bucket;
    for (const [target, rungName] of Object.entries(want)) {
      if (got[target] !== rungName) {
        fatal(
          `fixture broken: the golden rolls ${target} up to "${got[target]}", not "${rungName}" -- a ` +
            `golden that agrees with the ladder about every pair EXCEPT the one it is named for is a ` +
            `regenerated golden, not an authored one`,
        );
      }
    }
  });

  it("sort-tie-first-seen-order keeps first-seen order within each tie", () => {
    assertFirstSeenWithinTies(caseNamed("sort-tie-first-seen-order"));
  });

  it("preview-ascii-cut still shows a cut", () => {
    const ok = goldenFor("preview-ascii-cut").some((g) => {
      const r = Array.from(g.rationale_preview);
      return r.length === PREVIEW_MAX + 1 && r[r.length - 1] === ELL;
    });
    if (!ok) {
      fatal(
        `fixture broken: no expected preview is exactly ${PREVIEW_MAX + 1} runes ending in an ellipsis ` +
          `-- the case no longer shows a cut at all`,
      );
    }
  });

  it("preview-multibyte-cut is still multibyte", () => {
    for (const g of goldenFor("preview-multibyte-cut")) {
      const p = g.rationale_preview;
      if (runes(p) !== PREVIEW_MAX + 1) {
        fatal(`fixture broken: expected preview for ${g.target} is ${runes(p)} runes, want ${PREVIEW_MAX + 1}`);
      }
      if (runes(p) === units(p)) {
        fatal(
          `fixture broken: expected preview for ${g.target} has equal rune and UTF-16 counts, so it is ` +
            `effectively ASCII -- this is what someone 'simplifying' the case produces, and it silently ` +
            `stops separating a rune implementation from a code-unit one`,
        );
      }
    }
  });

  it("preview-multibyte-no-cut still shows the WHOLE string", () => {
    const c = caseNamed("preview-multibyte-no-cut");
    const src = new Map<string, string>();
    for (const r of c.rows) if (!src.has(rowKey(r))) src.set(rowKey(r), r.rationale_md);
    for (const g of goldenFor(c.name)) {
      if (g.rationale_preview !== src.get(ck(g.category, g.target))) {
        fatal(
          `fixture broken: the expected preview for ${g.target} is not byte-identical to its input ` +
            `rationale_md -- the no-cut branch is what this case exists to pin, and a golden that shows ` +
            `a cut here was regenerated from a code-unit implementation`,
        );
      }
      if (units(g.rationale_preview) <= PREVIEW_MAX) {
        fatal(
          `fixture broken: the expected preview for ${g.target} is under the cap in UTF-16 code units ` +
            `too, so a code-unit implementation would also have returned it whole`,
        );
      }
    }
  });

  it("preview-trim-boundary still shows the three characters SURVIVING, and the space trimmed", () => {
    const survivors = new Map<string, boolean>([
      [NBSP, false],
      [BOM, false],
      [LSEP, false],
    ]);
    let trimmed = false;
    for (const g of goldenFor("preview-trim-boundary")) {
      const r = Array.from(g.rationale_preview);
      if (r.length < 2 || r[r.length - 1] !== ELL) {
        fatal(
          `fixture broken: expected preview for ${g.target} does not end in an ellipsis, so it was not ` +
            `cut and the trim never ran`,
        );
      }
      const last = r[r.length - 2];
      if (survivors.has(last)) {
        survivors.set(last, true);
        if (r.length !== PREVIEW_MAX + 1) {
          fatal(
            `fixture broken: expected preview for ${g.target} kept a divergent character but is ` +
              `${r.length} runes, want ${PREVIEW_MAX + 1}`,
          );
        }
      } else if (r.length === PREVIEW_MAX) {
        trimmed = true;
      }
    }
    for (const [ch, ok] of survivors) {
      if (!ok) {
        fatal(
          `fixture broken: no expected preview ends (before the ellipsis) in ` +
            `U+${ch.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")} -- the golden no longer ` +
            `shows the server KEEPING a character JS's \\s strips, which is precisely what a golden ` +
            `regenerated from the \\s implementation looks like`,
        );
      }
    }
    if (!trimmed) {
      fatal(
        `fixture broken: no expected preview is ${PREVIEW_MAX} runes -- the U+0020 control is what ` +
          `proves the trim runs at all, and without it 'these three characters survive' is satisfied ` +
          `by never trimming`,
      );
    }
  });

  it("sort-stability-13-groups keeps first-seen order within each run_count", () => {
    const c = caseNamed("sort-stability-13-groups");
    if (goldenFor(c.name).length < 13) {
      fatal(
        `fixture broken: the golden holds ${goldenFor(c.name).length} groups, need at least 13 for an ` +
          `unstable sort to be observable`,
      );
    }
    assertFirstSeenWithinTies(c);
  });
});

// assertFirstSeenWithinTies checks that, within each (run_count, open_count) value, the
// golden's groups appear in the order their coordinates FIRST appear in the case's rows.
// That is the property Go buys with sort.SliceStable and JS gets from the ES2019 stability
// guarantee, and it is stated against the fixture's own input rather than either
// implementation.
function assertFirstSeenWithinTies(c: FidCase): void {
  const first = firstSeen(c.rows);
  const last = new Map<string, number>();
  let checked = 0;
  for (const g of goldenFor(c.name)) {
    const k = `${g.run_count}/${g.open_count}`;
    const idx = first.get(ck(g.category, g.target));
    if (idx === undefined) {
      fatal(`fixture broken: golden group ${g.category}/${g.target} has no row in cases.json`);
    }
    const prev = last.get(k);
    if (prev !== undefined) {
      checked += 1;
      if (idx < prev) {
        fatal(
          `fixture broken: within the (run_count ${g.run_count}, open_count ${g.open_count}) tie the ` +
            `golden puts ${g.target} at input index ${idx} after a group at index ${prev} -- ties must ` +
            `keep first-seen order, and a golden regenerated from an UNSTABLE sort is exactly what this ` +
            `looks like`,
        );
      }
    }
    last.set(k, idx);
  }
  if (checked === 0) {
    fatal(
      "fixture broken: the golden contains no two groups sharing a (run_count, open_count) value, so " +
        "nothing here constrains tie ordering at all",
    );
  }
}
