package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The terminal-preservation gate of verifyCompletedPublication (#4007 slice A m5). The sole opt-in
// is a run_terminal_contributions row for exactly the completing claim's generation.

type gateEnv struct {
	e    interlockLiveDB
	wid  uuid.UUID
	run  uuid.UUID
	hold uuid.UUID
	f    *publicationForge
	svc  *Service
	w    store.Worker
	req  StateRequest
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{capability.RecoveryCompletedPublicationV1})
	run := e.seedLegacyRunningRun(t, wid)
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
	hold := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
	final := strings.Repeat("a", 40)
	f := &publicationForge{t: t, projectID: 1, mrIID: 7, expectedBranch: "agent/issue-1", branch: "agent/issue-1",
		head: final, summaryHead: final, ancestry: forge.AncestryAncestor}
	svc := e.permitService(t)
	svc.SetForges(settleUnitBuilder{f: f})
	gen, mr := int64(1), int64(7)
	branch := "agent/issue-1"
	return &gateEnv{e: e, wid: wid, run: run, hold: hold, f: f, svc: svc,
		w:   store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: []string{capability.RecoveryCompletedPublicationV1}},
		req: StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &final, Branch: &branch, MrIID: &mr}}
}

// contribute inserts the run's preservation (insert-or-load) and a contribution of the given
// generation, then drives it to the wanted state through the real writers.
func (g *gateEnv) contribute(t *testing.T, gen int64, state string) {
	t.Helper()
	p, err := g.e.q.InsertTerminalPreservation(g.e.ctx, store.InsertTerminalPreservationParams{
		RunID: g.run, UserID: g.e.userID, RepoID: g.e.repoID, Ref: "refs/uzi-terminal/" + g.run.String(), LifetimeAtBind: "issue",
	})
	if err != nil {
		t.Fatalf("preservation: %v", err)
	}
	disposition := "preserved"
	if state == "recorded" {
		disposition = "closed_issue_abandoned"
	}
	c, err := g.e.q.InsertTerminalContribution(g.e.ctx, store.InsertTerminalContributionParams{
		PreservationID: p.ID, RunID: g.run, Generation: gen, WorkerID: g.wid, UserID: g.e.userID,
		Authority: "current_flight", Tip: strings.Repeat("b", 40), CoverageDigest: strings.Repeat("c", 64),
		Manifest: []byte(`{}`), Disposition: disposition,
	})
	if err != nil {
		t.Fatalf("contribution: %v", err)
	}
	if state == "prepared" || state == "recorded" {
		return
	}
	if _, err := g.e.q.MarkTerminalPreservationPublishing(g.e.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "bound"}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if c, err = g.e.q.MarkTerminalContributionInvoked(g.e.ctx, c.ID); err != nil {
		t.Fatalf("invoked: %v", err)
	}
	switch state {
	case "invoked":
	case "publish_unknown":
		_, err = g.e.q.RecordTerminalContributionUnknown(g.e.ctx, c.ID)
	case "published":
		_, err = g.e.q.RecordTerminalContributionPublished(g.e.ctx, store.RecordTerminalContributionPublishedParams{ID: c.ID, ExpectedState: "invoked"})
	case "refused":
		_, err = g.e.q.RecordTerminalContributionRefused(g.e.ctx, store.RecordTerminalContributionRefusedParams{
			ID: c.ID, ExpectedState: "invoked", RefusalReason: pgconv.Text("create_refused")})
	default:
		t.Fatalf("unknown wanted state %q", state)
	}
	if err != nil {
		t.Fatalf("advance to %s: %v", state, err)
	}
}

func (g *gateEnv) report(t *testing.T) *StateReportResult {
	t.Helper()
	res, err := g.svc.SetStateReportWithReconciliation(g.e.ctx, g.w, g.run, g.req)
	if err != nil || !res.Applied || res.Run.Status != "completed" {
		t.Fatalf("completion committed: %+v %v", res, err)
	}
	return &res
}

func (g *gateEnv) holdState(t *testing.T) (state string, reason pgtype.Text, receipt bool) {
	t.Helper()
	var raw []byte
	if err := g.e.pool.QueryRow(g.e.ctx, `SELECT state, completed_publication_reason, completed_publication_receipt
		FROM recovery_custody_holds WHERE id = $1`, g.hold).Scan(&state, &reason, &raw); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	return state, reason, len(raw) > 0
}

func (g *gateEnv) wantReleased(t *testing.T, res *StateReportResult) {
	t.Helper()
	state, _, receipt := g.holdState(t)
	if res.CompletedPublicationReceipt == nil || res.CompletedPublicationReason != "" || state != "released" || !receipt {
		t.Fatalf("want released with a receipt: reason=%q receipt=%v hold=%s stored=%v",
			res.CompletedPublicationReason, res.CompletedPublicationReceipt != nil, state, receipt)
	}
}

func (g *gateEnv) wantRefused(t *testing.T, res *StateReportResult, reason string) {
	t.Helper()
	state, stored, receipt := g.holdState(t)
	if res.CompletedPublicationReceipt != nil || res.CompletedPublicationReason != reason {
		t.Fatalf("result reason=%q receipt=%v, want refusal %q", res.CompletedPublicationReason, res.CompletedPublicationReceipt != nil, reason)
	}
	if state != "open" || receipt || !stored.Valid || stored.String != reason {
		t.Fatalf("hold state=%q receipt=%v stored reason=%+v, want open, no receipt, %q", state, receipt, stored, reason)
	}
}

func TestCompletedPublicationTerminalGateLiveDB(t *testing.T) {
	t.Run("no_contribution_row_releases_as_before", func(t *testing.T) {
		// The inventory-guarded hold and the capability alone must not opt a run in.
		g := newGateEnv(t)
		g.wantReleased(t, g.report(t))
		if g.f.compareCalls != 1 {
			t.Fatalf("compare calls = %d, want 1", g.f.compareCalls)
		}
	})
	t.Run("preservation_without_contribution_releases", func(t *testing.T) {
		g := newGateEnv(t)
		if _, err := g.e.q.InsertTerminalPreservation(g.e.ctx, store.InsertTerminalPreservationParams{
			RunID: g.run, UserID: g.e.userID, RepoID: g.e.repoID, Ref: "refs/uzi-terminal/" + g.run.String(), LifetimeAtBind: "issue"}); err != nil {
			t.Fatal(err)
		}
		g.wantReleased(t, g.report(t))
	})
	for _, state := range []string{"published", "recorded"} {
		t.Run(state+"_releases", func(t *testing.T) {
			g := newGateEnv(t)
			g.contribute(t, 1, state)
			g.wantReleased(t, g.report(t))
		})
	}
	for _, state := range []string{"prepared", "invoked", "publish_unknown"} {
		t.Run(state+"_is_pending_and_spends_no_forge_proof", func(t *testing.T) {
			g := newGateEnv(t)
			g.contribute(t, 1, state)
			res := g.report(t)
			g.wantRefused(t, res, "terminal_preservation_pending")
			if g.f.compareCalls != 0 {
				t.Fatalf("the pre-check must refuse before the forge proof; compare calls = %d", g.f.compareCalls)
			}
		})
	}
	t.Run("pending_then_published_releases_on_retry", func(t *testing.T) {
		g := newGateEnv(t)
		g.contribute(t, 1, "invoked")
		g.wantRefused(t, g.report(t), "terminal_preservation_pending")
		g.e.exec(t, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE run_id=$1 AND generation=1`, g.run)
		g.wantReleased(t, g.report(t))
	})
	t.Run("refused_is_refused", func(t *testing.T) {
		g := newGateEnv(t)
		g.contribute(t, 1, "refused")
		g.wantRefused(t, g.report(t), "terminal_preservation_refused")
		if g.f.compareCalls != 0 {
			t.Fatalf("compare calls = %d, want 0", g.f.compareCalls)
		}
	})
	t.Run("other_generation_does_not_gate", func(t *testing.T) {
		g := newGateEnv(t)
		g.contribute(t, 2, "invoked")
		g.wantReleased(t, g.report(t))
	})
	t.Run("authoritative_check_catches_a_contribution_created_after_the_pre_check", func(t *testing.T) {
		// The pre-check saw no row; the contribution appears while the forge proof runs. Only the
		// locked check in the write transaction can refuse it.
		g := newGateEnv(t)
		g.f.afterCompare = func() { g.contribute(t, 1, "invoked") }
		res := g.report(t)
		g.wantRefused(t, res, "terminal_preservation_pending")
		if g.f.compareCalls != 1 {
			t.Fatalf("compare calls = %d, want 1 (the proof ran before the locked check)", g.f.compareCalls)
		}
	})
	t.Run("owner_discard_query_of_a_gate_refused_hold_succeeds", func(t *testing.T) {
		g := newGateEnv(t)
		g.contribute(t, 1, "invoked")
		g.wantRefused(t, g.report(t), "terminal_preservation_pending")
		n, err := g.e.q.DiscardCustodyHoldForOwner(g.e.ctx, store.DiscardCustodyHoldForOwnerParams{
			HoldID: g.hold, RunID: g.run, UserID: g.e.userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard"),
		})
		if err != nil || n != 1 {
			t.Fatalf("owner discard rows=%d err=%v, want 1", n, err)
		}
		if state, _ := g.e.fpHoldEvidence(t, g.hold); state != "discarded" {
			t.Fatalf("hold state = %q, want discarded", state)
		}
	})
	t.Run("lookup_error_is_ancestry_unknown", func(t *testing.T) {
		g := newGateEnv(t)
		svc := New(failingTerminalReader{g.e.q}, newBox(t), testParams())
		svc.SetTxBeginner(g.e.pool)
		svc.SetBackground(func(func()) {})
		svc.SetForges(settleUnitBuilder{f: g.f})
		g.svc = svc
		res := g.report(t)
		g.wantRefused(t, res, "ancestry_unknown")
		if g.f.compareCalls != 0 {
			t.Fatalf("compare calls = %d, want 0", g.f.compareCalls)
		}
	})
}

// failingTerminalReader fails the pre-check's unlocked contribution read with a non-ErrNoRows error.
type failingTerminalReader struct{ *store.Queries }

func (failingTerminalReader) GetTerminalContributionByRunGeneration(_ context.Context, _ store.GetTerminalContributionByRunGenerationParams) (store.RunTerminalContribution, error) {
	return store.RunTerminalContribution{}, errors.New("injected lookup failure")
}
