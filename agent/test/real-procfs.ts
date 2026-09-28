// issue #1863 — ONE shared answer to "may a test that reads the REAL process table run here?".
//
// In the Landlock-confined Codex command sandbox, enumerating the proc root is denied (readdir
// throws EACCES) and so is reading files under it, while `fs.existsSync(<proc>/self/status)` stays
// true: stat is not a Landlock-handled access. The old `HAS_PROCFS` existence guards therefore let
// real-procfs tests run there and fail. This helper probes the one thing those tests need,
// directory enumeration of the proc root, and skips ONLY when that is denied with EACCES/EPERM.
// Any other outcome (it enumerates, ENOENT, anything else) returns false, so the test runs and a
// real regression fails loudly instead of skipping quietly. It never reads a file under the root:
// a test's own failed single-file read is never a skip condition.
//
// Every skip is recorded to stderr and, when SKIP_LOG_ENV (UZI_REAL_PROCFS_SKIP_LOG) names a file, appended to it
// as `label<TAB>reason`. scripts/real-procfs-skip-check.sh (wrapping `task test:agent`) reads that
// log and fails the run when a skip happened where enumeration works, or in CI at all.

import fs from "node:fs";
import path from "node:path";

type Readdir = (dir: string) => unknown;

/** Probe `root` for directory enumeration: a skip reason when it is DENIED (EACCES/EPERM), else false. */
export function probeProcEnumeration(readdir: Readdir = fs.readdirSync, root: string = path.join("/", "proc")): string | false {
  try {
    readdir(root);
    return false;
  } catch (err) {
    const code = (err as NodeJS.ErrnoException | null)?.code;
    if (code !== "EACCES" && code !== "EPERM") return false;
    return (
      `real-procfs-denied: scandir ${root}: ${code} ` +
      "(directory enumeration of the proc root is denied, e.g. the Landlock command sandbox; " +
      "this test needs the real process table)"
    );
  }
}

/** Computed once per test process, at module load. */
export const REAL_PROCFS_DENIED: string | false = probeProcEnumeration();

/** The env var naming the skip log that scripts/real-procfs-skip-check.sh creates and reads. */
export const SKIP_LOG_ENV = "UZI_REAL_PROCFS_SKIP_LOG";

/** Where a skip's human-readable line goes; injectable so unit tests print nothing. */
type Write = (line: string) => unknown;
const writeStderr: Write = (line) => process.stderr.write(line);

/**
 * The recording core of {@link realProcfsSkip}, with the denied value, log path and stderr writer
 * injectable so it is testable without the real proc root. Returns false when `denied` is false
 * (nothing recorded).
 */
export function skipFor(
  label: string,
  denied: string | false,
  logPath: string | undefined,
  write: Write = writeStderr,
): string | false {
  if (denied === false) return false;
  // One record per line, two tab-separated fields: a label's own tabs/newlines become spaces.
  label = label.replace(/[\t\r\n]/g, " ");
  const reason = `${denied} [${label}]`;
  write(`real-procfs skip: ${reason}\n`);
  // Append, never truncate: test files run in parallel child processes sharing this one log.
  if (logPath) fs.appendFileSync(logPath, `${label}\t${reason}\n`);
  return reason;
}

/**
 * A `skip` value for a test that reads the real process table: the reason when denied, else false.
 * Records to the log named by {@link SKIP_LOG_ENV}. `denied` and `write` are injectable for tests
 * only; real callers pass just the label.
 */
export function realProcfsSkip(
  label: string,
  denied: string | false = REAL_PROCFS_DENIED,
  write: Write = writeStderr,
): string | false {
  return skipFor(label, denied, process.env[SKIP_LOG_ENV] || undefined, write);
}
