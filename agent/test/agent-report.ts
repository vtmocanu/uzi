// The agent-readable report of one `npm test` (unit-stage) invocation. Agents, not humans, read
// the agent gate: a full spec log is megabytes of passing lines, and a run blocked on a long gate
// sees nothing until it returns. Each invocation writes, under
// $UZI_AGENT_TEST_REPORT_DIR (default agent/.test-report/), a directory <id>/ holding:
//
//   progress.json  live, owned by the reporter (agent-report-reporter.ts), rewritten atomically
//   summary.json   final, owned by the launcher (run-unit-tests.ts), written only after the
//   summary.txt    node --test process group has exited; absent means the invocation never finished
//
// plus latest.json beside the directories, naming the newest invocation.
//
// Fail-closed contract: this covers the UNIT stage only (task's exit code is the gate verdict:
// real-procfs-skip-check.sh, tmpdir-leak-guard.sh and test:codex-m4 run after it). A missing
// summary, a progress file still `running`, or `state: incomplete` is never green, however fresh
// its heartbeat.
import { closeSync, fsyncSync, openSync, renameSync, writeSync } from "node:fs";
import path from "node:path";
import { REPORT_SCHEMA, writeJsonAtomic } from "./agent-report-reporter.js";

export { REPORT_SCHEMA, writeJsonAtomic };
/** A file with no event for this long, once every other file finished, is treated as hung. */
export const DEFAULT_STALL_MS = 600_000;
export type FailureKind = "fail" | "cancelled" | "file" | "suite";

export interface Failure {
  file: string;
  name: string;
  line?: number;
  kind: FailureKind;
  failure_type?: string;
  error?: string;
}

export interface FileState {
  started_at: string;
  last_event_at: string;
  done_at?: string;
  duration_ms?: number;
  passed?: boolean;
}

export interface Counts {
  tests: number;
  pass: number;
  fail: number;
  cancelled: number;
  skip: number;
  todo: number;
  /** Whole-file failures: a top-level throw or exit, not attributed to one test. */
  file_failures: number;
  /** Suites failing in their own right (a hook or the describe body), apart from their tests. */
  suite_failures: number;
}

export interface Progress {
  schema: number;
  scope: "unit";
  /** running: events still flowing; ended: node's event stream finished; interrupted: node said so. */
  state: "running" | "ended" | "interrupted";
  started_at: string;
  /** The reporter's own liveness, kept apart from test activity so a live reporter cannot hide a stall. */
  heartbeat_at: string;
  last_event_at: string | null;
  files_total: number | null;
  files: Record<string, FileState>;
  counts: Counts;
  failures: Failure[];
  failures_omitted: number;
  /** node's own final tally (test:summary), for cross-checking the reporter's counts. */
  node_counts: Record<string, number> | null;
}

export interface Invocation {
  id: string;
  dir: string;
  command: string[];
  shard: string | null;
  filters: string[];
  /** True when the run selected everything: no shard, no name/skip/only filter. */
  full_suite: boolean;
  node: string;
  head: string | null;
  dirty: boolean | null;
  stall_ms: number;
  stall_overridden: boolean;
  started_at: string;
}

export type Termination = "none" | "signal" | "watchdog" | "spawn-error";

export interface Summary {
  schema: number;
  scope: "unit";
  verdict_note: string;
  /** complete: the stream ended and the process exited on its own. Not the same as green. */
  state: "complete" | "incomplete";
  /** complete, exit 0, and no failed/cancelled test or file. Still only the unit stage. */
  unit_green: boolean;
  reason: string | null;
  invocation: Invocation;
  exit: { code: number | null; signal: string | null };
  termination: Termination;
  cleanup: "not-needed" | "verified" | "unverified";
  hung_files: string[];
  in_flight_files: string[];
  finished_at: string;
  progress: Progress | null;
}

export function writeTextAtomic(file: string, text: string): void {
  const temp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.tmp`);
  const fd = openSync(temp, "w", 0o644);
  try {
    writeSync(fd, text);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
  renameSync(temp, file);
}

/** Files in flight (started, not done), oldest activity first. */
export function inFlight(progress: Progress): string[] {
  return Object.entries(progress.files)
    .filter(([, state]) => state.done_at === undefined)
    .sort(([, a], [, b]) => a.last_event_at.localeCompare(b.last_event_at))
    .map(([file]) => file);
}

/**
 * The hung-file verdict: every selected file is either done or silent for at least stallMs, and at
 * least one is silent. A file still queued (no event yet) or recently active blocks it, so the
 * watchdog never cuts coverage short. Silence means no test started or finished in that file: every
 * test is capped by --test-timeout, but a leaked handle keeps the file's process alive forever after
 * its last test, and node's file-level completion never arrives.
 */
export function hungFiles(progress: Progress, total: number, now: number, stallMs: number): string[] {
  const pending = inFlight(progress);
  const done = Object.keys(progress.files).length - pending.length;
  const silent = pending.filter((file) => now - Date.parse(progress.files[file]!.last_event_at) >= stallMs);
  if (silent.length === 0 || silent.length !== pending.length || done + silent.length !== total) return [];
  return silent;
}

export function renderSummaryText(summary: Summary, failureLines = 20): string {
  const p = summary.progress;
  const c = p?.counts;
  const lines: string[] = [];
  const outcome = summary.state === "complete"
    ? (summary.unit_green ? "COMPLETE, unit tests green" : "COMPLETE, unit tests NOT green")
    : `INCOMPLETE (${summary.reason ?? "unknown"}), NOT green`;
  lines.push(`agent unit tests: ${outcome}; exit=${summary.exit.code ?? "-"}${summary.exit.signal ? ` signal=${summary.exit.signal}` : ""}`);
  lines.push(`scope: unit stage only; the gate verdict is the task's exit code. ${summary.invocation.full_suite
    ? "Full suite" : `FILTERED (shard=${summary.invocation.shard ?? "-"} filters=${summary.invocation.filters.join(" ") || "-"})`}`);
  if (p && c) {
    const done = Object.values(p.files).filter((state) => state.done_at !== undefined).length;
    lines.push(`files ${done}/${p.files_total ?? "?"}  tests ${c.tests}  pass ${c.pass}  fail ${c.fail}  cancelled ${c.cancelled}`
      + `  skip ${c.skip}  todo ${c.todo}  file-failures ${c.file_failures}  suite-failures ${c.suite_failures}`);
    for (const failure of p.failures.slice(0, failureLines)) {
      const where = failure.line === undefined ? failure.file : `${failure.file}:${failure.line}`;
      // A crashed file's first line is node's generic "test failed"; its stderr names the error.
      const errorLines = (failure.error ?? "").split("\n");
      const headline = failure.kind === "file"
        ? errorLines.find((line, index) => index > 0 && /\b\w*Error\b/.test(line)) ?? errorLines[0]
        : errorLines[0];
      const why = headline ? `: ${headline.trim()}` : "";
      lines.push(`${failure.kind.toUpperCase()} ${where} ${JSON.stringify(failure.name)} ${failure.failure_type ?? ""}${why}`);
    }
    const more = p.failures.length - Math.min(p.failures.length, failureLines) + p.failures_omitted;
    if (more > 0) lines.push(`... ${more} more failure(s) in summary.json`);
  } else {
    lines.push("no progress was recorded");
  }
  for (const file of summary.hung_files) lines.push(`HUNG ${file} (no test event for the stall window after every other file finished)`);
  if (summary.state === "incomplete" && summary.in_flight_files.length > 0) {
    lines.push(`in flight when stopped: ${summary.in_flight_files.join(" ")}`);
  }
  if (summary.cleanup === "unverified") lines.push("CLEANUP UNVERIFIED: the test process group may still be running");
  lines.push(`report: ${summary.invocation.dir}`);
  return lines.join("\n") + "\n";
}
