import cp from "node:child_process";
import fs from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import { mock } from "node:test";
import type { Readable } from "node:stream";

const EVENTS = new Set(["started", "snapshot", "dispose", "child_exit", "abnormal"]);
const STATES = new Set(["drained", "unconfirmed"]);
const CLASSIFICATIONS = new Set(["failure", "not_clean"]);
const MAX_OUTPUT_BYTES = 16384;
const REASONS = new Set([
  "thrown_or_unavailable", "drain_unconfirmed", "drain_deadline", "drain_contradicted",
  "supervisor_exit_nonzero", "supervisor_exit_unconfirmed", "control_unavailable",
  "channels_unconfirmed", "final_evidence_unconfirmed", "evidence_closed",
  "unanswered_request", "invalid_dispose_evidence", "unmatched_response",
  "dispose_deadline", "exit_deadline", "already_exited", "outcome_unavailable", "other_unconfirmed",
]);

/** Test-only observer: 64 events, four roots, 8 KiB partial frame, 64 KiB total
 * evidence per supervisor. Overflow stops parsing that supervisor, not sibling
 * observations. Transport contents are never read or retained. */
export class AdviceTeardownDiagnostic {
  private sequence = 0;
  private events: Record<string, unknown>[] = [];
  private roots: { role: "data" | "cwd"; target: string; initial: Record<string, unknown> }[] = [];
  private lines: unknown[] = [];
  private printed = false;
  private originalFailure: unknown;
  private truncated = false;
  private listeners: (() => void)[] = [];

  constructor(private readonly output?: (line: string) => void) {}

  private print(line: string): void {
    if (this.output) this.output(line);
    else console.error(line);
  }

  private record(event: string, fields: Record<string, unknown> = {}): void {
    const ordinal = ++this.sequence;
    if (this.events.length < 64) this.events.push({ ordinal, event, ...fields });
    else this.truncated = true;
  }

  observe(child: import("node:child_process").ChildProcess): void {
    const ordinal = this.sequence + 1;
    this.record("supervisor_observed");
    const onExit = (code: number | null, signal: NodeJS.Signals | null): void => this.record("exit", {
      supervisor: ordinal, status: signal ? "signalled" : code === 0 ? "zero" : typeof code === "number" ? "nonzero" : "unknown",
    });
    const onClose = (): void => this.record("close", { supervisor: ordinal });
    child.on("exit", onExit);
    child.on("close", onClose);
    this.listeners.push(() => { child.off("exit", onExit); child.off("close", onClose); });
    for (const [role, stream] of [["evidence", child.stdio[4]], ["stdout", child.stdout], ["stderr", child.stderr]] as const) {
      if (!stream) continue;
      // end/close notifications only: never attach data listeners to transport pipes.
      const readable = stream as Readable;
      const end = (): void => this.record("pipe_end", { supervisor: ordinal, role });
      const close = (): void => this.record("pipe_close", { supervisor: ordinal, role });
      readable.on("end", end); readable.on("close", close);
      this.listeners.push(() => { readable.off("end", end); readable.off("close", close); });
    }
    const evidence = child.stdio[4] as Readable | null;
    if (!evidence) return;
    let pending = "";
    let bytes = 0;
    let stopped = false;
    let frames = 0;
    const data = (chunk: Buffer | string): void => {
      if (stopped) return;
      bytes += Buffer.byteLength(chunk);
      if (bytes > 65536) { stopped = true; pending = ""; this.truncated = true; return; }
      pending += String(chunk);
      let end: number;
      while ((end = pending.indexOf("\n")) >= 0) {
        if (++frames > 64) { stopped = true; pending = ""; this.truncated = true; return; }
        const frame = pending.slice(0, end);
        pending = pending.slice(end + 1);
        if (Buffer.byteLength(frame) > 8192) { stopped = true; pending = ""; this.truncated = true; return; }
        try {
          const value = JSON.parse(frame) as Record<string, unknown>;
          this.record("evidence", {
            supervisor: ordinal,
            category: EVENTS.has(String(value?.event)) ? String(value.event) : "unknown",
            state: STATES.has(String(value?.state)) ? String(value.state) : "unknown",
            authority: value?.authority === "ECHILD+__WALL" ? "ECHILD+__WALL" : "unknown",
          });
        } catch { this.record("evidence_invalid", { supervisor: ordinal }); }
      }
      if (Buffer.byteLength(pending) > 8192) { stopped = true; pending = ""; this.truncated = true; }
    };
    evidence.on("data", data);
    this.listeners.push(() => { evidence.off("data", data); pending = ""; });
  }

  async watch(data: string, cwd: string, lines: unknown[]): Promise<void> {
    this.lines = lines;
    for (const [role, target] of [["data", data], ["cwd", cwd]] as const) {
      if (this.roots.length < 4) this.roots.push({ role, target, initial: await this.metadata(target) });
    }
  }

  private async metadata(target: string): Promise<Record<string, unknown>> {
    try {
      const stat = await fs.lstat(target);
      return { existence: "present", uid: stat.uid, gid: stat.gid, mode: stat.mode & 0o7777,
        type: stat.isSymbolicLink() ? "symlink" : stat.isDirectory() ? "directory" : stat.isFile() ? "file" : "other",
        identity: { dev: stat.dev, ino: stat.ino } };
    } catch (error) {
      return { existence: (error as NodeJS.ErrnoException)?.code === "ENOENT" ? "absent" : "unknown" };
    }
  }

  async snapshot(): Promise<Record<string, unknown>> {
    const warnings: Record<string, unknown>[] = [];
    // recordingLogger lines stay in memory; select at most eight fixed-category warnings.
    for (const line of this.lines.slice(-64)) {
      if (warnings.length >= 8) break;
      try {
        if (typeof line !== "object" || line === null) continue;
        const value = line as Record<string, unknown>;
        if (value.msg === "Codex advice data retained: disposal was not confirmed clean") {
          warnings.push({
            category: "retained",
            classification: CLASSIFICATIONS.has(String(value.classification)) ? String(value.classification) : "unknown",
            reason: REASONS.has(String(value.reason)) ? String(value.reason) : "unknown",
            state: STATES.has(String(value.state)) ? String(value.state) : "unknown",
            authority: value.authority === "ECHILD+__WALL" ? "ECHILD+__WALL" : "unknown",
            cleanup_attempted: value.cleanup_attempted === false ? false : "unknown",
          });
        } else if (value.msg === "Codex advice data cleanup failed") {
          warnings.push({ category: "cleanup_failed", cleanup_attempted: true });
        }
      } catch { /* Never print malformed logger contents. */ }
    }
    const roots = [];
    for (const root of this.roots) roots.push({ role: root.role, initial: root.initial, current: await this.metadata(root.target) });
    const removed = roots.some((root) => root.role === "data" && root.initial.existence === "present" && root.current.existence === "absent");
    return { node_version: process.version, events: this.events, truncated: this.truncated, roots, warnings,
      cleanup_attempted_observation: warnings.some((w) => w.category === "cleanup_failed") ? "failure_warning" : "unavailable",
      cleanup_attempted_inference: warnings.some((w) => w.category === "retained") ? false : removed ? true : "unknown" };
  }

  async failure(error: unknown): Promise<void> {
    if (this.printed) return;
    this.printed = true;
    this.originalFailure = error;
    try {
      const line = "advice-teardown-diagnostic " + JSON.stringify(await this.snapshot());
      this.print(Buffer.byteLength(line) <= MAX_OUTPUT_BYTES ? line
        : 'advice-teardown-diagnostic {"snapshot":"oversized"}');
    }
    catch {
      try { this.print('advice-teardown-diagnostic {"snapshot":"unavailable"}'); } catch { /* Preserve the assertion even if printing fails. */ }
    }
  }

  rethrow(error: unknown): never {
    throw this.printed ? this.originalFailure : error;
  }

  async run(body: () => Promise<void>): Promise<void> {
    const original = cp.spawn.bind(cp);
    let observed = 0;
    const spawn = mock.method(cp, "spawn", ((command: string, args: string[], options: import("node:child_process").SpawnOptions) => {
      const child = original(command, args, options);
      if (args.some((arg) => arg.endsWith("/uzi-codex-supervisor"))) {
        if (observed++ < 4) this.observe(child); else this.truncated = true;
      }
      return child;
    }) as typeof cp.spawn);
    syncBuiltinESMExports();
    try { await body(); }
    catch (error) { await this.failure(error); throw this.originalFailure; }
    finally {
      spawn.mock.restore(); syncBuiltinESMExports();
      for (const remove of this.listeners) remove();
    }
  }
}
