import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";

// PRD #3244 M2: no observation path reads usage frames. The reporter and the observation adapter
// helpers may not import the usage recorder or either token accountant.

const SRC = path.join(import.meta.dirname, "..", "src");
const FILES = ["lead-config-report.ts", "runtime-observation.ts"];
const FORBIDDEN = [/usage-recorder/, /token-accounting/, /token-accountant/];

function specifiers(source: string): string[] {
  const out: string[] = [];
  for (const m of source.matchAll(/(?:^|\n)\s*(?:import|export)\s[^;]*?from\s+["']([^"']+)["']/g)) out.push(m[1]!);
  for (const m of source.matchAll(/(?:^|[^\w.])(?:import|require)\(\s*["']([^"']+)["']\s*\)/g)) out.push(m[1]!);
  for (const m of source.matchAll(/(?:^|\n)\s*import\s+["']([^"']+)["']/g)) out.push(m[1]!);
  return out;
}

test("the lead-config reporter and observation helpers import no usage or token-accounting module", () => {
  for (const file of FILES) {
    const source = fs.readFileSync(path.join(SRC, file), "utf8");
    const specs = specifiers(source);
    assert.ok(specs.length > 0, `${file}: the scan found its imports`);
    for (const spec of specs) {
      for (const bad of FORBIDDEN) assert.doesNotMatch(spec, bad, `${file} imports ${spec}`);
    }
  }
});

test("the forbidden module names exist, so the scan is guarding something real", () => {
  assert.ok(fs.existsSync(path.join(SRC, "usage-recorder.ts")));
  assert.ok(fs.existsSync(path.join(SRC, "codex", "token-accounting.ts")));
});
