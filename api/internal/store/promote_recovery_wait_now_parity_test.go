package store_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPromoteRecoveryWaitNowMirrorsSweepPass is the recovery-pair twin of
// TestPromoteLimitWaitNowMirrorsSweepPass (promote_limit_wait_now_parity_test.go): the
// single-row early promote `PromoteRecoveryWaitRunNow` (the `uzi run set-token` verb for a
// recovery_wait run) MUST mutate exactly the same runs columns as the sweeper's
// `PromoteRecoveryWaitRuns` pass — no more, no fewer — so an owner-driven early promote and
// the automatic promote leave a recovery_wait run in byte-identically the same shape (fresh
// wall, cleared pause bank, revoked codex cap, reset health). If someone adds a column to one
// promote and forgets the other, the two resume paths silently diverge; this DB-less test in
// the ordinary `go test ./...` gate catches that drift, exactly as the limit-wait guard does
// for its pair.
//
// It ALSO pins two intents by name:
//   - the mutated set is exactly the 10 columns listed below (naming every one, so a
//     reviewer sees the whole set and a drift in EITHER query reddens here);
//   - the load-bearing PRESERVED columns (worker_id, session_id, last_seq, and the recovery
//     backoff/history pair recovery_wait_count + recovery_retry_not_before) appear in NEITHER
//     SET clause — they are left in place so worker/session affinity survives the resume and
//     the recovery cadence keeps shaping the next park.
//
// It reads the .sql query source (not the generated Go const) via the shared setClauseColumns
// helper defined alongside the limit-wait parity test in this package.
func TestPromoteRecoveryWaitNowMirrorsSweepPass(t *testing.T) {
	const queryFile = "queries/runtime.sql"
	raw, err := os.ReadFile(filepath.FromSlash(queryFile))
	if err != nil {
		t.Fatalf("read %s: %v", queryFile, err)
	}

	sweep := setClauseColumns(t, string(raw), "PromoteRecoveryWaitRuns")
	now := setClauseColumns(t, string(raw), "PromoteRecoveryWaitRunNow")

	// The exact mutated set both must carry (mirrors PromoteLimitWaitRuns field-for-field).
	// Naming them here makes the reviewer's job a diff against this list, and a column added
	// to only one query fails against BOTH this expectation AND the sweep==now equality below.
	want := []string{
		"budget_paused_seconds",
		"codex_cap_hash",
		"codex_claim_epoch",
		"health",
		"health_reason",
		"health_since",
		"started_at",
		"status",
		"status_since",
		"updated_at",
	}

	if !reflect.DeepEqual(now, want) {
		t.Fatalf("PromoteRecoveryWaitRunNow mutates %v, want exactly %v", now, want)
	}
	if !reflect.DeepEqual(sweep, want) {
		t.Fatalf("PromoteRecoveryWaitRuns mutates %v, want exactly %v — the early promote pins itself to this set, so the sweep pass drifting reddens here too", sweep, want)
	}
	if !reflect.DeepEqual(sweep, now) {
		t.Fatalf("column-parity broken: PromoteRecoveryWaitRuns mutates %v but PromoteRecoveryWaitRunNow mutates %v — the two resume paths must stay identical", sweep, now)
	}

	// The load-bearing PRESERVED columns must appear in NEITHER SET clause. If any of these
	// creeps into the early promote, worker/session affinity is lost or the recovery cadence
	// (recovery_wait_count backoff shaping, recovery_retry_not_before history) is reset.
	preserved := []string{
		"worker_id", "session_id", "last_seq",
		"recovery_wait_count", "recovery_retry_not_before",
	}
	mutated := map[string]bool{}
	for _, c := range now {
		mutated[c] = true
	}
	for _, c := range preserved {
		if mutated[c] {
			t.Errorf("PromoteRecoveryWaitRunNow mutates %q, but it MUST be preserved (early promote must leave worker/session affinity and the recovery cadence intact)", c)
		}
	}
}
