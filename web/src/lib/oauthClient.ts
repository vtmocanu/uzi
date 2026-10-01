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
  if (u.username !== "" || u.password !== "") return "must not contain userinfo";
  if (u.hostname === "") return "must have a host";
  if (https) return null;
  if (u.hostname !== "127.0.0.1" && u.hostname !== "[::1]") {
    return "http is allowed only for the loopback IP literals 127.0.0.1 and [::1], never localhost";
  }
  // Read off the raw string: URL drops a default port ("http://127.0.0.1:80/" has port "").
  if (!/^http:\/\/[^/?]+:\d+(?:[/?]|$)/.test(raw)) return "a loopback http URL must include a port";
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
