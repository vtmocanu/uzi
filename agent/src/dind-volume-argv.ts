// Leaf module: the frozen docker argv pair used for the anonymous-volume prune, shared by
// dind-maintenance.ts (terminal-time optional prune) and dind-volume-prune.ts (issue #3891
// pressure-only leg). It has NO imports on purpose, so both can depend on it without a cycle.

/** `docker version --format {{.Server.APIVersion}}`: the server API version query. */
export const DIND_VOLUME_SERVER_VERSION_ARGV: readonly string[] = Object.freeze([
  "version",
  "--format",
  "{{.Server.APIVersion}}",
]);

/** `docker volume prune -f`: from API 1.42 the default removes only anonymous volumes. */
export const DIND_VOLUME_PRUNE_ARGV: readonly string[] = Object.freeze(["volume", "prune", "-f"]);

/** True only for a well-formed `major.minor` API version that is at least 1.42. */
export function apiVersionAtLeast142(version: string): boolean {
  const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.exec(version);
  if (!match || !Number.isSafeInteger(Number(match[1])) || !Number.isSafeInteger(Number(match[2]))) return false;
  return Number(match[1]) > 1 || (Number(match[1]) === 1 && Number(match[2]) >= 42);
}
