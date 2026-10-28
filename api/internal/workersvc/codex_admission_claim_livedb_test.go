package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func seedAdmissionCooldown(t *testing.T, e codexTestEnv, alias uuid.UUID) {
	t.Helper()
	e.exec(`INSERT INTO codex_account_admission_throttle (user_id, provider_account_id, rate_expiry, effective_expiry, episode_id, spread_width_ms)
		SELECT user_id, provider_account_id, now()+interval '1 hour', now()+interval '1 hour', gen_random_uuid(), 60000
		FROM codex_credential_state WHERE user_secret_id=$1
		ON CONFLICT (user_id,provider_account_id) DO UPDATE SET effective_expiry=EXCLUDED.effective_expiry`, alias)
}

func admissionClaimCounts(t *testing.T, e codexTestEnv, run uuid.UUID) store.CountOnlineWorkersClaimableForRunRow {
	t.Helper()
	now := time.Now()
	row, err := e.q.CountOnlineWorkersClaimableForRun(e.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: run, HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CodexCuratedModels:       CodexCuratedModels(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Exercise the generated ClaimRun statement, including its custody-opening CTE.
// Every refusal has a positive claim control after expiry, so another gate cannot
// explain the refusal. Resume affinity and prior generations do not bypass admission.
func TestCodexAdmissionClaimKindsLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	for _, kind := range []string{"issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework", "judge"} {
		for _, resume := range []bool{false, true} {
			name := kind + "/fresh"
			if resume {
				name = kind + "/resume"
			}
			t.Run(name, func(t *testing.T) {
				owner, _, repo := e.seedCodexInfra(t)
				alias := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("access"), codexToken("refresh"))
				run := e.seedQueuedCodexRun(t, owner, repo, alias, 1)
				worker := e.seedCapableCodexWorker(t, owner)
				caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1, capability.RecoveryArchiveV1}
				e.exec("UPDATE workers SET protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1", worker, caps)
				var target any
				if kind == "judge" || kind == "mr_rework" {
					target = e.seedQueuedCodexRun(t, owner, repo, alias, 2)
					e.exec("UPDATE runs SET status='completed' WHERE id=$1", target)
				}
				e.exec(`UPDATE runs SET kind=$2,
					issue_iid=CASE WHEN $2 IN ('prompt','task','judge') THEN NULL ELSE issue_iid END,
					repo_id=CASE WHEN $2='judge' THEN NULL ELSE repo_id END,
					branch=CASE WHEN $2='task' THEN 'uzi/task/admission' ELSE NULL END,
					dispatched_at=CASE WHEN $2='task' THEN now() ELSE NULL END,
					pipeline_id=CASE WHEN $2='ci_fix' THEN 1 ELSE NULL END,
					pipeline_ref=CASE WHEN $2 IN ('ci_fix','mr_rework') THEN 'main' ELSE NULL END,
					mr_iid=CASE WHEN $2='mr_rework' THEN 1 ELSE NULL END,
					target_run_id=$3 WHERE id=$1`, run, kind, target)
				if resume {
					e.exec("UPDATE runs SET claim_generation=3, worker_id=$2 WHERE id=$1", run, worker)
				}
				before := mustRun(t, e, run)
				seedAdmissionCooldown(t, e, alias)
				params := e.codexClaimParams(owner, worker, caps)
				params.RecoveryCapable = true
				params.WorkerIdentity = "admission-test"
				_, err := e.q.ClaimRun(e.ctx, params)
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("cooldown claim = %v, want no rows", err)
				}
				after := mustRun(t, e, run)
				if after.Status != "queued" || after.ClaimGeneration != before.ClaimGeneration ||
					after.WorkerID != before.WorkerID || after.ClaimReleasedAt != before.ClaimReleasedAt ||
					!after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
					t.Fatal("admission refusal changed queued state or generation")
				}
				var holds int
				if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", run).Scan(&holds); err != nil {
					t.Fatal(err)
				}
				if holds != 0 {
					t.Fatal("admission refusal opened custody")
				}
				counts := admissionClaimCounts(t, e, run)
				if counts.Claimable != 0 || counts.StrictEligible != 0 || counts.NonDrainingEligible != 0 {
					t.Fatalf("held run counted as available: %+v", counts)
				}
				if counts.DiagnosticSuitable != 1 || counts.ManualSuitable != 1 {
					t.Fatalf("admission corrupted static diagnostics: %+v", counts)
				}
				page, err := e.q.ParkQueuedCodexAccountUnavailablePage(e.ctx, store.ParkQueuedCodexAccountUnavailablePageParams{
					AfterID: uuidPredecessor(run), PageCap: 1,
				})
				if err != nil || len(page.ParkedIds) != 0 || mustRun(t, e, run).Status != "queued" {
					t.Fatalf("cooldown became account-unavailability park: %+v, %v", page, err)
				}
				// Move the entire recovery-spread window behind actual DB time.
				e.exec("UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '2 minutes' WHERE user_id=$1", owner)
				claimed, err := e.q.ClaimRun(e.ctx, params)
				if err != nil || claimed.ID != run || claimed.ClaimGeneration != before.ClaimGeneration+1 {
					t.Fatalf("expired cooldown claim: id=%s gen=%d err=%v", claimed.ID, claimed.ClaimGeneration, err)
				}
				if counts = admissionClaimCounts(t, e, run); counts.Claimable != 1 {
					t.Fatalf("expired run availability = %+v", counts)
				}
			})
		}
	}
}

func TestCodexAdmissionSharedAliasesAndOtherAccountLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	owner, _, repo := e.seedCodexInfra(t)
	a := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("a"), codexToken("ra"))
	b := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("b"), codexToken("rb"))
	other := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("c"), codexToken("rc"))
	// Duplicate import: a distinct saved alias resolves to the same canonical account.
	e.exec(`UPDATE codex_credential_state SET provider_account_id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$2)
		WHERE user_secret_id=$1`, b, a)
	runA := e.seedQueuedCodexRun(t, owner, repo, a, 1)
	runB := e.seedQueuedCodexRun(t, owner, repo, b, 2)
	runOther := e.seedQueuedCodexRun(t, owner, repo, other, 3)
	worker := e.seedCapableCodexWorker(t, owner)
	caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}
	seedAdmissionCooldown(t, e, a)
	// Refresh rotates credential authority without resetting account admission state.
	e.exec("UPDATE codex_provider_account SET generation=generation+1, credential_revision=credential_revision+1 WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)", a)
	claimed, err := e.q.ClaimRun(e.ctx, e.codexClaimParams(owner, worker, caps))
	if err != nil || claimed.ID != runOther {
		t.Fatalf("other account claim = %s, %v", claimed.ID, err)
	}
	for _, id := range []uuid.UUID{runA, runB} {
		if r := mustRun(t, e, id); r.Status != "queued" || r.ClaimGeneration != 0 {
			t.Fatalf("shared account escaped admission: %s %+v", id, r)
		}
	}
	// A new import discovers the same canonical identity while the cooldown is active.
	var identity codexauth.Identity
	if err := e.pool.QueryRow(e.ctx, `SELECT provider_user_id,workspace_account_id FROM codex_provider_account
		WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)`, a).
		Scan(&identity.ProviderUserID, &identity.WorkspaceAccountID); err != nil {
		t.Fatal(err)
	}
	access := codexToken("reimport")
	reimport := e.seedStagingAlias(t, owner, uuid.NewString(), codexLoginBlob{AccessToken: access, RefreshToken: codexToken("rr")})
	fake := newFakeCodexIdentity()
	fake.idByToken[access] = identity
	reconciler := NewCodexReconciler(e.q, e.vlt, e.box, fake, e.pool)
	if err := reconciler.ReconcileCodexAuthIdentity(e.ctx, owner, reimport); err != nil {
		t.Fatal(err)
	}
	reimportRun := e.seedQueuedCodexRun(t, owner, repo, reimport, 4)
	// Reimport/refresh revision changes do not remove the canonical account's row.
	_, err = e.q.ClaimRun(e.ctx, e.codexClaimParams(owner, worker, caps))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("shared alias claim = %v", err)
	}
	if r := mustRun(t, e, reimportRun); r.Status != "queued" || r.ClaimGeneration != 0 {
		t.Fatal("reimport reset admission")
	}
	// Expiry must reopen the shared canonical account, including the reimport.
	e.exec("UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '2 minutes' WHERE user_id=$1", owner)
	remaining := map[uuid.UUID]bool{runA: true, runB: true, reimportRun: true}
	for range 3 {
		claimed, err := e.q.ClaimRun(e.ctx, e.codexClaimParams(owner, worker, caps))
		if err != nil || !remaining[claimed.ID] || claimed.ClaimGeneration != 1 {
			t.Fatalf("expired shared account claim = %s gen=%d, %v", claimed.ID, claimed.ClaimGeneration, err)
		}
		delete(remaining, claimed.ID)
	}
}

func TestCodexAdmissionSubscriptionOnlyLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	for _, mode := range []string{"api_key", "claude"} {
		t.Run(mode, func(t *testing.T) {
			owner, _, repo := e.seedCodexInfra(t)
			sub := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("access"), codexToken("refresh"))
			seedAdmissionCooldown(t, e, sub)
			run := uuid.New()
			e.exec("INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES ($1,$2,$3,'issue',1,'t','d','queued')", run, owner, repo)
			if mode == "api_key" {
				key := e.seedStaticAPIKey(t, owner, uuid.NewString(), codexToken("api-key"))
				svc := &Service{q: e.q, box: e.box}
				if err := svc.FreezeCodexBinding(e.ctx, owner, run, key, codexAuthModeAPIKey); err != nil {
					t.Fatal(err)
				}
			} else {
				e.exec(`UPDATE runs SET harness='claude',codex_secret_id=NULL,codex_auth_mode=NULL,
					codex_secret_label=NULL,codex_material_revision=NULL,codex_account_key=NULL,codex_account_revision=NULL
					WHERE id=$1`, run)
			}
			worker := e.seedCapableCodexWorker(t, owner)
			claimed, err := e.q.ClaimRun(e.ctx, e.codexClaimParams(owner, worker,
				[]string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}))
			if err != nil || claimed.ID != run {
				t.Fatalf("%s claim = %s, %v", mode, claimed.ID, err)
			}
		})
	}
}

// Lock only the claimant worker to pause the actual ClaimRun statement after its
// snapshot starts. Evidence committed meanwhile affects the next statement only.
func TestCodexAdmissionClaimStatementSnapshotLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	owner, worker, repo := e.seedCodexInfra(t)
	alias := e.seedLinkedSubscription(t, owner, uuid.NewString(), codexToken("a"), codexToken("r"))
	run := e.seedQueuedCodexRun(t, owner, repo, alias, 1)
	caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, caps)
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	defer cancel()
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", worker); err != nil {
		t.Fatal(err)
	}
	reader, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(context.Background()) }()
	type result struct {
		run store.Run
		err error
	}
	done := make(chan result, 1)
	finished := false
	defer func() {
		cancel()
		_ = holder.Rollback(context.Background())
		if !finished {
			<-done
		}
	}()
	go func() {
		r, err := e.q.WithTx(reader).ClaimRun(ctx, e.codexClaimParams(owner, worker, caps))
		done <- result{r, err}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := e.pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)", reader.Conn().PgConn().PID()).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-done:
			finished = true
			t.Fatalf("claim finished before barrier: %v", r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	seedAdmissionCooldown(t, e, alias)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	finished = true
	if r.err != nil || r.run.ID != run {
		t.Fatalf("in-flight snapshot claim = %s, %v", r.run.ID, r.err)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = e.q.ClaimRun(ctx, e.codexClaimParams(owner, worker, caps))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("next statement missed evidence: %v", err)
	}
}
