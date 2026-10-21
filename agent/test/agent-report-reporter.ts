// node:test reporter writing progress.json for agents (see agent-report.ts for the contract).
// It runs in the parent `node --test` process, wired by run-unit-tests.ts.
//
// Counts come from `test:complete`, which node emits as each test finishes. `test:pass` /
// `test:fail` are emitted in FILE order instead, so one hung file holds back every later file's
// results (the reason a spec log can go silent for an hour while tests keep finishing). A file's
// own completion is the `test:complete` whose name is the file path itself, at nesting 0; it
// arrives only once that file's process has exited, so a leaked handle shows as a file that
// never completes. State is written synchronously as it changes: on SIGTERM node delivers
// `test:interrupted` and the stream never ends, so nothing may wait for the end.
//
// Self-contained at runtime on purpose: node loads a reporter module without the `.js` -> `.ts`
// mapping a relative import needs, so this file imports only node builtins and TYPES (erased).
// The launcher imports its shared runtime helpers from here, through agent-report.ts.
import { readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Counts, Failure, Progress } from "./agent-report.js";

export const REPORT_SCHEMA = 1;
/** Bounded error text per failure, and failures kept in full. */
const ERROR_CHARS = 600;
const FAILURE_LIMIT = 50;

function emptyCounts(): Counts {
  return { tests: 0, pass: 0, fail: 0, cancelled: 0, skip: 0, todo: 0, file_failures: 0, suite_failures: 0 };
}

function boundError(text: string): string {
  const single = text.replace(/\r\n?/g, "\n");
  return single.length > ERROR_CHARS ? single.slice(0, ERROR_CHARS) + " ...[truncated]" : single;
}

/** Atomic replace: a temp file in the SAME directory (never os.tmpdir(), which the leak guard deletes), then rename. */
export function writeTextAtomic(file: string, text: string): void {
  const temp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.tmp`);
  writeFileSync(temp, text, { mode: 0o644 });
  renameSync(temp, file);
}

export function writeJsonAtomic(file: string, value: unknown): void {
  writeTextAtomic(file, JSON.stringify(value, null, 1) + "\n");
}


interface TestEventData {
  name?: string;
  nesting?: number;
  file?: string;
  line?: number;
  skip?: boolean | string;
  todo?: boolean | string;
  details?: { passed?: boolean; duration_ms?: number; type?: string; error?: unknown };
  counts?: Record<string, number>;
  message?: string;
}

interface TestEvent {
  type: string;
  data?: TestEventData;
}

const THROTTLE_MS = 1_000;
const HEARTBEAT_MS = 10_000;
const CANCELLED = new Set(["testTimeoutFailure", "cancelledByParent"]);

function failureType(error: unknown): string | undefined {
  const value = (error as { failureType?: unknown } | undefined)?.failureType;
  return typeof value === "string" ? value : undefined;
}

function errorText(error: unknown): string | undefined {
  if (error === undefined || error === null) return undefined;
  const cause = (error as { cause?: unknown }).cause;
  const inner = cause instanceof Error ? cause : error;
  const text = inner instanceof Error ? `${inner.name}: ${inner.message}` : String(inner);
  return boundError(text);
}

export default async function* agentReport(source: AsyncIterable<TestEvent>): AsyncGenerator<string> {
  const dir = process.env.UZI_AGENT_TEST_REPORT_RUN_DIR;
  if (!dir) {
    for await (const _event of source) { /* not wired by the launcher: drain only */ }
    return;
  }
  const file = path.join(dir, "progress.json");
  // The launcher's exact selected inventory: only these paths are files; an event located anywhere
  // else (a test registered by a helper module) cannot be attributed and never counts as a file.
  let selected: Set<string> | null = null;
  try {
    const list: unknown = JSON.parse(readFileSync(path.join(dir, "selected.json"), "utf8"));
    if (Array.isArray(list) && list.every((entry) => typeof entry === "string")) selected = new Set(list as string[]);
  } catch {
    selected = null;
  }
  const now = (): string => new Date().toISOString();
  const progress: Progress = {
    schema: REPORT_SCHEMA,
    scope: "unit",
    state: "running",
    started_at: now(),
    heartbeat_at: now(),
    last_event_at: null,
    unattributed_last_event_at: null,
    selection_known: selected !== null,
    files_total: selected?.size ?? null,
    files: {},
    counts: emptyCounts(),
    failures: [],
    failures_omitted: 0,
    node_counts: null,
  };
  let lastWrite = 0;
  let trailing: NodeJS.Timeout | undefined;
  const write = (force: boolean): void => {
    if (!force && Date.now() - lastWrite < THROTTLE_MS) {
      // A throttled change still lands within THROTTLE_MS, even if no event follows it.
      trailing ??= setTimeout(() => write(true), THROTTLE_MS).unref();
      return;
    }
    if (trailing) clearTimeout(trailing);
    trailing = undefined;
    progress.heartbeat_at = now();
    writeJsonAtomic(file, progress);
    lastWrite = Date.now();
  };
  const record = (failure: Failure): void => {
    if (progress.failures.length < FAILURE_LIMIT) progress.failures.push(failure);
    else progress.failures_omitted++;
  };
  const heartbeat = setInterval(() => write(true), HEARTBEAT_MS);
  heartbeat.unref();
  write(true);
  try {
    for await (const event of source) {
      const data = event.data ?? {};
      if (event.type === "test:summary" && data.file === undefined && data.counts) {
        progress.node_counts = data.counts;
        continue;
      }
      if (event.type === "test:interrupted") {
        progress.state = "interrupted";
        write(true);
        continue;
      }
      if (!data.file) continue;
      const isFileWrapper = data.nesting === 0 && data.name !== undefined
        && path.resolve(process.cwd(), data.name) === data.file;
      // node dequeues every file's wrapper up front, whatever --test-concurrency says, so the
      // wrapper's own dequeue/start is not evidence the file runs. A file starts at its first
      // inner event; one that never reaches a test stays unlisted, so it can never be judged hung.
      if (isFileWrapper && event.type !== "test:complete") continue;
      const relative = path.relative(process.cwd(), data.file);
      // A crashed file's own error reaches stderr, which node reports after the file completes:
      // append it to that file's failure, bounded.
      if (event.type === "test:stderr" && typeof data.message === "string") {
        const crash = progress.failures.find((failure) => failure.kind === "file" && failure.file === relative);
        if (crash && (crash.error ?? "").length < ERROR_CHARS) {
          crash.error = boundError(`${crash.error ?? ""}\n${data.message}`);
          write(true);
        }
      }
      const at = now();
      progress.last_event_at = at;
      const attributable = isFileWrapper || selected?.has(relative) === true;
      let state = attributable ? progress.files[relative] : undefined;
      let important = false;
      if (!attributable) {
        progress.unattributed_last_event_at = at;
      } else {
        if (state === undefined) {
          state = { started_at: at, last_event_at: at };
          progress.files[relative] = state;
          important = true;
        }
        state.last_event_at = at;
      }
      if (event.type === "test:complete") {
        const details = data.details ?? {};
        const type = failureType(details.error);
        if (isFileWrapper && state !== undefined) {
          state.done_at = at;
          state.duration_ms = details.duration_ms;
          state.passed = details.passed === true;
          important = true;
          // subtestsFailed: the file's own tests already carry the failures.
          if (details.passed !== true && type !== "subtestsFailed") {
            progress.counts.file_failures++;
            record({ file: relative, name: relative, kind: "file", failure_type: type, error: errorText(details.error) });
          }
        } else if (details.type === "suite") {
          // node's tally counts tests only; a suite's own failure is kept apart, never dropped.
          if (details.passed !== true && type !== "subtestsFailed") {
            progress.counts.suite_failures++;
            important = true;
            record({ file: relative, name: data.name ?? "", line: data.line, kind: "suite", failure_type: type,
              error: errorText(details.error) });
          }
        } else {
          progress.counts.tests++;
          if (data.skip) progress.counts.skip++;
          else if (data.todo) progress.counts.todo++;
          else if (details.passed === true) progress.counts.pass++;
          else {
            const kind = type !== undefined && CANCELLED.has(type) ? "cancelled" : "fail";
            progress.counts[kind]++;
            important = true;
            record({ file: relative, name: data.name ?? "", line: data.line, kind, failure_type: type,
              error: errorText(details.error) });
          }
        }
      }
      write(important);
    }
    progress.state = progress.state === "interrupted" ? "interrupted" : "ended";
    write(true);
    // The reporter's own destination (reporter.out) gets one line; progress.json is the record.
    yield `agent-report: ${progress.state}, ${Object.values(progress.files).filter((state) => state.done_at).length} file(s) done\n`;
  } finally {
    clearInterval(heartbeat);
    if (trailing) clearTimeout(trailing);
    write(true);
  }
}
