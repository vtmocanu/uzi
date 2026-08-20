// A tiny, dependency-free comparator for the exact `X.Y.Z` shape uzi's release
// tags carry (Model B: chart `version` == `appVersion` == git tag). It exists so
// the changelog panel can decide "am I running this / is a newer release out?"
// with NUMERIC field ordering — `0.9.0 < 0.10.0` — which a lexical string compare
// gets wrong (`"0.9.0" > "0.10.0"`), and without pulling the full `semver` package
// (range parsing, prerelease precedence, coercion) into the browser bundle for a
// three-integer compare.
//
// NO PRERELEASE SUPPORT, deliberately: only strictly `\d+\.\d+\.\d+` parses. A
// `-rc.1` suffix, a `v` prefix, `dev`/`demo`, or anything else returns null from
// parseSemver, and the changelog panel reads that null as "not a comparable
// version" and shows no current/newer markers rather than guessing.

/** Parse a strict `X.Y.Z` (numeric, no `v` prefix, no `-rc` suffix) into its
 *  three integer fields, or null for anything else — including `dev`, `0.1.0-rc.1`
 *  and `v1.2.3`. */
export function parseSemver(v: string): [number, number, number] | null {
  const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(v);
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

/** Compare two version strings by NUMERIC field, returning <0 / 0 / >0 for
 *  a<b / a==b / a>b. Only meaningful for strict `X.Y.Z` inputs; a non-parseable
 *  input (dev, prerelease) is ordered BELOW any real semver and equal to another
 *  non-parseable one, so callers get a total order rather than a throw. Callers
 *  that care about "is this a real version at all" gate on parseSemver first. */
export function compareSemver(a: string, b: string): number {
  const pa = parseSemver(a);
  const pb = parseSemver(b);
  if (pa === null || pb === null) {
    if (pa === null && pb === null) return 0;
    return pa === null ? -1 : 1;
  }
  for (let i = 0; i < 3; i++) {
    if (pa[i] !== pb[i]) return pa[i] - pb[i];
  }
  return 0;
}
