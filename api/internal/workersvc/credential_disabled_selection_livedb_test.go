package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_selection_livedb_test.go is the live-DB half of PRD #1732 M2b's selection
// rungs (D2/D3/D4/D14): every lane that would open one specific disabled credential parks on
// credential_disabled BEFORE opening or recording it (the pinned per-run override, the pinned
// worker binding, the Judge pin, self-improve through the Judge binding, chat through the
// owner default), the Judge's #1140 fallback spends only the enabled default and parks when
// the slot has none, and a worker that loses its claim loses the D3 exception. Skipped unless
// UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// credentialEpochs counts the run_credential_epochs rows the run's claims recorded: a lane
// that parks before opening its credential records none for that claim.
func credentialEpochs(t *testing.T, env codexTestEnv, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertParkedUnspent asserts the claim parked on credential_disabled with nothing opened or
// recorded: no Anthropic credential stamped on the row and no credential epoch.
func assertParkedUnspent(t *testing.T, env codexTestEnv, runID uuid.UUID) {
	t.Helper()
	r := assertHeld(t, env, runID, true)
	if r.AnthropicSecretID.Valid || r.FailOrigin.Valid {
		t.Fatalf("parked run recorded credential %v (origin %v), want nothing spent", r.AnthropicSecretID, r.FailOrigin)
	}
	if n := credentialEpochs(t, env, runID); n != 0 {
		t.Fatalf("credential epochs = %d, want 0: a parked lane opens nothing", n)
	}
}

// judgeRun seeds a queued judge run reviewing a completed target run of the fixture's repo.
func (fx *cdFix) judgeRun(t *testing.T) uuid.UUID {
	t.Helper()
	target := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	    VALUES ($1, $2, $3, 'issue', 909, 't', 'd', 'completed')`, target, fx.userID, fx.repoID)
	id := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
	    VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3)`, id, fx.userID, target)
	fx.svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	return id
}

// disableAnthropicSlot disables every Anthropic token of the fixture owner and clears the
// default, the state D4 leaves after the slot's last enabled token is disabled.
func (fx *cdFix) disableAnthropicSlot() {
	fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1, is_default = false
	    WHERE user_id = $1 AND kind = 'anthropic_token'`, fx.userID)
}

// enableAsDefault re-enables tok into the empty slot as its default (D4 Enable and make default).
func (fx *cdFix) enableAsDefault(tok uuid.UUID) {
	fx.env.exec(`UPDATE user_secrets SET disabled_at = NULL, enablement_rev = enablement_rev + 1, is_default = true WHERE id = $1`, tok)
}

// TestClaimDisabledPinsParkUnspentLiveDB (D2): a pinned per-run override, a worker pin, and
// self-improve through a pinned Judge binding each park when their one credential is disabled,
// never falling through to the worker binding or the default, and open nothing.
//
// MUTATION: drop the disabled check in openAnthropic and runOverrideChoice; each claim then
// opens and records the disabled token before the finisher parks it, and this test fails.
func TestClaimDisabledPinsParkUnspentLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		setup func(fx *cdFix, runID uuid.UUID)
	}{
		{"pinned run override", "issue", func(fx *cdFix, runID uuid.UUID) {
			// The worker pin (pinTok) stays enabled: the run must not inherit it.
			fx.env.exec(`UPDATE runs SET credential_override_mode = 'pinned', credential_override_secret_id = $2 WHERE id = $1`, runID, fx.otherTok)
			fx.setEnabled(fx.otherTok, false)
		}},
		{"bound worker", "issue", func(fx *cdFix, _ uuid.UUID) { fx.setEnabled(fx.pinTok, false) }},
		{"self-improve pinned Judge", "self_improve", func(fx *cdFix, _ uuid.UUID) {
			fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, fx.otherTok)
			fx.setEnabled(fx.otherTok, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			runID := fx.queuedRun(t, tc.kind)
			tc.setup(fx, runID)
			payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
			}
			assertParkedUnspent(t, fx.env, runID)
		})
	}
}

// TestJudgeClaimDisabledCredentialLiveDB (D2/D4 and the #1140 fallback) on the judge lane itself:
// a disabled Judge pin parks; an auto Judge with no enabled pooled token spends the ENABLED
// default (pool_empty); with every token disabled (no default) the judge parks instead of
// failing, and resumes once a token is enabled as the default.
//
// MUTATION: drop judgeDefaultChoice's parkOnEmptyDefault (or parkIfDefaultSlotDisabled); the
// all-disabled judge then fails terminally with "no Anthropic token configured" and this fails.
func TestJudgeClaimDisabledCredentialLiveDB(t *testing.T) {
	t.Run("disabled pin parks", func(t *testing.T) {
		fx := newCDFix(t)
		judgeID := fx.judgeRun(t)
		fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, fx.otherTok)
		fx.setEnabled(fx.otherTok, false)
		if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
		}
		assertParkedUnspent(t, fx.env, judgeID)
		fx.setEnabled(fx.otherTok, true)
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		assertHeld(t, fx.env, judgeID, false)
	})
	t.Run("empty pool spends the enabled default", func(t *testing.T) {
		fx := newCDFix(t)
		judgeID := fx.judgeRun(t)
		fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
		fx.setEnabled(fx.pinTok, false)
		fx.setEnabled(fx.otherTok, false)
		payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
		if err != nil || payload == nil || payload.RunID != judgeID.String() {
			t.Fatalf("Claim = (%v, %v), want the judge payload", payload != nil, err)
		}
		r := mustRun(t, fx.env, judgeID)
		if uuid.UUID(r.AnthropicSecretID.Bytes) != fx.defTok || r.AnthropicSelectReason.String != "pool_empty" {
			t.Fatalf("judge spent %v (%q), want the enabled default %s (pool_empty)", r.AnthropicSecretID, r.AnthropicSelectReason.String, fx.defTok)
		}
	})
	t.Run("no default parks", func(t *testing.T) {
		fx := newCDFix(t)
		judgeID := fx.judgeRun(t)
		fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
		fx.disableAnthropicSlot()
		if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
		}
		assertParkedUnspent(t, fx.env, judgeID)
		// Still held while the slot has no default.
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		assertHeld(t, fx.env, judgeID, true)
		fx.enableAsDefault(fx.defTok)
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		assertHeld(t, fx.env, judgeID, false)
		payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
		if err != nil || payload == nil || payload.RunID != judgeID.String() {
			t.Fatalf("reclaim = (%v, %v), want the judge payload", payload != nil, err)
		}
	})
	t.Run("no token at all still fails", func(t *testing.T) {
		fx := newCDFix(t)
		judgeID := fx.judgeRun(t)
		fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'default', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
		fx.env.exec(`UPDATE workers SET anthropic_secret_id = NULL WHERE id = $1`, fx.workerID)
		fx.env.exec(`DELETE FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'`, fx.userID)
		if _, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if r := mustRun(t, fx.env, judgeID); r.Status != "failed" {
			t.Fatalf("token-less judge: status=%s hold=%v, want failed as today", r.Status, r.HoldReason)
		}
	})
}

// TestSelfImproveNoDefaultParksLiveDB (D2/D4): self-improve on the Judge default with every
// Anthropic token disabled (no default) parks rather than failing, and an ordinary issue run in
// the same state keeps today's token-less failure (D15: default-based work behaves as for a
// user with no credential).
func TestSelfImproveNoDefaultParksLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'default', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'default', anthropic_secret_id = NULL WHERE id = $1`, fx.workerID)
	fx.disableAnthropicSlot()
	selfImprove := fx.queuedRun(t, "self_improve")
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	assertParkedUnspent(t, fx.env, selfImprove)

	issue := fx.queuedRun(t, "issue")
	if _, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if r := mustRun(t, fx.env, issue); r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" {
		t.Fatalf("default-lane issue run: status=%s origin=%v, want the token-less failure", r.Status, r.FailOrigin)
	}
}

// TestChatCredentialDisabledLiveDB (D2, chat consistent with the other default-only lanes).
// Chat is not bindable: on a worker bound to a disabled token it spends the owner's enabled
// default, never the binding. With every token disabled (no default), or a disabled row still
// flagged default, the chat parks on credential_disabled instead of failing or spending another
// token, and the promoter queues it once an enabled default exists.
//
// MUTATION: route errCredentialDisabled back into recoverClaimAssembly (or drop openAnthropic's
// disabled check); the chat then fails (or opens the disabled default) and this test fails.
func TestChatCredentialDisabledLiveDB(t *testing.T) {
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
	t.Run("bound worker spends the default", func(t *testing.T) {
		fx := newCDFix(t)
		fx.setEnabled(fx.pinTok, false)
		chatID := newChat(t, fx)
		payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload == nil {
			t.Fatalf("ClaimChat = (%v, %v), want a payload", payload != nil, err)
		}
		if r := mustRun(t, fx.env, chatID); uuid.UUID(r.AnthropicSecretID.Bytes) != fx.defTok {
			t.Fatalf("chat spent %v, want the enabled default %s (never the disabled binding)", r.AnthropicSecretID, fx.defTok)
		}
	})
	for _, tc := range []struct {
		name    string
		disable func(fx *cdFix)
	}{
		{"every token disabled", func(fx *cdFix) { fx.disableAnthropicSlot() }},
		{"disabled row still default", func(fx *cdFix) { fx.setEnabled(fx.defTok, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			rec := newReadmitRecorder()
			fx.svc.SetBroadcaster(rec)
			tc.disable(fx)
			chatID := newChat(t, fx)
			payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
			if err != nil || payload != nil {
				t.Fatalf("ClaimChat = (%v, %v), want an idle park", payload != nil, err)
			}
			r := assertHeld(t, fx.env, chatID, true)
			if r.AnthropicSecretID.Valid || r.FailOrigin.Valid || r.WorkerID != pgconv.UUID(fx.workerID) {
				t.Fatalf("parked chat: credential=%v origin=%v worker=%v", r.AnthropicSecretID, r.FailOrigin, r.WorkerID)
			}
			if _, states := rec.published(chatID); len(states) != 1 || states[0] != "paused" {
				t.Fatalf("published states = %v, want [paused]", states)
			}
			fx.svc.RequestCredentialDisabledPromotion(fx.userID)
			assertHeld(t, fx.env, chatID, true)
			fx.enableAsDefault(fx.defTok)
			fx.svc.RequestCredentialDisabledPromotion(fx.userID)
			assertHeld(t, fx.env, chatID, false)
			payload, err = fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
			if err != nil || payload == nil || payload.RunID != chatID.String() {
				t.Fatalf("reclaim = (%v, %v), want the chat payload", payload != nil, err)
			}
		})
	}
}

// TestClaimLostClaimLosesExceptionLiveDB (D3): a flight that already holds its claim keeps
// running when its pinned token is disabled; once the worker loses the claim (requeued on
// re-register), the next claim obeys the disable and parks without opening the token.
//
// MUTATION: drop openAnthropic's disabled check; the re-claim then opens and records the
// disabled token (a second credential epoch) before the finisher parks it, and this fails.
func TestClaimLostClaimLosesExceptionLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim = (%v, %v), want the payload", payload != nil, err)
	}
	fx.env.exec(`UPDATE runs SET status = 'running', status_since = now() WHERE id = $1`, runID)
	fx.setEnabled(fx.pinTok, false)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	if r := mustRun(t, fx.env, runID); r.Status != "running" {
		t.Fatalf("live flight status = %s, want running (D3: an issued claim finishes)", r.Status)
	}
	epochs := credentialEpochs(t, fx.env, runID)

	if _, err := fx.env.q.RequeueWorkerRuns(fx.env.ctx, store.RequeueWorkerRunsParams{
		WorkerID: pgconv.UUID(fx.workerID), MaxRequeues: 5,
	}); err != nil {
		t.Fatalf("RequeueWorkerRuns: %v", err)
	}
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("re-claim = (%v, %v), want an idle park", payload != nil, err)
	}
	assertHeld(t, fx.env, runID, true)
	if n := credentialEpochs(t, fx.env, runID); n != epochs {
		t.Fatalf("credential epochs = %d, want %d: the lost claim's successor opened the disabled token", n, epochs)
	}
}

// TestCodexClaimFrozenAliasDisabledBeforeMintLiveDB (D2/D6): a Codex run whose FROZEN alias is
// disabled parks at claim before any capability is minted, even after the owner handed the
// Codex default to another enabled credential; it never switches to that default (or to Claude).
//
// MUTATION: drop codexClaimSecrets' pre-mint enablement check; the claim then mints a
// capability for the disabled alias (afterMint fires) before the finisher parks it.
func TestCodexClaimFrozenAliasDisabledBeforeMintLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	env.exec(`UPDATE runs SET budget_wall_seconds = 36000 WHERE id = $1`, fx.runID)
	// Default hand-off: the alias is disabled and another enabled Codex credential is default.
	env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1, is_default = false WHERE id = $1`, fx.aliasID)
	handoff := uuid.New()
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	    VALUES ($1, $2, 'openai_api_key', $3, true, $4, 'master')`, handoff, fx.userID, "key-"+handoff.String(), []byte("x"))
	minted := false
	fx.svc.claimHooks = &claimTestHooks{afterMint: func(_ context.Context, _ store.Run) { minted = true }}

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	if minted {
		t.Fatal("a capability was minted for the disabled frozen alias")
	}
	r := assertHeld(t, env, fx.runID, true)
	if uuid.UUID(r.CodexSecretID.Bytes) != fx.aliasID || r.Harness != harnessCodex || r.AnthropicSecretID.Valid || len(r.CodexCapHash) != 0 {
		t.Fatalf("park: alias=%v harness=%s anthropic=%v cap=%x, want the frozen alias kept", r.CodexSecretID, r.Harness, r.AnthropicSecretID, r.CodexCapHash)
	}
	// The hand-off never satisfies the frozen alias: still held.
	fx.svc.SetBackground(func(fn func()) { fn() })
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, env, fx.runID, true)
	env.exec(`UPDATE user_secrets SET disabled_at = NULL WHERE id = $1`, fx.aliasID)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, env, fx.runID, false)
}

// TestSetRunCredentialDisabledRefusedLiveDB (D5): set-token (and the reassignment of a held
// run) onto a disabled token is refused with ErrCredentialDisabled before any write: a queued
// run keeps its override, and a run held on credential_disabled stays held.
//
// MUTATION: drop the meta.Disabled check in validateCredentialOverrideOn; the override is then
// rewritten onto the disabled token (and the held run requeued) and this test fails.
func TestSetRunCredentialDisabledRefusedLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.setEnabled(fx.otherTok, false)
	queued := fx.queuedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET credential_override_mode = 'default' WHERE id = $1`, queued)
	held := fx.parkedRun(t, "issue")
	fx.setEnabled(fx.pinTok, false)
	for _, id := range []uuid.UUID{queued, held} {
		before := mustRun(t, fx.env, id)
		_, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, id, CredentialOverrideModePinned, &fx.otherTok)
		if !errors.Is(err, ErrCredentialDisabled) {
			t.Fatalf("run %s: SetRunCredential err = %v, want ErrCredentialDisabled", id, err)
		}
		after := mustRun(t, fx.env, id)
		if after.Status != before.Status || after.CredentialOverrideMode != before.CredentialOverrideMode ||
			after.CredentialOverrideSecretID != before.CredentialOverrideSecretID || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
			t.Fatalf("run %s changed on a refused set-token: status %s->%s override %v/%v -> %v/%v", id,
				before.Status, after.Status, before.CredentialOverrideMode, before.CredentialOverrideSecretID,
				after.CredentialOverrideMode, after.CredentialOverrideSecretID)
		}
	}
}
