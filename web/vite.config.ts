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
  // 🔴 20000 SURVIVES THE vitest 2 -> 4 MAJOR (PRD #103 M5 MR-C), AND THE TWO TESTS
  // THAT NEEDED MORE GOT A PER-TEST CAP INSTEAD. This comment records a ruling that
  // was made, measured against, and REVERSED inside one MR, because the reversal is
  // the useful part.
  //
  // WHAT HAPPENED. The first vitest 4.1.10 full-suite run was RED: 49 failed, all in
  // src/pages/RunView.test.tsx, exactly one of them a real 20000 ms timeout on a
  // 150-tick fake-timer poll test, the other 48 an empty-container cascade after it.
  // The collected pair never moved — 118 files / 1660 tests in every run, red and
  // green alike — so nothing narrowed; it is a timing fault. That is still true.
  //
  // THE RULING THAT FOLLOWED (raise this to 60000) WAS BUILT ON A RATE THAT DOES NOT
  // REPRODUCE. Three runs gave 1 red; ten further consecutive runs at 20000, in a
  // detached worktree with the value echoed into each log header, gave ZERO. Pooled:
  // 1 red in 13, not 1 in 3. And four of those ten were SLOWER than the 157s red run,
  // which closes the obvious "the machine was just quieter" dismissal. The red was
  // real. The rate was not, and the rate is what argued for tripling the ceiling.
  //
  // AND THE REMEDY THAT RULING FILED CANNOT WORK. "Split the 100-test file" is
  // refuted by its own measurement: per-test JSON durations put the failing test at a
  // 4228 ms mean inside the full 118-file suite and a 5416 ms mean running that file
  // SOLO. Solo is the LIMIT of splitting — zero competing files — and it is slower.
  // The cause is structural and readable at RunView.test.tsx:898 and :943: each awaits
  // advanceTimersByTimeAsync(149 * 4000) inside ONE it(), i.e. 149 sequential event-loop
  // turns each flushing a mocked promise and a React act. That chain moves intact into
  // whatever file it is split into.
  //
  // SO: a per-test timeout on those two (vitest 4 takes it as it()'s third argument),
  // and the other 1658 stay at 20000. Strictly less gate-weakening than tripling the
  // ceiling for everything, and it needs no follow-up issue about file size.
  //
  // ONE FIGURE FROM THE REVERSED VERSION IS NOT DERIVABLE AND IS RECORDED AS SUCH,
  // because it read as a measurement: "60000 is ~3x the spike that actually failed"
  // was wrong in kind. The test was CANCELLED at 20000 ms, so its true duration on
  // that run is bounded only from below and unknown. 60000 is 3x THE CAP THAT FIRED.
  // The companion "~15x the measured steady state" is sound.
  //
  // Both settings below were CONFIRMED IN EFFECT under vitest 4 rather than inferred
  // from a green, and the technique matters more than the result: assert the WRONG
  // value and let the runner print the actual one, because vitest's default reporter
  // suppresses console.log, so a probe that merely LOGS a value records nothing.
  // See probes/prd-103-mrc-coder/c6-config-in-effect.txt. This matters because
  // vitest 4's `test.projects` inherits NOTHING from this block without `extends`, so
  // whoever adds one must re-assert both rather than trust that a passing suite
  // implies them.
  test: {
    testTimeout: 20000,
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
