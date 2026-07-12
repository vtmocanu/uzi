// Redacting structured logger.
//
// The primary directive is that no PAT or Anthropic token ever reaches disk or
// logs (PRD §Success Criteria). The worker never passes a secret to the logger,
// but this adds defense-in-depth: every registered secret value is scrubbed
// from the final serialized line, so an accidental `log.info("...", { url })`
// that embedded a token cannot leak it.

export type LogLevel = "debug" | "info" | "warn" | "error";

const LEVEL_ORDER: Record<LogLevel, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
};

export interface Logger {
  debug(msg: string, fields?: Record<string, unknown>): void;
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
  /** Register a secret string so it is scrubbed from all future output. Balance
   *  every run-scoped `addSecret` with a `removeSecret` on terminal; a
   *  worker-lifetime secret (the join token) is added once and never removed. */
  addSecret(secret: string): void;
  /** Drop one registration of a run's secret when the run reaches a terminal state
   *  (PRD #42 Decision 7). Reference-counted: the string stops being scrubbed only
   *  once its LAST holder is removed, so evicting a completed run's PAT never
   *  un-scrubs it from a still-active sibling run that shares it (same user ⇒ same
   *  PAT + Anthropic token). A no-op for a never-added or worker-lifetime secret. */
  removeSecret(secret: string): void;
  child(fields: Record<string, unknown>): Logger;
}

const REDACTED = "***REDACTED***";

/** Secrets are shared by reference across child loggers so a secret registered
 *  from a claim is scrubbed everywhere, not just on the logger that saw it.
 *
 *  Run-scoped eviction (PRD #42 Decision 7) makes this reference-counted rather
 *  than a plain Set: two concurrent runs of the same user register the identical
 *  PAT/token, so a string must stay scrubbed until BOTH have evicted it. All three
 *  operations are synchronous with no `await` between a mutate and a read, so on
 *  Node's single thread no concurrent run can observe a half-updated registry. */
class SecretRegistry {
  private readonly counts = new Map<string, number>();

  add(secret: string): void {
    // Guard against short/empty values whose blanket replacement would corrupt
    // unrelated output.
    if (secret && secret.length >= 8) this.counts.set(secret, (this.counts.get(secret) ?? 0) + 1);
  }

  remove(secret: string): void {
    const n = this.counts.get(secret);
    if (n === undefined) return; // never added, or already fully evicted
    if (n <= 1) this.counts.delete(secret);
    else this.counts.set(secret, n - 1);
  }

  scrub(line: string): string {
    let out = line;
    for (const s of this.counts.keys()) out = out.split(s).join(REDACTED);
    return out;
  }
}

class ConsoleLogger implements Logger {
  constructor(
    private readonly minLevel: number,
    private readonly registry: SecretRegistry,
    private readonly base: Record<string, unknown>,
  ) {}

  private emit(level: LogLevel, msg: string, fields?: Record<string, unknown>): void {
    if (LEVEL_ORDER[level] < this.minLevel) return;
    const record = {
      ts: new Date().toISOString(),
      level,
      msg,
      ...this.base,
      ...fields,
    };
    const line = this.registry.scrub(JSON.stringify(record));
    // stderr for warn/error, stdout otherwise — keeps machine-readable logs
    // greppable and lets a supervisor split streams.
    if (level === "warn" || level === "error") process.stderr.write(line + "\n");
    else process.stdout.write(line + "\n");
  }

  debug(msg: string, fields?: Record<string, unknown>): void { this.emit("debug", msg, fields); }
  info(msg: string, fields?: Record<string, unknown>): void { this.emit("info", msg, fields); }
  warn(msg: string, fields?: Record<string, unknown>): void { this.emit("warn", msg, fields); }
  error(msg: string, fields?: Record<string, unknown>): void { this.emit("error", msg, fields); }

  addSecret(secret: string): void { this.registry.add(secret); }
  removeSecret(secret: string): void { this.registry.remove(secret); }

  child(fields: Record<string, unknown>): Logger {
    return new ConsoleLogger(this.minLevel, this.registry, { ...this.base, ...fields });
  }
}

export function createLogger(level: LogLevel = "info"): Logger {
  return new ConsoleLogger(LEVEL_ORDER[level], new SecretRegistry(), {});
}
