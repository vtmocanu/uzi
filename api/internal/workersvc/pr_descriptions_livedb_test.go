package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1798 D9 live-DB coverage of the PR-description artifact: the real migration, the real
// sqlc statements and the real fenced transaction (row locks, CAS on lock_version, claim
// generation fence). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).

const (
	prDescLiveHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prDescLiveHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	prDescLiveHashC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type prDescLive struct {
	env   codexTestEnv
	svc   *Service
	wkr   store.Worker
	runID uuid.UUID
	repo  uuid.UUID
}

func setupPrDescLive(t *testing.T) prDescLive {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET claim_generation = 1 WHERE id = $1`, runID)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	return prDescLive{env: env, svc: svc, wkr: store.Worker{ID: workerID, UserID: userID}, runID: runID, repo: repoID}
}

func gen(v int64) *int64 { return &v }

func (p prDescLive) stage(t *testing.T, g int64, summary string, mr *int64) apitypes.PrDescriptionVersionDTO {
	t.Helper()
	v, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(g), Source: "generated",
		Fields: apitypes.PrDescriptionFields{
			Summary: summary, Changes: []string{"one change"},
			Verification: []apitypes.PrDescriptionVerification{{Command: "task gate:api", Result: "pass", VerifiedAtSha: "abcdef0"}},
		},
		Size:    &apitypes.PrDescriptionSize{Files: 2, Tests: apitypes.PrDescriptionSizeBucket{Added: 5}},
		BaseSha: strings.Repeat("1", 40), HeadSha: strings.Repeat("2", 40), TargetBranch: "main", MrIid: mr,
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return v
}

func (p prDescLive) bind(t *testing.T, g int64, versionID string, mr int64, hash string) apitypes.PrDescriptionBindResponse {
	t.Helper()
	r, err := p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: gen(g), VersionID: versionID, MrIid: mr, RenderedRegionSha256: hash,
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	return r
}

func (p prDescLive) ack(g int64, versionID, outcome string, lock int64, observed *string) (apitypes.PrDescriptionAckResponse, error) {
	return p.svc.AckPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionAckRequest{
		ClaimGeneration: gen(g), VersionID: versionID, Outcome: outcome, ExpectedLockVersion: lock, ObservedRegionSha256: observed,
	})
}

// dbPR reads the pr_descriptions row straight from the table.
func (p prDescLive) dbPR(t *testing.T, mr int64) (published *uuid.UUID, lock int64, outcome *string) {
	t.Helper()
	if err := p.env.pool.QueryRow(p.env.ctx,
		`SELECT published_version_id, lock_version, last_outcome FROM pr_descriptions WHERE repo_id = $1 AND mr_iid = $2`,
		p.repo, mr).Scan(&published, &lock, &outcome); err != nil {
		t.Fatalf("read pr_descriptions: %v", err)
	}
	return published, lock, outcome
}

func (p prDescLive) dbVersionState(t *testing.T, id string) string {
	t.Helper()
	var state string
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT state FROM pr_description_versions WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read version state: %v", err)
	}
	return state
}

func TestPrDescriptionPublishAndStaleLockLiveDB(t *testing.T) {
	p := setupPrDescLive(t)

	v1 := p.stage(t, 1, "Adds retries. Closes #7 <!-- uzi:description:start v1 -->", nil)
	if strings.Contains(v1.Fields.Summary, "Closes #7") || strings.Contains(v1.Fields.Summary, "<!--") {
		t.Fatalf("stored summary not sanitized: %q", v1.Fields.Summary)
	}
	b := p.bind(t, 1, v1.ID, 41, prDescLiveHashA)
	if b.PR.LockVersion != 0 || b.PR.PublishedVersion != nil {
		t.Fatalf("fresh PR state = %+v", b.PR)
	}
	a, err := p.ack(1, v1.ID, "published", 0, nil)
	if err != nil {
		t.Fatalf("ack v1: %v", err)
	}
	if a.PR.LockVersion != 1 || a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != v1.ID ||
		a.PR.PublishedVersion.Size == nil || a.PR.PublishedVersion.Size.Tests.Added != 5 {
		t.Fatalf("after publish = %+v", a.PR)
	}

	// A second version acked with the stale lock_version 0 loses the CAS and changes nothing.
	v2 := p.stage(t, 1, "Second pass.", gen(41))
	p.bind(t, 1, v2.ID, 41, prDescLiveHashB)
	if _, err := p.ack(1, v2.ID, "published", 0, nil); !errors.Is(err, ErrPrDescriptionLockConflict) {
		t.Fatalf("stale lock ack err = %v, want ErrPrDescriptionLockConflict", err)
	}
	pub, lock, outcome := p.dbPR(t, 41)
	if pub == nil || pub.String() != v1.ID || lock != 1 || outcome == nil || *outcome != "published" {
		t.Fatalf("a lost CAS must change nothing: published=%v lock=%d outcome=%v", pub, lock, outcome)
	}
	if st := p.dbVersionState(t, v2.ID); st != "pending" {
		t.Fatalf("v2 state after the lost CAS = %q, want pending", st)
	}
	// The same ack at the current lock_version wins.
	if a, err = p.ack(1, v2.ID, "published", 1, nil); err != nil || a.PR.PublishedVersion.ID != v2.ID || a.PR.LockVersion != 2 {
		t.Fatalf("current-lock ack = %+v, %v", a, err)
	}

	// The DTO overlay and the claim read the published version.
	run := mustRun(t, p.env, p.runID)
	desc, last, err := p.svc.RunPrDescription(p.env.ctx, run)
	if err != nil || desc == nil || desc.MrIid != 41 || desc.Fields.Summary != "Second pass." || desc.PublishedAt == nil ||
		last == nil || *last != "published" {
		t.Fatalf("RunPrDescription = %+v %v %v", desc, last, err)
	}
}

func TestPrDescriptionStaleAndReleasedClaimLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v := p.stage(t, 1, "Summary.", nil)
	p.bind(t, 1, v.ID, 7, prDescLiveHashA)

	// A stale generation loses on every write.
	if _, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(0), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main",
	}); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("stale stage err = %v", err)
	}
	if _, err := p.ack(0, v.ID, "published", 0, nil); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("stale ack err = %v", err)
	}

	// A released claim (a held-state switch) loses even at the right generation.
	p.env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, p.runID)
	if _, err := p.ack(1, v.ID, "published", 0, nil); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("released-claim ack err = %v", err)
	}
	if _, err := p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: gen(1), VersionID: v.ID, MrIid: 7, RenderedRegionSha256: prDescLiveHashA,
	}); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("released-claim bind err = %v", err)
	}
	pub, lock, outcome := p.dbPR(t, 7)
	if pub != nil || lock != 0 || outcome != nil {
		t.Fatalf("stale writes must change nothing: %v %d %v", pub, lock, outcome)
	}

	// Another worker (same user) holding no claim on the run is refused as not-owned.
	other := uuid.New()
	p.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		other, p.wkr.UserID, "w-"+other.String(), other[:])
	if _, err := p.svc.AckPrDescription(p.env.ctx, store.Worker{ID: other, UserID: p.wkr.UserID}, p.runID,
		apitypes.PrDescriptionAckRequest{ClaimGeneration: gen(1), VersionID: v.ID, Outcome: "published"}); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign worker ack err = %v, want ErrRunNotOwned", err)
	}
}

// TestPrDescriptionReclaimAfterRequeueLiveDB: after a requeue and reclaim (claim_generation
// advances), the old flight loses on every call, the new flight's version wins, and the new
// flight cannot ack the old flight's version.
func TestPrDescriptionReclaimAfterRequeueLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	old := p.stage(t, 1, "Old flight.", nil)
	p.bind(t, 1, old.ID, 9, prDescLiveHashA)

	// Requeue + reclaim by the same worker: the claim lane bumps the generation.
	p.env.exec(`UPDATE runs SET claim_generation = 2, status = 'running' WHERE id = $1`, p.runID)

	if _, err := p.ack(1, old.ID, "published", 0, nil); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("old flight ack err = %v, want stale", err)
	}
	if _, err := p.ack(2, old.ID, "published", 0, nil); !errors.Is(err, ErrPrDescriptionStaleClaim) {
		t.Fatalf("new flight acking the old flight's version err = %v, want stale", err)
	}
	fresh := p.stage(t, 2, "New flight.", gen(9))
	b := p.bind(t, 2, fresh.ID, 9, prDescLiveHashB)
	a, err := p.ack(2, fresh.ID, "published", b.PR.LockVersion, nil)
	if err != nil || a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != fresh.ID || a.PR.PublishedVersion.ClaimGeneration != 2 {
		t.Fatalf("new flight ack = %+v, %v", a, err)
	}
	if st := p.dbVersionState(t, old.ID); st != "pending" {
		t.Fatalf("old version state = %q, want it left pending", st)
	}
}

// TestPrDescriptionSkippedWriteKeepsPublishedLiveDB: a skipped or failed write records only
// last_outcome and abandons its version; published_version_id still names what is on the forge.
func TestPrDescriptionSkippedWriteKeepsPublishedLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v1 := p.stage(t, 1, "First.", nil)
	p.bind(t, 1, v1.ID, 3, prDescLiveHashA)
	if _, err := p.ack(1, v1.ID, "published", 0, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	lock := int64(1)
	for _, outcome := range []string{"skipped_human_edit", "skipped_no_region", "skipped_malformed", "skipped_snapshot_moved", "write_failed"} {
		v := p.stage(t, 1, "Refresh "+outcome, gen(3))
		p.bind(t, 1, v.ID, 3, prDescLiveHashB)
		a, err := p.ack(1, v.ID, outcome, lock, nil)
		if err != nil {
			t.Fatalf("%s ack: %v", outcome, err)
		}
		lock++
		if a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != v1.ID || *a.PR.LastOutcome != outcome || a.PR.LockVersion != lock {
			t.Fatalf("%s: state = %+v", outcome, a.PR)
		}
		if st := p.dbVersionState(t, v.ID); st != "abandoned" {
			t.Fatalf("%s: version state = %q, want abandoned", outcome, st)
		}
		pub, _, _ := p.dbPR(t, 3)
		if pub == nil || pub.String() != v1.ID {
			t.Fatalf("%s: published_version_id moved to %v", outcome, pub)
		}
	}
	run := mustRun(t, p.env, p.runID)
	desc, last, err := p.svc.RunPrDescription(p.env.ctx, run)
	if err != nil || desc == nil || desc.Fields.Summary != "First." || last == nil || *last != "write_failed" {
		t.Fatalf("overlay after skips = %+v %v %v", desc, last, err)
	}
}

func TestPrDescriptionDiagramBindAndLostAckLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	lost := p.stage(t, 1, "Diagram written, ack lost.", nil)
	bind := func(id, hash string, flag *bool) (apitypes.PrDescriptionBindResponse, error) {
		return p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
			ClaimGeneration: gen(1), VersionID: id, MrIid: 5, RenderedRegionSha256: hash, RegionHasDiagram: flag,
		})
	}
	first, err := bind(lost.ID, prDescLiveHashA, genBool(true))
	if err != nil || first.Version.RegionHasDiagram == nil || !*first.Version.RegionHasDiagram {
		t.Fatalf("bind diagram flag = %+v, %v", first.Version, err)
	}
	if retry, err := bind(lost.ID, prDescLiveHashA, genBool(true)); err != nil || retry.Version.RegionHasDiagram == nil || !*retry.Version.RegionHasDiagram {
		t.Fatalf("idempotent bind = %+v, %v", retry.Version, err)
	}
	for _, flag := range []*bool{nil, genBool(false)} {
		if _, err := bind(lost.ID, prDescLiveHashA, flag); !errors.Is(err, ErrPrDescriptionVersionConflict) {
			t.Fatalf("changed flag %v: %v", flag, err)
		}
	}
	next := p.stage(t, 1, "Next write.", gen(5))
	if _, err := bind(next.ID, prDescLiveHashB, nil); err != nil {
		t.Fatalf("legacy bind: %v", err)
	}
	observed := prDescLiveHashA
	ack, err := p.ack(1, next.ID, "skipped_human_edit", 0, &observed)
	if err != nil || ack.RecoveredVersionID == nil || *ack.RecoveredVersionID != lost.ID ||
		ack.PR.PublishedVersion == nil || ack.PR.PublishedVersion.RegionHasDiagram == nil || !*ack.PR.PublishedVersion.RegionHasDiagram {
		t.Fatalf("lost ack recovery = %+v, %v", ack, err)
	}
	run := mustRun(t, p.env, p.runID)
	desc, _, err := p.svc.RunPrDescription(p.env.ctx, run)
	if err != nil || desc == nil || !desc.DiagramPublished {
		t.Fatalf("run diagram published = %+v, %v", desc, err)
	}
}

func TestPrDescriptionDiagramPublishedTransitionsLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "generated",
		Fields: apitypes.PrDescriptionFields{Summary: "With diagram.", Diagram: &apitypes.PrDescriptionDiagram{
			Kind: "flow", Title: "Flow",
			Nodes: []apitypes.PrDescriptionDiagramNode{{Key: "a", Label: "Start"}, {Key: "b", Label: "Work"}, {Key: "c", Label: "Done"}},
			Edges: []apitypes.PrDescriptionDiagramEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
		}},
		BaseSha: strings.Repeat("1", 40), HeadSha: strings.Repeat("2", 40), TargetBranch: "main",
	})
	if err != nil || v.Fields.Diagram == nil {
		t.Fatalf("stage diagram = %+v, %v", v, err)
	}
	bind := func(id, hash string, flag bool) apitypes.PrDescriptionBindResponse {
		t.Helper()
		r, err := p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
			ClaimGeneration: gen(1), VersionID: id, MrIid: 5, RenderedRegionSha256: hash, RegionHasDiagram: genBool(flag),
		})
		if err != nil {
			t.Fatalf("bind %s: %v", id, err)
		}
		return r
	}
	checkRun := func(want *bool) {
		t.Helper()
		desc, _, err := p.svc.RunPrDescription(p.env.ctx, mustRun(t, p.env, p.runID))
		if err != nil || (want == nil && desc != nil) || (want != nil && (desc == nil || desc.DiagramPublished != *want)) {
			t.Fatalf("RunPrDescription = %+v, %v; want diagram_published %v", desc, err, want)
		}
	}
	checkFlag := func(id string, want bool) {
		t.Helper()
		var flag *bool
		if err := p.env.pool.QueryRow(p.env.ctx, `SELECT region_has_diagram FROM pr_description_versions WHERE id = $1`, id).Scan(&flag); err != nil || flag == nil || *flag != want {
			t.Fatalf("stored region_has_diagram for %s = %v, %v; want %t", id, flag, err, want)
		}
	}
	checkRun(nil) // A staged version is not a published run description.
	b := bind(v.ID, prDescLiveHashA, true)
	if b.Version.RegionHasDiagram == nil || !*b.Version.RegionHasDiagram || b.PR.PublishedVersion != nil {
		t.Fatalf("bound pending version = %+v", b)
	}
	checkFlag(v.ID, true)
	checkRun(nil) // Binding alone cannot expose pending content in the run DTO.
	a, err := p.ack(1, v.ID, "published", 0, nil)
	if err != nil || a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != v.ID ||
		a.PR.PublishedVersion.RegionHasDiagram == nil || !*a.PR.PublishedVersion.RegionHasDiagram {
		t.Fatalf("publish diagram = %+v, %v", a, err)
	}
	checkRun(genBool(true))
	if retry, err := p.ack(1, v.ID, "published", 0, nil); err != nil || retry.PR.PublishedVersion == nil ||
		retry.PR.PublishedVersion.ID != v.ID || retry.PR.PublishedVersion.RegionHasDiagram == nil || !*retry.PR.PublishedVersion.RegionHasDiagram {
		t.Fatalf("lost ack retry = %+v, %v", retry, err)
	}
	checkFlag(v.ID, true)
	checkRun(genBool(true))

	attempt := p.stage(t, 1, "Attempt without diagram.", gen(5))
	bind(attempt.ID, prDescLiveHashB, false)
	if skipped, err := p.ack(1, attempt.ID, "skipped_human_edit", 1, nil); err != nil ||
		skipped.PR.PublishedVersion == nil || skipped.PR.PublishedVersion.ID != v.ID ||
		skipped.PR.PublishedVersion.RegionHasDiagram == nil || !*skipped.PR.PublishedVersion.RegionHasDiagram {
		t.Fatalf("skipped attempt changed publication = %+v, %v", skipped, err)
	}
	checkRun(genBool(true))
	checkFlag(v.ID, true)

	plain := p.stage(t, 1, "Published without diagram.", gen(5))
	if plain.Fields.Diagram != nil {
		t.Fatalf("diagram-less stage = %+v", plain.Fields)
	}
	bind(plain.ID, prDescLiveHashC, false)
	if final, err := p.ack(1, plain.ID, "published", 2, nil); err != nil || final.PR.PublishedVersion == nil ||
		final.PR.PublishedVersion.ID != plain.ID || final.PR.PublishedVersion.RegionHasDiagram == nil || *final.PR.PublishedVersion.RegionHasDiagram {
		t.Fatalf("publish diagram-less version = %+v, %v", final, err)
	}
	checkFlag(plain.ID, false)
	checkRun(genBool(false))
}

func genBool(v bool) *bool { return &v }

// TestPrDescriptionLostAckRecoveryLiveDB: a version whose forge write landed but whose ack was
// lost is recovered by the next ack that observes its region hash, before that ack applies.
func TestPrDescriptionLostAckRecoveryLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	lost := p.stage(t, 1, "Written, ack lost.", nil)
	p.bind(t, 1, lost.ID, 5, prDescLiveHashA)

	// Lookup classifies the forge region as a pending (lost-ack) version.
	look, err := p.svc.LookupPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionLookupRequest{
		ClaimGeneration: gen(1), MrIid: 5, RegionSha256: prDescLiveHashA,
	})
	if err != nil || look.Match != "pending" || look.MatchedVersionID == nil || *look.MatchedVersionID != lost.ID ||
		look.PR == nil || look.PR.PublishedVersion != nil {
		t.Fatalf("lookup = %+v, %v", look, err)
	}
	if look, err = p.svc.LookupPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionLookupRequest{
		ClaimGeneration: gen(1), MrIid: 5, RegionSha256: prDescLiveHashC,
	}); err != nil || look.Match != "none" {
		t.Fatalf("unknown region lookup = %+v, %v", look, err)
	}

	// The next writer observes hash A on the forge and publishes its own version.
	next := p.stage(t, 1, "Next write.", gen(5))
	p.bind(t, 1, next.ID, 5, prDescLiveHashB)
	observed := prDescLiveHashA
	a, err := p.ack(1, next.ID, "published", 0, &observed)
	if err != nil {
		t.Fatalf("ack with recovery: %v", err)
	}
	if a.RecoveredVersionID == nil || *a.RecoveredVersionID != lost.ID {
		t.Fatalf("recovered = %v, want the lost version", a.RecoveredVersionID)
	}
	if a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != next.ID || a.PR.LockVersion != 1 {
		t.Fatalf("after recovery + publish = %+v", a.PR)
	}
	if st := p.dbVersionState(t, lost.ID); st != "published" {
		t.Fatalf("lost version state = %q, want published", st)
	}

	// Recovery before a SKIP: the recovered version stays the published one.
	lost2 := p.stage(t, 1, "Written again, ack lost again.", gen(5))
	p.bind(t, 1, lost2.ID, 5, prDescLiveHashC)
	skip := p.stage(t, 1, "Human edited.", gen(5))
	p.bind(t, 1, skip.ID, 5, prDescLiveHashB)
	observed = prDescLiveHashC
	a, err = p.ack(1, skip.ID, "skipped_human_edit", 1, &observed)
	if err != nil || a.RecoveredVersionID == nil || *a.RecoveredVersionID != lost2.ID ||
		a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != lost2.ID || *a.PR.LastOutcome != "skipped_human_edit" {
		t.Fatalf("recovery before skip = %+v, %v", a, err)
	}
	if look, err = p.svc.LookupPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionLookupRequest{
		ClaimGeneration: gen(1), MrIid: 5, RegionSha256: prDescLiveHashC,
	}); err != nil || look.Match != "published" {
		t.Fatalf("lookup after recovery = %+v, %v", look, err)
	}
}

// TestAssembleClaimCarriesPrDescriptionLiveDB: a run whose PR already exists gets the PR's
// record in its claim, found by runs.mr_iid; a re-claimed issue run with no runs.mr_iid finds it
// through the latest version it bound; a run with no PR record carries none.
func TestAssembleClaimCarriesPrDescriptionLiveDB(t *testing.T) {
	p := setupPrDescLive(t)

	// A real claim assembly opens the bot PAT and an Anthropic token (see
	// claim_pause_pending_livedb_test.go).
	botPATSealed, err := p.env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	p.env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, p.wkr.UserID)
	anthropicSealed, err := p.env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	p.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), p.wkr.UserID, "anthropic-"+uuid.NewString(), anthropicSealed)

	// No record yet: the claim carries no pr_description.
	payload, err := p.svc.assembleClaim(p.env.ctx, p.wkr, mustRun(t, p.env, p.runID))
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	if payload.PrDescription != nil {
		t.Fatalf("a run with no PR record must carry no pr_description, got %+v", payload.PrDescription)
	}

	v := p.stage(t, 1, "Published text.", nil)
	p.bind(t, 1, v.ID, 11, prDescLiveHashA)
	if _, err := p.ack(1, v.ID, "published", 0, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Re-claimed issue run, runs.mr_iid never recorded: found through the bound version.
	payload, err = p.svc.assembleClaim(p.env.ctx, p.wkr, mustRun(t, p.env, p.runID))
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	pd := payload.PrDescription
	if pd == nil || pd.MrIid != 11 || pd.LockVersion != 1 || pd.PublishedVersion == nil ||
		pd.PublishedVersion.ID != v.ID || pd.PublishedVersion.RenderedRegionSha256 == nil ||
		*pd.PublishedVersion.RenderedRegionSha256 != prDescLiveHashA || pd.PublishedVersion.HeadSha != strings.Repeat("2", 40) ||
		pd.PublishedVersion.TargetBranch != "main" || pd.PublishedVersion.Fields.Summary != "Published text." {
		t.Fatalf("claim pr_description = %+v", pd)
	}

	// A refresh run on the same PR (a different run, runs.mr_iid set, as an mr_rework run has).
	refreshID := uuid.New()
	p.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, mr_iid)
	        VALUES ($1, $2, $3, 'issue', 2, 't', 'd', 'claimed', $4, 11)`, refreshID, p.wkr.UserID, p.repo, p.wkr.ID)
	payload, err = p.svc.assembleClaim(p.env.ctx, p.wkr, mustRun(t, p.env, refreshID))
	if err != nil {
		t.Fatalf("assembleClaim refresh: %v", err)
	}
	if payload.PrDescription == nil || payload.PrDescription.PublishedVersion == nil || payload.PrDescription.PublishedVersion.ID != v.ID {
		t.Fatalf("refresh claim pr_description = %+v", payload.PrDescription)
	}
}

// TestPrDescriptionLostAckNeverMovesBackwardsLiveDB (review N1), against the real SQL: V1 wrote
// region A and lost its ack, V2 wrote B and acked; V3 observing A (a human restored the older
// text) must NOT recover V1 over the newer published V2. A pending version rendering the same
// region as the published one is not recovered either; a NEWER lost ack still is.
func TestPrDescriptionLostAckNeverMovesBackwardsLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v1 := p.stage(t, 1, "V1.", nil)
	p.bind(t, 1, v1.ID, 21, prDescLiveHashA)
	v2 := p.stage(t, 1, "V2.", gen(21))
	p.bind(t, 1, v2.ID, 21, prDescLiveHashB)
	if _, err := p.ack(1, v2.ID, "published", 0, nil); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	v3 := p.stage(t, 1, "V3.", gen(21))
	p.bind(t, 1, v3.ID, 21, prDescLiveHashC)
	observed := prDescLiveHashA
	a, err := p.ack(1, v3.ID, "skipped_human_edit", 1, &observed)
	if err != nil || a.RecoveredVersionID != nil || a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != v2.ID {
		t.Fatalf("older pending recovered over the published version: %+v, %v", a, err)
	}
	if pub, _, _ := p.dbPR(t, 21); pub == nil || pub.String() != v2.ID {
		t.Fatalf("published_version_id = %v, want v2", pub)
	}
	if st := p.dbVersionState(t, v1.ID); st != "pending" {
		t.Fatalf("v1 state = %q, want pending", st)
	}

	// Same region as the published V2: nothing to recover.
	same := p.stage(t, 1, "Same text as V2.", gen(21))
	p.bind(t, 1, same.ID, 21, prDescLiveHashB)
	v4 := p.stage(t, 1, "V4.", gen(21))
	p.bind(t, 1, v4.ID, 21, prDescLiveHashC)
	observed = prDescLiveHashB
	if a, err = p.ack(1, v4.ID, "skipped_human_edit", 2, &observed); err != nil || a.RecoveredVersionID != nil || a.PR.PublishedVersion.ID != v2.ID {
		t.Fatalf("recovery ran on the published version's own region: %+v, %v", a, err)
	}

	// A newer lost ack is recovered.
	newer := p.stage(t, 1, "Newer, ack lost.", gen(21))
	p.bind(t, 1, newer.ID, 21, prDescLiveHashA)
	v5 := p.stage(t, 1, "V5.", gen(21))
	p.bind(t, 1, v5.ID, 21, prDescLiveHashC)
	observed = prDescLiveHashA
	a, err = p.ack(1, v5.ID, "skipped_human_edit", 3, &observed)
	if err != nil || a.RecoveredVersionID == nil || *a.RecoveredVersionID != newer.ID || a.PR.PublishedVersion.ID != newer.ID {
		t.Fatalf("newer lost ack not recovered: %+v, %v", a, err)
	}
}

// TestPrDescriptionPublishedNeedsBindLiveDB (review B2): a version staged with an mr_iid and
// never bound cannot be acked published, and the schema refuses a published row without its
// rendered hash.
func TestPrDescriptionPublishedNeedsBindLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v1 := p.stage(t, 1, "Bound.", nil)
	p.bind(t, 1, v1.ID, 31, prDescLiveHashA)
	if _, err := p.ack(1, v1.ID, "published", 0, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	unbound := p.stage(t, 1, "Staged for the PR, never bound.", gen(31))
	if _, err := p.ack(1, unbound.ID, "published", 1, nil); !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("published ack of an unbound version err = %v, want version_conflict", err)
	}
	if pub, lock, _ := p.dbPR(t, 31); pub == nil || pub.String() != v1.ID || lock != 1 {
		t.Fatalf("pr moved: %v lock %d", pub, lock)
	}
	if _, err := p.env.pool.Exec(p.env.ctx,
		`UPDATE pr_description_versions SET state = 'published' WHERE id = $1`, unbound.ID); err == nil ||
		!strings.Contains(err.Error(), "pr_description_versions_published_bound") {
		t.Fatalf("schema accepted a published row without its hash: %v", err)
	}
}

// TestPrDescriptionMrIidFenceLiveDB (review N2 / auditor L3): stage and bind must name the run's
// own PR: runs.mr_iid when set, otherwise the PR the run first staged or bound for.
func TestPrDescriptionMrIidFenceLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v := p.stage(t, 1, "First PR.", nil)
	p.bind(t, 1, v.ID, 40, prDescLiveHashA)
	if _, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main", MrIid: gen(41),
	}); !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("stage for a second PR err = %v", err)
	}
	other := p.stage(t, 1, "Unbound.", nil)
	if _, err := p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: gen(1), VersionID: other.ID, MrIid: 41, RenderedRegionSha256: prDescLiveHashB,
	}); !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("bind to a second PR err = %v", err)
	}

	// runs.mr_iid pins the PR even before any version exists.
	p2 := setupPrDescLive(t)
	p2.env.exec(`UPDATE runs SET mr_iid = 60 WHERE id = $1`, p2.runID)
	if _, err := p2.svc.StagePrDescription(p2.env.ctx, p2.wkr, p2.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main", MrIid: gen(61),
	}); !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("stage against runs.mr_iid err = %v", err)
	}
	w := p2.stage(t, 1, "Rework.", nil)
	if _, err := p2.svc.BindPrDescription(p2.env.ctx, p2.wkr, p2.runID, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: gen(1), VersionID: w.ID, MrIid: 61, RenderedRegionSha256: prDescLiveHashB,
	}); !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("bind against runs.mr_iid err = %v", err)
	}
	p2.bind(t, 1, w.ID, 60, prDescLiveHashB)
}

// TestPrDescriptionStageCapLiveDB (auditor M4): the run's pending versions are capped.
func TestPrDescriptionStageCapLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	for i := 0; i < MaxPrDescPendingVersionsPerRun; i++ {
		p.stage(t, 1, "x", nil)
	}
	if _, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main",
	}); !errors.Is(err, ErrPrDescriptionTooManyVersions) {
		t.Fatalf("stage past the cap err = %v", err)
	}
	var n int
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT count(*) FROM pr_description_versions WHERE run_id = $1`, p.runID).Scan(&n); err != nil || n != MaxPrDescPendingVersionsPerRun {
		t.Fatalf("stored versions = %d (%v), want %d", n, err, MaxPrDescPendingVersionsPerRun)
	}
}

// TestPrDescriptionStageCapAcrossGenerationsLiveDB (N1): 20 pending versions left by a crashed
// generation do not lock the run out. On the reclaimed generation's stage, the old generation's
// UNBOUND pending versions are abandoned, its BOUND one stays pending and is still recovered by
// lost-ack recovery, and the per-run total backstop still refuses past MaxPrDescVersionsPerRun.
func TestPrDescriptionStageCapAcrossGenerationsLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	lost := p.stage(t, 1, "Written by gen 1, ack lost.", nil)
	p.bind(t, 1, lost.ID, 80, prDescLiveHashA)
	for i := 1; i < MaxPrDescPendingVersionsPerRun; i++ {
		p.stage(t, 1, "gen 1 attempt", nil)
	}
	if _, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main",
	}); !errors.Is(err, ErrPrDescriptionTooManyVersions) {
		t.Fatalf("gen-1 stage past the cap err = %v", err)
	}

	p.env.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, p.runID)
	next := p.stage(t, 2, "Gen 2.", gen(80))

	var pending, abandoned int
	if err := p.env.pool.QueryRow(p.env.ctx,
		`SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE state = 'abandoned')
		 FROM pr_description_versions WHERE run_id = $1 AND claim_generation = 1`, p.runID).Scan(&pending, &abandoned); err != nil {
		t.Fatalf("count gen-1 versions: %v", err)
	}
	if pending != 1 || abandoned != MaxPrDescPendingVersionsPerRun-1 {
		t.Fatalf("gen-1 versions: %d pending, %d abandoned; want only the bound one pending", pending, abandoned)
	}
	if st := p.dbVersionState(t, lost.ID); st != "pending" {
		t.Fatalf("bound gen-1 version state = %q, want pending", st)
	}

	// Lost-ack recovery still finds the bound gen-1 version.
	p.bind(t, 2, next.ID, 80, prDescLiveHashB)
	observed := prDescLiveHashA
	a, err := p.ack(2, next.ID, "published", 0, &observed)
	if err != nil || a.RecoveredVersionID == nil || *a.RecoveredVersionID != lost.ID {
		t.Fatalf("ack with recovery = %+v, %v; want the gen-1 version recovered", a, err)
	}
	if st := p.dbVersionState(t, lost.ID); st != "published" {
		t.Fatalf("recovered gen-1 version state = %q, want published", st)
	}

	// The per-run backstop counts every version in any state and generation.
	var total int
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT count(*) FROM pr_description_versions WHERE run_id = $1`, p.runID).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	p.env.exec(`INSERT INTO pr_description_versions (run_id, claim_generation, repo_id, fields, base_sha, head_sha, target_branch, source, state)
		SELECT $1, 1, $2, '{}'::jsonb, 'abcdef0', 'abcdef1', 'main', 'generated', 'abandoned' FROM generate_series(1, $3::int)`,
		p.runID, p.repo, MaxPrDescVersionsPerRun-total)
	p.env.exec(`UPDATE runs SET claim_generation = 3 WHERE id = $1`, p.runID)
	if _, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(3), Source: "lead_only", BaseSha: "abcdef0", HeadSha: "abcdef1", TargetBranch: "main",
	}); !errors.Is(err, ErrPrDescriptionTooManyVersions) {
		t.Fatalf("stage at the per-run backstop err = %v", err)
	}
}

// TestPrDescriptionBoundHashImmutableLiveDB: once bound, a version's rendered hash may already be
// on the forge with only its ack lost, so a rebind with the same hash is an idempotent retry and a
// rebind with a different hash is refused (a different region needs a new staged version).
func TestPrDescriptionBoundHashImmutableLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v := p.stage(t, 1, "Bound once.", nil)
	p.bind(t, 1, v.ID, 9, prDescLiveHashA)
	if r := p.bind(t, 1, v.ID, 9, prDescLiveHashA); r.Version.RenderedRegionSha256 == nil || *r.Version.RenderedRegionSha256 != prDescLiveHashA {
		t.Fatalf("identical rebind = %+v", r.Version)
	}
	_, err := p.svc.BindPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: gen(1), VersionID: v.ID, MrIid: 9, RenderedRegionSha256: prDescLiveHashB,
	})
	if !errors.Is(err, ErrPrDescriptionVersionConflict) {
		t.Fatalf("rebind with a different hash: err = %v, want version conflict", err)
	}
	var hash string
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT rendered_region_sha256 FROM pr_description_versions WHERE id = $1`, v.ID).Scan(&hash); err != nil || hash != prDescLiveHashA {
		t.Fatalf("stored hash = %q, %v; want the original", hash, err)
	}
	// The lost-ack path still finds it by the original hash.
	look, err := p.svc.LookupPrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionLookupRequest{
		ClaimGeneration: gen(1), MrIid: 9, RegionSha256: prDescLiveHashA,
	})
	if err != nil || look.Match != "pending" || look.MatchedVersionID == nil || *look.MatchedVersionID != v.ID {
		t.Fatalf("lookup by original hash = %+v, %v", look, err)
	}
}

// TestPrDescriptionOwnRegionOnForgePublishesLiveDB: an ack reporting a non-published outcome while
// the forge shows the acking version's OWN region (a write that timed out but landed) publishes
// that version instead of abandoning it, so the record names what is on the forge; a retry of
// that ack is idempotent. An own region older than the current publication is not resurrected.
func TestPrDescriptionOwnRegionOnForgePublishesLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	v := p.stage(t, 1, "Write timed out but landed.", nil)
	p.bind(t, 1, v.ID, 11, prDescLiveHashA)
	observed := prDescLiveHashA
	a, err := p.ack(1, v.ID, "write_failed", 0, &observed)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if a.RecoveredVersionID == nil || *a.RecoveredVersionID != v.ID ||
		a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != v.ID || a.PR.LockVersion != 1 {
		t.Fatalf("own-region ack = %+v", a)
	}
	if st := p.dbVersionState(t, v.ID); st != "published" {
		t.Fatalf("version state = %q, want published", st)
	}
	if pub, lock, outcome := p.dbPR(t, 11); pub == nil || pub.String() != v.ID || lock != 1 || outcome == nil || *outcome != "published" {
		t.Fatalf("pr row = %v %d %v", pub, lock, outcome)
	}
	// A retry of the same ack (lost response) is idempotent.
	if r, err := p.ack(1, v.ID, "write_failed", 0, &observed); err != nil || r.PR.PublishedVersion == nil || r.PR.PublishedVersion.ID != v.ID || r.PR.LockVersion != 1 {
		t.Fatalf("retry = %+v, %v", r, err)
	}

	// A newer version publishes; an OLDER pending version whose region reappears is not resurrected.
	old := p.stage(t, 1, "Older, never acked.", gen(11))
	p.bind(t, 1, old.ID, 11, prDescLiveHashB)
	newer := p.stage(t, 1, "Newer.", gen(11))
	p.bind(t, 1, newer.ID, 11, prDescLiveHashC)
	if _, err := p.ack(1, newer.ID, "published", 1, nil); err != nil {
		t.Fatalf("publish newer: %v", err)
	}
	observed = prDescLiveHashB
	a, err = p.ack(1, old.ID, "skipped_human_edit", 2, &observed)
	if err != nil || a.RecoveredVersionID != nil || a.PR.PublishedVersion == nil || a.PR.PublishedVersion.ID != newer.ID {
		t.Fatalf("older own region = %+v, %v", a, err)
	}
	if st := p.dbVersionState(t, old.ID); st != "abandoned" {
		t.Fatalf("older version state = %q, want abandoned", st)
	}
}
