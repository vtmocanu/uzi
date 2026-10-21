// Every tracked place that pins the Codex native package version or digest, and the
// sync that rewrites them from agent/codex/codex-package.lock (#1565). The lock stays the
// single source of truth; Renovate updates only the lock, so the lander runs
// `task codex-pins:sync` on its own maintainer branch and commits the result. The
// coordination and parity tests (codex-runtime-coordination.test.ts,
// codex-pin-parity.test.ts) remain the fail-closed check: a lock-only edit is red there.
//
// Dev tooling kept under test/ (a non-shipping path): it must never reach the worker image,
// whose agent/src changes roll the hosted fleet at release.
import { spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const SEMVER = String.raw`[0-9]+\.[0-9]+\.[0-9]+`;

/** The live-pin scan: a version token after one of these prefixes, in any tracked file
 *  outside the exclusions. Shared with codex-runtime-coordination.test.ts. */
export const PIN_SCAN_PATTERN = `(/opt/uzi-codex/|codex-cli |Codex worker uses runtime )(${SEMVER})`;
const PIN_SCAN_EXCLUDES = [
  ":(exclude)adr/**", ":(exclude)prds/**", ":(exclude)CHANGELOG.md", ":(exclude)changelog.d/**",
  // Recorded execution evidence, not the current worker contract.
  ":(exclude)e2e/worker-uid/issue-2397-investigation.md",
  // Synthetic version output deliberately includes mismatches to test the verifier.
  ":(exclude)e2e/codex-m4/provision.test.ts",
];

/** Pins the scan pattern cannot see: each is a file plus an anchored matcher whose
 *  capture group 2 is the value, and the exact number of matches the file must hold. */
interface ExplicitSite {
  file: string;
  pattern: RegExp;
  count: number;
  value: (lock: CodexLock) => string;
}
const version = (lock: CodexLock): string => lock.version;
export const EXPLICIT_PIN_SITES: readonly ExplicitSite[] = [
  { file: "agent/src/codex/codex-runtime-probe.ts", pattern: new RegExp(`(\\n  version: ")(${SEMVER})(",\\n)`, "g"), count: 1, value: version },
  { file: "agent/src/codex/codex-runtime-probe.ts", pattern: /(\n {4}amd64: ")([0-9a-f]{64})(",\n)/g, count: 1, value: (lock) => lock.digests.amd64 },
  { file: "agent/src/codex/codex-runtime-probe.ts", pattern: /(\n {4}arm64: ")([0-9a-f]{64})(",\n)/g, count: 1, value: (lock) => lock.digests.arm64 },
  ...["e2e/codex-m4/large-resume.test.ts", "e2e/codex-m4/token-resume.test.ts"].flatMap((file) => [
    { file, pattern: new RegExp(`(assertBinaryVersion\\(bin, ")(${SEMVER})("\\))`, "g"), count: 1, value: version },
    { file, pattern: new RegExp(`(\\bversion: ")(${SEMVER})(")`, "g"), count: 1, value: version },
  ]),
];

export interface CodexLock {
  version: string;
  digests: { amd64: string; arm64: string };
}

/** Parse and cross-check the lock; any missing or disagreeing field throws. */
export function readCodexLock(text: string): CodexLock {
  const field = (key: string): string => {
    const entries = text.split("\n").filter((line) => line.startsWith(`${key}=`));
    if (entries.length !== 1) throw new Error(`lock: expected exactly one ${key}=, found ${entries.length}`);
    return entries[0]!.slice(key.length + 1);
  };
  const v = field("CODEX_VERSION");
  if (!new RegExp(`^${SEMVER}$`).test(v)) throw new Error(`lock: CODEX_VERSION ${JSON.stringify(v)} is not x.y.z`);
  if (field("CODEX_MANIFEST_VERSION") !== v) throw new Error("lock: CODEX_MANIFEST_VERSION disagrees with CODEX_VERSION");
  const tag = field("CODEX_TAG");
  if (tag !== `rust-v${v}`) throw new Error(`lock: CODEX_TAG ${tag} is not rust-v${v}`);
  const digests = { amd64: "", arm64: "" };
  for (const arch of ["amd64", "arm64"] as const) {
    if (field(`CODEX_TAG_${arch}`) !== tag) throw new Error(`lock: CODEX_TAG_${arch} disagrees with CODEX_TAG`);
    const digest = field(`CODEX_SHA256_${arch}`);
    if (!/^[0-9a-f]{64}$/.test(digest)) throw new Error(`lock: CODEX_SHA256_${arch} is not a sha256`);
    digests[arch] = digest;
  }
  return { version: v, digests };
}

/** Tracked files holding a scan-pattern match, relative to the repository root. */
export function scannedPinFiles(root: string): string[] {
  const scan = spawnSync("git", [
    "-c", "core.fsmonitor=false", "grep", "--no-color", "-I", "-l", "-z", "-E", PIN_SCAN_PATTERN, "--", ".",
    ...PIN_SCAN_EXCLUDES,
  ], { cwd: root, encoding: "utf8", maxBuffer: 1024 * 1024 });
  if (scan.error) throw scan.error;
  if (scan.status !== 0) throw new Error(`git grep found no live Codex pins (status ${scan.status}): ${scan.stderr}`);
  return scan.stdout.split("\0").filter(Boolean);
}

/** Compute every rewritten file from the lock. Throws, before anything is written, when an
 *  explicit site does not match exactly its expected count. Returns only changed files. */
export function planPinSync(root: string, lock: CodexLock): Map<string, string> {
  const files = new Map<string, string>();
  const read = (file: string): string => {
    let text = files.get(file);
    if (text === undefined) {
      text = readFileSync(join(root, file), "utf8");
      files.set(file, text);
    }
    return text;
  };
  const original = new Map<string, string>();
  for (const file of scannedPinFiles(root)) {
    const text = read(file);
    original.set(file, text);
    files.set(file, text.replace(new RegExp(PIN_SCAN_PATTERN, "g"), (_match, prefix: string) => `${prefix}${lock.version}`));
  }
  for (const site of EXPLICIT_PIN_SITES) {
    const text = read(site.file);
    if (!original.has(site.file)) original.set(site.file, text);
    const found = [...text.matchAll(site.pattern)].length;
    if (found !== site.count) {
      throw new Error(`${site.file}: expected ${site.count} match(es) of ${site.pattern}, found ${found}; refusing to write anything`);
    }
    files.set(site.file, text.replace(site.pattern, (_m, head: string, _old: string, tail: string) => `${head}${site.value(lock)}${tail}`));
  }
  for (const [file, text] of files) {
    if (original.get(file) === text) files.delete(file);
  }
  return files;
}

/** Plan, then write. Validation failures write nothing; a filesystem error part-way
 *  through is reported with the files already written (no transactional guarantee). */
export function syncCodexPins(root: string): string[] {
  const lock = readCodexLock(readFileSync(join(root, "agent/codex/codex-package.lock"), "utf8"));
  const plan = planPinSync(root, lock);
  const written: string[] = [];
  for (const [file, text] of plan) {
    try {
      writeFileSync(join(root, file), text);
    } catch (error) {
      throw new Error(`writing ${file} failed after ${written.length} file(s) were rewritten (${written.join(", ") || "none"}): ${String(error)}`);
    }
    written.push(file);
  }
  return written;
}
