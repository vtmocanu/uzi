// PRD #1648 D8 source guard: no Card call site may pass `p-0` in its className.
// `p-0` never removed Card's padding (cx has no tailwind-merge and Tailwind emits
// .p-0 before .p-5); an edge-to-edge table card uses the `flush` prop instead.
// Reads every non-test .tsx under web/src with node:fs (no module evaluation), so
// a new page is covered without being listed here.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const SRC_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "..");

function tsxSources(): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(SRC_DIR, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile()) continue;
    if (!entry.name.endsWith(".tsx") || entry.name.endsWith(".test.tsx")) continue;
    out.push(resolve(entry.parentPath, entry.name));
  }
  return out.sort();
}

// Every `<Card …>` opening tag whose className string (plain, template or inside
// an expression) carries a `p-0` class token.
function cardP0Offenders(text: string): string[] {
  const offenders: string[] = [];
  for (const m of text.matchAll(/<Card(?=[\s>/])([^>]*)>/g)) {
    const attrs = m[1];
    const cls = attrs.match(/className=(?:"([^"]*)"|\{([^}]*)\})/);
    if (!cls) continue;
    if (/(^|[\s"'`])p-0(?=$|[\s"'`])/.test(cls[1] ?? cls[2] ?? "")) offenders.push(m[0]);
  }
  return offenders;
}

describe("Card p-0 source guard", () => {
  it("detects the retired pattern (guard self-check)", () => {
    expect(cardP0Offenders('<Card className="p-0">')).toHaveLength(1);
    expect(cardP0Offenders('<Card className="overflow-hidden p-0">')).toHaveLength(1);
    expect(cardP0Offenders("<Card className={`p-0 ${x}`}>")).toHaveLength(1);
    expect(cardP0Offenders('<Card flush className="overflow-hidden">')).toHaveLength(0);
    expect(cardP0Offenders('<CardHeader className="p-0">')).toHaveLength(0);
  });

  it("finds no Card call site passing p-0 in web/src", () => {
    const files = tsxSources();
    expect(files.length).toBeGreaterThan(20);
    const hits = files.flatMap((f) =>
      cardP0Offenders(readFileSync(f, "utf8")).map((tag) => `${f.slice(SRC_DIR.length + 1)}: ${tag}`),
    );
    expect(hits).toEqual([]);
  });
});
