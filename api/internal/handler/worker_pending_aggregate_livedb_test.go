package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestIssue2009PendingAggregateRoutesLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, owner)
	worker, empty := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{worker, empty} {
		cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,pending_overflow,pending_overflow_until) VALUES($1,$2,'pending',$3,'online',true,now()-interval '1 hour')", id, owner, id[:])
	}
	since := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	listed := uuid.New()
	cliMustExec(t, pool, "INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,'chat','t','d','running',$3,7)", listed, owner, worker)
	cliMustExec(t, pool, "INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,terminal_pending_since,terminal_pending_until,snapshot_epoch,reported_at) VALUES($1,$2,7,'awaiting_input',true,$3,now()+interval '1 hour',1,now())", worker, listed, since)
	// The duplicate is counted once; identity age must never replace snapshot age.
	cliMustExec(t, pool, "INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,first_seen_at,reported_until) VALUES($1,$2,7,now()-interval '8 hours',now()+interval '1 hour')", worker, listed)
	for _, kind := range []string{"eligible", "expired", "wrong-generation", "wrong-owner", "released", "terminal"} {
		id := uuid.New()
		cliMustExec(t, pool, "INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,'chat','t','d','running',$3,7)", id, owner, worker)
		cliMustExec(t, pool, "INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,first_seen_at,reported_until) VALUES($1,$2,7,now()-interval '8 hours',now()+interval '1 hour')", worker, id)
		switch kind {
		case "expired":
			cliMustExec(t, pool, "UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 hour' WHERE run_id=$1", id)
		case "wrong-generation":
			cliMustExec(t, pool, "UPDATE runs SET claim_generation=8 WHERE id=$1", id)
		case "wrong-owner":
			cliMustExec(t, pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", id, empty)
		case "released":
			cliMustExec(t, pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
		case "terminal":
			cliMustExec(t, pool, "UPDATE runs SET status='completed' WHERE id=$1", id)
		}
	}
	expectReported := true
	check := func(t *testing.T, count *int64, overflow *bool) {
		t.Helper()
		for _, route := range []struct {
			method, path, body string
			list               bool
		}{
			{http.MethodGet, "/api/workers", "", true},
			{http.MethodGet, "/api/admin/workers", "", true},
			{http.MethodPatch, "/api/workers/" + worker.String(), `{"anthropic_token":null}`, false},
		} {
			rec := cookieReq(t, router, route.method, route.path, jwt, route.body)
			if rec.Code != 200 {
				t.Fatalf("%s: %d %s", route.path, rec.Code, rec.Body.String())
			}
			var dto apitypes.WorkerDTO
			if route.list {
				var body struct {
					Workers []apitypes.WorkerDTO `json:"workers"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, w := range body.Workers {
					if w.ID == worker.String() {
						dto = w
						found = true
					}
					if w.ID == empty.String() && count != nil && (w.PendingOutcomeCount == nil || *w.PendingOutcomeCount != 0 || w.PendingOverflow == nil || *w.PendingOverflow) {
						t.Fatalf("empty authority: %+v", w)
					}
				}
				if !found {
					t.Fatalf("worker missing: %s", rec.Body.String())
				}
			} else {
				var body struct {
					Worker apitypes.WorkerDTO `json:"worker"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				dto = body.Worker
			}
			if count == nil {
				if dto.PendingOutcomeCount != nil || dto.PendingOverflow != nil || strings.Contains(rec.Body.String(), "\"pending_outcome_count\"") || strings.Contains(rec.Body.String(), "\"pending_overflow\"") {
					t.Fatalf("failed query exposed authority: %s", rec.Body.String())
				}
			} else if dto.PendingOutcomeCount == nil || *dto.PendingOutcomeCount != *count || dto.PendingOverflow == nil || *dto.PendingOverflow != *overflow {
				t.Fatalf("authority: %s", rec.Body.String())
			}
			if !expectReported {
				if dto.ReportedRuns == nil || len(dto.ReportedRuns) != 0 {
					t.Fatalf("expected empty actual reported rows: %+v", dto.ReportedRuns)
				}
				continue
			}
			if len(dto.ReportedRuns) != 1 || dto.ReportedRuns[0].RunID != listed.String() || dto.ReportedRuns[0].Phase != "awaiting_input" || dto.ReportedRuns[0].ClaimGeneration != 7 || !dto.ReportedRuns[0].TerminalPending || dto.ReportedRuns[0].TerminalPendingSince == nil || !dto.ReportedRuns[0].TerminalPendingSince.Equal(since) {
				t.Fatalf("snapshot changed: %+v", dto.ReportedRuns)
			}
		}
	}
	count, overflow := int64(2), false
	check(t, &count, &overflow)
	cliMustExec(t, pool, "UPDATE workers SET pending_overflow_until=now()+interval '1 hour' WHERE id=$1", worker)
	overflow = true
	check(t, &count, &overflow)
	// This database belongs exclusively to this serial test run. Restore before
	// cleanup closes the pool, including when a response assertion aborts the test.
	t.Run("aggregate failure preserves snapshots", func(t *testing.T) {
		cliMustExec(t, pool, "ALTER TABLE worker_pending_outcome_identities RENAME TO issue2009_pending_identities_unavailable")
		t.Cleanup(func() {
			cliMustExec(t, pool, "ALTER TABLE issue2009_pending_identities_unavailable RENAME TO worker_pending_outcome_identities")
		})
		check(t, nil, nil)
	})
	t.Run("snapshot failure preserves authority", func(t *testing.T) {
		cliMustExec(t, pool, "ALTER TABLE worker_active_runs RENAME COLUMN phase TO issue2009_phase_unavailable")
		t.Cleanup(func() {
			cliMustExec(t, pool, "ALTER TABLE worker_active_runs RENAME COLUMN issue2009_phase_unavailable TO phase")
		})
		var dto apitypes.WorkerDTO
		h.overlayReportedRuns(&dto, h.reportedRunsByWorker(context.Background(), []uuid.UUID{worker}), worker)
		if dto.ReportedRuns == nil || len(dto.ReportedRuns) != 0 || dto.PendingOutcomeCount == nil || *dto.PendingOutcomeCount != 2 || dto.PendingOverflow == nil || !*dto.PendingOverflow {
			t.Fatalf("snapshot failure lost authority: %+v", dto)
		}
	})
	// Equal count, different members: the displayed tuple is stale while another
	// current tuple supplies the aggregate. Snapshot ID and age remain unchanged.
	cliMustExec(t, pool, "DELETE FROM worker_pending_outcome_identities WHERE worker_id=$1", worker)
	cliMustExec(t, pool, "UPDATE runs SET claim_generation=8 WHERE id=$1", listed)
	unlisted := uuid.New()
	cliMustExec(t, pool, "INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,'chat','t','d','running',$3,7)", unlisted, owner, worker)
	cliMustExec(t, pool, "INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,first_seen_at,reported_until) VALUES($1,$2,7,now()-interval '9 hours',now()+interval '1 hour')", worker, unlisted)
	count = 1
	check(t, &count, &overflow)
	// N=0 with fresh overflow and no actual reported rows is still authoritative.
	cliMustExec(t, pool, "DELETE FROM worker_active_runs WHERE worker_id=$1", worker)
	cliMustExec(t, pool, "UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 second' WHERE worker_id=$1", worker)
	expectReported = false
	count = 0
	check(t, &count, &overflow)
	cliMustExec(t, pool, "UPDATE workers SET pending_overflow_until=now()-interval '1 second' WHERE id=$1", worker)
	overflow = false
	check(t, &count, &overflow)
	cliMustExec(t, pool, "UPDATE workers SET pending_overflow=false,pending_overflow_until=NULL WHERE id=$1", worker)
	check(t, &count, &overflow)
	// A nil store is unavailable, never authoritative zero.
	h.q = nil
	var dto apitypes.WorkerDTO
	h.overlayReportedRuns(&dto, h.reportedRunsByWorker(context.Background(), []uuid.UUID{worker}), worker)
	if dto.ReportedRuns == nil || len(dto.ReportedRuns) != 0 || dto.PendingOutcomeCount != nil || dto.PendingOverflow != nil {
		t.Fatalf("nil store: %+v", dto)
	}
}
