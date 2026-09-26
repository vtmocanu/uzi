import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { gitleaksReportWellFormed } from "../src/secret-scan-guard.js";
import { nullLogger } from "./helpers.js";
import { writeGitleaksShim } from "./gitleaks-shim.js";

// scanPatchForSecrets gates every preserved_patch: only a trusted scan with zero findings of the
// EXACT stored text may attach it. These drive it through the real gitleaks-stdin seam with the
// test shim, one mechanism per case.

const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-patchscan-"));
after(() => fs.rmSync(dir, { recursive: true, force: true }));
type Mode = Parameters<typeof writeGitleaksShim>[1];
const cacheWith = (mode: Mode) =>
  new GitCache(path.join(dir, `data-${mode}`), nullLogger(), undefined, { gitleaksBin: writeGitleaksShim(dir, mode) });
// GitHub-PAT-shaped (what the shim detects), assembled at runtime (check:token-literals).
const token = () => ["gh", "p_", "x7Rq".repeat(9)].join("");
const patchWith = (line: string) =>
  `diff --git a/a.ts b/a.ts\n--- a/a.ts\n+++ b/a.ts\n@@ -1,2 +1,2 @@\n${line}\n+export const ok = 1;\n`;

describe("scanPatchForSecrets", () => {
  it("a clean patch is trusted with no findings", async () => {
    assert.deepEqual(await cacheWith("detect").scanPatchForSecrets(patchWith(" context")), { trusted: true, findings: [] });
  });

  it("an added-line secret is a finding", async () => {
    const res = await cacheWith("detect").scanPatchForSecrets(patchWith(`+const k = "${token()}";`));
    assert.equal(res.trusted, true);
    assert.ok(res.findings.length > 0);
  });

  it("a secret on a REMOVED or CONTEXT line is still a finding (the whole stored patch is scanned)", async () => {
    for (const line of [`-const k = "${token()}";`, ` const k = "${token()}";`]) {
      const res = await cacheWith("detect").scanPatchForSecrets(patchWith(line));
      assert.ok(res.findings.length > 0, `not flagged: ${line.slice(0, 1)}`);
    }
  });

  it("a scanner failure is untrusted", async () => {
    assert.equal((await cacheWith("fail").scanPatchForSecrets(patchWith(" context"))).trusted, false);
  });

  it("a byte count that does not match the fed patch is untrusted", async () => {
    assert.equal((await cacheWith("stdinlie").scanPatchForSecrets(patchWith(" context"))).trusted, false);
  });

  it("a malformed report is untrusted, never read as clean", async () => {
    assert.deepEqual(await cacheWith("badreport").scanPatchForSecrets(patchWith(" context")), { trusted: false, findings: [] });
  });
});

describe("finalize scan (scanLogRange) — malformed report", () => {
  it("is untrusted", async () => {
    const repo = path.join(dir, "repo");
    fs.mkdirSync(repo);
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const g = (args: string[]) => execFileSync("git", ["-C", repo, ...args], { env, encoding: "utf8" }).trim();
    g(["init", "-q", "--initial-branch=main"]);
    const commit = (f: string) => {
      fs.writeFileSync(path.join(repo, f), `${f}\n`);
      g(["add", f]);
      g(["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", f]);
      return g(["rev-parse", "HEAD"]);
    };
    const base = commit("a.txt");
    const head = commit("b.txt");
    const cache = cacheWith("badreport");
    const scan = (cache as unknown as {
      scanLogRange: (b: string, r: string, o: { label: string; timeoutMs: number; onUntrusted: string }) => Promise<{ trusted: boolean }>;
    }).scanLogRange.bind(cache);
    assert.equal((await scan(repo, `${base}..${head}`, { label: "t", timeoutMs: 30_000, onUntrusted: "x" })).trusted, false);
  });
});

describe("gitleaksReportWellFormed", () => {
  it("accepts null and arrays, rejects everything else", () => {
    assert.equal(gitleaksReportWellFormed("null"), true);
    assert.equal(gitleaksReportWellFormed("[]"), true);
    assert.equal(gitleaksReportWellFormed('[{"RuleID":"x"}]'), true);
    for (const bad of ["", "[broken", "{}", '"str"', "42", "[null, 42]", "[[]]"]) assert.equal(gitleaksReportWellFormed(bad), false, bad);
  });
});
