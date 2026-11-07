package store_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// downFx is one isolated database migrated to 413 with real rows produced by the production queries.
type downFx struct {
	t                   *testing.T
	ctx                 context.Context
	dsn                 string
	pool                *pgxpool.Pool
	q                   *store.Queries
	owner, worker, repo uuid.UUID
	iid                 int64
}

var recoveryColumns = []string{"policy_required_head", "policy_recovery_verified_generation", "policy_recovery_verified_head", "policy_hold_cause"}

func newDownFx(t *testing.T) *downFx {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "policy_recovery_down_")
	if err := store.MigrateTo(ctx, dsn, 413); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &downFx{t: t, ctx: ctx, dsn: dsn, pool: pool, q: store.New(pool), owner: uuid.New(), worker: uuid.New(), iid: 3607}
	conn, repo := uuid.New(), uuid.New()
	f.repo = repo
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", f.owner, f.owner.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'policy fixture',$3)", f.worker, f.owner, []byte(f.worker.String()))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, f.owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/policy','https://example.com/policy','main')`, repo, conn)
	return f
}

// running seeds a Codex run running at claim generation 1 with a saved session.
func (f *downFx) running() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.iid++
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,harness,worker_id,claim_generation,session_id,started_at)
		VALUES($1,$2,$3,$4,'policy fixture','desc','running','codex',$5,1,'saved-session',now())`, id, f.owner, f.repo, f.iid, f.worker)
	return id
}

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// hold applies a real policy-hold report at claim generation gen through ApplyPolicyHold.
func (f *downFx) hold(id uuid.UUID, gen int64, mode, head string) string {
	f.t.Helper()
	outcome := uuid.New()
	cause := "refused"
	if mode == "fresh_from_saved_work" {
		cause = "attempt_path"
	}
	latest := head != ""
	_, err := f.q.ApplyPolicyHold(f.ctx, store.ApplyPolicyHoldParams{
		Head: txt(head), Published: head != "", CheckpointContainsLatest: pgtype.Bool{Bool: latest, Valid: head != ""},
		ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, Cause: cause, OutcomeID: pgtype.UUID{Bytes: outcome, Valid: true},
		ContinuationMode: txt(mode), WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true}, SourceGeneration: pgtype.Int8{Int64: gen, Valid: true},
		ActualRefusal: true, PolicyTag: txt("cyberPolicy"), Phase: txt("implementation"), CorrelationID: txt("pr-00000000000000000000000000000000-1"),
		ID: id, UserID: f.owner,
	})
	if err != nil {
		f.t.Fatalf("ApplyPolicyHold: %v", err)
	}
	return outcome.String()
}

func (f *downFx) resume(id uuid.UUID, mode, episode string) {
	f.t.Helper()
	if _, err := f.q.ResumePolicyHold(f.ctx, store.ResumePolicyHoldParams{
		Mode: mode, ID: id, UserID: f.owner, ExpectedEpisodeID: pgtype.UUID{Bytes: uuid.MustParse(episode), Valid: true},
	}); err != nil {
		f.t.Fatalf("ResumePolicyHold: %v", err)
	}
}

// claimAt moves a resumed (queued) run back under a live claim at generation gen with the given status.
func (f *downFx) claimAt(id uuid.UUID, gen int64, status string) {
	mustExec(f.ctx, f.t, f.pool, "UPDATE runs SET status=$2,claim_generation=$3,worker_id=$4,claim_released_at=NULL WHERE id=$1", id, status, gen, f.worker)
}

func (f *downFx) pausedPreserving(head string) uuid.UUID {
	id := f.running()
	f.hold(id, 1, "same_conversation", head)
	return id
}

func (f *downFx) queuedPreserving(head string) uuid.UUID {
	id := f.running()
	f.resume(id, "preserve", f.hold(id, 1, "same_conversation", head))
	return id
}

func (f *downFx) queuedSavedWork(head string) uuid.UUID {
	id := f.running()
	f.resume(id, "fresh_from_saved_work", f.hold(id, 1, "fresh_from_saved_work", head))
	return id
}

// row is the run as JSON without the four columns migration 413 adds.
func (f *downFx) row(id uuid.UUID) map[string]any {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT (to_jsonb(r) - 'policy_required_head' - 'policy_recovery_verified_generation'
		- 'policy_recovery_verified_head' - 'policy_hold_cause') FROM runs r WHERE id=$1`, id).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *downFx) snapshot() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM runs r),
		'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_db_version v))::text`).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *downFx) version() int64 {
	f.t.Helper()
	var v int64
	if err := f.pool.QueryRow(f.ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *downFx) columns(names ...string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='runs' AND column_name = ANY($1)`, names).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// refused asserts a Down to 412 fails with a message naming why, changing nothing at all.
func (f *downFx) refused(wantMsg string) {
	f.t.Helper()
	before := f.snapshot()
	err := store.MigrateDownTo(f.ctx, f.dsn, 412)
	if err == nil || !strings.Contains(err.Error(), wantMsg) {
		f.t.Fatalf("Down error = %v, want it to contain %q", err, wantMsg)
	}
	if f.snapshot() != before || f.version() != 413 || f.columns(recoveryColumns...) != 4 {
		f.t.Fatal("refused Down changed rows, history or schema")
	}
}

// convertedKeys is the exact set of run columns the terminal conversion may change.
var convertedKeys = []string{"status", "status_since", "finished_at", "updated_at", "fail_origin", "failure_reason", "move_pending_since",
	"codex_claim_epoch", "pause_requested_at", "pause_mode", "pause_after_count", "milestones_in_progress", "milestones_agents",
	"credential_switch_requested_at", "credential_switch_generation", "health", "health_reason", "health_since", "codex_cap_hash", "plan_cross_check_gate_reason"}

func changedKeys(before, after map[string]any) []string {
	var out []string
	for k, v := range before {
		if !reflect.DeepEqual(v, after[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func subset(got, allowed []string) bool {
	for _, k := range got {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return false
		}
	}
	return true
}

func TestPolicyRecoveryDownConvertsPausedAndQueuedLiveDB(t *testing.T) {
	f := newDownFx(t)
	a := strings.Repeat("a", 40)
	paused := f.pausedPreserving(a)
	queuedPreserve := f.queuedPreserving(a)
	savedWork := f.queuedSavedWork(strings.Repeat("b", 64))
	// A paused row that also carries a verification record, produced by the real verification query.
	verifiedPaused := f.queuedPreserving(a)
	f.claimAt(verifiedPaused, 2, "running")
	if _, err := f.q.RecordPolicyRecoveryVerified(f.ctx, store.RecordPolicyRecoveryVerifiedParams{
		ID: verifiedPaused, UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
		ClaimGeneration: pgtype.Int8{Int64: 2, Valid: true}, RequiredHead: txt(a),
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='paused',hold_reason='credential_disabled' WHERE id=$1", verifiedPaused)
	// Unrelated rows: an ordinary running run and a terminal run that still stores a head.
	ordinary := f.running()
	completed := f.running()
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='completed',finished_at=now(),policy_required_head=$2 WHERE id=$1", completed, a)
	converted := []uuid.UUID{paused, queuedPreserve, savedWork, verifiedPaused}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range append([]uuid.UUID{ordinary, completed}, converted...) {
		before[id] = f.row(id)
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 412); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, id := range converted {
		after := f.row(id)
		if after["status"] != "failed" || after["fail_origin"] != "agent_failure" || after["finished_at"] == nil ||
			after["failure_reason"] != "Policy recovery requirement ended by schema rollback" {
			t.Fatalf("run %s not terminalized: %v", id, after)
		}
		if got := changedKeys(before[id], after); !subset(got, convertedKeys) {
			t.Fatalf("conversion changed more than the terminal bookkeeping: %v", got)
		}
		// Session, custody, capture and source facts survive the conversion.
		for _, k := range []string{"session_id", "worker_id", "claim_generation", "claim_released_at", "policy_outcome_id", "policy_continuation_mode",
			"policy_preserve_required", "policy_resolved_at", "hold_captured_head", "checkpoint_tip", "preserved_patch"} {
			if !reflect.DeepEqual(before[id][k], after[k]) {
				t.Fatalf("run %s lost %s: %v -> %v", id, k, before[id][k], after[k])
			}
		}
		// fresh_from_saved_work cleared the session at Resume (before Down); the rest keep it.
		if wantSession := before[id]["session_id"]; id != savedWork && wantSession != "saved-session" {
			t.Fatalf("session lost: %v", wantSession)
		}
		if after["codex_claim_epoch"].(float64) != before[id]["codex_claim_epoch"].(float64)+1 {
			t.Fatal("codex claim epoch not advanced")
		}
	}
	for _, id := range []uuid.UUID{ordinary, completed} {
		if !reflect.DeepEqual(before[id], f.row(id)) {
			t.Fatalf("unrelated run %s changed", id)
		}
	}
	if f.version() != 412 || f.columns(recoveryColumns...) != 0 {
		t.Fatalf("Down left version %d and %d recovery columns", f.version(), f.columns(recoveryColumns...))
	}
	if f.columns("policy_preserve_required", "policy_pending_fresh", "policy_outcome_id", "policy_continuation_mode", "policy_resolved_at",
		"policy_applying_worker_id", "policy_applying_generation", "policy_source_generation", "policy_refusal_count", "policy_refusal_tag",
		"policy_refusal_phase", "policy_refusal_correlation_id", "policy_hold_published") != 13 {
		t.Fatal("Down touched the 00397 columns")
	}
	// Re-runnable: Up again restores the columns with the shape check.
	if err := store.MigrateTo(f.ctx, f.dsn, 413); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	if f.version() != 413 || f.columns(recoveryColumns...) != 4 {
		t.Fatal("Up after Down did not restore the schema")
	}
}

// A resolved attempt-path episode is history: its retained cause neither blocks nor converts.
func TestPolicyRecoveryDownIgnoresResolvedAttemptPathLiveDB(t *testing.T) {
	f := newDownFx(t)
	headless := func(status string) uuid.UUID {
		id := f.running()
		f.resume(id, "fresh_from_saved_work", f.hold(id, 1, "fresh_from_saved_work", ""))
		if status != "queued" {
			f.claimAt(id, 2, status)
		}
		return id
	}
	verified := func(status string) uuid.UUID {
		id := f.queuedSavedWork(strings.Repeat("c", 40))
		f.claimAt(id, 2, "running")
		if _, err := f.q.ClearPolicySavedWorkHead(f.ctx, store.ClearPolicySavedWorkHeadParams{
			ID: id, UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true}, ClaimGeneration: 2, RequiredHead: txt(strings.Repeat("c", 40)),
		}); err != nil {
			t.Fatal(err)
		}
		f.claimAt(id, 2, status)
		return id
	}
	rows := []uuid.UUID{headless("queued"), headless("claimed"), headless("running"), verified("claimed"), verified("running")}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range rows {
		before[id] = f.row(id)
		var cause string
		var head *string
		if err := f.pool.QueryRow(f.ctx, "SELECT policy_hold_cause,policy_required_head FROM runs WHERE id=$1", id).Scan(&cause, &head); err != nil || cause != "attempt_path" || head != nil {
			t.Fatalf("fixture row must retain attempt_path with no head: %q %v %v", cause, head, err)
		}
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 412); err != nil {
		t.Fatalf("resolved attempt-path rows blocked Down: %v", err)
	}
	for _, id := range rows {
		if !reflect.DeepEqual(before[id], f.row(id)) {
			t.Fatalf("attempt-path row %s changed", id)
		}
	}
}

func TestPolicyRecoveryDownRefusesLiveRowsWithoutPartialConversionLiveDB(t *testing.T) {
	for _, live := range []string{"claimed", "running"} {
		t.Run(live, func(t *testing.T) {
			f := newDownFx(t)
			a := strings.Repeat("a", 40)
			convertible := f.pausedPreserving(a)
			queued := f.queuedSavedWork(a)
			active := f.queuedPreserving(a)
			f.claimAt(active, 2, live)
			f.refused("a live run still carries a required recovery head or preserving constraint")
			for _, id := range []uuid.UUID{convertible, queued} {
				var status string
				if err := f.pool.QueryRow(f.ctx, "SELECT status FROM runs WHERE id=$1", id).Scan(&status); err != nil || status == "failed" {
					t.Fatalf("convertible row %s was converted despite the refusal: %q %v", id, status, err)
				}
			}
			// Once the live run is drained to a convertible state Down proceeds and converts all three.
			mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='queued' WHERE id=$1", active)
			if err := store.MigrateDownTo(f.ctx, f.dsn, 412); err != nil {
				t.Fatal(err)
			}
			for _, id := range []uuid.UUID{convertible, queued, active} {
				if f.row(id)["status"] != "failed" {
					t.Fatalf("run %s not converted", id)
				}
			}
		})
	}
}

// headlessPreserving is a hold reported with no head and resumed with preserve: queued, preserving,
// resolved, and carrying no required head, as the real queries produce it.
func (f *downFx) headlessPreserving() uuid.UUID {
	f.t.Helper()
	id := f.running()
	f.resume(id, "preserve", f.hold(id, 1, "same_conversation", ""))
	var preserve, resolved bool
	var head *string
	if err := f.pool.QueryRow(f.ctx, "SELECT policy_preserve_required,policy_resolved_at IS NOT NULL,policy_required_head FROM runs WHERE id=$1", id).Scan(&preserve, &resolved, &head); err != nil || !preserve || !resolved || head != nil {
		f.t.Fatalf("fixture must be preserving, resolved and headless: %v %v %v %v", preserve, resolved, head, err)
	}
	return id
}

func TestPolicyRecoveryDownConvertsHeadlessResolvedPreservingLiveDB(t *testing.T) {
	f := newDownFx(t)
	queued := f.headlessPreserving()
	paused := f.headlessPreserving()
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='paused',hold_reason='credential_disabled' WHERE id=$1", paused)
	if err := store.MigrateDownTo(f.ctx, f.dsn, 412); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, id := range []uuid.UUID{queued, paused} {
		if got := f.row(id)["status"]; got != "failed" {
			t.Fatalf("headless preserving run %s status = %v, want failed", id, got)
		}
	}
}

func TestPolicyRecoveryDownRefusesLiveHeadlessResolvedPreservingLiveDB(t *testing.T) {
	for _, live := range []string{"claimed", "running"} {
		t.Run(live, func(t *testing.T) {
			f := newDownFx(t)
			convertible := f.pausedPreserving(strings.Repeat("a", 40))
			active := f.headlessPreserving()
			f.claimAt(active, 2, live)
			f.refused("a live run still carries a required recovery head or preserving constraint")
			if got := f.row(convertible)["status"]; got != "paused" {
				t.Fatalf("convertible row status = %v after refusal", got)
			}
		})
	}
}

func TestPolicyRecoveryDownRefusesOrphansLiveDB(t *testing.T) {
	t.Run("verification without a required head", func(t *testing.T) {
		f := newDownFx(t)
		convertible := f.pausedPreserving(strings.Repeat("a", 40))
		orphan := f.running()
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='queued',policy_recovery_verified_generation=3,policy_recovery_verified_head=$2 WHERE id=$1", orphan, strings.Repeat("d", 40))
		f.refused("orphaned or malformed recovery identity")
		if f.row(convertible)["status"] != "paused" {
			t.Fatal("convertible row changed")
		}
	})
	t.Run("malformed head that predates the shape check", func(t *testing.T) {
		f := newDownFx(t)
		mustExec(f.ctx, t, f.pool, "ALTER TABLE runs DROP CONSTRAINT runs_policy_required_recovery_shape")
		bad := f.running()
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='queued',policy_required_head='NOT-A-SHA' WHERE id=$1", bad)
		f.refused("orphaned or malformed recovery identity")
	})
	t.Run("terminal orphan is ignored", func(t *testing.T) {
		f := newDownFx(t)
		done := f.running()
		mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='failed',finished_at=now(),policy_recovery_verified_generation=3 WHERE id=$1", done)
		if err := store.MigrateDownTo(f.ctx, f.dsn, 412); err != nil {
			t.Fatalf("terminal row blocked Down: %v", err)
		}
	})
}

// waitForLock polls pg_locks until n ungranted requests of mode on runs exist.
func (f *downFx) waitForLock(mode string, n int) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND mode=$1 AND relation='runs'::regclass`, mode).Scan(&got); err != nil {
			f.t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("no %d ungranted %s requests on runs", n, mode)
}

// (i) A writer that already holds a row lock when Down starts: Down waits on its table lock, the writer
// commits a live status, and Down then refuses with nothing converted.
func TestPolicyRecoveryDownWaitsForInFlightWriterLiveDB(t *testing.T) {
	f := newDownFx(t)
	a := strings.Repeat("a", 40)
	other := f.pausedPreserving(a)
	target := f.queuedPreserving(a)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	var locked uuid.UUID
	if err := tx.QueryRow(f.ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", target).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.MigrateDownTo(f.ctx, f.dsn, 412) }()
	f.waitForLock("AccessExclusiveLock", 1)
	select {
	case err := <-done:
		t.Fatalf("Down finished while a writer held a row lock: %v", err)
	default:
	}
	if _, err := tx.Exec(f.ctx, "UPDATE runs SET status='claimed',claim_generation=2 WHERE id=$1", target); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "a live run still carries a required recovery head or preserving constraint") {
		t.Fatalf("Down after the writer committed a live status = %v", err)
	}
	if f.row(other)["status"] != "paused" || f.row(target)["status"] != "claimed" || f.version() != 413 || f.columns(recoveryColumns...) != 4 {
		t.Fatal("refused Down changed rows or schema")
	}
}

// downSQL returns the Down section of migration 413 so a test can run it inside a transaction it controls.
func downSQL(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("migrations/00413_policy_required_recovery_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration has no Down section")
	}
	down = strings.ReplaceAll(down, "-- +goose StatementBegin", "")
	return strings.ReplaceAll(down, "-- +goose StatementEnd", "")
}

// (ii) Down's lock is held while its transaction is open: concurrent writers wait, and after the commit
// they either touch no row (the converted row left their predicate) or fail cleanly on the dropped column.
func TestPolicyRecoveryDownBlocksConcurrentWritersLiveDB(t *testing.T) {
	f := newDownFx(t)
	target := f.queuedPreserving(strings.Repeat("a", 40))
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err := tx.Exec(f.ctx, downSQL(t)); err != nil {
		t.Fatalf("Down SQL: %v", err)
	}
	var wg sync.WaitGroup
	var tag pgtypeResult
	var dropped error
	wg.Add(2)
	go func() {
		defer wg.Done()
		c, err := f.pool.Exec(f.ctx, "UPDATE runs SET status='claimed' WHERE id=$1 AND status IN ('paused','queued')", target)
		tag = pgtypeResult{n: c.RowsAffected(), err: err}
	}()
	go func() {
		defer wg.Done()
		_, dropped = f.pool.Exec(f.ctx, "UPDATE runs SET policy_required_head=NULL WHERE id=$1", target)
	}()
	f.waitForLock("RowExclusiveLock", 2)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if tag.err != nil || tag.n != 0 {
		t.Fatalf("status writer after Down: rows=%d err=%v, want 0 rows and no error", tag.n, tag.err)
	}
	if dropped == nil || !strings.Contains(dropped.Error(), "policy_required_head") {
		t.Fatalf("writer on a dropped column: %v, want a clean column error", dropped)
	}
	if got := f.row(target)["status"]; got != "failed" {
		t.Fatalf("target status = %v, want failed", got)
	}
}

type pgtypeResult struct {
	n   int64
	err error
}
