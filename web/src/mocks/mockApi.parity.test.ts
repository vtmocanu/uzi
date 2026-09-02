import { describe, it, expect } from "vitest";
import { api } from "../lib/api";
import { mockApi } from "./mockApi";

// Runtime key-set parity between the mock API surface and the real API surface.
//
// WHY `api` IS `realApi` HERE. `web/src/lib/api.ts` exports
//   `export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;`
// where `MOCK_MODE = import.meta.env.VITE_UZI_MOCK === "1"`. Under vitest there is
// no `web/.env*` and `vite.config.ts` sets no `VITE_UZI_MOCK`, so `MOCK_MODE` is
// OFF and `import { api }` resolves to the (unexported) `realApi` object at
// runtime. That is what lets this file compare two INDEPENDENT key sets: `mockApi`
// (the mock surface) against `api` (the real surface).
//
// This runs under the node environment (the default for `src/mocks/**`, see the
// project split in vite.config.ts) with no `@vitest-environment jsdom` and no
// localStorage shim. Note the import chain is NOT free of module-load storage
// access — `mockApi.ts`'s top-level `const loadedSettings = loadSettings();` calls
// `localStorage.getItem(...)`, which is undefined under node. The import survives
// only because `loadSettings()` wraps that read in a try/catch that swallows the
// ReferenceError. So the node-safety of this file depends on that guard staying in
// place, not on the absence of any module-load access.
//
// WHY THIS EARNS ITS KEEP over the `typeof realApi` compile-time guard on the
// assignment above: that conditional-expression annotation cannot see an EXTRA key
// present on `mockApi` but absent from `realApi` (the mock is assignable to
// `typeof realApi` even with surplus keys). The runtime key-set equality below
// catches exactly that, in addition to a missing key.

describe("mockApi <-> realApi surface parity", () => {
  it("has identical top-level method key sets", () => {
    // Non-vacuity guard, co-located so it cannot be skipped/deleted apart from the
    // assertion it defends: if a future env change flips MOCK_MODE on (e.g. a stray
    // VITE_UZI_MOCK=1), `api` would BE `mockApi` and the key-set comparison below
    // would compare an object with itself — trivially green and meaningless.
    expect(api).not.toBe(mockApi);

    expect(Object.keys(mockApi).sort()).toEqual(Object.keys(api).sort());
  });
});

// ── Duplicate-key guard (PRD #991 M3) ────────────────────────────────────────
// mockApi is composed by spreading one partial object per domain module
// (`<domain>Api`) — see mockApi/index.ts. `{ ...a, ...b }` with a shared key is
// legal TypeScript: the later spread silently wins and `tsc` says nothing, so the
// `typeof realApi` completeness guard above CANNOT see a method contributed by two
// partials. This half closes that hole. It enumerates the partials by directory
// glob (the eager-glob-under-vitest pattern lib/sourceBytes.test.ts uses, but with
// the DEFAULT import to read each module's exports rather than its raw text), so a
// newly-added module is covered automatically and cannot dodge the guard.

const PARTIAL_MODULES = import.meta.glob("./mockApi/*.ts", { eager: true }) as Record<
  string,
  Record<string, unknown>
>;

// A domain partial is any export whose name ends in `Api` and whose value is an
// object — EXCEPT `mockApi` itself (the composed object, exported from index.ts),
// which legitimately carries every key and would swamp the duplicate check.
function collectPartials(): { name: string; keys: string[] }[] {
  const partials: { name: string; keys: string[] }[] = [];
  for (const mod of Object.values(PARTIAL_MODULES)) {
    for (const [name, value] of Object.entries(mod)) {
      if (!name.endsWith("Api") || name === "mockApi") continue;
      if (typeof value !== "object" || value === null) continue;
      partials.push({ name, keys: Object.keys(value) });
    }
  }
  return partials;
}

describe("mockApi partial composition (duplicate-key guard)", () => {
  const partials = collectPartials();

  it("enumerated enough modules and partials to be non-vacuous", () => {
    // A glob that matched nothing (a path typo, a rename that stopped matching)
    // would make both assertions below pass over the empty set — so floor the
    // counts, the way lib/sourceBytes.test.ts floors its own glob.
    expect(Object.keys(PARTIAL_MODULES).length).toBeGreaterThan(5);
    expect(partials.length).toBeGreaterThan(10);
  });

  it("has no method key contributed by two different partials", () => {
    const owners = new Map<string, string[]>();
    for (const p of partials) {
      for (const key of p.keys) {
        owners.set(key, [...(owners.get(key) ?? []), p.name]);
      }
    }
    // A failure names the duplicated key and the partials that both carry it, so
    // the report points straight at the collision `{ ...a, ...b }` would hide.
    const duplicates = [...owners.entries()]
      .filter(([, names]) => names.length > 1)
      .map(([key, names]) => `${key}: ${[...names].sort().join(", ")}`)
      .sort();
    expect(duplicates).toEqual([]);
  });

  it("has partial keys whose union is exactly the composed mockApi surface", () => {
    // Catches a partial that is not spread into index (or is mis-named), and a
    // method spread into mockApi that no partial owns.
    const union = new Set<string>();
    for (const p of partials) for (const key of p.keys) union.add(key);
    expect([...union].sort()).toEqual(Object.keys(mockApi).sort());
  });
});
