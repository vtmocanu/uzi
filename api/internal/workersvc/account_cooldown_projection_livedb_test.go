package workersvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAccountCooldownProjectionAuthorityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct {
		name, sql string
		alias     bool
	}{
		{"current", "", false},
		{"claimed", "UPDATE runs SET status='claimed' WHERE id=$1", false},
		{"running", "UPDATE runs SET status='running' WHERE id=$1", false},
		{"awaiting approval", "UPDATE runs SET status='awaiting_approval' WHERE id=$1", false},
		{"judge advice", "", false},
		{"review advice", "", false},
		{"generation", "UPDATE runs SET claim_generation=2 WHERE id=$1", false},
		{"zero generation", "UPDATE runs SET claim_generation=0 WHERE id=$1", false},
		{"released", "UPDATE runs SET claim_released_at=now() WHERE id=$1", false},
		{"queued", "UPDATE runs SET status='queued' WHERE id=$1", false},
		{"terminal", "UPDATE runs SET status='completed' WHERE id=$1", false},
		{"lease expiry", "UPDATE worker_active_runs SET cooldown_observed_at=now()-interval '46 seconds',cooldown_until=now()-interval '1 second' WHERE run_id=$1", false},
		{"lease extension forged", "UPDATE worker_active_runs SET cooldown_until=now()+interval '2 minutes' WHERE run_id=$1", false},
		{"worker freshness", "UPDATE workers SET last_heartbeat_at=now()-interval '46 seconds' WHERE id=(SELECT worker_id FROM runs WHERE id=$1)", false},
		{"worker offline", "UPDATE workers SET status='offline' WHERE id=(SELECT worker_id FROM runs WHERE id=$1)", false},
		{"disabled", "UPDATE user_secrets SET disabled_at=now() WHERE id=$1", true},
		{"unlinked", "UPDATE codex_credential_state SET status='failed' WHERE user_secret_id=$1", true},
		{"material", "UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", true},
		{"account revision", "UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)", true},
		{"quarantine", "UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)", true},
		{"canonical binding", "UPDATE runs SET codex_account_key='[]' WHERE id=$1", false},
		{"episode", "UPDATE codex_account_admission_throttle SET episode_id=gen_random_uuid() WHERE user_id=(SELECT user_id FROM runs WHERE id=$1)", false},
		{"policy expiry", "UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '1 hour' WHERE user_id=(SELECT user_id FROM runs WHERE id=$1)", false},
		{"optional advice", "UPDATE worker_active_runs SET cooldown_operation=jsonb_set(cooldown_operation,'{kind}','\"advice\"') WHERE run_id=$1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := admissionServiceFixture(t, env)
			env.exec("UPDATE workers SET status='online',last_heartbeat_at=now(),snapshot_register_nonce='m4' WHERE id=$1", f.wkr.ID)
			if err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); err != nil {
				t.Fatal(err)
			}
			req := operationRequest()
			raw, err := json.Marshal(req.CodexOperationIdentity)
			if err != nil {
				t.Fatal(err)
			}
			e := entry(f.runID, 1, "running", false)
			e.AccountCooldown = raw
			if ok, err := applySnapshot(t, snapshotSvc(env, testParams()), env, f.wkr.ID, &ActiveSnapshot{RegisterNonce: "m4", SnapshotEpoch: 1, Active: []ActiveRunEntry{e}}, snapshotModeHeartbeat); err != nil || !ok {
				t.Fatalf("snapshot=%v %v", ok, err)
			}
			read := func(viewer uuid.UUID) []store.ListRunAccountCooldownsRow {
				t.Helper()
				rows, err := env.q.ListRunAccountCooldowns(env.ctx, store.ListRunAccountCooldownsParams{RunIds: []uuid.UUID{f.runID}, ViewerID: viewer})
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			if got := read(f.userID); len(got) != 1 {
				t.Fatal("current positive control missing")
			}
			if got := read(uuid.New()); len(got) != 0 {
				t.Fatal("foreign reader got account fields")
			}
			if tc.name == "review advice" || tc.name == "judge advice" {
				targetID := uuid.New()
				env.exec("INSERT INTO runs (id,user_id,repo_id,kind,status,branch,issue_title,issue_description) SELECT $2::uuid,user_id,repo_id,'task','completed','uzi/task/'||$2::uuid::text,'target','' FROM runs WHERE id=$1", f.runID, targetID)
				if tc.name == "judge advice" {
					env.exec("UPDATE runs SET kind='judge',repo_id=NULL,issue_iid=NULL,branch=NULL,target_run_id=$2 WHERE id=$1", f.runID, targetID)
				} else {
					env.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/task/'||$2::uuid::text,review_target_run_id=$2::uuid WHERE id=$1", f.runID, targetID)
				}
			}
			if tc.sql != "" {
				id := f.runID
				if tc.alias {
					id = f.aliasID
				}
				env.exec(tc.sql, id)
			}
			want := 0
			switch tc.name {
			case "current", "claimed", "running", "awaiting approval":
				want = 1
			case "judge advice", "review advice":
				env.exec("UPDATE worker_active_runs SET cooldown_operation=jsonb_set(cooldown_operation,'{kind}','\"advice\"') WHERE run_id=$1", f.runID)
				want = 1
			}
			if got := read(f.userID); len(got) != want {
				t.Fatalf("projection rows=%d want %d", len(got), want)
			}
		})
	}
}

func TestAccountCooldownProjectionSourceAndExtensionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := admissionServiceFixture(t, env)
	env.exec("UPDATE workers SET status='online',last_heartbeat_at=now(),snapshot_register_nonce='m4' WHERE id=$1", f.wkr.ID)
	if err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); err != nil {
		t.Fatal(err)
	}
	req := operationRequest()
	req.Source = seedOperationSource(t, env, f)
	raw, err := json.Marshal(req.CodexOperationIdentity)
	if err != nil {
		t.Fatal(err)
	}
	e := entry(f.runID, 1, "running", false)
	e.AccountCooldown = raw
	if ok, err := applySnapshot(t, snapshotSvc(env, testParams()), env, f.wkr.ID, &ActiveSnapshot{RegisterNonce: "m4", SnapshotEpoch: 1, Active: []ActiveRunEntry{e}}, snapshotModeHeartbeat); err != nil || !ok {
		t.Fatalf("snapshot=%v %v", ok, err)
	}
	read := func() time.Time {
		t.Helper()
		rows, err := env.q.ListRunAccountCooldowns(env.ctx, store.ListRunAccountCooldownsParams{RunIds: []uuid.UUID{f.runID}, ViewerID: f.userID})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return time.Time{}
		}
		return rows[0].AccountCooldownEligibleAt.Time
	}
	before := read()
	if before.IsZero() {
		t.Fatal("source control missing")
	}
	env.exec("UPDATE codex_account_admission_throttle SET effective_expiry=effective_expiry+interval '1 minute' WHERE user_id=$1", f.userID)
	if after := read(); after.Sub(before) != time.Minute {
		t.Fatalf("extension delta=%s", after.Sub(before))
	}
	env.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", req.Source.RevisionInputID)
	if !read().IsZero() {
		t.Fatal("settled source still projected")
	}
}
