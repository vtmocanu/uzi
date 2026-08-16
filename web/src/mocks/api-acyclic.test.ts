// Deterministic guard against reopening the api → mockApi → api runtime import
// cycle behind issue #165. The flake was vitest's importActual("../lib/api")
// occasionally returning a namespace missing `ApiError` under parallel load
// ("No \"ApiError\" export is defined on the \"../lib/api\" mock"), because a
// runtime value import from the `../lib/api` barrel into the mock graph closes a
// cycle with lib/api.ts (which imports mockApi at module top).
//
// This test reads each mock source with node:fs (no module evaluation, no
// timing, no sleep) and asserts that any `import … from "../lib/api"` statement
// introduces NO runtime binding — every named binding must carry the `type`
// modifier, or the whole statement must be `import type`. Runtime values that
// used to come from the barrel (ApiError, isTerminalRun) now live in the leaf
// modules ../lib/apiError and ../lib/runStatus, which is what keeps the cycle
// closed. Reintroducing a runtime import from the barrel re-opens it.

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// Mock-graph files that (may) import from the api barrel. Resolved relative to
// this test via import.meta.url so the read works regardless of cwd.
const MOCK_FILES = [
  "mockApi.ts",
  "socket.ts",
  "engine.ts",
  "store.ts",
  "data.ts",
];

const API_SPECIFIER = "../lib/api";

// Strip block and line comments so a commented-out or annotated binding inside
// the brace list can't be miscounted.
function stripComments(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");
}

interface RuntimeBinding {
  binding: string;
}

// Return the list of RUNTIME (non-type) bindings a single `from "../lib/api"`
// import statement introduces. An empty list means the statement is safe.
function runtimeBindingsOf(clause: string): RuntimeBinding[] {
  const runtime: RuntimeBinding[] = [];

  const braceStart = clause.indexOf("{");
  const braceEnd = clause.lastIndexOf("}");
  const prefix = stripComments(
    braceStart === -1 ? clause : clause.slice(0, braceStart),
  ).trim();

  // `import type { … }` — a whole-statement type import introduces no runtime
  // binding regardless of the individual entries.
  const wholeStatementType = /^type\b/.test(prefix);

  // Anything before the brace that isn't the `type` keyword is a default or
  // namespace binding, which is always a runtime value.
  const prefixRemainder = prefix.replace(/^type\b/, "").trim();
  if (prefixRemainder.length > 0) {
    runtime.push({ binding: prefixRemainder });
  }

  if (braceStart !== -1 && braceEnd > braceStart) {
    const list = stripComments(clause.slice(braceStart + 1, braceEnd));
    for (const rawEntry of list.split(",")) {
      const entry = rawEntry.trim();
      if (entry.length === 0) continue;
      if (wholeStatementType) continue; // every entry is type-only
      // A binding is runtime UNLESS it starts with the `type` keyword.
      if (!/^type\b/.test(entry)) {
        runtime.push({ binding: entry });
      }
    }
  }

  return runtime;
}

// Extract every `import … from "../lib/api"` statement (multi-line aware) and
// return the clause between `import` and `from` for each.
function apiImportClauses(source: string): string[] {
  const clauses: string[] = [];
  const re = /import\s+([\s\S]*?)\s+from\s+["']([^"']+)["']/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(source)) !== null) {
    if (match[2] === API_SPECIFIER) {
      clauses.push(match[1]);
    }
  }
  return clauses;
}

describe("mock graph does not import runtime values from the api barrel (issue #165)", () => {
  for (const name of MOCK_FILES) {
    it(`${name} imports only types from "${API_SPECIFIER}"`, () => {
      const source = readFileSync(new URL(`./${name}`, import.meta.url), "utf8");
      const clauses = apiImportClauses(source);

      // Files with no ../lib/api import trivially pass.
      const offenders = clauses.flatMap((clause) =>
        runtimeBindingsOf(clause).map((b) => b.binding),
      );

      expect(
        offenders,
        `${name} introduces a RUNTIME import from the "${API_SPECIFIER}" barrel ` +
          `(${offenders.join(", ")}). This re-opens the api → mockApi → api ` +
          `runtime import cycle that caused the vitest 'No "ApiError" export is ` +
          `defined on the "../lib/api" mock' flake (issue #165). Import runtime ` +
          `values from a leaf module (e.g. ../lib/apiError, ../lib/runStatus) ` +
          `instead, or make the binding type-only.`,
      ).toEqual([]);
    });
  }
});
