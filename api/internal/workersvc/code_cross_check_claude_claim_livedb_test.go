package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func newClaudeCodeCheckerFix(t *testing.T) (crossCheckContentionFixture, store.CrossCheck) {
	t.Helper()
	f := codeFixture(t)
	f.env.exec("UPDATE runs SET harness='codex' WHERE id=$1", f.lead)
	f.env.exec("UPDATE workers SET protocol_capabilities=array_append(array_append(protocol_capabilities,$2),$3) WHERE id=$1",
		f.workerID, capability.CrossCheckCodexLeadV1, capability.CrossCheckPinsV1)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil || !cc.CheckerRunID.Valid || cc.CheckerHarness.String != "claude" {
		t.Fatalf("submit Claude code checker: %+v %v", cc, err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	return f, cc
}

func TestCodeCrossCheckTimeoutBoundsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		wantSeconds int32
	}{
		{"default", 0, 1800},
		{"positive subsecond", time.Nanosecond, 1},
		{"rounded seconds", time.Second + time.Nanosecond, 2},
		{"maximum", 2 * time.Hour, 7200},
		{"negative", -time.Nanosecond, 0},
		{"above maximum", 2*time.Hour + time.Nanosecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := codeFixture(t)
			f.svc.p.CodeCrossCheckTimeout = tc.timeout
			cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
			if tc.wantSeconds == 0 {
				if !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("invalid timeout accepted: %v", err)
				}
				return
			}
			if err != nil || !cc.CheckerRunID.Valid {
				t.Fatalf("valid timeout refused: %v", err)
			}
			if got := mustRun(t, f.env, uuid.UUID(cc.CheckerRunID.Bytes)).BudgetWallSeconds; !got.Valid || got.Int32 != tc.wantSeconds {
				t.Fatalf("child wall budget = %v, want %d", got, tc.wantSeconds)
			}
		})
	}
}

func TestCodeCrossCheckHealthyClaudeDeliveryLiveDB(t *testing.T) {
	f, cc := newClaudeCodeCheckerFix(t)
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil || p.RunID != f.runID.String() || p.CrossCheck == nil || p.CrossCheck.Stage != "code" ||
		p.CrossCheck.HeadCommit != cc.HeadCommit.String || p.CrossCheck.BaseCommit != cc.BaseCommit.String ||
		p.Secrets.AnthropicOAuthToken == "" || p.Secrets.Codex != nil {
		t.Fatalf("healthy Claude CODE delivery missing or wrong family: %+v", p)
	}
	child := mustRun(t, f.env, f.runID)
	if child.Status != "claimed" || !child.AnthropicSecretID.Valid || child.CodexSecretID.Valid || !child.CrossCheckLane {
		t.Fatalf("Claude child claim identity: %+v", child)
	}
}

func TestCodeCrossCheckClaudeRefusalLiveDB(t *testing.T) {
	for _, refusal := range []string{"disabled credential", "disabled after assembly", "pin unavailable"} {
		t.Run(refusal, func(t *testing.T) {
			f, cc := newClaudeCodeCheckerFix(t)
			disable := func() {
				f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind='anthropic_token'", f.userID)
			}
			switch refusal {
			case "disabled credential":
				disable()
			case "disabled after assembly":
				f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					if err != nil || p == nil {
						t.Fatalf("healthy assembly: payload=%v err=%v", p != nil, err)
					}
					disable()
				}}
			}
			if refusal == "pin unavailable" {
				// Code pin storage is deferred. Drive its refusal sentinel through the
				// production finalizer with the same claimed identity as the delivery probe.
				f.env.exec("UPDATE runs SET status='claimed',claim_generation=1 WHERE id=$1", f.runID)
				run := mustRun(t, f.env, f.runID)
				p, err := f.svc.finishRunClaim(f.env.ctx, run, nil, errCheckerPinUnavailable, claimRecoveryIdentity{workerID: f.workerID})
				if err != nil || p != nil {
					t.Fatalf("pin refusal: payload=%v err=%v", p != nil, err)
				}
			} else if p := laneClaim(t, f, "cross_check", nil); p != nil {
				t.Fatal("unavailable code checker delivered credentials")
			}
			child := mustRun(t, f.env, f.runID)
			if child.Status != "failed" || child.FailOrigin.String != "guardrail_blocked" ||
				!strings.HasPrefix(child.FailureReason.String, "code cross-check: checker unavailable") {
				t.Fatalf("refused child: status=%s origin=%s", child.Status, child.FailOrigin.String)
			}
			decided, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
			if err != nil || decided.Outcome.String != "failed" || decided.ReasonClass.String != "checker_unavailable" ||
				!decided.WaitCredited || string(decided.Findings) != "[]" || decided.HeadCommit != cc.HeadCommit || decided.BaseCommit != cc.BaseCommit {
				t.Fatalf("code refusal evidence: %+v %v", decided, err)
			}
			lead := mustRun(t, f.env, f.lead)
			if lead.Status != "running" || lead.AutoApprove || lead.BudgetPausedSeconds < 1 || lead.BudgetPausedSeconds > 150 {
				t.Fatalf("code refusal changed lead approval or banked wrong wait: status=%s approve=%v paused=%d", lead.Status, lead.AutoApprove, lead.BudgetPausedSeconds)
			}
			var events int
			if err := f.env.pool.QueryRow(f.env.ctx, "SELECT count(*) FROM run_messages WHERE run_id=$1 AND kind='cross_check'", f.lead).Scan(&events); err != nil || events != 0 {
				t.Fatalf("code refusal emitted plan event: events=%d err=%v", events, err)
			}
			if p := laneClaim(t, f, "cross_check", nil); p != nil {
				t.Fatal("failed checker redelivered")
			}
			if got := mustRun(t, f.env, f.lead).BudgetPausedSeconds; got != lead.BudgetPausedSeconds {
				t.Fatal("code wait banked twice")
			}
		})
	}
}
