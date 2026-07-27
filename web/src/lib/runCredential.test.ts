import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { describeCredential, SELECT_REASONS } from "./runCredential";

// PRD #111 M5 — the mode rendering, and the guard that keeps three languages in step.

function run(over: Partial<Parameters<typeof describeCredential>[0]> = {}) {
  return {
    anthropic_secret_id: "sec-1",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    ...over,
  };
}

// 🔴 THE CROSS-LANGUAGE GUARD, and it is the only assertion here that can catch the
// failure that actually matters.
//
// The vocabulary lives in THREE places: autoselect.Reason in Go, migration 00089's
// CHECK in SQL, and the SelectReason union in TypeScript. Go and SQL are already
// pinned to each other (workersvc's TestSelectReasonVocabularyMatchesCheck parses the
// same file). This is the third edge, and without it the web is the one surface that
// can silently fall behind: a ninth reason ships server-side, every Go test stays
// green, and the chip renders the raw wire string to a user forever.
//
// It parses the MIGRATION rather than the Go source, deliberately. The migration is
// the narrowest artefact of the three — one CHECK, one list, no comments in the
// statement — so the parser is four lines instead of a Go-syntax reader, and it is
// the artefact both other edges already point at, which makes it the hub rather than
// a third opinion.
//
// MUTATIONS THIS CATCHES, both directions measured: adding a value to 00089's CHECK
// that the web does not know, and deleting an entry from REASON_PHRASES while it
// stays in the CHECK.
//
// 🔴 IT COMPARES AGAINST SELECT_REASONS, WHICH IS DERIVED, and an earlier version
// compared against a hand-written array beside the union instead. That version was
// HOLLOW in one direction and the mutation run proved it: the array carried
// `satisfies readonly SelectReason[]`, which constrains it to contain only valid
// members and says nothing about containing all of them, so a union member missing
// from the array left this green. See SELECT_REASONS for why deriving it from the
// exhaustive Record closes that.
function reasonsFromMigration(): string[] {
  const path = "../api/internal/store/migrations/00089_run_select_reason_check.sql";
  const raw = readFileSync(path, "utf8");
  // Comments first. The prose above the statement names several reasons, and a regex
  // over the whole file would happily collect them and agree with itself.
  const stmt = raw
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("--"))
    .join("\n");
  return [...stmt.matchAll(/'([a-z_]+)'/g)].map((m) => m[1]).sort();
}

describe("the reason vocabulary is one vocabulary", () => {
  it("matches migration 00089's CHECK", () => {
    const fromSQL = reasonsFromMigration();
    expect(fromSQL.length).toBeGreaterThan(0);
    expect(fromSQL).toEqual([...SELECT_REASONS].sort());
  });

  // Exhaustiveness of the RENDERING is enforced by the typed Record in
  // runCredential.ts, so this asserts what the type cannot: that every entry produces
  // words a user can act on, and that no two reasons share a phrase. A rendering that
  // exists and says the same thing as its neighbour passes a coverage check and helps
  // nobody.
  it("gives every reason its own words", () => {
    const seen = new Map<string, string>();
    for (const reason of SELECT_REASONS) {
      const { mode, hint } = describeCredential(run({ anthropic_select_reason: reason }));
      expect(mode, `${reason} has no mode phrase`).not.toBe("");
      expect(hint, `${reason} has no hint`).not.toBe("");
      const prev = seen.get(mode);
      expect(prev, `${reason} and ${prev} share the phrase "${mode}"`).toBeUndefined();
      seen.set(mode, reason);
    }
  });
});

describe("describeCredential", () => {
  // fellBack is what the chip colours on, and it must be true for exactly the three
  // fallbacks. Too wide and an ordinary default wears a warning; too narrow and the
  // one state where the user's configuration and reality differ looks routine.
  it("flags exactly the three auto fallbacks", () => {
    const fell: string[] = [];
    for (const reason of SELECT_REASONS) {
      if (describeCredential(run({ anthropic_select_reason: reason })).fellBack) fell.push(reason);
    }
    expect(fell.sort()).toEqual(["open_failed", "pool_empty", "pool_stale"]);
  });

  it("appends the headroom only when the server measured one", () => {
    expect(
      describeCredential(run({ anthropic_select_reason: "auto", anthropic_headroom_pct: 62 })).mode,
    ).toBe("auto, 62% headroom");
    expect(describeCredential(run({ anthropic_select_reason: "auto" })).mode).toBe("auto");
  });

  // 0% is a LEGAL headroom (a fully-consumed token picked best-of-pool), so the guard
  // must be a null check and never a falsy one. This is the case a `pct ? …` would
  // silently drop, and it is exactly the run whose headroom a user most wants to see.
  it("renders a zero headroom rather than treating it as absent", () => {
    expect(
      describeCredential(
        run({ anthropic_select_reason: "best_of_pool", anthropic_headroom_pct: 0 }),
      ).mode,
    ).toBe("auto (best of pool), 0% headroom");
  });

  it("has no mode for a run claimed before the feature landed", () => {
    expect(describeCredential(run()).mode).toBe("");
  });

  it("passes an unrecognised reason through verbatim", () => {
    const got = describeCredential(run({ anthropic_select_reason: "some_future_reason" }));
    expect(got.mode).toBe("some_future_reason");
    expect(got.fellBack).toBe(false);
  });

  // The id and the reason are independent fields; `deleted` must not depend on the
  // mode, nor the mode on the id.
  it("reports deletion independently of the mode", () => {
    expect(describeCredential(run({ anthropic_secret_id: null })).deleted).toBe(true);
    expect(
      describeCredential(run({ anthropic_secret_id: null, anthropic_select_reason: "auto" }))
        .deleted,
    ).toBe(true);
    expect(describeCredential(run({ anthropic_select_reason: "auto" })).deleted).toBe(false);
  });
});
