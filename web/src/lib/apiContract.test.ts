import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import type { Run, RunListItem } from "./apiTypes";

import runZero from "../../../fixtures/api-contract/run.zero.json";
import runFull from "../../../fixtures/api-contract/run.full.json";
import runListItemZero from "../../../fixtures/api-contract/run_list_item.zero.json";
import runListItemFull from "../../../fixtures/api-contract/run_list_item.full.json";

// The api ⇄ SPA JSON wire-contract (PRD #982). This is the VITEST HALF; the Go
// half is api/internal/apitypes/contract_test.go. Neither reads the other: each
// side checks the SAME recorded fixtures under fixtures/api-contract/ with its
// OWN production definition (the Go struct, the TS type here), so a failure names
// the side that drifted. The precedent shape is fixtures/run-usage.
//
// Per DTO two fixtures:
//   <stem>.zero.json  == json.Marshal(T{})            — every null the type emits
//   <stem>.full.json  == json.Marshal(populate(T{}))  — key set + value kinds
//
// Three compile-time assertions per DTO (a tsc error here is a red gate:web, so
// the drift is caught by the typechecker, not only by vitest):
//   1. key-set equality, both directions (each `Exclude` must be `never`)
//   2. nullability: every null the zero value emits is accepted (ZeroOf exemptions)
//   3. value kinds: the populated shape is accepted with literal unions widened
// plus a runtime self-check that the fixtures exist and that assertion 2 is not
// vacuous (its zero.json really does carry a null).
//
// 🔴 THE FIXTURES ARE RECORDED, NOT AUTHORED. There is no -update flag; the Go
// half prints the exact JSON on a mismatch so re-recording is a copy-paste. The
// Go half additionally needs `go test -count=1` because fixtures/ sits above
// api/ and does not enter that module's test cache; vitest has no such cache.

// Widen<T> maps every string-literal union member to `string` (and number/boolean
// likewise), recursing through arrays and objects, while leaving null, undefined
// and unknown untouched. A JSON import types the fixture's "queued" as `string`,
// so a raw `= runZero` would false-fail on `status: RunStatus`; Widen keeps the
// nullability and kind checks (`title: string | null` still rejects a fixture
// null in a never-null slot) while ignoring enum narrowing. What it gives up is
// stated in the README under "What this cannot catch": an enum member the server
// adds and the TS union lacks is not caught here.
type Widen<T> = T extends string
  ? string
  : T extends number
    ? number
    : T extends boolean
      ? boolean
      : T extends readonly (infer E)[]
        ? Widen<E>[]
        : T extends object
          ? { [K in keyof T]: Widen<T[K]> }
          : T;

// ZeroOf<T, NeverNull> is Widen<T> with the named fields additionally accepting
// null. It is the per-field, reason-carrying exemption list of Decision 7: the
// zero fixture is json.Marshal(T{}), which emits null for every nil slice, but a
// mapper normalizes some of those to [] on the real wire, so the TS type is right
// to say never-null. Naming the field as a string literal means a rename breaks
// the exemption too, and the README cites the mapper line for each.
type ZeroOf<T, NeverNull extends keyof T = never> = {
  [K in keyof T]: K extends NeverNull ? Widen<T[K]> | null : Widen<T[K]>;
};

// ── Run ─────────────────────────────────────────────────────────────────────
// ZeroOf exemptions for Run (all three normalized to [] by capsOrEmpty in
// runToDTO): plan_changed_files (handler/workers.go:448), required_capabilities
// (:442), required_tools (:443); capsOrEmpty is handler/forge.go:148.
{
  // 1. key-set equality, both directions.
  const _runMissing: never = null as unknown as Exclude<keyof Run, keyof typeof runFull>;
  // @ts-expect-error #982: scope_ceiling, base_branch, open_mr, interactive, dispatched_at, branch_has_active_run, branch_has_open_mr — reconciled in M4
  const _runExtra: never = null as unknown as Exclude<keyof typeof runFull, keyof Run>;
  // 2. nullability with the mapper-normalized exemptions applied.
  const _runZero: ZeroOf<Run, "plan_changed_files" | "required_capabilities" | "required_tools"> = runZero;
  // 3. value kinds, literal unions widened.
  const _runFull: Widen<Run> = runFull;
  void _runMissing;
  void _runExtra;
  void _runZero;
  void _runFull;
}

// ── RunListItem ─────────────────────────────────────────────────────────────
// RunListItem extends Run, so its extra keys inherit the same 7 drift fields; its
// own fields (repo_path, worker_name, owner_email, judge_verdict, judge_todo_count,
// is_revising) match. Same three ZeroOf exemptions, inherited from Run.
{
  const _runListItemMissing: never = null as unknown as Exclude<keyof RunListItem, keyof typeof runListItemFull>;
  // @ts-expect-error #982: scope_ceiling, base_branch, open_mr, interactive, dispatched_at, branch_has_active_run, branch_has_open_mr — reconciled in M4
  const _runListItemExtra: never = null as unknown as Exclude<keyof typeof runListItemFull, keyof RunListItem>;
  const _runListItemZero: ZeroOf<RunListItem, "plan_changed_files" | "required_capabilities" | "required_tools"> =
    runListItemZero;
  const _runListItemFull: Widen<RunListItem> = runListItemFull;
  void _runListItemMissing;
  void _runListItemExtra;
  void _runListItemZero;
  void _runListItemFull;
}

// ── Runtime self-checks ─────────────────────────────────────────────────────
// A contract that passes on a missing fixture, or on a zero.json with no null in
// it, is the false-green shape this repo documents repeatedly. These fatal
// (never skip) so a gutted fixture reddens instead of quietly passing.

function read(name: string): string {
  const url = new URL(`../../../fixtures/api-contract/${name}`, import.meta.url);
  try {
    return readFileSync(url, "utf8");
  } catch (err) {
    throw new Error(
      `fixture unreadable: ${name}: ${String(err)} -- this contract asserts nothing ` +
        `without it, and skipping would look identical to passing`,
    );
  }
}

function hasNull(v: unknown): boolean {
  if (v === null) return true;
  if (Array.isArray(v)) return v.some(hasNull);
  if (typeof v === "object") return Object.values(v as Record<string, unknown>).some(hasNull);
  return false;
}

// Each DTO stem, and whether its Go struct has at least one nullable field (so
// its zero.json MUST carry a null — assertion 2 would be vacuous otherwise). Both
// hot M1 DTOs have nullable fields; an all-scalar DTO would be declared here with
// `nullable: false` rather than guarded, per the PRD.
const dtos: { stem: string; nullable: boolean }[] = [
  { stem: "run", nullable: true },
  { stem: "run_list_item", nullable: true },
];

describe("api-contract fixtures are present and discriminating", () => {
  for (const { stem, nullable } of dtos) {
    it(`${stem}: both fixtures are readable`, () => {
      expect(() => JSON.parse(read(`${stem}.zero.json`))).not.toThrow();
      expect(() => JSON.parse(read(`${stem}.full.json`))).not.toThrow();
    });

    it(`${stem}: zero.json carries a null iff the DTO has a nullable field`, () => {
      const zero = JSON.parse(read(`${stem}.zero.json`));
      if (nullable) {
        expect(hasNull(zero), `${stem}.zero.json has no null -- assertion 2 would be vacuous`).toBe(true);
      } else {
        expect(hasNull(zero), `${stem}.zero.json has a null but the DTO is declared all-scalar`).toBe(false);
      }
    });

    it(`${stem}: full.json contains no null (every field exercised)`, () => {
      const full = JSON.parse(read(`${stem}.full.json`));
      expect(hasNull(full), `${stem}.full.json has a null -- the populator left a field zero`).toBe(false);
    });
  }
});
