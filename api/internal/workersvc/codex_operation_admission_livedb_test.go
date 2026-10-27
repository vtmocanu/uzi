package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func operationRequest() CodexOperationRequest {
	return CodexOperationRequest{ClaimGeneration: 1, CodexOperationIdentity: CodexOperationIdentity{OperationID: uuid.NewString(), Kind: "root"}}
}

func operationReadState(t *testing.T, env codexTestEnv, f codexRunFixture) string {
	t.Helper()
	var state string
	if err := env.pool.QueryRow(env.ctx, `SELECT jsonb_build_object(
 'run',(SELECT to_jsonb(r) FROM runs r WHERE id=$1),
 'throttle',(SELECT jsonb_agg(to_jsonb(a)) FROM codex_account_admission_throttle a WHERE user_id=$2),
 'inputs',(SELECT jsonb_agg(to_jsonb(i)) FROM run_user_inputs i WHERE run_id=$1))::text`, f.runID, f.userID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCodexOperationAuthorityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct{ name, sql string }{
		{"released", "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{"stale", "UPDATE runs SET claim_generation=2 WHERE id=$1"},
		{"zero", "UPDATE runs SET claim_generation=0 WHERE id=$1"},
		{"queued", "UPDATE runs SET status='queued' WHERE id=$1"},
		{"invalid account", "UPDATE runs SET codex_account_key='invalid' WHERE id=$1"},
		{"wrong account", "UPDATE runs SET codex_account_key='[]' WHERE id=$1"},
		{"disabled", "UPDATE user_secrets SET disabled_at=now() WHERE id=$1"},
		{"unlinked", "UPDATE codex_credential_state SET status='failed' WHERE user_secret_id=$1"},
		{"material", "UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1"},
		{"kind", "UPDATE user_secrets SET kind='openai_api_key' WHERE id=$1"},
		{"account revision", "UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE user_id=$1"},
		{"account identity", "UPDATE codex_provider_account SET provider_user_id='replacement' WHERE user_id=$1"},
		{"foreign worker", ""}, {"foreign user", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			id := f.runID
			switch tc.name {
			case "disabled", "unlinked", "material", "kind":
				id = f.aliasID
			case "account revision", "account identity":
				id = f.userID
			}
			if tc.sql != "" {
				env.exec(tc.sql, id)
			}
			worker := f.wkr
			if tc.name == "foreign worker" {
				worker.ID = uuid.New()
			}
			if tc.name == "foreign user" {
				worker.UserID = uuid.New()
			}
			before := operationReadState(t, env, f)
			_, err := f.svc.CheckCodexOperationAdmission(env.ctx, worker, f.runID, operationRequest())
			if !errors.Is(err, ErrCodexAdmissionRefused) {
				t.Fatalf("refusal=%v", err)
			}
			if after := operationReadState(t, env, f); after != before {
				t.Fatal("read mutated run/policy/receipt")
			}
		})
	}
	for _, status := range []string{"claimed", "running", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			env.exec("UPDATE runs SET status=$2 WHERE id=$1", f.runID, status)
			before := operationReadState(t, env, f)
			req := operationRequest()
			got, err := f.svc.CheckCodexOperationAdmission(env.ctx, f.wkr, f.runID, req)
			if err != nil || got.Decision != "admit" || got.OperationID != req.OperationID || got.ClaimGeneration != 1 || got.EligibleAt != nil {
				t.Fatalf("no throttle: %+v %v", got, err)
			}
			if operationReadState(t, env, f) != before {
				t.Fatal("read created policy")
			}
		})
	}
}

func TestCodexOperationCooldownLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	req := operationRequest()
	if err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); err != nil {
		t.Fatal(err)
	}
	read := func() CodexOperationDecision {
		t.Helper()
		before := operationReadState(t, env, f)
		got, err := f.svc.CheckCodexOperationAdmission(env.ctx, f.wkr, f.runID, req)
		if err != nil {
			t.Fatal(err)
		}
		if operationReadState(t, env, f) != before {
			t.Fatal("read mutated admission")
		}
		return got
	}
	first := read()
	if first.Decision != "wait" || first.Class != "rate_limit" || first.EpisodeID == "" || first.ServerNow == nil || first.EligibleAt == nil || !first.EligibleAt.After(*first.ServerNow) {
		t.Fatalf("wait=%+v", first)
	}
	env.exec(`UPDATE codex_account_admission_throttle SET rate_expiry=now()+interval '10 minutes',
 effective_expiry=now()+interval '10 minutes',spread_width_ms=30000 WHERE user_id=$1`, f.userID)
	extended := read()
	if !extended.EligibleAt.After(*first.EligibleAt) {
		t.Fatal("extension not seen")
	}
	var expiry time.Time
	if err := env.pool.QueryRow(env.ctx, "SELECT effective_expiry FROM codex_account_admission_throttle WHERE user_id=$1", f.userID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if extended.EligibleAt.Before(expiry) || !extended.EligibleAt.Before(expiry.Add(30*time.Second)) {
		t.Fatal("spread omitted or out of bounds")
	}
	if again := read(); !again.EligibleAt.Equal(*extended.EligibleAt) || again.EpisodeID != extended.EpisodeID {
		t.Fatal("unstable spread")
	}
	env.exec(`UPDATE codex_account_admission_throttle SET overload_expiry=now()+interval '20 minutes',
 effective_expiry=now()+interval '20 minutes' WHERE user_id=$1`, f.userID)
	if got := read(); got.Class != "overload" || !got.EligibleAt.After(*extended.EligibleAt) {
		t.Fatalf("class=%+v", got)
	}
	env.exec(`UPDATE codex_account_admission_throttle SET rate_expiry=now()-interval '1 hour',
 overload_expiry=now()-interval '1 hour',effective_expiry=now()-interval '1 hour' WHERE user_id=$1`, f.userID)
	if got := read(); got.Decision != "admit" || got.EligibleAt != nil || got.Class != "" || got.EpisodeID != "" {
		t.Fatalf("expired=%+v", got)
	}
}

func seedOperationSource(t *testing.T, env codexTestEnv, f codexRunFixture) *CodexOperationSource {
	t.Helper()
	source := &CodexOperationSource{GateRevision: 1, PresentationID: uuid.NewString()}
	env.exec("UPDATE runs SET gate_revision=1,gate_presentation_id=$2 WHERE id=$1", f.runID, source.PresentationID)
	if err := env.pool.QueryRow(env.ctx, `INSERT INTO run_user_inputs
 (run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_worker_id,consumed_claim_generation)
 VALUES($1,'revise_plan','revise','bound',1,now(),$2,1) RETURNING id`, f.runID, f.wkr.ID).Scan(&source.RevisionInputID); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCodexOperationSourcesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"valid", "missing", "presentation", "revision", "receipt", "applied", "unconsumed", "foreign", "generation", "superseded", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			req := operationRequest()
			req.Source = seedOperationSource(t, env, f)
			id := req.Source.RevisionInputID
			switch mode {
			case "missing":
				req.Source = nil
			case "presentation":
				req.Source.PresentationID = uuid.NewString()
			case "revision":
				req.Source.GateRevision++
			case "receipt":
				req.Source.RevisionInputID++
			case "applied":
				env.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", id)
			case "unconsumed":
				env.exec("UPDATE run_user_inputs SET consumed_at=NULL WHERE id=$1", id)
			case "foreign":
				env.exec("UPDATE run_user_inputs SET consumed_worker_id=$2 WHERE id=$1", id, uuid.New())
			case "generation":
				env.exec("UPDATE run_user_inputs SET consumed_claim_generation=2 WHERE id=$1", id)
			case "superseded":
				env.exec("UPDATE run_user_inputs SET disposition='superseded',applied_at=now() WHERE id=$1", id)
			case "unbound":
				env.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", id)
			}
			before := operationReadState(t, env, f)
			_, err := f.svc.CheckCodexOperationAdmission(env.ctx, f.wkr, f.runID, req)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrStaleRevisionSource) {
				t.Fatalf("stale=%v", err)
			}
			if operationReadState(t, env, f) != before {
				t.Fatal("read changed receipt")
			}
		})
	}
}

func TestActiveCooldownSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	svc := snapshotSvc(env, testParams())
	env.exec("UPDATE workers SET snapshot_register_nonce='operation-nonce' WHERE id=$1", f.wkr.ID)
	if err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); err != nil {
		t.Fatal(err)
	}
	req := operationRequest()
	raw, err := json.Marshal(req.CodexOperationIdentity)
	if err != nil {
		t.Fatal(err)
	}
	epoch := int64(0)
	send := func(generation int64, pending bool, metadata json.RawMessage) {
		t.Helper()
		epoch++
		e := entry(f.runID, generation, "running", pending)
		e.AccountCooldown = metadata
		ok, err := applySnapshot(t, svc, env, f.wkr.ID, &ActiveSnapshot{RegisterNonce: "operation-nonce", SnapshotEpoch: epoch, Active: []ActiveRunEntry{e}}, snapshotModeHeartbeat)
		if err != nil || !ok {
			t.Fatalf("snapshot=%v %v", ok, err)
		}
	}
	hasCooldown := func(want bool) {
		t.Helper()
		var actual bool
		err := env.pool.QueryRow(env.ctx, `SELECT cooldown_operation IS NOT NULL AND cooldown_class='rate_limit'
 AND cooldown_episode_id IS NOT NULL AND cooldown_eligible_at>cooldown_observed_at
 AND cooldown_until-cooldown_observed_at=interval '45 seconds'
 FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.wkr.ID, f.runID).Scan(&actual)
		if err != nil || actual != want {
			t.Fatalf("cooldown=%v want=%v err=%v", actual, want, err)
		}
	}
	send(1, false, raw)
	hasCooldown(true)
	// Duplicate epoch and wrong nonce preserve accepted metadata and observed time.
	var before string
	if err := env.pool.QueryRow(env.ctx, "SELECT to_jsonb(a)::text FROM worker_active_runs a WHERE worker_id=$1 AND run_id=$2", f.wkr.ID, f.runID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, snap := range []*ActiveSnapshot{
		{RegisterNonce: "operation-nonce", SnapshotEpoch: epoch, Active: []ActiveRunEntry{entry(f.runID, 1, "running", false)}},
		{RegisterNonce: "wrong", SnapshotEpoch: epoch + 1, Active: []ActiveRunEntry{entry(f.runID, 1, "running", false)}},
	} {
		ok, err := applySnapshot(t, svc, env, f.wkr.ID, snap, snapshotModeHeartbeat)
		if err != nil || ok {
			t.Fatalf("invalid snapshot=%v %v", ok, err)
		}
	}
	var after string
	if err := env.pool.QueryRow(env.ctx, "SELECT to_jsonb(a)::text FROM worker_active_runs a WHERE worker_id=$1 AND run_id=$2", f.wkr.ID, f.runID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("invalid snapshot renewed metadata")
	}
	send(1, false, nil)
	hasCooldown(false)
	for _, gen := range []int64{0, 2} {
		send(gen, true, raw)
		hasCooldown(false)
		row, ok := readActiveRun(t, env, f.wkr.ID, f.runID)
		if !ok || row.gen != gen || !row.untilFuture || !row.terminalPending {
			t.Fatal("legacy custody generation/lease changed")
		}
		send(gen, false, raw)
		hasCooldown(false)
	}
	for _, bad := range []json.RawMessage{json.RawMessage("42"), json.RawMessage(`{"operation_id":false}`), json.RawMessage(`{"operation_id":"bad","kind":"root"}`), json.RawMessage(`{"kind":"unknown"}`)} {
		send(1, true, bad)
		hasCooldown(false)
		row, ok := readActiveRun(t, env, f.wkr.ID, f.runID)
		if !ok || !row.untilFuture {
			t.Fatal("malformed optional metadata invalidated custody")
		}
	}
	req.Source = seedOperationSource(t, env, f)
	send(1, false, raw)
	hasCooldown(false) // Active human source is now required.
	raw, err = json.Marshal(req.CodexOperationIdentity)
	if err != nil {
		t.Fatal(err)
	}
	send(1, false, raw)
	hasCooldown(true)
	env.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", req.Source.RevisionInputID)
	send(1, false, raw)
	hasCooldown(false)
}

func TestCodexOperationLockBudgetLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(env.ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", f.runID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = f.svc.CheckCodexOperationAdmission(env.ctx, f.wkr, f.runID, operationRequest())
	if err == nil || errors.Is(err, ErrCodexAdmissionRefused) {
		t.Fatalf("locked read=%v", err)
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatal("lock exceeded operation budget")
	}
}
