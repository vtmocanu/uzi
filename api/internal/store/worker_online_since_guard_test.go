package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// PRD #251 — online_since is a DISPLAY-ONLY liveness fact, the same contract PRD #49's
// stats_* columns hold: it is WRITTEN by the liveness path (RegisterWorker,
// HeartbeatWorker) and CLEARED by the sweeper (MarkStaleWorkersOffline), and READ only by
// the worker-list SELECTs that feed the DTO. No claim/scheduling/sweeper-SELECT query may
// READ it — a value the control plane stamps for display must never become an input to who
// gets scheduled, or "how long has this been up" quietly turns into a scheduling lever.
//
// This is a static scan, not *LiveDB: it runs in the ordinary `go test ./...` gate where a
// drifting query is actually noticed, and it needs no database.
//
// APPROACH — why a bare column-name scan is enough here, stated because the task flagged
// that SELECT */w.* reads could make one noisy. They do NOT: the worker-list reads
// (ListWorkersByUser, ListAllWorkers, GetWorkerByID/…, CreateWorker, …) project the row
// with `SELECT *` / `sqlc.embed(w)` / `RETURNING *`, so they never spell `online_since`
// literally — sqlc expands the star at generate time, and the DTO gets the column for free
// through the model struct. So the queries whose *source text* contains the identifier
// `online_since` are the three liveness WRITES plus the one authorized non-scheduling
// consumer below. The guard is: every query whose body names `online_since` must be in the
// allow set. Any new reader added in a WHERE/ORDER BY/JOIN of a CLAIM/SCHEDULING query —
// the shapes we are defending against — names the column literally and trips this. A future
// explicit-projection SELECT that legitimately reads the column for the DTO, or another
// GC/teardown consumer, would need adding to allowedOnlineSinceQueries with a note.
func TestOnlineSinceIsWriteOnlyAndNeverScheduled(t *testing.T) {
	// The queries permitted to reference online_since at all. The first three are WRITES on
	// the liveness path; none READ it in a predicate/order/join. The fourth is the ONE
	// authorized reader — a GC/teardown DELETE, never a scheduling lever.
	allowedOnlineSinceQueries := map[string]bool{
		"RegisterWorker":          true, // stamps/preserves the anchor on coming online
		"HeartbeatWorker":         true, // stamps/preserves the anchor on coming online
		"MarkStaleWorkersOffline": true, // clears the anchor when the worker goes stale
		// ReapEphemeralWorkers (PRD #529 M5, Decision 6) reads online_since in its WHERE to
		// time out a stuck ephemeral worker: NULL-past-the-deadline == "never booted", and
		// online-past-the-deadline gates the idle-stolen shape. This is GC teardown, not
		// scheduling — it only DELETEs an ephemeral worker that can no longer make progress
		// and never orders/selects/prefers a live worker for claiming, so online_since stays
		// off the who-gets-scheduled path the rest of this guard protects.
		"ReapEphemeralWorkers": true,
	}

	paths, err := filepath.Glob(filepath.Join("queries", "*.sql"))
	if err != nil {
		t.Fatalf("glob queries: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("scanned 0 query files — this test would pass for any tree")
	}

	nameRe := regexp.MustCompile(`(?m)^-- name: (\w+) `)

	var offenders []string
	var sawAllowedRef int
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(b)

		// Split the file into per-query segments keyed by the `-- name:` header so a
		// reference can be attributed to the query it belongs to.
		locs := nameRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			name := text[loc[2]:loc[3]]
			start := loc[0]
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := text[start:end]
			if !strings.Contains(body, "online_since") {
				continue
			}
			if allowedOnlineSinceQueries[name] {
				sawAllowedRef++
				continue
			}
			offenders = append(offenders, filepath.Base(path)+":"+name)
		}
	}

	// Positive control: the three allowed writes must actually reference the column, or a
	// rename/removal would make this test vacuously green while the anchor stopped being
	// written at all.
	if sawAllowedRef < len(allowedOnlineSinceQueries) {
		t.Fatalf("only %d of the %d allowed queries reference online_since; the write path may have "+
			"been renamed or dropped, which would make this guard vacuous", sawAllowedRef, len(allowedOnlineSinceQueries))
	}

	if len(offenders) > 0 {
		t.Errorf("online_since is DISPLAY-ONLY (PRD #251), but %d query/queries outside the liveness "+
			"write set reference it: %s.\nIt must be written by RegisterWorker/HeartbeatWorker, cleared "+
			"by MarkStaleWorkersOffline, and read only via the worker-list SELECTs' `SELECT *` expansion "+
			"— never in a WHERE/ORDER BY/JOIN of a claim, scheduling, or sweeper-SELECT query. If a new "+
			"query legitimately projects it for the DTO, add it to allowedOnlineSinceQueries with a note.",
			len(offenders), strings.Join(offenders, ", "))
	}
}
