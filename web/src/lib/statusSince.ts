// Issue #1727: the instant a run entered its CURRENT status. The API sends runs.status_since
// on every run DTO; updated_at is NOT a substitute on a current server, because it moves on
// unrelated writes (an extend, for example) while the run stays in the same status. The
// fallback to updated_at covers an older server that omits the key, a defensive null, and an
// unparseable value, and a narrow shape (the board's LatestRun) that never carries it.
export interface StatusSinceInput {
  updated_at: string;
  status_since?: string | null;
}

// statusSinceIso returns status_since when it is present and parseable, else updated_at.
export function statusSinceIso(run: StatusSinceInput): string {
  const since = run.status_since;
  if (since != null && !Number.isNaN(Date.parse(since))) return since;
  return run.updated_at;
}
