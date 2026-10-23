import { spawn, spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdirSync, readFileSync, readdirSync } from "node:fs";
import { constants } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { selectAgentTests } from "./agent-test-shards.js";
import {
  DEFAULT_STALL_MS,
  REPORT_SCHEMA,
  hungFiles,
  inFlight,
  renderSummaryText,
  writeJsonAtomic,
  writeTextAtomic,
  type Invocation,
  type Progress,
  type Summary,
  type Termination,
} from "./agent-report.js";

function reportError(error: unknown): void {
  const message = error instanceof Error ? error.message : String(error);
  // JSON escaping handles C0/ANSI bytes; also escape C1, line and bidi controls.
  const diagnostic = JSON.stringify(message).slice(1, -1)
    .replace(/[\u007f-\u009f\u061c\u200e\u200f\u2028-\u202e\u2066-\u2069]/g,
      (character) => "\\u" + character.charCodeAt(0).toString(16).padStart(4, "0"));
  console.error(diagnostic);
}

function filters(args: string[]): string[] {
  const result: string[] = [];
  for (let index = 0; index < args.length; index++) {
    const arg = args[index]!;
    if (arg === "--test-only") {
      result.push(arg);
      continue;
    }
    const match = /^(--test-(?:name|skip)-pattern)(?:=(.*))?$/.exec(arg);
    if (!match) throw new Error(`Unsupported test argument: ${arg}`);
    const value = match[2] ?? args[++index];
    if (value === undefined || (match[2] === undefined && value.startsWith("--"))) {
      throw new Error(`Missing pattern for ${match[1]}`);
    }
    new RegExp(value); // Reject malformed expressions before spawning.
    result.push(`${match[1]}=${value}`);
  }
  return result;
}

/**
 * The hung-file window. Only a test may shorten it, and only with both variables set, so a
 * leaked override cannot arm an early kill in a real gate (test:agent also refuses either).
 */
function stallWindow(): { ms: number; overridden: boolean } {
  const value = process.env.UZI_AGENT_TEST_STALL_MS;
  if (value === undefined || value === "") return { ms: DEFAULT_STALL_MS, overridden: false };
  if (process.env.UZI_AGENT_TEST_STALL_TESTONLY !== "1") {
    throw new Error("UZI_AGENT_TEST_STALL_MS is honoured only with UZI_AGENT_TEST_STALL_TESTONLY=1 (tests of the launcher)");
  }
  if (!/^[1-9]\d*$/.test(value) || !Number.isSafeInteger(Number(value))) {
    throw new Error("UZI_AGENT_TEST_STALL_MS must be a positive safe integer");
  }
  return { ms: Number(value), overridden: true };
}

function git(root: string, args: string[]): string | null {
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8", timeout: 10_000 });
  return result.status === 0 ? result.stdout : null;
}

function readProgress(dir: string): Progress | null {
  try {
    return JSON.parse(readFileSync(path.join(dir, "progress.json"), "utf8")) as Progress;
  } catch {
    return null;
  }
}

// Only ESRCH proves the group is gone. macOS answers EPERM while a member is a zombie not yet
// reaped, so EPERM means "maybe alive": poll again, and never count it as gone.
function groupAlive(pid: number): boolean {
  try {
    process.kill(-pid, 0);
    return true;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ESRCH") return false;
    if (code === "EPERM") return true;
    throw error;
  }
}

/** Strict: only a successful probe counts (EPERM from a lingering zombie does not). */
function groupHasLiveMember(pid: number): boolean {
  try {
    process.kill(-pid, 0);
    return true;
  } catch {
    return false;
  }
}

function signalGroup(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(-pid, signal);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code !== "ESRCH" && code !== "EPERM") throw error;
  }
}

async function waitGroupGone(pid: number, ms: number): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (groupAlive(pid)) {
    if (Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  return true;
}

function main(): void {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const inventory = readdirSync(new URL("./", import.meta.url), { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".test.ts"))
    .map((entry) => `test/${entry.name}`);
  const pins: unknown = JSON.parse(readFileSync(new URL("../test-shards.json", import.meta.url), "utf8"));
  const shard = process.env.UZI_AGENT_TEST_SHARD || undefined;
  const selected = selectAgentTests(inventory, pins, shard);
  const stall = stallWindow();
  const selection = filters(process.argv.slice(2));
  const id = `${new Date().toISOString().replace(/[-:.]/g, "")}-${process.pid}-${randomBytes(3).toString("hex")}`;
  const base = path.resolve(root, process.env.UZI_AGENT_TEST_REPORT_DIR || ".test-report");
  const dir = path.join(base, id);
  mkdirSync(dir, { recursive: true });
  const args = ["--import", "tsx", "--import", "./test/setup/hermetic-proc.ts", "--test"];
  const concurrency = process.env.UZI_AGENT_TEST_CONCURRENCY;
  if (concurrency) {
    if (!/^[1-9]\d*$/.test(concurrency) || !Number.isSafeInteger(Number(concurrency))) {
      throw new Error("UZI_AGENT_TEST_CONCURRENCY must be a positive safe integer");
    }
    args.push(`--test-concurrency=${concurrency}`);
  }
  // Naming any reporter drops node's default, so spec to stdout is explicit. The agent reporter's
  // own output goes to a real file (node 24 fails fsync on /dev/null).
  args.push("--test-reporter=spec", "--test-reporter-destination=stdout",
    "--test-reporter=./test/agent-report-reporter.ts", `--test-reporter-destination=${path.join(dir, "reporter.out")}`);
  const junit = process.env.UZI_AGENT_TEST_JUNIT;
  if (junit) args.push("--test-reporter=junit", `--test-reporter-destination=${junit}`);
  args.push("--test-timeout=120000", ...selection, ...selected);
  const status = git(root, ["--no-optional-locks", "status", "--porcelain", "--untracked-files=normal"]);
  const invocation: Invocation = {
    id,
    dir,
    command: [process.execPath, ...args.filter((arg) => !selected.includes(arg))],
    shard: shard ?? null,
    filters: selection,
    full_suite: shard === undefined && selection.length === 0,
    node: process.versions.node,
    head: git(root, ["rev-parse", "HEAD"])?.trim() ?? null,
    dirty: status === null ? null : status.trim() !== "",
    stall_ms: stall.ms,
    stall_overridden: stall.overridden,
    started_at: new Date().toISOString(),
  };
  writeJsonAtomic(path.join(dir, "invocation.json"), invocation);
  writeJsonAtomic(path.join(dir, "selected.json"), selected);
  writeJsonAtomic(path.join(base, "latest.json"), { id, dir, started_at: invocation.started_at });
  console.log(`agent-test-report: ${dir}`);

  // Its own process group, so cleanup reaches every per-file child: SIGKILL to the runner alone
  // orphans them (node's own cleanup needs it alive), and they would keep git and servers running
  // while tmpdir-leak-guard deletes their TMPDIR.
  const child = spawn(process.execPath, args, {
    cwd: root,
    env: { ...process.env, UZI_AGENT_TEST_REPORT_RUN_DIR: dir },
    stdio: "inherit",
    detached: true,
  });
  let termination: Termination = "none";
  let interrupted: NodeJS.Signals | undefined;
  let hung: string[] = [];
  let escalation: NodeJS.Timeout | undefined;
  let finished = false;
  const stop = (signal: NodeJS.Signals): void => {
    if (child.pid === undefined) return;
    signalGroup(child.pid, signal);
    escalation ??= setTimeout(() => signalGroup(child.pid!, "SIGKILL"), 2_000);
  };
  // Only this owned group is signalled. One grace period bounds cleanup; a second signal
  // escalates immediately.
  const forward = (signal: NodeJS.Signals): void => {
    if (interrupted) {
      if (child.pid !== undefined) signalGroup(child.pid, "SIGKILL");
      return;
    }
    interrupted = signal;
    if (termination === "none") termination = "signal";
    stop(signal);
  };
  const onInt = (): void => forward("SIGINT");
  const onTerm = (): void => forward("SIGTERM");
  process.on("SIGINT", onInt);
  process.on("SIGTERM", onTerm);
  const watchdog = setInterval(() => {
    const progress = readProgress(dir);
    if (!progress || termination !== "none") return;
    hung = hungFiles(progress, selected.length, Date.now(), stall.ms);
    if (hung.length === 0) return;
    termination = "watchdog";
    console.error(`agent-tests: HUNG test file(s) ${hung.join(" ")}: no test event for ${Math.round(stall.ms / 1000)}s `
      + "after every other file finished; stopping the run (the files' tests ran, but the process never exited)");
    stop("SIGTERM");
  }, Math.min(5_000, Math.max(250, Math.floor(stall.ms / 4))));
  const finish = (code: number | null, signal: NodeJS.Signals | null, cleanup: Summary["cleanup"], reason: string | null): void => {
    if (finished) return;
    finished = true;
    clearInterval(watchdog);
    if (escalation) clearTimeout(escalation);
    const progress = readProgress(dir);
    const complete = termination === "none" && signal === null && progress?.state === "ended";
    const counts = progress?.counts;
    const summary: Summary = {
      schema: REPORT_SCHEMA,
      scope: "unit",
      verdict_note: "Unit stage only. The gate verdict is the task's exit code; a missing summary or a "
        + "running/incomplete state is never green.",
      state: complete ? "complete" : "incomplete",
      unit_green: complete && code === 0 && counts !== undefined
        && counts.fail === 0 && counts.cancelled === 0 && counts.file_failures === 0 && counts.suite_failures === 0,
      reason: complete ? null
        : reason ?? (termination === "watchdog" ? "hung test file(s)"
          : termination === "signal" ? `interrupted by ${interrupted}`
            : signal ? `test process killed by ${signal}`
              : progress === null ? "no progress was recorded" : `reporter state ${progress.state}`),
      invocation,
      exit: { code, signal },
      termination,
      cleanup,
      hung_files: hung,
      in_flight_files: progress ? inFlight(progress) : [],
      finished_at: new Date().toISOString(),
      progress,
    };
    writeJsonAtomic(path.join(dir, "summary.json"), summary);
    const text = renderSummaryText(summary);
    writeTextAtomic(path.join(dir, "summary.txt"), text);
    process.stdout.write("\n" + text);
  };
  child.on("error", (error) => {
    reportError(error);
    termination = "spawn-error";
    finish(null, null, "not-needed", `spawn failed: ${error.message}`);
    process.exitCode = 1;
  });
  child.on("close", (code, signal) => {
    void (async () => {
      let cleanup: Summary["cleanup"] = "not-needed";
      if (termination === "none" && !interrupted && signal === null
        && child.pid !== undefined && groupHasLiveMember(child.pid)) {
        // Observe only an initially positive diagnostic probe. This never signals an uninterrupted
        // group; 50ms polls over 500ms allow short-lived descendants to exit before warning.
        const deadline = Date.now() + 500;
        cleanup = "leftover";
        while (termination === "none" && !interrupted && Date.now() < deadline) {
          await new Promise((resolve) => setTimeout(resolve, 50));
          if (termination !== "none" || interrupted) break;
          if (!groupHasLiveMember(child.pid)) {
            cleanup = "not-needed";
            break;
          }
        }
      }
      // Recheck after observation: a caller's signal must finish abnormal cleanup before finish
      // clears the escalation timer.
      // Also when the runner itself died from a signal nobody here sent (OOM killer, kill -9): its
      // per-file children are then orphans in a detached group that nothing else will reap.
      if ((termination !== "none" || signal !== null) && child.pid !== undefined) {
        // The runner exiting does not prove its group did: a descendant may ignore SIGTERM.
        try {
          if (termination === "none") signalGroup(child.pid, "SIGTERM");
          let gone = await waitGroupGone(child.pid, 2_000);
          if (!gone) {
            signalGroup(child.pid, "SIGKILL");
            gone = await waitGroupGone(child.pid, 5_000);
          }
          cleanup = gone ? "verified" : "unverified";
        } catch (error) {
          reportError(error);
          cleanup = "unverified";
        }
        if (cleanup === "unverified") console.error("agent-tests: cleanup UNVERIFIED: the test process group may still be alive");
      } else if (cleanup === "leftover") {
        // Diagnostic only: an uninterrupted run is not swept, so a test that leaves a process behind
        // is named here rather than silently killed.
        cleanup = "leftover";
        console.error("agent-tests: processes from the test run are still alive after it exited (left behind by a test)");
      }
      finish(code, signal, cleanup, null);
      process.off("SIGINT", onInt);
      process.off("SIGTERM", onTerm);
      // A caller's signal always wins and is re-raised; the watchdog's own SIGTERM is not one.
      if (termination === "watchdog" && !interrupted) {
        process.exitCode = 1;
        return;
      }
      const ended = interrupted ?? (termination === "watchdog" ? undefined : signal);
      if (ended) {
        process.exitCode = 128 + constants.signals[ended];
        // Restore default signal handling only after the owned group has closed.
        process.kill(process.pid, ended);
        return;
      }
      process.exitCode = code ?? 1;
    })().catch((error: unknown) => {
      reportError(error);
      clearInterval(watchdog);
      process.exitCode = 1;
    });
  });
}

try {
  main();
} catch (error) {
  reportError(error);
  process.exitCode = 1;
}
