package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const redundancyCaptureBytes = 4096

// redundancyLive is a completed run with a guarded hold and one available archive capture.
// receipt mode: the hold was released by the completed-publication receipt (no final capture).
// ack mode: the receipt was refused and the hold was released by archive FINAL, which protects the
// capture (ADR-2417) and makes it the hold's final capture.
type redundancyLive struct {
	e       interlockLiveDB
	svc     *Service
	w       store.Worker
	run     uuid.UUID
	hold    uuid.UUID
	capture uuid.UUID
	vec     vecCase
	forge   *redundancyForge
	final   string
	head    string
	gen     int64
}

type redundancyLiveOpts struct {
	vec      string
	ack      bool
	nullMR   bool
	noID     bool // complete the run without a completion head, so the hold has no identity
	prereqs  []string
	noReport bool // leave the run running and the hold open
}

func newRedundancyLive(t *testing.T, o redundancyLiveOpts) *redundancyLive {
	t.Helper()
	e := setupInterlockLiveDB(t)
	c := loadVectors(t)[o.vec]
	wid := e.seedWorker(t, []string{capability.RecoveryCompletedPublicationV1})
	r := &redundancyLive{
		e: e, svc: e.permitService(t), vec: c, gen: 1, run: e.seedLegacyRunningRun(t, wid), hold: uuid.New(), capture: uuid.New(),
		w:     store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: []string{capability.RecoveryCompletedPublicationV1}},
		final: strings.Repeat("a", 40), head: strings.Repeat("b", 40),
	}
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", r.run)
	e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, r.hold, e.userID, e.repoID, r.run, wid)
	prereqs := o.prereqs
	if prereqs == nil {
		prereqs = []string{}
	}
	e.exec(t, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,expires_at,coverage_digest)
 VALUES($1,$2,$3,$4,$5,'ident',$6,'k','available',true,$7,'sum',1,$8,now()+interval '7 days',$9)`,
		r.capture, r.hold, r.run, e.userID, wid, c.SourceSha, redundancyCaptureBytes, prereqs, c.Digest)
	e.exec(t, `INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,4,'\x01020304')`, r.capture)

	if !o.noReport {
		mr := int64(7)
		pf := &publicationForge{t: t, projectID: 1, mrIID: mr, expectedBranch: "agent/issue-1", branch: "agent/issue-1", head: r.final, summaryHead: r.final, ancestry: forge.AncestryAncestor}
		if o.ack {
			pf.ancestry = forge.AncestryNotAncestor
		}
		r.svc.SetForges(settleUnitBuilder{f: pf})
		branch := "agent/issue-1"
		req := StateRequest{State: "completed", ClaimGeneration: &r.gen, CompletionFinalHead: &r.final, Branch: &branch, MrIID: &mr}
		switch {
		case o.nullMR:
			req.MrIID = nil
		case o.noID:
			req.CompletionFinalHead = nil
		}
		if o.noID {
			// A run completed outside the completed-publication protocol has no identity on its hold.
			e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", r.run)
		} else {
			res, err := r.svc.SetStateReportWithReconciliation(e.ctx, r.w, r.run, req)
			if err != nil || !res.Applied {
				t.Fatalf("complete: %+v %v", res, err)
			}
		}
		if o.ack || o.nullMR || o.noID {
			r.releaseByArchive(t)
		}
	}

	r.forge = &redundancyForge{
		branch: "agent/issue-1", head: r.head, summaryHead: r.head,
		answers: map[string]forge.Ancestry{r.final: forge.AncestryAncestor, c.CurrentSha: forge.AncestryAncestor}, unknown: map[string]bool{},
	}
	for _, root := range c.Roots {
		r.forge.answers[root] = forge.AncestryAncestor
	}
	for _, p := range prereqs {
		r.forge.answers[p] = forge.AncestryAncestor
	}
	r.svc.SetForges(settleUnitBuilder{f: r.forge})
	return r
}

// releaseByArchive protects the capture and releases the hold with archive FINAL.
func (r *redundancyLive) releaseByArchive(t *testing.T) {
	t.Helper()
	n, err := r.e.q.ProtectFinalInventoryCapture(r.e.ctx, store.ProtectFinalInventoryCaptureParams{
		WorkerID: r.w.ID, RetentionSeconds: 3600, ID: r.capture, HoldID: r.hold, SourceSha: r.vec.SourceSha, CoverageDigest: pgconv.Text(r.vec.Digest),
	})
	if err != nil || n != 1 {
		t.Fatalf("protect: %d %v", n, err)
	}
	n, err = r.e.q.ReleaseFinalInventoryHold(r.e.ctx, store.ReleaseFinalInventoryHoldParams{
		FinalDisposition: "archive", FinalCaptureID: pgconv.UUID(r.capture), FinalSourceSha: pgconv.Text(r.vec.SourceSha),
		FinalCoverageDigest: r.vec.Digest, ReleaseEvidence: "archive", ID: r.hold, RunID: r.run, UserID: r.e.userID,
		Generation: r.gen, WorkerID: r.w.ID,
	})
	if err != nil || n != 1 {
		t.Fatalf("release by archive: %d %v", n, err)
	}
}

func (r *redundancyLive) call(t *testing.T, req apitypes.RecoveryArchiveRedundancyRequest) apitypes.RecoveryArchiveRedundancyResponse {
	t.Helper()
	res, err := r.svc.ProveArchiveRedundancy(r.e.ctx, r.w, r.run, r.capture, req)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	return res
}

func (r *redundancyLive) claim() apitypes.RecoveryArchiveRedundancyRequest {
	return r.vec.request(r.gen)
}

func (r *redundancyLive) chunks(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.e.pool.QueryRow(r.e.ctx, "SELECT count(*) FROM recovery_capture_chunks WHERE capture_id=$1", r.capture).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type captureRow struct {
	State, Reason, Refusal                     string
	Proof                                      []byte
	LocalReplica                               pgtype.UUID
	ExpiresPast, RefusedAtSet, ReservedCleared bool
}

func (r *redundancyLive) row(t *testing.T) captureRow {
	t.Helper()
	var c captureRow
	err := r.e.pool.QueryRow(r.e.ctx, `SELECT state, COALESCE(reason,''), COALESCE(redundancy_refusal,''), redundancy_proof, local_replica_worker_id,
 expires_at <= now(), redundancy_refused_at IS NOT NULL, reserved_bytes IS NULL FROM recovery_captures WHERE id=$1`, r.capture).
		Scan(&c.State, &c.Reason, &c.Refusal, &c.Proof, &c.LocalReplica, &c.ExpiresPast, &c.RefusedAtSet, &c.ReservedCleared)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *redundancyLive) ownerBytes(t *testing.T) int64 {
	t.Helper()
	row, err := r.e.q.SumStoredFileBytes(r.e.ctx, store.SumStoredFileBytesParams{UserID: r.e.userID})
	if err != nil {
		t.Fatal(err)
	}
	return row.OwnerRecoveryBytes
}

func (r *redundancyLive) wantExpired(t *testing.T, res apitypes.RecoveryArchiveRedundancyResponse) {
	t.Helper()
	if res.Outcome != apitypes.RecoveryRedundancyExpired || res.Reason != "" || res.CaptureID != r.capture.String() {
		t.Fatalf("response %+v", res)
	}
	c := r.row(t)
	if c.State != "expired" || c.Reason != "published_redundant" || len(c.Proof) == 0 || c.LocalReplica.Valid || !c.ExpiresPast {
		t.Fatalf("expired capture row: %+v", c)
	}
	if n := r.chunks(t); n != 0 {
		t.Fatalf("expired capture kept %d chunks", n)
	}
}

func (r *redundancyLive) wantRetained(t *testing.T, res apitypes.RecoveryArchiveRedundancyResponse, reason string) {
	t.Helper()
	if res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != reason {
		t.Fatalf("response %+v want retained/%s", res, reason)
	}
	c := r.row(t)
	if c.State != "available" || len(c.Proof) != 0 || c.Reason == "published_redundant" {
		t.Fatalf("retained capture changed: %+v", c)
	}
	if n := r.chunks(t); n != 1 {
		t.Fatalf("retained capture has %d chunks, want its bytes intact", n)
	}
}

// (1) a receipt-released completed run whose archive is fully on the published branch.
func TestArchiveRedundancyReceiptHoldLiveDB(t *testing.T) {
	r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
	if got := r.ownerBytes(t); got != redundancyCaptureBytes {
		t.Fatalf("owner recovery bytes before: %d", got)
	}
	res := r.call(t, r.claim())
	r.wantExpired(t, res)
	if got := r.ownerBytes(t); got != 0 {
		t.Fatalf("stored bytes must drop with the archive: %d", got)
	}
	var proof redundancyProof
	if err := json.Unmarshal(r.row(t).Proof, &proof); err != nil || proof.AnchorHead != r.head || proof.FinalHead != r.final ||
		proof.CurrentSha != r.vec.CurrentSha || proof.Tree != r.vec.Tree || proof.HoldID != r.hold.String() || proof.Generation != 1 || proof.MRIID != 7 {
		t.Fatalf("recorded proof: %+v %v", proof, err)
	}
	calls := r.forge.callCount()
	// An exact replay is answered from the recorded proof without the forge.
	r.wantExpired(t, r.call(t, r.claim()))
	if r.forge.callCount() != calls {
		t.Fatal("replay consulted the forge")
	}
	// Discard of an expired-by-proof capture keeps working and keeps the proof immutable.
	if n, err := r.e.q.DiscardCaptureForOwner(r.e.ctx, store.DiscardCaptureForOwnerParams{ID: r.capture, RunID: r.run, UserID: r.e.userID}); err != nil || n != 1 {
		t.Fatalf("discard: %d %v", n, err)
	}
	if c := r.row(t); c.State != "discarded" || c.Reason != "published_redundant" || len(c.Proof) == 0 {
		t.Fatalf("after discard: %+v", c)
	}
}

// (2) parked WIP: the WIP commit is not published but a witness commit with its tree is.
func TestArchiveRedundancyWIPWitnessLiveDB(t *testing.T) {
	setup := func(t *testing.T) *redundancyLive {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "wip"})
		r.forge.answers[r.vec.CurrentSha] = forge.AncestryNotAncestor
		r.forge.answers[r.vec.WitnessObject.Sha] = forge.AncestryAncestor
		return r
	}
	t.Run("proved", func(t *testing.T) {
		r := setup(t)
		r.wantExpired(t, r.call(t, r.claim()))
	})
	t.Run("uncovered_without_wip", func(t *testing.T) {
		r := setup(t)
		req := r.claim()
		req.WIP = nil
		r.wantRetained(t, r.call(t, req), apitypes.RecoveryRedundancyUncoveredWIP)
	})
	t.Run("witness_unpublished", func(t *testing.T) {
		r := setup(t)
		r.forge.answers[r.vec.WitnessObject.Sha] = forge.AncestryNotAncestor
		r.wantRetained(t, r.call(t, r.claim()), apitypes.RecoveryRedundancyUncoveredWIP)
	})
	t.Run("witness_tree_mismatch", func(t *testing.T) {
		r := setup(t)
		req := r.claim()
		// A published commit with another tree proves nothing about the WIP content.
		req.WIP = &apitypes.RecoveryArchiveRedundancyWIP{WitnessObject: b64s("tree " + strings.Repeat("3", 40) + "\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nother\n")}
		r.wantRetained(t, r.call(t, req), apitypes.RecoveryRedundancyTreeMismatch)
		if r.forge.callCount() != 0 {
			t.Fatal("tree mismatch must be refused before the forge")
		}
	})
	t.Run("witness_bad_parent_case", func(t *testing.T) {
		r := setup(t)
		req := r.claim()
		req.WIP = &apitypes.RecoveryArchiveRedundancyWIP{WitnessObject: b64s("tree " + r.vec.Tree + "\nparent " + strings.Repeat("A", 40) + "\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nother\n")}
		r.wantRetained(t, r.call(t, req), apitypes.RecoveryRedundancyBadObject)
	})
}

// (3) every incomplete or mismatching claim keeps the archive and its bytes.
func TestArchiveRedundancyRetainsLiveDB(t *testing.T) {
	type tc struct {
		name   string
		opts   redundancyLiveOpts
		edit   func(*redundancyLive, *apitypes.RecoveryArchiveRedundancyRequest)
		reason string
	}
	for _, c := range []tc{
		{"uncovered_root", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.answers[r.vec.Roots[0]] = forge.AncestryNotAncestor
		}, apitypes.RecoveryRedundancyUncoveredRoot},
		{"ancestry_unknown", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.unknown[r.vec.Roots[0]] = true
		}, apitypes.RecoveryRedundancyAncestryUnknown},
		{"forge_timeout", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.compareErr = context.DeadlineExceeded
		}, apitypes.RecoveryRedundancyForgeTimeout},
		{"uncovered_prerequisite", redundancyLiveOpts{vec: "small", prereqs: []string{strings.Repeat("9", 40)}}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.answers[strings.Repeat("9", 40)] = forge.AncestryNotAncestor
		}, apitypes.RecoveryRedundancyUncoveredPrereq},
		{"digest_mismatch_with_capture", redundancyLiveOpts{vec: "small"}, func(_ *redundancyLive, req *apitypes.RecoveryArchiveRedundancyRequest) {
			req.CoverageDigest = strings.Repeat("d", 64)
		}, apitypes.RecoveryRedundancyDigestMismatch},
		{"aggregate_is_not_the_archived_source", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, req *apitypes.RecoveryArchiveRedundancyRequest) {
			// Same digest and heads, but the objects rebuild another commit than capture.source_sha.
			req.AggregateObjects = loadVectors(t)["two_rounds"].request(1).AggregateObjects
		}, apitypes.RecoveryRedundancySourceMismatch},
		{"generation_mismatch", redundancyLiveOpts{vec: "small"}, func(_ *redundancyLive, req *apitypes.RecoveryArchiveRedundancyRequest) {
			req.Generation = 2
		}, apitypes.RecoveryRedundancyBindingMismatch},
		{"fork_pr_branch", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.branch = "fork:agent/issue-1"
		}, apitypes.RecoveryRedundancyBranchMismatch},
		{"mr_head_differs_from_branch_head", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.summaryHead = strings.Repeat("c", 40)
		}, apitypes.RecoveryRedundancyHeadMismatch},
		{"branch_deleted", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.forge.headErr = forge.ErrRefNotFound
		}, apitypes.RecoveryRedundancyBranchMissing},
		{"failed_run", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, _ *apitypes.RecoveryArchiveRedundancyRequest) {
			r.e.exec(t, "UPDATE runs SET status='failed' WHERE id=$1", r.run)
		}, apitypes.RecoveryRedundancyNotCompleted},
		{"open_hold", redundancyLiveOpts{vec: "small", noReport: true}, nil, apitypes.RecoveryRedundancyNotCompleted},
		{"no_identity", redundancyLiveOpts{vec: "small", noID: true}, nil, apitypes.RecoveryRedundancyIdentityMissing},
		{"null_mr_iid", redundancyLiveOpts{vec: "small", nullMR: true}, nil, apitypes.RecoveryRedundancyMRMissing},
		{"current_object_is_another_commit", redundancyLiveOpts{vec: "small"}, func(r *redundancyLive, req *apitypes.RecoveryArchiveRedundancyRequest) {
			body, _ := synthCommit(strings.Repeat("4", 40), nil, "unrelated")
			req.CurrentObject = b64s(body)
		}, apitypes.RecoveryRedundancyBadObject},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRedundancyLive(t, c.opts)
			req := r.claim()
			if c.edit != nil {
				c.edit(r, &req)
			}
			r.wantRetained(t, r.call(t, req), c.reason)
		})
	}
}

// (3h) a forge-derived refusal is remembered for an hour: the repeat makes no forge call.
func TestArchiveRedundancyCooldownLiveDB(t *testing.T) {
	r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
	r.forge.answers[r.vec.Roots[1]] = forge.AncestryNotAncestor
	r.wantRetained(t, r.call(t, r.claim()), apitypes.RecoveryRedundancyUncoveredRoot)
	if c := r.row(t); c.Refusal != apitypes.RecoveryRedundancyUncoveredRoot || !c.RefusedAtSet {
		t.Fatalf("memo not recorded: %+v", c)
	}
	calls := r.forge.callCount()
	// The fix landed on the branch, but the hour has not passed.
	r.forge.answers[r.vec.Roots[1]] = forge.AncestryAncestor
	r.wantRetained(t, r.call(t, r.claim()), apitypes.RecoveryRedundancyCoolingDown)
	if r.forge.callCount() != calls {
		t.Fatalf("cool-down made %d forge calls", r.forge.callCount()-calls)
	}
	r.e.exec(t, "UPDATE recovery_captures SET redundancy_refused_at=now()-interval '2 hours' WHERE id=$1", r.capture)
	r.wantExpired(t, r.call(t, r.claim()))
}

// (4) protected final capture of a receipt-refused completed run (ADR-2417).
func TestArchiveRedundancyProtectedFinalCaptureLiveDB(t *testing.T) {
	isGuardRefusal := func(t *testing.T, err error) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55000" {
			t.Fatalf("want SQLSTATE 55000, got %v", err)
		}
	}
	// A statement with the TTL sweep's shape: delete the chunks, then flip the state.
	flip := func(set string) string {
		return `WITH expiring AS (SELECT $1::uuid AS capture_id),
 del AS (DELETE FROM recovery_capture_chunks WHERE capture_id IN (SELECT capture_id FROM expiring) RETURNING capture_id)
 UPDATE recovery_captures SET ` + set + ` WHERE id IN (SELECT capture_id FROM expiring) AND (SELECT count(*) FROM del)>=0`
	}
	proofJSON := func(r *redundancyLive, edit func(map[string]any)) string {
		p := map[string]any{
			"hold_id": r.hold.String(), "generation": 1, "coverage_digest": r.vec.Digest, "source_sha": r.vec.SourceSha,
			"current_sha": r.vec.CurrentSha, "tree": r.vec.Tree, "mr_iid": 7, "final_head": r.final,
			"anchor_head": r.head, "proved_at": "2026-10-10T00:00:00Z",
		}
		if edit != nil {
			edit(p)
		}
		b, _ := json.Marshal(p)
		return string(b)
	}
	flipWithProof := func(r *redundancyLive, proof string) error {
		_, err := r.e.pool.Exec(r.e.ctx, flip(`state='expired', reason='published_redundant', local_replica_worker_id=NULL, expires_at=LEAST(expires_at, now()), redundancy_proof=$2::jsonb`), r.capture, proof)
		return err
	}

	t.Run("without_proof_stays_protected", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small", ack: true})
		if c := r.row(t); !c.LocalReplica.Valid {
			t.Fatalf("not protected: %+v", c)
		}
		_, err := r.e.pool.Exec(r.e.ctx, flip(`state='expired'`), r.capture)
		isGuardRefusal(t, err)
		_, err = r.e.pool.Exec(r.e.ctx, "UPDATE recovery_captures SET state='expired' WHERE id=$1", r.capture)
		isGuardRefusal(t, err)
		// The TTL sweep skips a protected capture whatever the clock says.
		if _, err := r.e.q.ExpireReadyCaptures(r.e.ctx, pgtype.Timestamptz{Time: time.Now().Add(30 * 24 * time.Hour), Valid: true}); err != nil {
			t.Fatal(err)
		}
		if c := r.row(t); c.State != "available" || !c.LocalReplica.Valid {
			t.Fatalf("TTL sweep touched a protected capture: %+v", c)
		}
		if n := r.chunks(t); n != 1 {
			t.Fatalf("chunks %d", n)
		}
	})
	// The proof check is the guard's own: it raises for a mismatched proof whether or not the
	// capture is a protected final one, so it cannot hide behind the older final-capture raise.
	wantMismatch := func(t *testing.T, err error) {
		t.Helper()
		isGuardRefusal(t, err)
		if !strings.Contains(err.Error(), "redundancy proof does not match its capture") {
			t.Fatalf("refused for another reason: %v", err)
		}
	}
	t.Run("mismatched_proof_on_unprotected_capture_raises", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
		wantMismatch(t, flipWithProof(r, proofJSON(r, func(p map[string]any) { p["final_head"] = strings.Repeat("e", 40) })))
		if c := r.row(t); c.State != "available" || len(c.Proof) != 0 {
			t.Fatalf("row changed: %+v", c)
		}
		if n := r.chunks(t); n != 1 {
			t.Fatalf("chunks %d", n)
		}
	})
	t.Run("mismatched_proof_raises_and_keeps_bytes", func(t *testing.T) {
		for name, edit := range map[string]func(map[string]any){
			"final_head":     func(p map[string]any) { p["final_head"] = strings.Repeat("e", 40) },
			"digest":         func(p map[string]any) { p["coverage_digest"] = strings.Repeat("f", 64) },
			"hold":           func(p map[string]any) { p["hold_id"] = uuid.NewString() },
			"generation":     func(p map[string]any) { p["generation"] = 2 },
			"mr_iid":         func(p map[string]any) { p["mr_iid"] = 8 },
			"null_mr_iid":    func(p map[string]any) { p["mr_iid"] = nil },
			"source_sha":     func(p map[string]any) { p["source_sha"] = strings.Repeat("1", 40) },
			"no_anchor":      func(p map[string]any) { delete(p, "anchor_head") },
			"no_tree":        func(p map[string]any) { p["tree"] = "TREE" },
			"no_current_sha": func(p map[string]any) { delete(p, "current_sha") },
		} {
			t.Run(name, func(t *testing.T) {
				r := newRedundancyLive(t, redundancyLiveOpts{vec: "small", ack: true})
				wantMismatch(t, flipWithProof(r, proofJSON(r, edit)))
				if c := r.row(t); c.State != "available" || len(c.Proof) != 0 || !c.LocalReplica.Valid {
					t.Fatalf("row changed: %+v", c)
				}
				if n := r.chunks(t); n != 1 {
					t.Fatalf("chunks %d", n)
				}
			})
		}
	})
	t.Run("proof_on_another_transition_raises", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small", ack: true})
		_, err := r.e.pool.Exec(r.e.ctx, "UPDATE recovery_captures SET state='discarded', reason='published_redundant', redundancy_proof=$2::jsonb WHERE id=$1", r.capture, proofJSON(r, nil))
		isGuardRefusal(t, err)
		if c := r.row(t); c.State != "available" || len(c.Proof) != 0 {
			t.Fatalf("row changed: %+v", c)
		}
	})
	t.Run("proof_on_unprotected_run_state_raises", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small", ack: true})
		r.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", r.run)
		isGuardRefusal(t, flipWithProof(r, proofJSON(r, nil)))
		if n := r.chunks(t); n != 1 {
			t.Fatalf("chunks %d", n)
		}
	})
	t.Run("valid_proof_expires", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small", ack: true})
		r.wantExpired(t, r.call(t, r.claim()))
		// An expired-by-proof final capture can be touched again without tripping the window arm.
		r.e.exec(t, "UPDATE recovery_captures SET updated_at=now() WHERE id=$1", r.capture)
		for name, set := range map[string]string{
			"proof":  "redundancy_proof=NULL, reason=NULL",
			"reason": "reason='unaccepted_capture_replaced', redundancy_proof=NULL",
			"edit":   `redundancy_proof=redundancy_proof || '{"tree":"x"}'::jsonb`,
		} {
			_, err := r.e.pool.Exec(r.e.ctx, "UPDATE recovery_captures SET "+set+" WHERE id=$1", r.capture)
			if err == nil {
				t.Fatalf("%s: a recorded proof must be immutable", name)
			}
		}
	})
	t.Run("ttl_sweep_first_returns_not_available", func(t *testing.T) {
		r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
		r.e.exec(t, "UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", r.capture)
		if n, err := r.e.q.ExpireReadyCaptures(r.e.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true}); err != nil || n < 1 {
			t.Fatalf("sweep: %d %v", n, err)
		}
		res := r.call(t, r.claim())
		if res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != apitypes.RecoveryRedundancyNotAvailable {
			t.Fatalf("%+v", res)
		}
		if c := r.row(t); c.State != "expired" || c.Reason == "published_redundant" || len(c.Proof) != 0 {
			t.Fatalf("TTL-expired row was rewritten: %+v", c)
		}
	})
}

// (3j) The 00305 guard silently drops an UPDATE that changes the identity tuple (RETURN NULL), so
// a Fence-shaped statement returns zero rows and deletes no chunk. The service therefore treats
// anything but exactly one returned row as not expired.
func TestArchiveRedundancyGuardSilentDropReturnsZeroRowsLiveDB(t *testing.T) {
	r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
	rows, err := r.e.pool.Query(r.e.ctx, `WITH flipped AS (
 UPDATE recovery_captures c SET state='expired', reason='unaccepted_capture_replaced', source_sha=$2
 WHERE c.id=$1 RETURNING c.id
), deleted AS (DELETE FROM recovery_capture_chunks WHERE capture_id IN (SELECT id FROM flipped) RETURNING capture_id)
SELECT id FROM flipped WHERE (SELECT count(*) FROM deleted) >= 0`, r.capture, strings.Repeat("7", 40))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil || n != 0 {
		t.Fatalf("guard silent drop returned %d rows, err %v", n, err)
	}
	if c := r.row(t); c.State != "available" {
		t.Fatalf("state %q", c.State)
	}
	if got := r.chunks(t); got != 1 {
		t.Fatalf("chunks %d", got)
	}
	// The real mutation statement, with an identity that does not match the stored hold, also
	// returns no row and leaves the bytes.
	ids, err := r.e.q.ExpireRedundantCapture(r.e.ctx, store.ExpireRedundantCaptureParams{
		Proof: []byte(`{}`), ID: r.capture, HoldID: r.hold, RunID: r.run, UserID: r.e.userID, WorkerID: r.w.ID,
		CoverageDigest: r.vec.Digest, SourceSha: r.vec.SourceSha, Generation: r.gen, Identity: []byte(`{"hold_id":"x"}`),
	})
	if err != nil || len(ids) != 0 {
		t.Fatalf("mismatched identity expired rows %v: %v", ids, err)
	}
	if got := r.chunks(t); got != 1 {
		t.Fatalf("chunks %d", got)
	}
}

// (5) the table constraints that back the guard. INSERT reaches them without the UPDATE guard.
func TestArchiveRedundancyConstraintsLiveDB(t *testing.T) {
	r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
	insert := func(name, cols, vals string) {
		t.Helper()
		_, err := r.e.pool.Exec(r.e.ctx, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state`+cols+`)
 VALUES(gen_random_uuid(),$1,$2,$3,$4,'ident',$5,gen_random_uuid()::text,'expired'`+vals+`)`, r.hold, r.run, r.e.userID, r.w.ID, r.vec.SourceSha)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatalf("%s: want check violation 23514, got %v", name, err)
		}
	}
	insert("reason without proof", ",reason", ",'published_redundant'")
	insert("proof without reason", ",redundancy_proof", ",'{}'::jsonb")
	insert("proof with another reason", ",reason,redundancy_proof", ",'other','{}'::jsonb")
	insert("refusal without time", ",redundancy_refusal", ",'x'")
	insert("time without refusal", ",redundancy_refused_at", ",now()")
	// A proof can only be attached by the guarded available-to-expired transition.
	_, err := r.e.pool.Exec(r.e.ctx, "UPDATE recovery_captures SET redundancy_proof='{}'::jsonb WHERE id=$1", r.capture)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55000" {
		t.Fatalf("proof outside the expiry transition: %v", err)
	}
	if _, err := r.e.pool.Exec(r.e.ctx, "UPDATE recovery_captures SET reason='published_redundant' WHERE id=$1", r.capture); err == nil {
		t.Fatal("a reason without a proof must violate the check")
	} else if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("reason without proof: %v", err)
	}
}

// zeroRowsTx runs the real locked reads and writes but aims the expiry at a source_sha the capture
// does not have, so the statement's WHERE matches nothing, as a lost race or a guard drop would.
type zeroRowsTx struct{ *store.Queries }

func (z zeroRowsTx) ExpireRedundantCapture(ctx context.Context, a store.ExpireRedundantCaptureParams) ([]uuid.UUID, error) {
	a.SourceSha = strings.Repeat("0", 40)
	return z.Queries.ExpireRedundantCapture(ctx, a)
}

// Service-level (3j): zero returned rows is a refusal with the transaction rolled back and the
// archive untouched, on the real statement and real locks.
func TestArchiveRedundancyZeroRowsIsNotExpiredLiveDB(t *testing.T) {
	r := newRedundancyLive(t, redundancyLiveOpts{vec: "small"})
	p := r.svc.archiveRedundancyProver()
	p.txq = func(tx pgx.Tx) archiveRedundancyTxStore { return zeroRowsTx{store.New(tx)} }
	res, err := p.prove(r.e.ctx, r.w, r.run, r.capture, r.claim())
	if err != nil {
		t.Fatal(err)
	}
	r.wantRetained(t, res, apitypes.RecoveryRedundancyNotExpired)
	// Nothing was left half-done: a normal claim afterwards still succeeds.
	r.wantExpired(t, r.call(t, r.claim()))
}
