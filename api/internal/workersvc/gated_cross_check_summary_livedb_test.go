package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

type seededRound struct {
	round    int32
	gen      int64
	verdict  string
	plan     string
	findings string
	pending  bool
}

// seedRounds writes server-owned fixture rows; the summary under test reads them back through
// the real service and query.
func seedRounds(t *testing.T, f gatedFixture, rounds []seededRound) {
	t.Helper()
	digest, err := f.c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rounds {
		findings := r.findings
		if findings == "" {
			findings = `{"summary":"outcome","items":[]}`
		}
		var reason any
		if r.verdict != "pending" {
			reason = r.verdict
		}
		decided := "now()-interval '1 minute'"
		deadline := "now()+interval '30 minutes'"
		if r.verdict == "pending" {
			decided = "NULL"
		}
		f.env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
			required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
			checker_harness,verdict,reason_class,findings,automatic_rounds_enabled,automatic_revision_limit,decided_at,deadline_at)
			VALUES ($1,'plan',$2,$3,$4,'[]','{}','{}','s',$5,'',$6,'codex',$7,$8,$9,true,4,`+decided+`,`+deadline+`)`,
			f.lead, r.round, r.gen, r.plan, f.c.BaseCommit, digest, r.verdict, reason, findings)
	}
}

func (f gatedFixture) summary(t *testing.T) *apitypes.PlanCrossCheckSummaryDTO {
	t.Helper()
	got, err := f.svc.PlanCrossCheckSummary(f.env.ctx, f.w.UserID, f.lead)
	if err != nil || got == nil {
		t.Fatalf("PlanCrossCheckSummary: %+v err=%v", got, err)
	}
	return got
}

func dispositions(body string) string { return "\n\n## Cross-check dispositions\n" + body + "\n" }

// TestGatedPlanCrossCheckSummaryRoundsLiveDB is the three-round case of PRD #3708 R2: each
// later plan keeps only the dispositions that answer its immediately preceding findings, and
// each round shows its OWN response, never the latest or an accumulation.
func TestGatedPlanCrossCheckSummaryRoundsLiveDB(t *testing.T) {
	f := newGatedFixture(t, 4)
	const secretMarker = "CANDIDATE-BODY-MARKER"
	p1 := "# Plan one " + secretMarker + "-1"
	p2 := "# Plan two " + secretMarker + "-2" + dispositions("R1: moved the migration behind a flag")
	p3 := "# Plan three " + secretMarker + "-3" + dispositions("R2: added the missing test")
	seedRounds(t, f, []seededRound{
		{round: 1, gen: 1, verdict: "revise", plan: p1, findings: `{"summary":"one","items":[{"file":"a.go","severity":"error","summary":"bug one","rationale":"why one"}]}`},
		{round: 2, gen: 1, verdict: "revise", plan: p2, findings: `{"summary":"two","items":[{"file":"b.go","severity":"warning","summary":"nit two","rationale":"why two"}]}`},
		{round: 3, gen: 1, verdict: "approve", plan: p3},
	})
	f.env.exec(`UPDATE runs SET plan_md=$2 WHERE id=$1`, f.lead, p3)
	got := f.summary(t)
	if len(got.Rounds) != 3 {
		t.Fatalf("rounds = %d", len(got.Rounds))
	}
	want := []struct {
		round         int32
		verdict       string
		state, text   string
		findingsFirst string
	}{
		{1, "revise", apitypes.PlanCrossCheckResponseReported, "R1: moved the migration behind a flag", "bug one"},
		{2, "revise", apitypes.PlanCrossCheckResponseReported, "R2: added the missing test", "nit two"},
		{3, "approve", apitypes.PlanCrossCheckResponseNoFollowingCandidate, "", ""},
	}
	for i, w := range want {
		r := got.Rounds[i]
		if r.Round != w.round || r.Verdict != w.verdict || r.Response.State != w.state || r.Response.Text != w.text {
			t.Fatalf("round %d: %+v, want %+v", i+1, r, w)
		}
		if w.findingsFirst != "" && (r.Findings == nil || len(r.Findings.Items) != 1 || r.Findings.Items[0].Summary != w.findingsFirst) {
			t.Fatalf("round %d findings: %+v", i+1, r.Findings)
		}
		if r.DecidedAt == nil {
			t.Fatalf("round %d lost decided_at", i+1)
		}
	}
	if got.DeadlineAt != nil {
		t.Fatalf("deadline_at must be set only while pending: %v", got.DeadlineAt)
	}
	// plan_md never reaches the client, only the extracted response does.
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), secretMarker) || strings.Contains(string(wire), "Plan three") {
		t.Fatalf("candidate text leaked into the summary: %s", wire)
	}
}

func TestGatedPlanCrossCheckSummaryResponseStatesLiveDB(t *testing.T) {
	long := strings.Repeat("b", crossCheckResponseMaxBytes+500)
	atCap := strings.Repeat("a", planCandidateNormalizationCap)
	presented := "# Revised after round one" + dispositions("R1: handled in the presented plan")
	for _, tc := range []struct {
		name      string
		rounds    []seededRound
		presented string
		revisions int
		wantState string
		wantText  string
		wantLen   int // when wantText is empty and wantLen>0: exact text length
	}{
		{"reported", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}, {round: 2, gen: 1, verdict: "approve", plan: "p2" + dispositions("R1: fixed")}},
			"p2", 0, apitypes.PlanCrossCheckResponseReported, "R1: fixed", 0},
		{"not_reported", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}, {round: 2, gen: 1, verdict: "approve", plan: "p2 has no response"}},
			"p2", 0, apitypes.PlanCrossCheckResponseNotReported, "", 0},
		{"truncated", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}, {round: 2, gen: 1, verdict: "approve", plan: "p2" + dispositions(long)}},
			"p2", 0, apitypes.PlanCrossCheckResponseTruncated, "", crossCheckResponseMaxBytes},
		{"unknown", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}, {round: 2, gen: 1, verdict: "approve", plan: atCap}},
			"p2", 0, apitypes.PlanCrossCheckResponseUnknown, "", 0},
		{"replanned_after_interruption", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}, {round: 2, gen: 2, verdict: "approve", plan: "p2" + dispositions("R1: written by the reclaimed lead")}},
			"p2", 0, apitypes.PlanCrossCheckResponseReplannedAfterInterrupted, "", 0},
		{"reported_in_presented_plan", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}},
			presented, 0, apitypes.PlanCrossCheckResponseReportedInPresentedPlan, "R1: handled in the presented plan", 0},
		{"no_following_candidate: nothing presented", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}},
			"", 0, apitypes.PlanCrossCheckResponseNoFollowingCandidate, "", 0},
		{"no_following_candidate: the owner revised the presented plan", []seededRound{{round: 1, gen: 1, verdict: "revise", plan: "p1"}},
			presented, 1, apitypes.PlanCrossCheckResponseNoFollowingCandidate, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatedFixture(t, 4)
			seedRounds(t, f, tc.rounds)
			if tc.presented != "" {
				f.env.exec(`UPDATE runs SET plan_md=$2, revise_count=$3 WHERE id=$1`, f.lead, tc.presented, tc.revisions)
			}
			got := f.summary(t)
			r := got.Rounds[0]
			if r.Response.State != tc.wantState {
				t.Fatalf("state = %q, want %q (text %.80q)", r.Response.State, tc.wantState, r.Response.Text)
			}
			switch {
			case tc.wantText != "":
				if r.Response.Text != tc.wantText {
					t.Fatalf("text = %.120q, want %q", r.Response.Text, tc.wantText)
				}
			case tc.wantLen > 0:
				if len(r.Response.Text) != tc.wantLen {
					t.Fatalf("text length = %d, want %d", len(r.Response.Text), tc.wantLen)
				}
			default:
				if r.Response.Text != "" {
					t.Fatalf("a %s response carries no text, got %.80q", tc.wantState, r.Response.Text)
				}
			}
		})
	}
}

func TestGatedPlanCrossCheckSummaryPendingDeadlineLiveDB(t *testing.T) {
	f := newGatedFixture(t, 2)
	cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := f.summary(t)
	if got.DeadlineAt == nil || !got.DeadlineAt.Equal(cc.DeadlineAt.Time) || len(got.Rounds) != 1 || got.Rounds[0].Verdict != "pending" || got.Rounds[0].DecidedAt != nil {
		t.Fatalf("pending summary: deadline=%v rounds=%+v", got.DeadlineAt, got.Rounds)
	}
	f.decide(t, cc, "revise")
	if got := f.summary(t); got.DeadlineAt != nil {
		t.Fatalf("a decided round has no deadline: %v", got.DeadlineAt)
	}
}

// Only the newest five rounds are returned, oldest first, and the response of the oldest shown
// round is still read from its following row.
func TestGatedPlanCrossCheckSummaryWindowLiveDB(t *testing.T) {
	f := newGatedFixture(t, 4)
	rounds := make([]seededRound, 0, 5)
	for r := int32(1); r <= 5; r++ {
		verdict := "revise"
		if r == 5 {
			verdict = "approve"
		}
		rounds = append(rounds, seededRound{round: r, gen: 1, verdict: verdict, plan: "plan " + string(rune('0'+r)) + dispositions("answer to round "+string(rune('0'+r-1)))})
	}
	seedRounds(t, f, rounds)
	got := f.summary(t)
	if len(got.Rounds) != 5 || got.Rounds[0].Round != 1 || got.Rounds[4].Round != 5 {
		t.Fatalf("rounds: %+v", got.Rounds)
	}
	if got.Rounds[0].Response.Text != "answer to round 1" || got.Rounds[3].Response.Text != "answer to round 4" {
		t.Fatalf("responses: %q / %q", got.Rounds[0].Response.Text, got.Rounds[3].Response.Text)
	}
	// A foreign owner reads nothing.
	if foreign, err := f.svc.PlanCrossCheckSummary(f.env.ctx, uuid.New(), f.lead); err != nil || foreign != nil {
		t.Fatalf("foreign owner: %+v err=%v", foreign, err)
	}
}
