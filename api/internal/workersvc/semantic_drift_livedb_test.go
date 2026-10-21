package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type driftForgeBuilder struct{ f forge.Forge }

func (b driftForgeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type driftLive struct {
	*coverageLiveFixture
	forge *driftFakeForge
	wkr   store.Worker
}

func newDriftLive(t *testing.T) *driftLive {
	t.Helper()
	f := newCoverageLiveFixture(t)
	f.svc.q = f.e.q
	d := &driftLive{coverageLiveFixture: f, forge: &driftFakeForge{}, wkr: store.Worker{ID: f.worker, UserID: f.e.userID}}
	f.svc.forges = driftForgeBuilder{d.forge}
	d.forge.issue = forge.Issue{Description: "frozen issue requirement"}
	return d
}

func (d *driftLive) publication(sha string) apitypes.SemanticObservationRequest {
	// Every frozen PRD source is reported once, as the agent does.
	prds := []apitypes.SemanticObservedSource{}
	for _, ref := range d.candidate.Sources {
		if ref.Kind == "prd" {
			prds = append(prds, apitypes.SemanticObservedSource{SourceID: ref.ID, Ref: sha, Outcome: "unknown", Reason: "missing"})
		}
	}
	return apitypes.SemanticObservationRequest{Phase: "publication", PushedSHA: sha, PRDSources: prds}
}

func (d *driftLive) observe(t *testing.T, generation int64, req apitypes.SemanticObservationRequest) (apitypes.SemanticDriftObservation, error) {
	t.Helper()
	return d.svc.ObserveSemanticDrift(d.e.ctx, d.wkr, d.run, generation, req)
}

func (d *driftLive) stored(t *testing.T) (storedSemanticObservations, string) {
	t.Helper()
	raw, err := d.e.q.GetSemanticObservations(d.e.ctx, d.run)
	if err != nil {
		t.Fatal(err)
	}
	var out storedSemanticObservations
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, string(raw)
}

func sha40(n int) string { return fmt.Sprintf("%040x", n) }

func TestSemanticDriftObservationLiveDB(t *testing.T) {
	d := newDriftLive(t)
	var reviewBefore, candidateBefore string
	if err := d.e.pool.QueryRow(d.e.ctx, `SELECT coalesce(review_state::text,''), candidate::text FROM semantic_planning WHERE run_id=$1`, d.run).Scan(&reviewBefore, &candidateBefore); err != nil {
		t.Fatal(err)
	}

	t.Run("unchanged then changed with checkbox only", func(t *testing.T) {
		rec, err := d.observe(t, 7, d.publication(sha40(1)))
		if err != nil || rec.Issue.Outcome != "unchanged" || rec.Issue.ObservedHash == "" || rec.Issue.SourceID != "issue" || len(rec.PRDs) != 1 || rec.PRDs[0].Outcome != "unknown" {
			t.Fatalf("unchanged: %+v err=%v", rec, err)
		}
		d.forge.issue.Description = "frozen issue requirement\nsee prds/9-new.md"
		rec, err = d.observe(t, 7, d.publication(sha40(2)))
		if err != nil || rec.Issue.Outcome != "changed" || rec.Issue.CheckboxOnly || len(rec.LinksAdded) != 1 || rec.LinksAdded[0] != "prds/9-new.md" {
			t.Fatalf("changed: %+v err=%v", rec, err)
		}
		d.forge.issue.Description = "frozen issue requirement"
	})

	t.Run("forge failure records unknown and never fails the request", func(t *testing.T) {
		d.forge.err = errors.New("forge down")
		rec, err := d.observe(t, 7, d.publication(sha40(3)))
		d.forge.err = nil
		if err != nil || rec.Issue.Outcome != "unknown" || rec.Issue.Reason != "unreadable" || rec.Issue.ObservedHash != "" {
			t.Fatalf("forge error: %+v err=%v", rec, err)
		}
	})

	t.Run("generation fence and ownership", func(t *testing.T) {
		before, _ := d.stored(t)
		if _, err := d.observe(t, 6, d.publication(sha40(4))); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale generation: %v", err)
		}
		other := d.e.seedWorker(t, nil)
		if _, err := d.svc.ObserveSemanticDrift(d.e.ctx, store.Worker{ID: other, UserID: d.e.userID}, d.run, 7, d.publication(sha40(4))); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker: %v", err)
		}
		if after, _ := d.stored(t); len(after.Items) != len(before.Items) {
			t.Fatal("a refused observation was stored")
		}
	})

	t.Run("stale, foreign and unenrolled requests cost no forge call", func(t *testing.T) {
		calls := d.forge.calls
		if _, err := d.observe(t, 6, d.publication(sha40(4))); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale generation: %v", err)
		}
		other := d.e.seedWorker(t, nil)
		if _, err := d.svc.ObserveSemanticDrift(d.e.ctx, store.Worker{ID: other, UserID: d.e.userID}, d.run, 7, d.publication(sha40(4))); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker: %v", err)
		}
		bare := d.e.seedActiveRunOwnedBy(t, d.worker)
		d.e.exec(t, `UPDATE runs SET claim_generation=7,completion_contract_version=1 WHERE id=$1`, bare)
		if _, err := d.svc.ObserveSemanticDrift(d.e.ctx, d.wkr, bare, 7, d.publication(sha40(4))); !errors.Is(err, errCoverageUnavailable) {
			t.Fatalf("run without a semantic row: %v", err)
		}
		d.e.exec(t, `UPDATE runs SET status='completed' WHERE id=$1`, bare)
		if _, err := d.svc.ObserveSemanticDrift(d.e.ctx, d.wkr, bare, 7, d.publication(sha40(4))); err == nil {
			t.Fatal("terminal run accepted")
		}
		if d.forge.calls != calls {
			t.Fatalf("a request refused by the pre-check read the forge %d time(s)", d.forge.calls-calls)
		}
	})

	t.Run("validation refusals store nothing", func(t *testing.T) {
		before, _ := d.stored(t)
		foreign := d.publication(sha40(5))
		foreign.PRDSources[0].SourceID = "prd:prds/foreign.md"
		omitted := d.publication(sha40(5))
		omitted.PRDSources = nil
		for name, req := range map[string]apitypes.SemanticObservationRequest{"foreign source": foreign, "omitted frozen source": omitted} {
			if _, err := d.observe(t, 7, req); !errors.Is(err, ErrSemanticObservationInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if after, _ := d.stored(t); len(after.Items) != len(before.Items) {
			t.Fatal("a refused observation was stored")
		}
	})

	t.Run("presentation is bound to the candidate digest and gate revision", func(t *testing.T) {
		d.e.exec(t, `UPDATE runs SET gate_revision=3 WHERE id=$1`, d.run)
		gate := int64(3)
		req := apitypes.SemanticObservationRequest{Phase: "presentation", CandidateDigest: d.candidate.Digest, GateRevision: &gate,
			PRDSources: []apitypes.SemanticObservedSource{{SourceID: "missing-prd", Ref: sha40(6), Outcome: "unknown", Reason: "unreadable"}}}
		rec, err := d.observe(t, 7, req)
		if err != nil || rec.Phase != "presentation" || rec.GateRevision != 3 || rec.CandidateDigest != d.candidate.Digest {
			t.Fatalf("presentation: %+v err=%v", rec, err)
		}
		stale := int64(2)
		req.GateRevision = &stale
		if _, err = d.observe(t, 7, req); !errors.Is(err, ErrSemanticGateMismatch) {
			t.Fatalf("stale gate revision: %v", err)
		}
		req.GateRevision, req.CandidateDigest = &gate, strings.Repeat("0", 64)
		if _, err = d.observe(t, 7, req); !errors.Is(err, ErrSemanticGateMismatch) {
			t.Fatalf("foreign candidate digest: %v", err)
		}
	})

	t.Run("bounded at 32 with a dropped count and no body text stored", func(t *testing.T) {
		before, _ := d.stored(t)
		const total = 40
		for i := range total {
			req := d.publication(sha40(1000 + i))
			if _, err := d.observe(t, 7, req); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		after, raw := d.stored(t)
		if len(after.Items) != 32 || after.Dropped != int64(len(before.Items)+total-32) {
			t.Fatalf("items=%d dropped=%d, want 32 and %d", len(after.Items), after.Dropped, len(before.Items)+total-32)
		}
		if after.Items[31].PushedSHA != sha40(1000+total-1) {
			t.Fatalf("the newest observation must be last, got %s", after.Items[31].PushedSHA)
		}
		if strings.Contains(raw, "frozen issue requirement") {
			t.Fatalf("stored observations carry issue text: %s", raw)
		}
		var review, candidate string
		if err := d.e.pool.QueryRow(d.e.ctx, `SELECT coalesce(review_state::text,''), candidate::text FROM semantic_planning WHERE run_id=$1`, d.run).Scan(&review, &candidate); err != nil {
			t.Fatal(err)
		}
		if review != reviewBefore || candidate != candidateBefore {
			t.Fatal("an observation changed the coverage ledger or the candidate")
		}
		var history int
		if err := d.e.pool.QueryRow(d.e.ctx, `SELECT count(*) FROM semantic_candidate_history WHERE run_id=$1`, d.run).Scan(&history); err != nil || history != 1 {
			t.Fatalf("candidate history rows=%d err=%v", history, err)
		}
	})

	t.Run("owner projection", func(t *testing.T) {
		run, err := d.e.q.GetRunByID(d.e.ctx, d.run)
		if err != nil {
			t.Fatal(err)
		}
		dto, err := d.svc.SemanticDriftForOwner(d.e.ctx, d.e.userID, run)
		if err != nil || dto == nil || len(dto.Observations) != 32 || dto.Dropped < 1 || !dto.Contradiction {
			t.Fatalf("dto=%+v err=%v", dto, err)
		}
		if _, err = d.svc.SemanticDriftForOwner(d.e.ctx, uuid.New(), run); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("a non-owner must be refused: %v", err)
		}
	})
}

// One changed observation followed by 32 unchanged ones pushes the changed record out of the
// kept list; the sticky flag must still project a contradiction.
func TestSemanticDriftStickyContradictionLiveDB(t *testing.T) {
	d := newDriftLive(t)
	// No PRD source: every kept record is clean once the changed one is dropped, so only the
	// sticky flag can carry the contradiction.
	d.sources = d.sources[:1]
	d.submit(t)
	publish := func(n int) apitypes.SemanticObservationRequest {
		return apitypes.SemanticObservationRequest{Phase: "publication", PushedSHA: sha40(n), PRDSources: []apitypes.SemanticObservedSource{}}
	}
	d.forge.issue.Description = "frozen issue requirement edited"
	if rec, err := d.observe(t, 7, publish(1)); err != nil || rec.Issue.Outcome != "changed" {
		t.Fatalf("changed: %+v err=%v", rec, err)
	}
	d.forge.issue.Description = "frozen issue requirement"
	for i := range 32 {
		if rec, err := d.observe(t, 7, publish(100+i)); err != nil || rec.Issue.Outcome != "unchanged" {
			t.Fatalf("append %d: %+v err=%v", i, rec, err)
		}
	}
	stored, _ := d.stored(t)
	if len(stored.Items) != 32 || stored.Dropped != 1 || !stored.Contradicted {
		t.Fatalf("items=%d dropped=%d contradicted=%v", len(stored.Items), stored.Dropped, stored.Contradicted)
	}
	for _, o := range stored.Items {
		if o.Issue.Outcome != "unchanged" {
			t.Fatal("the changed record should have been dropped from the kept list")
		}
	}
	run, err := d.e.q.GetRunByID(d.e.ctx, d.run)
	if err != nil {
		t.Fatal(err)
	}
	dto, err := d.svc.SemanticDriftForOwner(d.e.ctx, d.e.userID, run)
	if err != nil || dto == nil || !dto.Contradiction || dto.Dropped != 1 {
		t.Fatalf("dto=%+v err=%v", dto, err)
	}
}

// A composed (scheduled guidance) frozen text compares the current raw body against the
// saved raw body: no edit is unchanged, an edit is changed.
func TestSemanticDriftComposedRunLiveDB(t *testing.T) {
	d := newDriftLive(t)
	d.e.exec(t, `UPDATE runs SET issue_saved_body=$2 WHERE id=$1`, d.run, "raw forge body")
	d.forge.issue.Description = "raw forge body"
	rec, err := d.observe(t, 7, d.publication(sha40(1)))
	if err != nil || rec.Issue.Outcome != "unchanged" || rec.Issue.ObservedHash != driftHash("raw forge body") {
		t.Fatalf("unedited composed run: %+v err=%v", rec, err)
	}
	d.forge.issue.Description = "raw forge body, edited"
	if rec, err = d.observe(t, 7, d.publication(sha40(2))); err != nil || rec.Issue.Outcome != "changed" {
		t.Fatalf("edited composed run: %+v err=%v", rec, err)
	}
}

// The worker status carries exactly the candidate's frozen PRD refs (so a resumed process can
// observe drift at publication) and omits the field when the status is unavailable.
func TestSemanticCoverageStatusPRDSourcesLiveDB(t *testing.T) {
	d := newDriftLive(t)
	want := []apitypes.SemanticSourceRef{}
	for _, ref := range d.candidate.Sources {
		if ref.Kind == "prd" {
			want = append(want, ref)
		}
	}
	if len(want) == 0 {
		t.Fatal("fixture has no frozen prd source")
	}
	gotp := d.status(t).PRDSources
	if gotp == nil {
		t.Fatal("PRDSources absent on a frozen candidate")
	}
	got := *gotp
	if len(got) != len(want) {
		t.Fatalf("PRDSources=%+v want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || got[i].Kind != "prd" {
			t.Fatalf("PRDSources[%d]=%+v want %+v", i, got[i], want[i])
		}
	}

	bare := d.e.seedActiveRunOwnedBy(t, d.worker)
	d.e.exec(t, `UPDATE runs SET claim_generation=7,completion_contract_version=1 WHERE id=$1`, bare)
	unavailable, err := d.svc.SemanticCoverageStatus(d.e.ctx, d.worker, bare, 7)
	if err != nil || unavailable.Outcome != "unavailable" || unavailable.PRDSources != nil {
		t.Fatalf("unavailable status: %+v err=%v", unavailable, err)
	}
	raw, err := json.Marshal(unavailable)
	if err != nil || strings.Contains(string(raw), "prd_sources") {
		t.Fatalf("an unavailable status must omit prd_sources: %s err=%v", raw, err)
	}
}

// A composed run whose raw body is unedited reports no link drift even though the composed
// text carried a PRD path the raw body never had (the frozen PRD source came from the guidance).
func TestSemanticDriftComposedLinksLiveDB(t *testing.T) {
	d := newDriftLive(t)
	// Freeze a PRD source whose path exists only in the composed text, never in the raw bodies.
	d.sources = append(d.sources, apitypes.SemanticSourceCapture{ID: "prd:prds/7-x.md", Kind: "prd", Provenance: "repo:prds/7-x.md", Completeness: "complete", Bytes: []byte("composed-only prd")})
	d.submit(t)
	frozenPRD := false
	for _, ref := range d.candidate.Sources {
		frozenPRD = frozenPRD || ref.ID == "prd:prds/7-x.md"
	}
	if !frozenPRD {
		t.Fatalf("fixture did not freeze prd:prds/7-x.md: %+v", d.candidate.Sources)
	}
	d.e.exec(t, `UPDATE runs SET issue_saved_body=$2 WHERE id=$1`, d.run, "raw forge body")
	d.forge.issue.Description = "raw forge body"
	rec, err := d.observe(t, 7, d.publication(sha40(11)))
	if err != nil || rec.Issue.Outcome != "unchanged" || len(rec.LinksAdded) != 0 || len(rec.LinksRemoved) != 0 {
		t.Fatalf("unedited composed run reports links: %+v err=%v", rec, err)
	}
	d.forge.issue.Description = "raw forge body\nsee prds/9-new.md"
	if rec, err = d.observe(t, 7, d.publication(sha40(12))); err != nil || len(rec.LinksAdded) != 1 || rec.LinksAdded[0] != "prds/9-new.md" || len(rec.LinksRemoved) != 0 {
		t.Fatalf("link added to the raw body: %+v err=%v", rec, err)
	}
}

// An empty saved raw body (issue_saved_body = ”) is a real baseline: a composed run whose
// raw body is still empty is unchanged, not changed forever.
func TestSemanticDriftEmptySavedBodyLiveDB(t *testing.T) {
	d := newDriftLive(t)
	d.e.exec(t, `UPDATE runs SET issue_saved_body='' WHERE id=$1`, d.run)
	d.forge.issue.Description = ""
	rec, err := d.observe(t, 7, d.publication(sha40(13)))
	if err != nil || rec.Issue.Outcome != "unchanged" {
		t.Fatalf("empty saved body: %+v err=%v", rec, err)
	}
}

// A candidate with only the issue source sends an explicit empty PRDSources (not absent): the
// resumed process then knows there is nothing to observe, rather than that the field is missing.
func TestSemanticCoverageStatusNoPRDSourceLiveDB(t *testing.T) {
	d := newDriftLive(t)
	d.sources = d.sources[:1]
	d.submit(t)
	got := d.status(t)
	if got.PRDSources == nil || len(*got.PRDSources) != 0 {
		t.Fatalf("PRDSources=%+v want explicit empty", got.PRDSources)
	}
	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"prd_sources":[]`) {
		t.Fatalf("worker status JSON lacks an explicit empty prd_sources: %s err=%v", raw, err)
	}
}
