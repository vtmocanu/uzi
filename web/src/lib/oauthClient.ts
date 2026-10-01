// Redirect-URI rules for a product's OAuth client registration (PRD #1910 D2). The server
// (api/internal/oauthsrv ValidateRedirectURI) is the authority; this mirrors it so the admin form
// refuses a bad list before the PUT and the mock API answers like the real one.

export const MAX_REDIRECT_URIS = 5;
const MAX_REDIRECT_URI_BYTES = 2048;

/** Splits a one-URI-per-line textarea into trimmed, non-empty lines. */
export function parseRedirectUriLines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");
}

// Query keys the server itself adds to a redirect (RFC 6749 4.1.2, 4.1.2.1, RFC 9207); a registered
// URI may not carry them. Case-sensitive on the decoded key, as the server's check is.
const RESERVED_QUERY_KEYS = new Set(["code", "state", "iss", "error", "error_description", "error_uri"]);

function hasReservedQueryKey(raw: string): boolean {
  const q = raw.indexOf("?");
  if (q < 0) return false;
  for (const pair of raw.slice(q + 1).split("&")) {
    if (pair === "") continue;
    const key = pair.split("=", 1)[0];
    try {
      if (RESERVED_QUERY_KEYS.has(decodeURIComponent(key.replace(/\+/g, " ")))) return true;
    } catch {
      return true; // an undecodable key cannot be shown to be harmless
    }
  }
  return false;
}

function uriError(raw: string): string | null {
  if (new TextEncoder().encode(raw).length > MAX_REDIRECT_URI_BYTES) {
    return `must be at most ${MAX_REDIRECT_URI_BYTES} bytes`;
  }
  // Printable ASCII only: no whitespace, control or non-ASCII character.
  if (!/^[\x21-\x7e]+$/.test(raw)) return "must contain only printable ASCII characters";
  if (raw.includes("#")) return "must not contain a fragment";
  const https = raw.startsWith("https://");
  if (!https && !raw.startsWith("http://")) {
    return "must be an https URL (http is allowed only for 127.0.0.1 and [::1] with a port)";
  }
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return "is not a valid URL";
  }
  if (hasReservedQueryKey(raw)) {
    return "query must not contain code, state, iss, error, error_description or error_uri";
  }
  if (u.username !== "" || u.password !== "") return "must not contain userinfo";
  if (u.hostname === "") return "must have a host";
  if (https) return null;
  // Compare the RAW authority text, as the server does (oauthsrv.ValidateRedirectURI): the host
  // must literally be 127.0.0.1 or [::1]. u.hostname is the WHATWG-normalised host, which would
  // also admit 127.1, 2130706433 and [0:0:0:0:0:0:0:1] (all normalise to a loopback address).
  const authority = raw.slice("http://".length).split(/[/?]/, 1)[0];
  const m = /^(127\.0\.0\.1|\[::1\])(?::(\d*))?$/.exec(authority);
  if (!m) {
    return "http is allowed only for the loopback IP literals 127.0.0.1 and [::1], never localhost";
  }
  if (!m[2]) return "a loopback http URL must include a port";
  const port = Number(m[2]);
  if (port < 1 || port > 65535) return "port must be between 1 and 65535";
  return null;
}

/** Returns the first problem with a redirect-URI list, or null when it is acceptable. */
export function redirectUrisError(uris: string[]): string | null {
  if (uris.length > MAX_REDIRECT_URIS) return `at most ${MAX_REDIRECT_URIS} redirect URIs are allowed`;
  const seen = new Set<string>();
  for (const [i, u] of uris.entries()) {
    const err = uriError(u);
    if (err !== null) return `Redirect URI ${i + 1} ${err}`;
    if (seen.has(u)) return `Redirect URI ${i + 1} is a duplicate`;
    seen.add(u);
  }
  return null;
}

// What each OAuth scope lets a connected product do, in plain words, shared by the consent page,
// the user's Connected products list and the admin Connections panel so all three say it the same
// way. An unknown scope (a newer server) is shown as its raw text by scopeText, never hidden: the
// reader should see everything that was granted.
const SCOPE_TEXT: Record<string, string> = {
  "jobs:run": "Run jobs",
  "jobs:read": "Read jobs and their results",
};

export function scopeText(scope: string): string {
  return SCOPE_TEXT[scope] ?? scope;
}
