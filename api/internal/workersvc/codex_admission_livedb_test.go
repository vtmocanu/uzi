package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func admissionServiceFixture(t *testing.T, env codexTestEnv) codexRunFixture {
	t.Helper()
	f := newSubscriptionFixture(t, env)
	env.exec("UPDATE runs SET claim_generation=1 WHERE id=$1", f.runID)
	f.svc.SetTxBeginner(env.pool)
	return f
}
func admissionSignal(class string) CodexAdmissionSignal {
	return CodexAdmissionSignal{ClaimGeneration: 1, AttemptID: uuid.NewString(), Class: class}
}
func admissionRowCount(t *testing.T, env codexTestEnv, owner uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM codex_account_admission_throttle WHERE user_id=$1", owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCodexAdmissionAuthorityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct {
		name   string
		change func(codexRunFixture)
	}{
		{"foreign worker", func(f codexRunFixture) { f.wkr.ID = uuid.New() }},
		{"released", func(f codexRunFixture) { env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID) }},
		{"generation", func(f codexRunFixture) { env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", f.runID) }},
		{"zero generation", func(f codexRunFixture) { env.exec("UPDATE runs SET claim_generation=0 WHERE id=$1", f.runID) }},
		{"Claude harness", func(f codexRunFixture) {
			// Keep the binding coherent under runs_codex_harness_coherence_check.
			env.exec(`UPDATE runs SET harness='claude', codex_secret_id=NULL, codex_auth_mode=NULL,
				codex_secret_label=NULL, codex_account_key=NULL, codex_material_revision=NULL,
				codex_account_revision=NULL WHERE id=$1`, f.runID)
		}},
		{"wrong credential kind", func(f codexRunFixture) {
			env.exec("UPDATE user_secrets SET kind='openai_api_key' WHERE id=$1", f.aliasID)
		}},
		{"disabled", func(f codexRunFixture) { env.exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", f.aliasID) }},
		{"material revision", func(f codexRunFixture) {
			env.exec("UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", f.aliasID)
		}},
		{"account revision", func(f codexRunFixture) {
			env.exec("UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE user_id=$1", f.userID)
		}},
		{"identity", func(f codexRunFixture) {
			env.exec("UPDATE codex_provider_account SET provider_user_id='replacement' WHERE user_id=$1", f.userID)
		}},
		{"unlinked", func(f codexRunFixture) {
			env.exec("UPDATE codex_credential_state SET status='failed' WHERE user_secret_id=$1", f.aliasID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			tc.change(f)
			worker := f.wkr
			if tc.name == "foreign worker" {
				worker.ID = uuid.New()
			}
			err := f.svc.RecordCodexAdmissionSignal(env.ctx, worker, f.runID, admissionSignal("rate_limit"))
			if !errors.Is(err, ErrCodexAdmissionRefused) {
				t.Fatalf("refusal = %v", err)
			}
			if admissionRowCount(t, env, f.userID) != 0 {
				t.Fatal("refused evidence persisted")
			}
		})
	}
	t.Run("API-key mode", func(t *testing.T) {
		f := newAPIKeyFixture(t, env)
		env.exec("UPDATE runs SET claim_generation=1 WHERE id=$1", f.runID)
		f.svc.SetTxBeginner(env.pool)
		err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
		if !errors.Is(err, ErrCodexAdmissionRefused) {
			t.Fatalf("refusal = %v", err)
		}
		if admissionRowCount(t, env, f.userID) != 0 {
			t.Fatal("refused API-key evidence persisted")
		}
	})
	for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "completed", "failed", "cancelled", "paused", "limit_wait", "recovery_wait", "pool_wait"} {
		t.Run(status, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			env.exec("UPDATE runs SET status=$2 WHERE id=$1", f.runID, status)
			err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
			accepted := status == "claimed" || status == "running" || status == "awaiting_approval"
			if accepted {
				if err != nil || admissionRowCount(t, env, f.userID) != 1 {
					t.Fatalf("valid status refused: %v", err)
				}
			} else if !errors.Is(err, ErrCodexAdmissionRefused) || admissionRowCount(t, env, f.userID) != 0 {
				t.Fatalf("inactive status admitted: %v", err)
			}
		})
	}
	f := admissionServiceFixture(t, env)
	foreign := f.wkr
	foreign.UserID = uuid.New()
	if err := f.svc.RecordCodexAdmissionSignal(env.ctx, foreign, f.runID, admissionSignal("rate_limit")); !errors.Is(err, ErrCodexAdmissionRefused) {
		t.Fatalf("foreign owner = %v", err)
	}
}

func TestCodexAdmissionConcurrentUpsertLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	// Two first inserts and then two updates of existing state. Distinct receipts
	// must all survive; no stale EXCLUDED state may replace a concurrent transition.
	for round := 0; round < 2; round++ {
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var receipts int
		if err := env.pool.QueryRow(env.ctx, `SELECT jsonb_array_length(receipts) FROM codex_account_admission_throttle WHERE user_id=$1`, f.userID).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if receipts != (round+1)*2 {
			t.Fatalf("lost concurrent evidence: receipts=%d", receipts)
		}
	}
}

func TestCodexAdmissionForeignKeyTimeoutLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(env.ctx, "SELECT id FROM codex_provider_account WHERE user_id=$1 FOR UPDATE", f.userID); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	err = f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
	if err == nil || errors.Is(err, ErrCodexAdmissionRefused) {
		t.Fatalf("held account FK did not time out: %v", err)
	}
	if time.Since(before) > 2500*time.Millisecond {
		t.Fatal("FK wait exceeded request budget")
	}
	if admissionRowCount(t, env, f.userID) != 0 {
		t.Fatal("timed-out insert persisted")
	}
	run := mustRun(t, env, f.runID)
	if run.Status != "claimed" || run.ClaimReleasedAt.Valid || run.ClaimGeneration != 1 {
		t.Fatal("FK timeout changed lifecycle")
	}
}

// Wrapping the real transaction exposes an event at the exact lock/INSERT seam,
// without wall-clock sleeps or a production test hook.
type admissionBarrierBeginner struct {
	TxBeginner
	locked           chan struct{}
	proceed          chan struct{}
	afterObservation bool
}
type admissionBarrierTx struct {
	pgx.Tx
	barrier *admissionBarrierBeginner
	once    sync.Once
}

func (b *admissionBarrierBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	return &admissionBarrierTx{Tx: tx, barrier: b}, err
}
func (tx *admissionBarrierTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if tx.barrier.afterObservation && !strings.Contains(sql, "-- name: ObserveCodexAdmission") {
		return row
	}
	// The lock query's Scan must finish before signalling that the run is locked.
	return &admissionBarrierRow{Row: row, tx: tx}
}

type admissionBarrierRow struct {
	pgx.Row
	tx *admissionBarrierTx
}

func (r *admissionBarrierRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.tx.once.Do(func() {
		close(r.tx.barrier.locked)
		select {
		case <-r.tx.barrier.proceed:
		case <-time.After(2 * time.Second):
		}
	})
	return err
}
func newAdmissionBarrier(env codexTestEnv) *admissionBarrierBeginner {
	return &admissionBarrierBeginner{TxBeginner: env.pool, locked: make(chan struct{}), proceed: make(chan struct{})}
}

func TestCodexAdmissionReplacementSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	b := newAdmissionBarrier(env)
	f.svc.SetTxBeginner(b)
	result := make(chan error, 1)
	go func() {
		result <- f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
	}()
	select {
	case <-b.locked:
	case <-time.After(2 * time.Second):
		t.Fatal("run not locked")
	}
	// Account and alias writes remain unlocked while the run SHARE lock is held.
	env.exec("UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", f.aliasID)
	close(b.proceed)
	if err := <-result; !errors.Is(err, ErrCodexAdmissionRefused) {
		t.Fatalf("replacement before INSERT snapshot accepted: %v", err)
	}
	if admissionRowCount(t, env, f.userID) != 0 {
		t.Fatal("replacement evidence persisted")
	}
}

// Replacement after the observation statement's snapshot may coexist with the
// accepted signal. The run lock orders lifecycle changes, not alias revisions.
func TestCodexAdmissionReplacementAfterObservationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	b := newAdmissionBarrier(env)
	b.afterObservation = true
	f.svc.SetTxBeginner(b)
	result := make(chan error, 1)
	go func() {
		result <- f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
	}()
	select {
	case <-b.locked:
	case <-time.After(2 * time.Second):
		t.Fatal("observation did not reach its commit boundary")
	}
	env.exec("UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", f.aliasID)
	close(b.proceed)
	if err := <-result; err != nil {
		t.Fatalf("replacement after observation rejected accepted evidence: %v", err)
	}
	if admissionRowCount(t, env, f.userID) != 1 {
		t.Fatal("accepted snapshot evidence did not persist")
	}
	run := mustRun(t, env, f.runID)
	if run.Status != "claimed" || run.ClaimGeneration != 1 || run.ClaimReleasedAt.Valid {
		t.Fatal("snapshot interleaving changed lifecycle")
	}
}

func TestCodexAdmissionParkSerializationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	b := newAdmissionBarrier(env)
	f.svc.SetTxBeginner(b)
	result := make(chan error, 1)
	go func() {
		result <- f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit"))
	}()
	select {
	case <-b.locked:
	case <-time.After(2 * time.Second):
		t.Fatal("run not locked")
	}
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(env.ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(env.ctx, "UPDATE runs SET status='recovery_wait',claim_released_at=now() WHERE id=$1", f.runID)
	if err == nil {
		t.Fatal("park bypassed run SHARE lock")
	}
	if err = tx.Rollback(env.ctx); err != nil {
		t.Fatal(err)
	}
	close(b.proceed)
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE runs SET status='recovery_wait',claim_released_at=now() WHERE id=$1", f.runID)
	var before string
	if err = env.pool.QueryRow(env.ctx, "SELECT receipts::text FROM codex_account_admission_throttle WHERE user_id=$1", f.userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.svc.SetTxBeginner(env.pool)
	if err = f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); !errors.Is(err, ErrCodexAdmissionRefused) {
		t.Fatalf("post-park signal: %v", err)
	}
	var after string
	if err = env.pool.QueryRow(env.ctx, "SELECT receipts::text FROM codex_account_admission_throttle WHERE user_id=$1", f.userID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("post-park observation changed throttle")
	}
}
