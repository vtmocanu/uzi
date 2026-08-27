import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { RATE_LIMIT_SOURCES } from "./api";

// PRD #217 M3 — the guard that keeps the rate-limit source vocabulary one vocabulary.
//
// 🔴 THE CROSS-LANGUAGE GUARD. The vocabulary lives in three places: anthropic.AllSources()
// in Go, migration 00109's widened CHECK in SQL, and the RateLimitSource union in
// TypeScript. Go and SQL are pinned to each other (M4's drift test parses the same file).
// This is the third edge, and without it the web can silently fall behind: a fourth source
// ships server-side, every Go test stays green, and a surface renders the raw wire string.
//
// It parses the MIGRATION rather than the Go source, mirroring runCredential.test.ts: the
// migration is the narrowest artefact — one CHECK, one list — and it is the artefact the
// other edges already point at, which makes it the hub rather than a third opinion.
//
// 🔴 IT COMPARES AGAINST RATE_LIMIT_SOURCES, WHICH IS THE UNION ITSELF. The RateLimitSource
// type is derived from that array (`(typeof RATE_LIMIT_SOURCES)[number]`), so there is no
// hand-written mirror that could carry only valid members while missing some — the array
// and the union cannot drift.
function sourcesFromMigration(): string[] {
  const path = "../api/internal/store/migrations/00109_rate_limit_source_limit_report.sql";
  const raw = readFileSync(path, "utf8");
  // Scope to the `+goose Up` widening ADD. The file carries a Down ADD that narrows back
  // to two values, and a regex over the whole file would collect those too and disagree
  // with itself. Comments (the prose names the sources) are stripped for the same reason.
  const up = raw.slice(raw.indexOf("+goose Up"), raw.indexOf("+goose Down"));
  const stmt = up
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("--"))
    .join("\n");
  return [...stmt.matchAll(/'([a-z_]+)'/g)].map((m) => m[1]).sort();
}

describe("the rate-limit source vocabulary is one vocabulary", () => {
  it("matches migration 00109's CHECK", () => {
    const fromSQL = sourcesFromMigration();
    expect(fromSQL.length).toBeGreaterThan(0);
    expect(fromSQL).toEqual([...RATE_LIMIT_SOURCES].sort());
  });
});
