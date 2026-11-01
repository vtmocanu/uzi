package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

const crossRunTransition = `UPDATE recovery_custody_holds SET state='discarded',
    live_worker_id=NULL,live_run_id=NULL,release_evidence='merged_predecessor_discard',
    merged_predecessor_discard_binding=$2::jsonb WHERE id=$1`

// This fixture supplies database policy identity only. Direct SQL does not
// establish that a forge merge was verified.
func newCrossRunBindingEnv(t *testing.T, targetGen, witnessGen int64) (*inventoryEnv, uuid.UUID, map[string]any) {
	t.Helper()
	e, snapshot := newMergedPredecessorEnv(t)
	b := snapshot.binding
	witness := uuid.New()
	finished, created := "2025-01-02T03:04:05.123456Z", "2025-01-02T03:04:06.654321Z"
	e.exec(`UPDATE runs SET status='cancelled',kind='issue',issue_iid=101,
        claim_generation=$2,mr_iid=NULL,fail_origin=NULL,finished_at=$3 WHERE id=$1`, e.run, targetGen, finished)
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
        status,claim_generation,mr_iid,created_at)
        VALUES($1,$2,$3,'issue',101,'witness','body','completed',$4,7,$5)`,
		witness, b.OwnerID, b.RepoID, witnessGen, created)
	return e, witness, map[string]any{
		"basis": "same_issue_witness", "hold_id": e.hold.String(), "generation": int64(1),
		"target_run_id": e.run.String(), "target_terminal_generation": targetGen, "target_terminal_status": "cancelled",
		"witness_run_id": witness.String(), "witness_completed_generation": witnessGen,
		"owner_id": b.OwnerID.String(), "repo_id": b.RepoID.String(), "issue_iid": int64(101),
		"target_finished_at": finished, "witness_created_at": created,
		"connection_id": b.ConnectionID.String(), "forge_type": b.ForgeType, "base_url": b.BaseURL,
		"project_id": b.ProjectID, "witness_mr_iid": int64(7),
		"verified_at": time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond).Format(time.RFC3339Nano),
	}
}

func crossRunJSON(t *testing.T, b map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func crossRunTx(t *testing.T, e *inventoryEnv, witness uuid.UUID) pgx.Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	t.Cleanup(cancel)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	rows, err := tx.Query(ctx, "SELECT id FROM runs WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE", []uuid.UUID{e.run, witness})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout='5s'"); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestCrossRunBindingAcceptedLiveDB(t *testing.T) {
	for _, gens := range [][2]int64{{1, 1}, {3, 1}} {
		t.Run(fmt.Sprintf("A%d_B%d", gens[0], gens[1]), func(t *testing.T) {
			e, w, b := newCrossRunBindingEnv(t, gens[0], gens[1])
			tx := crossRunTx(t, e, w)
			if _, err := tx.Exec(e.ctx, "SET LOCAL TIME ZONE 'Pacific/Honolulu'"); err != nil {
				t.Fatal(err)
			}
			tag, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, b))
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("transition rows=%d err=%v", tag.RowsAffected(), err)
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			assertMergedHoldState(t, e, e.hold, "discarded")
			e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
			out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
			if err != nil || out == nil || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" || out.FinalReceipt != nil {
				t.Fatalf("exact observation=%+v err=%v", out, err)
			}
			for _, sql := range []string{
				"UPDATE recovery_custody_holds SET merged_predecessor_discard_binding=NULL WHERE id=$1",
				"UPDATE recovery_custody_holds SET original_worker_identity='changed' WHERE id=$1",
				"UPDATE recovery_custody_holds SET state='open' WHERE id=$1",
				"DELETE FROM recovery_custody_holds WHERE id=$1",
			} {
				if _, err := e.pool.Exec(e.ctx, sql, e.hold); err == nil {
					t.Fatalf("audit mutation accepted: %s", sql)
				}
			}
			e.exec("UPDATE recovery_custody_holds SET updated_at=clock_timestamp() WHERE id=$1", e.hold)
		})
	}
}

func TestCrossRunDiscardBindingGuardLiveDB(t *testing.T) {
	e, w, original := newCrossRunBindingEnv(t, 3, 1)
	cases := []struct {
		name, key string
		value     any
		remove    bool
	}{
		{"wrong_basis", "basis", "other", false}, {"missing_basis", "basis", nil, true},
		{"extra", "extra", true, false}, {"schema_version", "schema_version", 1, false},
		{"mode", "mode", "policy", false}, {"policy", "policy", "merged", false},
		{"uuid_type", "hold_id", 1, false}, {"wrong_uuid", "hold_id", uuid.NewString(), false},
		{"uppercase_uuid", "target_run_id", strings.ToUpper(e.run.String()), false},
		{"short_uuid", "owner_id", "not-a-uuid", false}, {"null", "repo_id", nil, false},
		{"status", "target_terminal_status", "completed", false},
		{"future", "verified_at", "2999-01-01T00:00:00Z", false},
	}
	for _, key := range []string{"generation", "target_terminal_generation", "witness_completed_generation", "issue_iid", "project_id", "witness_mr_iid"} {
		for _, v := range []any{"1", 0, -1, json.Number("1.0"), json.Number("1.5"), json.Number("9223372036854775808"), nil, true} {
			cases = append(cases, struct {
				name, key string
				value     any
				remove    bool
			}{fmt.Sprintf("%s_%v", key, v), key, v, false})
		}
	}
	for _, key := range []string{"target_finished_at", "witness_created_at", "verified_at"} {
		for _, v := range []any{"infinity", "now", "2025-01-02T03:04:05", "2025-01-02T03:04:05+00:00", "2025-01-02T03:04:05.1234567Z", "2025-13-02T03:04:05Z", "2025-01-02T24:00:00Z", "2025-01-02T03:60:00Z", 1, nil} {
			cases = append(cases, struct {
				name, key string
				value     any
				remove    bool
			}{fmt.Sprintf("%s_%v", key, v), key, v, false})
		}
	}
	// Every required field must be present, and every string identity must match.
	for key := range original {
		cases = append(cases, struct {
			name, key string
			value     any
			remove    bool
		}{"missing_" + key, key, nil, true})
	}
	for _, key := range []string{"target_run_id", "witness_run_id", "owner_id", "repo_id", "connection_id", "forge_type", "base_url"} {
		cases = append(cases, struct {
			name, key string
			value     any
			remove    bool
		}{"drift_" + key, key, "wrong", false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := make(map[string]any, len(original))
			for k, v := range original {
				b[k] = v
			}
			if tc.remove {
				delete(b, tc.key)
			} else {
				b[tc.key] = tc.value
			}
			tx := crossRunTx(t, e, w)
			if _, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, b)); err == nil {
				if tc.key == "extra" {
					t.Fatal("extra binding key must be rejected")
				}
				t.Fatal("invalid binding accepted")
			}
			_ = tx.Rollback(e.ctx)
			assertMergedHoldState(t, e, e.hold, "open")
		})
	}
}

func TestCrossRunBindingPolicyVetoLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	cases := []struct {
		name, sql string
		args      []any
	}{
		{"timeout", "UPDATE runs SET fail_origin='run_timeout' WHERE id=$1", []any{e.run}},
		{"target_completed", "UPDATE runs SET status='completed' WHERE id=$1", []any{e.run}},
		{"target_running", "UPDATE runs SET status='running' WHERE id=$1", []any{e.run}},
		{"target_zero_issue", "UPDATE runs SET issue_iid=0 WHERE id=$1", []any{e.run}},
		{"target_zero_mr", "UPDATE runs SET mr_iid=0 WHERE id=$1", []any{e.run}},
		{"target_mr", "UPDATE runs SET mr_iid=8 WHERE id=$1", []any{e.run}},
		{"target_kind", "UPDATE runs SET kind='self_improve' WHERE id=$1", []any{e.run}},
		{"target_generation", "UPDATE runs SET claim_generation=0 WHERE id=$1", []any{e.run}},
		{"target_unfinished", "UPDATE runs SET finished_at=NULL WHERE id=$1", []any{e.run}},
		{"target_infinite", "UPDATE runs SET finished_at='infinity' WHERE id=$1", []any{e.run}},
		{"chronology", "UPDATE runs SET created_at='2024-01-01' WHERE id=$1", []any{w}},
		{"witness_kind", "UPDATE runs SET kind='self_improve' WHERE id=$1", []any{w}},
		{"witness_issue", "UPDATE runs SET issue_iid=102 WHERE id=$1", []any{w}},
		{"witness_mr", "UPDATE runs SET mr_iid=NULL WHERE id=$1", []any{w}},
		{"witness_zero_mr", "UPDATE runs SET mr_iid=0 WHERE id=$1", []any{w}},
		{"witness_cancelled", "UPDATE runs SET status='cancelled' WHERE id=$1", []any{w}},
		{"witness_unfinished", "UPDATE runs SET status='running' WHERE id=$1", []any{w}},
		{"witness_generation", "UPDATE runs SET claim_generation=0 WHERE id=$1", []any{w}},
		{"witness_failed", "UPDATE runs SET status='failed' WHERE id=$1", []any{w}},
		{"active_any_generation", `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,snapshot_epoch,reported_at)
            VALUES($1,$2,99,'running',1,now())`, []any{e.w.ID, e.run}},
		{"project", "UPDATE repos SET forge_project_id=2 WHERE id=$1", []any{b["repo_id"]}},
		{"connection_owner", "UPDATE forge_connections SET user_id=$2 WHERE id=$1", []any{b["connection_id"], uuid.Nil}},
	}
	// Use an existing different owner to satisfy the connection FK, while leaving
	// its own connection at a different URL so the owner/url uniqueness is valid.
	other := newInventoryEnv(t)
	other.exec("UPDATE forge_connections SET base_url='https://other-owner.test' WHERE user_id=$1", other.w.UserID)
	cases[len(cases)-1].args[1] = other.w.UserID
	var otherRepo uuid.UUID
	if err := other.pool.QueryRow(e.ctx, "SELECT repo_id FROM runs WHERE id=$1", other.run).Scan(&otherRepo); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct {
		name, sql string
		args      []any
	}{"witness_owner", "UPDATE runs SET user_id=$2 WHERE id=$1", []any{w, other.w.UserID}})
	cases = append(cases, struct {
		name, sql string
		args      []any
	}{"witness_repo", "UPDATE runs SET repo_id=$2 WHERE id=$1", []any{w, otherRepo}})
	cases = append(cases, struct {
		name, sql string
		args      []any
	}{"same_run", "SELECT 1", nil})
	for _, status := range []string{"running", "claimed", "pool_wait", "awaiting_approval", "recovery_wait"} {
		cases = append(cases, struct {
			name, sql string
			args      []any
		}{"consumer_" + status,
			`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status)
            VALUES($1,$2,$3,'issue',101,'consumer','body',$4)`, []any{uuid.New(), e.w.UserID, b["repo_id"], status}})
	}
	cases = append(cases, struct {
		name, sql string
		args      []any
	}{"continued_task",
		`INSERT INTO runs(id,user_id,repo_id,kind,continued_issue_iid,issue_title,issue_description,status,branch)
        VALUES($1,$2,$3,'task',101,'consumer','body','pool_wait','task/consumer')`, []any{uuid.New(), e.w.UserID, b["repo_id"]}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := crossRunTx(t, e, w)
			if _, err := tx.Exec(e.ctx, tc.sql, tc.args...); err != nil {
				t.Fatal(err)
			}
			current := make(map[string]any, len(b))
			for k, v := range b {
				current[k] = v
			}
			// Keep chronology evidence consistent with the now-ineligible rows.
			// Otherwise a stale timestamp alone would hide a missing chronology veto.
			if tc.name == "chronology" {
				current["witness_created_at"] = "2024-01-01T00:00:00Z"
			}
			if tc.name == "same_run" {
				current["witness_run_id"] = e.run.String()
			}
			if tc.name == "target_completed" {
				current["target_terminal_status"] = "completed"
			}
			if tc.name == "target_running" {
				current["target_terminal_status"] = "running"
			}
			if _, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, current)); err == nil {
				t.Fatal("policy veto accepted")
			}
			_ = tx.Rollback(e.ctx)
			assertMergedHoldState(t, e, e.hold, "open")
		})
	}
}

func TestCrossRunBindingAtomicityLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	// Legacy holds can carry prior FINAL; all non-lifecycle columns survive.
	legacy := uuid.New()
	insertMergedHold(e, legacy, e.w.UserID, uuid.MustParse(b["repo_id"].(string)), e.run, e.w.ID, 2, false)
	e.exec(`UPDATE recovery_custody_holds SET final_disposition='settled',
        release_evidence='publication',final_coverage_digest=$2,final_source_sha='prior' WHERE id=$1`, legacy, strings.Repeat("c", 64))
	retained := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(h)-ARRAY['state','live_worker_id','live_run_id',
            'release_evidence','merged_predecessor_discard_binding','updated_at'])::text
            FROM recovery_custody_holds h WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := retained(legacy)
	tx := crossRunTx(t, e, w)
	if _, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, b)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{"basis": "same_issue_witness"}
	if _, err := tx.Exec(e.ctx, crossRunTransition, legacy, crossRunJSON(t, bad)); err == nil {
		t.Fatal("invalid sibling accepted")
	}
	_ = tx.Rollback(e.ctx)
	assertMergedHoldState(t, e, e.hold, "open")
	assertMergedHoldState(t, e, legacy, "open")
	b["hold_id"], b["generation"] = legacy.String(), int64(2)
	tx = crossRunTx(t, e, w)
	if tag, err := tx.Exec(e.ctx, crossRunTransition, legacy, crossRunJSON(t, b)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("legacy transition: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if retained(legacy) != before {
		t.Fatal("prior FINAL or provenance changed")
	}
}

func TestCrossRunBindingPublicationVetoLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	// A closed hold at a later generation is still part of the publication veto.
	for _, closed := range []bool{false, true} {
		for _, linkedByHold := range []bool{false, true} {
			for _, state := range []string{"prepared", "invoked", "create_unknown"} {
				t.Run(fmt.Sprintf("closed_%v_hold_link_%v_%s", closed, linkedByHold, state), func(t *testing.T) {
					tx := crossRunTx(t, e, w)
					hold, pub := uuid.New(), uuid.New()
					run := e.run
					holdRun := w
					if linkedByHold {
						run, holdRun = w, e.run
					}
					if _, err := tx.Exec(e.ctx, `INSERT INTO recovery_custody_holds
                        (id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,
                        live_worker_id,live_run_id,state,inventory_guarded)
                        VALUES($1,$2,$3,$4,99,$5,'publication sibling',$5,$4,'open',false)`,
						hold, e.w.UserID, b["repo_id"], holdRun, e.w.ID); err != nil {
						t.Fatal(err)
					}
					if closed {
						if _, err := tx.Exec(e.ctx, `UPDATE recovery_custody_holds SET state='discarded',
                            live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard' WHERE id=$1`, hold); err != nil {
							t.Fatal(err)
						}
					}
					if tag, err := tx.Exec(e.ctx, `INSERT INTO run_held_publications
                        (id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
                        VALUES($1,$2,99,$3,$4,$5,$6,$2,'refs/uzi-held/'||$2::uuid::text||'/99',$7,$8,'prepared')`,
						pub, run, hold, e.w.UserID, b["repo_id"], e.w.ID, strings.Repeat("a", 40), strings.Repeat("b", 64)); err != nil || tag.RowsAffected() != 1 {
						t.Fatalf("prepare rows=%d err=%v", tag.RowsAffected(), err)
					}
					if state != "prepared" {
						if tag, err := tx.Exec(e.ctx, "UPDATE run_held_publications SET state='invoked',create_invoked_at=now() WHERE id=$1", pub); err != nil || tag.RowsAffected() != 1 {
							t.Fatalf("invoke: %v", err)
						}
					}
					if state == "create_unknown" {
						if tag, err := tx.Exec(e.ctx, "UPDATE run_held_publications SET state='create_unknown' WHERE id=$1", pub); err != nil || tag.RowsAffected() != 1 {
							t.Fatalf("unknown: %v", err)
						}
					}
					if _, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, b)); err == nil {
						t.Fatal("publication veto accepted")
					}
					_ = tx.Rollback(e.ctx)
					assertMergedHoldState(t, e, e.hold, "open")
				})
			}
		}
	}
}

func TestCrossRunBindingLegacySQLLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	b := snapshot.binding
	binding := map[string]any{
		"hold_id": e.hold.String(), "generation": int64(1), "run_id": e.run.String(),
		"owner_id": b.OwnerID.String(), "repo_id": b.RepoID.String(), "connection_id": b.ConnectionID.String(),
		"forge_type": b.ForgeType, "base_url": b.BaseURL, "project_id": b.ProjectID,
		"mr_iid": b.MRIID, "completed_generation": b.CompletedGeneration,
		"verified_at": snapshot.verifiedAt.Format(time.RFC3339Nano),
	}
	if tag, err := e.pool.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, binding)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("legacy SQL: rows=%d err=%v", tag.RowsAffected(), err)
	}
	assertMergedHoldState(t, e, e.hold, "discarded")
}

func TestCrossRunBindingTransitionRestrictionsLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	for _, set := range []string{
		"state='open'", "release_evidence='owner_discard'", "live_worker_id=original_worker_id",
		"live_run_id=run_id", "final_source_sha='changed'", "original_worker_identity='changed'",
		"generation=2", "updated_at=clock_timestamp(),created_at=clock_timestamp()",
	} {
		t.Run(set, func(t *testing.T) {
			tx := crossRunTx(t, e, w)
			// Fixed source fragments above are SQL, never untrusted payloads.
			query := strings.Replace(crossRunTransition, " WHERE id=$1", ", "+set+" WHERE id=$1", 1)
			// Avoid assigning a column twice in SET.
			switch {
			case strings.HasPrefix(set, "state="):
				query = strings.Replace(crossRunTransition, "state='discarded'", set, 1)
			case strings.HasPrefix(set, "release_evidence="):
				query = strings.Replace(crossRunTransition, "release_evidence='merged_predecessor_discard'", set, 1)
			case strings.HasPrefix(set, "live_worker_id="):
				query = strings.Replace(crossRunTransition, "live_worker_id=NULL", set, 1)
			case strings.HasPrefix(set, "live_run_id="):
				query = strings.Replace(crossRunTransition, "live_run_id=NULL", set, 1)
			}
			if tag, err := tx.Exec(e.ctx, query, e.hold, crossRunJSON(t, b)); err == nil && tag.RowsAffected() != 0 {
				t.Fatal("invalid transition accepted")
			}
			_ = tx.Rollback(e.ctx)
			assertMergedHoldState(t, e, e.hold, "open")
		})
	}
	tx := crossRunTx(t, e, w)
	if _, err := tx.Exec(e.ctx, `INSERT INTO recovery_custody_holds
        SELECT (jsonb_populate_record(NULL::recovery_custody_holds,to_jsonb(h) ||
        jsonb_build_object('id',gen_random_uuid(),'state','discarded','live_worker_id',NULL,
        'live_run_id',NULL,'release_evidence','merged_predecessor_discard',
        'merged_predecessor_discard_binding',$2::jsonb))).*
        FROM recovery_custody_holds h WHERE id=$1`, e.hold, crossRunJSON(t, b)); err == nil {
		t.Fatal("audit INSERT accepted")
	}
}

func TestCrossRunBindingZeroUUIDIdentityLiveDB(t *testing.T) {
	crossRunRolloutDatabase(t)
	e, witness, binding := newCrossRunBindingEnv(t, 3, 1)
	repo := uuid.MustParse(binding["repo_id"].(string))
	// An actual zero-ID row must not turn equality with the database snapshot
	// into permission to record an invalid policy identity.
	insertMergedHold(e, uuid.Nil, e.w.UserID, repo, e.run, e.w.ID, 2, true)
	binding["hold_id"], binding["generation"] = uuid.Nil.String(), int64(2)
	tx := crossRunTx(t, e, witness)
	if _, err := tx.Exec(e.ctx, crossRunTransition, uuid.Nil, crossRunJSON(t, binding)); err == nil {
		t.Fatal("zero UUID identity must be rejected even when the database row matches")
	}
	_ = tx.Rollback(e.ctx)
	assertMergedHoldState(t, e, uuid.Nil, "open")
}

func TestCrossRunBindingFailedTargetLiveDB(t *testing.T) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)
	b["target_terminal_status"] = "failed"
	tx := crossRunTx(t, e, w)
	if tag, err := tx.Exec(e.ctx, crossRunTransition, e.hold, crossRunJSON(t, b)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("failed target: rows=%d err=%v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
}
