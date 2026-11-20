package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 acceptance, claim half: the claim backstop (assembleClaim's
// privcheck.GuardRepo call, driven through Service.Claim with the real privcheck
// service over a guardForge) runs against the forge only. It clears a healthy forge and
// fails closed (no payload, the run failed with origin guardrail_blocked) when the forge
// cannot be read.
//
// The claim context row carries the repo's link (issue_tracker_project_short), and this
// test seeds a truly linked claim: an issue run on a repo whose project short is ABC. On
// a service wired with an issue tracker it proves the claim path makes no issue tracker
// resolution (fakeSources.calls stays empty) and no request (the YouTrack fake stays
// open, so any request would be recorded), and that a cleared linked claim carries the
// issue_key the repo link derives (ABC-4) without asking the issue tracker anything.
// Whether that issue tracker is reachable is irrelevant: no request is made.
func TestLinkedRepoClaimGuardRunsAgainstTheForgeOnlyAndFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		verifyErr error
		wantBlock bool
	}{
		"healthy forge clears the claim":  {},
		"failing forge refuses the claim": {verifyErr: errors.New("401 token revoked"), wantBlock: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := youtracktest.NewServer(t)
			yt := youTrackDriver(t, srv)
			srv.ResetRequests() // left open: any request the claim made would be recorded

			f := newClaimFixture(t)
			f.fs.claimRun.Kind = runkind.Issue
			f.fs.claimCtx.IssueTrackerProjectShort = pgtype.Text{String: "ABC", Valid: true}
			g := &guardForge{verifyErr: tc.verifyErr}
			f.svc.SetRepoGuard(privcheck.NewService(nil, m2Builder{g}))
			src := &fakeSources{tr: yt}
			f.svc.SetIssueSources(src)

			payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner}, nil)
			if err != nil {
				t.Fatalf("Claim = %v, want a nil error (idle on a refusal, a payload otherwise)", err)
			}
			if len(src.calls) != 0 {
				t.Fatalf("the claim resolved an issue tracker: %+v", src.calls)
			}
			if n := len(srv.Requests()); n != 0 {
				t.Fatalf("the claim made %d issue tracker requests", n)
			}
			if !tc.wantBlock {
				if payload == nil {
					t.Fatal("a healthy forge must clear the claim, got idle")
				}
				if payload.IssueKey != "ABC-4" {
					t.Fatalf("issue_key = %q, want ABC-4 from the repo link", payload.IssueKey)
				}
				if f.fs.claimFailed != nil {
					t.Fatalf("a cleared claim must not fail the run, got %+v", f.fs.claimFailed)
				}
				return
			}
			if payload != nil {
				t.Fatalf("a refused claim must return no payload, got %+v", payload)
			}
			if f.fs.claimFailed == nil || f.fs.claimFailed.ID != f.runID {
				t.Fatalf("claimFailed = %+v, want the run failed", f.fs.claimFailed)
			}
			if origin := f.fs.claimFailed.FailOrigin.String; origin != "guardrail_blocked" {
				t.Fatalf("fail origin = %q, want guardrail_blocked", origin)
			}
			if reason := f.fs.claimFailed.FailureReason.String; !strings.Contains(reason, "default-branch guardrail") {
				t.Fatalf("failure reason = %q, want it to name the guardrail", reason)
			}
		})
	}
}
