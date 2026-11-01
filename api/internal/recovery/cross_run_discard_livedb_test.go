package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Freeze the identities supplied by the binding fixture before any database drift.
func crossRunInputs(t *testing.T, e *inventoryEnv, witness uuid.UUID, binding map[string]any) (CrossRunTargetBinding, VerifiedCrossRunWitnessSnapshot) {
	t.Helper()
	stamp := func(key string) time.Time {
		at, err := time.Parse(time.RFC3339Nano, binding[key].(string))
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	b := CrossRunWitnessBinding{MergeBinding: MergeBinding{
		RunID: witness, OwnerID: e.w.UserID, RepoID: uuid.MustParse(binding["repo_id"].(string)),
		ConnectionID: uuid.MustParse(binding["connection_id"].(string)), ProjectID: binding["project_id"].(int64),
		MRIID: 7, CompletedGeneration: binding["witness_completed_generation"].(int64),
		ForgeType: binding["forge_type"].(string), BaseURL: binding["base_url"].(string)},
		Kind: "issue", IssueIID: 101, CreatedAt: stamp("witness_created_at")}
	s, err := NewVerifiedCrossRunWitnessSnapshot(b, stamp("verified_at"), &MergeObservation{IID: 7, State: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	return CrossRunTargetBinding{RunID: e.run, OwnerID: e.w.UserID, RepoID: b.RepoID, Kind: "issue",
		Status: "cancelled", Generation: binding["target_terminal_generation"].(int64), IssueIID: 101,
		FinishedAt: stamp("target_finished_at")}, s
}

func crossRunRow(t *testing.T, e *inventoryEnv, table string, id uuid.UUID) string {
	t.Helper()
	// table is selected only by literal callers in this file.
	var row string
	if err := e.pool.QueryRow(e.ctx, "SELECT to_jsonb(r)::text FROM "+table+" r WHERE id=$1", id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func crossRunRetained(t *testing.T, e *inventoryEnv, a CrossRunTargetBinding, s VerifiedCrossRunWitnessSnapshot, message string) {
	t.Helper()
	before := mergedWorkSnapshot(t, e)
	calls := 0
	e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
	n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
	if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
		t.Fatal(message + ": custody or partial bytes changed")
	}
	if n != 0 || (!errors.Is(err, ErrMergeSnapshotMismatch) && !errors.Is(err, ErrBadRequest)) || calls != 0 {
		t.Fatalf("%s: changed=%d err=%v hooks=%d", message, n, err, calls)
	}
}

func TestCrossRunDiscardPolicyAndArchiveLiveDB(t *testing.T) {
	for _, mode := range []string{
		"foreign_owner", "foreign_repo", "future_generation", "zero_generation", "negative_generation", "duplicate_generation",
		"foreign_owner_closed", "foreign_repo_closed", "future_generation_closed", "zero_generation_closed", "negative_generation_closed", "duplicate_generation_closed",
		"open_live_run_other", "open_live_worker_other", "ancestry_missing_successor", "ancestry_future_successor", "ancestry_wrong_worker", "ancestry_missing_worker_identity",
	} {
		t.Run("full_inventory/"+mode, func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			unrelated := newInventoryEnv(t)
			unrelatedBefore := mergedWorkSnapshot(t, unrelated)
			var foreignRepo uuid.UUID
			if err := unrelated.pool.QueryRow(unrelated.ctx, "SELECT repo_id FROM runs WHERE id=$1", unrelated.run).Scan(&foreignRepo); err != nil {
				t.Fatal(err)
			}
			// Keep eligible custody, ready bytes and partial chunks in the batch. The
			// malformed sibling is historical and unguarded; no trigger is disabled.
			ready := e.reserve("inventory-ready")
			e.upload(ready)
			partial := e.reserve("inventory-partial")
			e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", partial, []byte("partial"))
			sibling := uuid.New()
			gen := int64(2)
			owner, repo := a.OwnerID, a.RepoID
			switch strings.TrimSuffix(mode, "_closed") {
			case "foreign_owner":
				owner = unrelated.w.UserID
			case "foreign_repo":
				repo = foreignRepo
			case "future_generation":
				gen = 4
			case "zero_generation":
				gen = 0
			case "negative_generation":
				gen = -1
			case "duplicate_generation":
				gen = 1
			}
			insertMergedHold(e, sibling, owner, repo, e.run, e.w.ID, gen, false)
			if strings.HasSuffix(mode, "_closed") {
				e.exec("UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard' WHERE id=$1", sibling)
			}
			switch mode {
			case "open_live_run_other":
				e.exec("UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", sibling, unrelated.run)
			case "open_live_worker_other":
				e.exec("UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1", sibling, unrelated.w.ID)
			case "ancestry_missing_successor", "ancestry_future_successor", "ancestry_wrong_worker", "ancestry_missing_worker_identity":
				successorGen := int64(3)
				if mode == "ancestry_future_successor" {
					successorGen = 4
				}
				if mode == "ancestry_wrong_worker" {
					insertMergedHold(e, uuid.New(), a.OwnerID, a.RepoID, e.run, unrelated.w.ID, 3, false)
				}
				if mode == "ancestry_missing_worker_identity" {
					successor := uuid.New()
					insertMergedHold(e, successor, a.OwnerID, a.RepoID, e.run, e.w.ID, 3, false)
					e.exec("UPDATE recovery_custody_holds SET original_worker_identity='' WHERE id=$1", successor)
				}
				// SQL CHECKs require a complete ancestry shape but do not prove that a
				// successor exists or belongs to the original worker.
				e.exec(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
 release_evidence='live_ancestry',released_at=now(),release_successor_generation=$2,
 release_branch='agent/cross-run',release_target='branch',release_pushed_sha=$3,
 release_source_sha=$3,release_adopted_sha=$3,release_final_head_sha=$3 WHERE id=$1`, sibling, successorGen, heldTestTip)
			}
			// A guard can refuse an UPDATE without an SQL error. Verify the
			// intended anomaly actually exists before asking policy to reject it.
			var actualOwner, actualRepo uuid.UUID
			var actualGen int64
			var state string
			var liveRun, liveWorker pgtype.UUID
			if err := e.pool.QueryRow(e.ctx, "SELECT user_id,repo_id,generation,state,live_run_id,live_worker_id FROM recovery_custody_holds WHERE id=$1", sibling).
				Scan(&actualOwner, &actualRepo, &actualGen, &state, &liveRun, &liveWorker); err != nil {
				t.Fatal(err)
			}
			wantState := "open"
			if strings.HasSuffix(mode, "_closed") {
				wantState = "discarded"
			}
			if strings.HasPrefix(mode, "ancestry_") {
				wantState = "released"
			}
			if actualOwner != owner || actualRepo != repo || actualGen != gen || state != wantState {
				t.Fatal("inventory anomaly setup was not admitted")
			}
			if mode == "open_live_run_other" && liveRun != pgconv.UUID(unrelated.run) {
				t.Fatal("foreign live run setup was not admitted")
			}
			if mode == "open_live_worker_other" && liveWorker != pgconv.UUID(unrelated.w.ID) {
				t.Fatal("foreign live worker setup was not admitted")
			}
			if mode == "ancestry_missing_worker_identity" {
				var identity string
				if err := e.pool.QueryRow(e.ctx, "SELECT original_worker_identity FROM recovery_custody_holds WHERE run_id=$1 AND generation=3", e.run).Scan(&identity); err != nil || identity != "" {
					t.Fatalf("missing identity setup err=%v", err)
				}
			}
			crossRunRetained(t, e, a, s, "full inventory "+mode)
			if !bytes.Equal(unrelatedBefore, mergedWorkSnapshot(t, unrelated)) {
				t.Fatal("inventory refusal changed unrelated custody")
			}
		})
	}
	t.Run("historical_legacy_writer", func(t *testing.T) {
		e, legacy := newMergedPredecessorEnv(t)
		if n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, legacy); err != nil || n != 1 {
			t.Fatalf("actual legacy writer: changed=%d err=%v", n, err)
		}
		assertMergedHoldState(t, e, e.hold, "discarded")
		prior := crossRunRow(t, e, "recovery_custody_holds", e.hold)
		// Keep the writer's immutable Hgen1 audit verbatim. Only the run's
		// current terminal identity changes; the new witness is a distinct run.
		e.exec("UPDATE runs SET status='cancelled',kind='issue',issue_iid=101,claim_generation=4,mr_iid=NULL,fail_origin=NULL,finished_at='2025-01-02T03:04:05.123456Z' WHERE id=$1", e.run)
		witness := uuid.New()
		binding := legacy.binding
		e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,mr_iid,created_at)
 VALUES($1,$2,$3,'issue',101,'witness','body','completed',1,7,'2025-01-02T03:04:06.654321Z')`, witness, binding.OwnerID, binding.RepoID)
		// These are service inputs, not a fabricated historical audit.
		b := map[string]any{
			"repo_id": binding.RepoID.String(), "connection_id": binding.ConnectionID.String(),
			"project_id": binding.ProjectID, "forge_type": binding.ForgeType, "base_url": binding.BaseURL,
			"target_terminal_generation": int64(4), "witness_completed_generation": int64(1),
			"target_finished_at": "2025-01-02T03:04:05.123456Z", "witness_created_at": "2025-01-02T03:04:06.654321Z",
			"verified_at": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano),
		}
		a, s := crossRunInputs(t, e, witness, b)
		open := uuid.New()
		insertMergedHold(e, open, a.OwnerID, a.RepoID, e.run, e.w.ID, 4, true)
		calls := 0
		e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
		n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
		if prior != crossRunRow(t, e, "recovery_custody_holds", e.hold) {
			t.Fatal("historical writer's closed hold and immutable audit changed")
		}
		if err != nil || n != 1 || calls != 1 {
			t.Fatalf("eligible target should discard: changed=%d err=%v hooks=%d", n, err, calls)
		}
		assertMergedHoldState(t, e, open, "discarded")
	})
	for _, count := range []int{1000, 1001} {
		t.Run(fmt.Sprintf("mixed_closed_open_inventory_%d", count), func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 1001, 1)
			a, s := crossRunInputs(t, e, w, b)
			// Normal owner-discard closure, followed by equivalent earlier
			// generations. No guard or constraint is disabled.
			e.exec("UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard' WHERE id=$1", e.hold)
			e.exec(`INSERT INTO recovery_custody_holds
 SELECT (jsonb_populate_record(NULL::recovery_custody_holds,to_jsonb(h)||
 jsonb_build_object('id',gen_random_uuid(),'generation',g))).*
 FROM recovery_custody_holds h CROSS JOIN generate_series(2,$2::bigint) g WHERE h.id=$1`, e.hold, count-1)
			closedRows := func() string {
				var rows string
				if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_agg(to_jsonb(h) ORDER BY generation,id)::text
 FROM recovery_custody_holds h WHERE run_id=$1 AND generation<1001 AND state='discarded'`, e.run).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				return rows
			}
			closed := closedRows()
			open := uuid.New()
			insertMergedHold(e, open, a.OwnerID, a.RepoID, e.run, e.w.ID, 1001, true)
			before := mergedWorkSnapshot(t, e)
			calls := 0
			e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
			n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
			if closed != closedRows() {
				t.Fatal("closed inventory rows changed")
			}
			if count == 1001 {
				if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
					t.Fatal("all-state inventory overflow must roll back the entire batch")
				}
				if n != 0 || !errors.Is(err, ErrMergedPredecessorWorkLimit) || calls != 0 {
					t.Fatalf("mixed inventory overflow: changed=%d err=%v hooks=%d", n, err, calls)
				}
			} else {
				if err != nil || n != 1 || calls != 1 {
					t.Fatalf("mixed inventory exact limit: changed=%d err=%v hooks=%d", n, err, calls)
				}
				assertMergedHoldState(t, e, open, "discarded")
			}
		})
	}
	t.Run("malformed_closed_sibling", func(t *testing.T) {
		e, w, b := newCrossRunBindingEnv(t, 3, 1)
		a, s := crossRunInputs(t, e, w, b)
		sibling := uuid.New()
		insertMergedHold(e, sibling, a.OwnerID, a.RepoID, e.run, e.w.ID, 2, false)
		// An unguarded legacy row may be closed while retaining a live pointer.
		// The service must inspect it as well as the otherwise eligible OPEN row.
		e.exec("UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard' WHERE id=$1", sibling)
		crossRunRetained(t, e, a, s, "malformed closed sibling must refuse the entire batch")
	})
	for _, generation := range []int64{1, 3} {
		t.Run(fmt.Sprintf("A%d_B1_policy_control", generation), func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, generation, 1)
			a, s := crossRunInputs(t, e, w, b)
			if generation == 1 {
				a.Status = "failed"
				e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)
			}
			other := newInventoryEnv(t)
			unrelated := mergedWorkSnapshot(t, other)
			selected := []uuid.UUID{e.hold}
			for gen := int64(2); gen <= generation; gen++ {
				id := uuid.New()
				insertMergedHold(e, id, a.OwnerID, a.RepoID, e.run, e.w.ID, gen, true)
				selected = append(selected, id)
			}
			// This thin archive's inventory is unique to A; no ancestry proof is offered.
			ready := e.reserve("unique-nonancestor-policy-control")
			body := []byte("unique A inventory, absent from witness")
			manifest := manifestOf(body)
			manifest.ChunkCount = 1
			manifest.PrerequisiteShas = []string{strings.Repeat("b", 40)}
			if _, err := e.svc.Upload(e.ctx, e.w, e.run, ready, manifest, bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			before, err := mergedCapture(t, e, ready)
			if err != nil {
				t.Fatal(err)
			}
			var partial []uuid.UUID
			for _, state := range []string{"preparing", "uploading", "needs_action"} {
				id := e.reserve(state)
				e.exec("UPDATE recovery_captures SET state=$2 WHERE id=$1", id, state)
				e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", id, []byte("partial"))
				partial = append(partial, id)
			}
			calls := 0
			e.svc.SetCustodySettledHook(func(run uuid.UUID) {
				calls++
				if run != e.run {
					t.Error("hook must name only target A")
				}
				for _, id := range selected {
					assertMergedHoldState(t, e, id, "discarded")
				}
				for _, id := range partial {
					c, err := mergedCapture(t, e, id)
					if err != nil || c.State != "discarded" {
						t.Error("AFTER COMMIT hook must see captures settled")
					}
				}
				if !bytes.Equal(unrelated, mergedWorkSnapshot(t, other)) {
					t.Error("hook observed unrelated custody changed")
				}
			})
			n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
			if err != nil || n != generation || calls != 1 {
				t.Fatalf("eligible target should discard: changed=%d err=%v hooks=%d", n, err, calls)
			}
			after, err := mergedCapture(t, e, ready)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("thin archive metadata, TTL and prerequisites must remain identical")
			}
			e.bytes(ready, body)
			for _, id := range partial {
				c, err := mergedCapture(t, e, id)
				chunks, ce := e.q.ListCaptureChunks(e.ctx, id)
				if err != nil || ce != nil || c.State != "discarded" || len(chunks) != 0 {
					t.Fatal("nonready captures and bytes must settle atomically")
				}
			}
			if !bytes.Equal(unrelated, mergedWorkSnapshot(t, other)) {
				t.Fatal("unrelated custody changed")
			}
			if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 0 || calls != 1 {
				t.Fatalf("lost ACK replay must change zero and emit no extra hook: %d %v %d", n, err, calls)
			}
		})
	}
	for _, count := range []int{1000, 1001} {
		t.Run(fmt.Sprintf("shared_capture_budget_%d", count), func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			e.svc.limits.MaxCapturesPerClaim = 0
			e.svc.limits.MaxCapturesPerOwner = 0
			sibling := uuid.New()
			insertMergedHold(e, sibling, a.OwnerID, a.RepoID, e.run, e.w.ID, 3, false)
			seed := e.reserve("bulk-seed")
			bulkMergedCaptures(e, seed, e.hold, 499)
			bulkMergedCaptures(e, seed, sibling, count-500)
			e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) SELECT id,0,7,$2 FROM recovery_captures WHERE run_id=$1", e.run, []byte("partial"))
			before := mergedWorkSnapshot(t, e)
			calls := 0
			e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
			n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
			if count == 1001 {
				if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
					t.Fatal("shared overflow must roll back siblings and bytes")
				}
				if n != 0 || !errors.Is(err, ErrMergedPredecessorWorkLimit) || calls != 0 {
					t.Fatalf("shared overflow: %d %v hooks=%d", n, err, calls)
				}
			} else {
				if err != nil || n != 2 || calls != 1 {
					t.Fatalf("shared 1000 capture budget: %d %v hooks=%d", n, err, calls)
				}
				var remaining int
				if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM recovery_captures WHERE run_id=$1 AND state<>'discarded'", e.run).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatal("all 1000 captures must be discarded")
				}
			}
		})
	}
	for _, kind := range []string{"inventory", "publication"} {
		t.Run(kind+"_overflow_1001", func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 1001, 1)
			a, s := crossRunInputs(t, e, w, b)
			e.exec(`INSERT INTO recovery_custody_holds
 SELECT (jsonb_populate_record(NULL::recovery_custody_holds,to_jsonb(h)||
 jsonb_build_object('id',gen_random_uuid(),'generation',g))).*
 FROM recovery_custody_holds h CROSS JOIN generate_series(2,1001) g WHERE h.id=$1`, e.hold)
			if kind == "publication" {
				// Keep holds under their cap. Publications have distinct run/generation keys;
				// the cap must fire before classifying their mismatched hold generations.
				e.exec("DELETE FROM recovery_custody_holds WHERE run_id=$1 AND generation>1", e.run)
				e.exec(`INSERT INTO run_held_publications
 (id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
 SELECT gen_random_uuid(),$1,g,$2,$3,$4,$5,$1,'refs/uzi-held/'||$1::uuid::text||'/'||g::text,$6,$7,'prepared'
 FROM generate_series(1,1001) g`, e.run, e.hold, a.OwnerID, a.RepoID, e.w.ID, heldTestTip, heldTestDigest)
			}
			before := mergedWorkSnapshot(t, e)
			calls := 0
			e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
			n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
			if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
				t.Fatal("inventory overflow must leave siblings unchanged")
			}
			if n != 0 || !errors.Is(err, ErrMergedPredecessorWorkLimit) || calls != 0 {
				t.Fatalf("%s overflow: %d %v hooks=%d", kind, n, err, calls)
			}
		})
	}
}

// These tests deliberately use the guarded ancestry writer before making A terminal.
func TestCrossRunDiscardReleasedLiveAncestryDependencyLiveDB(t *testing.T) {
	for _, shared := range []string{"independent_capture", "foreign_live_pointer"} {
		t.Run(shared, func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			foreign := newInventoryEnv(t)
			foreign.exec("DELETE FROM recovery_custody_holds WHERE id=$1", foreign.hold)
			insertMergedHold(foreign, foreign.hold, foreign.w.UserID, a.RepoID, foreign.run, foreign.w.ID, 1, false)
			if shared == "independent_capture" {
				id := e.reserve("independent-shared-archive")
				e.upload(id)
				// This capture has no FINAL on A. An external reference still conflicts.
				foreign.exec("UPDATE recovery_custody_holds SET final_capture_id=$2 WHERE id=$1", foreign.hold, id)
			} else {
				// No cross-run custody transfer is recognized by this policy.
				foreign.exec("UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", foreign.hold, e.run)
			}
			before := mergedWorkSnapshot(t, foreign)
			crossRunRetained(t, e, a, s, "shared consumer must retain target")
			if !bytes.Equal(before, mergedWorkSnapshot(t, foreign)) {
				t.Fatal("shared consumer custody changed")
			}
		})
	}
	for _, mode := range []string{"live_ancestry", "live_ancestry_missing_successor_archive", "completed_ancestry_no_successor", "archive_final", "shared_archive_final"} {
		t.Run(mode, func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", e.hold)
			insertMergedHold(e, e.hold, a.OwnerID, a.RepoID, e.run, e.w.ID, 1, false)
			successor := uuid.New()
			insertMergedHold(e, successor, a.OwnerID, a.RepoID, e.run, e.w.ID, 3, true)
			switch mode {
			case "live_ancestry", "live_ancestry_missing_successor_archive":
				e.exec("UPDATE runs SET status='running',branch='agent/cross-run',claim_released_at=NULL WHERE id=$1", e.run)
				n, err := e.q.ReleasePredecessorCustodyHoldByLiveAncestry(e.ctx, store.ReleasePredecessorCustodyHoldByLiveAncestryParams{
					HoldID: e.hold, RunID: e.run, UserID: a.OwnerID, PredecessorGeneration: 1, SuccessorGeneration: 3,
					WorkerID: e.w.ID, PublishedSha: heldTestTip, SourceSha: heldTestTip, AdoptedSha: heldTestTip, FinalHeadSha: heldTestTip,
					ProvenBranch: "agent/cross-run", Target: "branch", CapturedBranch: pgtype.Text{String: "agent/cross-run", Valid: true},
					Kind: "issue", IssueIid: pgtype.Int8{Int64: 101, Valid: true}})
				if err != nil || n != 1 {
					t.Fatalf("guarded live ancestry fixture: %d %v", n, err)
				}
			case "completed_ancestry_no_successor":
				e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", successor)
				// Keep an independent OPEN generation to prove the service really disposes.
				successor = uuid.New()
				insertMergedHold(e, successor, a.OwnerID, a.RepoID, e.run, e.w.ID, 2, true)
				e.exec("UPDATE runs SET status='completed',branch='agent/cross-run' WHERE id=$1", e.run)
				var since pgtype.Timestamptz
				if err := e.pool.QueryRow(e.ctx, "SELECT status_since FROM runs WHERE id=$1", e.run).Scan(&since); err != nil {
					t.Fatal(err)
				}
				n, err := e.q.ReleasePredecessorCustodyHoldByAncestry(e.ctx, store.ReleasePredecessorCustodyHoldByAncestryParams{
					HoldID: e.hold, RunID: e.run, PredecessorGeneration: 1, SuccessorGeneration: 3, WorkerID: e.w.ID,
					PushedSha: heldTestTip, SourceSha: heldTestTip, AdoptedSha: heldTestTip, FinalHeadSha: heldTestTip, Branch: "agent/cross-run", CompletedSince: since})
				if err != nil || n != 1 {
					t.Fatalf("guarded completed ancestry fixture: %d %v", n, err)
				}
			case "archive_final", "shared_archive_final":
				// Recreate a guarded original identity matching the capture worker.
				e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", e.hold)
				e.exec(`INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
 VALUES($1,$2,$3,$4,1,$5,$6,$5,$4,true,'open')`, e.hold, a.OwnerID, a.RepoID, e.run, e.w.ID, e.w.Name)
				id := e.reserve("historical-final")
				e.upload(id)
				e.release(e.request(id))
				if mode == "shared_archive_final" {
					foreign := newInventoryEnv(t)
					// A fresh legacy hold may carry malformed historical FINAL links without
					// weakening any trigger. The foreign reference must veto A's settlement.
					foreign.exec("DELETE FROM recovery_custody_holds WHERE id=$1", foreign.hold)
					insertMergedHold(foreign, foreign.hold, foreign.w.UserID, a.RepoID, foreign.run, foreign.w.ID, 1, false)
					foreign.exec("UPDATE recovery_custody_holds SET final_capture_id=$2 WHERE id=$1", foreign.hold, id)
					crossRunRetained(t, e, a, s, "shared foreign FINAL evidence must retain target")
					return
				}
			}
			// A separate available archive is never a substitute for the transfer evidence.
			e.gen = 2
			if mode != "completed_ancestry_no_successor" {
				id := uuid.New()
				insertMergedHold(e, id, a.OwnerID, a.RepoID, e.run, e.w.ID, 2, true)
			}
			available := e.reserve("independently-available")
			body := e.upload(available)
			archiveBefore, err := mergedCapture(t, e, available)
			if err != nil {
				t.Fatal(err)
			}
			e.exec("UPDATE runs SET status='cancelled',finished_at=$2 WHERE id=$1", e.run, a.FinishedAt)
			before := crossRunRow(t, e, "recovery_custody_holds", e.hold)
			if mode == "live_ancestry_missing_successor_archive" {
				e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", successor)
				crossRunRetained(t, e, a, s, "independent archive cannot replace live ancestry successor custody")
				after, err := mergedCapture(t, e, available)
				if err != nil || !reflect.DeepEqual(archiveBefore, after) {
					t.Fatal("refusal changed independent archive")
				}
				e.bytes(available, body)
				return
			}
			n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
			want := int64(2)
			if mode == "completed_ancestry_no_successor" {
				want = 1
			}
			if err != nil || n != want {
				t.Fatalf("historical %s must permit all eligible OPEN A holds: %d %v", mode, n, err)
			}
			if crossRunRow(t, e, "recovery_custody_holds", e.hold) != before {
				t.Fatal("released ancestry/FINAL row must remain byte-equal")
			}
			assertMergedHoldState(t, e, successor, "discarded")
			after, err := mergedCapture(t, e, available)
			if err != nil || !reflect.DeepEqual(archiveBefore, after) {
				t.Fatal("independent archive must remain unchanged")
			}
			e.bytes(available, body)
		})
	}
	for _, consumer := range []struct{ kind, status string }{
		{"issue", "running"}, {"issue", "pool_wait"}, {"task", "running"}, {"task", "pool_wait"},
	} {
		t.Run(consumer.kind+"_"+consumer.status+"_veto", func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			other := newInventoryEnv(t)
			unrelated := mergedWorkSnapshot(t, other)
			if consumer.kind == "task" {
				e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,continued_issue_iid,issue_title,issue_description,status,branch)
 VALUES($1,$2,$3,'task',101,'continued','body',$4,'task/consumer')`, uuid.New(), a.OwnerID, a.RepoID, consumer.status)
			} else {
				e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status)
 VALUES($1,$2,$3,'issue',101,'consumer','body',$4)`, uuid.New(), a.OwnerID, a.RepoID, consumer.status)
			}
			crossRunRetained(t, e, a, s, "live same-issue consumer must veto")
			if !bytes.Equal(unrelated, mergedWorkSnapshot(t, other)) {
				t.Fatal("consumer veto changed unrelated custody")
			}
		})
	}
	// Database evidence domains forbid future strings. Exercise those corrupt/future
	// observations in the classifier without disabling any database guard.
	t.Run("future_and_conflicting_evidence", func(t *testing.T) {
		e, w, b := newCrossRunBindingEnv(t, 3, 1)
		a, _ := crossRunInputs(t, e, w, b)
		holds, err := e.q.LockCrossRunInventory(e.ctx, e.run)
		if err != nil || len(holds) != 1 {
			t.Fatalf("classifier fixture: holds=%d err=%v", len(holds), err)
		}
		h := holds[0]
		h.State = "released"
		h.LiveWorkerID = pgtype.UUID{}
		h.LiveRunID = pgtype.UUID{}
		h.ReleasedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		h.ReleaseEvidence = pgtype.Text{String: "future_transfer", Valid: true}
		if err := classifyCrossRunHold(h, 3, nil, nil, nil); err == nil {
			t.Fatal("unknown future transfer must retain custody")
		}
		h.ReleaseEvidence = pgtype.Text{String: "live_ancestry", Valid: true}
		h.InventoryGuarded = false
		h.ReleaseSuccessorGeneration = pgtype.Int8{Int64: 3, Valid: true}
		h.ReleaseBranch = pgtype.Text{String: "agent/cross-run", Valid: true}
		h.ReleaseTarget = pgtype.Text{String: "branch", Valid: true}
		h.ReleasePushedSha = pgtype.Text{String: heldTestTip, Valid: true}
		h.ReleaseSourceSha = h.ReleasePushedSha
		h.ReleaseAdoptedSha = h.ReleasePushedSha
		h.ReleaseFinalHeadSha = h.ReleasePushedSha
		successor := h
		successor.ID = uuid.New()
		successor.Generation = 3
		successor.RunID = uuid.New()
		if err := classifyCrossRunHold(h, 3, map[int64]store.RecoveryCustodyHold{3: successor}, nil, nil); err == nil {
			t.Fatal("foreign successor must retain custody")
		}
		successor.RunID = e.run
		successor.UserID = a.OwnerID
		if err := classifyCrossRunHold(h, 3, map[int64]store.RecoveryCustodyHold{3: successor}, nil, nil); err != nil {
			t.Fatalf("matching live ancestry control: %v", err)
		}
		h.FinalDisposition = pgtype.Text{String: "archive", Valid: true}
		if err := classifyCrossRunHold(h, 3, map[int64]store.RecoveryCustodyHold{3: successor}, nil, nil); err == nil {
			t.Fatal("conflicting archive and transfer evidence must retain custody")
		}
	})
}

func crossRunPublication(t *testing.T, e *inventoryEnv, hold uuid.UUID, generation int64, state string) uuid.UUID {
	t.Helper()
	var repo uuid.UUID
	if err := e.pool.QueryRow(e.ctx, "SELECT repo_id FROM runs WHERE id=$1", e.run).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	p, err := e.q.UpsertHeldPublication(e.ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: generation, HoldID: hold, UserID: e.w.UserID, RepoID: repo, WorkerID: e.w.ID,
		Ref: fmt.Sprintf("refs/uzi-held/%s/%d", e.run, generation), Tip: heldTestTip, CoverageDigest: heldTestDigest})
	if err != nil {
		t.Fatal(err)
	}
	if state != "prepared" {
		if _, err := e.q.MarkHeldCreateInvoked(e.ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if state != "invoked" {
			if _, err := e.q.RecordHeldCreateOutcome(e.ctx, store.RecordHeldCreateOutcomeParams{ID: p.ID, State: state}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return p.ID
}

func TestCrossRunDiscardHeldOrphanSelectionLiveDB(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, state := range []string{"prepared", "invoked", "create_unknown"} {
			t.Run(fmt.Sprintf("%s_old_closed_%v", state, closed), func(t *testing.T) {
				e, w, b := newCrossRunBindingEnv(t, 3, 1)
				a, s := crossRunInputs(t, e, w, b)
				hold := e.hold
				if closed {
					e.exec("UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard' WHERE id=$1", hold)
					sibling := uuid.New()
					insertMergedHold(e, sibling, a.OwnerID, a.RepoID, e.run, e.w.ID, 3, true)
				}
				pub := crossRunPublication(t, e, hold, 1, state)
				before := crossRunRow(t, e, "run_held_publications", pub)
				message := "unresolved create must veto"
				if state == "create_unknown" {
					message = "unknown create must veto"
				}
				defer func() {
					if crossRunRow(t, e, "run_held_publications", pub) != before {
						t.Error("veto must not reclassify publication")
					} else {
						t.Logf("publication outcome unchanged: %s", state)
					}
				}()
				crossRunRetained(t, e, a, s, message)
			})
		}
	}
	t.Run("created_unacknowledged_orphan", func(t *testing.T) {
		e, w, b := newCrossRunBindingEnv(t, 3, 1)
		a, s := crossRunInputs(t, e, w, b)
		pub := crossRunPublication(t, e, e.hold, 1, "created")
		before := crossRunRow(t, e, "run_held_publications", pub)
		due := func() bool {
			rows, err := e.q.ListHeldPublicationsDueOrphan(e.ctx, store.ListHeldPublicationsDueOrphanParams{Now: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, Lim: 10000})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range rows {
				if p.ID == pub {
					return true
				}
			}
			return false
		}
		if due() {
			t.Fatal("open hold must protect created ref from orphan selection")
		}
		if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 1 {
			t.Fatalf("discard: %d %v", n, err)
		}
		if !due() {
			t.Fatal("created unACK unreferenced ref must become orphan after all holds close")
		}
		if crossRunRow(t, e, "run_held_publications", pub) != before {
			t.Fatal("discard must not alter created ref or TTL")
		}
	})
	for _, acknowledged := range []bool{true, false} {
		t.Run(fmt.Sprintf("held_receipt_ack_%v_survives", acknowledged), func(t *testing.T) {
			h := newHeldEnv(t)
			if acknowledged {
				h.mustRelease() // Real Release supplies an ACK and a receipt, including retention.
			} else {
				// The real guarded first FINAL write also protects a receipt-named ref
				// independently of the ACK state. Do not bypass or disable its guard.
				n, err := h.q.ReleaseHeldPublicationHold(h.ctx, store.ReleaseHeldPublicationHoldParams{
					ID: h.hold, RunID: h.run, UserID: h.user, WorkerID: h.w.ID, Generation: 1,
					PublicationID: h.pub, Tip: heldTestTip, CoverageDigest: heldTestDigest,
				})
				if err != nil || n != 1 {
					t.Fatalf("guarded receipt setup: %d %v", n, err)
				}
			}
			e := &inventoryEnv{t: t, ctx: h.ctx, pool: h.pool, q: h.q, svc: h.svc, w: h.w, run: h.run, hold: h.hold, gen: 1}
			var conn uuid.UUID
			if err := h.pool.QueryRow(h.ctx, "SELECT connection_id FROM repos WHERE id=$1", h.repo).Scan(&conn); err != nil {
				t.Fatal(err)
			}
			witness := uuid.New()
			h.exec("UPDATE runs SET issue_iid=101,status='cancelled',claim_generation=3,mr_iid=NULL,finished_at='2025-01-02T03:04:05.123456Z' WHERE id=$1", h.run)
			h.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,mr_iid,created_at)
 VALUES($1,$2,$3,'issue',101,'witness','body','completed',1,7,'2025-01-02T03:04:06.654321Z')`, witness, h.user, h.repo)
			b := map[string]any{"repo_id": h.repo.String(), "connection_id": conn.String(), "project_id": int64(1),
				"witness_completed_generation": int64(1), "target_terminal_generation": int64(3), "forge_type": "github", "base_url": "https://example.com",
				"witness_created_at": "2025-01-02T03:04:06.654321Z", "target_finished_at": "2025-01-02T03:04:05.123456Z",
				"verified_at": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)}
			a, s := crossRunInputs(t, e, witness, b)
			sibling := uuid.New()
			insertMergedHold(e, sibling, h.user, h.repo, h.run, h.w.ID, 3, true)
			beforePub := crossRunRow(t, e, "run_held_publications", h.pub)
			beforeHold := crossRunRow(t, e, "recovery_custody_holds", h.hold)
			if n, err := h.svc.DiscardCrossRunHolds(h.ctx, a, s); err != nil || n != 1 {
				t.Fatalf("receipt lifecycle: %d %v", n, err)
			}
			if beforePub != crossRunRow(t, e, "run_held_publications", h.pub) || beforeHold != crossRunRow(t, e, "recovery_custody_holds", h.hold) {
				t.Fatal("ACK and receipt reference must survive with unchanged TTL")
			}
			rows, err := h.q.ListHeldPublicationsDueOrphan(h.ctx, store.ListHeldPublicationsDueOrphanParams{Now: pgtype.Timestamptz{Time: time.Now().UTC().Add(72 * time.Hour), Valid: true}, Lim: 10000})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range rows {
				if p.ID == h.pub {
					t.Fatal("ACK receipt ref must never enter orphan lane")
				}
			}
		})
	}
}

func TestCrossRunDiscardEligibilityLiveDB(t *testing.T) {
	// runs_kind_shape requires issue rows to have an IID. NULL-IID database
	// cases therefore use valid prompt rows; constructor tests isolate own-IID shape.
	cases := []struct {
		name, sql string
		witness   bool
	}{
		{"target_kind", "kind='self_improve'", false}, {"target_task", "kind='task',issue_iid=NULL,continued_issue_iid=101,branch='task/target'", false},
		{"target_status", "status='completed'", false}, {"target_running", "status='running'", false},
		{"target_generation", "claim_generation=4", false}, {"target_issue", "issue_iid=102", false},
		{"target_missing_issue", "kind='prompt',issue_iid=NULL", false}, {"target_zero_issue", "issue_iid=0", false}, {"target_negative_issue", "issue_iid=-1", false},
		{"target_finished_null", "finished_at=NULL", false}, {"target_finished_equal", "finished_at='2025-01-02T03:04:06.654321Z'", false},
		{"target_finished_infinite", "finished_at='infinity'", false}, {"target_timeout", "fail_origin='run_timeout'", false},
		{"target_mr_zero", "mr_iid=0", false}, {"target_mr", "mr_iid=8", false},
		{"witness_kind", "kind='self_improve'", true}, {"witness_task", "kind='task',issue_iid=NULL,continued_issue_iid=101,branch='task/witness'", true},
		{"witness_status", "status='failed'", true}, {"witness_generation", "claim_generation=2", true},
		{"witness_issue", "issue_iid=102", true}, {"witness_missing_issue", "kind='prompt',issue_iid=NULL", true},
		{"witness_zero_issue", "issue_iid=0", true}, {"witness_negative_issue", "issue_iid=-1", true},
		{"witness_created", "created_at='2024-01-01T00:00:00Z'", true},
		{"witness_created_infinite", "created_at='infinity'", true},
		{"witness_pr", "mr_iid=8", true}, {"witness_null_pr", "mr_iid=NULL", true}, {"witness_zero_pr", "mr_iid=0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			id := e.run
			if tc.witness {
				id = w
			}
			e.exec("UPDATE runs SET "+tc.sql+" WHERE id=$1", id)
			message := tc.name + " must retain target"
			if tc.name == "target_finished_equal" {
				// Supply the actual equal chronology, not a stale pre-drift target.
				b["target_finished_at"] = b["witness_created_at"]
				a, s = crossRunInputs(t, e, w, b)
				message = "equal chronology must retain target"
			}
			crossRunRetained(t, e, a, s, message)
		})
	}
	for _, name := range []string{"target_owner", "target_repo", "witness_owner", "witness_repo", "connection", "connection_owner", "project", "forge", "base_url", "war_any_generation"} {
		t.Run(name, func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			other := newInventoryEnv(t)
			var repo, conn uuid.UUID
			if err := other.pool.QueryRow(e.ctx, "SELECT r.repo_id,p.connection_id FROM runs r JOIN repos p ON p.id=r.repo_id WHERE r.id=$1", other.run).Scan(&repo, &conn); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "target_owner":
				e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", e.run, other.w.UserID)
			case "target_repo":
				e.exec("UPDATE runs SET repo_id=$2 WHERE id=$1", e.run, repo)
			case "witness_owner":
				e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", w, other.w.UserID)
			case "witness_repo":
				e.exec("UPDATE runs SET repo_id=$2 WHERE id=$1", w, repo)
			case "connection":
				// UNIQUE(user_id,forge_type,base_url) prevents an equivalent
				// replacement connection. Change only the frozen connection ID
				// to exercise the locked comparison with all DB identity intact.
				b["connection_id"] = uuid.New().String()
				a, s = crossRunInputs(t, e, w, b)
			case "connection_owner":
				// Free the destination owner's equivalent URL first; drift
				// only user_id on the original, snapshot-bound connection.
				e.exec("UPDATE forge_connections SET base_url='https://other.example.com' WHERE id=$1", conn)
				e.exec("UPDATE forge_connections SET user_id=$2 WHERE id=$1", s.binding.ConnectionID, other.w.UserID)
			case "project":
				e.exec("UPDATE repos SET forge_project_id=2 WHERE id=$1", a.RepoID)
			case "forge":
				e.exec("UPDATE forge_connections SET forge_type='gitlab' WHERE id=$1", s.binding.ConnectionID)
			case "base_url":
				e.exec("UPDATE forge_connections SET base_url='https://drift.example.com' WHERE id=$1", s.binding.ConnectionID)
			case "war_any_generation":
				e.exec("INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,snapshot_epoch,reported_at) VALUES($1,$2,99,'running',1,now())", e.w.ID, e.run)
			}
			crossRunRetained(t, e, a, s, name+" must retain target")
		})
	}
	t.Run("equal_chronology_combined_policy_control", func(t *testing.T) {
		e, w, b := newCrossRunBindingEnv(t, 3, 1)
		a, s := crossRunInputs(t, e, w, b)
		a.FinishedAt = s.binding.CreatedAt
		e.exec("UPDATE runs SET finished_at=$2 WHERE id=$1", e.run, a.FinishedAt)
		crossRunRetained(t, e, a, s, "equal chronology must retain target")
	})
	t.Run("distinct_runs", func(t *testing.T) {
		e, w, b := newCrossRunBindingEnv(t, 3, 1)
		a, s := crossRunInputs(t, e, w, b)
		a.RunID = w
		crossRunRetained(t, e, a, s, "same run must retain target")
	})
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("late_target_mr_before_commit_%v", before), func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, 3, 1)
			a, s := crossRunInputs(t, e, w, b)
			reconcile := func() {
				n, err := e.q.ReconcileRunMR(e.ctx, store.ReconcileRunMRParams{ID: e.run, WorkerID: pgconv.UUID(e.w.ID), MrIid: pgtype.Int8{Int64: 8, Valid: true}})
				if err != nil || n != 1 {
					t.Fatalf("late MR must be accepted: %d %v", n, err)
				}
			}
			if before {
				reconcile()
				crossRunRetained(t, e, a, s, "late MR before settlement must retain target")
				return
			}
			if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 1 {
				t.Fatalf("discard: %d %v", n, err)
			}
			reconcile()
			assertMergedHoldState(t, e, e.hold, "discarded")
		})
	}
}
