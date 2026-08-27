import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

// The barred-CLI list exists in TWO languages: `deniedPackageExecutables` in
// api/internal/toolprofile/toolprofile.go (which the deterministic pre-scan filters on)
// and JUDGE_SYSTEM_PROMPT in agent/src/judge-runner.ts (which is what stops the judge
// MODEL recommending them). They live in different packages and nothing links them.
//
// 🔴 THIS IS THE DEFECT THE MR THAT ADDED THEM EXISTS TO FIX, so it must not recur here.
// A hand-maintained second copy that nobody checks is exactly how the toolchain guard
// came to verify 5 of 13 packages while reporting green. Measured on the first cut of
// the prompt: 21 executables in Go, 17 in the prompt — `kubelogin` and the two gcloud
// credential helpers were missing on day one.
//
// Same trick as guardrails.test.ts's REASON_* scan: read the literals out of the other
// language's source rather than restating them here, which would just be a fourth copy.
describe("judge prompt covers every denylisted executable", () => {
  const goSrc = fs.readFileSync(new URL("../../api/internal/toolprofile/toolprofile.go", import.meta.url), "utf8");
  const promptSrc = fs.readFileSync(new URL("../src/judge-runner.ts", import.meta.url), "utf8");

  /** Executables named in the Go map's VALUE lists (`"pkg": {"a", "b"}`). */
  function goExecutables(): string[] {
    const start = goSrc.indexOf("var deniedPackageExecutables");
    assert.ok(start >= 0, "deniedPackageExecutables not found — did the map move or get renamed?");
    const block = goSrc.slice(start, goSrc.indexOf("\n}\n", start));
    // `:\s*\{` and not a bare `\{`: the first bare match starts at the map literal's OWN
    // opening brace, so it spans `map[string][]string{ "glab": {"glab"` and pulls the KEY
    // into the executable set. Invisible today only because the first entry's key equals
    // its value; a reordered map would make the test demand a package name in the prompt.
    const groups = [...block.matchAll(/:\s*\{([^}]*)\}/g)].map((m) => m[1]!);
    return [...new Set(groups.flatMap((g) => [...g.matchAll(/"([^"]+)"/g)].map((m) => m[1]!)))].sort();
  }

  /** The JUDGE_SYSTEM_PROMPT template literal, isolated so a match elsewhere in the
   *  file (a comment, another prompt) cannot make this pass. */
  function judgePrompt(): string {
    const start = promptSrc.indexOf("const JUDGE_SYSTEM_PROMPT = `");
    assert.ok(start >= 0, "JUDGE_SYSTEM_PROMPT not found — did it move or get renamed?");
    const body = promptSrc.slice(start + "const JUDGE_SYSTEM_PROMPT = `".length);
    const end = body.indexOf("`;");
    assert.ok(end > 0, "could not find the end of the JUDGE_SYSTEM_PROMPT template literal");
    return body.slice(0, end);
  }

  /** The barred-CLI names the prompt actually lists, parsed as a SET.
   *
   *  🔴 NOT a substring test over the whole prompt. That was the first version and it
   *  was partially vacuous: `gh` matches inside `"low|medium|high"`, `op` inside
   *  `propose`, `tea` inside `instead` — so the GitHub CLI could be deleted from the
   *  barred list and this test stayed green. Measured, not theorised. Compare sets. */
  function promptExecutables(): string[] {
    const prompt = judgePrompt();
    const start = prompt.indexOf("NEVER use this category for these credential-bearing CLIs:");
    assert.ok(start >= 0, "the barred-CLI paragraph is not in JUDGE_SYSTEM_PROMPT");
    const end = prompt.indexOf("They are barred by policy", start);
    assert.ok(end > start, "could not find the end of the barred-CLI list");
    const list = prompt.slice(start + "NEVER use this category for these credential-bearing CLIs:".length, end);
    // Strip TRAILING prose punctuation only — the list ends a sentence, so its last
    // entry arrives as `vault.`. Trailing-only matters: `git-credential-gcloud.sh`
    // carries an interior dot that must survive.
    return [
      ...new Set(
        list
          .split(/[\s,]+/)
          .map((t) => t.trim().replace(/[.;:]+$/, ""))
          .filter((t) => t !== ""),
      ),
    ].sort();
  }

  it("names exactly the executables the Go denylist map suppresses", () => {
    const execs = goExecutables();
    // Floor: stops the whole scan passing vacuously if the map is renamed, reformatted
    // into a shape the regex misses, or emptied. The same guard guardrails.test.ts uses.
    assert.ok(execs.length >= 20, `expected >= 20 denied executables, parsed ${execs.length} — did the map's shape change?`);

    const listed = promptExecutables();
    assert.ok(listed.length >= 20, `parsed only ${listed.length} names out of the prompt's list — did its wording change?`);

    // Equality, both directions. Missing means the model is not told a barred CLI is
    // barred; extra means the prompt names something no longer denied.
    assert.deepStrictEqual(
      listed,
      execs,
      `JUDGE_SYSTEM_PROMPT's barred-CLI list has drifted from deniedPackageExecutables in ` +
        `api/internal/toolprofile/toolprofile.go.\n  only in Go:     ${execs.filter((e) => !listed.includes(e)).join(", ") || "(none)"}` +
        `\n  only in prompt: ${listed.filter((e) => !execs.includes(e)).join(", ") || "(none)"}`,
    );
  });
});
