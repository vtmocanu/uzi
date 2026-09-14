import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, chmod, rm, readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { probeCodexRuntime } from "../src/codex/codex-runtime-probe.js";

// PRD #1332 D3 (M5A / C2a) — installer↔probe FORMAT-AGREEMENT integration test.
//
// The other two suites test each side of the trust boundary in isolation:
// templates-guardrails.test.ts regex-matches install-codex.sh's SOURCE, and
// codex-runtime-probe.test.ts hand-authors a receipt object it BELIEVES the installer
// writes. Neither runs the REAL installer and feeds its EMITTED receipt to the probe, so
// a real format drift between the two (a mode-string edge case, a field rename, a
// printf/path-join difference between bash and the TS probe) would ship undetected.
//
// This suite closes that gap end to end: it builds a small SYNTHETIC codex package
// tarball with the exact members install-codex.sh's lock requires, runs the REAL
// install-codex.sh rootless via its documented override env (UZI_CODEX_ARTIFACT /
// UZI_CODEX_PREFIX / UZI_CODEX_LOCK / TARGETARCH), then feeds the receipt the installer
// actually wrote to probeCodexRuntime and asserts capable === true — proving the two
// agree on the field names, the absolute-path join and the octal mode-string shape. It
// then tampers one installed member and asserts the next probe flips to not-capable,
// proving the receipt is a live integrity binding, not a static shape match.
//
// The probe bakes the pinned digest as a compile-time constant, and a SYNTHETIC tarball
// can never carry the production digest, so the probe is pointed at the synthetic
// tarball's REAL digest via expectedLockDigest (the installer's fail-closed SHA check is
// satisfied the same way, by a temp lock whose CODEX_SHA256_<arch> == that digest). The
// version is likewise passed through from the (real-mirroring) synthetic lock. Everything
// else — every member, path, mode and per-member SHA — is produced by the real installer
// and verified by the real probe, unmodified.

const here = path.dirname(fileURLToPath(import.meta.url));
const installScript = path.resolve(here, "../codex/install-codex.sh");
const realLockPath = path.resolve(here, "../codex/codex-package.lock");

/** amd64/arm64 for this runtime, or undefined on an unsupported arch (skip). */
function mapArch(nodeArch: string): "amd64" | "arm64" | undefined {
  if (nodeArch === "x64") return "amd64";
  if (nodeArch === "arm64") return "arm64";
  return undefined;
}

/** Read a `KEY=value` line out of the lock text (installer's own format). */
function lockVal(lockText: string, key: string): string {
  const m = lockText.match(new RegExp(`^${key}=(.*)$`, "m"));
  if (!m) throw new Error(`lock is missing ${key}`);
  return m[1]!.trim();
}

/** True only when the POSIX tooling the real installer shells out to is present; else the
 *  suite skips rather than forcing a fragile path (per the task's fallback guidance). */
function haveInstallerTooling(): boolean {
  const r = spawnSync("sh", ["-c", "command -v bash && command -v tar && command -v sha256sum && command -v stat"], {
    encoding: "utf8",
  });
  return r.status === 0;
}

const ARCH = mapArch(process.arch);
const TOOLING = haveInstallerTooling();
const REAL_INSTALLER = ARCH !== undefined && TOOLING;
const skip = REAL_INSTALLER
  ? false
  : `real-installer path unavailable (arch=${process.arch}, tooling=${TOOLING})`;

const tempRoots: string[] = [];
after(async () => {
  await Promise.all(tempRoots.map((dir) => rm(dir, { recursive: true, force: true })));
});

interface InstalledState {
  prefix: string;
  version: string;
  arch: string;
  synthSha: string;
  members: string[];
  receiptPath: string;
  versionRoot: string;
}

// Populated by `before` when the real-installer path is available.
let state: InstalledState | undefined;

/** Build the synthetic package, run the REAL installer once, and capture what it wrote. */
async function buildAndInstall(): Promise<InstalledState> {
  const arch = ARCH!;
  const lockText = await readFile(realLockPath, "utf8");
  const version = lockVal(lockText, "CODEX_VERSION");
  const muslTarget = lockVal(lockText, `CODEX_TARGET_${arch}`);
  const members = lockVal(lockText, "CODEX_MEMBERS").split(/\s+/).filter(Boolean);

  // The manifest the installer asserts field-by-field against the lock. `layoutVersion`
  // is numeric in the lock, the rest are strings; `target` is this arch's musl target.
  const manifest = {
    layoutVersion: Number(lockVal(lockText, "CODEX_MANIFEST_LAYOUT_VERSION")),
    version: lockVal(lockText, "CODEX_MANIFEST_VERSION"),
    variant: lockVal(lockText, "CODEX_MANIFEST_VARIANT"),
    entrypoint: lockVal(lockText, "CODEX_MANIFEST_ENTRYPOINT"),
    resourcesDir: lockVal(lockText, "CODEX_MANIFEST_RESOURCES_DIR"),
    pathDir: lockVal(lockText, "CODEX_MANIFEST_PATH_DIR"),
    target: muslTarget,
  };

  // --- 1. lay out the synthetic package tree with every required member --------------
  const scratch = await mkdtemp(path.join(os.tmpdir(), "codex-installer-it-"));
  tempRoots.push(scratch);
  const pkgDir = path.join(scratch, "pkg");
  for (const member of members) {
    const abs = path.join(pkgDir, member);
    await mkdir(path.dirname(abs), { recursive: true });
    if (member === "codex-package.json") {
      await writeFile(abs, `${JSON.stringify(manifest, null, 2)}\n`);
      await chmod(abs, 0o644);
    } else {
      // Distinct executable stub per member, so each member's SHA-256 is meaningful and a
      // tamper of one cannot collide with another.
      await writeFile(abs, `#!/bin/sh\n# synthetic ${member}\nexit 0\n`);
      await chmod(abs, 0o755);
    }
  }

  // --- 2. tar it up (members at the archive root, exactly as the real package) --------
  const tarball = path.join(scratch, `codex-package-${muslTarget}.tar.gz`);
  const tarRes = spawnSync("tar", ["-C", pkgDir, "-czf", tarball, "."], { encoding: "utf8" });
  assert.equal(tarRes.status, 0, `tar failed: ${tarRes.stderr}`);

  // --- 3. real digest of the synthetic tarball (drives BOTH the installer's SHA gate
  //        via the temp lock AND the probe's expectedLockDigest) -----------------------
  const synthSha = createHash("sha256").update(await readFile(tarball)).digest("hex");

  // --- 4. temp lock mirroring the real lock, only the arch's SHA256 pin swapped -------
  const synthLockPath = path.join(scratch, "codex-package.lock");
  const synthLockText = lockText.replace(
    new RegExp(`^CODEX_SHA256_${arch}=.*$`, "m"),
    `CODEX_SHA256_${arch}=${synthSha}`,
  );
  assert.match(synthLockText, new RegExp(`^CODEX_SHA256_${arch}=${synthSha}$`, "m"), "temp lock must carry the synthetic digest");
  await writeFile(synthLockPath, synthLockText);

  // --- 5. run the REAL installer rootless via its override env ------------------------
  const prefix = path.join(scratch, "prefix");
  const installRes = spawnSync("bash", [installScript, arch], {
    encoding: "utf8",
    env: {
      ...process.env,
      UZI_CODEX_ARTIFACT: tarball,
      UZI_CODEX_PREFIX: prefix,
      UZI_CODEX_LOCK: synthLockPath,
      TARGETARCH: arch,
    },
  });
  assert.equal(installRes.status, 0, `install-codex.sh failed (arch=${arch}):\n${installRes.stderr}`);

  return {
    prefix,
    version,
    arch,
    synthSha,
    members,
    receiptPath: path.join(prefix, `${version}.receipt.json`),
    versionRoot: path.join(prefix, version),
  };
}

describe("install-codex.sh ↔ probeCodexRuntime end-to-end format agreement", () => {
  before(async () => {
    if (!REAL_INSTALLER) return;
    state = await buildAndInstall();
  });

  it("the installer's EMITTED receipt has exactly the fields/path/mode shape the probe reads", { skip }, async () => {
    const s = state!;
    const receipt = JSON.parse(await readFile(s.receiptPath, "utf8")) as Record<string, unknown>;

    // Top-level binding the probe cross-checks against version/arch/digest.
    assert.equal(receipt.codexVersion, s.version, "receipt.codexVersion");
    assert.equal(receipt.targetArch, s.arch, "receipt.targetArch");
    assert.equal(receipt.lockDigest, s.synthSha, "receipt.lockDigest == synthetic tarball digest");

    const members = receipt.members;
    assert.ok(Array.isArray(members), "receipt.members must be an array");
    assert.equal(members.length, s.members.length, "receipt lists every lock member");
    const seen = new Set<string>();
    for (const raw of members) {
      const entry = raw as Record<string, unknown>;
      // Exactly the four fields the probe destructures, each a string of the shape it
      // requires: absolute path == prefix/version/member, an octal mode string, a 64-hex
      // SHA. This is the structural contract; the probe's own acceptance (next test)
      // proves it is not just shaped right but verifiable.
      assert.equal(typeof entry.member, "string", "member is a string");
      assert.equal(typeof entry.path, "string", "path is a string");
      assert.equal(typeof entry.sha256, "string", "sha256 is a string");
      assert.equal(typeof entry.mode, "string", "mode is a string");
      const member = entry.member as string;
      assert.ok(s.members.includes(member), `receipt member ${member} is a lock member`);
      assert.equal(entry.path, path.join(s.versionRoot, member), `member ${member} path is prefix/version/member`);
      assert.match(entry.mode as string, /^[0-7]{3,4}$/, `member ${member} mode is an octal string`);
      assert.match(entry.sha256 as string, /^[0-9a-f]{64}$/, `member ${member} sha256 is 64 hex`);
      seen.add(member);
    }
    assert.equal(seen.size, s.members.length, "no duplicate members");
  });

  it("the probe ACCEPTS the installer's real receipt → capable === true", { skip }, async () => {
    const s = state!;
    // Point the probe at the SAME prefix, and reconcile the two values a synthetic
    // artifact cannot inherit from the baked constants: the version (from the real-
    // mirroring lock) and the digest (the synthetic tarball's real SHA-256, which the
    // installer recorded into the receipt's lockDigest). Everything the probe verifies
    // beyond that — every member's on-disk path, mode and SHA-256 vs. the receipt — is
    // the installer's own output, unmodified.
    const result = await probeCodexRuntime({
      prefix: s.prefix,
      arch: s.arch,
      expectedVersion: s.version,
      expectedLockDigest: s.synthSha,
    });
    assert.strictEqual(result.capable, true, `expected capable, got: ${result.reason ?? ""}`);
    assert.strictEqual(result.reason, undefined);
  });

  it("tampering an installed member flips the probe to not-capable (digest mismatch)", { skip }, async () => {
    const s = state!;
    // Sanity: capable before the tamper (same call as above, so the flip is attributable
    // to the tamper alone).
    const before = await probeCodexRuntime({
      prefix: s.prefix,
      arch: s.arch,
      expectedVersion: s.version,
      expectedLockDigest: s.synthSha,
    });
    assert.strictEqual(before.capable, true, `expected capable pre-tamper, got: ${before.reason ?? ""}`);

    // Overwrite one installed member's BYTES (same length or not — the SHA changes either
    // way), leaving the receipt untouched. The next probe recomputes the member's digest
    // and finds it no longer matches the recorded one.
    const tampered = path.join(s.versionRoot, "bin/codex");
    await writeFile(tampered, "#!/bin/sh\n# tampered\nexit 1\n");

    const after = await probeCodexRuntime({
      prefix: s.prefix,
      arch: s.arch,
      expectedVersion: s.version,
      expectedLockDigest: s.synthSha,
    });
    assert.strictEqual(after.capable, false, "a tampered member must flip the probe to not-capable");
    assert.match(after.reason ?? "", /member digest mismatch/);
  });
});
