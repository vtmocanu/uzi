package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Snapshot the complete run and attempt log, so a refusal cannot quietly mutate metadata.
func nonClosingSnapshot(t *testing.T, e interlockLiveDB, id uuid.UUID) string {
	t.Helper()
	var snapshot string
	err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object('run',to_jsonb(r),'attempts',
 COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM run_completion_attempts a WHERE a.run_id=r.id),'[]'::jsonb))::text FROM runs r WHERE r.id=$1`, id).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCompletionNonClosingPersistenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	w := store.Worker{ID: wid}
	id := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET milestones_in_progress='["m2"]',scope_ceiling=1,
 budget_max_iterations=17,budget_wall_seconds=3600,budget_paused_seconds=12,
 budget_extension_seconds=60,budget_finalize_seconds=90,
 last_activity_at=now()-interval '10 minutes' WHERE id=$1`, id)
	initial, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if eligible, verifiable := scopeCapEligibility(initial); !eligible || !verifiable {
		t.Fatal("fixture must start eligible for scope-capped delivery")
	}
	var original string
	if err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(r)-ARRAY['completion_non_closing_reason','completion_attempts','latest_completion_attempt','updated_at'])::text FROM runs r WHERE id=$1`, id).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for i, intent := range []*CompletionNonClosing{{Reason: "\x00  first 😀 \x00"}, {Reason: "different"}, nil} {
		res, err := svc.RecordCompletionAttempt(e.ctx, w, id, CompletionAttemptRequest{Head: []string{"h1", "h2", "h3"}[i], NonClosing: intent})
		if err != nil || res.AttemptCount != i+1 || len(res.Unmet) != 1 || res.Unmet[0] != "m2" {
			t.Fatalf("attempt=%+v err=%v", res, err)
		}
		var reason, unchanged string
		var latest []byte
		if err := e.pool.QueryRow(e.ctx, `SELECT completion_non_closing_reason,(to_jsonb(r)-ARRAY['completion_non_closing_reason','completion_attempts','latest_completion_attempt','updated_at'])::text,latest_completion_attempt FROM runs r WHERE id=$1`, id).Scan(&reason, &unchanged, &latest); err != nil {
			t.Fatal(err)
		}
		if reason != "first 😀" || unchanged != original {
			t.Fatalf("reason=%q unrelated state changed=%v", reason, unchanged != original)
		}
		current, err := e.q.GetRunByID(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if eligible, verifiable := scopeCapEligibility(current); !eligible || !verifiable {
			t.Fatal("recording intent changed scope-cap eligibility")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(latest, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["worktree_fingerprint"]) != "null" {
			t.Fatalf("latest attempt must explicitly carry a null fingerprint: %s", latest)
		}
		var summary struct {
			Head                string
			Unmet               []string
			WorktreeFingerprint *string `json:"worktree_fingerprint"`
		}
		if err := json.Unmarshal(latest, &summary); err != nil {
			t.Fatal(err)
		}
		if summary.Head != []string{"h1", "h2", "h3"}[i] || summary.WorktreeFingerprint != nil || len(summary.Unmet) != 1 || summary.Unmet[0] != "m2" {
			t.Fatalf("summary=%s", latest)
		}
	}
	before := nonClosingSnapshot(t, e, id)
	if _, err := svc.RecordCompletionAttempt(e.ctx, w, id, CompletionAttemptRequest{Head: "invalid", NonClosing: &CompletionNonClosing{Reason: "\x00 "}}); err == nil {
		t.Fatal("invalid retry accepted")
	}
	if after := nonClosingSnapshot(t, e, id); after != before {
		t.Fatal("invalid retry changed state")
	}
	// A permit denial is another attempt, with no non-closing declaration.
	if _, err := svc.RequestCompletionPermit(e.ctx, w, id, CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "permit"}); err != nil {
		t.Fatal(err)
	}
	run, err := e.q.GetRunByID(e.ctx, id)
	if err != nil || !run.CompletionNonClosingReason.Valid || run.CompletionNonClosingReason.String != "first 😀" {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func TestCompletionNonClosingRefusalsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	other := e.seedWorker(t, nil)
	for _, tc := range []struct {
		name, sql  string
		worker     uuid.UUID
		generation int64
	}{
		{"stale", `UPDATE runs SET claim_generation=9 WHERE id=$1`, wid, 8},
		{"released", `UPDATE runs SET claim_generation=8,claim_released_at=now() WHERE id=$1`, wid, 8},
		{"other worker", "", other, 0},
		{"not interlocked", `UPDATE runs SET completion_contract_version=NULL WHERE id=$1`, wid, 0},
		{"non issue", `UPDATE runs SET kind='prompt',issue_iid=NULL WHERE id=$1`, wid, 0},
		{"unbound approval", `UPDATE runs SET plan_cross_check_required=true,auto_approve=false,plan_md='plan' WHERE id=$1`, wid, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
			// Prove both NULL and already-stored values survive refusals.
			for _, stored := range []bool{false, true} {
				if stored {
					e.exec(t, `UPDATE runs SET completion_non_closing_reason='original' WHERE id=$1`, id)
				}
				if tc.sql != "" {
					e.exec(t, tc.sql, id)
				}
				before := nonClosingSnapshot(t, e, id)
				var generation *int64
				if tc.generation != 0 {
					g := tc.generation
					generation = &g
				}
				_, err := svc.RecordCompletionAttempt(e.ctx, store.Worker{ID: tc.worker}, id, CompletionAttemptRequest{Head: "refused", ClaimGeneration: generation, NonClosing: &CompletionNonClosing{Reason: "new"}})
				if err == nil {
					t.Fatal("service accepted refusal")
				}
				if nonClosingSnapshot(t, e, id) != before {
					t.Fatal("service refusal changed state")
				}
				// Exercise the actual SQL seam too; Go guards must not hide a missing SQL fence.
				_, err = e.q.RecordCompletionAttempt(e.ctx, store.RecordCompletionAttemptParams{
					RunID: id, WorkerID: pgconv.UUID(tc.worker), ContractRevision: pgtype.Int4{Int32: 1, Valid: true},
					Head: pgconv.TextOrNull("refused"), Unmet: []byte(`["m1.c1"]`),
					CompletionNonClosingReason: pgconv.TextOrNull("new"), ClaimGeneration: pgconv.Int8Ptr(generation),
				})
				if err != pgx.ErrNoRows {
					t.Fatalf("SQL refusal err=%v", err)
				}
				if nonClosingSnapshot(t, e, id) != before {
					t.Fatal("SQL refusal changed state")
				}
			}
		})
	}
}

func TestCompletionNonClosingClaimRestorationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, userID)
	env.exec(`UPDATE workers SET protocol_capabilities=ARRAY['completion_interlock_v1'] WHERE id=$1`, workerID)
	id := seedUnassignedRun(t, env, userID, repoID, 0)
	env.exec(`UPDATE runs SET completion_contract_version=1,contract_revision=1,completion_contract=$2,milestones_frozen=$3 WHERE id=$1`, id, contractJSON(t, "m1"), frozenJSON(t, "m1"))
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})
	w := wkrRow(t, env, workerID)
	payload, err := svc.Claim(env.ctx, w, nil)
	if err != nil || payload == nil || payload.RunID != id.String() {
		t.Fatalf("initial claim=%+v err=%v", payload, err)
	}
	if payload.Config.NonClosing != nil {
		t.Fatal("unset reason projected")
	}
	generation := payload.ClaimGeneration
	env.exec(`UPDATE runs SET status='running' WHERE id=$1`, id)
	if _, err := svc.RecordCompletionAttempt(env.ctx, w, id, CompletionAttemptRequest{ClaimGeneration: &generation, NonClosing: &CompletionNonClosing{Reason: "  later 😀  "}}); err != nil {
		t.Fatal(err)
	}
	// Restore a released run to the queue, as resume does; the new flight must receive the stored intent.
	env.exec(`UPDATE runs SET status='queued',claim_released_at=now(),updated_at=now()-interval '3 hours' WHERE id=$1`, id)
	restored, err := svc.Claim(env.ctx, w, nil)
	if err != nil || restored == nil || restored.RunID != id.String() {
		t.Fatalf("restored claim=%+v err=%v", restored, err)
	}
	if restored.ClaimGeneration <= generation || restored.Config.NonClosing == nil || restored.Config.NonClosing.Reason != "later 😀" {
		t.Fatalf("restored=%+v", restored)
	}
	raw, err := json.Marshal(restored.Config)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["non_closing"]) != `{"reason":"later 😀"}` {
		t.Fatalf("wire=%s", raw)
	}
}
