package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4320 M4: the REAL successor constructors and cross-check entrypoints against a real Postgres.
// provider_source_livedb_test.go drives createRunAtomic with an InheritFrom origin directly; this file
// proves each production call site passes it (a constructor that dropped the origin would resolve the
// owner's CURRENT source instead of copying the source run's binding). Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// cancelRun frees the one-active-per-target guards between stages.
func (pf *provFix) cancelRun(id uuid.UUID) {
	pf.t.Helper()
	pf.env.exec(`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, id)
}

// TestProviderSuccessorConstructorsCopyTheBindingLiveDB: each successor constructor copies the source
// run's frozen binding verbatim, and keeps copying it after the owner moves to another provider or
// clears the source (an owner's later choice never re-resolves a continuation).
func TestProviderSuccessorConstructorsCopyTheBindingLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	src := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(src, 1, fx.keyRev())
	want := bindingOfRun(src)

	checkBound := func(t *testing.T, what string, run store.Run) {
		t.Helper()
		got := mustRun(t, fx.env, run.ID)
		fx.wantBound(got, 1, src.ModelProviderKeyRev.Int64)
		if bindingOfRun(got) != want {
			t.Fatalf("%s binding %+v != source %+v", what, bindingOfRun(got), want)
		}
	}
	stages := []struct {
		name string
		move func()
	}{
		{"source still selected", func() {}},
		{"owner switched to another provider", func() {
			other, _ := seedProviderFor(t, fx.env, fx.userID, pvDefaultMap)
			fx.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, fx.userID, other)
		}},
		{"owner cleared the source", func() {
			fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
		}},
	}
	for i, st := range stages {
		st.move()
		n := int64(i + 1)
		branch := "uzi/task/" + uuid.NewString()

		rework, err := fx.svc.CreateAutoMRReworkRun(fx.env.ctx, fx.userID, fx.repoID, "agent/issue-"+uuid.NewString()[:4], 900+n, src.ID, "Rework", "d", sampleReviewSnapshot())
		if err != nil {
			t.Fatalf("%s: mr_rework: %v", st.name, err)
		}
		checkBound(t, st.name+": mr_rework", rework)

		review, err := fx.svc.CreateTaskReviewRun(fx.env.ctx, fx.userID, fx.repoID, src.ID, branch, "main", HarnessClaude)
		if err != nil {
			t.Fatalf("%s: task review: %v", st.name, err)
		}
		checkBound(t, st.name+": task review", review)

		fix, err := fx.svc.CreateThenFixRun(fx.env.ctx, fx.userID, fx.repoID, src.ID, branch, "main", "fix", pgtype.Int4{}, pgtype.Int4{}, HarnessClaude)
		if err != nil {
			t.Fatalf("%s: then-fix: %v", st.name, err)
		}
		checkBound(t, st.name+": then-fix", fix)

		for _, id := range []uuid.UUID{rework.ID, review.ID, fix.ID} {
			fx.cancelRun(id)
		}
	}
}

// providerOwnerLead gives the cross-check fixture's owner (whose lead is a VENDOR run) a provider
// source, so the lane guard, not the lead's binding, is what must refuse.
func providerOwnerLead(t *testing.T, f crossCheckContentionFixture) {
	t.Helper()
	f.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	provider, _ := seedProviderFor(t, f.env, f.userID, pvDefaultMap)
	f.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, f.userID, provider)
	t.Cleanup(func() {
		if _, err := f.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, f.userID); err != nil {
			t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	if lead := mustRun(t, f.env, f.lead); providerBoundRun(lead) {
		t.Fatalf("fixture lead is provider-bound; the test needs a vendor lead")
	}
}

// TestProviderOwnerCrossCheckSubmitFailsClosedLiveDB: a provider-sourced owner whose lead is a vendor
// run gets no checker child from the real plan and code submit entrypoints. The plan stage reports
// ErrCrossCheckUnavailable (the lead's human gate); the code stage records the checker_unavailable
// failure. Neither creates a child run.
func TestProviderOwnerCrossCheckSubmitFailsClosedLiveDB(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		f, w := admissionCheckerBase(t, "plan")
		providerOwnerLead(t, f)
		before := f.env.countRunsForUser(t, f.userID)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, w, f.lead, 1, f.candidate)
		if !errors.Is(err, ErrCrossCheckUnavailable) || cc.CheckerRunID.Valid {
			t.Fatalf("plan submit: checker=%v err=%v, want ErrCrossCheckUnavailable and no child", cc.CheckerRunID, err)
		}
		if got := f.env.countRunsForUser(t, f.userID); got != before {
			t.Fatalf("a refused plan checker left %d run(s)", got-before)
		}
	})
	t.Run("code", func(t *testing.T) {
		f, w := admissionCheckerBase(t, "code")
		providerOwnerLead(t, f)
		before := f.env.countRunsForUser(t, f.userID)
		cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
		if err != nil || cc.CheckerRunID.Valid || cc.Outcome.String != "failed" || cc.ReasonClass.String != "checker_unavailable" {
			t.Fatalf("code submit: checker=%v outcome=%q reason=%q err=%v, want the recorded checker_unavailable failure and no child",
				cc.CheckerRunID, cc.Outcome.String, cc.ReasonClass.String, err)
		}
		if got := f.env.countRunsForUser(t, f.userID); got != before {
			t.Fatalf("a refused code checker left %d run(s)", got-before)
		}
	})
}
