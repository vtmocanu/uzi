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
// The SAME two properties apply to the file READS the scan does (manifests + root build
// scripts): `readTextBestEffort` `lstat`s first and skips anything that is not a REGULAR
// file, so a manifest that is actually a symlink (e.g. `go.mod` → `/dev/zero`) is never
// followed, and it caps the read at MAX_READ_BYTES so a multi-GB regular manifest cannot
// OOM the worker. Never reintroduce a bare `readFile` here — it follows symlinks and is
// unbounded, which breaks both guarantees.
//
// Detection is HIGH-SIGNAL ONLY. It does not attempt to enumerate transitive CLIs a repo
// might shell out to — that is unbounded and unknowable from a static tree. It keys on a
// handful of unambiguous markers (a Dockerfile, a manifest declaring testcontainers, a
// build manifest for the JVM, a language's project file). A false negative is cheap (the
// requirement is simply not asserted); a false positive would over-constrain scheduling,
// so the rules stay conservative.

import { lstat, open, readdir } from "node:fs/promises";
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

/** Docker CLI subcommands that unambiguously mean the script INVOKES docker (as opposed to
 *  merely mentioning the word). Requiring one of these after the `docker` token is what
 *  separates a real invocation from prose — a comment like `# migrated off docker to
 *  podman` names docker but is not followed by a subcommand, so it does not over-assert. */
const DOCKER_SUBCOMMANDS =
  "build|buildx|compose|run|exec|push|pull|image|images|ps|rm|rmi|start|stop|restart|" +
  "up|down|network|volume|login|logout|tag|save|load|system|container|cp|logs|inspect|" +
  "commit|create|kill|pause|unpause|port|stats|top|attach|export|import|history|manifest|" +
  "service|swarm|node|secret|config|context|plugin|builder|version|info|init";

/** A `docker`/`docker-compose` invocation at a COMMAND position: line start, or after
 *  whitespace or a shell command separator (`;`, `&`, `|`, `(`, backtick, `$(`), followed
 *  by a recognized subcommand. This replaces the old `text.includes("docker ")` substring,
 *  which tripped on any prose naming docker (a comment, an echo, a variable name). */
const DOCKER_INVOCATION = new RegExp(
  "(?:^|[\\s;&|(`])docker(?:-compose)?\\s+(?:" + DOCKER_SUBCOMMANDS + ")\\b",
);

/** Strip a trailing shell/Makefile comment from a line. A `#` starts a comment when it is
 *  at the line start or preceded by whitespace (the shell rule, approximately); a `#`
 *  inside a word (e.g. `${VAR#prefix}`) is left intact. Stripping first ensures a
 *  subcommand-shaped word inside a comment (`# we run docker compose`) cannot trigger the
 *  invocation match — the over-assertion this fix closes. */
function stripLineComment(line: string): string {
  const m = /(?:^|\s)#/.exec(line);
  return m ? line.slice(0, m.index) : line;
}

/** Whether a build/test script actually INVOKES docker (not merely mentions it). Comments
 *  are stripped per line, then each remaining line is tested for a command-position docker
 *  invocation. Conservative by design: a false negative (missing a docker usage) is cheap,
 *  a false positive over-constrains scheduling. */
function scriptInvokesDocker(text: string): boolean {
  for (const rawLine of text.split("\n")) {
    if (DOCKER_INVOCATION.test(stripLineComment(rawLine))) return true;
  }
  return false;
}

/** Max bytes read from any single manifest/script. The scan only searches a file's text
 *  for short marker substrings (`testcontainers`, a `docker` invocation) that sit near the
 *  top of a real manifest, so reading the whole file buys nothing — while a git-tracked
 *  multi-GB regular file would OOM the worker if read whole. Exported so the test can pin
 *  the boundary. 512 KiB is far larger than any real manifest/build script. */
export const MAX_READ_BYTES = 512 * 1024;

/** Read a file's text best-effort, upholding the module's "never follows symlinks / fixed
 *  small cost" guarantee for the READ path exactly as the directory walk upholds it for
 *  descent:
 *    - `lstat` (which does NOT follow symlinks) rejects anything that is not a REGULAR
 *      file. A manifest that is actually a symlink (e.g. `go.mod` → `/dev/zero`, or → a
 *      huge file outside the clone) is skipped rather than followed, so a hostile repo
 *      cannot make the read escape the clone or read an unbounded device.
 *    - the read is CAPPED at MAX_READ_BYTES via a bounded buffer, so even a legitimately
 *      (or maliciously) multi-GB regular manifest costs a fixed, small amount of memory.
 *  An unreadable/vanished/non-regular file yields "" rather than throwing, keeping the
 *  whole scan best-effort. */
async function readTextBestEffort(abs: string): Promise<string> {
  try {
    const st = await lstat(abs);
    if (!st.isFile()) return ""; // symlink, fifo, device, socket, dir → not read
    const fh = await open(abs, "r");
    try {
      const buf = Buffer.allocUnsafe(MAX_READ_BYTES);
      const { bytesRead } = await fh.read(buf, 0, MAX_READ_BYTES, 0);
      return buf.toString("utf8", 0, bytesRead);
    } finally {
      await fh.close();
    }
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
 *     `Taskfile.yml`, any `*.sh`) INVOKES docker (a command-position `docker`/
 *     `docker-compose <subcommand>`, comments stripped — not a prose mention).
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

    // ── Docker: ROOT-LEVEL build/test scripts that INVOKE docker (kept cheap) ──
    if (cur.rel === ".") {
      const scripts = [...files].filter(
        (f) => f === "Makefile" || f === "Taskfile.yml" || f.endsWith(".sh"),
      );
      for (const script of scripts) {
        const text = await readTextBestEffort(join(cur.abs, script));
        if (scriptInvokesDocker(text)) {
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
