package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Persistence-level compatibility for mixed pacing producers (issue #4467).
func TestControllerStatusMixedPacingCompatibilityLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }
	row := func(id uuid.UUID, pacing any) map[string]any {
		e := map[string]any{"id": id.String(), "phase": "settled"}
		if pacing != nil {
			e["pacing"] = pacing
		}
		return e
	}
	post := func(kind string, entries ...any) {
		t.Helper()
		if code := pacingLivePost(t, h, kind, entries...); code != http.StatusNoContent {
			t.Fatalf("status %d", code)
		}
	}
	held := map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2.0.0", "blocking_reason": "pool paced", "blocking_worker_id": "missing-canary"}

	id := pacingLiveSeedHosted(t, pool, user, "compat")
	post("fleet", row(id, held))
	first := receipt
	r := pacingLiveFetch(t, pool, id)
	if r == nil || r.PacingNull || r.HasBlockingWorker || r.PacingObservedAt == nil || !r.PacingObservedAt.Equal(first) {
		t.Fatalf("held report: %+v", r)
	}

	// A readiness report preserves pacing and its original receipt.
	receipt = receipt.Add(time.Minute)
	post("readiness", row(id, nil))
	r = pacingLiveFetch(t, pool, id)
	if r.PacingNull || r.PacingObservedAt == nil || !r.PacingObservedAt.Equal(first) || !r.ObservedAt.Equal(receipt) {
		t.Fatalf("readiness must preserve pacing and its receipt: %+v", r)
	}

	// Stored pacing expires on the api receipt, independent of the ordinary observed_at.
	var data []byte
	var stamp pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(), `SELECT pacing, pacing_observed_at FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&data, &stamp); err != nil {
		t.Fatal(err)
	}
	if workersvc.ParseFreshPacingReport(data, stamp, first.Add(time.Minute)) == nil {
		t.Fatal("fresh pacing must render")
	}
	if workersvc.ParseFreshPacingReport(data, stamp, first.Add(workersvc.DefaultControllerSignalTTL+time.Second)) != nil {
		t.Fatal("stale pacing must not render")
	}

	// A newer fleet report without pacing clears it and advances observed_at.
	receipt = receipt.Add(time.Minute)
	post("fleet", row(id, nil))
	r = pacingLiveFetch(t, pool, id)
	if !r.PacingNull || !r.PacingObservedNull || !r.ObservedAt.Equal(receipt) {
		t.Fatalf("fleet without pacing must clear it: %+v", r)
	}

	// Unknown and external workers never gain a row.
	unknown := uuid.New()
	external := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version) VALUES($1,$2,'compat-external',$3,'offline','external','1.0.0')`, external, user, []byte(external.String()))
	post("fleet", row(unknown, held), row(external, held))
	if pacingLiveFetch(t, pool, unknown) != nil || pacingLiveFetch(t, pool, external) != nil {
		t.Fatal("unknown or external worker gained a worker_upgrade_reports row")
	}
}
