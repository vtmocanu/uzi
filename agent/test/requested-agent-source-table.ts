import type { AgentSource } from "../src/protocol.js";

export interface RequestedSourceCase {
  requested: AgentSource | null | undefined;
  roster: boolean;
  want: AgentSource;
  note: boolean;
}

export const REQUESTED_SOURCE_TABLE: readonly RequestedSourceCase[] = [
  { requested: undefined, roster: true, want: "repo", note: false },
  { requested: undefined, roster: false, want: "own", note: false },
  { requested: null, roster: true, want: "repo", note: false },
  { requested: "own", roster: true, want: "own", note: false },
  { requested: "own", roster: false, want: "own", note: false },
  { requested: "repo", roster: true, want: "repo", note: false },
  { requested: "repo", roster: false, want: "own", note: true },
];

export const NO_ROSTER_NOTE = "this clone has no repo agent roster; using your own agent templates";

export function caseLabel(c: RequestedSourceCase): string {
  return `requested=${String(c.requested)} roster=${c.roster}`;
}
