package recovery

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRecoveryInventoryRepoAgentSourceHoldSettledLiveDB(t *testing.T) {
	evidence := "forge_no_output"
	settled := func(e *inventoryEnv) apitypes.RecoveryReleaseRequest {
		return apitypes.RecoveryReleaseRequest{Generation: &e.gen, ReleaseEvidence: &evidence, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}}
	}
	holdState := func(e *inventoryEnv) string {
		var state string
		if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	for _, tc := range []struct {
		name       string
		holdReason any
		commit     any
		ok         bool
	}{
		{"unpinned-source-hold", "repo_agent_source_unavailable", nil, true},
		{"pinned-source-hold", "repo_agent_source_unavailable", strings.Repeat("a", 40), false},
		{"base-attestation-hold", "base_attestation_missing", nil, false},
		{"owner-pause", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE runs SET status='paused', hold_reason=$2, repo_agent_source_commit=$3 WHERE id=$1", e.run, tc.holdReason, tc.commit)
			if tc.ok {
				e.release(settled(e))
				e.release(settled(e))
				if got := holdState(e); got != "released" {
					t.Fatalf("hold state = %q, want released", got)
				}
			} else {
				e.reject(e.w, settled(e), ErrNotAuthorized)
				if got := holdState(e); got != "open" {
					t.Fatalf("hold state = %q, want open", got)
				}
			}
			var status string
			if err := e.pool.QueryRow(e.ctx, "SELECT status FROM runs WHERE id=$1", e.run).Scan(&status); err != nil || status != "paused" {
				t.Fatalf("run status = %q, %v; want paused", status, err)
			}
		})
	}
	t.Run("unpinned-source-hold-keeps-a-non-empty-inventory", func(t *testing.T) {
		e := newInventoryEnv(t)
		id := e.reserve("final")
		e.upload(id)
		e.exec("UPDATE runs SET status='paused', hold_reason='repo_agent_source_unavailable' WHERE id=$1", e.run)
		e.reject(e.w, e.request(id), ErrNotAuthorized)
		if got := holdState(e); got != "open" {
			t.Fatalf("hold state = %q, want open", got)
		}
	})
	t.Run("the release statement and the hold guard refuse a pinned source hold on their own", func(t *testing.T) {
		e := newInventoryEnv(t)
		e.exec("UPDATE runs SET status='paused', hold_reason='repo_agent_source_unavailable', repo_agent_source_commit=$2 WHERE id=$1", e.run, strings.Repeat("a", 40))
		params := store.ReleaseFinalInventoryHoldParams{
			FinalDisposition: "settled", FinalCoverageDigest: emptyInventoryDigest, ReleaseEvidence: evidence,
			ID: e.hold, RunID: e.run, UserID: e.w.UserID, Generation: e.gen, WorkerID: e.w.ID,
		}
		if n, err := e.q.ReleaseFinalInventoryHold(e.ctx, params); err != nil || n != 0 {
			t.Fatalf("pinned release = (%d, %v), want 0 rows", n, err)
		}
		tag, err := e.pool.Exec(e.ctx, "UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, final_disposition='settled', final_coverage_digest=$2, release_evidence='forge_no_output' WHERE id=$1", e.hold, emptyInventoryDigest)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("pinned guard bypass = (%v, %v), want 0 rows", tag, err)
		}
		e.exec("UPDATE runs SET repo_agent_source_commit=NULL WHERE id=$1", e.run)
		if n, err := e.q.ReleaseFinalInventoryHold(e.ctx, params); err != nil || n != 1 {
			t.Fatalf("unpinned release = (%d, %v), want 1 row", n, err)
		}
	})
}
