package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumeUnapprovedNoSessionClaimLiveDB pins the claim a credential switch hands the next
// flight of a run parked at the plan gate with a pending revise (issue #1604): the persisted,
// unapproved agent plan and a NULL session_id. The resume-relevant payload fields must equal
// fixtures/claims/resume-unapproved-no-session.json, which the agent's interruption test builds
// its claim from, and the revise must still replay on the new claim's GET /inputs so the
// resumed flight applies it instead of losing it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestResumeUnapprovedNoSessionClaimLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	raw, err := os.ReadFile("../../../fixtures/claims/resume-unapproved-no-session.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	planMd, ok := fixture["plan_md"].(string)
	if !ok || planMd == "" {
		t.Fatalf("fixture plan_md = %v, want a non-empty string", fixture["plan_md"])
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
	box := newBox(t)
	q := store.New(pool)
	svc := New(q, box, testParams())
	svc.SetTxBeginner(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	sealedPAT, err := box.Seal([]byte("bot-pat-1604-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedAnthropic, err := box.Seal([]byte("anthropic-1604-token-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("r1604-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r1604', 'https://forge.e2e/g/r1604', 'main', true)`, repoID, connID)
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)

	wkrID := uuid.New()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-1604', $3, 'offline')`,
		wkrID, userID, wkrID[:])
	// Receipt-capable, so GET /inputs is a read-only replay the worker must ACK and apply.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID, ProtocolCapabilities: []string{capability.InputReceiptsV1}}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	wkr, err := q.GetWorkerByID(ctx, wkrID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}

	// The run as a credential switch released it from the plan gate: the unapproved agent plan
	// persisted, no session recorded, generation 1's claim released and the run requeued, and
	// the owner's revise ACKed by generation 1 but never applied.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
	        claim_generation, claim_released_at, credential_switch_requested_at, credential_switch_generation,
	        auto_approve, plan_source, plan_md, session_id)
	      VALUES ($1, $2, $3, 'issue', 1604, 't', 'd', 'queued', $4,
	        1, now(), now(), 1,
	        false, 'agent', $5, NULL)`, runID, userID, repoID, wkrID, planMd)
	var revise int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, consumed_claim_generation, consumed_worker_id)
	      VALUES ($1, 'revise_plan', 'split milestone two', now(), 1, $2) RETURNING id`, runID, wkrID).Scan(&revise); err != nil {
		t.Fatalf("insert revise: %v", err)
	}

	payload, err := svc.Claim(ctx, wkr, nil)
	if err != nil {
		t.Fatalf("svc.Claim: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() {
		t.Fatalf("svc.Claim = %+v, want the requeued run %s", payload, runID)
	}
	if payload.ClaimGeneration != 2 {
		t.Fatalf("claim generation = %d, want 2", payload.ClaimGeneration)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var claim map[string]any
	if err := json.Unmarshal(wire, &claim); err != nil {
		t.Fatal(err)
	}
	if phase, present := claim["resume_phase"]; present {
		t.Fatalf("resume_phase = %v, want absent: an unapproved plan without a session re-plans", phase)
	}
	for key, want := range fixture {
		if key == "_comment" {
			continue
		}
		got, present := claim[key]
		if !present {
			t.Fatalf("claim lacks %q; the fixture carries %v", key, want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("claim %s = %#v, fixture %#v", key, got, want)
		}
	}

	// The new claim's GET /inputs replays the revise (read-only, receipt mode), not a
	// credential-switch signal: the lingering switch stamp belongs to generation 1.
	res, err := svc.ConsumeInputs(ctx, wkr, runID)
	if err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
	if res.CredentialSwitch != nil || !res.Receipts {
		t.Fatalf("GET /inputs = %+v, want a receipt-mode replay with no switch signal", res)
	}
	if len(res.Inputs) != 1 || res.Inputs[0].ID != revise || res.Inputs[0].Kind != "revise_plan" {
		t.Fatalf("GET /inputs replayed %+v, want only the revise %d", res.Inputs, revise)
	}
	if res.Inputs[0].Body == nil || *res.Inputs[0].Body != "split milestone two" {
		t.Fatalf("replayed revise body = %v", res.Inputs[0].Body)
	}
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE id = $1`, revise).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("the replay applied the revise; only the worker's APPLIED may")
	}
}
