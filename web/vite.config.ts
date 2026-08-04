import { defineConfig } from "vite";
// configDefaults.exclude, so the `jsdom` project below can add ONE exclusion without
// silently dropping vitest's own (node_modules, dist, the *.config.* family). Setting
// `exclude` REPLACES the default list rather than extending it.
import { configDefaults } from "vitest/config";
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
  // 150-tick fake-timer poll test, the other 48 failing in its wake. The collected
  // pair never moved — 118 files / 1660 tests in every run, red and green alike — so
  // nothing narrowed; it is a timing fault. That is still true.
  //
  // "AN EMPTY-CONTAINER CASCADE" IS WHAT THAT SENTENCE USED TO SAY ABOUT THE 48, AND
  // THE LOG DOES NOT SUPPORT IT. Re-derived from the archived run rather than quoted:
  // `probes/prd-103-mrc-tester-tt/conc-c1-2-tt20000.txt` has 49 failure markers and 49
  // FAIL headers, and **zero** occurrences of either "empty container" or "container is
  // empty". The 48 are 23 missing-text and 10 missing-role TestingLibraryElementErrors,
  // 10 `expected '' to contain/match`, 2 `expected null not to be null`, 1 bad-args
  // assertion — and 2 more whose class none of those patterns names, stated rather than
  // rounded away, so the categories deliberately do NOT sum to 48. Breakdown archived at
  // `probes/prd-103-mrc-lead/cascade-characterisation.txt`.
  //
  // The MECHANISM the retired phrase was reaching for is right — one starved test leaves
  // the rest of the file querying components that never rendered — but the phrase names a
  // single uniform signature the run does not have, and a reader who greps for it finds
  // nothing and concludes the record is wrong about the run.
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
  //
  // 🔴 AND THE COMPANION FIGURE, "~15x the measured steady state", WAS RECORDED HERE AS
  // "sound" AND IS REFUTED. It is not wrong arithmetic; it is the wrong denominator.
  // Steady state is not the scale a starvation tail is measured against, and a 73-run
  // study (matched pairs, Clopper-Pearson intervals, ambient load recorded per run —
  // `probes/prd-103-mrc-tester-tt/`) supplies the one that is: against the worst
  // OBSERVED excursion the margin is **1.20x**, not 15x. Both readings, re-derived here
  // from the raw logs rather than carried:
  //
  //   49869 ms  conc-c1... c10-1-tt60000.txt:650  — the largest anywhere. That run was
  //             RED (`Tests  1 failed | 1659 passed`), red for an unrelated IssueView
  //             test; this cap test itself PASSED at 83.1% of the ceiling.
  //   49065 ms  conc-c10-2-tt60000.txt:650        — the largest inside a fully GREEN run
  //             (`Test Files 118 passed`, `Tests 1660 passed`), 81.8% of the ceiling.
  //
  // The brief carried the first as "inside a GREEN run at the shipped cap". The number is
  // right and the run description is not — a distinction that matters here because the
  // green-run reading is the one that argues the ceiling is live rather than merely
  // untested, and it is 49065.
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

    // PRD #103 M6. `provider` is declared rather than left to the default for a
    // reason that is not about coverage at all: M4's `task deadcode:web` runs knip
    // with `"devDependencies": "error"`, a GATING tier, and a devDependency that
    // nothing references is a finding. Measured three-arm control, restored and
    // `git status`-verified — baseline rc=0; `@vitest/coverage-v8` added with no
    // coverage config rc=1 naming `Unused devDependencies (1) @vitest/coverage-v8
    // package.json:39:6`; this block added rc=0 with zero mentions. The alternative
    // was an `ignoreDependencies` entry in knip.jsonc, and it is the wrong fix:
    // declaring the provider makes knip's finding genuinely FALSE, where the ignore
    // would silence a TRUE one.
    //
    // `reporter` is explicit because the DEFAULT WRITES HTML into web/coverage/ —
    // a directory of thousands of files that nobody reads and .gitignore now covers.
    // `text-summary` prints the totals block the GitLab `coverage:` regex in
    // .gitlab-ci.yml reads; `cobertura` is the artifact that annotates the MR diff.
    // NO THRESHOLD (PRD #103 Decision 6): this milestone measures, and a threshold
    // chosen before the number is known is either vacuous or blocks unrelated work.
    coverage: {
      provider: "v8",
      reporter: ["text-summary", "cobertura"],
      reportsDirectory: "./coverage",
    },

    // 🔴 THE jsdom SPLIT (PRD #103 M6). Read the measurement before editing the
    // globs, because the obvious edit is the one that breaks it.
    //
    // WHAT WAS WRONG. Every DOM test opts in per-file with a
    // `// @vitest-environment jsdom` docblock. Measured at 98c99d76: 118 test files
    // under web/src, 76 carrying the pragma, 42 not — and there is no `environment`
    // in this block, so those 42 ran under node. A new test under src/components or
    // src/pages that forgets the pragma therefore runs a DOM test under node, which
    // is usually a loud ReferenceError and is NOT reliably one: src/lib/prefs.ts
    // guards every access with `typeof window === "undefined"`, so a pragma-less
    // test of it passes 2/2 under node while touching no DOM at all.
    //
    // WHY NOT JUST SET environment: "jsdom" HERE. That moves all 42 from node to
    // jsdom — the same silent-wrong-environment bug pointing the other way, which
    // the PRD warns about by name. All 42 are node-side: zero are .tsx and zero
    // import @testing-library/react.
    //
    // WHY THE DIRECTORIES AND NOT THE PRAGMA SET. src/lib (42 files, 6 with the
    // pragma) and src/mocks (14 files, 8 with the pragma) are MIXED, so a
    // directory-keyed split assigns 14 files that run under jsdom today to the node
    // project. That is only safe because of a measurement, not because of a reading:
    //
    //   vitest 4.1.10, four cells, both controls behaving
    //   (probes/prd-103-mrc-m6-coder/b2-vitest4-projects-precedence.{sh,txt}):
    //     projects env=node  + pragma      typeof document = object     <- PRAGMA WINS
    //     projects env=node  + no pragma   typeof document = undefined  <- control
    //     projects env=jsdom + pragma      typeof document = object
    //     projects env=jsdom + no pragma   typeof document = object     <- control
    //
    // => the docblock pragma OUTRANKS a project's `environment` on vitest 4, exactly
    // as it outranked `environmentMatchGlobs` on 2.1.9. The 76 are frozen by their
    // own pragmas; only the 42 are movable by anything in this file. That is what
    // makes the mixed directories harmless HERE — and it is also why this split
    // cannot be verified by reading it.
    //
    // THE ACCEPTANCE CRITERION WAS A PER-FILE CENSUS, NOT A COUNT. All 118 files
    // were enumerated by the environment they ACTUALLY RUN UNDER, before and after,
    // and the two are byte-identical — 76 jsdom, 42 node, same files
    // (probes/prd-103-mrc-m6-coder/b4-census-{before,after}.txt). A wrong partition
    // shows up as a changed line and a file that stopped running shows up as a
    // missing one, so being right about precedence was not required.
    //
    // WHAT IT BUYS, STATED NARROWLY. A new pragma-less test under src/components or
    // src/pages now gets jsdom instead of node. Under src/lib and src/mocks it still
    // gets node and a DOM test there still needs its pragma — those two directories
    // hold every node-side test in the suite, so the alternative was to give up the
    // fix for the other 62 files. It does NOT retire the pragmas; per the table
    // above they outrank this config and remain the per-file authority.
    //
    // 🔴 `extends: true` IS LOAD-BEARING: a project inherits NOTHING from this block
    // without it, and the two things it inherits here are `testTimeout: 20000` and
    // `setupFiles` — a suite-wide timeout and Testing Library's 5s asyncUtilTimeout,
    // both of which this file's own comment above says must be re-asserted rather
    // than trusted. Both were re-asserted under the projects config by the
    // assert-the-wrong-value technique, NOT by a passing suite:
    // probes/prd-103-mrc-m6-coder/b6-projects-inherit.txt.
    projects: [
      {
        extends: true,
        test: {
          name: "jsdom",
          environment: "jsdom",
          // Deliberately the WHOLE of src minus the two node directories, rather
          // than a list of the three DOM ones. A new top-level directory under src/
          // must default INTO this project; enumerating src/components and
          // src/pages would leave its tests collected by no project at all, and a
          // suite that silently stops running a directory is green.
          include: ["src/**/*.test.{ts,tsx}"],
          exclude: [...configDefaults.exclude, "src/lib/**", "src/mocks/**"],
        },
      },
      {
        extends: true,
        test: {
          name: "node",
          environment: "node",
          include: ["src/lib/**/*.test.{ts,tsx}", "src/mocks/**/*.test.{ts,tsx}"],
        },
      },
    ],
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
