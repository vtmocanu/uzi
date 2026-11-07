package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credentials_reset_livedb_test.go is the live-DB half of PRD #3989 M3: after an account reset
// (every credential deleted, users.credentials_reset_at stamped), a run or chat created before the
// reset that finds the owner holding no Anthropic token parks on paused/credentials_reset instead
// of failing, and the credential promoter releases it once a credential exists. A Codex run is
// never parked (D16): it fails terminally with a reason that names the reset. Skipped unless
// UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

const wantCodexResetReason = "the bound Codex login is unavailable after your account reset; start a new run"

// resetAccountCredentials is what an account reset leaves behind: every credential of the owner
// gone (the FKs null the worker, run and judge bindings) and the reset marker stamped at marker
// ("now()" for a reset after the runs were created, or an older instant for one before them).
func resetAccountCredentials(env codexTestEnv, userID uuid.UUID, marker string) {
	env.exec(`DELETE FROM user_secrets WHERE user_id = $1`, userID)
	env.exec(`DELETE FROM codex_provider_account WHERE user_id = $1`, userID)
	env.exec(`UPDATE users SET credentials_reset_at = `+marker+` WHERE id = $1`, userID)
}

// resetAccountCredentialsTx is resetAccountCredentials as vault.Reset commits it: the marker stamp
// and both credential deletes in ONE transaction, so a claim sees the reset either entirely or not
// at all.
func resetAccountCredentialsTx(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Errorf("begin reset: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	for _, stmt := range []string{
		`UPDATE users SET credentials_reset_at = now() WHERE id = $1`,
		`DELETE FROM user_secrets WHERE user_id = $1`,
		`DELETE FROM codex_provider_account WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(env.ctx, stmt, userID); err != nil {
			t.Errorf("reset %q: %v", stmt, err)
			return
		}
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Errorf("commit reset: %v", err)
	}
}

func assertHeldOn(t *testing.T, env codexTestEnv, runID uuid.UUID, reason string) store.Run {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "paused" || !r.HoldReason.Valid || r.HoldReason.String != reason {
		t.Fatalf("run %s: status=%s hold=%v, want paused on %s (origin=%v reason=%v)",
			runID, r.Status, r.HoldReason, reason, r.FailOrigin, r.FailureReason)
	}
	return r
}

func assertFailedCredentialUnavailable(t *testing.T, env codexTestEnv, runID uuid.UUID) store.Run {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" {
		t.Fatalf("run %s: status=%s origin=%v hold=%v, want failed/credential_unavailable", runID, r.Status, r.FailOrigin, r.HoldReason)
	}
	return r
}

// TestClaimCredentialsResetParksClaudeLiveDB: a Claude run created before the reset parks at claim
// (custody released, session and worker kept, nothing recorded as failed), stays parked across
// two promoter sweeps while no credential exists, and is promoted when a credential is created.
//
// MUTATION: remove the credentials_reset branch in finishRunClaimTx; the run then fails
// credential_unavailable and this test fails.
func TestClaimCredentialsResetParksClaudeLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	resetAccountCredentials(fx.env, fx.userID, "now()")

	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	r := assertHeldOn(t, fx.env, runID, "credentials_reset")
	if r.FailOrigin.Valid || r.FailureReason.Valid || !r.ClaimReleasedAt.Valid || r.WorkerID != pgconv.UUID(fx.workerID) ||
		r.SessionID.String != "sess-cd" || !r.StartedAt.Valid || r.AnthropicSecretID.Valid {
		t.Fatalf("park state: origin=%v reason=%v released=%v worker=%v session=%q started=%v credential=%v",
			r.FailOrigin, r.FailureReason, r.ClaimReleasedAt, r.WorkerID, r.SessionID.String, r.StartedAt, r.AnthropicSecretID)
	}
	if n := openCustodyHolds(t, fx.env, runID); n != 0 {
		t.Fatalf("open custody holds = %d, want 0: the park releases the hold this claim opened", n)
	}

	// Two sweeps with no credential: nothing is promoted and the park is not restamped, so a
	// credential-less owner's run cannot churn promote and re-park every pass.
	for i := range 2 {
		// The sweep fallback walks every owner in the shared test database, so only this owner's
		// own pass is counted; the sweep's effect on this run is read from the row below.
		if _, err := fx.svc.promoteAllCredentialDisabledRuns(fx.env.ctx); err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
		if n, err := fx.svc.promoteCredentialDisabledRuns(fx.env.ctx, fx.userID); err != nil || n != 0 {
			t.Fatalf("owner pass %d promoted %d (err %v), want 0", i+1, n, err)
		}
		again := assertHeldOn(t, fx.env, runID, "credentials_reset")
		if !again.StatusSince.Time.Equal(r.StatusSince.Time) || !again.UpdatedAt.Time.Equal(r.UpdatedAt.Time) {
			t.Fatalf("sweep %d rewrote the parked run: status_since %v -> %v", i+1, r.StatusSince, again.StatusSince)
		}
	}

	// Creating a credential (the handler's withSecretLock requests this pass) promotes it.
	fx.env.seedAnthropicSecret(t, fx.userID, "after-reset-"+uuid.NewString(), true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	if got := mustRun(t, fx.env, runID); got.Status != "queued" || got.HoldReason.Valid {
		t.Fatalf("after credential create: status=%s hold=%v, want queued", got.Status, got.HoldReason)
	}
	payload, err = fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload == nil || payload.RunID != runID.String() {
		t.Fatalf("reclaim = (%v, %v), want the run's payload", payload != nil, err)
	}
}

// TestClaimCredentialsResetRepeatParksLiveDB: a credential that disappears between promotion and
// the next claim parks the run again; promotion never causes a terminal failure.
func TestClaimCredentialsResetRepeatParksLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	resetAccountCredentials(fx.env, fx.userID, "now()")
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	assertHeldOn(t, fx.env, runID, "credentials_reset")

	tok := fx.env.seedAnthropicSecret(t, fx.userID, "brief-"+uuid.NewString(), true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	if got := mustRun(t, fx.env, runID); got.Status != "queued" {
		t.Fatalf("status=%s, want queued after the credential appeared", got.Status)
	}
	fx.env.exec(`DELETE FROM user_secrets WHERE id = $1`, tok)

	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle re-park", payload != nil, err)
	}
	r := assertHeldOn(t, fx.env, runID, "credentials_reset")
	if r.FailOrigin.Valid || r.FailureReason.Valid {
		t.Fatalf("re-park recorded a failure: origin=%v reason=%v", r.FailOrigin, r.FailureReason)
	}
}

// TestClaimNeverResetUserStillFailsLiveDB: without a marker, a token-less owner's run fails with
// the unchanged text and origin; the same holds for a run created after the marker.
func TestClaimNeverResetUserStillFailsLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, marker string }{
		{"never reset", "NULL"},
		{"run created after the reset", "now() - interval '1 hour'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			resetAccountCredentials(fx.env, fx.userID, tc.marker)
			runID := fx.queuedRun(t, "issue")
			if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want an idle failure", payload != nil, err)
			}
			r := assertFailedCredentialUnavailable(t, fx.env, runID)
			if r.FailureReason.String != "credential unavailable: no Anthropic token configured for this user" {
				t.Fatalf("failure_reason = %q, want the unchanged token-less text", r.FailureReason.String)
			}
		})
	}
}

// TestSetRunCredentialRefusesCredentialsResetHoldLiveDB: set-token on a run held on
// credentials_reset is refused up front with the named error and writes no override (a resume
// would be refused after the write otherwise).
//
// MUTATION: remove the credentials_reset arm in SetRunCredential's paused case; the override is
// written and the error becomes ErrCredentialSwitchRaced.
func TestSetRunCredentialRefusesCredentialsResetHoldLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credentials_reset',
	    status_since = now() - interval '10 minutes', claim_released_at = now() - interval '10 minutes' WHERE id = $1`, runID)
	before := mustRun(t, fx.env, runID)
	_, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, runID, CredentialOverrideModePinned, &fx.otherTok)
	if !errors.Is(err, ErrCredentialsResetHeld) {
		t.Fatalf("SetRunCredential err = %v, want ErrCredentialsResetHeld", err)
	}
	assertOverrideUnchanged(t, fx.env, before)
	assertHeldOn(t, fx.env, runID, "credentials_reset")
}

// TestClaimCredentialsResetJudgeAndJobStayTerminalLiveDB: judge and job runs are unchanged by the
// reset park; created before the marker and token-less, they still fail with the old text.
//
// MUTATION: drop runkind.Judge or runkind.Job from the guard in finishRunClaimTx; that case parks.
func TestClaimCredentialsResetJudgeAndJobStayTerminalLiveDB(t *testing.T) {
	for _, kind := range []string{"judge", "job"} {
		t.Run(kind, func(t *testing.T) {
			fx := newCDFix(t)
			var runID uuid.UUID
			if kind == "judge" {
				runID = fx.judgeRun(t)
			} else {
				runID = fx.queuedRun(t, "issue")
			}
			resetAccountCredentials(fx.env, fx.userID, "now()")
			// Both kinds reach the finisher with the assembly error a token-less owner produces.
			run, err := fx.env.q.ClaimRun(fx.env.ctx, claimRunParamsFor(fx.worker(t)))
			if err != nil || run.ID != runID {
				t.Fatalf("ClaimRun = (%v, %v), want run %s", run.ID, err, runID)
			}
			if kind == "job" {
				// The run lane does not hand a job to this fixture worker, so claim an issue run and
				// reshape the claimed row into a repo-less job before the finisher sees it.
				// A job opens no custody hold, so drop the one the issue claim opened.
				fx.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id=$1`, runID)
				fx.env.exec(`UPDATE runs SET kind='job', repo_id=NULL, issue_iid=NULL, branch=NULL, job_type='research' WHERE id=$1`, runID)
				run = mustRun(t, fx.env, runID)
			}
			payload, err := fx.svc.finishRunClaim(fx.env.ctx, run, nil, errNoModelCredential,
				claimRecoveryIdentity{workerID: fx.workerID, recoveryCapable: true})
			if payload != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want an idle failure", payload != nil, err)
			}
			r := assertFailedCredentialUnavailable(t, fx.env, runID)
			if r.HoldReason.Valid || r.FailureReason.String != "credential unavailable: no Anthropic token configured for this user" {
				t.Fatalf("hold=%v failure_reason=%q, want the unchanged token-less text", r.HoldReason, r.FailureReason.String)
			}
		})
	}
}

// TestClaimCredentialsResetOtherRefusalsStayTerminalLiveDB: errCredentialUnavailable also covers
// forge-origin refusal, repo-owner mismatch and an undecryptable PAT; only errNoModelCredential
// parks, so a user with a reset marker still fails these terminally with their own reason.
//
// MUTATION: classify on errCredentialUnavailable instead of errNoModelCredential; these park.
func TestClaimCredentialsResetOtherRefusalsStayTerminalLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forge origin refusal", errForgeOriginClaimRefused},
		{"repo owner mismatch", errRunRepoOwnerMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			runID := fx.queuedRun(t, "issue")
			resetAccountCredentials(fx.env, fx.userID, "now()")
			run, err := fx.env.q.ClaimRun(fx.env.ctx, claimRunParamsFor(fx.worker(t)))
			if err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			assembly := fmt.Errorf("%w: %w", errCredentialUnavailable, tc.err)
			payload, err := fx.svc.finishRunClaim(fx.env.ctx, run, nil, assembly,
				claimRecoveryIdentity{workerID: fx.workerID, recoveryCapable: true})
			if payload != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want an idle failure", payload != nil, err)
			}
			r := assertFailedCredentialUnavailable(t, fx.env, runID)
			if r.FailureReason.String != assembly.Error() {
				t.Fatalf("failure_reason = %q, want %q", r.FailureReason.String, assembly.Error())
			}
		})
	}
}

// TestClaimCredentialsResetUndecryptableTokenStaysTerminalLiveDB: a reset marker with an Anthropic
// token the vault cannot open is not "no credential": the run fails with the undecryptable reason.
//
// MUTATION: classify on errCredentialUnavailable instead of errNoModelCredential; this run parks.
func TestClaimCredentialsResetUndecryptableTokenStaysTerminalLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.env.exec(`UPDATE user_secrets SET ciphertext = '\x6e6f742d7365616c65642d62792d746869732d626f78'::bytea
	    WHERE user_id = $1 AND kind = 'anthropic_token'`, fx.userID)
	fx.env.exec(`UPDATE users SET credentials_reset_at = now() WHERE id = $1`, fx.userID)
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle failure", payload != nil, err)
	}
	r := assertFailedCredentialUnavailable(t, fx.env, runID)
	if r.HoldReason.Valid || !strings.Contains(r.FailureReason.String, "could not be decrypted") {
		t.Fatalf("hold=%v failure_reason=%q, want the undecryptable reason", r.HoldReason, r.FailureReason.String)
	}
}

// TestClaimCredentialsResetAutoBoundWorkerPoolWaitsLiveDB: an auto-bound worker with nothing
// pooled keeps today's transient pool_wait; the reset marker does not turn it into a park.
func TestClaimCredentialsResetAutoBoundWorkerPoolWaitsLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	resetAccountCredentials(fx.env, fx.userID, "now()")
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'auto' WHERE id = $1`, fx.workerID)
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle wait", payload != nil, err)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "pool_wait" || r.HoldReason.Valid || r.FailOrigin.Valid {
		t.Fatalf("status=%s hold=%v origin=%v, want pool_wait", r.Status, r.HoldReason, r.FailOrigin)
	}
}

// TestChatCredentialsResetLiveDB: the chat twin. A chat created before the reset parks on
// credentials_reset and the promoter queues it once the owner has a default token; a never-reset
// token-less owner's chat keeps failing.
func TestChatCredentialsResetLiveDB(t *testing.T) {
	newChat := func(t *testing.T, fx *cdFix) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := fx.env.q.CreateChatRun(fx.env.ctx, store.CreateChatRunParams{
			RunID: id, UserID: fx.userID, IssueTitle: "chat", IssueDescription: "hello",
			Title: pgtype.Text{String: "chat", Valid: true},
		}); err != nil {
			t.Fatalf("CreateChatRun: %v", err)
		}
		return id
	}
	t.Run("reset parks and promotes", func(t *testing.T) {
		fx := newCDFix(t)
		rec := newReadmitRecorder()
		fx.svc.SetBroadcaster(rec)
		chatID := newChat(t, fx)
		resetAccountCredentials(fx.env, fx.userID, "now()")
		payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload != nil {
			t.Fatalf("ClaimChat = (%v, %v), want an idle park", payload != nil, err)
		}
		r := assertHeldOn(t, fx.env, chatID, "credentials_reset")
		if r.FailOrigin.Valid || r.WorkerID != pgconv.UUID(fx.workerID) {
			t.Fatalf("parked chat: origin=%v worker=%v", r.FailOrigin, r.WorkerID)
		}
		if _, states := rec.published(chatID); len(states) != 1 || states[0] != "paused" {
			t.Fatalf("published states = %v, want [paused]", states)
		}
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		assertHeldOn(t, fx.env, chatID, "credentials_reset")
		fx.env.seedAnthropicSecret(t, fx.userID, "chat-after-reset-"+uuid.NewString(), true)
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		if got := mustRun(t, fx.env, chatID); got.Status != "queued" || got.HoldReason.Valid {
			t.Fatalf("after credential create: status=%s hold=%v, want queued", got.Status, got.HoldReason)
		}
		payload, err = fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload == nil || payload.RunID != chatID.String() {
			t.Fatalf("reclaim = (%v, %v), want the chat payload", payload != nil, err)
		}
	})
	t.Run("never reset still fails", func(t *testing.T) {
		fx := newCDFix(t)
		chatID := newChat(t, fx)
		resetAccountCredentials(fx.env, fx.userID, "NULL")
		if payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t)); err != nil || payload != nil {
			t.Fatalf("ClaimChat = (%v, %v), want an idle failure", payload != nil, err)
		}
		assertFailedCredentialUnavailable(t, fx.env, chatID)
	})
	t.Run("chat created after the reset still fails", func(t *testing.T) {
		fx := newCDFix(t)
		chatID := newChat(t, fx)
		resetAccountCredentials(fx.env, fx.userID, "now() - interval '1 hour'")
		if payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t)); err != nil || payload != nil {
			t.Fatalf("ClaimChat = (%v, %v), want an idle failure", payload != nil, err)
		}
		assertFailedCredentialUnavailable(t, fx.env, chatID)
	})
}

// TestClaimCredentialsResetCodexFailsWithNamedReasonLiveDB (D16): a Codex run is never parked. A
// reset that lands before assembly, or one that commits after a successful assembly and before
// finishRunClaimTx's lock (classified there by classifyLockedCodexClaim), stores the same exact
// reason with fail_origin credential_unavailable. A never-reset owner's deleted alias, and a run
// created after the marker, keep the existing reason.
//
// MUTATION: remove the Codex classification in finishRunClaimTx's failure arm; the reset cases
// then store the generic reason and fail.
func TestClaimCredentialsResetCodexFailsWithNamedReasonLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		marker    string
		atLock    bool // reset after a successful assembly instead of before it
		wantReset bool
	}{
		{"reset before assembly", "now()", false, true},
		{"reset between assembly and the finish lock", "now()", true, true},
		{"never reset, deleted alias", "NULL", false, false},
		{"run created after the reset", "now() - interval '1 hour'", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			reset := func() { resetAccountCredentials(env, fx.userID, tc.marker) }
			var sawErr error
			hooks := &claimTestHooks{}
			if tc.atLock {
				hooks.afterAssembly = func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					if err != nil || p == nil || p.Secrets.Codex == nil {
						t.Fatalf("assembly = (%v, %v), want a Codex payload before the reset", p != nil, err)
					}
					reset()
				}
			} else {
				hooks.beforeAssembly = func(context.Context, store.Run) { reset() }
				hooks.afterAssembly = func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					sawErr = err
					if p != nil || !errors.Is(err, ErrCodexRunNotBound) {
						t.Fatalf("assembly = (%v, %v), want (nil, ErrCodexRunNotBound)", p != nil, err)
					}
				}
			}
			fx.svc.claimHooks = hooks
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			fx.assertNoClaudeFallback(t, payload)
			if !tc.atLock && sawErr == nil {
				t.Fatal("the assembly hook did not run")
			}
			r := assertFailedCredentialUnavailable(t, env, fx.runID)
			if r.HoldReason.Valid {
				t.Fatalf("a Codex run must never park: hold=%v", r.HoldReason)
			}
			if tc.wantReset {
				if r.FailureReason.String != wantCodexResetReason {
					t.Fatalf("failure_reason = %q, want %q", r.FailureReason.String, wantCodexResetReason)
				}
				return
			}
			if !strings.HasPrefix(r.FailureReason.String, "credential unavailable: ") ||
				!strings.Contains(r.FailureReason.String, "run is not codex-bound") {
				t.Fatalf("failure_reason = %q, want the existing credential-unavailable reason", r.FailureReason.String)
			}
		})
	}
}

// TestClaimCredentialsResetAfterAssemblyParksLiveDB (PRD #3989 review rework): an account reset
// commits (marker, secret deletes and the FK nulling of runs.anthropic_secret_id in one
// transaction) after assembly resolved a token and before finishRunClaimTx's run lock. The token
// must not be delivered: a Claude run parks on credentials_reset, a judge or job fails terminally.
// Without a marker a plain secret delete keeps delivering as before.
//
// MUTATION: remove the resetSeen branch in finishRunClaimTx; the issue case delivers the payload
// with the deleted account's token and the judge and job cases are delivered instead of failing.
func TestClaimCredentialsResetAfterAssemblyParksLiveDB(t *testing.T) {
	t.Run("issue run parks", func(t *testing.T) {
		fx := newCDFix(t)
		runID := fx.queuedRun(t, "issue")
		fired := 0
		fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
			fired++
			if err != nil || p == nil || p.Secrets.AnthropicOAuthToken == "" {
				t.Errorf("assembly = (%v, %v), want a payload carrying a token before the reset", p != nil, err)
			}
			resetAccountCredentialsTx(t, fx.env, fx.userID)
		}}
		payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
		if err != nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want an idle park with no token delivered", payload != nil, err)
		}
		if fired != 1 {
			t.Fatalf("afterAssembly fired %d times, want 1", fired)
		}
		r := assertHeldOn(t, fx.env, runID, "credentials_reset")
		if r.AnthropicSecretID.Valid || r.FailOrigin.Valid || r.FailureReason.Valid {
			t.Fatalf("parked run: credential=%v origin=%v reason=%v", r.AnthropicSecretID, r.FailOrigin, r.FailureReason)
		}
		if n := openCustodyHolds(t, fx.env, runID); n != 0 {
			t.Fatalf("open custody holds = %d, want 0", n)
		}
	})
	for _, kind := range []string{"judge", "job"} {
		t.Run(kind+" fails terminally", func(t *testing.T) {
			fx := newCDFix(t)
			var runID uuid.UUID
			if kind == "judge" {
				runID = fx.judgeRun(t)
			} else {
				runID = fx.queuedRun(t, "issue")
			}
			run, err := fx.env.q.ClaimRun(fx.env.ctx, claimRunParamsFor(fx.worker(t)))
			if err != nil || run.ID != runID {
				t.Fatalf("ClaimRun = (%v, %v), want run %s", run.ID, err, runID)
			}
			if kind == "job" {
				fx.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id=$1`, runID)
				fx.env.exec(`UPDATE runs SET kind='job', repo_id=NULL, issue_iid=NULL, branch=NULL, job_type='research' WHERE id=$1`, runID)
			}
			// Assembly recorded the credential it resolved; the reset then deletes it.
			fx.env.exec(`UPDATE runs SET anthropic_secret_id = $2 WHERE id = $1`, runID, fx.defTok)
			resetAccountCredentialsTx(t, fx.env, fx.userID)
			run = mustRun(t, fx.env, runID)
			if run.AnthropicSecretID.Valid {
				t.Fatalf("anthropic_secret_id = %v, want NULL after the reset", run.AnthropicSecretID)
			}
			payload := &ClaimPayload{RunID: runID.String(), Secrets: ClaimSecrets{
				AnthropicOAuthToken: strings.Join([]string{"fixture", uuid.NewString(), "not", "real"}, "-"),
			}}
			got, err := fx.svc.finishRunClaim(fx.env.ctx, run, payload, nil,
				claimRecoveryIdentity{workerID: fx.workerID, recoveryCapable: true})
			if got != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want an idle failure with no token delivered", got != nil, err)
			}
			assertFailedCredentialUnavailable(t, fx.env, runID)
		})
	}
	t.Run("no marker delivers", func(t *testing.T) {
		fx := newCDFix(t)
		runID := fx.queuedRun(t, "issue")
		fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
			fx.env.exec(`DELETE FROM user_secrets WHERE user_id = $1`, fx.userID)
		}}
		payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
		if err != nil || payload == nil || payload.RunID != runID.String() || payload.Secrets.AnthropicOAuthToken == "" {
			t.Fatalf("Claim = (%v, %v), want the payload with its token (no reset marker)", payload != nil, err)
		}
	})
}

// TestChatCredentialsResetAfterAssemblyParksLiveDB: the chat twin. A reset committing between chat
// assembly and finishChatClaimTx parks the chat instead of delivering the deleted account's token;
// a secret delete without a marker still delivers.
//
// MUTATION: remove the else branch in finishChatClaimTx; the chat is delivered with its token.
func TestChatCredentialsResetAfterAssemblyParksLiveDB(t *testing.T) {
	newChat := func(t *testing.T, fx *cdFix) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := fx.env.q.CreateChatRun(fx.env.ctx, store.CreateChatRunParams{
			RunID: id, UserID: fx.userID, IssueTitle: "chat", IssueDescription: "hello",
			Title: pgtype.Text{String: "chat", Valid: true},
		}); err != nil {
			t.Fatalf("CreateChatRun: %v", err)
		}
		return id
	}
	t.Run("reset parks", func(t *testing.T) {
		fx := newCDFix(t)
		rec := newReadmitRecorder()
		fx.svc.SetBroadcaster(rec)
		chatID := newChat(t, fx)
		fx.svc.claimHooks = &claimTestHooks{afterChatAssembly: func(context.Context, store.Run) {
			resetAccountCredentialsTx(t, fx.env, fx.userID)
		}}
		payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload != nil {
			t.Fatalf("ClaimChat = (%v, %v), want an idle park with no token delivered", payload != nil, err)
		}
		r := assertHeldOn(t, fx.env, chatID, "credentials_reset")
		if r.AnthropicSecretID.Valid {
			t.Fatalf("anthropic_secret_id = %v, want NULL", r.AnthropicSecretID)
		}
		if _, states := rec.published(chatID); len(states) != 1 || states[0] != "paused" {
			t.Fatalf("published states = %v, want [paused]", states)
		}
	})
	t.Run("no marker delivers", func(t *testing.T) {
		fx := newCDFix(t)
		chatID := newChat(t, fx)
		fx.svc.claimHooks = &claimTestHooks{afterChatAssembly: func(context.Context, store.Run) {
			fx.env.exec(`DELETE FROM user_secrets WHERE user_id = $1`, fx.userID)
		}}
		payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload == nil || payload.RunID != chatID.String() {
			t.Fatalf("ClaimChat = (%v, %v), want the chat payload (no reset marker)", payload != nil, err)
		}
	})
}
