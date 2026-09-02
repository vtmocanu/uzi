export function SAMPLE_PLAN(): string {
  return [
    "## Plan",
    "",
    "1. **Pair tool results to calls by id** in `web/src/components/RunEvent.tsx` — never by adjacency, so parallel calls pair correctly.",
    "2. **Fold each result under its call** with an auto-expanding error state.",
    "3. **Cap the DOM** on very long runs behind a \"show earlier\" expander.",
    "4. **Tests**: pairing (parallel calls), orphan results, cap interaction with folding.",
    "",
    "No schema or API changes. Touches `web/` only.",
  ].join("\n");
}

// SEEDED_PLAN is a user-authored plan for the PRD #209 seeded-run demo. It is written
// to be SELF-SUFFICIENT (D7): it names files and steps explicitly and assumes no prior
// conversation, because a seeded run starts cold with plan_md as its only instructions —
// unlike SAMPLE_PLAN above, which is the worker's own Phase-1 output.
export function SEEDED_PLAN(): string {
  return [
    "## Add a `--since` filter to `uzi run list`",
    "",
    "Goal: let `uzi run list` take `--since <duration>` (e.g. `--since 24h`) and show only runs created within that window.",
    "",
    "1. **CLI flag** — in `api/cmd/uzi/run.go`, add a `--since` string flag to the `run list` command; parse it with `time.ParseDuration` and reject an invalid value with a usage error.",
    "2. **Client** — thread the parsed cutoff to `ListRuns` as an optional `since time.Time`, encoded onto the query string.",
    "3. **Filter server-side** — in the runs list handler, when `since` is present add `created_at >= $since` to the query rather than filtering in Go.",
    "4. **Tests** — a unit test for the duration parse (valid + invalid), and a handler test asserting an out-of-window run is excluded.",
    "",
    "Roster: the repo's own agents in `.claude/agents/`. No migration. Touches `api/` only.",
  ].join("\n");
}
