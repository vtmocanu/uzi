// Plain-semver parse and compare for the changelog's current/newer markers
// (PRD #415 M3). Model B pins the release coordinate to `X.Y.Z` (chart version ==
// appVersion == git tag), so ONLY that exact shape parses. A `-rc.N` prerelease is
// a documented limitation and returns null; so do the `dev`/`demo` pseudo-versions
// and anything unshaped. The drawer treats a null-parsing running version as
// NEUTRAL — no markers, no banner — rather than mis-ordering it, which is why the
// null case is a first-class return here and not a throw.

// parseSemver returns the three numeric fields, or null when the input is not a
// plain `X.Y.Z`. Fields compare NUMERICALLY (see compareSemver), so `0.10.0`
// sorts after `0.9.0` — a lexical compare would get that backwards.
export function parseSemver(v: string | null | undefined): [number, number, number] | null {
  if (typeof v !== "string") return null;
  const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(v);
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

// compareSemver returns <0 / 0 / >0 for a<b / a==b / a>b, comparing the three
// fields numerically. An UNPARSEABLE side sorts LAST (a non-semver `a` returns >0
// so it orders after a real version, and vice versa); two unparseable sides are
// equal. The drawer's marker logic guards with parseSemver before calling this, so
// the null branches exist to keep the function total rather than to be relied on
// for ordering pseudo-versions.
export function compareSemver(a: string, b: string): number {
  const pa = parseSemver(a);
  const pb = parseSemver(b);
  if (pa === null && pb === null) return 0;
  if (pa === null) return 1; // a is not semver: push it last
  if (pb === null) return -1; // b is not semver: push it last
  for (let i = 0; i < 3; i++) {
    if (pa[i] !== pb[i]) return pa[i] - pb[i];
  }
  return 0;
}
