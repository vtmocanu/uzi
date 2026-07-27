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
    const groups = [...block.matchAll(/\{([^}]*)\}/g)].map((m) => m[1]!);
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

  it("names every executable the Go denylist map suppresses", () => {
    const execs = goExecutables();
    // Floor: stops the whole scan passing vacuously if the map is renamed, reformatted
    // into a shape the regex misses, or emptied. The same guard guardrails.test.ts uses.
    assert.ok(execs.length >= 20, `expected >= 20 denied executables, parsed ${execs.length} — did the map's shape change?`);

    const prompt = judgePrompt();
    const missing = execs.filter((e) => !prompt.includes(e));
    assert.deepStrictEqual(
      missing,
      [],
      `JUDGE_SYSTEM_PROMPT does not name these denylisted executables, so the judge model is ` +
        `not told they are barred and will keep recommending their installation: ${missing.join(", ")}`,
    );
  });
});
