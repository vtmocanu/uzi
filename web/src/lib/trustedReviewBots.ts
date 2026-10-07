// Client-side check of the server's trusted review-bot allowlist rules, stricter than the
// server in places (never looser) (issue #2347,
// api/internal/settings/settings_review_bots.go). The stored value of the
// `mr_review_trusted_bots` setting is a comma-separated list of
// `<base_url>#<forge_user_id>` entries, e.g. `https://github.com#136622811`.
//
// Shared by TrustedReviewBotsCard (inline feedback) and the mock backend's PUT
// validation, so the two cannot drift from each other. The server stays the source
// of truth and re-checks every write.

export const MAX_TRUSTED_BOTS = 50;

// Go's int64 ceiling: the server parses the id with strconv.ParseInt(…, 10, 64).
const INT64_MAX = 9223372036854775807n;

// splitTrustedBots splits the stored value into trimmed, non-empty tokens, exactly as
// the server does before parsing each one.
export function splitTrustedBots(value: string): string[] {
  return value
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t !== "");
}

// baseUrlProblem explains why a base URL is not in the canonical `https://host[:port]`
// form the server requires, or returns null when it is. `fix` is the canonical form
// when one can be derived (a pasted profile URL, an upper-case host), so the UI can
// offer it as a one-click correction.
//
// The server normalizes with Go's url.Parse and then demands the input already equal
// `https://` + lower-cased host: any path (even a trailing slash), query, fragment or
// user info makes the two differ and is refused. The default port :443 is refused here
// as well, although the server accepts it: a connection can be stored as https://h:443,
// and the server's match treats the default port as absent on both sides, so the portless
// https://h this offers matches that connection too.
export function baseUrlProblem(raw: string): { message: string; fix?: string } | null {
  const v = raw.trim();
  if (v === "") return { message: "Enter the forge's address, for example https://github.com." };
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)(.*)$/.exec(v);
  if (!m) return { message: "Enter a full address starting with https://." };
  const [, scheme, authority, rest] = m;
  if (scheme.toLowerCase() !== "https") return { message: "Use https://." };
  const host = authority.includes("@") ? authority.slice(authority.lastIndexOf("@") + 1) : authority;
  if (host === "") return { message: "Add the host name." };
  // A host name or bracketed IPv6 literal, then an optional numeric port: the shapes
  // Go's url.Parse accepts in practice for a forge address.
  if (!/^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(:\d+)?$/.test(host)) {
    return { message: "This is not a valid host name." };
  }
  let canonicalHost = host.toLowerCase();
  if (canonicalHost.endsWith(":443")) canonicalHost = canonicalHost.slice(0, -4);
  const fix = `https://${canonicalHost}`;
  if (authority.includes("@")) return { message: "Leave out the user name.", fix };
  if (rest !== "") return { message: "Leave out the path.", fix };
  if (host.endsWith(":443")) return { message: "Leave out the default port :443.", fix };
  if (v !== fix) return { message: "Use lower case.", fix };
  return null;
}

// userIdProblem returns why a forge user id is not a positive base-10 int64 without
// leading zeros (the server round-trips it through ParseInt/FormatInt), or null.
export function userIdProblem(raw: string): string | null {
  const v = raw.trim();
  if (v === "") return "Enter the bot's numeric id.";
  if (!/^\d+$/.test(v)) return "Use the numeric id, not the login name.";
  if (!/^[1-9]\d*$/.test(v)) return "Use a positive number without leading zeros.";
  if (BigInt(v) > INT64_MAX) return "This number is too large to be a user id.";
  return null;
}

// entryKey is the stored form of one entry; two rows with the same key are duplicates.
export function entryKey(baseUrl: string, userId: string): string {
  return `${baseUrl.trim()}#${userId.trim()}`;
}

// trustedBotsValueError follows the server's write-time validateTrustedBots (but is
// stricter, e.g. it refuses :443): empty is valid; otherwise every entry must be canonical, unique, and there are at most
// MAX_TRUSTED_BOTS of them. Returns the first problem, or null.
export function trustedBotsValueError(value: string): string | null {
  const seen = new Set<string>();
  const tokens = splitTrustedBots(value);
  for (const tok of tokens) {
    const hash = tok.indexOf("#");
    if (hash < 0) return `entry "${tok}" must look like <base_url>#<forge_user_id>`;
    const base = tok.slice(0, hash);
    const id = tok.slice(hash + 1);
    // The server compares the untrimmed base with its normalized form, so a space
    // around the '#' is an error there too.
    const baseErr = base !== base.trim() ? { message: "remove the spaces" } : baseUrlProblem(base);
    if (baseErr) return `entry "${tok}": ${baseErr.message}`;
    const idErr = id !== id.trim() ? "remove the spaces" : userIdProblem(id);
    if (idErr) return `entry "${tok}": ${idErr}`;
    if (seen.has(tok)) return `duplicate entry "${tok}"`;
    seen.add(tok);
  }
  if (seen.size > MAX_TRUSTED_BOTS) return `at most ${MAX_TRUSTED_BOTS} entries are allowed`;
  return null;
}
