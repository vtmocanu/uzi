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
  /** Register a secret string so it is scrubbed from all future output. */
  addSecret(secret: string): void;
  child(fields: Record<string, unknown>): Logger;
}

const REDACTED = "***REDACTED***";

/** Secrets are shared by reference across child loggers so a secret registered
 *  from a claim is scrubbed everywhere, not just on the logger that saw it. */
class SecretRegistry {
  private readonly secrets = new Set<string>();

  add(secret: string): void {
    // Guard against short/empty values whose blanket replacement would corrupt
    // unrelated output.
    if (secret && secret.length >= 8) this.secrets.add(secret);
  }

  scrub(line: string): string {
    let out = line;
    for (const s of this.secrets) out = out.split(s).join(REDACTED);
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

  child(fields: Record<string, unknown>): Logger {
    return new ConsoleLogger(this.minLevel, this.registry, { ...this.base, ...fields });
  }
}

export function createLogger(level: LogLevel = "info"): Logger {
  return new ConsoleLogger(LEVEL_ORDER[level], new SecretRegistry(), {});
}
