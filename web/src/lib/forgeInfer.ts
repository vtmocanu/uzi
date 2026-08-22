// Host→forge-type inference for the connect form's base-URL ⇄ type sync
// (PRD #337 Feature A / D2). Kept in a sibling module rather than forgeNoun.ts so
// the "exactly one GitLab-noun literal" acceptance test there stays clean.

// Canonical public hosts whose name does not advertise the software (D3). Matched by
// host equality OR suffix, so git.codeberg.org resolves too. Deliberately minimal.
const HOST_ALIASES: Array<{ host: string; type: string }> = [
  { host: "codeberg.org", type: "forgejo" },
];

// Resolve the single candidate type from a host, ignoring the advertised gate.
// null = unrecognized. Substring checks run in a fixed order (github, gitlab,
// forgejo); a real host cannot contain two of these, so the order is only a
// determinism guarantee, not a precedence policy.
function candidateForHost(host: string): string | null {
  if (host.includes("github")) return "github";
  if (host.includes("gitlab")) return "gitlab";
  if (host.includes("forgejo")) return "forgejo";
  for (const { host: alias, type } of HOST_ALIASES) {
    if (host === alias || host.endsWith(`.${alias}`)) return type;
  }
  return null;
}

// inferForgeType returns the type a base URL's host maps to, but ONLY if that type is
// advertised in forgeTypes (D4) — so it can never select a forge the instance did not
// enable. A malformed/empty baseUrl or an unrecognized host returns null ("user
// chooses"). hostname is already lowercased, so ports and case need no handling.
export function inferForgeType(baseUrl: string, forgeTypes: string[]): string | null {
  let host: string;
  try {
    host = new URL(baseUrl).hostname;
  } catch {
    return null;
  }
  const candidate = candidateForHost(host);
  if (candidate === null) return null;
  return forgeTypes.includes(candidate) ? candidate : null;
}

// defaultUrlForType returns the first allowlist URL whose host infers to forgeType,
// else null. Reuses inferForgeType with forgeTypes=[forgeType] so the advertised gate
// passes exactly when the host matches the type we are looking for.
export function defaultUrlForType(forgeType: string, allowedUrls: string[]): string | null {
  for (const url of allowedUrls) {
    if (inferForgeType(url, [forgeType]) === forgeType) return url;
  }
  return null;
}
