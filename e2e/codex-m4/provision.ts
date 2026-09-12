// PRD #1287 C1 — the Codex package provisioner + cache receipt (D7 points 1-2).
//
// The P smoke needs the pinned Codex binary. This module resolves it, PREFERRING the
// image-baked absolute package at /opt/uzi-codex/<version> when its version and layout match
// (the uzi worker's path — no install, no image build). On a Linux contributor/CI host WITHOUT
// that package it runs agent/codex/install-codex.sh into a rootless, gitignored cache prefix
// and writes a receipt binding {version, arch, lockDigest}; a later run reuses the cache only
// when the receipt matches AND the layout is complete, else it revalidates/reinstalls.
//
// HARD PREREQUISITE FAILURES (throw, NEVER a skip): wrong binary version, unsupported
// architecture, an unexpected /etc/codex present, or an incomplete package. The pure decision
// helpers (arch, lock parse, digest, receipt match, reinstall decision) and the hard-prereq
// branches are unit-tested with synthetic inputs (provision.test.ts) — no real install runs in
// a test; on this worker the image-baked path is taken and the smoke needs no install.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";

/** A hard prerequisite failure. Never caught to produce a platform skip. */
export class CodexProvisionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CodexProvisionError";
  }
}

export type Arch = "amd64" | "arm64";

/** The package members whose presence makes a package "layout complete": the CLI plus its
 *  code-mode-host sibling (the real code-mode path needs the host next to the CLI). */
export const REQUIRED_MEMBERS = ["bin/codex", "bin/codex-code-mode-host"] as const;

/** Map a Node `process.arch` to the lock's TARGETARCH. Unsupported → HARD failure. */
export function resolveArch(nodeArch: string): Arch {
  if (nodeArch === "x64") return "amd64";
  if (nodeArch === "arm64") return "arm64";
  throw new CodexProvisionError(`unsupported architecture "${nodeArch}" — expected x64 (amd64) or arm64`);
}

/** The version + per-arch SHA256 pins parsed out of codex-package.lock. */
export interface LockFacts {
  readonly version: string;
  readonly sha256: Readonly<Record<Arch, string>>;
}

const HEX64 = /^[0-9a-f]{64}$/;

function lockValue(lockText: string, key: string): string {
  for (const line of lockText.split("\n")) {
    const trimmed = line.trim();
    if (trimmed.startsWith(`${key}=`)) return trimmed.slice(key.length + 1).trim();
  }
  throw new CodexProvisionError(`codex-package.lock is missing "${key}"`);
}

/** Parse CODEX_VERSION + CODEX_SHA256_<arch> out of the lock (fail-closed on a bad pin). */
export function parseLock(lockText: string): LockFacts {
  const version = lockValue(lockText, "CODEX_VERSION");
  if (version.length === 0) throw new CodexProvisionError("codex-package.lock CODEX_VERSION is empty");
  const amd64 = lockValue(lockText, "CODEX_SHA256_amd64");
  const arm64 = lockValue(lockText, "CODEX_SHA256_arm64");
  for (const [arch, sha] of [["amd64", amd64], ["arm64", arm64]] as const) {
    if (!HEX64.test(sha)) throw new CodexProvisionError(`codex-package.lock CODEX_SHA256_${arch} is not a 64-hex digest`);
  }
  return { version, sha256: { amd64, arm64 } };
}

/** Content-address the lock so a receipt is invalidated by ANY lock change (D7 point 2). */
export function lockDigest(lockText: string): string {
  return createHash("sha256").update(lockText, "utf8").digest("hex");
}

/** The cache receipt binding a rootless install to its exact inputs. */
export interface Receipt {
  readonly version: string;
  readonly arch: Arch;
  readonly lockDigest: string;
}

/** True when a parsed receipt matches the expected {version, arch, lockDigest}. A version
 *  text match alone is NOT trusted — the lock digest must match too (D7 point 2). */
export function receiptMatches(receipt: unknown, expected: Receipt): boolean {
  if (receipt === null || typeof receipt !== "object") return false;
  const r = receipt as Record<string, unknown>;
  return r.version === expected.version && r.arch === expected.arch && r.lockDigest === expected.lockDigest;
}

/** True when every required member exists under `prefix`. Injectable `exists` for tests. */
export function layoutComplete(prefix: string, exists: (p: string) => boolean = existsSync): boolean {
  return REQUIRED_MEMBERS.every((member) => exists(path.join(prefix, member)));
}

export type ProvisionDecision = "use-cache" | "reinstall";

/** Pure reinstall decision: reuse the cache ONLY when the layout is complete AND the receipt
 *  matches; incomplete or stale contents are revalidated/reinstalled, never trusted by version
 *  text alone. */
export function decideProvision(input: {
  readonly receipt: unknown;
  readonly layoutComplete: boolean;
  readonly expected: Receipt;
}): ProvisionDecision {
  return input.layoutComplete && receiptMatches(input.receipt, input.expected) ? "use-cache" : "reinstall";
}

/** HARD failure if a system Codex config root exists — a fresh HOME does not neutralize it.
 *  Injectable lstat for tests (default: throwIfNoEntry:false lstat of /etc/codex). */
export function assertNoUnexpectedEtcCodex(
  lstat: (p: string) => { present: boolean } = defaultEtcLstat,
  etcCodexDir = "/etc/codex",
): void {
  if (lstat(etcCodexDir).present) {
    throw new CodexProvisionError(`unexpected system Codex configuration at ${etcCodexDir}; refusing to provision`);
  }
}

function defaultEtcLstat(p: string): { present: boolean } {
  return { present: lstatSync(p, { throwIfNoEntry: false }) !== undefined };
}

/** The minimal command runner the version assertion needs; injectable for tests. */
export type VersionRunner = (bin: string, args: readonly string[]) => { status: number | null; stdout: string };

function defaultVersionRunner(bin: string, args: readonly string[]): { status: number | null; stdout: string } {
  const r = spawnSync(bin, [...args], { encoding: "utf8" });
  return { status: r.status, stdout: String(r.stdout ?? "") };
}

/** HARD failure unless `<codexBin> --version` is exactly `codex-cli <version>`. */
export function assertBinaryVersion(codexBin: string, version: string, run: VersionRunner = defaultVersionRunner): void {
  const r = run(codexBin, ["--version"]);
  if (r.status !== 0) {
    throw new CodexProvisionError(`codex --version exited ${String(r.status)} at ${codexBin}`);
  }
  const text = r.stdout.trim();
  if (text !== `codex-cli ${version}`) {
    throw new CodexProvisionError(`codex binary version mismatch at ${codexBin}: got "${text}", expected "codex-cli ${version}"`);
  }
}

/** The resolved package. `source` records which path was taken. */
export interface ProvisionResult {
  readonly codexBin: string;
  readonly prefix: string;
  readonly source: "image-baked" | "cache-install";
}

/** Injectable seams for {@link resolveCodexBin}; production uses the real fs/spawn defaults. */
export interface ProvisionDeps {
  readonly nodeArch?: string;
  readonly lockPath?: string;
  readonly imageRoot?: string;
  readonly cachePrefix?: string;
  readonly etcLstat?: (p: string) => { present: boolean };
  readonly versionRunner?: VersionRunner;
}

const M4_DIR = __dirname;
const REPO_ROOT = path.resolve(M4_DIR, "..", "..");
const DEFAULT_LOCK_PATH = path.join(REPO_ROOT, "agent", "codex", "codex-package.lock");
const DEFAULT_IMAGE_ROOT = "/opt/uzi-codex";
const DEFAULT_CACHE_PREFIX = path.join(M4_DIR, ".codex-cache");
const INSTALL_SCRIPT = path.join(REPO_ROOT, "agent", "codex", "install-codex.sh");

/**
 * Resolve the pinned Codex binary, preferring the image-baked package and otherwise
 * installing into the rootless cache. Used by the P smoke (which, on this worker, always takes
 * the image-baked branch). Every prerequisite failure throws {@link CodexProvisionError}.
 */
export function resolveCodexBin(deps: ProvisionDeps = {}): ProvisionResult {
  assertNoUnexpectedEtcCodex(deps.etcLstat ?? defaultEtcLstat);
  const arch = resolveArch(deps.nodeArch ?? process.arch);
  const lockPath = deps.lockPath ?? DEFAULT_LOCK_PATH;
  const facts = parseLock(readFileSync(lockPath, "utf8"));
  const { version } = facts;
  const run = deps.versionRunner ?? defaultVersionRunner;

  // 1. Prefer the image-baked absolute package.
  const imagePrefix = path.join(deps.imageRoot ?? DEFAULT_IMAGE_ROOT, version);
  if (layoutComplete(imagePrefix)) {
    const codexBin = path.join(imagePrefix, "bin", "codex");
    assertBinaryVersion(codexBin, version, run); // HARD on mismatch (never a skip)
    return { codexBin, prefix: imagePrefix, source: "image-baked" };
  }

  // 2. Rootless cache install (Linux contributor/CI host without the baked package).
  const cachePrefix = deps.cachePrefix ?? DEFAULT_CACHE_PREFIX;
  const installedPrefix = path.join(cachePrefix, version);
  const lockText = readFileSync(lockPath, "utf8");
  const expected: Receipt = { version, arch, lockDigest: lockDigest(lockText) };
  const receiptPath = path.join(cachePrefix, "receipt.json");
  const receipt = readReceipt(receiptPath);
  const decision = decideProvision({ receipt, layoutComplete: layoutComplete(installedPrefix), expected });
  if (decision === "reinstall") {
    mkdirSync(cachePrefix, { recursive: true });
    const install = spawnSync(INSTALL_SCRIPT, [arch], {
      encoding: "utf8",
      env: { ...process.env, UZI_CODEX_PREFIX: cachePrefix, UZI_CODEX_LOCK: lockPath },
    });
    if (install.status !== 0) {
      throw new CodexProvisionError(`install-codex.sh ${arch} exited ${String(install.status)}: ${String(install.stderr)}`);
    }
    writeFileSync(receiptPath, `${JSON.stringify(expected, null, 2)}\n`, "utf8");
  }
  if (!layoutComplete(installedPrefix)) {
    throw new CodexProvisionError(`incomplete Codex package at ${installedPrefix} after provisioning`);
  }
  const codexBin = path.join(installedPrefix, "bin", "codex");
  assertBinaryVersion(codexBin, version, run);
  return { codexBin, prefix: installedPrefix, source: "cache-install" };
}

function readReceipt(receiptPath: string): unknown {
  try {
    return JSON.parse(readFileSync(receiptPath, "utf8"));
  } catch {
    return undefined;
  }
}
