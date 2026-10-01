package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// inputPublishCounter records PublishInput calls; the other Broadcaster methods are no-ops.
type inputPublishCounter struct {
	mu sync.Mutex
	n  int
}

func (*inputPublishCounter) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (*inputPublishCounter) PublishState(uuid.UUID, string)                {}
func (*inputPublishCounter) PublishHealth(uuid.UUID, string, string, bool) {}

func (c *inputPublishCounter) PublishInput(uuid.UUID) { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *inputPublishCounter) count() int             { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// TestInputInclusionLiveDB pins the included_at receipt: it is fenced on the caller's claim only
// (current worker AND generation, whatever the run status), stamps follow_up rows once, needs the
// input_inclusion_v1 capability, and ACK records whether the ACKing worker reports inclusion.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestInputInclusionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := New(q, newBox(t), testParams())
	svc.SetTxBeginner(pool)
	pub := &inputPublishCounter{}
	svc.SetBroadcaster(pub)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID := uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("incl-%s@e2e", userID))
	newWorker := func(caps ...string) store.Worker {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'offline')`, id, userID, "w-"+id.String(), id[:])
		if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: id, ProtocolCapabilities: caps}); err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
		w, err := q.GetWorkerByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	capable := newWorker(capability.InputReceiptsV1, capability.InputInclusionV1)
	legacy := newWorker(capability.InputReceiptsV1)
	other := newWorker(capability.InputReceiptsV1, capability.InputInclusionV1)
	newRun := func(w store.Worker, generation int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,$4)`, id, userID, w.ID, generation)
		return id
	}
	addInput := func(run uuid.UUID, kind string, consumed bool, gen int64, w store.Worker) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id)
			VALUES ($1,$2,'x',CASE WHEN $3 THEN now() END,CASE WHEN $3 THEN $4::bigint END,CASE WHEN $3 THEN $5::uuid END) RETURNING id`,
			run, kind, consumed, gen, w.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	included := func(id int64) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT included_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Run("stamps on the current claim, idempotent, one publish", func(t *testing.T) {
		run := newRun(capable, 1)
		id := addInput(run, "follow_up", true, 1, capable)
		before := pub.count()
		res, err := svc.IncludeInputs(ctx, capable, run, 1, []int64{id})
		if err != nil || !res.Active || res.Reason != "" {
			t.Fatalf("IncludeInputs = %+v, %v", res, err)
		}
		if !included(id) || pub.count() != before+1 {
			t.Fatalf("included=%v publishes=%d want stamp and one publish", included(id), pub.count()-before)
		}
		if res, err = svc.IncludeInputs(ctx, capable, run, 1, []int64{id}); err != nil || !res.Active {
			t.Fatalf("repeat = %+v, %v", res, err)
		}
		if pub.count() != before+1 {
			t.Fatalf("repeat published again (%d)", pub.count()-before)
		}
	})

	t.Run("lands after the run went terminal", func(t *testing.T) {
		run := newRun(capable, 1)
		id := addInput(run, "follow_up", true, 1, capable)
		exec(`UPDATE runs SET status='completed' WHERE id=$1`, run)
		res, err := svc.IncludeInputs(ctx, capable, run, 1, []int64{id})
		if err != nil || !res.Active || !included(id) {
			t.Fatalf("terminal run: res=%+v err=%v included=%v", res, err, included(id))
		}
	})

	t.Run("lands while a credential switch is pending", func(t *testing.T) {
		run := newRun(capable, 1)
		id := addInput(run, "follow_up", true, 1, capable)
		exec(`UPDATE runs SET credential_switch_requested_at=now(), credential_switch_generation=1 WHERE id=$1`, run)
		if res, err := svc.IncludeInputs(ctx, capable, run, 1, []int64{id}); err != nil || !res.Active || !included(id) {
			t.Fatalf("switch_pending: res=%+v err=%v included=%v", res, err, included(id))
		}
	})

	t.Run("refused for another worker or generation", func(t *testing.T) {
		run := newRun(capable, 2)
		id := addInput(run, "follow_up", true, 2, capable)
		before := pub.count()
		for name, c := range map[string]struct {
			w   store.Worker
			gen int64
		}{"other worker": {other, 2}, "old generation": {capable, 1}, "future generation": {capable, 3}} {
			res, err := svc.IncludeInputs(ctx, c.w, run, c.gen, []int64{id})
			if err != nil || res.Active || res.Reason != ReceiptStale {
				t.Fatalf("%s: res=%+v err=%v, want inactive stale", name, res, err)
			}
			if included(id) {
				t.Fatalf("%s: stamped despite the fence", name)
			}
		}
		if pub.count() != before {
			t.Fatal("a refused receipt published")
		}
	})

	t.Run("row consumed by an earlier claim is stamped by the current one", func(t *testing.T) {
		run := newRun(capable, 2)
		id := addInput(run, "follow_up", true, 1, other)
		if res, err := svc.IncludeInputs(ctx, capable, run, 2, []int64{id}); err != nil || !res.Active || !included(id) {
			t.Fatalf("recovered row: res=%+v err=%v included=%v", res, err, included(id))
		}
	})

	t.Run("ignores non-follow_up and unconsumed rows", func(t *testing.T) {
		run := newRun(capable, 1)
		answer := addInput(run, "answer", true, 1, capable)
		cancel := addInput(run, "cancel", true, 1, capable)
		pending := addInput(run, "follow_up", false, 0, capable)
		before := pub.count()
		if res, err := svc.IncludeInputs(ctx, capable, run, 1, []int64{answer, cancel, pending}); err != nil || !res.Active {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if included(answer) || included(cancel) || included(pending) || pub.count() != before {
			t.Fatal("a non-follow_up or unconsumed row was stamped or published")
		}
	})

	t.Run("requires the capability and valid ids", func(t *testing.T) {
		run := newRun(legacy, 1)
		id := addInput(run, "follow_up", true, 1, legacy)
		if _, err := svc.IncludeInputs(ctx, legacy, run, 1, []int64{id}); !errors.Is(err, ErrInputReceiptInvalid) {
			t.Fatalf("incapable worker err = %v, want ErrInputReceiptInvalid", err)
		}
		if included(id) {
			t.Fatal("incapable worker stamped a row")
		}
		for _, ids := range [][]int64{nil, {id, id}, {0}} {
			if _, err := svc.IncludeInputs(ctx, capable, run, 1, ids); !errors.Is(err, ErrInputReceiptInvalid) {
				t.Fatalf("ids %v err = %v, want ErrInputReceiptInvalid", ids, err)
			}
		}
	})

	t.Run("ACK records inclusion_reported per worker capability and GET follow-ups returns both fields", func(t *testing.T) {
		for _, c := range []struct {
			w    store.Worker
			want bool
		}{{capable, true}, {legacy, false}} {
			run := newRun(c.w, 1)
			id := addInput(run, "follow_up", false, 0, c.w)
			if _, err := svc.AckInputs(ctx, c.w, run, 1, []int64{id}); err != nil {
				t.Fatal(err)
			}
			got := func() []InputDTO {
				out, err := svc.ConsumedFollowUps(ctx, c.w, run)
				if err != nil || len(out) != 1 {
					t.Fatalf("ConsumedFollowUps = %v, %v", out, err)
				}
				return out
			}
			dto := got()[0]
			if dto.InclusionReported == nil || *dto.InclusionReported != c.want || dto.IncludedAt != nil {
				t.Fatalf("after ACK: reported=%v included=%v, want reported=%v and no included_at", dto.InclusionReported, dto.IncludedAt, c.want)
			}
			if c.want {
				if _, err := svc.IncludeInputs(ctx, c.w, run, 1, []int64{id}); err != nil {
					t.Fatal(err)
				}
				if dto = got()[0]; dto.IncludedAt == nil {
					t.Fatal("GET follow-ups lacks included_at after the receipt")
				}
			}
		}
	})

	t.Run("re-ACK by a later claim replaces inclusion_reported", func(t *testing.T) {
		run := newRun(legacy, 1)
		id := addInput(run, "follow_up", false, 0, legacy)
		if _, err := svc.AckInputs(ctx, legacy, run, 1, []int64{id}); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE runs SET worker_id=$2, claim_generation=2 WHERE id=$1`, run, capable.ID)
		if _, err := svc.AckInputs(ctx, capable, run, 2, []int64{id}); err != nil {
			t.Fatal(err)
		}
		out, err := svc.ConsumedFollowUps(ctx, capable, run)
		if err != nil || len(out) != 1 || out[0].InclusionReported == nil || !*out[0].InclusionReported {
			t.Fatalf("after re-ACK: %v, %v", out, err)
		}
	})

	t.Run("applied receipt of a follow_up publishes, of another kind does not", func(t *testing.T) {
		run := newRun(capable, 1)
		follow := addInput(run, "follow_up", true, 1, capable)
		answer := addInput(run, "answer", true, 1, capable)
		before := pub.count()
		if _, err := svc.ApplyInputs(ctx, capable, run, 1, []int64{answer}); err != nil {
			t.Fatal(err)
		}
		if pub.count() != before {
			t.Fatal("applying a non-follow_up published")
		}
		if _, err := svc.ApplyInputs(ctx, capable, run, 1, []int64{follow}); err != nil {
			t.Fatal(err)
		}
		if pub.count() != before+1 {
			t.Fatalf("applying a follow_up published %d times, want 1", pub.count()-before)
		}
		if _, err := svc.ApplyInputs(ctx, capable, run, 1, []int64{follow}); err != nil {
			t.Fatal(err)
		}
		if pub.count() != before+1 {
			t.Fatal("a retried applied receipt published again")
		}
	})

	reportedOf := func(id int64) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT inclusion_reported FROM run_user_inputs WHERE id=$1`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Run("legacy ACK then capable claim stamps: inclusion_reported becomes true", func(t *testing.T) {
		run := newRun(legacy, 1)
		id := addInput(run, "follow_up", false, 0, legacy)
		if _, err := svc.AckInputs(ctx, legacy, run, 1, []int64{id}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ApplyInputs(ctx, legacy, run, 1, []int64{id}); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE runs SET worker_id=$2, claim_generation=2 WHERE id=$1`, run, capable.ID)
		if res, err := svc.IncludeInputs(ctx, capable, run, 2, []int64{id}); err != nil || !res.Active {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if !included(id) || !reportedOf(id) {
			t.Fatalf("included=%v reported=%v, want both true", included(id), reportedOf(id))
		}
	})

	t.Run("legacy re-ACK of an already-stamped unapplied row keeps inclusion_reported", func(t *testing.T) {
		run := newRun(capable, 1)
		id := addInput(run, "follow_up", false, 0, capable)
		if _, err := svc.AckInputs(ctx, capable, run, 1, []int64{id}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.IncludeInputs(ctx, capable, run, 1, []int64{id}); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE runs SET worker_id=$2, claim_generation=2 WHERE id=$1`, run, legacy.ID)
		if _, err := svc.AckInputs(ctx, legacy, run, 2, []int64{id}); err != nil {
			t.Fatal(err)
		}
		if !included(id) || !reportedOf(id) {
			t.Fatalf("included=%v reported=%v after legacy re-ACK, want both true", included(id), reportedOf(id))
		}
	})

	t.Run("a foreign run's row is not stamped through this run's claim", func(t *testing.T) {
		runA := newRun(capable, 1)
		runB := newRun(capable, 1)
		foreign := addInput(runB, "follow_up", true, 1, capable)
		own := addInput(runA, "follow_up", true, 1, capable)
		res, err := svc.IncludeInputs(ctx, capable, runA, 1, []int64{foreign, own})
		if err != nil || !res.Active {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if included(foreign) || !included(own) {
			t.Fatalf("foreign=%v own=%v, want foreign unstamped and own stamped", included(foreign), included(own))
		}
		if len(res.Inputs) != 1 || res.Inputs[0].ID != own || res.Inputs[0].Kind != "follow_up" {
			t.Fatalf("Inputs = %+v, want only the newly stamped own row", res.Inputs)
		}
	})
}
