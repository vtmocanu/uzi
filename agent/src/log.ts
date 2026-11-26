// Redacting structured logger.
//
// The primary directive is that no PAT or Anthropic token ever reaches disk or
// logs (PRD §Success Criteria). The worker never passes a secret to the logger,
// but this adds defense-in-depth: every registered secret value is scrubbed
// from the final serialized line, so an accidental `log.info("...", { url })`
// that embedded a token cannot leak it.

import { LiteralIndex, PER_SECRET_MAX, REDACTION_WITHHELD, replaceSpans, runWithinBudget, spansOfEach } from "./redact.js";

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
  /** Scrub registered secrets before a caller truncates a diagnostic. A logger
   * without this method must not be given raw diagnostic text as a fallback. */
  redactDiagnostic?(text: string): string;
  child(fields: Record<string, unknown>): Logger;
}

/** The msg of the fixed line written in place of a record whose redaction passed the work budget. */
const LOG_WITHHELD_MSG = "log record withheld: redaction work budget exceeded";

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

  /** Every registered string and its JSON-escaped form, built when the registry first passes PER_SECRET_MAX
   *  strings and kept current by every add and remove after that. Its own reference counts keep a form that
   *  two registered strings share (the escaped form of one can equal another). */
  private index: LiteralIndex | undefined;

  add(secret: string): void {
    // Guard against short/empty values whose blanket replacement would corrupt
    // unrelated output.
    if (secret && secret.length >= 8) {
      const held = this.counts.get(secret);
      this.counts.set(secret, (held ?? 0) + 1);
      if (held === undefined) this.indexForms(secret, (form) => this.index?.add(form));
    }
  }

  remove(secret: string): void {
    const n = this.counts.get(secret);
    if (n === undefined) return; // never added, or already fully evicted
    if (n <= 1) {
      this.counts.delete(secret);
      this.indexForms(secret, (form) => this.index?.remove(form));
    } else this.counts.set(secret, n - 1);
  }

  /** Applies `apply` to the secret and, when it differs, its JSON-escaped form. */
  private indexForms(secret: string, apply: (form: string) => void): void {
    apply(secret);
    const escaped = JSON.stringify(secret).slice(1, -1);
    if (escaped !== secret) apply(escaped);
  }

  /** `line` with every registered secret (and its JSON-escaped form) masked; undefined when the matching passed the
   *  redaction work budget, so the caller must withhold the whole line rather than emit it. */
  scrub(line: string): string | undefined {
    return runWithinBudget((work) => {
      if (this.counts.size > PER_SECRET_MAX) {
        if (this.index === undefined) {
          const index = new LiteralIndex();
          for (const s of this.counts.keys()) this.indexForms(s, (form) => index.add(form));
          this.index = index;
        }
        return replaceSpans(line, this.index.find(line, work));
      }
      // A JSON-serialized line carries a secret containing `"` or `\` in its escaped form.
      const forms = new Set<string>();
      for (const s of this.counts.keys()) this.indexForms(s, (form) => forms.add(form));
      return replaceSpans(line, spansOfEach([...forms])(line, work));
    });
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
    const ts = new Date().toISOString();
    const record = {
      ts,
      level,
      msg,
      ...this.base,
      ...fields,
    };
    // Over the redaction work budget the whole record is withheld: a fixed, valid JSON line with no base or fields.
    // The fallback is unscrubbed by design and writes only logger-generated values (ts, level, LOG_WITHHELD_MSG);
    // `record.ts` is not used because a base or call-field `ts` overrides it.
    const line = this.registry.scrub(JSON.stringify(record)) ??
      JSON.stringify({ ts, level, msg: LOG_WITHHELD_MSG });
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
  redactDiagnostic(text: string): string { return this.registry.scrub(text) ?? REDACTION_WITHHELD; }

  child(fields: Record<string, unknown>): Logger {
    return new ConsoleLogger(this.minLevel, this.registry, { ...this.base, ...fields });
  }
}

export function createLogger(level: LogLevel = "info"): Logger {
  return new ConsoleLogger(LEVEL_ORDER[level], new SecretRegistry(), {});
}
