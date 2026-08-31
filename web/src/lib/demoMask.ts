// PRD #886 — Demo mode pure masking helpers.
//
// Every helper is deterministic (same input → same output, no per-render randomness so
// a multi-screenshot demo stays coherent) and is the IDENTITY function when `enabled` is
// false, so a call site can wrap any display value unconditionally and pay nothing when
// demo mode is off. Empty / null / undefined input passes through (coerced to "") rather
// than throwing. `enabled` comes from useDemoMode() at the call site.
//
// These mask the DISPLAY channel only. Never route a React key, an href/route, an input
// value, or a search/dirty-check input through these — decision 1 in the PRD keeps those
// raw. People collapse to a first name; infrastructure uses reserved/unmistakably-fake
// values (example.com, forge.example.com, TEST-NET-3) so a leaked mask is obviously fake.

// capitalize upper-cases the first character and lower-cases the rest, so "vlad" and
// "VLAD" both render as "Vlad".
function capitalize(token: string): string {
  if (!token) return token;
  return token.charAt(0).toUpperCase() + token.slice(1).toLowerCase();
}

// maskEmail derives a first name from an email's local-part: the text before "@" (or the
// whole string if there is no "@"), split on the first of ./_/+/-, first token
// capitalized. A local-part with no separator uses the whole local-part capitalized.
// `vlad.mocanu@metaminds.com` → `Vlad`; `vlad@x.com` → `Vlad`; `vlad` → `Vlad`.
export function maskEmail(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  const atIndex = value.indexOf("@");
  const local = atIndex === -1 ? value : value.slice(0, atIndex);
  const firstToken = local.split(/[._+-]/)[0];
  return capitalize(firstToken);
}

// maskName keeps the first whitespace token, capitalized. `Vlad Mocanu` → `Vlad`;
// single-word `Vlad` → `Vlad`.
export function maskName(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  const firstToken = value.trim().split(/\s+/)[0];
  return capitalize(firstToken);
}

// maskRepoPath keeps the LAST path segment (the repo) and replaces everything before it
// with `demo`. `vtmocanu/uzi` → `demo/uzi`; `group/sub/repo` → `demo/repo`.
// Decision: a bare segment with no `/` also becomes `demo/<repo>` (e.g. `uzi` →
// `demo/uzi`) — the namespace is what identifies the operator, and prefixing `demo/`
// uniformly is the safest hide (never leaks a real namespace) and keeps the display
// shape consistent whether or not the raw value carried one.
export function maskRepoPath(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  const parts = value.split("/").filter(Boolean);
  if (parts.length === 0) return value;
  const repo = parts[parts.length - 1];
  return `demo/${repo}`;
}

// maskHost replaces the host, port, path, and query with a single fake host —
// `forge.example.com` — KEEPING only the scheme, per the registry ("replace host with
// forge.example.com, keep scheme"). `https://gitlab.metaminds.com/team → https://forge.example.com`.
// Dropping the path/query is deliberate: a self-hosted forge under a subpath would otherwise
// leak that segment, which is the exact class of identity leak demo mode exists to hide.
// Unparseable input or a bare host with no scheme falls back to the bare fake host.
export function maskHost(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  try {
    const u = new URL(value);
    return `${u.protocol}//forge.example.com`;
  } catch {
    return "forge.example.com";
  }
}

// maskUsername masks a forge username by role: `demo-user` for a human, `demo-bot` for a
// bot. (The PRD tech-scope lists it as `maskUsername(value, role)`, but every helper also
// takes `enabled`; the concrete signature is `(value, role, enabled)`.)
export function maskUsername(
  value: string | null | undefined,
  role: "human" | "bot",
  enabled: boolean,
): string {
  if (!enabled || !value) return value ?? "";
  return role === "bot" ? "demo-bot" : "demo-user";
}

// maskIp masks a last-used IP to a fixed TEST-NET-3 address (RFC 5737), which is
// reserved for documentation so a leaked one is obviously not real.
export function maskIp(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  return "203.0.113.7";
}

// maskDomains masks the JOINED display string of the registration email-domain allowlist
// to `example.com`. Per the PRD caveat, mask the joined display string at each display
// site; never mask the underlying domains array (it feeds emailDomainAllowed()).
export function maskDomains(value: string | null | undefined, enabled: boolean): string {
  if (!enabled || !value) return value ?? "";
  return "example.com";
}
