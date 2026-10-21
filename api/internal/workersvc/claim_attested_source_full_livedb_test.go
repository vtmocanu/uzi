package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFullClaimCarriesAttestedSourceLiveDB pins issue #3194 through the REAL Claim path (not only
// claim assembly): a requeued run whose row holds a persisted checkpoint tip and attestation is
// claimed by its worker, and the claim body re-delivers checkpoint_attested_source exactly; a run
// with no attestation omits the key. Fixture shape follows TestResumeUnapprovedNoSessionClaimLiveDB.
func TestFullClaimCarriesAttestedSourceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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

	for _, stored := range []pgtype.Text{{String: attestS2, Valid: true}, {}} {
		t.Run(map[bool]string{true: "attested", false: "unattested"}[stored.Valid], func(t *testing.T) {
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatalf("exec %q: %v", sql, err)
				}
			}
			svc := New(q, box, testParams())
			svc.SetTxBeginner(pool)
			userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
			sealedPAT, err := box.Seal([]byte("bot-pat-3194-abcdef1234567890"))
			if err != nil {
				t.Fatal(err)
			}
			sealedAnthropic, err := box.Seal([]byte("anthropic-3194-token-abcdef1234567890"))
			if err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("r3194-%s@e2e", userID))
			exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
			exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			      VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/r3194-"+repoID.String(), "https://forge.e2e/g/r3194-"+repoID.String())
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)
			wkrID := uuid.New()
			exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'offline')`,
				wkrID, userID, "w-3194-"+wkrID.String(), wkrID[:])
			if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID, ProtocolCapabilities: []string{capability.InputReceiptsV1}}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			wkr, err := q.GetWorkerByID(ctx, wkrID)
			if err != nil {
				t.Fatalf("GetWorkerByID: %v", err)
			}
			// A seeded --require-base run requeued after a park, its checkpoint and attestation persisted.
			runID := uuid.New()
			exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
			        claim_generation, claim_released_at, auto_approve, plan_source, plan_md, session_id,
			        checkpoint_tip, checkpoint_attested_source)
			      VALUES ($1, $2, $3, 'issue', 3194, 't', 'd', 'queued', $4,
			        1, now(), true, 'seeded', '# Seeded plan', NULL, $5, $6)`, runID, userID, repoID, wkrID, attestTipA, stored)

			payload, err := svc.Claim(ctx, wkr, nil)
			if err != nil || payload == nil || payload.RunID != runID.String() {
				t.Fatalf("svc.Claim = %+v (err %v), want the requeued run %s", payload, err, runID)
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(wire, &m); err != nil {
				t.Fatal(err)
			}
			if got, present := m["checkpoint_attested_source"]; present != stored.Valid || (present && got != stored.String) {
				t.Fatalf("claimed checkpoint_attested_source = %v (present %v), want %+v", got, present, stored)
			}
			if m["checkpoint_tip"] != attestTipA {
				t.Fatalf("claimed checkpoint_tip = %v, want %s", m["checkpoint_tip"], attestTipA)
			}
		})
	}
}
