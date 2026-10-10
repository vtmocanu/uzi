package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestCodeCrossCheckStageLocalPinsDeliveryLiveDB(t *testing.T) {
	for _, leadHarness := range []string{"claude", "codex"} {
		t.Run(leadHarness, func(t *testing.T) {
			f := codeFixture(t)
			f.env.exec("UPDATE runs SET harness=$2 WHERE id=$1", f.lead, leadHarness)
			f.env.exec("UPDATE workers SET protocol_capabilities=array_append(array_append(protocol_capabilities,$2),$3) WHERE id=$1", f.workerID, capability.CrossCheckPinsV1, capability.CrossCheckCodexLeadV1)
			f.env.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort)
    VALUES($1,'plan','claude','opus','high'),($1,'plan','codex','gpt-6-astra','xhigh'),
    ($1,'code','claude','haiku','low'),($1,'code','codex','gpt-6-sol','low')`, f.userID)
			cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err != nil {
				t.Fatal(err)
			}
			f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
			p := laneClaim(t, f, "cross_check", nil)
			if p == nil {
				t.Fatal("pinned code checker not delivered")
			}
			recorded, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
			want := "gpt-6-sol"
			if leadHarness == "codex" {
				want = "haiku"
			}
			if err != nil || recorded.CheckerModel.String != want || recorded.CheckerEffort.String != "low" ||
				recorded.CheckerModelSource.String != "pin" || recorded.CheckerEffortSource.String != "pin" {
				t.Fatalf("stage/family pins: %+v %v", recorded, err)
			}
		})
	}
}

func TestCodeCrossCheckInvalidPinNeverFallsBackLiveDB(t *testing.T) {
	for _, checker := range []string{"claude", "codex"} {
		for _, field := range []string{"model", "effort"} {
			t.Run(checker+"/"+field, func(t *testing.T) {
				var f crossCheckContentionFixture
				if checker == "claude" {
					f, _ = newClaudeCodeCheckerFix(t)
				} else {
					f = codeFixture(t)
					f.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,$2) WHERE id=$1", f.workerID, capability.CrossCheckPinsV1)
					cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
					if err != nil {
						t.Fatal(err)
					}
					f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
				}
				if field == "model" {
					invalidModel := "sonnet"
					if checker == "claude" {
						invalidModel = "gpt-6-sol"
					}
					f.env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,'code',$2,$3)", f.userID, checker, invalidModel)
				} else {
					f.env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,'code',$2,'bogus')", f.userID, checker)
				}
				if p := laneClaim(t, f, "cross_check", nil); p != nil {
					t.Fatal("invalid pin fell back to default")
				}
				cc, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
				if err != nil || cc.Outcome.String != "failed" || cc.ReasonClass.String != "checker_unavailable" {
					t.Fatalf("refusal not persisted: %+v %v", cc, err)
				}
			})
		}
	}
}

func TestCodeCrossCheckAbsentPinNoPlanInheritanceLiveDB(t *testing.T) {
	f, _ := newClaudeCodeCheckerFix(t)
	f.env.exec("UPDATE users SET default_claude_model='sonnet',default_effort='medium' WHERE id=$1", f.userID)
	f.env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','claude','haiku','low')", f.userID)
	if p := laneClaim(t, f, "cross_check", nil); p == nil {
		t.Fatal("default code checker not delivered")
	}
	cc, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
	if err != nil || cc.CheckerModel.String != "sonnet" || cc.CheckerEffort.String != "medium" ||
		cc.CheckerModelSource.String != "worker default" || cc.CheckerEffortSource.String != "worker default" {
		t.Fatalf("plan inheritance: %+v %v", cc, err)
	}
}
