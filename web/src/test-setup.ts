import { configure } from "@testing-library/dom";

// Testing Library's async utilities (waitFor, findBy*) default to a 1s timeout, and that is
// SEPARATE from vitest's testTimeout — raising one does nothing for the other. This project
// had neither configured.
//
// Measured during PRD #98 across ~25 full-suite runs: five unrelated tests each failed at
// least once under CPU contention, in two distinct shapes.
//
//   * "Test timed out in 5000ms" — vitest's testTimeout. mockApi's persistence reload
//     (resetModules + re-import) and the Judge nav-badge cross-mount.
//   * a waitFor giving up at ~1s — Repos' repo-skills opt-in (three tests) and the Agents
//     shadowed-name hint, failing at 1049ms and 1068ms, i.e. right on the default.
//
// Different files, different owners, different PRDs, one cause: the suite runs 84 files
// worth of jsdom mounting against defaults that are marginal on a loaded machine. Patching
// whichever test surfaced last is whack-a-mole — I did that twice before the third and
// fourth appeared elsewhere.
//
// Raising a ceiling never loosens an assertion: a passing test costs nothing extra, and a
// genuinely broken one still fails, just later. What it removes is the 1-in-10 red that
// trains everyone to re-run instead of read — which is the actual damage, and which lands in
// CI where nobody has the context to tell it from a real regression.
configure({ asyncUtilTimeout: 5000 });

// jsdom Web Storage polyfill (issue #340).
//
// Node >=26 ships an EXPERIMENTAL built-in Web Storage `localStorage` global that is
// `undefined` unless the process was started with `--localstorage-file`. Under the vitest
// jsdom environment on Node 26, jsdom installs no Storage of its own, so BOTH the bare
// `localStorage` global and `window.localStorage` come back `undefined` — and a bare
// `localStorage.clear()` (e.g. AuthContext.test.tsx's beforeEach) throws
// "TypeError: Cannot read properties of undefined (reading 'clear')".
//
// This repo pins Node 24 (.nvmrc), where jsdom DOES provide a working `localStorage`, so the
// guard below is a strict no-op there: it only installs a store when one is missing or broken.
// Several test files carry their own per-`beforeEach` `makeStorage()` shim for the same reason;
// they redefine the (configurable) property with a fresh Map for isolation and keep working.
function makeMemoryStorage(): Storage {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
}

function isWorkingStorage(s: unknown): s is Storage {
  return (
    !!s &&
    typeof (s as Storage).clear === "function" &&
    typeof (s as Storage).getItem === "function"
  );
}

// Idempotent: install an in-memory Storage on `globalThis` only when the built-in one is
// missing/broken. Exported so the regression test can force the "no Storage" condition and
// re-run the guard without depending on the Node version. In jsdom `globalThis` IS `window`,
// so this also satisfies `window.localStorage` accessors (e.g. lib/prefs.ts).
export function ensureWebStorage(): void {
  for (const name of ["localStorage", "sessionStorage"] as const) {
    if (!isWorkingStorage((globalThis as Record<string, unknown>)[name])) {
      Object.defineProperty(globalThis, name, {
        value: makeMemoryStorage(),
        configurable: true,
        writable: true,
      });
    }
  }
}

ensureWebStorage();
