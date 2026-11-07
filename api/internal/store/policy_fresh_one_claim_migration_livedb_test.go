package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const freshShapeConstraint = "runs_policy_fresh_generation_shape"

var freshColumns = []string{"policy_fresh_consumed_generation", "policy_fresh_confirmed_generation"}

// queuedFresh is a same_conversation hold resumed with the real one-claim Fresh query: queued,
// pending_fresh true, session cleared, still preserving.
func (f *downFx) queuedFresh(head string) uuid.UUID {
	f.t.Helper()
	id := f.running()
	f.resume(id, "fresh_one_claim", f.hold(id, 1, "same_conversation", head))
	return id
}

// consumedFresh is a queuedFresh that a claim at generation gen consumed; confirmed stamps it too.
func (f *downFx) consumedFresh(gen int64, confirmed bool) uuid.UUID {
	f.t.Helper()
	id := f.queuedFresh(strings.Repeat("a", 40))
	mustExec(f.ctx, f.t, f.pool, "UPDATE runs SET policy_pending_fresh=false,policy_fresh_consumed_generation=$2,claim_generation=$2 WHERE id=$1", id, gen)
	if confirmed {
		mustExec(f.ctx, f.t, f.pool, "UPDATE runs SET policy_fresh_confirmed_generation=$2 WHERE id=$1", id, gen)
	}
	return id
}

// baseColumns are migration 00397 columns that neither the 415 nor the 416 Down may drop.
var baseColumns = []string{"policy_preserve_required", "policy_outcome_id", "policy_hold_published", "policy_pending_fresh"}

func (f *downFx) freshValidated() bool {
	f.t.Helper()
	var v bool
	if err := f.pool.QueryRow(f.ctx, "SELECT convalidated FROM pg_constraint WHERE conname=$1", freshShapeConstraint).Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

// toMf steps the head fixture (417) down to 416 through migration 417's Down: no conversion and no
// refusal, the Fresh columns stay and the shape check is NOT VALID. It is a no-op once at 416.
func (f *downFx) toMf() {
	f.t.Helper()
	if f.version() != 417 {
		return
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 416); err != nil {
		f.t.Fatalf("Down to 416: %v", err)
	}
	if f.version() != 416 || f.columns(freshColumns...) != 2 || f.freshValidated() {
		f.t.Fatalf("417 Down left version %d, %d fresh columns, validated=%v", f.version(), f.columns(freshColumns...), f.freshValidated())
	}
}

func (f *downFx) mfRefused(wantMsg string) {
	f.t.Helper()
	f.toMf()
	before := f.snapshot()
	err := store.MigrateDownTo(f.ctx, f.dsn, 415)
	if err == nil || !strings.Contains(err.Error(), wantMsg) {
		f.t.Fatalf("Down error = %v, want it to contain %q", err, wantMsg)
	}
	if f.snapshot() != before || f.version() != 416 || f.columns(freshColumns...) != 2 {
		f.t.Fatal("refused Down changed rows, history or schema")
	}
}

// Migration 416 is additive: existing rows gain NULL stamps, the shape check is added NOT VALID and
// still guards new writes, and Down drops exactly its two columns.
func TestPolicyFreshOneClaimMigrationLiveDB(t *testing.T) {
	f := newDownFx(t)
	if f.version() != 417 || f.columns(freshColumns...) != 2 {
		t.Fatalf("head fixture: version %d, %d fresh columns", f.version(), f.columns(freshColumns...))
	}
	f.toMf()
	var validated bool
	if err := f.pool.QueryRow(f.ctx, "SELECT convalidated FROM pg_constraint WHERE conname=$1", freshShapeConstraint).Scan(&validated); err != nil || validated {
		t.Fatalf("shape check must be NOT VALID (validated=%v, err=%v)", validated, err)
	}
	id := f.queuedPreserving(strings.Repeat("a", 40))
	var nulls bool
	if err := f.pool.QueryRow(f.ctx, "SELECT policy_fresh_consumed_generation IS NULL AND policy_fresh_confirmed_generation IS NULL FROM runs WHERE id=$1", id).Scan(&nulls); err != nil || !nulls {
		t.Fatalf("a row without Fresh history must carry NULL stamps: %v %v", nulls, err)
	}
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET policy_fresh_consumed_generation=2 WHERE id=$1", id)
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET policy_fresh_confirmed_generation=2 WHERE id=$1", id)
	for name, stmt := range map[string]string{
		"zero consumed":                   "UPDATE runs SET policy_fresh_consumed_generation=0,policy_fresh_confirmed_generation=NULL WHERE id=$1",
		"unsafe consumed":                 "UPDATE runs SET policy_fresh_consumed_generation=9007199254740992,policy_fresh_confirmed_generation=NULL WHERE id=$1",
		"confirmed differs from consumed": "UPDATE runs SET policy_fresh_confirmed_generation=3 WHERE id=$1",
		"confirmed without consumed":      "UPDATE runs SET policy_fresh_consumed_generation=NULL WHERE id=$1",
	} {
		if _, err := f.pool.Exec(f.ctx, stmt, id); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET policy_fresh_confirmed_generation=NULL,policy_fresh_consumed_generation=NULL WHERE id=$1", id)
	if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if f.version() != 415 || f.columns(freshColumns...) != 0 || f.columns(recoveryColumns...) != 4 {
		t.Fatalf("Down left version %d, %d fresh columns and %d recovery columns", f.version(), f.columns(freshColumns...), f.columns(recoveryColumns...))
	}
	if err := store.MigrateTo(f.ctx, f.dsn, 416); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	if f.version() != 416 || f.columns(freshColumns...) != 2 {
		t.Fatal("Up after Down did not restore the schema")
	}
}

// Migration 417 validates the 416 shape check; its Down restores the NOT VALID check, which still
// guards new writes, and the 416 Down then proceeds as before.
func TestPolicyFreshOneClaimValidateMigrationLiveDB(t *testing.T) {
	f := newDownFx(t)
	id := f.consumedFresh(2, true)
	validated := func() bool {
		var v bool
		if err := f.pool.QueryRow(f.ctx, "SELECT convalidated FROM pg_constraint WHERE conname=$1", freshShapeConstraint).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := store.MigrateTo(f.ctx, f.dsn, 417); err != nil {
		t.Fatalf("Up to 417: %v", err)
	}
	if f.version() != 417 || !validated() {
		t.Fatalf("417 left version %d, validated=%v", f.version(), validated())
	}
	if _, err := f.pool.Exec(f.ctx, "UPDATE runs SET policy_fresh_confirmed_generation=3 WHERE id=$1", id); err == nil {
		t.Fatal("validated shape check accepted confirmed != consumed")
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 416); err != nil {
		t.Fatalf("Down to 416: %v", err)
	}
	if f.version() != 416 || validated() || f.columns(freshColumns...) != 2 {
		t.Fatalf("417 Down left version %d, validated=%v, %d fresh columns", f.version(), validated(), f.columns(freshColumns...))
	}
	if _, err := f.pool.Exec(f.ctx, "UPDATE runs SET policy_fresh_consumed_generation=NULL WHERE id=$1", id); err == nil {
		t.Fatal("restored NOT VALID shape check accepted confirmed without consumed")
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
		t.Fatalf("416 Down after 417 Down: %v", err)
	}
	if f.version() != 415 || f.columns(freshColumns...) != 0 {
		t.Fatalf("chain left version %d and %d fresh columns", f.version(), f.columns(freshColumns...))
	}
}

// Down converts a paused or queued row still carrying a pending or unconfirmed one-claim Fresh
// authorization, leaves confirmed and retired history alone, keeps every migration 415 column and
// value, and the Down to 414 still works afterwards.
func TestPolicyFreshOneClaimDownConvertsActiveLiveDB(t *testing.T) {
	f := newDownFx(t)
	a := strings.Repeat("a", 40)
	pending := f.queuedFresh(a)
	pausedPending := f.queuedFresh(a)
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='paused',hold_reason='credential_disabled' WHERE id=$1", pausedPending)
	unconfirmed := f.consumedFresh(2, false)
	confirmed := f.consumedFresh(2, true)
	plainPreserving := f.queuedPreserving(a)
	ordinary := f.running()
	terminal := f.consumedFresh(2, false)
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", terminal)
	// A new hold after a Fresh retired its history (ApplyPolicyHold nulls both stamps and pending).
	retired := f.consumedFresh(2, false)
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='running',worker_id=$2 WHERE id=$1", retired, f.worker)
	f.hold(retired, 2, "same_conversation", a)

	converted := []uuid.UUID{pending, pausedPending, unconfirmed}
	kept := []uuid.UUID{confirmed, plainPreserving, ordinary, terminal, retired}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range append(append([]uuid.UUID{}, converted...), kept...) {
		before[id] = f.row(id)
	}
	f.toMf()
	if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, id := range converted {
		after := f.row(id)
		if after["status"] != "failed" || after["fail_origin"] != "agent_failure" || after["finished_at"] == nil ||
			after["failure_reason"] != "Policy fresh authorization ended by schema rollback" {
			t.Fatalf("run %s not terminalized: %v", id, after)
		}
		if got := changedKeys(before[id], after); !subset(got, convertedKeys) {
			t.Fatalf("conversion changed more than the terminal bookkeeping: %v", got)
		}
		for _, k := range []string{"session_id", "worker_id", "claim_generation", "policy_outcome_id", "policy_continuation_mode",
			"policy_preserve_required", "policy_resolved_at", "policy_pending_fresh", "hold_captured_head"} {
			if !reflect.DeepEqual(before[id][k], after[k]) {
				t.Fatalf("run %s lost %s: %v -> %v", id, k, before[id][k], after[k])
			}
		}
	}
	for _, id := range kept {
		if !reflect.DeepEqual(before[id], f.row(id)) {
			t.Fatalf("run %s changed: %v -> %v", id, before[id], f.row(id))
		}
	}
	if f.version() != 415 || f.columns(freshColumns...) != 0 || f.columns(recoveryColumns...) != 4 {
		t.Fatalf("Down left version %d, %d fresh columns, %d recovery columns", f.version(), f.columns(freshColumns...), f.columns(recoveryColumns...))
	}
	// Migration 415's own columns and values are untouched by this Down.
	var head string
	if err := f.pool.QueryRow(f.ctx, "SELECT policy_required_head FROM runs WHERE id=$1", confirmed).Scan(&head); err != nil || head != a {
		t.Fatalf("Down touched the 415 required head: %q %v", head, err)
	}
	// The Shape A chain continues: 415 -> 414 still works.
	if err := store.MigrateDownTo(f.ctx, f.dsn, 414); err != nil {
		t.Fatalf("Down 415 to 414: %v", err)
	}
	if f.version() != 414 || f.columns(recoveryColumns...) != 0 {
		t.Fatalf("chain left version %d and %d recovery columns", f.version(), f.columns(recoveryColumns...))
	}
	if f.columns(baseColumns...) != len(baseColumns) {
		t.Fatal("the 415 Down dropped a 00397 column")
	}
}

func TestPolicyFreshOneClaimDownRefusesLiveRowsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(f *downFx) uuid.UUID
	}{
		{"pending", func(f *downFx) uuid.UUID { return f.queuedFresh(strings.Repeat("a", 40)) }},
		{"consumed unconfirmed", func(f *downFx) uuid.UUID { return f.consumedFresh(2, false) }},
	} {
		for _, live := range []string{"claimed", "running", "awaiting_input"} {
			t.Run(tc.name+" "+live, func(t *testing.T) {
				f := newDownFx(t)
				convertible := f.queuedFresh(strings.Repeat("a", 40))
				active := tc.seed(f)
				mustExec(f.ctx, t, f.pool, "UPDATE runs SET status=$2 WHERE id=$1", active, live)
				f.mfRefused("a live run still carries a one-claim fresh authorization")
				if got := f.row(convertible)["status"]; got != "queued" {
					t.Fatalf("convertible row status = %v after the refusal", got)
				}
				// Once drained to queued, Down proceeds and converts both.
				mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='queued' WHERE id=$1", active)
				if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
					t.Fatal(err)
				}
				for _, id := range []uuid.UUID{convertible, active} {
					if f.row(id)["status"] != "failed" {
						t.Fatalf("run %s not converted", id)
					}
				}
				if v, n := f.version(), f.columns(freshColumns...); v != 415 || n != 0 {
					t.Fatalf("retried Down left version %d with %d fresh columns, want 415 and 0", v, n)
				}
			})
		}
	}
}

func TestPolicyFreshOneClaimDownRefusesOrphansLiveDB(t *testing.T) {
	const orphanMsg = "orphaned fresh authorization identity"
	dropShape := func(f *downFx) {
		mustExec(f.ctx, f.t, f.pool, "ALTER TABLE runs DROP CONSTRAINT "+freshShapeConstraint)
	}
	t.Run("confirmed without consumed", func(t *testing.T) {
		f := newDownFx(t)
		convertible := f.queuedFresh(strings.Repeat("a", 40))
		f.toMf()
		dropShape(f)
		orphan := f.running()
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='queued',policy_fresh_confirmed_generation=3 WHERE id=$1", orphan)
		f.mfRefused(orphanMsg)
		if f.row(convertible)["status"] != "queued" {
			t.Fatal("convertible row changed")
		}
	})
	t.Run("consumed without the preserving constraint", func(t *testing.T) {
		f := newDownFx(t)
		orphan := f.consumedFresh(2, true)
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET policy_preserve_required=false WHERE id=$1", orphan)
		f.mfRefused(orphanMsg)
	})
	t.Run("pending and consumed together", func(t *testing.T) {
		f := newDownFx(t)
		orphan := f.queuedFresh(strings.Repeat("a", 40))
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET policy_fresh_consumed_generation=2 WHERE id=$1", orphan)
		f.mfRefused(orphanMsg)
	})
	t.Run("terminal orphan is ignored", func(t *testing.T) {
		f := newDownFx(t)
		f.toMf()
		dropShape(f)
		done := f.running()
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='failed',finished_at=now(),policy_fresh_confirmed_generation=3 WHERE id=$1", done)
		// Re-add a constraint of that name (NOT VALID, so the orphan row is accepted) so Down finds what it drops.
		mustExec(f.ctx, t, f.pool, `ALTER TABLE runs ADD CONSTRAINT `+freshShapeConstraint+` CHECK (policy_fresh_confirmed_generation IS NULL OR policy_fresh_consumed_generation IS NOT NULL) NOT VALID`)
		if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
			t.Fatalf("terminal row blocked Down: %v", err)
		}
	})
}

// A compat-lane Fresh history, confirmed or not, carried through the compat to attempt re-hold and the
// fresh_from_saved_work Resume is retired: both stamps NULL, pending false, no orphan shape, and Down
// neither converts nor refuses the later ordinary runs.
func TestPolicyFreshOneClaimHistoryRetiredThenDownLiveDB(t *testing.T) {
	f := newDownFx(t)
	a := strings.Repeat("a", 40)
	var runs []uuid.UUID
	for _, confirmed := range []bool{false, true} {
		id := f.consumedFresh(2, confirmed)
		// The consuming claim's flight re-holds as an attempt-path episode (a real ApplyPolicyHold).
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='running',worker_id=$2 WHERE id=$1", id, f.worker)
		episode := f.hold(id, 2, "fresh_from_saved_work", a)
		var pending bool
		var consumed, confirmedGen *int64
		if err := f.pool.QueryRow(f.ctx, "SELECT policy_pending_fresh,policy_fresh_consumed_generation,policy_fresh_confirmed_generation FROM runs WHERE id=$1", id).
			Scan(&pending, &consumed, &confirmedGen); err != nil || pending || consumed != nil || confirmedGen != nil {
			t.Fatalf("re-hold must retire the history: %v %v %v %v", pending, consumed, confirmedGen, err)
		}
		f.resume(id, "fresh_from_saved_work", episode)
		f.claimAt(id, 3, "running")
		runs = append(runs, id)
	}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range runs {
		before[id] = f.row(id)
	}
	f.toMf()
	if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
		t.Fatalf("Down refused retired history: %v", err)
	}
	for _, id := range runs {
		if !reflect.DeepEqual(before[id], f.row(id)) {
			t.Fatalf("Down changed the retired-history run %s: %v -> %v", id, before[id], f.row(id))
		}
	}
	if f.version() != 415 || f.columns(freshColumns...) != 0 {
		t.Fatalf("Down left version %d and %d fresh columns", f.version(), f.columns(freshColumns...))
	}
}

// TestPolicyShapeAFullRollbackWalkLiveDB is the Shape-A rollback walk (issue #3607): from head 417 it
// goes down one version at a time (417, 416, 415), asserting each step converts only what that
// migration's Down owns, with rows produced by the real production queries.
func TestPolicyShapeAFullRollbackWalkLiveDB(t *testing.T) {
	// Up from H0: a database at 414 (the pre-feature head) migrates through 415 and 416 to 417.
	upCtx, upDSN := standIsolatedDB(t, "policy_shape_a_up_")
	for _, v := range []int64{414, 415, 416, 417} {
		if err := store.MigrateTo(upCtx, upDSN, v); err != nil {
			t.Fatalf("Up to %d: %v", v, err)
		}
		upPool, err := store.OpenPool(upCtx, upDSN)
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		err = upPool.QueryRow(upCtx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&got)
		upPool.Close()
		if err != nil || got != v {
			t.Fatalf("after Up to %d the version is %d (%v)", v, got, err)
		}
	}
	f := newDownFx(t) // a fresh isolated database migrated 0 -> 417
	if f.version() != 417 {
		t.Fatalf("head fixture at version %d, want 417", f.version())
	}
	a := strings.Repeat("a", 40)
	requiredHead := f.pausedPreserving(a)
	freshPending := f.queuedFresh(a)
	rows := []uuid.UUID{requiredHead, freshPending}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range rows {
		before[id] = f.row(id)
	}
	if before[requiredHead]["status"] != "paused" || before[freshPending]["status"] != "queued" {
		t.Fatalf("fixture statuses: %v %v", before[requiredHead]["status"], before[freshPending]["status"])
	}

	// 417 -> 416: validation is undone, nothing is converted.
	f.toMf()
	if f.version() != 416 || f.freshValidated() || f.columns(freshColumns...) != 2 {
		t.Fatalf("417 Down: version %d, validated=%v, %d fresh columns", f.version(), f.freshValidated(), f.columns(freshColumns...))
	}
	for _, id := range rows {
		if !reflect.DeepEqual(before[id], f.row(id)) {
			t.Fatalf("417 Down changed run %s", id)
		}
	}

	// 416 -> 415: the Fresh columns go and the pending Fresh row is converted; the Mc columns stay.
	if err := store.MigrateDownTo(f.ctx, f.dsn, 415); err != nil {
		t.Fatalf("416 Down: %v", err)
	}
	if f.version() != 415 || f.columns(freshColumns...) != 0 || f.columns(recoveryColumns...) != 4 {
		t.Fatalf("416 Down: version %d, %d fresh columns, %d recovery columns", f.version(), f.columns(freshColumns...), f.columns(recoveryColumns...))
	}
	if got := f.row(freshPending)["status"]; got != "failed" {
		t.Fatalf("pending Fresh run status = %v after the 416 Down, want failed", got)
	}
	if got := f.row(requiredHead)["status"]; got != "paused" {
		t.Fatalf("required-head run status = %v after the 416 Down, want paused", got)
	}

	// 415 -> 414: the Mc columns go and the required-head row is converted; 00397 columns stay.
	if err := store.MigrateDownTo(f.ctx, f.dsn, 414); err != nil {
		t.Fatalf("415 Down: %v", err)
	}
	if f.version() != 414 || f.columns(recoveryColumns...) != 0 || f.columns(baseColumns...) != len(baseColumns) {
		t.Fatalf("415 Down: version %d, %d recovery columns, %d 00397 columns", f.version(), f.columns(recoveryColumns...), f.columns(baseColumns...))
	}
	for _, id := range rows {
		if got := f.row(id)["status"]; got != "failed" {
			t.Fatalf("run %s status = %v after the full walk, want failed", id, got)
		}
	}
}
