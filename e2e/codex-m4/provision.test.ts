// PRD #1287 C1 — self-tests for the PURE provisioner logic + hard-prerequisite branches (D7
// points 1-2). No real install runs: synthetic inputs drive arch mapping, lock parsing, digest,
// receipt matching, the reinstall decision, layout completeness, and the hard-fail branches. The
// resolveCodexBin incomplete-package-after-provisioning throw is driven through its injected
// `exists`/`installer` seams (an installer that "succeeds" but leaves an incomplete layout), so
// still no network install runs. On this worker the image-baked path is taken and the smoke needs
// no install.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import {
  CodexProvisionError,
  assertBinaryVersion,
  assertNoUnexpectedEtcCodex,
  decideProvision,
  layoutComplete,
  lockDigest,
  parseLock,
  receiptMatches,
  resolveArch,
  resolveCodexBin,
  type Receipt,
} from "./provision.js";

const SHA = "e10fa0cee78e9f0bd395880f03fd4fd227d903ca7af649bbc08d1649101e9225";
const SHA2 = "a3bfaf4b62fcb17e0a0338dfe0502413ddce0ab1b39028679390539c45d2c6e3";
const LOCK = [
  "CODEX_VERSION=0.153.2",
  "CODEX_TAG=rust-v0.153.2",
  `CODEX_SHA256_amd64=${SHA}`,
  `CODEX_SHA256_arm64=${SHA2}`,
  "",
].join("\n");

describe("resolveArch", () => {
  it("maps x64 → amd64 and arm64 → arm64", () => {
    assert.equal(resolveArch("x64"), "amd64");
    assert.equal(resolveArch("arm64"), "arm64");
  });
  it("hard-fails on an unsupported architecture", () => {
    assert.throws(() => resolveArch("ia32"), CodexProvisionError);
    assert.throws(() => resolveArch("ppc64"), /unsupported architecture/);
  });
});

describe("parseLock", () => {
  it("extracts version + per-arch SHA256", () => {
    const facts = parseLock(LOCK);
    assert.equal(facts.version, "0.153.2");
    assert.equal(facts.sha256.amd64, SHA);
    assert.equal(facts.sha256.arm64, SHA2);
  });
  it("hard-fails on a missing key", () => {
    assert.throws(() => parseLock(`CODEX_SHA256_amd64=${SHA}\nCODEX_SHA256_arm64=${SHA2}`), /missing "CODEX_VERSION"/);
  });
  it("hard-fails on a non-64-hex digest", () => {
    assert.throws(
      () => parseLock("CODEX_VERSION=0.153.2\nCODEX_SHA256_amd64=nothex\nCODEX_SHA256_arm64=" + SHA2),
      /not a 64-hex digest/,
    );
  });
});

describe("lockDigest", () => {
  it("is deterministic and content-sensitive", () => {
    assert.equal(lockDigest(LOCK), lockDigest(LOCK));
    assert.notEqual(lockDigest(LOCK), lockDigest(LOCK + "x"));
  });
});

describe("receiptMatches", () => {
  const expected: Receipt = { version: "0.153.2", arch: "amd64", lockDigest: "abc" };
  it("matches an exact receipt", () => {
    assert.equal(receiptMatches({ version: "0.153.2", arch: "amd64", lockDigest: "abc" }, expected), true);
  });
  it("rejects a version / arch / digest mismatch", () => {
    assert.equal(receiptMatches({ version: "0.153.1", arch: "amd64", lockDigest: "abc" }, expected), false);
    assert.equal(receiptMatches({ version: "0.153.2", arch: "arm64", lockDigest: "abc" }, expected), false);
    assert.equal(receiptMatches({ version: "0.153.2", arch: "amd64", lockDigest: "zzz" }, expected), false);
  });
  it("rejects a non-object receipt", () => {
    assert.equal(receiptMatches(undefined, expected), false);
    assert.equal(receiptMatches("nope", expected), false);
  });
});

describe("decideProvision", () => {
  const expected: Receipt = { version: "0.153.2", arch: "amd64", lockDigest: "abc" };
  it("reuses the cache only when layout is complete AND the receipt matches", () => {
    assert.equal(decideProvision({ receipt: { ...expected }, layoutComplete: true, expected }), "use-cache");
  });
  it("reinstalls on an incomplete layout", () => {
    assert.equal(decideProvision({ receipt: { ...expected }, layoutComplete: false, expected }), "reinstall");
  });
  it("reinstalls on a stale receipt (version text alone is not trusted)", () => {
    assert.equal(
      decideProvision({ receipt: { version: "0.153.2", arch: "amd64", lockDigest: "STALE" }, layoutComplete: true, expected }),
      "reinstall",
    );
  });
});

describe("layoutComplete", () => {
  it("requires every member to exist", () => {
    assert.equal(layoutComplete("/pkg", () => true), true);
    assert.equal(layoutComplete("/pkg", (p) => !p.endsWith("codex-code-mode-host")), false);
  });
});

describe("assertNoUnexpectedEtcCodex", () => {
  it("hard-fails when /etc/codex is present", () => {
    assert.throws(() => assertNoUnexpectedEtcCodex(() => ({ present: true })), /unexpected system Codex configuration/);
  });
  it("passes when /etc/codex is absent", () => {
    assert.doesNotThrow(() => assertNoUnexpectedEtcCodex(() => ({ present: false })));
  });
});

describe("assertBinaryVersion", () => {
  it("passes on an exact version match", () => {
    assert.doesNotThrow(() => assertBinaryVersion("/bin/codex", "0.153.2", () => ({ status: 0, stdout: "codex-cli 0.153.2\n" })));
  });
  it("hard-fails on a version mismatch", () => {
    assert.throws(
      () => assertBinaryVersion("/bin/codex", "0.153.2", () => ({ status: 0, stdout: "codex-cli 0.153.1\n" })),
      /version mismatch/,
    );
  });
  it("hard-fails on a non-zero exit", () => {
    assert.throws(() => assertBinaryVersion("/bin/codex", "0.153.2", () => ({ status: 1, stdout: "" })), /exited 1/);
  });
});

describe("resolveCodexBin incomplete-package hard fail", () => {
  it("throws CodexProvisionError when provisioning leaves an INCOMPLETE layout (no real install)", () => {
    // Drive the cache-install path with injected seams so no network install runs:
    //   - exists=()=>false  -> the image-baked layout is incomplete (skip branch 1) AND the
    //     post-install layout is incomplete (branch 2 throws);
    //   - installer         -> "succeeds" (status 0) but writes nothing, simulating a broken
    //     install that left the package incomplete.
    const cachePrefix = mkdtempSync(path.join(tmpdir(), "codex-m4-provision-"));
    const lockPath = path.join(cachePrefix, "codex-package.lock");
    writeFileSync(lockPath, LOCK, "utf8");
    let installerCalls = 0;
    try {
      assert.throws(
        () =>
          resolveCodexBin({
            nodeArch: "x64",
            lockPath,
            cachePrefix,
            etcLstat: () => ({ present: false }),
            exists: () => false,
            installer: () => {
              installerCalls += 1;
              return { status: 0, stderr: "" };
            },
          }),
        (err: unknown) =>
          err instanceof CodexProvisionError && /incomplete Codex package .* after provisioning/.test(err.message),
      );
      assert.equal(installerCalls, 1, "the injected installer ran exactly once (reinstall decided, no real install)");
    } finally {
      rmSync(cachePrefix, { recursive: true, force: true });
    }
  });

  it("throws when the injected installer itself fails (non-zero status)", () => {
    const cachePrefix = mkdtempSync(path.join(tmpdir(), "codex-m4-provision-"));
    const lockPath = path.join(cachePrefix, "codex-package.lock");
    writeFileSync(lockPath, LOCK, "utf8");
    try {
      assert.throws(
        () =>
          resolveCodexBin({
            nodeArch: "x64",
            lockPath,
            cachePrefix,
            etcLstat: () => ({ present: false }),
            exists: () => false,
            installer: () => ({ status: 7, stderr: "boom" }),
          }),
        /install-codex\.sh amd64 exited 7: boom/,
      );
    } finally {
      rmSync(cachePrefix, { recursive: true, force: true });
    }
  });
});
