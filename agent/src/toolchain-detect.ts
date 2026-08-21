// PRD #84 M4 — AGENT-side deterministic toolchain/capability inference.
//
// Capability-aware scheduling needs a run's requirement set to be known AT PLAN TIME
// so the server can gate plan-approval on whether an eligible worker exists. That
// requirement set MUST be deterministic — derived from what the cloned repo actually
// contains, NOT from LLM prose, which cannot be gated on. This module is that
// deterministic function: given a fresh runner clone, it walks the tree and infers a
// small, high-signal set of non-provisionable capabilities and provisionable tools.
//
// THE SCAN MIRRORS `discoverJsProjects` in js-deps.ts (the scan precedent in this
// package): a breadth-first, bounded, best-effort walk that never follows symlinks and
// never throws. That discipline is load-bearing rather than incidental — a repo the user
// controls must cost a fixed, small amount of work, and a symlink to `/` or `..` must not
// let the walk (or any decision derived from it) escape the clone. `Dirent.isDirectory()`
// is lstat-based, so a symlinked directory lands among the files and is never descended,
// exactly as in js-deps; keep that behavior. See js-deps.ts for the full rationale on each
// bound and on why the symlink property must survive a refactor.
//
// Detection is HIGH-SIGNAL ONLY. It does not attempt to enumerate transitive CLIs a repo
// might shell out to — that is unbounded and unknowable from a static tree. It keys on a
// handful of unambiguous markers (a Dockerfile, a manifest declaring testcontainers, a
// build manifest for the JVM, a language's project file). A false negative is cheap (the
// requirement is simply not asserted); a false positive would over-constrain scheduling,
// so the rules stay conservative.

import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

export interface ToolchainDetection {
  /** Non-provisionable capabilities a worker must already have, from the CLOSED
   *  vocabulary {docker, jvm}. A worker cannot be provisioned with these at run time,
   *  so they gate which workers are eligible to claim the run. */
  required_capabilities: string[];
  /** Provisionable toolchain names (e.g. go, node, python, rust, jvm). A worker without
   *  one can install it, so these steer provisioning rather than hard-gating eligibility. */
  required_tools: string[];
  /** A coarse, SOFT/display-only size bucket derived from how many directories the scan
   *  read. Never used to gate anything — it exists purely to give a human a sense of the
   *  repo's shape. */
  size_class: "s" | "m" | "l";
}

/** Max directory depth below the clone root the scan descends. Matches
 *  `js-deps.ts` MAX_SCAN_DEPTH: real project markers sit shallow, and a marker deeper
 *  than this is far likelier to be a test fixture or a vendored copy than a signal about
 *  the repo itself. Exported so the test can pin the boundary. */
export const MAX_SCAN_DEPTH = 4;

/** Max directories READ during the walk (matches `js-deps.ts` MAX_SCAN_DIRS). The only
 *  bound on the walk's own cost: a repo with tens of thousands of directories at shallow
 *  depth would otherwise readdir() all of them. */
const MAX_SCAN_DIRS = 2000;

/** Directories never descended into, mirroring `js-deps.ts` SKIP_DIRS. `node_modules`
 *  holds thousands of a dependency's own manifests (not this repo's), and `.git` is pure
 *  walk cost with nothing inferable inside. */
const SKIP_DIRS = new Set(["node_modules", ".git"]);

/** Files that, present in ANY directory, mean the repo builds or runs containers. */
const DOCKER_MARKERS = [
  "Dockerfile",
  "docker-compose.yml",
  "docker-compose.yaml",
  "compose.yml",
  "compose.yaml",
];

/** Manifest files whose TEXT is searched for a `testcontainers` dependency — a very
 *  strong signal the test suite needs a working Docker daemon. */
const TESTCONTAINERS_MANIFESTS = [
  "package.json",
  "go.mod",
  "pyproject.toml",
  "requirements.txt",
  "Cargo.toml",
  "pom.xml",
  "build.gradle",
];

/** JVM build manifests: present in ANY directory ⇒ both the `jvm` capability and the
 *  `jvm` tool. `build.gradle.kts` is the Kotlin-DSL spelling of `build.gradle`. */
const JVM_MARKERS = ["pom.xml", "build.gradle", "build.gradle.kts"];

/** Read a file's text best-effort — an unreadable/vanished file yields "" rather than
 *  throwing, keeping the whole scan best-effort. */
async function readTextBestEffort(abs: string): Promise<string> {
  try {
    return await readFile(abs, "utf8");
  } catch {
    return "";
  }
}

/** Coarse size bucket from the number of directories the scan read. Thresholds are
 *  deliberately simple and soft — this is display-only and gates nothing:
 *    < 20 dirs   ⇒ "s"   (a small/single-package repo)
 *    < 150 dirs  ⇒ "m"   (a typical multi-package repo)
 *    otherwise   ⇒ "l"   (a large monorepo, or the scan hit its cost bound). */
function sizeClassFor(dirCount: number): "s" | "m" | "l" {
  if (dirCount < 20) return "s";
  if (dirCount < 150) return "m";
  return "l";
}

/**
 * Deterministically infer a run's requirement set from a cloned repo at `clonePath`.
 *
 * Best-effort by construction: an unreadable directory or file is skipped, never thrown,
 * so the caller can wrap the whole thing knowing it resolves rather than rejecting. The
 * walk is bounded (depth + total dirs) and never follows symlinks, mirroring
 * `discoverJsProjects`.
 *
 * Rules (high-signal only):
 *   capability "docker" — any dir has a Docker/compose marker file, OR a manifest names a
 *     `testcontainers` dependency, OR a ROOT-LEVEL build/test script (`Makefile`,
 *     `Taskfile.yml`, any `*.sh`) contains a `docker ` token.
 *   capability "jvm"    — any dir has `pom.xml`, `build.gradle`, or `build.gradle.kts`.
 *   tools               — go.mod⇒go, package.json⇒node, pyproject.toml|requirements.txt⇒
 *                         python, Cargo.toml⇒rust, JVM manifest⇒jvm.
 *
 * Each output array is de-duplicated and returned in STABLE sorted order.
 */
export async function detectToolchain(clonePath: string): Promise<ToolchainDetection> {
  const capabilities = new Set<string>();
  const tools = new Set<string>();

  const queue: { abs: string; rel: string; depth: number }[] = [
    { abs: clonePath, rel: ".", depth: 0 },
  ];
  let scanned = 0;

  while (queue.length > 0) {
    if (scanned >= MAX_SCAN_DIRS) break;
    const cur = queue.shift()!;
    scanned++;

    let entries;
    try {
      entries = await readdir(cur.abs, { withFileTypes: true });
    } catch {
      continue; // unreadable/vanished dir: skip it, detection is best-effort
    }

    // isDirectory() is lstat-based, so a symlink lands here (a "file"), which is exactly
    // why a symlinked directory is never descended below — mirroring js-deps.
    const files = new Set(entries.filter((e) => !e.isDirectory()).map((e) => e.name));

    // ── Docker: marker files present in ANY directory ──
    if (DOCKER_MARKERS.some((m) => files.has(m))) capabilities.add("docker");

    // ── Docker: a manifest naming a testcontainers dependency ──
    for (const manifest of TESTCONTAINERS_MANIFESTS) {
      if (!files.has(manifest)) continue;
      const text = await readTextBestEffort(join(cur.abs, manifest));
      if (text.includes("testcontainers")) {
        capabilities.add("docker");
        break;
      }
    }

    // ── Docker: ROOT-LEVEL build/test scripts mentioning `docker ` (kept cheap) ──
    if (cur.rel === ".") {
      const scripts = [...files].filter(
        (f) => f === "Makefile" || f === "Taskfile.yml" || f.endsWith(".sh"),
      );
      for (const script of scripts) {
        const text = await readTextBestEffort(join(cur.abs, script));
        if (text.includes("docker ")) {
          capabilities.add("docker");
          break;
        }
      }
    }

    // ── JVM capability + tool ──
    if (JVM_MARKERS.some((m) => files.has(m))) {
      capabilities.add("jvm");
      tools.add("jvm");
    }

    // ── Language tools ──
    if (files.has("go.mod")) tools.add("go");
    if (files.has("package.json")) tools.add("node");
    if (files.has("pyproject.toml") || files.has("requirements.txt")) tools.add("python");
    if (files.has("Cargo.toml")) tools.add("rust");

    if (cur.depth >= MAX_SCAN_DEPTH) continue;
    const subdirs = entries
      .filter((e) => e.isDirectory() && !SKIP_DIRS.has(e.name))
      .map((e) => e.name)
      .sort();
    for (const name of subdirs) {
      queue.push({
        abs: join(cur.abs, name),
        rel: cur.rel === "." ? name : `${cur.rel}/${name}`,
        depth: cur.depth + 1,
      });
    }
  }

  return {
    required_capabilities: [...capabilities].sort(),
    required_tools: [...tools].sort(),
    size_class: sizeClassFor(scanned),
  };
}
