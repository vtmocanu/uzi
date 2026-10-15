// PRD #71 M5 / PRD #2865 (load-bearing security): the pre-push CI-config guards' path matcher.
//
// The protected-path set is the worker floor (DEFAULT_CI_CONFIG_PATHS) unioned with the
// SERVER-produced claim.config.ci_config_paths. Those paths are all LEADING-DOT files, which a
// stock glob matcher silently misses by default (dotfiles are excluded unless a `dot` flag is
// set), a fail-open trap, so the guard matches with an anchored RegExp, which has no dotfile
// special-casing, rather than pulling in a glob library.

// DEFAULT_CI_CONFIG_PATHS is the WORKER-SIDE FLOOR every CI-config guard unions with the claim's
// ci_config_paths (never replaced by them): the GitLab files plus the GitHub, Forgejo and Gitea
// workflow directories. The server-produced set adds the project's real ci_config_path
// (MergeCIConfigPaths), which the worker cannot know; the floor guarantees a server set that
// omits a forge's workflow directory, or a claim that omits the field, cannot fail the guard open.
export const DEFAULT_CI_CONFIG_PATHS = [
  ".gitlab-ci.yml",
  ".gitlab/**",
  "**/*.gitlab-ci.yml",
  ".github/workflows/**",
  ".forgejo/workflows/**",
  ".gitea/workflows/**",
];

// ciConfigGuardRegex is the matcher both CI-config guards use (the default-branch repair lane and
// the automatic ci_fix push guard). The glob is server-trusted configuration, so it is trimmed and an
// empty one is dropped by the caller; the FILE PATH is matched raw. `**/` is an optional run of any
// characters ending in `/`, `**` is any characters, `*` is any character except `/`; "any"
// includes newlines (`[\s\S]`, never `.`). Anchored on the full input, without the `m` flag.
export function ciConfigGuardRegex(glob: string): RegExp {
  const escaped = glob.trim().replace(/[.+?^${}()|[\]\\]/g, "\\$&");
  const body = escaped.replace(/\*\*\/|\*\*|\*/g, (m) =>
    m === "**/" ? "(?:[\\s\\S]*/)?" : m === "**" ? "[\\s\\S]*" : "[^/]*",
  );
  return new RegExp(`^${body}$`);
}

// flagCIConfigPathsRaw flags every changed file that matches ANY of the globs, matching the exact
// raw git path: no trim, no normalisation (leading/trailing spaces, control characters, embedded
// newlines and Unicode are all preserved). Only a fully empty entry is ignored. De-duplicated, in
// input order.
export function flagCIConfigPathsRaw(changedFiles: string[], globs: string[]): string[] {
  const regexes = globs.map((g) => g.trim()).filter((g) => g !== "").map(ciConfigGuardRegex);
  const hits: string[] = [];
  for (const file of changedFiles) {
    if (file === "") continue;
    if (regexes.some((re) => re.test(file)) && !hits.includes(file)) {
      hits.push(file);
    }
  }
  return hits;
}
