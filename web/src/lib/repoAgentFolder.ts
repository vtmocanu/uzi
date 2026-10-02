import type { RepoAgent } from "./apiTypes";

/** The folder a repo agent roster was read from. All elements share one folder, so the
 *  first element decides; an absent folder (pre-feature run, no negotiation) or an empty
 *  or null roster means ".claude/agents". */
export function repoAgentFolder(roster: readonly RepoAgent[] | null | undefined): string {
  return roster?.[0]?.folder ?? ".claude/agents";
}
