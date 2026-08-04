import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In production the SPA is served by nginx, which also proxies /api to the API
// service (same origin, no CORS). The dev proxy below mirrors that so `npm run
// dev` works against a locally running API on :8080. ws: true upgrades the
// /api/ws WebSocket too (nginx does this via its dedicated /api/ws location), so
// the live run view streams under `npm run dev` and not just behind nginx.
export default defineConfig({
  plugins: [react()],
  // Vitest's default testTimeout is 5s, and this project had no `test` block at all — so
  // every one of the 862 tests ran on that default. Measured during PRD #98: under full-suite
  // CPU contention, THREE unrelated tests each timed out once across ~20 runs —
  // mockApi's persistence reload, the Judge nav-badge cross-mount, and Repos' repo-skills
  // opt-in. Different files, different owners, one symptom: "Test timed out in 5000ms",
  // never a wrong assertion.
  //
  // So this is a suite-wide default that is marginal on a loaded machine, not three broken
  // tests, and raising the ceiling here fixes the class instead of patching whichever one
  // surfaced last. It only ever delays a failure — no assertion is loosened, and a passing
  // test costs nothing extra. A 1-in-10 red trains people to re-run instead of read, which is
  // the actual damage.
  // 🔴 20000 WAS NOT A SPARE MARGIN EITHER, AND THE vitest 2 -> 4 MAJOR (PRD #103 M5
  // MR-C) IS WHAT EXPOSED IT. Measured 2026-08-04 on this tree, darwin/arm64,
  // node v26.4.0, five full-suite runs:
  //
  //   vitest 2.1.9, testTimeout 20000     118 files / 1660 tests   GREEN
  //   vitest 4.1.10, testTimeout 20000    118 files / 1660 tests   RED once, green twice
  //   vitest 4.1.10, testTimeout 120000   118 files / 1660 tests   GREEN
  //
  // THE COLLECTED PAIR NEVER MOVED, which is the point of quoting it: the red run
  // collected the same 118/1660 and failed 49 of them, so nothing narrowed and this
  // is a timing fault rather than a suite that lost tests. All 49 were in ONE file,
  // src/pages/RunView.test.tsx, and exactly one of them timed out — the 150-tick
  // fake-timer poll test — with the other 48 the cascade after it, presenting as an
  // EMPTY container ("expected '' to contain …"). That file passes 100/100 when run
  // alone, under vitest 4, with no flag.
  //
  // AND THE FAILING TEST'S OWN STEADY-STATE DURATION IS 3.90s (per-test JSON
  // reporter, whole suite, same host). So it did not get slower, it got STARVED:
  // a >5x spike over its own median under contention. Tuning to "just above the
  // worst observed" is what made agent/package.json's --test-timeout a knife-edge
  // at 30000 (see Taskfile.yml's test:agent comment); 60000 is ~15x the measured
  // steady state and ~3x the spike that actually failed, and the flag's real job is
  // catching a HANG, which is unbounded, so headroom costs a healthy run nothing.
  //
  // What this does NOT fix, stated so nobody reads the green as structural: that one
  // file holds 100 tests, and under any per-file scheduling a large test file is a
  // serialization point no timeout value repairs. Splitting it is the durable fix
  // and is not this MR's scope.
  //
  // Both settings below were CONFIRMED IN EFFECT under vitest 4 rather than inferred
  // from a green — a throwaway test asserted getConfig().asyncUtilTimeout === 5000
  // (so setupFiles ran) and config.testTimeout === 20000 before this change. That
  // matters because vitest 4's `test.projects` inherits NOTHING from this block
  // without `extends`, so whoever adds one must re-assert both rather than trust
  // that a passing suite implies them.
  test: {
    testTimeout: 60000,
    // Raises Testing Library's own 1s async default too — a SEPARATE knob from testTimeout,
    // and the one the Repos/Agents failures were hitting. See src/test-setup.ts.
    setupFiles: ["./src/test-setup.ts"],
  },
  server: {
    // The docs viewer raw-imports repo-root `docs/*.md` (a sibling of `web/`),
    // which lives outside Vite's project root; allow reading the repo root in
    // dev. The production rollup build is unaffected by this.
    fs: { allow: [".."] },
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        ws: true,
      },
    },
  },
});
