import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { scanIsTrustworthy } from "../src/secret-scan-guard.js";
import { nullLogger } from "./helpers.js";
import { writeGitleaksShim } from "./gitleaks-shim.js";

// gitleaks v8.30.x colours its level tokens even when stderr is a pipe (ESC[31mERR ESC[0m). The
// word-boundary error match in scanIsTrustworthy cannot see "ERR" right after an SGR "m", so an
// errored finalize scan read as trustworthy. The SGR strip belongs in the shared predicate so every
// caller is covered, not only the checkpoint scanner that stripped its own copy (#1618).

const ESC = "\u001b";
const colourErr = `${ESC}[90m8:15AM${ESC}[0m ${ESC}[31mERR${ESC}[0m failed to scan commit\n`;

describe("scanIsTrustworthy — ANSI-coloured error tokens", () => {
  it("is false for a coloured ERR line, other inputs good", () => {
    assert.equal(
      scanIsTrustworthy({ stderr: `${colourErr}INF 2 commits scanned.\n`, scannedCommits: 2, expectedCommits: 2, execOk: true }),
      false,
    );
  });

  it("is false for a coloured FTL/fatal line", () => {
    assert.equal(
      scanIsTrustworthy({ stderr: `${ESC}[31mfatal${ESC}[0m: bad object\n`, scannedCommits: 2, expectedCommits: 2, execOk: true }),
      false,
    );
  });

  it("stays true for coloured benign output", () => {
    assert.equal(
      scanIsTrustworthy({ stderr: `${ESC}[1mINF${ESC}[0m 2 commits scanned.\n`, scannedCommits: 2, expectedCommits: 2, execOk: true }),
      true,
    );
  });
});

describe("finalize scan (scanLogRange) — coloured gitleaks error", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sgr-"));
  after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const g = (args: string[]) => execFileSync("git", ["-C", repo, ...args], { env, encoding: "utf8" }).trim();
  const repo = path.join(dir, "repo");
  fs.mkdirSync(repo);
  g(["init", "-q", "--initial-branch=main"]);
  const commit = (f: string) => {
    fs.writeFileSync(path.join(repo, f), `${f}\n`);
    g(["add", f]);
    g(["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", f]);
    return g(["rev-parse", "HEAD"]);
  };
  const base = commit("a.txt");
  commit("b.txt");
  const head = commit("c.txt");

  type ScanLogRange = (
    barePath: string,
    logRange: string,
    opts: { label: string; timeoutMs: number; onUntrusted: string },
  ) => Promise<{ trusted: boolean; findings: unknown[] }>;
  const scanWith = (mode: "detect" | "colorerr") => {
    const cache = new GitCache(path.join(dir, `data-${mode}`), nullLogger(), undefined, {
      gitleaksBin: writeGitleaksShim(dir, mode),
    });
    // The private finalize core, driven directly: its only caller (secretScanRange) first resolves
    // a publication floor against a remote, which this unit does not need.
    const scan = (cache as unknown as { scanLogRange: ScanLogRange }).scanLogRange.bind(cache);
    return scan(repo, `${base}..${head}`, { label: "test scan", timeoutMs: 30_000, onUntrusted: "failing open" });
  };

  it("control: a clean, uncoloured scan of the range is trusted", async () => {
    assert.deepEqual(await scanWith("detect"), { trusted: true, findings: [] });
  });

  it("a scan whose gitleaks logged a coloured ERR is NOT trusted", async () => {
    const res = await scanWith("colorerr");
    assert.equal(res.trusted, false);
  });
});
