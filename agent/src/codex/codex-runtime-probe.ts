// PRD #1332 D3 (M5A / C2): the startup runtime probe that decides whether this
// worker may advertise the `codex_harness_v1` PROTOCOL capability.
//
// The build (agent/codex/install-codex.sh, run identically by both worker templates)
// installs the pinned Codex native package to ${UZI_CODEX_PREFIX}/${version}/ and, in
// its final step, writes a ROOT-OWNED, WORLD-READABLE runtime receipt BESIDE the
// version root at ${UZI_CODEX_PREFIX}/${version}.receipt.json. The receipt binds — with
// NO credential or mutable HOME state — the lock digest (this arch's pinned tarball
// SHA256), the architecture, the exact codex-cli version, and for every required
// package member its absolute installed path, SHA-256 and file mode.
//
// This module re-derives the SAME receipt path at worker startup, re-verifies the
// receipt against the agent's compile-time expectation of the package it targets, then
// re-verifies every recorded member ON DISK (stat + recompute SHA-256 + compare
// mode/path). Advertisement follows ONLY a fully successful probe:
//
//   - a stripped / hand-built image (no installer run) has no receipt        -> not capable
//   - a corrupt / truncated / tampered member (digest or mode drift)         -> not capable
//   - a mismatched image (wrong version, arch or pinned tarball)             -> not capable
//   - an old image predating this receipt                                    -> not capable
//
// so a worker that cannot PROVE it holds the pinned, intact Codex layout keeps serving
// Claude. The probe NEVER executes Codex, searches PATH, starts app-server, or touches
// the network — BY CONSTRUCTION it imports only node's fs/crypto/path built-ins (see
// test/codex-runtime-probe.test.ts, which asserts that import surface). It never throws
// on bad input: any failure resolves to `{ capable: false }` so the worker's
// registration loop is never broken by a probe error. Resolve it ONCE at startup and
// carry the result on Config, mirroring the docker-wiring keystone.

import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { readFile, stat } from "node:fs/promises";
import path from "node:path";

/** The protocol-capability wire string this probe gates. Advertised in worker.ts ONLY
 *  when {@link probeCodexRuntime} returns `capable`. It is a PROTOCOL capability (beside
 *  completion_interlock_v1 / recovery_archive_v1), NOT a user-selectable scheduler
 *  capability, and NOT a required_capability (PRD #1332 D3). */
export const CODEX_HARNESS_CAPABILITY = "codex_harness_v1";

/** PRD #1551 (D6): the protocol capability advertised ONLY by a worker whose Codex
 *  renderer can pass a validated CUSTOM (non-curated) worker-root model through unchanged
 *  (agent/src/codex/render.ts). The API gates every non-bypassable placement seam on it for
 *  a run whose effective Codex root is a custom model, so an old `codex_harness_v1`-only
 *  worker cannot claim such a run and silently substitute `gpt-6-astra`. Advertised beside
 *  {@link CODEX_HARNESS_CAPABILITY} in worker.ts, under the SAME condition (this build's
 *  renderer always has the passthrough behavior, so the two are advertised together). */
export const CODEX_CUSTOM_MODEL_CAPABILITY = "codex_custom_model_v1";

/** This build implements completion attempts for Codex turns. Advertise only with the Codex harness. */
export const CODEX_COMPLETION_INTERLOCK_CAPABILITY = "codex_completion_interlock_v1";

/** Default install prefix, matching agent/codex/install-codex.sh's UZI_CODEX_PREFIX
 *  default. The receipt is written at `${prefix}/${version}.receipt.json`. */
const DEFAULT_CODEX_PREFIX = "/opt/uzi-codex";

/**
 * The agent's compile-time expectation of the pinned Codex package it targets.
 *
 * SOURCE OF TRUTH: agent/codex/codex-package.lock. These values are a deliberate,
 * commented duplication of the lock so the probe can cross-check — at runtime, without
 * the lock (which the Dockerfiles delete after install) — that the installed package is
 * the exact one this agent build was written for. A version bump updates BOTH the lock
 * and these constants; if they ever disagree the probe fails CLOSED (no capability),
 * which is the intended behavior, never a silent false advertisement.
 */
export const CODEX_PROBE_EXPECTATION: {
  readonly version: string;
  readonly lockDigest: Readonly<Record<string, string>>;
} = {
  version: "0.156.1",
  lockDigest: {
    amd64: "8b711520beddf385467b8da4d2c93736637c6ba1e46811cf0d8606b7c490b6f6",
    arm64: "fdd47ed6aade0360796fd3f6f95a45096f327c15e19e8c7339f9dc5633041786",
  },
};

/** The resolved probe outcome for this worker's lifetime. `capable` gates the
 *  advertisement; `reason` is a bounded, credential-free diagnostic string set only
 *  when NOT capable (safe to log). */
export interface CodexRuntimeProbeResult {
  capable: boolean;
  reason?: string;
}

/** Options, all injectable so the probe is fully provable against small receipt
 *  fixtures with no real 30MB binary and no root. */
export interface ProbeCodexRuntimeOptions {
  /** Install prefix; default = env UZI_CODEX_PREFIX (trimmed) or {@link DEFAULT_CODEX_PREFIX}. */
  prefix?: string;
  /** Expected codex-cli version; default = {@link CODEX_PROBE_EXPECTATION}.version. */
  expectedVersion?: string;
  /** Runtime architecture (`amd64` | `arm64`); default = mapped from `process.arch`. */
  arch?: string;
  /** Expected pinned tarball SHA256 for `arch`; default = the matching lock digest. */
  expectedLockDigest?: string;
  /** Env source for the prefix default; defaults to `process.env`. */
  env?: NodeJS.ProcessEnv;
}

/** A single member entry as recorded in the receipt. */
interface ReceiptMember {
  member?: unknown;
  path?: unknown;
  sha256?: unknown;
  mode?: unknown;
}

/** The receipt shape written by install-codex.sh. All fields are validated defensively
 *  (the file is a build artifact, but the probe must never trust it structurally). */
interface Receipt {
  codexVersion?: unknown;
  targetArch?: unknown;
  lockDigest?: unknown;
  members?: unknown;
}

/** Local, dependency-free error stringify — inlined so this module imports ONLY node's
 *  fs/crypto/path built-ins (the "no exec / no network by construction" guarantee the
 *  test asserts over the import surface). */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function notCapable(reason: string): CodexRuntimeProbeResult {
  return { capable: false, reason };
}

/** Map node's `process.arch` to the receipt/lock arch vocabulary; undefined for an
 *  unsupported arch (fails the probe closed). */
function mapNodeArch(nodeArch: string): string | undefined {
  if (nodeArch === "x64") return "amd64";
  if (nodeArch === "arm64") return "arm64";
  return undefined;
}

/** Stream-hash a file to a lowercase hex SHA-256. Uses fs.createReadStream only; never
 *  executes anything. Rejects on a read error (caller maps to not-capable). */
function sha256File(abs: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const hash = createHash("sha256");
    const stream = createReadStream(abs);
    stream.on("error", reject);
    stream.on("data", (chunk) => hash.update(chunk));
    stream.on("end", () => resolve(hash.digest("hex")));
  });
}

/**
 * Probe the pinned Codex runtime layout and decide capability. Reads and validates the
 * root-owned receipt, then verifies every recorded member on disk. Returns
 * `{ capable: true }` only when the version, arch, lock digest, and EVERY member's
 * absolute path, mode and SHA-256 match; otherwise `{ capable: false, reason }`. Never
 * throws, never executes Codex, never searches PATH or starts app-server, never uses the
 * network.
 */
export async function probeCodexRuntime(opts: ProbeCodexRuntimeOptions = {}): Promise<CodexRuntimeProbeResult> {
  try {
    const env = opts.env ?? process.env;
    const prefix = opts.prefix ?? (env.UZI_CODEX_PREFIX?.trim() || DEFAULT_CODEX_PREFIX);
    const expectedVersion = opts.expectedVersion ?? CODEX_PROBE_EXPECTATION.version;
    const arch = opts.arch ?? mapNodeArch(process.arch);
    if (!arch) return notCapable(`unsupported runtime arch ${process.arch}`);
    const expectedLockDigest = opts.expectedLockDigest ?? CODEX_PROBE_EXPECTATION.lockDigest[arch];
    if (!expectedLockDigest) return notCapable(`no pinned lock digest for arch ${arch}`);

    const receiptPath = path.join(prefix, `${expectedVersion}.receipt.json`);

    let raw: string;
    try {
      raw = await readFile(receiptPath, "utf8");
    } catch {
      // Absent/unreadable receipt = a stripped, hand-built or old image. Fail closed.
      return notCapable(`receipt not readable at ${receiptPath}`);
    }

    let receipt: Receipt;
    try {
      receipt = JSON.parse(raw) as Receipt;
    } catch {
      return notCapable(`receipt is not valid JSON at ${receiptPath}`);
    }
    if (typeof receipt !== "object" || receipt === null) {
      return notCapable(`receipt is not a JSON object at ${receiptPath}`);
    }

    // --- verify version + arch + lock digest against the compile-time expectation ---
    if (receipt.codexVersion !== expectedVersion) {
      return notCapable(`receipt version ${String(receipt.codexVersion)} != expected ${expectedVersion}`);
    }
    if (receipt.targetArch !== arch) {
      return notCapable(`receipt arch ${String(receipt.targetArch)} != runtime ${arch}`);
    }
    if (receipt.lockDigest !== expectedLockDigest) {
      return notCapable(`receipt lock digest does not match the pinned tarball for ${arch}`);
    }

    const members = receipt.members;
    if (!Array.isArray(members) || members.length === 0) {
      return notCapable("receipt has no members");
    }

    // The version root the receipt's members MUST live under, derived the SAME way as
    // the installer (`${prefix}/${version}/…`). Every member's recorded absolute path is
    // required to equal the path we derive from its relative member name, both to catch a
    // relocated/mismatched receipt and to reject any `..` traversal in the member name.
    const versionRoot = path.join(prefix, expectedVersion);

    for (const entry of members as ReceiptMember[]) {
      if (
        !entry ||
        typeof entry !== "object" ||
        typeof entry.member !== "string" ||
        typeof entry.path !== "string" ||
        typeof entry.sha256 !== "string" ||
        typeof entry.mode !== "string"
      ) {
        return notCapable("receipt member entry is malformed");
      }
      const expectedAbs = path.join(versionRoot, entry.member);
      const rel = path.relative(versionRoot, expectedAbs);
      if (rel.startsWith("..") || path.isAbsolute(rel)) {
        return notCapable(`receipt member ${entry.member} escapes the version root`);
      }
      if (entry.path !== expectedAbs) {
        return notCapable(`receipt member path ${entry.path} != expected ${expectedAbs}`);
      }

      let st;
      try {
        st = await stat(expectedAbs);
      } catch {
        return notCapable(`member missing on disk: ${expectedAbs}`);
      }
      if (!st.isFile()) {
        return notCapable(`member is not a regular file: ${expectedAbs}`);
      }
      const mode = (st.mode & 0o777).toString(8);
      if (mode !== entry.mode) {
        return notCapable(`member mode mismatch for ${expectedAbs}: on-disk ${mode} != receipt ${entry.mode}`);
      }

      let digest: string;
      try {
        digest = await sha256File(expectedAbs);
      } catch (err) {
        return notCapable(`could not hash member ${expectedAbs}: ${errText(err)}`);
      }
      if (digest !== entry.sha256) {
        return notCapable(`member digest mismatch for ${expectedAbs}`);
      }
    }

    return { capable: true };
  } catch (err) {
    // Belt-and-suspenders: any unexpected error is "not capable", never a throw that
    // could break the worker's registration loop.
    return notCapable(`codex runtime probe error: ${errText(err)}`);
  }
}
