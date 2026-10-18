package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func mergedCapture(t *testing.T, e *inventoryEnv, id uuid.UUID) (store.RecoveryCapture, error) {
	t.Helper()
	return e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
}

func newMergedPredecessorEnv(t *testing.T) (*inventoryEnv, VerifiedMergeSnapshot) {
	t.Helper()
	e := newInventoryEnv(t)
	var b MergeBinding
	err := e.pool.QueryRow(e.ctx, `SELECT r.id,r.user_id,r.repo_id,p.connection_id,
		p.forge_project_id,c.forge_type,c.base_url FROM runs r JOIN repos p ON p.id=r.repo_id
		JOIN forge_connections c ON c.id=p.connection_id WHERE r.id=$1`, e.run).
		Scan(&b.RunID, &b.OwnerID, &b.RepoID, &b.ConnectionID, &b.ProjectID, &b.ForgeType, &b.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Use the policy fixture's immutable original identity from birth rather than
	// mutating the common fixture's guarded provenance.
	e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", e.hold)
	insertMergedHold(e, e.hold, b.OwnerID, b.RepoID, b.RunID, e.w.ID, 1, true)
	b.MRIID, b.CompletedGeneration = 7, 4
	e.exec("UPDATE runs SET status='completed',claim_generation=4,mr_iid=7 WHERE id=$1", e.run)
	snapshot, err := NewVerifiedMergeSnapshot(b, time.Now().UTC().Add(-time.Second), &MergeObservation{IID: 7, State: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	return e, snapshot
}

func insertMergedHold(e *inventoryEnv, id, owner, repo, run, worker uuid.UUID, gen int64, guarded bool) {
	e.t.Helper()
	e.exec(`INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,
		live_worker_id,live_run_id,inventory_guarded,state)
		VALUES($1,$2,$3,$4,$5,$6,'original identity',$6,$4,$7,'open')`,
		id, owner, repo, run, gen, worker, guarded)
}

func assertMergedHoldState(t *testing.T, e *inventoryEnv, id uuid.UUID, state string) {
	t.Helper()
	var got string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", id).Scan(&got); err != nil || got != state {
		t.Fatalf("hold %s state=%s err=%v, want %s", id, got, err, state)
	}
}

func TestMergedPredecessorSelectionAndArchiveLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	b := snapshot.binding
	other := newInventoryEnv(t)
	var otherRepo uuid.UUID
	if err := other.pool.QueryRow(e.ctx, "SELECT repo_id FROM runs WHERE id=$1", other.run).Scan(&otherRepo); err != nil {
		t.Fatal(err)
	}
	legacy, differentWorker := uuid.New(), uuid.New()
	insertMergedHold(e, legacy, b.OwnerID, b.RepoID, b.RunID, e.w.ID, 2, false)
	insertMergedHold(e, differentWorker, b.OwnerID, b.RepoID, b.RunID, other.w.ID, 3, true)
	// Existing legacy receipt/provenance is retained verbatim by the discard.
	e.exec(`UPDATE recovery_custody_holds SET release_evidence='publication',
		final_disposition='settled',final_coverage_digest=$2,final_source_sha='prior source'
		WHERE id=$1`, legacy, strings.Repeat("c", 64))
	var prior []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(h)-ARRAY['state','live_worker_id',
		'live_run_id','release_evidence','merged_predecessor_discard_binding','updated_at'])::text
		FROM recovery_custody_holds h WHERE id=$1`, legacy).Scan(&prior); err != nil {
		t.Fatal(err)
	}
	var untouched []uuid.UUID
	for _, spec := range []struct {
		owner, repo, run uuid.UUID
		gen              int64
	}{
		{b.OwnerID, b.RepoID, b.RunID, 4}, {b.OwnerID, b.RepoID, b.RunID, 5},
		{other.w.UserID, b.RepoID, b.RunID, 1},
		{b.OwnerID, otherRepo, b.RunID, 1}, {b.OwnerID, b.RepoID, other.run, 1},
	} {
		id := uuid.New()
		insertMergedHold(e, id, spec.owner, spec.repo, spec.run, other.w.ID, spec.gen, false)
		untouched = append(untouched, id)
	}
	ready := e.reserve("thin-ready")
	body := []byte("thin archive durable bytes")
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
	e.svc.SetCustodySettledHook(func(runID uuid.UUID) {
		calls++
		if runID != e.run {
			t.Errorf("hook run=%s", runID)
		}
		for _, id := range []uuid.UUID{e.hold, legacy, differentWorker} {
			assertMergedHoldState(t, e, id, "discarded") // A separate connection sees the commit.
		}
	})
	n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
	if err != nil || n != 3 || calls != 1 {
		t.Fatalf("discard=%d err=%v hooks=%d", n, err, calls)
	}
	for _, id := range untouched {
		assertMergedHoldState(t, e, id, "open")
	}
	after, err := mergedCapture(t, e, ready)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("available archive changed: before=%+v after=%+v err=%v", before, after, err)
	}
	e.bytes(ready, body)
	for _, id := range partial {
		capture, err := mergedCapture(t, e, id)
		chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, id)
		if err != nil || chunkErr != nil || capture.State != "discarded" || len(chunks) != 0 {
			t.Fatalf("partial capture %+v chunks=%+v errors=%v/%v", capture, chunks, err, chunkErr)
		}
	}
	for _, id := range []uuid.UUID{e.hold, legacy, differentWorker} {
		var evidence string
		var audit []byte
		var liveWorker, liveRun *uuid.UUID
		if err := e.pool.QueryRow(e.ctx, `SELECT release_evidence,merged_predecessor_discard_binding,
			live_worker_id,live_run_id FROM recovery_custody_holds WHERE id=$1`, id).
			Scan(&evidence, &audit, &liveWorker, &liveRun); err != nil {
			t.Fatal(err)
		}
		var binding map[string]any
		if err := json.Unmarshal(audit, &binding); err != nil {
			t.Fatal(err)
		}
		if evidence != "merged_predecessor_discard" || liveWorker != nil || liveRun != nil ||
			binding["hold_id"] != id.String() || binding["run_id"] != b.RunID.String() ||
			binding["owner_id"] != b.OwnerID.String() || binding["repo_id"] != b.RepoID.String() ||
			binding["connection_id"] != b.ConnectionID.String() || binding["forge_type"] != b.ForgeType ||
			binding["base_url"] != b.BaseURL || binding["project_id"] != float64(1) ||
			binding["mr_iid"] != float64(7) || binding["completed_generation"] != float64(4) ||
			binding["verified_at"] != snapshot.verifiedAt.Format(time.RFC3339Nano) {
			t.Fatalf("bad audit %s live=%v/%v evidence=%s", audit, liveWorker, liveRun, evidence)
		}
	}
	var retained []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(h)-ARRAY['state','live_worker_id',
		'live_run_id','release_evidence','merged_predecessor_discard_binding','updated_at'])::text
		FROM recovery_custody_holds h WHERE id=$1`, legacy).Scan(&retained); err != nil || !bytes.Equal(prior, retained) {
		t.Fatalf("legacy provenance changed: %s -> %s err=%v", prior, retained, err)
	}
	// Restart/replay has no effect and emits no hook.
	restarted := New(store.New(e.pool), e.pool, e.svc.box, e.svc.limits, nil)
	restarted.SetCustodySettledHook(func(uuid.UUID) { t.Error("hook on replay") })
	if n, err := restarted.DiscardMergedPredecessorHolds(e.ctx, snapshot); n != 0 || err != nil {
		t.Fatalf("restart replay=%d %v", n, err)
	}
	if ok, err := e.svc.DiscardHold(e.ctx, b.OwnerID, b.RunID, e.hold); ok || err != nil || calls != 1 {
		t.Fatalf("owner replay=%v %v hooks=%d", ok, err, calls)
	}
	e.bytes(ready, body)
}

// Clone a real reservation's schema-complete row. All admission quotas are disabled
// in these fixtures; the transaction guard must stand independently of configuration.
func bulkMergedCaptures(e *inventoryEnv, seed, hold uuid.UUID, count int) {
	e.t.Helper()
	e.exec(`INSERT INTO recovery_captures
		SELECT (jsonb_populate_record(NULL::recovery_captures, to_jsonb(c) ||
			jsonb_build_object('id',gen_random_uuid(),'hold_id',$2::uuid,
				'idempotency_key','bulk-' || g::text))).*
		FROM recovery_captures c CROSS JOIN generate_series(1,$3::int) g
		WHERE c.id=$1`, seed, hold, count)
}

// Snapshot every hold/capture field (including audit) and partial chunk bytes so a
// refusal cannot hide earlier sibling cleanup behind unchanged row counts.
func mergedWorkSnapshot(t *testing.T, e *inventoryEnv) []byte {
	t.Helper()
	var snapshot []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
		'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id)
			FROM recovery_custody_holds h WHERE h.run_id=$1),
		'captures',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id)
			FROM recovery_captures c WHERE c.run_id=$1),
		'chunks',(SELECT jsonb_agg(to_jsonb(ch) ORDER BY ch.capture_id,ch.chunk_index)
			FROM recovery_capture_chunks ch JOIN recovery_captures c ON c.id=ch.capture_id
			WHERE c.run_id=$1))::text`, e.run).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestMergedPredecessorCaptureLimitLiveDB(t *testing.T) {
	for _, spec := range []struct {
		name          string
		first, second int
		refuse        bool
	}{
		{"single_overflow", 1001, 0, true},
		{"exact_limit_across_siblings", 500, 500, false},
		{"combined_overflow", 500, 501, true},
	} {
		t.Run(spec.name, func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			e.svc.limits.MaxCapturesPerClaim = 0
			e.svc.limits.MaxCapturesPerOwner = 0
			ready := e.reserve("ready-outside-budget")
			body := e.upload(ready)
			readyBefore, err := mergedCapture(t, e, ready)
			if err != nil {
				t.Fatal(err)
			}
			seed := e.reserve("limit-seed")
			e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", seed, []byte("partial"))
			bulkMergedCaptures(e, seed, e.hold, spec.first-1)
			holds := int64(1)
			if spec.second > 0 {
				second := uuid.New()
				insertMergedHold(e, second, e.w.UserID, snapshot.binding.RepoID, e.run, e.w.ID, 2, false)
				bulkMergedCaptures(e, seed, second, spec.second)
				holds++
			}
			// Partial bytes on every row catch cleanup of the first sibling before
			// the second sibling exhausts the shared capture budget.
			e.exec(`INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed)
				SELECT id,0,7,$2 FROM recovery_captures
				WHERE run_id=$1 AND state='preparing' AND id<>$3`, e.run, []byte("partial"), seed)
			before := mergedWorkSnapshot(t, e)
			calls := 0
			e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
			n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
			if spec.refuse {
				if n != 0 || !errors.Is(err, ErrMergedPredecessorWorkLimit) || calls != 0 {
					t.Fatalf("overflow: n=%d err=%v hooks=%d", n, err, calls)
				}
				if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
					t.Fatal("overflow changed holds, audit, captures or partial chunks")
				}
				return
			}
			if err != nil || n != holds || calls != 1 {
				t.Fatalf("exact limit: n=%d err=%v hooks=%d", n, err, calls)
			}
			var discarded, partialChunks, open int
			if err := e.pool.QueryRow(e.ctx, `SELECT
				(SELECT count(*) FROM recovery_captures WHERE run_id=$1 AND state='discarded'),
				(SELECT count(*) FROM recovery_capture_chunks ch JOIN recovery_captures c
					ON c.id=ch.capture_id WHERE c.run_id=$1 AND c.id<>$2),
				(SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open')`,
				e.run, ready).Scan(&discarded, &partialChunks, &open); err != nil {
				t.Fatal(err)
			}
			if discarded != 1000 || partialChunks != 0 || open != 0 {
				t.Fatalf("exact limit: discarded=%d partial chunks=%d open holds=%d", discarded, partialChunks, open)
			}
			readyAfter, err := mergedCapture(t, e, ready)
			if err != nil || !reflect.DeepEqual(readyBefore, readyAfter) {
				t.Fatalf("available archive changed: %v", err)
			}
			e.bytes(ready, body)
		})
	}
}

func TestMergedPredecessorHoldLimitLiveDB(t *testing.T) {
	for _, count := range []int{1000, 1001} {
		t.Run(map[int]string{1000: "exact_limit", 1001: "overflow"}[count], func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			e.svc.limits.MaxCapturesPerClaim = 0
			e.svc.limits.MaxCapturesPerOwner = 0
			e.exec(`INSERT INTO recovery_custody_holds
				SELECT (jsonb_populate_record(NULL::recovery_custody_holds, to_jsonb(h) ||
					jsonb_build_object('id',gen_random_uuid()))).*
				FROM recovery_custody_holds h CROSS JOIN generate_series(1,$2::int)
				WHERE h.id=$1`, e.hold, count-1)
			seed := e.reserve("hold-limit")
			e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", seed, []byte("partial"))
			if count > 1000 {
				// A rollback alone cannot prove the limit was checked before updates.
				name := "merged_limit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				e.exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.run_id='" + e.run.String() + "'::uuid AND NEW.state='discarded' THEN RAISE EXCEPTION 'hold updated before limit check'; END IF; RETURN NEW; END $$")
				e.exec("CREATE TRIGGER " + name + " BEFORE UPDATE ON recovery_custody_holds FOR EACH ROW EXECUTE FUNCTION " + name + "()")
				t.Cleanup(func() {
					e.exec("DROP TRIGGER " + name + " ON recovery_custody_holds")
					e.exec("DROP FUNCTION " + name + "()")
				})
			}
			before := mergedWorkSnapshot(t, e)
			calls := 0
			e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
			n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
			if count > 1000 {
				if n != 0 || !errors.Is(err, ErrMergedPredecessorWorkLimit) || calls != 0 {
					t.Fatalf("hold overflow: n=%d err=%v hooks=%d", n, err, calls)
				}
				if !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
					t.Fatal("hold overflow changed holds, audit, captures or chunks")
				}
				return
			}
			if err != nil || n != 1000 || calls != 1 {
				t.Fatalf("exact hold limit: n=%d err=%v hooks=%d", n, err, calls)
			}
			capture, err := mergedCapture(t, e, seed)
			chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, seed)
			if err != nil || chunkErr != nil || capture.State != "discarded" || len(chunks) != 0 {
				t.Fatalf("exact hold limit capture=%+v chunks=%d errors=%v/%v", capture, len(chunks), err, chunkErr)
			}
		})
	}
}

func TestMergedPredecessorSnapshotDriftLiveDB(t *testing.T) {
	for _, name := range []string{"owner", "repo", "connection", "connection_owner", "project", "base_url", "forge", "mr", "generation", "status", "null_mr"} {
		t.Run(name, func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			other := newInventoryEnv(t)
			var otherRepo, otherConn uuid.UUID
			if err := other.pool.QueryRow(e.ctx, `SELECT r.repo_id,p.connection_id FROM runs r
				JOIN repos p ON p.id=r.repo_id WHERE r.id=$1`, other.run).Scan(&otherRepo, &otherConn); err != nil {
				t.Fatal(err)
			}
			b := snapshot.binding
			switch name {
			case "owner":
				e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", e.run, other.w.UserID)
			case "repo":
				e.exec("UPDATE runs SET repo_id=$2 WHERE id=$1", e.run, otherRepo)
			case "connection":
				e.exec("UPDATE repos SET connection_id=$2,forge_project_id=2 WHERE id=$1", b.RepoID, otherConn)
			case "connection_owner":
				e.exec("UPDATE forge_connections SET user_id=$2,base_url='https://owner-drift.example.com' WHERE id=$1", b.ConnectionID, other.w.UserID)
			case "project":
				e.exec("UPDATE repos SET forge_project_id=2 WHERE id=$1", b.RepoID)
			case "base_url":
				e.exec("UPDATE forge_connections SET base_url='https://other.example.com' WHERE id=$1", b.ConnectionID)
			case "forge":
				e.exec("UPDATE forge_connections SET forge_type='gitlab' WHERE id=$1", b.ConnectionID)
			case "mr":
				e.exec("UPDATE runs SET mr_iid=8 WHERE id=$1", e.run)
			case "null_mr":
				e.exec("UPDATE runs SET mr_iid=NULL WHERE id=$1", e.run)
			case "generation":
				e.exec("UPDATE runs SET claim_generation=5 WHERE id=$1", e.run)
			case "status":
				e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)
			}
			e.svc.SetCustodySettledHook(func(uuid.UUID) { t.Error("hook on mismatch") })
			if n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot); n != 0 || !errors.Is(err, ErrMergeSnapshotMismatch) {
				t.Fatalf("drift %s: n=%d err=%v", name, n, err)
			}
			assertMergedHoldState(t, e, e.hold, "open")
		})
	}
}

func TestMergedPredecessorBindingGuardLiveDB(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "guarded"}[guarded], func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			id := e.hold
			if !guarded {
				e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", id)
				insertMergedHold(e, id, snapshot.binding.OwnerID, snapshot.binding.RepoID, e.run, e.w.ID, 1, false)
			}
			unsafe := func(sql string, args ...any) {
				t.Helper()
				tag, err := e.pool.Exec(e.ctx, sql, args...)
				if err == nil && tag.RowsAffected() != 0 {
					t.Fatalf("unsafe SQL accepted: %s", sql)
				}
			}
			for _, binding := range []any{nil, "{}", "null", "[]", `{"verified_at":"nonsense"}`, `{"verified_at":"2026-01-01T00:00:00Z"}`} {
				unsafe(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
					live_run_id=NULL,release_evidence='merged_predecessor_discard',
					merged_predecessor_discard_binding=$2::jsonb WHERE id=$1`, id, binding)
				assertMergedHoldState(t, e, id, "open")
			}
			unsafe(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,
				original_worker_identity,original_worker_id,state,release_evidence,merged_predecessor_discard_binding)
				VALUES($1,$2,$3,$4,9,'fabricated',$5,'discarded','merged_predecessor_discard','{}')`,
				uuid.New(), e.w.UserID, snapshot.binding.RepoID, e.run, e.w.ID)
			if _, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			var audit []byte
			if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", id).Scan(&audit); err != nil {
				t.Fatal(err)
			}
			// A structurally complete receipt cannot authorize a different open hold or a
			// non-discarded state, even on the legacy path.
			otherHold := uuid.New()
			insertMergedHold(e, otherHold, e.w.UserID, snapshot.binding.RepoID, e.run, e.w.ID, 2, guarded)
			unsafe(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
				live_run_id=NULL,release_evidence='merged_predecessor_discard',
				merged_predecessor_discard_binding=$2 WHERE id=$1`, otherHold, audit)
			var full map[string]any
			if err := json.Unmarshal(audit, &full); err != nil {
				t.Fatal(err)
			}
			full["hold_id"], full["generation"] = otherHold.String(), float64(2)
			for _, key := range []string{"hold_id", "generation", "run_id", "owner_id", "repo_id", "connection_id",
				"forge_type", "base_url", "project_id", "mr_iid", "completed_generation", "verified_at"} {
				malformed := make(map[string]any)
				for k, v := range full {
					malformed[k] = v
				}
				delete(malformed, key)
				missing, err := json.Marshal(malformed)
				if err != nil {
					t.Fatal(err)
				}
				unsafe(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
					live_run_id=NULL,release_evidence='merged_predecessor_discard',
					merged_predecessor_discard_binding=$2 WHERE id=$1`, otherHold, missing)
				malformed[key] = "wrong"
				wrong, err := json.Marshal(malformed)
				if err != nil {
					t.Fatal(err)
				}
				unsafe(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
					live_run_id=NULL,release_evidence='merged_predecessor_discard',
					merged_predecessor_discard_binding=$2 WHERE id=$1`, otherHold, wrong)
			}
			complete, err := json.Marshal(full)
			if err != nil {
				t.Fatal(err)
			}
			unsafe(`UPDATE recovery_custody_holds SET release_evidence='merged_predecessor_discard',
				merged_predecessor_discard_binding=$2 WHERE id=$1`, otherHold, complete)
			for _, set := range []string{
				"merged_predecessor_discard_binding=NULL", "merged_predecessor_discard_binding='{}'",
				"merged_predecessor_discard_binding=jsonb_set(merged_predecessor_discard_binding,'{mr_iid}','8')",
				"state='open'", "release_evidence='owner_discard'", "generation=9",
				"original_worker_identity='changed'", "completed_publication_receipt='{}'",
			} {
				unsafe("UPDATE recovery_custody_holds SET "+set+" WHERE id=$1", id)
			}
			unsafe("DELETE FROM recovery_custody_holds WHERE id=$1", id)
		})
	}
}

// Fault injection is scoped to one fixture row. Cleanup removes every installed trigger;
// calls have deadlines and no failure is allowed to strand an operation.
func TestMergedPredecessorRollbackLiveDB(t *testing.T) {
	for _, fault := range []string{"hold_suppression", "capture_error", "capture_suppression"} {
		t.Run(fault, func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			second := uuid.New()
			insertMergedHold(e, second, e.w.UserID, snapshot.binding.RepoID, e.run, e.w.ID, 2, false)
			capture := e.reserve("rollback")
			e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", capture, []byte("partial"))
			table, target, action := "recovery_custody_holds", second, "RETURN NULL;"
			if fault != "hold_suppression" {
				table, target = "recovery_captures", capture
				if fault == "capture_error" {
					action = "RAISE EXCEPTION 'injected capture failure';"
				}
			}
			name := "merged_fault_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			e.exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='" + target.String() + "'::uuid AND NEW.state='discarded' THEN " + action + " END IF; RETURN NEW; END $$")
			e.exec("CREATE TRIGGER " + name + " BEFORE UPDATE ON " + table + " FOR EACH ROW EXECUTE FUNCTION " + name + "()")
			t.Cleanup(func() {
				e.exec("DROP TRIGGER " + name + " ON " + table)
				e.exec("DROP FUNCTION " + name + "()")
			})
			e.svc.SetCustodySettledHook(func(uuid.UUID) { t.Error("hook on rollback") })
			if n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot); n != 0 || err == nil {
				t.Fatalf("fault %s: n=%d err=%v", fault, n, err)
			}
			assertMergedHoldState(t, e, e.hold, "open")
			assertMergedHoldState(t, e, second, "open")
			row, err := mergedCapture(t, e, capture)
			chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, capture)
			if err != nil || row.State != "preparing" || chunkErr != nil || len(chunks) != 1 {
				t.Fatalf("rollback capture=%+v chunks=%+v err=%v/%v", row, chunks, err, chunkErr)
			}
		})
	}
}

func TestMergedPredecessorConcurrentOwnerDiscardLiveDB(t *testing.T) {
	for _, ownerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "merge_first", true: "owner_first"}[ownerFirst], func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			capture := e.reserve("owner-race")
			tx, pid := lockReconcileCapture(t, e, capture)
			var firstPID int32
			var owner <-chan reconcileOperation[bool]
			var merged <-chan reconcileOperation[int64]
			if ownerFirst {
				owner = startReconcileOperation(t, func(ctx context.Context) (bool, error) {
					return e.svc.DiscardHold(ctx, e.w.UserID, e.run, e.hold)
				})
				firstPID = waitReconcileLock(t, e, pid, "DiscardNonReadyCapturesForHold")
				merged = startReconcileOperation(t, func(ctx context.Context) (int64, error) {
					return e.svc.DiscardMergedPredecessorHolds(ctx, snapshot)
				})
				waitReconcileLock(t, e, firstPID, "LockMergedPredecessorHolds")
			} else {
				merged = startReconcileOperation(t, func(ctx context.Context) (int64, error) {
					return e.svc.DiscardMergedPredecessorHolds(ctx, snapshot)
				})
				firstPID = waitReconcileLock(t, e, pid, "LockMergedPredecessorNonReadyCaptures")
				owner = startReconcileOperation(t, func(ctx context.Context) (bool, error) {
					return e.svc.DiscardHold(ctx, e.w.UserID, e.run, e.hold)
				})
				waitReconcileLock(t, e, firstPID, "DiscardCustodyHoldForOwner")
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			ownerResult := awaitReconcileOperation(t, owner)
			mergeResult := awaitReconcileOperation(t, merged)
			if ownerResult.err != nil || mergeResult.err != nil ||
				ownerResult.value != ownerFirst || mergeResult.value != map[bool]int64{true: 0, false: 1}[ownerFirst] {
				t.Fatalf("owner=%+v merge=%+v", ownerResult, mergeResult)
			}
			assertMergedHoldState(t, e, e.hold, "discarded")
		})
	}
}

func TestMergedPredecessorConcurrentReserveLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	e.svc.limits.MaxCapturesPerClaim = 0
	e.svc.limits.MaxCapturesPerOwner = 0
	capture := e.reserve("before-discard")
	tx, pid := lockReconcileCapture(t, e, capture)
	merged := startReconcileOperation(t, func(ctx context.Context) (int64, error) {
		return e.svc.DiscardMergedPredecessorHolds(ctx, snapshot)
	})
	discardPID := waitReconcileLock(t, e, pid, "LockMergedPredecessorNonReadyCaptures")
	reserve := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReserveResponse, error) {
		return e.svc.Reserve(ctx, e.w, e.run, apitypes.RecoveryReserveRequest{
			Generation: &e.gen, IdempotencyKey: "during-discard",
			SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64),
		})
	})
	waitReconcileLock(t, e, discardPID, "ORDER BY created_at DESC LIMIT 1 FOR UPDATE")
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	mergeResult := awaitReconcileOperation(t, merged)
	reserveResult := awaitReconcileOperation(t, reserve)
	if mergeResult.err != nil || mergeResult.value != 1 || !errors.Is(reserveResult.err, ErrNotAuthorized) {
		t.Fatalf("discard=%+v reserve=%+v", mergeResult, reserveResult)
	}
	var count int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM recovery_captures WHERE run_id=$1", e.run).Scan(&count); err != nil {
		t.Fatal(err)
	}
	row, err := mergedCapture(t, e, capture)
	if count != 1 || err != nil || row.State != "discarded" {
		t.Fatalf("reservation entered locked workset: count=%d state=%s err=%v", count, row.State, err)
	}
}

func TestMergedPredecessorConcurrentUploadLiveDB(t *testing.T) {
	for _, uploadFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "discard_first", true: "upload_first"}[uploadFirst], func(t *testing.T) {
			e, snapshot := newMergedPredecessorEnv(t)
			id := e.reserve("upload-race")
			body := []byte("concurrent durable bytes")
			m := manifestOf(body)
			m.ChunkCount = 1
			var upload <-chan reconcileOperation[apitypes.RecoveryCaptureStatusResponse]
			var merged <-chan reconcileOperation[int64]
			if uploadFirst {
				// Upload reaches the reader after admission and stream locks the capture,
				// so it can commit available while discard waits behind it.
				reader := newMergedBlockingReader(t, body)
				upload = startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
					return e.svc.Upload(ctx, e.w, e.run, id, m, reader)
				})
				reader.await(t)
				merged = startReconcileOperation(t, func(ctx context.Context) (int64, error) {
					return e.svc.DiscardMergedPredecessorHolds(ctx, snapshot)
				})
				waitReconcileLock(t, e, 0, "LockMergedPredecessorHolds")
				reader.release()
			} else {
				tx, pid := lockReconcileCapture(t, e, id)
				merged = startReconcileOperation(t, func(ctx context.Context) (int64, error) {
					return e.svc.DiscardMergedPredecessorHolds(ctx, snapshot)
				})
				waitReconcileLock(t, e, pid, "LockMergedPredecessorNonReadyCaptures")
				upload = startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
					return e.svc.Upload(ctx, e.w, e.run, id, m, bytes.NewReader(body))
				})
				waitReconcileLock(t, e, 0, "WHERE h.id=$1 FOR NO KEY UPDATE OF h")
				if err := tx.Commit(e.ctx); err != nil {
					t.Fatal(err)
				}
			}
			mergeResult := awaitReconcileOperation(t, merged)
			uploadResult := awaitReconcileOperation(t, upload)
			if mergeResult.err != nil || mergeResult.value != 1 {
				var detail *pgconn.PgError
				if errors.As(mergeResult.err, &detail) {
					t.Fatalf("discard=%d err=%v detail=%s where=%s", mergeResult.value, mergeResult.err, detail.Detail, detail.Where)
				}
				t.Fatalf("discard=%d err=%v", mergeResult.value, mergeResult.err)
			}
			if uploadFirst {
				if uploadResult.err != nil || uploadResult.value.State != "available" {
					t.Fatalf("upload=%+v", uploadResult)
				}
				e.bytes(id, body)
			} else {
				if !errors.Is(uploadResult.err, ErrNotAuthorized) && !errors.Is(uploadResult.err, ErrNotAvailable) {
					t.Fatalf("late upload=%+v", uploadResult)
				}
				capture, err := mergedCapture(t, e, id)
				chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, id)
				if err != nil || capture.State != "discarded" || chunkErr != nil || len(chunks) != 0 {
					t.Fatalf("discard-first capture=%+v chunks=%+v errors=%v/%v", capture, chunks, err, chunkErr)
				}
			}
			assertMergedHoldState(t, e, e.hold, "discarded")
		})
	}
}

// The reader's wait is bounded; cleanup releases it if an ordering assertion fails.
type mergedBlockingReader struct {
	body    *bytes.Reader
	entered chan struct{}
	proceed chan struct{}
}

func newMergedBlockingReader(t *testing.T, body []byte) *mergedBlockingReader {
	r := &mergedBlockingReader{bytes.NewReader(body), make(chan struct{}, 1), make(chan struct{})}
	t.Cleanup(r.release)
	return r
}
func (r *mergedBlockingReader) release() {
	select {
	case <-r.proceed:
	default:
		close(r.proceed)
	}
}
func (r *mergedBlockingReader) await(t *testing.T) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not enter stream")
	}
}
func (r *mergedBlockingReader) Read(p []byte) (int, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.proceed:
		return r.body.Read(p)
	case <-time.After(5 * time.Second):
		return 0, context.DeadlineExceeded
	}
}
