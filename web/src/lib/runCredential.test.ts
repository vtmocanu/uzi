import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import {
  CODEX_SELECT_REASONS,
  describeCodexCredential,
  describeCredential,
  SELECT_REASONS,
} from "./runCredential";

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
  // PRD #1247 M1 widened the vocabulary in 00233 (run_pinned, run_default) alongside the
  // override-MODE CHECKs, so this parses 00233 now. 00233 carries other quoted
  // vocabularies (the override-mode IN-list 'pinned'/'auto'/'default'), so a whole-file
  // regex would pull those in; anchor strictly on the runs_anthropic_select_reason_check
  // IN-list, the same way the Go guard (TestSelectReasonVocabularyMatchesCheck) does.
  const path = "../api/internal/store/migrations/00233_per_run_credential_override.sql";
  const raw = readFileSync(path, "utf8");
  // Comments first. The prose above the statement names several reasons, and a regex
  // over the whole file would happily collect them and agree with itself.
  const stmt = raw
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("--"))
    .join("\n");
  // Isolate the select-reason IN-list. 00233 both DROPs (no IN-list) and re-ADDs the
  // constraint; this non-global .match() returns only the FIRST match in the file, the
  // Up section's re-ADD (the NOT VALID one, the widened vocabulary), before the Down
  // section's copy.
  const inList = stmt.match(/anthropic_select_reason IN \(([^)]*)\)/);
  if (!inList) throw new Error("could not find the anthropic_select_reason IN-list in 00233");
  return [...inList[1].matchAll(/'([a-z_]+)'/g)].map((m) => m[1]).sort();
}

describe("the reason vocabulary is one vocabulary", () => {
  it("matches migration 00233's CHECK", () => {
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
  // The tone ordering, which web-ux F17 found inverted. Amber for exactly the three
  // fallbacks (the selector skipped the pool), info for best_of_pool (it picked, and
  // the pool is nearly exhausted), neutral for the rest. Too wide and an ordinary
  // default wears a warning; too narrow and the one state where the user's
  // configuration and reality differ looks routine.
  it("assigns amber to exactly the three fallbacks and info to best_of_pool", () => {
    const byTone: Record<string, string[]> = { neutral: [], info: [], warning: [] };
    for (const reason of SELECT_REASONS) {
      byTone[describeCredential(run({ anthropic_select_reason: reason })).tone].push(reason);
    }
    expect(byTone.warning.sort()).toEqual(["open_failed", "pool_empty", "pool_stale"]);
    expect(byTone.info).toEqual(["best_of_pool"]);
    // run_default/run_pinned (PRD #1247) are neutral: a deliberate per-run override, not
    // a fallback or a warning.
    expect(byTone.neutral.sort()).toEqual([
      "auto",
      "default",
      "judge",
      "pinned",
      "run_default",
      "run_pinned",
    ]);
  });

  // ONE rule, asserted as one: link iff the tone is not neutral. Two independent
  // predicates would drift the first time a reason changed tone.
  it("links exactly the non-neutral states", () => {
    for (const reason of SELECT_REASONS) {
      const got = describeCredential(run({ anthropic_select_reason: reason }));
      expect(got.linked, `${reason} tone=${got.tone} linked=${got.linked}`).toBe(
        got.tone !== "neutral",
      );
    }
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
    expect(got.tone).toBe("neutral");
    expect(got.linked).toBe(false);
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

// ── Codex (issue #3719) ───────────────────────────────────────────────────────────────

// The Codex edge of the same three-language guard. codexselect.AllReasons in Go is
// already pinned to migration 00387's runs_codex_select_reason_check; this pins the web's
// exhaustive phrase map to that CHECK too, so a reason added server-side without a web
// phrase reddens here instead of rendering the raw wire string. Comments are stripped
// first, and the match is anchored on the codex_select_reason IN-list, because the file
// carries other quoted vocabularies (users_codex_account_mode_check's 'default'/'auto').
function codexReasonsFromMigration(): string[] {
  const path = "../api/internal/store/migrations/00387_codex_account_autoselect.sql";
  const stmt = readFileSync(path, "utf8")
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("--"))
    .join("\n");
  const inList = stmt.match(/codex_select_reason IN \(([^)]*)\)/);
  if (!inList) throw new Error("could not find the codex_select_reason IN-list in 00387");
  return [...inList[1].matchAll(/'([a-z_]+)'/g)].map((m) => m[1]).sort();
}

function codexRun(reason: string | null, id: string | null = "sec-codex") {
  return { codex_secret_id: id, codex_select_reason: reason };
}

describe("the Codex reason vocabulary is one vocabulary", () => {
  it("matches migration 00387's CHECK", () => {
    const fromSQL = codexReasonsFromMigration();
    // Six, so a parser that matched nothing (or the wrong, two-value list) cannot pass.
    expect(fromSQL).toHaveLength(6);
    expect(fromSQL).toEqual([...CODEX_SELECT_REASONS].sort());
  });

  it("gives every Codex reason its own words", () => {
    const seen = new Map<string, string>();
    for (const reason of CODEX_SELECT_REASONS) {
      const { mode, hint } = describeCodexCredential(codexRun(reason));
      expect(mode, `${reason} has no mode phrase`).not.toBe("");
      expect(hint, `${reason} has no hint`).not.toBe("");
      const prev = seen.get(mode);
      expect(prev, `${reason} and ${prev} share the phrase "${mode}"`).toBeUndefined();
      seen.set(mode, reason);
    }
  });
});

describe("describeCodexCredential", () => {
  it("is calm for default and auto, info for the actionable picks, amber where auto could not rank or every login was spent", () => {
    const byTone: Record<string, string[]> = { neutral: [], info: [], warning: [] };
    for (const reason of CODEX_SELECT_REASONS) {
      byTone[describeCodexCredential(codexRun(reason)).tone].push(reason);
    }
    expect(byTone.neutral.sort()).toEqual(["auto", "default"]);
    expect(byTone.info.sort()).toEqual(["best_of_pool", "pool_empty"]);
    expect(byTone.warning.sort()).toEqual(["pool_exhausted", "pool_stale"]);
  });

  it("links exactly the non-neutral states", () => {
    for (const reason of CODEX_SELECT_REASONS) {
      const got = describeCodexCredential(codexRun(reason));
      expect(got.linked, `${reason}`).toBe(got.tone !== "neutral");
    }
  });

  it("renders no mode for a run that recorded no reason, and says it was not recorded", () => {
    for (const reason of [null, ""]) {
      const got = describeCodexCredential(codexRun(reason));
      expect(got.mode).toBe("");
      expect(got.hint).toMatch(/not recorded/);
      expect(got.tone).toBe("neutral");
      expect(got.linked).toBe(false);
    }
  });

  it("shows an unrecognised reason as itself rather than guessing", () => {
    const got = describeCodexCredential(codexRun("pool_sharded"));
    expect(got.mode).toBe("pool_sharded");
    expect(got.hint).toContain("“pool_sharded”");
    expect(got.tone).toBe("neutral");
  });

  it("marks a deleted login", () => {
    expect(describeCodexCredential(codexRun("auto", null)).deleted).toBe(true);
    expect(describeCodexCredential(codexRun("auto")).deleted).toBe(false);
  });
});
