import { describe, expect, it } from "vitest";
import { MODULE_ICON_NAMES } from "./icons";
// Vite `?raw` imports, for the reason capabilityVocabulary.test.ts documents: web/Dockerfile
// copies only web/ and docs/, `npm run build` type-checks the test files, and a `?raw` path is
// typed by the vite/client wildcard so tsc never resolves it. CI runs this from a full checkout.
import manifestSrc from "./manifest.ts?raw";
import chartTpl from "../../../deploy/chart/templates/_web-modules.tpl?raw";
import generatorSrc from "../../../scripts/web-modules-gen.py?raw";

// Three copies of the module vocabulary exist (PRD #4686): this host (icons.tsx, manifest.ts),
// the Helm chart (_web-modules.tpl) and the compose generator (scripts/web-modules-gen.py). The
// chart and the generator reject what the host would skip, so each list and each character set
// must stay equal; this test is what keeps them so. It reads the two non-TypeScript sources as
// text and extracts the literals, and it fails when it cannot find one (so a reformat of a
// source cannot turn the test into a pass).

function quoted(text: string): string[] {
  return [...text.matchAll(/"([^"]*)"/g)].map((m) => m[1] ?? "");
}

function mustMatch(src: string, re: RegExp, what: string): string {
  const m = re.exec(src);
  if (!m || m[1] === undefined) throw new Error(`parity test: cannot find ${what}; update the extraction here`);
  return m[1];
}

const sorted = (xs: readonly string[]) => [...xs].sort();

const chartIcons = quoted(mustMatch(chartTpl, /define "uzi\.webModules\.icons" -}}\s*\{\{-\s*list ((?:"[^"]*"\s*)+)\|/, "the chart icon list"));
const chartNavGroups = quoted(mustMatch(chartTpl, /has \$m\.navGroup \(list ((?:"[^"]*"\s*)+)\)/, "the chart navGroup list"));
const genIcons = quoted(mustMatch(generatorSrc, /^ICONS = \(([^)]*)\)/m, "the generator ICONS tuple"));
const genNavGroups = quoted(mustMatch(generatorSrc, /^NAV_GROUPS = \(([^)]*)\)/m, "the generator NAV_GROUPS tuple"));
const hostNavGroups = quoted(mustMatch(manifestSrc, /const NAV_GROUPS: readonly string\[\] = \[([^\]]*)\]/, "the host NAV_GROUPS list"));

// A character class body as a canonical list of merged code point ranges. Understands the three
// spellings the sources use: `\p{Cc}`, `\uXXXX` (JavaScript and Python) and `\x{XXXX}` (Go).
function codePoints(body: string): string {
  const CC: Array<[number, number]> = [
    [0x00, 0x1f],
    [0x7f, 0x9f],
  ];
  const tok = /\\p\{Cc\}|\\u([0-9a-fA-F]{4})|\\x\{([0-9a-fA-F]+)\}/y;
  const ranges: Array<[number, number]> = [];
  let i = 0;
  const point = (): number | "Cc" => {
    tok.lastIndex = i;
    const m = tok.exec(body);
    if (!m) throw new Error(`parity test: unreadable character class at "${body.slice(i, i + 12)}"`);
    i = tok.lastIndex;
    if (m[0] === "\\p{Cc}") return "Cc";
    return parseInt((m[1] ?? m[2]) as string, 16);
  };
  while (i < body.length) {
    const a = point();
    if (a === "Cc") {
      ranges.push(...CC);
      continue;
    }
    if (body[i] === "-") {
      i += 1;
      const b = point();
      if (b === "Cc") throw new Error("parity test: \\p{Cc} as a range end");
      ranges.push([a, b]);
    } else {
      ranges.push([a, a]);
    }
  }
  ranges.sort((x, y) => x[0] - y[0]);
  const merged: Array<[number, number]> = [];
  for (const r of ranges) {
    const last = merged[merged.length - 1];
    if (last && r[0] <= last[1] + 1) last[1] = Math.max(last[1], r[1]);
    else merged.push([r[0], r[1]]);
  }
  return merged.map(([a, b]) => `${a.toString(16)}-${b.toString(16)}`).join(",");
}

const hostUnsafe = codePoints(mustMatch(manifestSrc, /const UNSAFE_TEXT_RE =\s*\/\[(.*?)\]\/u;/s, "the host UNSAFE_TEXT_RE"));
const chartUnsafe = codePoints(mustMatch(chartTpl, /\$unsafe := `\[(.*?)\]`/s, "the chart $unsafe"));
const genUnsafe = codePoints(mustMatch(generatorSrc, /^UNSAFE_TEXT_RE = re\.compile\("\[(.*?)\]"\)/ms, "the generator UNSAFE_TEXT_RE"));

describe("module vocabulary parity across the host, the chart and the compose generator", () => {
  it("icon names: the chart and the generator equal MODULE_ICON_NAMES", () => {
    expect(MODULE_ICON_NAMES.length).toBeGreaterThan(10);
    expect(sorted(chartIcons)).toEqual(sorted(MODULE_ICON_NAMES));
    expect(sorted(genIcons)).toEqual(sorted(MODULE_ICON_NAMES));
  });

  it("navGroup set: the chart and the generator equal the host's", () => {
    expect(hostNavGroups.length).toBe(3);
    expect(sorted(chartNavGroups)).toEqual(sorted(hostNavGroups));
    expect(sorted(genNavGroups)).toEqual(sorted(hostNavGroups));
  });

  it("unsafe-text code points (label, version, page): the chart and the generator equal the host's", () => {
    // Sanity on the extraction: a known member and a known non-member.
    expect(hostUnsafe).toContain("ad-ad");
    expect(hostUnsafe).not.toContain("41-41");
    expect(chartUnsafe).toBe(hostUnsafe);
    expect(genUnsafe).toBe(hostUnsafe);
  });

  it("the extraction itself: a different set does not compare equal", () => {
    expect(codePoints("\\p{Cc}\\u200b-\\u200f")).not.toBe(codePoints("\\p{Cc}\\u200b-\\u200e"));
    expect(codePoints("\\x{200B}-\\x{200F}\\u0000-\\u001f")).toBe(codePoints("\\u0000-\\u001f\\u200b-\\u200f"));
  });
});
