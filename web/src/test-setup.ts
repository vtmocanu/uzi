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
