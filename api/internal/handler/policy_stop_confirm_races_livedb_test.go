package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Source context comes from the seeded fixture; these tests prove API transaction
// ordering and rollback, not source pinning, worker delivery or finalization.
type policyStopRaceBeginner struct {
	pool     *pgxpool.Pool
	attempt  chan struct{}
	acquired chan struct{}
	release  <-chan struct{}
}

func (b policyStopRaceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}

func (b policyStopRaceBeginner) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &policyStopRaceTx{Tx: tx, observer: b}, nil
}

type policyStopRaceTx struct {
	pgx.Tx
	observer policyStopRaceBeginner
}

func (tx *policyStopRaceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !strings.HasPrefix(sql, "-- name: GetRunByIDForUpdate :one") {
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
	// This notification precedes the real lock statement, including when it blocks.
	if tx.observer.attempt != nil {
		close(tx.observer.attempt)
	}
	return policyStopRaceRow{
		Row: tx.Tx.QueryRow(ctx, sql, args...), ctx: ctx, observer: tx.observer,
	}
}

type policyStopRaceRow struct {
	pgx.Row
	ctx      context.Context
	observer policyStopRaceBeginner
}

func (r policyStopRaceRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	close(r.observer.acquired)
	select {
	case <-r.observer.release:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

type policyStopDelayedReviseStore struct {
	*wallParkRaceStore
	attempt chan struct{}
	release <-chan struct{}
}

func (s *policyStopDelayedReviseStore) CreateRunReviseInputIfUnderCap(ctx context.Context, arg store.CreateRunReviseInputIfUnderCapParams) (store.RunUserInput, error) {
	// SubmitInput has already read the pre-Stop run and passed its early guard.
	close(s.attempt)
	select {
	case <-s.release:
		return s.wallParkRaceStore.CreateRunReviseInputIfUnderCap(ctx, arg)
	case <-ctx.Done():
		return store.RunUserInput{}, ctx.Err()
	}
}

func policyStopRacePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func policyStopRaceService(f *policyStopConfirmEnv, q workersvc.Store, beginner workersvc.TxBeginner) {
	f.h.wsvc = workersvc.New(q, nil, workersvc.Params{
		PolicyStopNegotiated: true, PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true,
		TerminalPendingLease: time.Hour, RunTimeout: 16 * time.Hour, PlanMaxRevisions: 10,
	})
	f.h.wsvc.SetTxBeginner(beginner)
}

func policyStopRaceWait(t *testing.T, ctx context.Context, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", label, ctx.Err())
	}
}

// Each request runs once, with a ten-second deadline; cleanup cancels and joins
// its goroutine even if an assertion fails while a transaction observer is paused.
func policyStopRacePOST(t *testing.T, f *policyStopConfirmEnv, body, token string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	req := inputReq(f.owner, f.id, body)
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	req = req.WithContext(ctx)
	if token != "" {
		req.Header.Set("If-Match", token)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(11 * time.Second):
			t.Error("request goroutine did not exit after cancellation")
		}
	})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		f.h.CreateRunInput(rec, req)
		result <- rec
	}()
	return result
}

func policyStopRaceResponse(t *testing.T, ctx context.Context, result <-chan *httptest.ResponseRecorder, status int, reason string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-result:
		if rec.Code != status {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, status, rec.Body)
		}
		if reason != "" {
			var wire struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Reason != reason {
				t.Fatalf("reason=%q want=%q body=%s", wire.Reason, reason, rec.Body)
			}
		}
		return rec
	case <-ctx.Done():
		t.Fatalf("waiting for HTTP response: %v", ctx.Err())
		return nil
	}
}

func TestPolicyStopConcurrentFeedbackLiveDB(t *testing.T) {
	t.Run("new_feedback_first", func(t *testing.T) {
		f := newPolicyStopConfirmEnv(t, 1, "revise")
		token := f.review().StopIfMatch
		pool := policyStopRacePool(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		writer, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.Rollback(context.Background()) }()
		var locked uuid.UUID
		if err := writer.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", f.id).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		// Explicitly model applicable bound feedback, not a paused HTTP producer.
		// The insert and counter bump commit together under the parent row lock.
		var feedbackID int64
		if err := writer.QueryRow(ctx, `INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
 VALUES($1,'revise_plan','new bound feedback','bound',3) RETURNING id`, f.id).Scan(&feedbackID); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(ctx, "UPDATE runs SET revise_count=revise_count+1 WHERE id=$1", f.id); err != nil {
			t.Fatal(err)
		}
		attempt, acquired, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		policyStopRaceService(f, f.race, policyStopRaceBeginner{pool, attempt, acquired, release})
		result := policyStopRacePOST(t, f, `{"kind":"stop","body":"keep completed work"}`, token)
		policyStopRaceWait(t, ctx, attempt, "Stop lock attempt while writer holds run")
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		policyStopRaceWait(t, ctx, acquired, "Stop lock acquired after writer commit")
		before := f.snapshot()
		close(release)
		policyStopRaceResponse(t, ctx, result, http.StatusConflict, "policy_stop_token_stale")
		if after := f.snapshot(); after != before {
			t.Fatalf("stale Stop mutated committed feedback/run/audits: before=%s after=%s", before, after)
		}
		var unsettled bool
		if err := pool.QueryRow(ctx, `SELECT disposition IS NULL AND settled_at IS NULL
 AND superseded_by_stop_input_id IS NULL AND body='new bound feedback'
 FROM run_user_inputs WHERE id=$1`, feedbackID).Scan(&unsettled); err != nil {
			t.Fatal(err)
		}
		if !unsettled {
			t.Fatal("new feedback was settled or changed by stale Stop")
		}
	})

	t.Run("Stop_first", func(t *testing.T) {
		f := newPolicyStopConfirmEnv(t, 1, "revise")
		token := f.review().StopIfMatch
		pool := policyStopRacePool(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		acquired, releaseStop := make(chan struct{}), make(chan struct{})
		reviseAttempt, releaseRevise := make(chan struct{}), make(chan struct{})
		q := &policyStopDelayedReviseStore{f.race, reviseAttempt, releaseRevise}
		policyStopRaceService(f, q, policyStopRaceBeginner{pool: pool, acquired: acquired, release: releaseStop})
		stopResult := policyStopRacePOST(t, f, `{"kind":"stop","body":"keep completed work"}`, token)
		policyStopRaceWait(t, ctx, acquired, "Stop owns run lock")
		// Omit expected_gate_revision: this must reach the capped SQL write.
		reviseResult := policyStopRacePOST(t, f, `{"kind":"revise_plan","body":"late revise"}`, "")
		policyStopRaceWait(t, ctx, reviseAttempt, "revise passed early service guard")
		close(releaseStop)
		policyStopRaceResponse(t, ctx, stopResult, http.StatusAccepted, "")
		before := f.snapshot()
		// Execute the delayed SQL only after the actual Stop ACK and baseline.
		close(releaseRevise)
		policyStopRaceResponse(t, ctx, reviseResult, http.StatusConflict, "policy_stop_gate_resolved")
		if after := f.snapshot(); after != before {
			t.Fatalf("delayed revise changed accepted Stop: before=%s after=%s", before, after)
		}
		// Also exercise the scope SQL backstop directly for a delayed SQL caller.
		_, err := store.New(pool).CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
			RunID: f.id, ScopeCeiling: pgtype.Int4{Int32: 2, Valid: true},
			Body: pgtype.Text{String: "late scope", Valid: true},
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("scope SQL after Stop: got %v, want ErrNoRows", err)
		}
		if after := f.snapshot(); after != before {
			t.Fatalf("delayed scope changed accepted Stop: before=%s after=%s", before, after)
		}
	})
}

func TestPolicyStopRollbackLiveDB(t *testing.T) {
	f := newPolicyStopConfirmEnv(t, 1, "revise")
	token := f.review().StopIfMatch
	pool := policyStopRacePool(t)
	policyStopRaceService(f, f.race, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	function := pgx.Identifier{"stop_fault_fn_" + suffix}.Sanitize()
	trigger := pgx.Identifier{"stop_fault_tr_" + suffix}.Sanitize()
	sequence := pgx.Identifier{"stop_fault_seq_" + suffix}.Sanitize()
	// Cleanup is registered after pool.Close, so all owned DDL is dropped first.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		// Three independent drops, each attempted once under the shared deadline;
		// one failed drop does not suppress cleanup of the other owned objects.
		for _, sql := range []string{
			"DROP TRIGGER IF EXISTS " + trigger + " ON cross_checks",
			"DROP FUNCTION IF EXISTS " + function + "()",
			"DROP SEQUENCE IF EXISTS " + sequence,
		} {
			if _, err := pool.Exec(cleanupCtx, sql); err != nil {
				t.Errorf("drop owned Stop fault object: %v", err)
			}
		}
	})
	for _, sql := range []string{
		"CREATE SEQUENCE " + sequence,
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $ownedstop$
 BEGIN
 IF NEW.id = '%s'::uuid AND NEW.revision_disposition = 'superseded_by_stop' THEN
   IF NOT EXISTS (SELECT 1 FROM run_user_inputs WHERE id=%d AND run_id='%s'::uuid
     AND disposition='superseded_by_stop' AND settled_at IS NOT NULL
     AND superseded_by_stop_input_id=NEW.revision_superseded_by_stop_input_id
     AND applied_at IS NULL AND body=E'  exact human body\n\n'
     AND consumed_claim_generation=1 AND consumed_worker_id='%s'::uuid) THEN
     RAISE EXCEPTION 'primary human not settled before cross-check';
   END IF;
   PERFORM nextval('%s'::regclass);
   RAISE EXCEPTION 'ownedstopfault';
 END IF;
 RETURN NEW;
 END; $ownedstop$`, function, f.ccID, f.humanID, f.id, f.worker, sequence),
		fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE ON cross_checks FOR EACH ROW EXECUTE FUNCTION %s()", trigger, function),
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	before := f.snapshot()
	result := policyStopRacePOST(t, f, `{"kind":"stop","body":"keep completed work"}`, token)
	policyStopRaceResponse(t, ctx, result, http.StatusInternalServerError, "")
	var reached bool
	if err := pool.QueryRow(ctx, "SELECT is_called FROM "+sequence).Scan(&reached); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("Stop did not reach owned cross-check fault after primary human settlement")
	}
	if after := f.snapshot(); after != before {
		t.Fatalf("Stop fault did not roll back whole run/inputs/checks/audits: before=%s after=%s", before, after)
	}
}
