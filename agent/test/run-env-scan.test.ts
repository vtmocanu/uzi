import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, RECOVERY_BUNDLE_REF } from "../src/git.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
import { type ScanNeedle, runEnvClaimForms } from "../src/run-env-forms.js";
import {
  FORMS_OVER_BUDGET, hitVariableNames, patchImages, patchScanVerdict, resolveScanNeedles, scanCoversCandidate, scanRunEnvRange, textHitVariables, type RunEnvScanVerdict,
} from "../src/run-env-scan.js";
import { renderSecretFinding } from "../src/secret-finding-render.js";

// PRD 4416 M2: the fail-closed exact-value scan of a run's secret variables. Real git throughout; every
// value is an obvious dummy assembled at runtime and lives only in these throwaway fixture repos.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const SECRET = ["rv-Qk7", "Zp2m", "Xw9d", "Lt4s"].join("-");
const QUOTED = ["pq\"R7", "zN4x", "vB8k", "mW2c"].join("");
const PLAIN = ["plain-visible", "value", "9876"].join("-");

const secretVar = (name: string, value: string, secret = true): ClaimRunEnvVar =>
  ({ var_id: UUID, scope: "repo", rev: 1, name, value, secret, description: "" });
const retired = (name: string, value: string): ClaimRunEnvRedact => ({ var_id: UUID, scope: "repo", rev: 1, name, value });
const NEEDLES: ScanNeedle[] = runEnvClaimForms([secretVar("DB_PASSWORD", SECRET), secretVar("QUOTED_SECRET", QUOTED)], []).needles;

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
}
function commitFile(repo: string, name: string, content: string | Buffer): string {
  fs.mkdirSync(path.dirname(path.join(repo, name)), { recursive: true });
  fs.writeFileSync(path.join(repo, name), content);
  git(repo, "add", "-A");
  git(repo, "-c", "commit.gpgsign=false", "commit", "-m", `edit ${path.basename(name).slice(0, 8)}`);
  return git(repo, "rev-parse", "HEAD");
}

let fx: Fixture;
let cache: GitCache;
let bare: string;
let work: string;
let base: string;

function syncFeature(): void {
  git(bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*");
  git(bare, "update-ref", "refs/uzi-test/feature", git(work, "rev-parse", "feature"));
}
const scan = (tip: string, excludes: string[], needles: readonly ScanNeedle[] = NEEDLES, limits: { deadlineMs?: number; maxBytes?: number } = {}) =>
  scanRunEnvRange(cache, bare, { tip, excludes }, needles, { deadlineMs: limits.deadlineMs ?? 30_000, ...(limits.maxBytes === undefined ? {} : { maxBytes: limits.maxBytes }) });

function assertNoValue(label: string, value: unknown): void {
  const text = JSON.stringify(value);
  for (const needle of NEEDLES) for (const form of needle.forms) assert.ok(!text.includes(form), `${label}: no form of a value appears`);
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  work = fx.originPath;
  git(work, "config", "user.email", "t@t");
  git(work, "config", "user.name", "t");
  base = git(work, "rev-parse", "main");
  git(work, "checkout", "-q", "-b", "feature");
});
afterEach(() => fx.cleanup());

describe("scanRunEnvRange over a real range", () => {
  it("holds a value that sits in a MIDDLE commit and is gone from the tip tree", async () => {
    commitFile(work, "a.txt", "ordinary\n");
    const middle = commitFile(work, "src/conf.txt", `db=${SECRET}\n`);
    commitFile(work, "src/conf.txt", "db=redacted\n");
    const tip = commitFile(work, "z.txt", "later\n");
    syncFeature();
    const v = await scan(tip, [base]);
    assert.equal(v.kind, "hit");
    if (v.kind !== "hit") return;
    assert.equal(v.findings.length, 1);
    assert.equal(v.findings[0]!.ruleId, "uzi-run-env");
    assert.equal(v.findings[0]!.variable, "DB_PASSWORD");
    assert.equal(v.findings[0]!.commit, middle);
    assert.deepEqual(hitVariableNames(v.findings), ["DB_PASSWORD"]);
    assertNoValue("verdict", v);
    assert.match(renderSecretFinding(v.findings[0]!), /rule uzi-run-env, variable DB_PASSWORD/);
  });

  it("holds a value in an EMPTY commit's message", async () => {
    git(work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", `note ${SECRET} tail`);
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    const v = await scan(tip, [base]);
    assert.equal(v.kind, "hit");
    if (v.kind === "hit") assert.equal(v.findings[0]!.commit, tip);
  });

  it("holds the base64 form inside a docker config auth entry, and the JSON-escaped form of a quoted value", async () => {
    const auth = Buffer.from(`ci-user:${SECRET}`).toString("base64");
    commitFile(work, ".docker/config.json", JSON.stringify({ auths: { "registry.example.test": { auth } } }));
    const tip = commitFile(work, "data.json", JSON.stringify({ value: QUOTED }));
    syncFeature();
    const v = await scan(tip, [base]);
    assert.equal(v.kind, "hit");
    if (v.kind !== "hit") return;
    assert.deepEqual(hitVariableNames(v.findings).sort(), ["DB_PASSWORD", "QUOTED_SECRET"]);
    assertNoValue("verdict", v);
  });

  it("holds a retired version delivered only through run_env_redact", async () => {
    const tip = commitFile(work, "old.txt", `was ${SECRET}\n`);
    syncFeature();
    const needles = runEnvClaimForms([], [retired("DB_PASSWORD", SECRET)]).needles;
    assert.equal((await scan(tip, [base], needles)).kind, "hit");
  });

  it("holds NOTHING for a plain value in the tree: plain values are never needles", async () => {
    const tip = commitFile(work, "plain.txt", `region=${PLAIN}\n`);
    syncFeature();
    const needles = runEnvClaimForms([secretVar("REGION", PLAIN, false), secretVar("DB_PASSWORD", SECRET)], []).needles;
    assert.deepEqual(needles.map((n) => n.name), ["DB_PASSWORD"]);
    const v = await scan(tip, [base], needles);
    assert.deepEqual(v, { kind: "clean", range: { tip, excludes: [base] } });
  });

  it("is clean for an ordinary range and binds the verdict to the scanned tip and excludes", async () => {
    const tip = commitFile(work, "a.txt", "nothing to see\n");
    syncFeature();
    const v = await scan(tip, [base]);
    assert.equal(v.kind, "clean");
    assert.equal(scanCoversCandidate([v], { tip }), true);
    assert.equal(scanCoversCandidate([v], { tip, excludes: [base] }), true);
  });

  it("with no needles it is clean and spawns no git child at all", async (t) => {
    const tip = commitFile(work, "a.txt", `${SECRET}\n`);
    syncFeature();
    const exec = t.mock.method(cache as unknown as { execScoped: () => unknown }, "execScoped");
    const spawn = t.mock.method(cache as unknown as { spawnGit: () => unknown }, "spawnGit");
    assert.equal((await scan(tip, [base], [])).kind, "clean");
    assert.equal(exec.mock.callCount() + spawn.mock.callCount(), 0);
  });
});

describe("scanRunEnvRange when the committed bytes differ from the value in its line endings", () => {
  const BODY = ["Zx3-line-one-dummy-0001", "Qm8-line-two-dummy-0002", "Ty5-line-three-dummy-0003"];
  const ARMOR_BEGIN = ["-----BEGIN ", "DUMMY KEY", "-----"].join("");
  const ARMOR_END = ["-----END ", "DUMMY KEY", "-----"].join("");
  const MULTI = [ARMOR_BEGIN, ...BODY, ARMOR_END];

  async function verdictFor(value: string, committed: string): Promise<RunEnvScanVerdict> {
    const tip = commitFile(work, "k.pem", committed);
    syncFeature();
    return scan(tip, [base], runEnvClaimForms([secretVar("SIGNING_KEY", value)], []).needles);
  }
  const hitNames = (v: RunEnvScanVerdict): string[] => (v.kind === "hit" ? hitVariableNames(v.findings) : [`NOT a hit: ${v.kind}`]);

  it("a multi-line value stored with a trailing line break is held when committed without it", async () => {
    assert.deepEqual(hitNames(await verdictFor(`${MULTI.join("\n")}\n`, MULTI.join("\n"))), ["SIGNING_KEY"]);
  });

  it("a CRLF value is held when committed with LF line endings", async () => {
    assert.deepEqual(hitNames(await verdictFor(MULTI.join("\r\n"), `${MULTI.join("\n")}\n`)), ["SIGNING_KEY"]);
  });

  it("a single-line token stored with a trailing line break is held when committed bare", async () => {
    assert.deepEqual(hitNames(await verdictFor(`${SECRET}\n`, `token=${SECRET}`)), ["SIGNING_KEY"]);
  });

  it("a CRLF value with a trailing CRLF is held when committed LF and bare", async () => {
    assert.deepEqual(hitNames(await verdictFor(`${MULTI.join("\r\n")}\r\n`, MULTI.join("\n"))), ["SIGNING_KEY"]);
  });

  it("the redaction forms cover the same variants", () => {
    const forms = runEnvClaimForms([secretVar("SIGNING_KEY", `${SECRET}\n`)], []).redaction;
    assert.ok(forms.includes(SECRET));
  });
});

describe("fail closed: anything that cannot be proven clean is a withhold, never a clean", () => {
  const withheld = (v: RunEnvScanVerdict): string => (v.kind === "withhold" ? v.reason : `NOT withheld: ${v.kind}`);

  it("an unresolved ref", async () => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    // The path pass (the first git work of a scan) meets the unresolvable revision first.
    assert.equal(withheld(await scan(tip, ["f".repeat(40)])), "path_scan_incomplete");
    assert.equal(withheld(await scan("f".repeat(40), [base])), "path_scan_incomplete");
  });

  it("a malformed range", async () => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    assert.equal(withheld(await scan("not-a-sha", [base])), "range_invalid");
    assert.equal(withheld(await scan(tip, ["HEAD"])), "range_invalid");
  });

  it("a git error (a bare that does not exist)", async () => {
    const tip = commitFile(work, "a.txt", "a\n");
    const v = await scanRunEnvRange(cache, path.join(fx.dataDir, "no-such-bare"), { tip, excludes: [base] }, NEEDLES, { deadlineMs: 30_000 });
    assert.equal(withheld(v), "path_scan_incomplete");
  });

  it("a timeout of the git child", async (t) => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    t.mock.method(cache as unknown as { execScoped: () => Promise<never> }, "execScoped", async () => {
      throw Object.assign(new Error("Command failed: timed out"), { killed: true, signal: "SIGTERM" });
    });
    assert.equal(withheld(await scan(tip, [base])), "path_scan_incomplete");
  });

  it("more bytes than maxBytes (counted over the objects) and more objects than the cap", async () => {
    commitFile(work, "a.txt", "a\n");
    const tip = commitFile(work, "b.txt", "b\n");
    syncFeature();
    assert.equal(withheld(await scan(tip, [base], NEEDLES, { maxBytes: 1 })), "scan_too_large");
    const res = await cache.scanObjectsForRunEnv(bare, { tip, excludes: [base] }, NEEDLES, { caps: { maxObjects: 1 } });
    assert.equal(res.trusted, false);
  });

  it("a deadline already spent", async () => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    const v = await scan(tip, [base], NEEDLES, { deadlineMs: 0 });
    assert.notEqual(v.kind, "clean", "a spent deadline never reads as clean");
  });

  it("a candidate that differs from the scanned one is refused", async () => {
    const first = commitFile(work, "a.txt", "a\n");
    syncFeature();
    const second = commitFile(work, "b.txt", "b\n");
    const v = await scan(first, [base]);
    assert.equal(scanCoversCandidate([v], { tip: first }), true);
    assert.equal(scanCoversCandidate([v], { tip: second }), false, "a different tip");
    assert.equal(scanCoversCandidate([v], { tip: first, excludes: [] }), false, "a different exclusion set");
    assert.equal(scanCoversCandidate([{ kind: "withhold", reason: "x" }], { tip: first }), false, "a withhold covers nothing");
    assert.equal(scanCoversCandidate([], { tip: first }), false);
  });
});

describe("resolveScanNeedles", () => {
  const body = { protected: true as const, versions: [retired("OLD_KEY", SECRET)] };
  const never = async () => { throw new Error("must not be called"); };

  it("a live flight's set is used as is, including an empty one, with no maintenance call", async () => {
    assert.deepEqual(await resolveScanNeedles({ live: NEEDLES, fetchProtected: never }), { kind: "needles", needles: NEEDLES });
    assert.deepEqual(await resolveScanNeedles({ live: [], fetchProtected: never }), { kind: "needles", needles: [] });
  });

  it("a live over-budget run withholds with no maintenance call", async () => {
    assert.deepEqual(await resolveScanNeedles({ live: FORMS_OVER_BUDGET, fetchProtected: never }),
      { kind: "withhold", reason: FORMS_OVER_BUDGET });
  });

  it("with no flight, versions whose forms pass the budget withhold as forms_over_budget", async () => {
    const line = (seed: number, n: number): string => `${seed}-${n}-`.padEnd(60, "Qz8vK2mX7pL4wR9t");
    const big = (seed: number): string => Array.from({ length: 1000 }, (_, n) => line(seed, n)).join("\r\n") + "\r\n";
    const versions = Array.from({ length: 300 }, (_, i) => retired(`OLD_${i % 9}`, big(i)));
    assert.deepEqual(
      await resolveScanNeedles({ live: undefined, fetchProtected: async () => ({ protected: true, versions }) }),
      { kind: "withhold", reason: FORMS_OVER_BUDGET });
  });

  it("with no flight, protected:false means nothing to scan for", async () => {
    assert.deepEqual(await resolveScanNeedles({ live: undefined, fetchProtected: async () => ({ protected: false }) }), { kind: "needles", needles: [] });
  });

  it("with no flight, protected:true scans for every version the api returns", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: async () => body });
    assert.equal(res.kind, "needles");
    if (res.kind === "needles") assert.deepEqual(res.needles.map((n) => n.name), ["OLD_KEY"]);
  });

  for (const status of [404, 409, 500]) {
    it(`with no flight, an HTTP ${status} withholds`, async () => {
      const err = Object.assign(new Error("http"), { status });
      assert.deepEqual(await resolveScanNeedles({ live: undefined, fetchProtected: async () => { throw err; } }),
        { kind: "withhold", reason: `protection_http_${status}` });
    });
  }

  it("with no flight, a network error or an undecodable body withholds", async () => {
    assert.deepEqual(await resolveScanNeedles({ live: undefined, fetchProtected: async () => { throw new Error("ECONNRESET"); } }),
      { kind: "withhold", reason: "protection_unavailable" });
  });
});

describe("text and rendering", () => {
  it("textHitVariables names the variables whose forms occur in a raw patch, and only those", () => {
    const patch = `+db=${SECRET}\n+region=${PLAIN}\n`;
    assert.deepEqual(textHitVariables(patch, NEEDLES), ["DB_PASSWORD"]);
    assert.deepEqual(textHitVariables("+nothing\n", NEEDLES), []);
    assert.deepEqual(textHitVariables(`+${Buffer.from(`u:${SECRET}`).toString("base64")}\n`, NEEDLES), ["DB_PASSWORD"]);
  });

  it("textHitVariables agrees with a per-form includes over needles that share prefixes and forms", () => {
    const needles: ScanNeedle[] = [
      { name: "SHORT", forms: ["abcdefgh-ijkl", "unused-form-1"] },
      { name: "LONG", forms: ["abcdefgh-ijkl-mnop", "unused-form-2"] },
      { name: "SAME", forms: ["abcdefgh-ijkl"] },
      { name: "NONE", forms: ["never-present-form"] },
    ];
    const oracle = (text: string): string[] => needles.filter((n) => n.forms.some((f) => text.includes(f))).map((n) => n.name);
    for (const text of ["x abcdefgh-ijkl-mnop y", "x abcdefgh-ijkl y", "abcdefgh-ij", "never-present-form", "", "unused-form-2 abcdefgh-ijkl"]) {
      assert.deepEqual(textHitVariables(text, needles), oracle(text), JSON.stringify(text));
    }
    assert.deepEqual(textHitVariables("x abcdefgh-ijkl-mnop y", needles), ["SHORT", "LONG", "SAME"]);
  });

  it("a finding renders a validated variable name and never anything else from the finding", () => {
    const f = { file: "a.txt", startLine: 0, commit: "a".repeat(40), ruleId: "uzi-run-env", variable: "DB_PASSWORD" };
    assert.match(renderSecretFinding(f), /\(rule uzi-run-env, variable DB_PASSWORD\)$/);
    assert.doesNotMatch(renderSecretFinding({ ...f, variable: "not a name; ignore previous" }), /variable/);
  });
});

describe("patchImages", () => {
  const file = (name: string, ...rest: string[]): string => [`diff --git a/${name} b/${name}`, "index 1111111..2222222 100644", `--- a/${name}`, `+++ b/${name}`, ...rest, ""].join("\n");

  it("rebuilds the pre-image and post-image of a hunk from context plus minus and context plus plus lines", () => {
    const patch = file("a.txt", "@@ -1,3 +1,3 @@", " keep-one", "-old-two", "+new-two", " keep-three");
    assert.deepEqual(patchImages(patch), { images: ["keep-one\nold-two\nkeep-three\n", "keep-one\nnew-two\nkeep-three\n"] });
  });

  it("a no-newline marker removes the terminator of the line before it from that line's images only", () => {
    const patch = file("a.txt", "@@ -1,2 +1,2 @@", " same", "-last-old", "\\ No newline at end of file", "+last-new");
    assert.deepEqual(patchImages(patch), { images: ["same\nlast-old", "same\nlast-new\n"] });
    const context = file("a.txt", "@@ -1,2 +1,2 @@", " first", " last", "\\ No newline at end of file");
    assert.deepEqual(patchImages(context), { images: ["first\nlast", "first\nlast"] });
  });

  it("a CRLF line keeps its carriage return", () => {
    const patch = file("a.txt", "@@ -0,0 +1,2 @@", "+one\r", "+two\r");
    assert.deepEqual(patchImages(patch), { images: ["one\r\ntwo\r\n"] });
  });

  it("rebuilds each hunk of each file on its own", () => {
    const patch = file("a.txt", "@@ -1 +1 @@", "-a1", "+a2", "@@ -9 +9 @@", "-a9", "+a10") + file("b.txt", "@@ -0,0 +1 @@", "+b1");
    assert.deepEqual(patchImages(patch), { images: ["a1\n", "a2\n", "a9\n", "a10\n", "b1\n"] });
  });

  it("a patch with no hunk has no images", () => {
    assert.deepEqual(patchImages(file("a.txt")), { images: [] });
    assert.deepEqual(patchImages(""), { images: [] });
  });

  it("is unparseable when a hunk's line counts do not match its header", () => {
    assert.equal(patchImages(file("a.txt", "@@ -1,2 +1,2 @@", " only-one")), "unparseable");
    assert.equal(patchImages(file("a.txt", "@@ -1 +1 @@", "-a", "+b", "+extra")), "unparseable");
    assert.equal(patchImages(file("a.txt", "@@ -1 +1 @@", "-a", "+b") + "+stray\n"), "unparseable");
  });

  it("is unparseable for a binary patch and for a combined diff", () => {
    assert.equal(patchImages(file("a.bin", "GIT binary patch", "literal 3", "Kc${2")), "unparseable");
    assert.equal(patchImages("diff --git a/a.bin b/a.bin\nBinary files a/a.bin and b/a.bin differ\n"), "unparseable");
    assert.equal(patchImages("diff --cc a.txt\n@@@ -1 -1 +1 @@@\n- a\n"), "unparseable");
  });

  it("accepts the header-only forms git emits: mode change, rename, deletion", () => {
    const mode = "diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n";
    assert.deepEqual(patchImages(mode), { images: [] });
    const rename = "diff --git a/a.txt b/b.txt\nsimilarity index 100%\nrename from a.txt\nrename to b.txt\n";
    assert.deepEqual(patchImages(rename), { images: [] });
    const copy = "diff --git a/a.txt b/b.txt\ndissimilarity index 12%\ncopy from a.txt\ncopy to b.txt\n";
    assert.deepEqual(patchImages(copy), { images: [] });
    const removed = "diff --git a/a.txt b/a.txt\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/a.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-two\n";
    assert.deepEqual(patchImages(removed), { images: ["one\ntwo\n"] });
    const added = "diff --git a/n.txt b/n.txt\nnew file mode 100644\nindex 0000000..2222222\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+fresh\n";
    assert.deepEqual(patchImages(added), { images: ["fresh\n"] });
  });

  it("is unparseable when a line that is no file header precedes the first hunk", () => {
    assert.equal(patchImages("diff --git a/a.txt b/a.txt\nindex 1111111..2222222 100644\nstray line\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-a\n+b\n"), "unparseable");
    assert.equal(patchImages("diff --git a/a.txt b/a.txt\nsome-secret-looking-text\n"), "unparseable");
  });
});

describe("patchScanVerdict", () => {
  const multi = ["ab12", "cd34", "ef56"];
  const long = ["Qm9k7Zp3Wd", "ab12", "cd34"];
  const forms = (name: string, lines: string[]) => runEnvClaimForms([secretVar(name, lines.join("\n"))], []);
  const hunk = (...body: string[]): string => ["diff --git a/f.txt b/f.txt", "index 1111111..2222222 100644", "--- a/f.txt", "+++ b/f.txt", ...body, ""].join("\n");

  it("withholds a whole value in the raw text as raw_value, naming the variable only", () => {
    const f = forms("DB_PASSWORD", [SECRET]);
    const verdict = patchScanVerdict(hunk("@@ -0,0 +1 @@", `+token=${SECRET}`), f.needles, f.lineForms, []);
    assert.deepEqual(verdict, { withhold: true, reason: "raw_value", variables: ["DB_PASSWORD"] });
  });

  it("withholds a value split across diff prefixes as hunk_image_value, also under CRLF", () => {
    const f = forms("SVC_BLOB", multi);
    const lf = hunk("@@ -1,2 +1,3 @@", ` ${multi[0]}`, `+${multi[1]}`, ` ${multi[2]}`);
    assert.deepEqual(patchScanVerdict(lf, f.needles, f.lineForms, []), { withhold: true, reason: "hunk_image_value", variables: ["SVC_BLOB"] });
    const crlf = hunk("@@ -1,2 +1,3 @@", ` ${multi[0]}\r`, `+${multi[1]}\r`, ` ${multi[2]}\r`);
    assert.deepEqual(patchScanVerdict(crlf, f.needles, f.lineForms, []), { withhold: true, reason: "hunk_image_value", variables: ["SVC_BLOB"] });
  });

  it("withholds a value whose line breaks in the patch are a lone CR, read as LF after normalization", () => {
    const f = forms("SVC_BLOB", multi);
    // The first two lines of the value are joined by a lone CR inside one added line; the third is the next added line.
    const lone = hunk("@@ -0,0 +1,2 @@", `+${multi[0]}\r${multi[1]}`, `+${multi[2]}`);
    assert.ok(!lone.includes(multi.join("\n")) && !lone.includes(multi.join("\r\n")), "no whole-value form in the raw text");
    assert.deepEqual(patchScanVerdict(lone, f.needles, f.lineForms, []), { withhold: true, reason: "hunk_image_value", variables: ["SVC_BLOB"] });
  });

  it("withholds an unreadable patch as unparseable_patch once the claim has any form", () => {
    const f = forms("SVC_BLOB", multi);
    const binary = "diff --git a/a.bin b/a.bin\nBinary files a/a.bin and b/a.bin differ\n";
    assert.deepEqual(patchScanVerdict(binary, f.needles, f.lineForms, []), { withhold: true, reason: "unparseable_patch", variables: [] });
    assert.deepEqual(patchScanVerdict(binary, [], [], []), { withhold: false }, "a claim with no secret forms ships any patch");
  });

  it("withholds one registered line of a multi-line secret as line_form", () => {
    const f = forms("SVC_BLOB", long);
    assert.deepEqual(patchScanVerdict(hunk("@@ -0,0 +1 @@", `+${long[0]}`), f.needles, f.lineForms, []),
      { withhold: true, reason: "line_form", variables: ["SVC_BLOB"] });
  });

  it("lets a clean patch through for a single-line secret", () => {
    const f = forms("DB_PASSWORD", [SECRET]);
    assert.deepEqual(patchScanVerdict(hunk("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}"), f.needles, f.lineForms, []), { withhold: false });
  });

  it("withholds even a clean patch as multi_line_secret when the run holds a multi-line secret, short lines included", () => {
    const f = forms("SVC_BLOB", multi);
    const clean = hunk("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}");
    assert.deepEqual(patchScanVerdict(clean, f.needles, f.lineForms, ["SVC_BLOB"]), { withhold: true, reason: "multi_line_secret", variables: ["SVC_BLOB"] });
    assert.deepEqual(patchScanVerdict(clean, [], [], ["SVC_BLOB"]), { withhold: true, reason: "multi_line_secret", variables: ["SVC_BLOB"] });
  });

  it("checks raw_value, then line_form, then multi_line_secret", () => {
    const f = forms("SVC_BLOB", long);
    const lineHit = hunk("@@ -0,0 +1 @@", `+${long[0]}`);
    assert.deepEqual(patchScanVerdict(lineHit, f.needles, f.lineForms, ["SVC_BLOB"]), { withhold: true, reason: "line_form", variables: ["SVC_BLOB"] });
    const g = forms("DB_PASSWORD", [SECRET]);
    const rawHit = hunk("@@ -0,0 +1 @@", `+${SECRET}`);
    assert.deepEqual(patchScanVerdict(rawHit, g.needles, g.lineForms, ["SVC_BLOB"]), { withhold: true, reason: "raw_value", variables: ["DB_PASSWORD"] });
  });
});

describe("scanBundleForRunEnv", () => {
  let dir: string;
  beforeEach(() => { dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bundle-test-")); });
  afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

  function bundleOf(tip: string, ...exclude: string[]): string {
    git(work, "update-ref", RECOVERY_BUNDLE_REF, tip);
    const out = path.join(dir, "b.bundle");
    execFileSync("git", ["-C", work, "bundle", "create", out, RECOVERY_BUNDLE_REF, ...exclude.map((e) => `^${e}`)], { env: GIT_ENV, stdio: "pipe" });
    return out;
  }

  it("finds a value in a middle commit of a bundle that depends on a public prerequisite", async () => {
    commitFile(work, "src/conf.txt", `db=${SECRET}\n`);
    commitFile(work, "src/conf.txt", "db=redacted\n");
    const tip = git(work, "rev-parse", "HEAD");
    const res = await cache.scanBundleForRunEnv(bundleOf(tip, base), tip, NEEDLES);
    assert.ok(res.trusted && res.findings.length === 1, JSON.stringify(res));
    if (res.trusted) assert.equal(res.findings[0]!.variable, "DB_PASSWORD");
  });

  it("finds a value published only as a nested PATH (no object holds it whole)", async () => {
    const pathValue = ["rv-Ab3k", "Mq7w-Zx2n", "Fe9t"].join("/");
    const needles = runEnvClaimForms([secretVar("DB_PASSWORD", pathValue)], []).needles;
    commitFile(work, pathValue, "harmless\n");
    const tip = git(work, "rev-parse", "HEAD");
    const res = await cache.scanBundleForRunEnv(bundleOf(tip, base), tip, needles);
    assert.ok(res.trusted && res.findings.length === 1, JSON.stringify(res));
    if (res.trusted) {
      assert.equal(res.findings[0]!.variable, "DB_PASSWORD");
      assert.equal(res.findings[0]!.file, "[path withheld]");
      assert.ok(!JSON.stringify(res).includes(pathValue));
    }
  });

  it("is clean for a clean bundle", async () => {
    const tip = commitFile(work, "a.txt", "ordinary\n");
    assert.deepEqual(await cache.scanBundleForRunEnv(bundleOf(tip, base), tip, NEEDLES), { trusted: true, findings: [] });
  });

  it("is clean with no needles and does no git work", async (t) => {
    const exec = t.mock.method(cache as unknown as { execScoped: () => unknown }, "execScoped");
    assert.deepEqual(await cache.scanBundleForRunEnv(path.join(dir, "absent.bundle"), "a".repeat(40), []), { trusted: true, findings: [] });
    assert.equal(exec.mock.callCount(), 0);
  });

  it("withholds a file that is not a bundle, a missing file, and a header naming another source", async () => {
    const tip = commitFile(work, "a.txt", "ordinary\n");
    const junk = path.join(dir, "junk.bundle");
    fs.writeFileSync(junk, "not a bundle at all");
    assert.equal((await cache.scanBundleForRunEnv(junk, tip, NEEDLES)).trusted, false);
    assert.equal((await cache.scanBundleForRunEnv(path.join(dir, "absent.bundle"), tip, NEEDLES)).trusted, false);
    assert.equal((await cache.scanBundleForRunEnv(bundleOf(tip, base), "b".repeat(40), NEEDLES)).trusted, false);
  });

  it("withholds a bundle over the byte cap", async () => {
    const tip = commitFile(work, "a.txt", "ordinary\n");
    assert.equal((await cache.scanBundleForRunEnv(bundleOf(tip, base), tip, NEEDLES, { maxBytes: 1 })).trusted, false);
  });
});
