import { describe, test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import {
  DEFAULT_CI_CONFIG_PATHS,
  DEFAULT_CI_REPAIR_CONFIG_PATHS,
  ciConfigGuardRegex,
  flagCIConfigPaths,
  flagCIConfigPathsRaw,
} from "../src/ci-config-guard.js";
import { composeCIConfigGuardReason } from "../src/runner.js";

// The guarded raw-path matcher (issue #2865). Every expectation below is string data: the shared
// parity fixture is read by the Go port's test too, and no real CI file is written anywhere.
interface MatchCase { name: string; globs: string[]; path: string; match: boolean }
interface ChangeSet { name: string; globs: string[]; changed: string[]; flagged: string[] }
const fixture = JSON.parse(
  readFileSync(join(import.meta.dirname, "..", "..", "fixtures", "ci-config-globs", "cases.json"), "utf8"),
) as { cases: MatchCase[]; changeSets: ChangeSet[] };

describe("guarded CI-config matcher: shared parity fixture", () => {
  test("the fixture is not empty", () => {
    assert.ok(fixture.cases.length >= 50);
    assert.ok(fixture.changeSets.length >= 5);
  });
  for (const c of fixture.cases) {
    test(`case: ${c.name}`, () => {
      assert.equal(flagCIConfigPathsRaw([c.path], c.globs).length === 1, c.match);
    });
  }
  for (const cs of fixture.changeSets) {
    test(`change set: ${cs.name}`, () => {
      assert.deepEqual(flagCIConfigPathsRaw(cs.changed, cs.globs), cs.flagged);
    });
  }
});

describe("guarded CI-config matcher: contract", () => {
  test("the repair floor is the ci_fix floor plus the forgejo workflow directory", () => {
    assert.deepEqual(DEFAULT_CI_REPAIR_CONFIG_PATHS, [...DEFAULT_CI_CONFIG_PATHS, ".forgejo/workflows/**"]);
    assert.ok(!DEFAULT_CI_CONFIG_PATHS.includes(".forgejo/workflows/**"), "the ci_fix floor is unchanged");
  });

  test("the guarded regex is anchored without the multiline flag", () => {
    const re = ciConfigGuardRegex(".github/workflows/**");
    assert.equal(re.flags.includes("m"), false);
    assert.equal(re.test(".github/workflows/a.yml"), true);
    assert.equal(re.test("x\n.github/workflows/a.yml"), false);
  });

  test("the ci_fix matcher keeps its trim and its line-terminator gap (unchanged)", () => {
    // flagCIConfigPaths is the ci_fix lane's matcher and stays as it was: it trims the path and
    // its `.*` does not cross a newline. The guarded lane must not inherit either behaviour.
    const floor = DEFAULT_CI_REPAIR_CONFIG_PATHS;
    assert.deepEqual(flagCIConfigPaths([" .github/workflows/a.yml"], floor), [".github/workflows/a.yml"]);
    assert.deepEqual(flagCIConfigPaths([".github/workflows/a\n.yml"], floor), []);
    assert.deepEqual(flagCIConfigPathsRaw([".github/workflows/a\n.yml"], floor), [".github/workflows/a\n.yml"]);
  });
});

const hasControl = (text: string): boolean =>
  [...text].some((ch) => {
    const c = ch.codePointAt(0)!;
    return c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x2028 || c === 0x2029;
  });

describe("CI-config guard refusal text", () => {
  test("escapes control characters, quotes each path and never carries a raw line break", () => {
    const reason = composeCIConfigGuardReason([".github/workflows/a\nb\u0007.yml", ".forgejo/workflows/x\u2028y.yml "]);
    assert.equal(hasControl(reason), false);
    assert.match(reason, /"\.github\/workflows\/a\\nb\\u0007\.yml"/);
    assert.match(reason, /"\.forgejo\/workflows\/x\\u2028y\.yml "/);
    assert.match(reason, /reported, not pushed/);
  });

  test("truncates a long path list and keeps the fixed tail within the failure-reason budget", () => {
    const many = Array.from({ length: 40 }, (_, i) => `.github/workflows/${"x".repeat(60)}${i}.yml`);
    const reason = composeCIConfigGuardReason(many);
    assert.ok(reason.length <= 512, `length ${reason.length}`);
    assert.match(reason, /and \d+ more/);
    assert.match(reason, /reported, not pushed/);
    assert.ok(composeCIConfigGuardReason([`.github/workflows/${"y".repeat(500)}`]).length <= 512);
  });

  test("an uncomputable diff names the fail-closed reason", () => {
    assert.match(composeCIConfigGuardReason(null), /could not compute the diff.*failing closed.*reported, not pushed/);
  });
});
