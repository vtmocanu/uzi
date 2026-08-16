// Deterministic guard against reopening the api → mockApi → api runtime import
// cycle behind issue #165. The flake was vitest's importActual("../lib/api")
// occasionally returning a namespace missing `ApiError` under parallel load
// ("No \"ApiError\" export is defined on the \"../lib/api\" mock"), because a
// runtime value import from the `../lib/api` barrel into the mock graph closes a
// cycle with lib/api.ts (which imports mockApi at module top).
//
// This test reads each mock source with node:fs (no module evaluation, no
// timing, no sleep) and asserts that no statement introduces a RUNTIME edge to
// the `../lib/api` barrel. Three edge shapes are scanned:
//   1. `import … from "../lib/api"`      — every named binding must carry the
//      `type` modifier, or the whole statement must be `import type`.
//   2. `import "../lib/api"`             — a bare side-effect import eagerly
//      evaluates the barrel, so it is always a runtime edge.
//   3. `export … from "../lib/api"`      — a re-export is classified exactly
//      like an import (`export type { … }` or per-entry `type X` is safe; a
//      bare re-exported binding is runtime).
// Runtime values that used to come from the barrel (ApiError, isTerminalRun)
// now live in the leaf modules ../lib/apiError and ../lib/runStatus, which is
// what keeps the cycle closed. Reintroducing any runtime edge re-opens it.

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

// The barrel is only ever referenced as `../lib/api` in mock sources, but a
// future edit could write the explicit-extension form. Normalize by stripping a
// single trailing `.ts`/`.js` before the exact compare so `../lib/api.ts` and
// `../lib/api.js` still resolve to the barrel. (No path-alias resolution — mocks
// use `../lib/api` only; keep it simple.)
function isApiBarrel(specifier: string): boolean {
  return specifier.replace(/\.(ts|js)$/, "") === API_SPECIFIER;
}

// Strip block and line comments so a commented-out or annotated binding inside
// the brace list can't be miscounted.
function stripComments(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");
}

type OffenderKind = "import" | "side-effect import" | "re-export";

interface Offender {
  kind: OffenderKind;
  binding: string;
}

// Return the RUNTIME (non-type) bindings a single import/re-export clause (the
// text between the `import`/`export` keyword and `from`) introduces. An empty
// list means the clause is type-only and therefore safe. The classification is
// identical for `import … from` and `export … from`: `type { … }` (whole
// statement) or a per-entry `type X` is safe; anything else is runtime.
function runtimeBindingsOf(clause: string): string[] {
  const runtime: string[] = [];

  const braceStart = clause.indexOf("{");
  const braceEnd = clause.lastIndexOf("}");
  const prefix = stripComments(
    braceStart === -1 ? clause : clause.slice(0, braceStart),
  ).trim();

  // `import type { … }` / `export type { … }` — a whole-statement type edge
  // introduces no runtime binding regardless of the individual entries.
  const wholeStatementType = /^type\b/.test(prefix);

  // Anything before the brace that isn't the `type` keyword is a default,
  // namespace, or `export *` binding, which is always a runtime value.
  const prefixRemainder = prefix.replace(/^type\b/, "").trim();
  if (!wholeStatementType && prefixRemainder.length > 0) {
    runtime.push(prefixRemainder);
  }

  if (braceStart !== -1 && braceEnd > braceStart) {
    const list = stripComments(clause.slice(braceStart + 1, braceEnd));
    for (const rawEntry of list.split(",")) {
      const entry = rawEntry.trim();
      if (entry.length === 0) continue;
      if (wholeStatementType) continue; // every entry is type-only
      // A binding is runtime UNLESS it starts with the `type` keyword.
      if (!/^type\b/.test(entry)) {
        runtime.push(entry);
      }
    }
  }

  return runtime;
}

// Scan a mock source for every RUNTIME edge to the api barrel across three
// shapes: `import … from`, bare side-effect `import "…"`, and `export … from`.
function barrelOffenders(source: string): Offender[] {
  const offenders: Offender[] = [];

  // `import … from "spec"` and `export … from "spec"` (both multi-line aware).
  const importFrom = /import\s+([\s\S]*?)\s+from\s+["']([^"']+)["']/g;
  const exportFrom = /export\s+([\s\S]*?)\s+from\s+["']([^"']+)["']/g;
  // Bare side-effect `import "spec"` — no clause, no `from`. Evaluates the
  // barrel eagerly, so it is always a runtime edge. (The `["']` right after the
  // whitespace is what distinguishes it from an `import … from` statement.)
  const sideEffect = /import\s+["']([^"']+)["']/g;

  let match: RegExpExecArray | null;

  for (const [re, kind] of [
    [importFrom, "import"],
    [exportFrom, "re-export"],
  ] as const) {
    while ((match = re.exec(source)) !== null) {
      if (!isApiBarrel(match[2])) continue;
      for (const binding of runtimeBindingsOf(match[1])) {
        offenders.push({ kind, binding });
      }
    }
  }

  while ((match = sideEffect.exec(source)) !== null) {
    if (isApiBarrel(match[1])) {
      offenders.push({ kind: "side-effect import", binding: match[1] });
    }
  }

  return offenders;
}

function describeOffender(o: Offender): string {
  switch (o.kind) {
    case "side-effect import":
      return `side-effect import "${o.binding}"`;
    case "re-export":
      return `re-export ${o.binding}`;
    default:
      return `import ${o.binding}`;
  }
}

describe("mock graph does not import runtime values from the api barrel (issue #165)", () => {
  for (const name of MOCK_FILES) {
    it(`${name} imports only types from "${API_SPECIFIER}"`, () => {
      const source = readFileSync(new URL(`./${name}`, import.meta.url), "utf8");

      // Files with no ../lib/api edge trivially pass.
      const offenders = barrelOffenders(source).map(describeOffender);

      expect(
        offenders,
        `${name} introduces a RUNTIME edge to the "${API_SPECIFIER}" barrel ` +
          `(${offenders.join(", ")}). This re-opens the api → mockApi → api ` +
          `runtime import cycle that caused the vitest 'No "ApiError" export is ` +
          `defined on the "../lib/api" mock' flake (issue #165). Import runtime ` +
          `values from a leaf module (e.g. ../lib/apiError, ../lib/runStatus) ` +
          `instead, make the binding type-only, or drop the side-effect import.`,
      ).toEqual([]);
    });
  }
});
