package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMRStateIsWatcherOwned enforces the PRD #24 (M2 / review finding 11)
// invariant: runs.mr_state is written by exactly ONE query, SetRunMRState (the
// MR-close watcher). Any other query that writes the runs table — every
// run-status transition, requeue, and sweep — must never touch mr_state, or a
// run-status event would silently corrupt the watcher's edge tracking (e.g.
// clobber a recorded 'closed' back to NULL and re-fire the move). Scanning the
// query source makes a future edit that adds mr_state to, say, SetRunCompleted
// fail here at build time rather than as a stuck/looping card in production.
func TestMRStateIsWatcherOwned(t *testing.T) {
	writesRuns := regexp.MustCompile(`(?is)(update\s+runs|insert\s+into\s+runs)\b`)

	sawWriter := false
	for _, fn := range []string{"queries/runtime.sql", "queries/forge.sql"} {
		src, err := os.ReadFile(fn)
		if err != nil {
			t.Fatalf("read %s: %v", fn, err)
		}
		for name, body := range splitNamedQueries(string(src)) {
			mentionsMRState := strings.Contains(body, "mr_state")
			if name == "SetRunMRState" {
				if !mentionsMRState {
					t.Error("SetRunMRState no longer writes mr_state — the watcher's only writer vanished")
				}
				sawWriter = true
				continue
			}
			if mentionsMRState && writesRuns.MatchString(body) {
				t.Errorf("query %q writes the runs table AND references mr_state; "+
					"mr_state is watcher-owned — only SetRunMRState may write it", name)
			}
		}
	}
	if !sawWriter {
		t.Fatal("SetRunMRState query not found — did the watcher's writer move or get renamed?")
	}
}

// splitNamedQueries carves a .sql file into per-query blocks keyed by the sqlc
// `-- name: <Name> :<kind>` marker. Each block runs from its marker up to the
// next one (or EOF).
func splitNamedQueries(src string) map[string]string {
	nameRe := regexp.MustCompile(`(?m)^-- name: (\w+) :`)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	out := make(map[string]string, len(locs))
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[name] = src[loc[0]:end]
	}
	return out
}
