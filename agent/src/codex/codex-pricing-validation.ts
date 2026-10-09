interface TierRates {
  readonly uncached_input: number;
  readonly cached_input: number;
  readonly cache_write: number;
  readonly output: number;
}

interface ModelRates {
  readonly verified_at: string;
  readonly promo_review_date?: string;
  readonly sources: readonly string[];
  readonly low: TierRates;
  readonly high: TierRates;
}

interface PricingTable {
  readonly version: string;
  readonly input_tier_threshold_tokens: number;
  readonly models: Readonly<Record<string, ModelRates>>;
}

class CodexPricingValidationError extends Error {
  constructor(path: string, reason: string) {
    super(`${path}: ${reason}`);
    this.name = "CodexPricingValidationError";
  }
}

function fail(path: string, reason: string): never {
  throw new CodexPricingValidationError(path, reason);
}

function object(value: unknown, path: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail(path, "expected object");
  return value as Record<string, unknown>;
}

function fields(value: Record<string, unknown>, path: string, required: readonly string[], optional: readonly string[] = []): void {
  for (const key of required) {
    if (!Object.hasOwn(value, key)) fail(`${path}.${key}`, "missing required field");
  }
  for (const key of Object.keys(value)) {
    if (!required.includes(key) && !optional.includes(key)) fail(`${path}.${key}`, "unknown field");
  }
}

function date(value: unknown, path: string): void {
  if (typeof value !== "string" || !/^[0-9]{4}-[0-9]{2}-[0-9]{2}$/.test(value)) fail(path, "expected YYYY-MM-DD date");
  const year = Number(value.slice(0, 4));
  const month = Number(value.slice(5, 7));
  const day = Number(value.slice(8, 10));
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (year < 1 || month < 1 || month > 12 || day < 1 || day > (days[month - 1] ?? 0)) {
    fail(path, "expected real Gregorian date in years 0001–9999");
  }
}

// Portable ASCII URL policy: DNS-style host labels, optional decimal port 1–65535,
// RFC 3986 suffix characters and complete percent escapes. No credentials or IPv6 literals.
// This grammar is deliberately explicit so the Go and jq validators can match it.
function source(value: unknown, path: string): void {
  if (typeof value !== "string" || /[^\x21-\x7e]/.test(value)) fail(path, "expected ASCII HTTPS URL");
  const match = /^https:\/\/([^/:?#]+)(?::([0-9]+))?([/?#][A-Za-z0-9._~!$&'()*+,;=:@/?#%+-]*)?$/.exec(value);
  if (!match) fail(path, "expected HTTPS URL with host");
  const host = match[1]!;
  if (host.length > 253 || !host.split(".").every(label =>
    label.length <= 63 && /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$/.test(label))) {
    fail(path, "invalid host");
  }
  if (/^[0-9]+(?:\.[0-9]+){3}$/.test(host) && host.split(".").some(part => Number(part) > 255)) {
    fail(path, "invalid IPv4 host");
  }
  if (match[2] !== undefined && (Number(match[2]) < 1 || Number(match[2]) > 65535)) fail(path, "invalid port");
  if (/%(?![0-9A-Fa-f]{2})/.test(match[3] ?? "")) fail(path, "invalid percent escape");
}

function unicode(value: string, path: string): void {
  if (/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(value)) {
    fail(path, "expected well-formed Unicode");
  }
}

/** Reject malformed bytes before any decoder can replace them or collapse model keys. */
export function validateCodexPricingBytes(bytes: Uint8Array): void {
  let raw: string;
  try {
    raw = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    fail("table", "expected valid UTF-8 bytes");
  }
  validateCodexPricingJSONStrings(raw);
}

/** Pricing-local lexical check: native JSON parsing still owns syntax and duplicates. */
function validateCodexPricingJSONStrings(raw: string): void {
  for (const match of raw.matchAll(/"(?:[^"\\]|\\.)*"/g)) {
    // Consume escaped backslashes/quotes as units, and valid surrogate pairs together.
    for (const escape of match[0].matchAll(/\\u[dD][89aAbB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}|\\(?:u[0-9a-fA-F]{4}|.)/g)) {
      if (escape[0].length === 6 && /^\\u[dD][89a-fA-F]/.test(escape[0])) {
        fail("table", "JSON strings must contain well-formed Unicode");
      }
    }
    unicode(match[0], "table");
  }
}

/** Validate parsed JSON once at the production module's load boundary. Duplicate keys follow JSON.parse's last-key-wins policy. */
export function validateCodexPricing(value: unknown): PricingTable {
  const table = object(value, "table");
  fields(table, "table", ["version", "input_tier_threshold_tokens", "models"]);
  if (typeof table.version !== "string" || table.version.length === 0) fail("version", "expected nonempty string");
  unicode(table.version, "version");
  const threshold = table.input_tier_threshold_tokens;
  if (typeof threshold !== "number" || !Number.isFinite(threshold) || !Number.isInteger(threshold) || threshold <= 0) {
    fail("input_tier_threshold_tokens", "expected positive finite integer");
  }
  const models = object(table.models, "models");
  if (Object.keys(models).length === 0) fail("models", "expected nonempty object");
  for (const [model, value] of Object.entries(models)) {
    if (model.length === 0) fail("models", "expected nonempty model identifier");
    unicode(model, "models");
    const path = `models.${model}`;
    const row = object(value, path);
    fields(row, path, ["verified_at", "sources", "low", "high"], ["promo_review_date"]);
    date(row.verified_at, `${path}.verified_at`);
    if (Object.hasOwn(row, "promo_review_date")) date(row.promo_review_date, `${path}.promo_review_date`);
    if (!Array.isArray(row.sources) || row.sources.length === 0) fail(`${path}.sources`, "expected nonempty array");
    for (let index = 0; index < row.sources.length; index++) source(row.sources[index], `${path}.sources[${index}]`);
    for (const tier of ["low", "high"]) {
      const rates = object(row[tier], `${path}.${tier}`);
      fields(rates, `${path}.${tier}`, ["uncached_input", "cached_input", "cache_write", "output"]);
      for (const [bucket, rate] of Object.entries(rates)) {
        if (typeof rate !== "number" || !Number.isFinite(rate) || rate < 0) {
          fail(`${path}.${tier}.${bucket}`, "expected nonnegative finite number");
        }
      }
    }
  }
  return value as PricingTable;
}
