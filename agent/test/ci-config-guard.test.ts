import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  ciConfigPathToRegex,
  flagCIConfigPaths,
  DEFAULT_CI_CONFIG_PATHS,
} from "../src/ci-config-guard.js";

// PRD #71 M5 (load-bearing): the pre-push CI-config guard's dotfile-safe matcher.
// These are the exact examples the milestone pins — the leading-dot defaults are
// the stock-glob trap, and `**/` must match zero directories at the root.
describe("ciConfigPathToRegex", () => {
  it("matches a leading-dot literal path (the stock-glob trap) but nothing else", () => {
    const re = ciConfigPathToRegex(".gitlab-ci.yml");
    assert.ok(re.test(".gitlab-ci.yml"), "the exact dotfile matches");
    assert.ok(!re.test("src/foo.yml"), "an unrelated file does not");
    assert.ok(!re.test("x.gitlab-ci.yml"), "no accidental suffix match");
    // The `.` is a literal, not a regex any-char.
    assert.ok(!re.test("Xgitlab-ci.yml"), "the leading `.` is a literal");
  });

  it("`.gitlab/**` matches the whole subtree, not a sibling that merely shares the prefix", () => {
    const re = ciConfigPathToRegex(".gitlab/**");
    assert.ok(re.test(".gitlab/ci/x.yml"), "a nested file under the dir matches");
    assert.ok(re.test(".gitlab/x"), "a direct child matches");
    assert.ok(!re.test(".gitlabfoo"), "a sibling sharing the prefix does NOT match");
  });

  it("`**/*.gitlab-ci.yml` matches at the root (zero dirs) AND nested", () => {
    const re = ciConfigPathToRegex("**/*.gitlab-ci.yml");
    assert.ok(re.test(".gitlab-ci.yml"), "the `**/` optional prefix matches zero dirs");
    assert.ok(re.test("deep/nested/.gitlab-ci.yml"), "and matches when nested");
    assert.ok(re.test("custom.gitlab-ci.yml"), "the `*` matches a run of non-slash chars");
  });

  it("an arbitrary configured path matches exactly, not a near-miss extension", () => {
    const re = ciConfigPathToRegex("ci/pipeline.yml");
    assert.ok(re.test("ci/pipeline.yml"), "the exact configured path matches");
    assert.ok(!re.test("ci/pipeline.yaml"), ".yaml is NOT .yml");
    assert.ok(!re.test("x/ci/pipeline.yml"), "no unanchored prefix match");
  });

  it("a single `*` is a non-slash run and does not cross a path segment", () => {
    const re = ciConfigPathToRegex(".gitlab/*.yml");
    assert.ok(re.test(".gitlab/ci.yml"), "one segment matches");
    assert.ok(!re.test(".gitlab/ci/x.yml"), "`*` must not span a `/`");
  });

  it("trims surrounding whitespace before anchoring", () => {
    const re = ciConfigPathToRegex("  .gitlab-ci.yml  ");
    assert.ok(re.test(".gitlab-ci.yml"));
  });
});

describe("flagCIConfigPaths", () => {
  const paths = [".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", "ci/pipeline.yml"];

  it("flags every changed file matching ANY configured path", () => {
    const changed = [
      "src/app.ts",
      ".gitlab-ci.yml",
      ".gitlab/ci/build.yml",
      "deep/nested/.gitlab-ci.yml",
      "ci/pipeline.yml",
      "README.md",
    ];
    assert.deepEqual(flagCIConfigPaths(changed, paths), [
      ".gitlab-ci.yml",
      ".gitlab/ci/build.yml",
      "deep/nested/.gitlab-ci.yml",
      "ci/pipeline.yml",
    ]);
  });

  it("de-duplicates repeated hits, preserving input order", () => {
    const changed = [".gitlab-ci.yml", "src/x.ts", ".gitlab-ci.yml"];
    assert.deepEqual(flagCIConfigPaths(changed, paths), [".gitlab-ci.yml"]);
  });

  it("returns [] for a code-only changed-file list", () => {
    const changed = ["src/app.ts", "web/App.tsx", "README.md"];
    assert.deepEqual(flagCIConfigPaths(changed, paths), []);
  });

  it("returns [] on empty inputs and skips blank/whitespace entries", () => {
    assert.deepEqual(flagCIConfigPaths([], paths), []);
    assert.deepEqual(flagCIConfigPaths([".gitlab-ci.yml"], []), []);
    assert.deepEqual(flagCIConfigPaths(["", "   ", ".gitlab-ci.yml"], paths), [
      ".gitlab-ci.yml",
    ]);
  });
});

// PRD #71 M5: the worker-side FLOOR the runner falls back to when a claim omits
// ci_config_paths (a bug or an older server). The whole point is that a missing field
// cannot fail the backstop OPEN — an empty path set flags nothing — so with the floor
// the static CI-config files are still flagged.
describe("DEFAULT_CI_CONFIG_PATHS floor", () => {
  it("still flags .gitlab-ci.yml (and the other static defaults) with no server paths", () => {
    const changed = [
      "src/app.ts",
      ".gitlab-ci.yml",
      ".gitlab/ci/build.yml",
      "deep/nested/.gitlab-ci.yml",
      ".github/workflows/ci.yml",
      "README.md",
    ];
    assert.deepEqual(flagCIConfigPaths(changed, DEFAULT_CI_CONFIG_PATHS), [
      ".gitlab-ci.yml",
      ".gitlab/ci/build.yml",
      "deep/nested/.gitlab-ci.yml",
      ".github/workflows/ci.yml",
    ]);
  });

  it("does not flag a code-only diff even under the floor", () => {
    assert.deepEqual(
      flagCIConfigPaths(["src/app.ts", "web/App.tsx"], DEFAULT_CI_CONFIG_PATHS),
      [],
    );
  });
});
