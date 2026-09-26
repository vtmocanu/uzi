package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_budget_livedb_test.go pins how a run held on credential_disabled leaves
// the hold when something besides the credential stands in its way (PRD #1732 D2/D14): the
// untimed lanes (chat, judge, interactive) have no wall and so no spent budget; a timed run
// whose budget is spent settles into budget_exhausted (resumed by Extend) instead of staying
// held; and an owner reassignment settles a pending pause or a spent budget the same way the
// promoter does, never answering "raced". It also pins the chat lane's locked re-check between
// opening the token and delivering it (D1). Skipped unless UZI_TEST_DATABASE_URL is set; run
// via ./e2e/run-store-it.sh.

// staleStart is a started_at far enough back that a timed run on the global timeout has spent
// its whole budget: RunTimeout plus one hour ago.
func staleStart() time.Time { return time.Now().Add(-(testParams().RunTimeout + time.Hour)) }

// holdAs puts an existing run into the credential_disabled park shape (held ten minutes, claim
// released, the fixture worker kept) with started_at at started and no per-run wall, so the
// global timeout decides its budget.
func (fx *cdFix) holdAs(id uuid.UUID, started time.Time) {
	fx.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credential_disabled',
	    status_since = now() - interval '10 minutes', claim_released_at = now() - interval '10 minutes',
	    worker_id = $2, started_at = $3, budget_wall_seconds = NULL WHERE id = $1`, id, fx.workerID, started)
}

func (fx *cdFix) chatRun(t *testing.T) uuid.UUID {
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

// interactiveTask seeds a queued interactive task run (repo, branch, no issue) on the fixture's
// worker, the shape runs_kind_shape requires for kind 'task'.
func (fx *cdFix) interactiveTask(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_title, issue_description,
	    status, status_since, worker_id, started_at, session_id, interactive)
	    VALUES ($1, $2, $3, 'task', 'uzi/task/cd', 't', 'd', 'queued', now(), $4, now(), 'sess-cd', true)`,
		id, fx.userID, fx.repoID, fx.workerID)
	return id
}

// TestCredentialPromoterUntimedRunsIgnoreSpentBudgetLiveDB (item 1): a held judge, chat and
// interactive task whose started_at is older than the whole global timeout are promoted on
// Enable: they have no wall (RequestWallParks' exclusions), so the spent-budget guard must not
// strand them. A timed issue row with the same started_at does NOT reach the queue: it settles
// into budget_exhausted, and the owner's Extend then resumes it (item 3).
//
// MUTATION: drop "kind IN ('chat', 'judge') OR interactive OR" from PromoteCredentialDisabledRun;
// the three untimed runs then stay held (or, with the settle, land in budget_exhausted) and this
// test fails. MUTATION: drop the SettleCredentialDisabledSpentBudget fallback in
// releaseCredentialDisabledHold; the timed issue then stays held on credential_disabled forever.
func TestCredentialPromoterUntimedRunsIgnoreSpentBudgetLiveDB(t *testing.T) {
	fx := newCDFix(t)
	stale := staleStart()
	judge := fx.judgeRun(t)
	chat := fx.chatRun(t)
	task := fx.interactiveTask(t)
	timed := fx.queuedRun(t, "issue")
	for _, id := range []uuid.UUID{judge, chat, task, timed} {
		fx.holdAs(id, stale)
	}
	// Every requirement disabled first (the whole Anthropic slot, D4): nothing leaves the hold.
	fx.disableAnthropicSlot()
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	for _, id := range []uuid.UUID{judge, chat, task, timed} {
		assertHeld(t, fx.env, id, true)
	}

	fx.setEnabled(fx.pinTok, true)
	fx.enableAsDefault(fx.defTok)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	for name, id := range map[string]uuid.UUID{"judge": judge, "chat": chat, "interactive task": task} {
		if r := mustRun(t, fx.env, id); r.Status != "queued" || r.HoldReason.Valid {
			t.Fatalf("%s: status=%s hold=%v, want queued (an untimed run has no spent budget)", name, r.Status, r.HoldReason)
		}
	}
	r := mustRun(t, fx.env, timed)
	if r.Status != "paused" || r.HoldReason.String != "budget_exhausted" {
		t.Fatalf("timed issue: status=%s hold=%v, want paused on budget_exhausted", r.Status, r.HoldReason)
	}
	if !r.ClaimReleasedAt.Valid || r.WorkerID != pgconv.UUID(fx.workerID) {
		t.Fatalf("timed issue: claim_released=%v worker=%v, want the server-park shape kept", r.ClaimReleasedAt, r.WorkerID)
	}
	// A second pass leaves it alone: it is no longer this hold.
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	if r := mustRun(t, fx.env, timed); r.HoldReason.String != "budget_exhausted" {
		t.Fatalf("second pass moved the settled run: hold=%v", r.HoldReason)
	}
	// The existing Extend path resumes it.
	if _, err := fx.env.q.ExtendAndResumeWallPark(fx.env.ctx, store.ExtendAndResumeWallParkParams{
		Secs: 3600, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
		ID: timed, UserID: fx.userID, Cap: 86400,
	}); err != nil {
		t.Fatalf("ExtendAndResumeWallPark: %v", err)
	}
	if r := mustRun(t, fx.env, timed); r.Status != "queued" || r.HoldReason.Valid {
		t.Fatalf("extended: status=%s hold=%v, want queued", r.Status, r.HoldReason)
	}
}

// TestReassignHeldRunSettlesLiveDB (items 1 and 4): the owner's reassignment of a held run is
// never refused as "raced" for a reason that is not a race. An interactive task past the whole
// global timeout is untimed and goes straight back to the queue on the new token. A timed run
// whose budget is spent settles into budget_exhausted, and a run carrying a pending pause lands
// in that pause (an owner request: the ordinary owner pause; a wall request: budget_exhausted),
// each with the new override written, exactly as the promoter would settle it.
//
// MUTATION: restore the single-statement reassignment (ReassignCredentialDisabledRun alone, 0
// rows mapped to ErrCredentialSwitchRaced); every settle case then returns
// ErrCredentialSwitchRaced with nothing written and this test fails.
func TestReassignHeldRunSettlesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seed       func(t *testing.T, fx *cdFix) uuid.UUID
		wantStatus string
		wantHold   string
	}{
		{"untimed interactive task past the global timeout", func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.interactiveTask(t)
			fx.holdAs(id, staleStart())
			return id
		}, "queued", ""},
		{"timed run with a spent budget", func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.queuedRun(t, "issue")
			fx.holdAs(id, staleStart())
			return id
		}, "paused", "budget_exhausted"},
		{"pending owner pause", func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.parkedRun(t, "issue")
			fx.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'now' WHERE id = $1`, id)
			fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'now')`, id)
			return id
		}, "paused", ""},
		{"pending wall pause", func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.parkedRun(t, "issue")
			fx.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'wall' WHERE id = $1`, id)
			fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'wall')`, id)
			return id
		}, "paused", "budget_exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			fx.setEnabled(fx.pinTok, false)
			id := tc.seed(t, fx)
			res, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, id, CredentialOverrideModePinned, &fx.otherTok)
			if err != nil {
				t.Fatalf("SetRunCredential: %v (want the hold settled, never ErrCredentialSwitchRaced)", err)
			}
			r := mustRun(t, fx.env, id)
			if res.Run.Status != r.Status || r.Status != tc.wantStatus || r.HoldReason.String != tc.wantHold {
				t.Fatalf("status=%s (result %s) hold=%q, want %s hold=%q", r.Status, res.Run.Status, r.HoldReason.String, tc.wantStatus, tc.wantHold)
			}
			if r.CredentialOverrideMode.String != CredentialOverrideModePinned || uuid.UUID(r.CredentialOverrideSecretID.Bytes) != fx.otherTok {
				t.Fatalf("override = %v/%v, want pinned %s", r.CredentialOverrideMode, r.CredentialOverrideSecretID, fx.otherTok)
			}
			if r.PauseRequestedAt.Valid || r.PauseMode.Valid {
				t.Fatalf("pending pause left behind: at=%v mode=%v", r.PauseRequestedAt, r.PauseMode)
			}
			var unapplied int
			if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_user_inputs
			    WHERE run_id = $1 AND kind = 'pause' AND applied_at IS NULL`, id).Scan(&unapplied); err != nil {
				t.Fatal(err)
			}
			if unapplied != 0 {
				t.Fatalf("unapplied pause inputs = %d, want 0", unapplied)
			}
			if tc.name == "pending owner pause" {
				// The owner's later resume keeps worker affinity, as for any owner pause.
				if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
					ID: id, UserID: fx.userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
				}); err != nil {
					t.Fatalf("ResumePausedRun: %v", err)
				}
				if r := mustRun(t, fx.env, id); r.Status != "queued" || r.WorkerID != pgconv.UUID(fx.workerID) {
					t.Fatalf("resumed: status=%s worker=%v, want queued on the parking worker %s", r.Status, r.WorkerID, fx.workerID)
				}
			}
		})
	}
}

// TestChatDisableBetweenOpenAndDeliveryParksLiveDB (item 5, D1): a disable that commits after
// assembleChatClaim opened the owner's default token but before the claim is delivered parks
// the chat on credential_disabled with no payload, exactly as the run lane's locked re-check
// does. A concurrent writer holding the credential row makes the claim give up (bounded NOWAIT
// retries) without blocking and without delivering.
//
// MUTATION: return the assembled payload from ClaimChat without finishChatClaim; the first
// case then delivers the disabled token and this test fails.
func TestChatDisableBetweenOpenAndDeliveryParksLiveDB(t *testing.T) {
	t.Run("disable commits before delivery", func(t *testing.T) {
		fx := newCDFix(t)
		rec := newReadmitRecorder()
		fx.svc.SetBroadcaster(rec)
		chat := fx.chatRun(t)
		fx.svc.claimHooks = &claimTestHooks{afterChatAssembly: func(context.Context, store.Run) {
			fx.setEnabled(fx.defTok, false)
		}}
		payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload != nil {
			t.Fatalf("ClaimChat = (%v, %v), want an idle park (no disabled token delivered)", payload != nil, err)
		}
		r := assertHeld(t, fx.env, chat, true)
		if r.WorkerID != pgconv.UUID(fx.workerID) {
			t.Fatalf("parked chat worker = %v, want %s kept as affinity", r.WorkerID, fx.workerID)
		}
		if _, states := rec.published(chat); len(states) != 1 || states[0] != "paused" {
			t.Fatalf("published states = %v, want [paused]", states)
		}
		fx.svc.claimHooks = nil
		fx.setEnabled(fx.defTok, true)
		fx.svc.RequestCredentialDisabledPromotion(fx.userID)
		payload, err = fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
		if err != nil || payload == nil || payload.RunID != chat.String() {
			t.Fatalf("reclaim after enable = (%v, %v), want the chat payload", payload != nil, err)
		}
	})
	t.Run("credential row held by a writer", func(t *testing.T) {
		fx := newCDFix(t)
		chat := fx.chatRun(t)
		tx, err := fx.env.pool.Begin(fx.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.env.ctx) }()
		fx.svc.claimHooks = &claimTestHooks{afterChatAssembly: func(context.Context, store.Run) {
			if _, err := tx.Exec(fx.env.ctx, `SELECT 1 FROM user_secrets WHERE id = $1 FOR UPDATE`, fx.defTok); err != nil {
				t.Errorf("lock credential row: %v", err)
			}
		}}
		ctx, cancel := context.WithTimeout(fx.env.ctx, 10*time.Second)
		defer cancel()
		payload, err := fx.svc.ClaimChat(ctx, fx.worker(t))
		if payload != nil || err == nil || !isLockNotAvailable(err) {
			t.Fatalf("ClaimChat = (%v, %v), want no payload and the 55P03 after the bounded retries", payload != nil, err)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("ClaimChat blocked on the held credential row")
		}
		if r := mustRun(t, fx.env, chat); r.Status != "claimed" {
			t.Fatalf("chat status = %s, want still claimed (nothing written)", r.Status)
		}
	})
}

// TestJudgeOptInAndBindingAllOrNothingLiveDB (item 7, D5): the Judge PUT's opt-in and binding
// are one transaction under the secret mutation lock. A binding onto a disabled token is refused
// with judge_enabled unchanged in the database; onto an enabled token both halves commit.
//
// MUTATION: write the opt-in before the locked enablement check (or commit it separately); the
// refused request then leaves judge_enabled flipped and this test fails.
func TestJudgeOptInAndBindingAllOrNothingLiveDB(t *testing.T) {
	fx := newCDFix(t)
	judgeEnabled := func() bool {
		t.Helper()
		var on bool
		if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT judge_enabled FROM users WHERE id = $1`, fx.userID).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	fx.env.exec(`UPDATE users SET judge_enabled = false WHERE id = $1`, fx.userID)
	fx.setEnabled(fx.otherTok, false)
	on := true
	if _, err := fx.svc.SetUserJudgeBinding(fx.env.ctx, fx.userID, BindModePinned, &fx.otherTok, &on); !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("SetUserJudgeBinding onto a disabled token = %v, want ErrCredentialDisabled", err)
	}
	if judgeEnabled() {
		t.Fatal("a refused Judge binding left judge_enabled flipped")
	}
	fx.setEnabled(fx.otherTok, true)
	u, err := fx.svc.SetUserJudgeBinding(fx.env.ctx, fx.userID, BindModePinned, &fx.otherTok, &on)
	if err != nil {
		t.Fatalf("SetUserJudgeBinding onto an enabled token: %v", err)
	}
	if !judgeEnabled() || uuid.UUID(u.JudgeAnthropicSecretID.Bytes) != fx.otherTok {
		t.Fatalf("enabled=%t binding=%v, want both halves committed", judgeEnabled(), u.JudgeAnthropicSecretID)
	}
}
