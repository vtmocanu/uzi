// Deterministic guard against reopening the api → mockApi → api runtime import
// cycle behind issue #165. The flake was vitest's importActual("../lib/api")
// occasionally returning a namespace missing `ApiError` under parallel load
// ("No \"ApiError\" export is defined on the \"../lib/api\" mock"), because a
// runtime value import from the `../lib/api` barrel into the mock graph closes a
// cycle with lib/api.ts (which imports mockApi at module top).
//
// This test reads each mock source with node:fs (no module evaluation, no
// timing, no sleep) and asserts that no statement introduces a RUNTIME edge to
// the api barrel (web/src/lib/api). Three edge shapes are scanned:
//   1. `import … from "…/lib/api"`      — every named binding must carry the
//      `type` modifier, or the whole statement must be `import type`.
//   2. `import "…/lib/api"`             — a bare side-effect import eagerly
//      evaluates the barrel, so it is always a runtime edge.
//   3. `export … from "…/lib/api"`      — a re-export is classified exactly
//      like an import (`export type { … }` or per-entry `type X` is safe; a
//      bare re-exported binding is runtime).
//
// The mock graph is enumerated by a DIRECTORY WALK of every non-test `.ts` file
// under src/mocks/ (recursive), so a file added in a subdirectory — e.g. the
// mocks/data/ domain modules split out of the former mocks/data.ts — cannot dodge
// the guard. A specifier is classified as the api barrel by RESOLVING it against
// the importing file's own directory to an absolute path and comparing to the
// barrel's absolute path, so both `../lib/api` (from src/mocks/) and
// `../../lib/api` (from src/mocks/data/) are recognized — string equality to a
// single literal would miss the deeper form and let a runtime edge added in a
// data/ module pass silently.
//
// Runtime values that used to come from the barrel (ApiError, isTerminalRun)
// now live in the leaf modules ../lib/apiError and ../lib/runStatus, which is
// what keeps the cycle closed. Reintroducing any runtime edge re-opens it.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const MOCKS_DIR = dirname(fileURLToPath(import.meta.url));
// Absolute path of the api barrel (web/src/lib/api), extensionless, resolved
// relative to this test (src/mocks/ → ../lib/api).
const API_BARREL_PATH = resolve(MOCKS_DIR, "../lib/api");

// Every non-test `.ts` file under src/mocks/, recursively, as an absolute path.
// A directory walk (not a hardcoded list) means a future file — including one in
// a subdirectory like mocks/data/ — is covered automatically.
function mockSourceFiles(): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(MOCKS_DIR, {
    recursive: true,
    withFileTypes: true,
  })) {
    if (!entry.isFile()) continue;
    if (!entry.name.endsWith(".ts")) continue;
    if (entry.name.endsWith(".test.ts")) continue;
    // parentPath (Node ≥ 20) is the directory the entry was found in.
    out.push(resolve(entry.parentPath, entry.name));
  }
  return out.sort();
}

// Classify an import specifier as the api barrel by resolving it against the
// importing FILE's directory and comparing the absolute path to the barrel's.
// Strip a single trailing `.ts`/`.js` first so `../lib/api.ts` still resolves.
// (No path-alias resolution — mocks use relative specifiers only.)
function isApiBarrel(specifier: string, fromDir: string): boolean {
  if (!specifier.startsWith(".")) return false; // bare/package specifier, never the barrel
  const bare = specifier.replace(/\.(ts|js)$/, "");
  return resolve(fromDir, bare) === API_BARREL_PATH;
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
  // namespace, or `export *` binding, which is always a runtime value. Drop a
  // trailing comma so a default+named form (`Foo, { … }`) reports `Foo`, not
  // `Foo,`.
  const prefixRemainder = prefix
    .replace(/^type\b/, "")
    .replace(/,\s*$/, "")
    .trim();
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
// `fromDir` is the importing file's directory, used to resolve each specifier.
//
// The scan is STATEMENT-ANCHORED: comments are stripped, then the source is
// split on `;` so each fragment is a single statement. This is what keeps the
// non-greedy `… from "…"` match from crossing a statement boundary — otherwise
// an unrelated `export const X = …` sitting above a legit `export type { … }
// from "../lib/api"` would pair its keyword with the later `from` and
// false-flag a type-only re-export. The repo enforces semicolons (prettier), so
// every import/export-from statement is exactly one `;`-delimited fragment.
function barrelOffenders(source: string, fromDir: string): Offender[] {
  const offenders: Offender[] = [];

  // Anchored to the whole (trimmed) fragment via ^…$ so nothing outside the one
  // statement can be swallowed. `[\s\S]*?` still handles multi-line brace lists.
  const importFrom = /^import\s+([\s\S]*?)\s+from\s+["']([^"']+)["']$/;
  const exportFrom = /^export\s+([\s\S]*?)\s+from\s+["']([^"']+)["']$/;
  // Bare side-effect `import "spec"` — no clause, no `from`. Evaluates the
  // barrel eagerly, so it is always a runtime edge. (The `["']` right after the
  // whitespace is what distinguishes it from an `import … from` statement.)
  const sideEffect = /^import\s+["']([^"']+)["']$/;

  for (const fragment of stripComments(source).split(";")) {
    const stmt = fragment.trim();
    if (stmt.length === 0) continue;

    const side = sideEffect.exec(stmt);
    if (side) {
      if (isApiBarrel(side[1], fromDir)) {
        offenders.push({ kind: "side-effect import", binding: side[1] });
      }
      continue;
    }

    for (const [re, kind] of [
      [importFrom, "import"],
      [exportFrom, "re-export"],
    ] as const) {
      const match = re.exec(stmt);
      if (!match) continue;
      if (isApiBarrel(match[2], fromDir)) {
        for (const binding of runtimeBindingsOf(match[1])) {
          offenders.push({ kind, binding });
        }
      }
      break;
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
  for (const file of mockSourceFiles()) {
    const rel = file.slice(MOCKS_DIR.length + 1);
    it(`${rel} imports only types from the api barrel`, () => {
      const source = readFileSync(file, "utf8");

      // Files with no api-barrel edge trivially pass.
      const offenders = barrelOffenders(source, dirname(file)).map(describeOffender);

      expect(
        offenders,
        `${rel} introduces a RUNTIME edge to the api barrel ` +
          `(${offenders.join(", ")}). This re-opens the api → mockApi → api ` +
          `runtime import cycle that caused the vitest 'No "ApiError" export is ` +
          `defined on the "../lib/api" mock' flake (issue #165). Import runtime ` +
          `values from a leaf module (e.g. ../lib/apiError, ../lib/runStatus) ` +
          `instead, make the binding type-only, or drop the side-effect import.`,
      ).toEqual([]);
    });
  }
});
