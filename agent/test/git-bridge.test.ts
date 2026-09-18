import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";

// PRD #1416 M3 — the ancestry BRIDGE (git.ts bridgeToFloors) and the structure-validated bridge
// DETECTOR (git.ts rangeContainsBridge), exercised over REAL on-disk repos so the commit graph
// (divergent siblings, worker bridges, agent `-s ours` merges, spoofed markers) is genuine. A
// GitCache git op runs in any git dir, so a plain working repo doubles as the "bare" here.

const ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const OID = /^[0-9a-f]{40}$/;

let base: string;
let repo: string;
let git: GitCache;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Write `file`=`content`, stage it and commit on the CURRENT branch; return the new HEAD sha. */
function commit(file: string, content: string, msg: string): string {
  fs.writeFileSync(path.join(repo, file), content);
  gitIn(repo, ["add", "."]);
  gitIn(repo, [...IDENT, "commit", "-m", msg]);
  return gitIn(repo, ["rev-parse", "HEAD"]);
}

/** True when `ancestor` is an ancestor of (or equal to) `descendant`. */
function isAncestor(ancestor: string, descendant: string): boolean {
  try {
    execFileSync("git", ["-C", repo, "merge-base", "--is-ancestor", ancestor, descendant], {
      env: ENV,
    });
    return true;
  } catch {
    return false;
  }
}

beforeEach(() => {
  base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bridge-test-"));
  repo = path.join(base, "repo");
  const dataDir = path.join(base, "data");
  fs.mkdirSync(dataDir);
  execFileSync("git", ["init", "-b", "main", repo], { env: ENV });
  gitIn(repo, ["config", "user.email", "t@t"]);
  gitIn(repo, ["config", "user.name", "t"]);
  gitIn(repo, ["config", "commit.gpgsign", "false"]);
  gitIn(repo, ["config", "maintenance.auto", "false"]);
  gitIn(repo, ["config", "gc.auto", "0"]);
  gitIn(repo, ["config", "core.fsmonitor", "false"]);
  git = new GitCache(dataDir, nullLogger());
});

afterEach(() => fs.rmSync(base, { recursive: true, force: true }));

/**
 * Build the canonical rewrite graph: a root R, a published tip P on top of R, and a DIVERGENT
 * tip H that is a sibling of P (also on R). P is NOT an ancestor of H, so H is "rewritten below P".
 * Leaves the working tree checked out on the `h` branch at H.
 */
function rootPublishedAndDivergent(): { R: string; P: string; H: string } {
  const R = commit("base.txt", "base\n", "root");
  const P = commit("published.txt", "published\n", "published work");
  gitIn(repo, ["checkout", "-b", "h", R]);
  const H = commit("impl.ts", "export const x = 1;\n", "rewritten work");
  return { R, P, H };
}

/**
 * #1416 (MR-rework): bridgeToFloors now returns a three-way tagged result. This helper asserts the
 * BUILT case and unwraps its sha as a plain non-null string, so a "built" call site reads exactly
 * as it did before the return type changed.
 */
async function built(tip: string, floors: string[]): Promise<string> {
  const r = await git.bridgeToFloors(repo, tip, floors);
  assert.strictEqual(r.kind, "built", "expected a built bridge");
  return (r as { kind: "built"; sha: string }).sha;
}

describe("GitCache.bridgeToFloors (PRD #1416 M3)", () => {
  it("wraps a divergent H in B whose tree === H's tree byte-for-byte, with P and H both ancestors", async () => {
    const { P, H } = rootPublishedAndDivergent();
    const B = await built(H, [P]);
    assert.ok(OID.test(B), "a bridge sha was returned");
    // B's tree is byte-identical to H's tree.
    assert.strictEqual(
      gitIn(repo, ["rev-parse", `${B}^{tree}`]),
      gitIn(repo, ["rev-parse", `${H}^{tree}`]),
      "B's tree === H's tree",
    );
    // Both the published floor P and H are ancestors of B → a plain fast-forward push lands over P.
    assert.ok(isAncestor(P!, B), "P is an ancestor of B");
    assert.ok(isAncestor(H, B), "H is an ancestor of B");
    // H is the FIRST parent (so `git log --first-parent` reads as the agent's history).
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^1`]), H, "H is B's first parent");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^2`]), P, "P is B's second parent");
  });

  it("uses the EXACT marker message naming P (floors[0]) and H", async () => {
    const { P, H } = rootPublishedAndDivergent();
    const B = await built(H, [P]);
    const msg = gitIn(repo, ["log", "-1", "--format=%B", B]);
    assert.strictEqual(
      msg,
      `bridge: restore published tip ${P} as an ancestor of ${H} (tree unchanged)`,
    );
  });

  it("is DETERMINISTIC: the same inputs yield the same sha", async () => {
    const { P, H } = rootPublishedAndDivergent();
    const B1 = await built(H, [P]);
    const B2 = await built(H, [P]);
    assert.strictEqual(B1, B2, "a re-bridge of the same H over the same floors is byte-identical");
  });

  it("returns { kind: 'noop' } when no floor is missing (P already an ancestor of the tip)", async () => {
    const R = commit("base.txt", "base\n", "root");
    const P = commit("published.txt", "p\n", "published");
    const onTop = commit("more.ts", "1\n", "work on top of P"); // descends from P
    assert.ok(isAncestor(P, onTop));
    // #1416 (MR-rework): a genuine no-op (a fast-forward, nothing missing) is a distinct tag from a
    // failure, so a fast-forwardable run is NOT reported as history_rewritten.
    assert.deepStrictEqual(
      await git.bridgeToFloors(repo, onTop, [P]),
      { kind: "noop" },
      "nothing to bridge → noop, not failed",
    );
    void R;
  });

  it("appends only the floors actually missing (C-only: P an ancestor, C divergent)", async () => {
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    const C = commit("c.txt", "c\n", "checkpoint above P"); // C descends from P
    gitIn(repo, ["checkout", "-b", "h", P]);
    const H = commit("impl.ts", "1\n", "work on P but diverging from C"); // descends from P, not C
    const B = await built(H, [P, C]);
    assert.ok(OID.test(B));
    // P is already an ancestor of H → NOT a bridge parent; only the missing C is appended.
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^1`]), H, "first parent is H");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^2`]), C, "the only extra parent is C");
    assert.throws(() => gitIn(repo, ["rev-parse", `${B}^3`]), "no third parent (P was not missing)");
    // The marker still names P (floors[0]).
    assert.match(
      gitIn(repo, ["log", "-1", "--format=%B", B]),
      new RegExp(`restore published tip ${P} `),
    );
    void R;
  });

  it("returns { kind: 'failed' } on a malformed tip or empty floors (never throws)", async () => {
    const { H } = rootPublishedAndDivergent();
    // #1416 (MR-rework): malformed input is "failed", never conflated with a clean no-op.
    assert.deepStrictEqual(await git.bridgeToFloors(repo, "not-an-oid", ["a".repeat(40)]), {
      kind: "failed",
    });
    assert.deepStrictEqual(await git.bridgeToFloors(repo, H, []), { kind: "failed" });
    assert.deepStrictEqual(await git.bridgeToFloors(repo, H, ["nope"]), { kind: "failed" });
  });

  it("is IDEMPOTENT on an idle re-bridge: a prior bridge B1 passed as an additional floor is SKIPPED (no nesting)", async () => {
    // FIX 1 (#1416) — the nested-bridge accumulation guard. B1 bridges the divergent H over [P].
    const { P, H } = rootPublishedAndDivergent();
    const B1 = await built(H, [P]);
    assert.ok(OID.test(B1));
    // A later idle checkpoint tick re-bridges the SAME unchanged H, now with C === B1 as an
    // additional floor. B1's tree === H's tree, so it contributes no unique content and is skipped —
    // the result is byte-identical to B1, NOT a new nested commit.
    const B2 = await built(H, [P, B1]);
    assert.strictEqual(B2, B1, "an idle re-bridge of an unchanged H yields the identical B (no growth)");
    // B2 has EXACTLY the two parents H and P — B1 was NOT appended as a third parent.
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B2}^1`]), H, "first parent is H");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B2}^2`]), P, "second parent is P");
    assert.throws(() => gitIn(repo, ["rev-parse", `${B2}^3`]), "no third parent (B1 skipped as tree-equal)");
  });

  it("NEVER skips the published floor floors[0] on the tree-equal basis (P stays an ancestor of B)", async () => {
    // A published floor P whose tree coincides with H's tree must STILL be appended: build P as a
    // tree-preserving sibling of the root so P^{tree} === H^{tree}, then bridge H over [P].
    const R = commit("base.txt", "base\n", "root");
    const rootTree = gitIn(repo, ["rev-parse", `${R}^{tree}`]);
    // P: an empty commit on R (tree unchanged = rootTree), a sibling-less advance so it is a real tip.
    gitIn(repo, [...IDENT, "commit", "--allow-empty", "-m", "published (tree unchanged)"]);
    const P = gitIn(repo, ["rev-parse", "HEAD"]);
    // H: a DIVERGENT sibling of P on R that ALSO leaves the tree unchanged (empty commit) → H^{tree}
    // === rootTree === P^{tree}, yet P is not an ancestor of H.
    gitIn(repo, ["checkout", "-b", "h", R]);
    gitIn(repo, [...IDENT, "commit", "--allow-empty", "-m", "divergent (tree unchanged)"]);
    const H = gitIn(repo, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(repo, ["rev-parse", `${H}^{tree}`]), rootTree, "H's tree === root tree");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${P}^{tree}`]), rootTree, "P's tree === root tree");
    const B = await built(H, [P]);
    assert.ok(OID.test(B), "a bridge is built even though P's tree coincides with H's");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^2`]), P, "P (floors[0]) is still an extra parent");
    assert.ok(isAncestor(P, B), "P remains an ancestor of B");
    assert.ok(isAncestor(H, B), "H remains an ancestor of B");
  });

  it("still appends an ADDITIONAL floor whose tree DIFFERS from the tip's tree", async () => {
    // The complement of the idempotency guard: a floor with genuinely different tree content must
    // NOT be skipped — this is the legitimate one-per-rewrite chaining the plan permits.
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    const C = commit("c.txt", "c\n", "checkpoint above P"); // C's tree adds c.txt → differs from H's
    gitIn(repo, ["checkout", "-b", "h", R]);
    const H = commit("impl.ts", "1\n", "diverges below P");
    const B = await built(H, [P, C]);
    assert.ok(OID.test(B));
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^2`]), P, "P appended");
    assert.strictEqual(
      gitIn(repo, ["rev-parse", `${B}^3`]),
      C,
      "the tree-differing additional floor C is still appended",
    );
  });

  it("RETAINS a GENUINE divergent sibling C whose tree coincidentally equals H's tree (FIX finding 2)", async () => {
    // The reviewer's exact scenario: C is a REAL commit (not a worker bridge), a divergent SIBLING of
    // H that does NOT descend from H, whose tree coincidentally equals H's. Equal trees do NOT imply
    // equal histories, so C MUST stay a parent of B (its lineage remains an ancestor of B) — the old
    // tree-equality skip wrongly dropped it. Build C and H as siblings on R that add the SAME
    // file+content, so C^{tree} === H^{tree} while neither is an ancestor of the other.
    const R = commit("base.txt", "base\n", "root");
    const P = commit("published.txt", "published\n", "published work"); // a real published tip above R
    gitIn(repo, ["checkout", "-b", "c", R]);
    const C = commit("shared.ts", "same\n", "genuine checkpoint sibling"); // divergent sibling of H
    gitIn(repo, ["checkout", "-b", "h", R]);
    const H = commit("shared.ts", "same\n", "rewritten work"); // same file+content → same tree as C
    assert.strictEqual(
      gitIn(repo, ["rev-parse", `${C}^{tree}`]),
      gitIn(repo, ["rev-parse", `${H}^{tree}`]),
      "C's tree coincidentally equals H's tree",
    );
    assert.ok(!isAncestor(C, H) && !isAncestor(H, C), "C and H are divergent siblings, neither an ancestor");
    const B = await built(H, [P, C]);
    assert.ok(OID.test(B), "a bridge is built");
    // C is RETAINED as an extra parent even though its tree equals H's → C's lineage is an ancestor of B.
    assert.ok(isAncestor(C, B), "C (a genuine divergent sibling with an equal tree) is an ancestor of B");
    assert.ok(isAncestor(P, B), "P is an ancestor of B");
    assert.ok(isAncestor(H, B), "H is an ancestor of B");
    const parents = gitIn(repo, ["rev-list", "--parents", "-n", "1", B]).split(/\s+/).slice(1);
    assert.ok(parents.includes(C), "C is among B's parents");
    void R;
  });

  it("is IDEMPOTENT across ticks: a prior bridge B1 over a DISTINCT-tree C is returned unchanged and still covers C", async () => {
    // Multi-tick idempotency with a GENUINE, distinct-tree checkpoint C. B1 bridges the divergent H
    // over [P, C_genuine], so B1's parents are [H, P, C]. A later idle re-bridge over [P, B1] must
    // return B1 UNCHANGED (the superset-check sees B1 already descends from H, preserves H's tree, and
    // covers P) — and B1 still carries C_genuine as an ancestor, so no lineage is lost.
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    gitIn(repo, ["checkout", "-b", "c", R]);
    const C = commit("c.txt", "c\n", "genuine checkpoint sibling"); // distinct tree (adds c.txt)
    gitIn(repo, ["checkout", "-b", "h", R]);
    const H = commit("impl.ts", "1\n", "rewritten work"); // distinct tree (adds impl.ts)
    const B1 = await built(H, [P, C]);
    assert.ok(OID.test(B1));
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B1}^1`]), H, "B1's first parent is H");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B1}^2`]), P, "B1's second parent is P");
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B1}^3`]), C, "B1's third parent is the genuine C");
    assert.ok(isAncestor(C, B1), "C is an ancestor of B1");
    // A later idle re-bridge over [P, B1] returns B1 unchanged (true superset idempotency).
    const B2 = await built(H, [P, B1]);
    assert.strictEqual(B2, B1, "an idle re-bridge over [P, B1] returns B1 unchanged (no nesting)");
    assert.ok(isAncestor(C, B2), "C_genuine remains an ancestor of the returned bridge");
    void R;
  });
});

describe("GitCache.rangeContainsBridge (PRD #1416 M3 — the structure-validated detector)", () => {
  it("ACCEPTS a P-only worker bridge", async () => {
    const { P, H } = rootPublishedAndDivergent();
    const B = await built(H, [P]);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), true);
  });

  it("ACCEPTS a C-only worker bridge", async () => {
    const P = (() => {
      commit("base.txt", "base\n", "root");
      return commit("p.txt", "p\n", "published");
    })();
    const C = commit("c.txt", "c\n", "checkpoint above P");
    gitIn(repo, ["checkout", "-b", "h", P]);
    const H = commit("impl.ts", "1\n", "on P, diverging from C");
    const B = await built(H, [P, C]);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), true);
  });

  it("ACCEPTS a P+C worker bridge", async () => {
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    const C = commit("c.txt", "c\n", "checkpoint above P");
    gitIn(repo, ["checkout", "-b", "h", R]);
    const H = commit("impl.ts", "1\n", "diverges below P");
    const B = await built(H, [P, C]);
    assert.strictEqual(gitIn(repo, ["rev-parse", `${B}^3`]), C, "both P and C are extra parents");
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), true);
  });

  it("ACCEPTS an agent `git merge -s ours P` bridge (no marker)", async () => {
    const { P } = rootPublishedAndDivergent(); // working tree on `h` at H
    gitIn(repo, [...IDENT, "merge", "-s", "ours", P, "-m", "restore published tip"]);
    const B = gitIn(repo, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(repo, ["log", "-1", "--format=%B", B]).includes("bridge: restore"), false);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), true);
  });

  it("REJECTS an unrelated `-s ours` merge (extra parent ≠ P)", async () => {
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    gitIn(repo, ["checkout", "-b", "q", R]);
    const Q = commit("q.txt", "q\n", "unrelated sibling");
    gitIn(repo, ["checkout", "-b", "h", R]);
    commit("impl.ts", "1\n", "diverges below P");
    gitIn(repo, [...IDENT, "merge", "-s", "ours", Q, "-m", "merge unrelated"]);
    const B = gitIn(repo, ["rev-parse", "HEAD"]);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), false, "extra parent is Q, not P");
  });

  it("REJECTS a merged default-history merge commit (tree ≠ first-parent tree; no spoof/inflation under --first-parent)", async () => {
    commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    // D is a default-branch sibling that adds its own file (so a merge tree differs from H's).
    gitIn(repo, ["checkout", "-b", "d", P]);
    const D = commit("default.txt", "from default\n", "default history advances");
    gitIn(repo, ["checkout", "-b", "h", P]);
    commit("impl.ts", "1\n", "agent work on P");
    gitIn(repo, [...IDENT, "merge", "--no-edit", D]); // ordinary merge → tree combines H and D
    const M = gitIn(repo, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(
      gitIn(repo, ["rev-parse", `${M}^{tree}`]),
      gitIn(repo, ["rev-parse", `${M}^1^{tree}`]),
      "the merge tree differs from its first parent's tree",
    );
    assert.strictEqual(await git.rangeContainsBridge(repo, P, M), false);
  });

  it("REJECTS a commit carrying the marker TEXT but a WRONG X (published tip mismatch)", async () => {
    const { R, P, H } = rootPublishedAndDivergent();
    // Structurally a valid bridge (tree = H's, extra parent = P) but the marker names R, not P.
    const tree = gitIn(repo, ["rev-parse", `${H}^{tree}`]);
    const spoof = gitIn(repo, [
      ...IDENT,
      "commit-tree",
      tree,
      "-p",
      H,
      "-p",
      P,
      "-m",
      `bridge: restore published tip ${R} as an ancestor of ${H} (tree unchanged)`,
    ]);
    assert.strictEqual(
      await git.rangeContainsBridge(repo, P, spoof),
      false,
      "a marker naming the wrong published tip is rejected, never re-checked as the markerless form",
    );
  });

  it("REJECTS a commit carrying the marker TEXT but tree ≠ first-parent tree", async () => {
    const { P, H } = rootPublishedAndDivergent();
    // Marker names P correctly, extra parent is P, but the tree is P's (not H's) → invalid.
    const wrongTree = gitIn(repo, ["rev-parse", `${P}^{tree}`]);
    const spoof = gitIn(repo, [
      ...IDENT,
      "commit-tree",
      wrongTree,
      "-p",
      H,
      "-p",
      P,
      "-m",
      `bridge: restore published tip ${P} as an ancestor of ${H} (tree unchanged)`,
    ]);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, spoof), false);
  });

  it("returns false (never throws) on a malformed publishedTip/pushedTip", async () => {
    const { P, H } = rootPublishedAndDivergent();
    assert.strictEqual(await git.rangeContainsBridge(repo, "nope", H), false);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, "nope"), false);
    assert.strictEqual(await git.rangeContainsBridge(repo, P, "f".repeat(40)), false);
  });

  it("returns false on an empty range (nothing was bridged)", async () => {
    const P = (() => {
      commit("base.txt", "base\n", "root");
      return commit("p.txt", "p\n", "published");
    })();
    const H = commit("impl.ts", "1\n", "clean fast-forward work"); // descends from P, no bridge
    assert.strictEqual(await git.rangeContainsBridge(repo, P, H), false);
  });

  it("is NOT fooled by a 0x1e byte embedded in a commit body (NUL-separated parse — FIX 5, #1416)", async () => {
    // git round-trips 0x1e (the old record separator) AND 0x1f (the old field separator) in a commit
    // MESSAGE, so a crafted body could inject a phantom record under the OLD 0x1e/0x1f-split parse.
    // The NUL-based parse cannot be split by any byte a body can contain, so the phantom is inert.
    // Built from \u escapes at RUNTIME — never a raw control byte in THIS source.
    const RS = "\u001e";
    const US = "\u001f";
    const R = commit("base.txt", "base\n", "root");
    const P = commit("p.txt", "p\n", "published");
    // D: a divergent sibling of P whose tree the phantom masquerades as a bridge over.
    gitIn(repo, ["checkout", "-b", "d", R]);
    const D = commit("d.ts", "1\n", "divergent sibling D");
    const dTree = gitIn(repo, ["rev-parse", `${D}^{tree}`]);
    // M: the carrier — a divergent sibling of P (so P..M = {M}) whose BODY embeds a phantom bridge
    // record. Under the OLD parse the injected 0x1e would start a fake record that parses as a valid
    // worker bridge (tree === D's; extra parent P descends from P; P not an ancestor of D) → true,
    // fooled. Under the NUL parse it all stays inside M's single body field, and M itself is not a
    // bridge (its own tree differs from its parent's) → false.
    gitIn(repo, ["checkout", "-b", "m", R]);
    fs.writeFileSync(path.join(repo, "m.ts"), "1\n");
    gitIn(repo, ["add", "."]);
    const phantomBody =
      `real work${RS}junkH${US}${dTree}${US}${D} ${P}${US}` +
      `bridge: restore published tip ${P} as an ancestor of ${D} (tree unchanged)`;
    gitIn(repo, [...IDENT, "commit", "-m", phantomBody]);
    const M = gitIn(repo, ["rev-parse", "HEAD"]);
    // Sanity: git preserved the 0x1e byte in the stored body (the exact case the fix hardens).
    assert.ok(
      gitIn(repo, ["log", "-1", "--format=%B", M]).includes(RS),
      "the 0x1e byte round-tripped into the stored body",
    );
    assert.strictEqual(
      await git.rangeContainsBridge(repo, P, M),
      false,
      "the phantom record injected via 0x1e does not fool the NUL-separated parse",
    );
  });

  it("ACCEPTS a genuine agent bridge whose body CONTAINS a 0x1e byte (parse not corrupted — FIX 5)", async () => {
    const RS = "\u001e"; // runtime byte from a printable escape
    const { P } = rootPublishedAndDivergent(); // working tree on `h` at H
    // A genuine agent `-s ours` bridge, but with a 0x1e byte in its message: the NUL parse keeps the
    // whole body in one field, so the bridge is still detected by its structure (tree + extra parent).
    gitIn(repo, [...IDENT, "merge", "-s", "ours", P, "-m", `restore${RS}published tip`]);
    const B = gitIn(repo, ["rev-parse", "HEAD"]);
    assert.ok(
      gitIn(repo, ["log", "-1", "--format=%B", B]).includes(RS),
      "the 0x1e byte round-tripped into the stored body",
    );
    assert.strictEqual(await git.rangeContainsBridge(repo, P, B), true);
  });
});
