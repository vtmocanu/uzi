import { describe, it } from "node:test";
import assert from "node:assert/strict";
import crypto from "node:crypto";
import { LiteralIndex } from "../src/redact.js";

// LiteralIndex against a naive reference: every live literal is checked with startsWith at every position.

const KEY = "abababab"; // exactly MIN_SECRET_LEN units
const naiveFind = (live: ReadonlySet<string>, text: string): Array<[number, number]> => {
  const spans: Array<[number, number]> = [];
  for (let i = 0; i < text.length; i++) {
    let best = -1;
    for (const l of live) if (text.startsWith(l, i) && l.length > best) best = l.length;
    if (best >= 0) spans.push([i, i + best]);
  }
  return spans;
};
const naivePresent = (live: ReadonlySet<string>, text: string): Set<string> => {
  const found = new Set<string>();
  for (const l of live) if (text.includes(l)) found.add(l);
  return found;
};

function check(index: LiteralIndex, live: ReadonlySet<string>, text: string, label: string): void {
  assert.deepEqual(index.find(text, { charged: 0 }), naiveFind(live, text), `find ${label}`);
  assert.deepEqual([...index.present(text, { charged: 0 })].sort(), [...naivePresent(live, text)].sort(), `present ${label}`);
  assert.equal(index.size, live.size, `size ${label}`);
}

/** A deterministic PRNG seeded from a hash (xorshift128 over the digest). */
function prng(seed: string): () => number {
  const d = crypto.createHash("sha256").update(seed).digest();
  let a = d.readUInt32LE(0) | 1, b = d.readUInt32LE(4), c = d.readUInt32LE(8), e = d.readUInt32LE(12);
  return () => {
    let t = e;
    const s = a;
    e = c; c = b; b = s;
    t ^= t << 11; t ^= t >>> 8;
    a = t ^ s ^ (s >>> 19);
    return (a >>> 0) / 0x100000000;
  };
}

describe("LiteralIndex agrees with a naive reference", () => {
  it("a literal that is a prefix of another at the same position", () => {
    const index = new LiteralIndex();
    const live = new Set([`${KEY}xy`, `${KEY}xyzzy`, KEY]);
    for (const l of live) index.add(l);
    const text = `..${KEY}xyzzy..${KEY}xy.${KEY}xyz`;
    check(index, live, text, "prefix");
    assert.deepEqual([...index.present(text, { charged: 0 })].sort(), [...live].sort(), "present reports every literal, prefixes included");
    assert.deepEqual(index.find(`${KEY}xyzzy`, { charged: 0 }),[[0, KEY.length + 5]], "find reports the longest only");
  });

  it("a literal of exactly 8 units", () => {
    const index = new LiteralIndex();
    index.add(KEY);
    index.add("short"); // below the floor: ignored
    assert.equal(index.size, 1);
    check(index, new Set([KEY]), `x${KEY}${KEY}y`, "eight");
    index.remove(KEY);
    check(index, new Set(), `x${KEY}y`, "eight removed");
  });

  it("add twice, remove once keeps it; the second remove drops it", () => {
    const index = new LiteralIndex();
    const lit = `${KEY}tail`;
    index.add(lit); index.add(lit);
    index.remove(lit);
    check(index, new Set([lit]), `a${lit}b`, "held once");
    index.remove(lit);
    check(index, new Set(), `a${lit}b`, "dropped");
    index.remove(lit); // removing an absent literal is a no-op
    assert.equal(index.size, 0);
  });

  it("removing a literal that split an edge, then its sibling", () => {
    const index = new LiteralIndex();
    const live = new Set([`${KEY}abcdef`, `${KEY}abcxyz`, `${KEY}abc`]);
    index.add(`${KEY}abcdef`);
    index.add(`${KEY}abcxyz`); // splits the edge at "abc"
    index.add(`${KEY}abc`);
    const text = `${KEY}abcdef|${KEY}abcxyz|${KEY}abc|${KEY}ab`;
    check(index, live, text, "all three");
    index.remove(`${KEY}abc`); live.delete(`${KEY}abc`);
    check(index, live, text, "terminal split node removed");
    index.remove(`${KEY}abcxyz`); live.delete(`${KEY}abcxyz`);
    check(index, live, text, "sibling removed, edge merged");
    index.remove(`${KEY}abcdef`); live.delete(`${KEY}abcdef`);
    check(index, live, text, "empty");
  });

  it("overlapping matches at nearby starts", () => {
    const index = new LiteralIndex();
    const live = new Set(["aaaaaaaab", "aaaaaaaaaa", "aaaaaaaaaab"]);
    for (const l of live) index.add(l);
    check(index, live, "aaaaaaaaaaab aaaaaaaabaaaaaaaaab", "overlap");
  });

  it("non-BMP characters, including edges that split a surrogate pair", () => {
    const index = new LiteralIndex();
    const live = new Set([`${KEY}\u{1F600}x`, `${KEY}\u{1F601}y`, `${KEY}\u{1F600}\u{1F600}`, "\u{1F600}\u{1F600}\u{1F600}\u{1F600}z", "\u{1F600}\u{1F600}\u{1F600}\u{1F600}\u{1F601}"]);
    for (const l of live) index.add(l);
    const text = [...live].join(" ") + " \ud83d lone " + [...live].reverse().join("");
    check(index, live, text, "surrogates");
    index.remove(`${KEY}\u{1F600}x`); live.delete(`${KEY}\u{1F600}x`);
    check(index, live, text, "surrogates after remove");
  });

  it("JSON-escaped forms", () => {
    const index = new LiteralIndex();
    const raw = `${KEY}"quoted"\\path\nline`;
    const escaped = JSON.stringify(raw).slice(1, -1);
    const live = new Set([raw, escaped, `${KEY}"quoted"`]);
    for (const l of live) index.add(l);
    check(index, live, `x ${raw} y ${escaped} z ${JSON.stringify({ k: raw })}`, "json");
  });

  it("randomized add/remove sequences against random texts", () => {
    const pieces = ["a", "b", "ab", "\u{1F600}", "\u{1F601}", "\"", "\\", "\n"];
    const keys = [KEY, "abababac", "\u{1F600}\u{1F600}\u{1F600}\u{1F600}", "babababa"];
    for (let seed = 0; seed < 40; seed++) {
      const rand = prng(`literal-index-${seed}`);
      const pick = <T>(xs: readonly T[]): T => xs[Math.floor(rand() * xs.length)]!;
      const literal = (): string => {
        let l = pick(keys);
        for (let n = Math.floor(rand() * 5); n > 0; n--) l += pick(pieces);
        return l;
      };
      const index = new LiteralIndex();
      const counts = new Map<string, number>();
      for (let step = 0; step < 60; step++) {
        const op = rand();
        const live = new Set(counts.keys());
        if (op < 0.55 || live.size === 0) {
          const l = literal();
          index.add(l);
          counts.set(l, (counts.get(l) ?? 0) + 1);
        } else {
          const l = op < 0.9 ? pick([...live]) : literal();
          index.remove(l);
          const held = counts.get(l);
          if (held !== undefined) { if (held > 1) counts.set(l, held - 1); else counts.delete(l); }
        }
        let text = "";
        for (let n = 0; n < 14; n++) text += rand() < 0.4 ? pick([...counts.keys(), ...keys]) : pick(pieces);
        check(index, new Set(counts.keys()), text, `seed ${seed} step ${step}`);
      }
      for (const [l, held] of counts) for (let n = held; n > 0; n--) index.remove(l);
      assert.equal(index.size, 0, `seed ${seed} drained`);
      assert.deepEqual(index.find(keys.join(""), { charged: 0 }), []);
    }
  });
});
