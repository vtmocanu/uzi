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
