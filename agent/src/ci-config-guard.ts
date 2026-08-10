// PRD #71 M5 (load-bearing security): the pre-push CI-config guard's path matcher.
//
// The protected-path set is SERVER-produced (claim.config.ci_config_paths) — the
// static CI_AUTOFIX_CONFIG_PATHS defaults (`.gitlab-ci.yml`, `.gitlab/**`,
// `**/*.gitlab-ci.yml`) unioned with the project's configured `ci_config_path`.
// Those defaults are all LEADING-DOT files, which a stock glob matcher silently
// misses by default (dotfiles are excluded unless a `dot` flag is set) — a
// fail-open trap. So, exactly as self-improve.ts's flagGuardPaths does with its
// GUARD_CRITICAL_PATTERNS, we match with anchored RegExp, which has no dotfile
// special-casing. This module reuses that MECHANISM, parameterized by the claim's
// ci_config_paths, rather than pulling in a glob library.

// ciConfigPathToRegex converts one server-supplied CI-config path/glob to an
// anchored, dotfile-safe RegExp. Supports: `**` (any path segments, incl. none),
// `*` (any run of non-slash chars), and literal segments; a plain path (no
// wildcards) matches exactly. Regex has no dotfile special-casing, so
// `.gitlab-ci.yml` matches (the stock-glob trap).
export function ciConfigPathToRegex(glob: string): RegExp {
  const trimmed = glob.trim();
  // Escape every regex metacharacter EXCEPT `*` (glob wildcard) and `/` (a plain
  // path separator, not a regex metachar). `*` is translated afterward.
  const escaped = trimmed.replace(/[.+?^${}()|[\]\\]/g, "\\$&");
  // Translate globs in ONE left-to-right pass so an expansion is never re-scanned:
  // a two-step `**`→`.*` then `*`→`[^/]*` would re-mangle the `.*` (its own `*`
  // becomes `[^/]*`). The alternation is ordered longest-first — `**/` (optional
  // prefix, matches zero dirs too) before a bare `**` before a single `*` — so each
  // wildcard is consumed by exactly one arm. Only `*` runs remain in `escaped`
  // (every regex metachar was escaped above), so nothing else is touched.
  const body = escaped.replace(/\*\*\/|\*\*|\*/g, (m) =>
    m === "**/" ? "(?:.*/)?" : m === "**" ? ".*" : "[^/]*",
  );
  return new RegExp(`^${body}$`);
}

// flagCIConfigPaths flags every changed file that matches ANY of the CI-config
// paths, de-duplicated and in input order — mirroring flagGuardPaths' shape.
export function flagCIConfigPaths(changedFiles: string[], paths: string[]): string[] {
  const regexes = paths.map((p) => p.trim()).filter((p) => p !== "").map(ciConfigPathToRegex);
  const hits: string[] = [];
  for (const f of changedFiles) {
    const file = f.trim();
    if (file === "") continue;
    if (regexes.some((re) => re.test(file)) && !hits.includes(file)) {
      hits.push(file);
    }
  }
  return hits;
}
