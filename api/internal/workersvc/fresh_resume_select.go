package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Fresh resume selection (issue #4628, plan section 2): which durable evidence a start would seed the
// new worker from, and the token that binds the owner's confirmation to exactly that selection.
//
// The evidence generation Ge is the highest claim generation <= the run's latest G that saved anything:
// a recovery capture under that generation's custody hold, or a checkpoint tip stamped with it. An older
// generation is therefore only used when every later generation saved nothing, and the preview names
// those generations. When Ge has no usable candidate the selection refuses with the reason; it never
// falls back to an older generation.

// FreshResumeRefused is a refusal of a preview-class or start-class request: a closed reason string the
// handler answers as 409. The reasons are the apitypes.FreshResumeStatus* values plus
// apitypes.FreshResumeReason*.
type FreshResumeRefused struct{ Reason string }

func (e *FreshResumeRefused) Error() string { return "fresh resume refused: " + e.Reason }

// ErrFreshResumeUnavailable is returned when the service has no transaction beginner wired. A fresh
// resume needs one: every start and refusal is a locked multi-row transaction.
var ErrFreshResumeUnavailable = errors.New("fresh resume unavailable: no transaction beginner wired")

const (
	// freshResumeForgeBudget bounds all forge calls of one selection.
	freshResumeForgeBudget = 20 * time.Second
	// freshResumeMaxForgeCalls bounds the ancestry and ref reads of one selection. Past it every further
	// question is unproven, so the selection degrades to a refusal, never to an unproven pick.
	freshResumeMaxForgeCalls = 96
	// freshResumeUncoveredCap bounds the generations the preview lists.
	freshResumeUncoveredCap = 64
)

// freshCandidate is one piece of durable evidence at the evidence generation.
type freshCandidate struct {
	kind      string // apitypes.FreshResumeKindCapture or FreshResumeKindCheckpointRef
	captureID uuid.UUID
	ref       string
	sha       string
	prereqs   []string
}

// freshSelection is the outcome of selecting evidence: Status is apitypes.FreshResumeStatusAvailable
// with a chosen candidate, or the reason there is none.
type freshSelection struct {
	Status    string
	Chosen    freshCandidate
	Ge        int64
	G         int64
	Version   int64
	Uncovered []int64
	Token     string
}

// freshFacts are the database facts one selection reads, loaded in one short transaction so no forge
// call runs while a transaction is open.
type freshFacts struct {
	run       store.Run
	captures  []store.ListFreshResumeCaptureEvidenceRow
	openHolds int64
	rc        store.GetRunClaimContextRow
	rcOK      bool
}

// loadFreshFacts reads the run (owner-scoped), its capture evidence, its open-hold count and its forge
// context. ErrRunNotFound when the run is not the owner's.
func loadFreshFacts(ctx context.Context, q *store.Queries, userID, runID uuid.UUID) (freshFacts, error) {
	var f freshFacts
	run, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrRunNotFound
	}
	if err != nil {
		return f, err
	}
	f.run = run
	if f.captures, err = q.ListFreshResumeCaptureEvidence(ctx, store.ListFreshResumeCaptureEvidenceParams{RunID: runID, UserID: userID}); err != nil {
		return f, err
	}
	if f.openHolds, err = q.CountOpenHoldsForFreshRun(ctx, store.CountOpenHoldsForFreshRunParams{RunID: runID, UserID: userID}); err != nil {
		return f, err
	}
	rc, err := q.GetRunClaimContext(ctx, runID)
	switch {
	case err == nil:
		f.rc, f.rcOK = rc, true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return f, err
	}
	return f, nil
}

// freshToken is the selection token: sha256 over the run, its latest claim generation G, the hold
// version, the evidence generation Ge, the evidence kind and identity, and the evidence sha. Any change
// of those (a new claim, a restore or an exit from pending, a different pick) makes an old token stale.
func freshToken(runID uuid.UUID, g, version, ge int64, c freshCandidate) string {
	id := c.ref
	if c.kind == apitypes.FreshResumeKindCapture {
		id = c.captureID.String()
	}
	h := sha256.New()
	for _, part := range []string{"fresh-resume-v1", runID.String(), strconv.FormatInt(g, 10), strconv.FormatInt(version, 10),
		strconv.FormatInt(ge, 10), c.kind, id, c.sha} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// freshProver asks the forge the bounded questions a selection needs. A question the budget, the clock
// or the forge cannot answer is unproven, never assumed.
type freshProver struct {
	ctx       context.Context
	host      forge.CodeHost
	projectID int64
	branch    string
	calls     int

	headRead bool
	head     string
	headErr  bool
	memo     map[[2]string]bool
}

func (p *freshProver) spend() bool {
	if p.host == nil || p.ctx.Err() != nil || p.calls >= freshResumeMaxForgeCalls {
		return false
	}
	p.calls++
	return true
}

// defaultHead reads the default branch head once.
func (p *freshProver) defaultHead() (string, bool) {
	if !p.headRead {
		p.headRead = true
		if p.branch == "" || !p.spend() {
			p.headErr = true
		} else if head, err := p.host.BranchHead(p.ctx, p.projectID, p.branch); err != nil || !isSettleSHA(head) {
			p.headErr = true
		} else {
			p.head = head
		}
	}
	return p.head, !p.headErr
}

// ancestor reports whether candidate is an ancestor of, or equal to, head, proven by the forge.
func (p *freshProver) ancestor(head, candidate string) bool {
	if !isSettleSHA(head) || !isSettleSHA(candidate) {
		return false
	}
	if head == candidate {
		return true
	}
	key := [2]string{head, candidate}
	if v, ok := p.memo[key]; ok {
		return v
	}
	proven := false
	if p.spend() {
		a, err := p.host.CompareAncestry(p.ctx, p.projectID, head, candidate)
		proven = err == nil && a == forge.AncestryAncestor
	}
	if p.memo == nil {
		p.memo = map[[2]string]bool{}
	}
	p.memo[key] = proven
	return proven
}

// dominates reports whether x dominates the sha y: they are the same commit, y is one of x's
// prerequisites, or the forge proves y is an ancestor of one of x's prerequisites.
func (p *freshProver) dominates(x freshCandidate, y string) bool {
	if x.sha == y {
		return true
	}
	for _, pre := range x.prereqs {
		if pre == y {
			return true
		}
	}
	for _, pre := range x.prereqs {
		if p.ancestor(pre, y) {
			return true
		}
	}
	return false
}

// decideFreshEvidence selects the evidence from the loaded facts, asking the forge for the proofs. It
// reads nothing from the database and holds no transaction. Status is the apitypes preview status for
// the evidence alone: available, or one of no_evidence, expired, unbound, prereq_unproven, ambiguous.
func (s *Service) decideFreshEvidence(ctx context.Context, f freshFacts) freshSelection {
	run := f.run
	sel := freshSelection{G: run.ClaimGeneration, Version: run.FreshHoldVersion}

	tip := ""
	if run.CheckpointTip.Valid {
		tip = run.CheckpointTip.String
	}
	tipStamped := tip != "" && run.CheckpointTipGeneration.Valid && run.CheckpointTipGenerationTip.Valid &&
		run.CheckpointTipGenerationTip.String == tip
	tipGen := run.CheckpointTipGeneration.Int64
	// A tip only counts as evidence at its generation while the stamp still names it and the generation
	// is one this run has claimed. Any other tip is unbound: it may hold work of an unknown generation.
	tipUsable := tipStamped && tipGen >= 1 && tipGen <= sel.G
	tipUnbound := tip != "" && !tipUsable

	repoMatches := func(r store.ListFreshResumeCaptureEvidenceRow) bool {
		return run.RepoID.Valid && r.HoldRepoID.Valid && r.HoldRepoID.Bytes == run.RepoID.Bytes && r.HoldRunID == run.ID
	}
	var ge int64
	for _, c := range f.captures {
		if c.HoldGeneration >= 1 && c.HoldGeneration <= sel.G && repoMatches(c) && c.HoldGeneration > ge {
			ge = c.HoldGeneration
		}
	}
	if tipUsable && tipGen > ge {
		ge = tipGen
	}
	if ge == 0 {
		if tipUnbound {
			sel.Status = apitypes.FreshResumeStatusUnbound
		} else {
			sel.Status = apitypes.FreshResumeStatusNoEvidence
		}
		return sel
	}
	sel.Ge = ge
	for g := ge + 1; g <= sel.G && len(sel.Uncovered) < freshResumeUncoveredCap; g++ {
		sel.Uncovered = append(sel.Uncovered, g)
	}

	// Forge access, built lazily and bounded as a whole.
	fctx, cancel := context.WithTimeout(ctx, freshResumeForgeBudget)
	defer cancel()
	prover := &freshProver{ctx: fctx}
	if f.rcOK && s.forges != nil {
		if fg, err := s.forges.ForgeForConnection(f.rc.ForgeType, f.rc.BaseUrl, f.rc.TokenCiphertext); err == nil {
			prover.host = fg
			prover.projectID = f.rc.ForgeProjectID
			if f.rc.DefaultBranch.Valid {
				prover.branch = f.rc.DefaultBranch.String
			}
		} else {
			slog.Warn("fresh resume selection: build forge", "run", run.ID.String(), "error", err)
		}
	}

	var (
		cands       []freshCandidate
		expiredHint bool
		proofHint   bool
	)
	for _, c := range f.captures {
		if c.HoldGeneration != ge || !repoMatches(c) {
			continue
		}
		switch {
		case c.State == "expired":
			expiredHint = true
			continue
		case c.State != "available" || !c.ManifestBound:
			continue
		case !c.TtlOk:
			expiredHint = true
			continue
		}
		if !isSettleSHA(c.SourceSha) {
			proofHint = true
			continue
		}
		proven := true
		for _, pre := range c.PrerequisiteShas {
			if tipStamped && pre == tip {
				continue
			}
			head, ok := prover.defaultHead()
			if !ok || !prover.ancestor(head, pre) {
				proven = false
				break
			}
		}
		if !proven {
			proofHint = true
			continue
		}
		cands = append(cands, freshCandidate{kind: apitypes.FreshResumeKindCapture, captureID: c.ID, sha: c.SourceSha, prereqs: c.PrerequisiteShas})
	}
	if tipUsable && tipGen == ge && f.openHolds > 0 && isSettleSHA(tip) {
		if coords, err := publicationCoordinates(run); err == nil && coords != nil && isSettleBranchName(coords.CheckpointKey) {
			ref := checkpointRefPrefix + coords.CheckpointKey
			switch {
			case !prover.spend():
				proofHint = true
			default:
				head, err := prover.host.RefHead(fctx, prover.projectID, ref)
				switch {
				case err == nil && head == tip:
					cands = append(cands, freshCandidate{kind: apitypes.FreshResumeKindCheckpointRef, ref: ref, sha: tip})
				case err == nil || errors.Is(err, forge.ErrRefNotFound):
					// The ref moved on or is gone: this evidence no longer exists as selected.
					expiredHint = true
				default:
					proofHint = true
				}
			}
		}
	}

	if len(cands) == 0 {
		switch {
		case proofHint:
			sel.Status = apitypes.FreshResumeStatusPrereqUnproven
		case expiredHint:
			sel.Status = apitypes.FreshResumeStatusExpired
		case tipUnbound:
			sel.Status = apitypes.FreshResumeStatusUnbound
		default:
			sel.Status = apitypes.FreshResumeStatusNoEvidence
		}
		return sel
	}

	// Dominance: the chosen candidate must dominate every other, so nothing the others saved is lost.
	var dominators []freshCandidate
	for _, x := range cands {
		all := true
		for _, y := range cands {
			if x.kind == y.kind && x.captureID == y.captureID && x.ref == y.ref {
				continue
			}
			if !prover.dominates(x, y.sha) {
				all = false
				break
			}
		}
		if all {
			dominators = append(dominators, x)
		}
	}
	if len(dominators) == 0 {
		sel.Status = apitypes.FreshResumeStatusAmbiguous
		return sel
	}
	chosen := dominators[0]
	for _, d := range dominators[1:] {
		if freshTieBefore(d, chosen) {
			chosen = d
		}
	}
	// A tip of unknown generation may hold newer work than any candidate: refuse unless the chosen
	// candidate provably contains it.
	if tipUnbound && !prover.dominates(chosen, tip) {
		sel.Status = apitypes.FreshResumeStatusUnbound
		return sel
	}
	sel.Chosen = chosen
	sel.Status = apitypes.FreshResumeStatusAvailable
	sel.Token = freshToken(run.ID, sel.G, sel.Version, ge, chosen)
	return sel
}

// freshTieBefore orders equally dominant candidates: a checkpoint ref before a capture, then the lowest
// capture id.
func freshTieBefore(a, b freshCandidate) bool {
	if a.kind != b.kind {
		return a.kind == apitypes.FreshResumeKindCheckpointRef
	}
	return bytes.Compare(a.captureID[:], b.captureID[:]) < 0
}

// freshNotes is the cutoff text a confirmation is made against.
func freshNotes(uncovered []int64) []string {
	notes := []string{
		"Edits made after the saved evidence may be missing from the resumed run.",
		"The original worker's source and custody are kept; starting a fresh resume does not delete them.",
		"Unsaved edits may be lost if the original worker's volume is gone.",
		"Each attempt keeps its own custody hold, and a healthy seeded run counts one extra admission unit until its new hold is released or the run ends.",
		"While the original worker and the new worker both hold custody, only a third eligible hosted worker can take a repeat fresh resume. After seeding, the original worker never takes this run again, so an owner with only two workers whose new worker is lost must add a worker or cancel the run.",
	}
	if len(uncovered) > 0 {
		parts := make([]string, len(uncovered))
		for i, g := range uncovered {
			parts[i] = strconv.FormatInt(g, 10)
		}
		notes = append(notes, fmt.Sprintf("No saved evidence exists for generation %s; any edits made in it are not included.", strings.Join(parts, ", ")))
	}
	return notes
}

// PreviewFreshResume answers GET /api/runs/{id}/fresh-resume for the run's owner: whether a start would
// be accepted now, from which evidence, and the selection token a start must present. ErrRunNotFound
// when the run is not the owner's. It never writes.
func (s *Service) PreviewFreshResume(ctx context.Context, userID, runID uuid.UUID) (apitypes.FreshResumePreviewDTO, error) {
	dto := apitypes.FreshResumePreviewDTO{UncoveredGenerations: []int64{}, Notes: freshNotes(nil)}
	if s.txBeginner == nil {
		return dto, ErrFreshResumeUnavailable
	}
	f, status, err := s.freshPrecheck(ctx, userID, runID)
	if err != nil {
		return dto, err
	}
	dto.ClaimGeneration = f.run.ClaimGeneration
	if status != "" {
		dto.Status = status
		return dto, nil
	}
	sel := s.decideFreshEvidence(ctx, f)
	dto.Status = sel.Status
	dto.EvidenceGeneration = sel.Ge
	if len(sel.Uncovered) > 0 {
		dto.UncoveredGenerations = sel.Uncovered
		dto.Notes = freshNotes(sel.Uncovered)
	}
	if sel.Status == apitypes.FreshResumeStatusAvailable {
		dto.SelectionToken = sel.Token
		dto.Kind = sel.Chosen.kind
		dto.Sha = sel.Chosen.sha
		if sel.Chosen.kind == apitypes.FreshResumeKindCapture {
			dto.CaptureID = sel.Chosen.captureID.String()
		} else {
			dto.CheckpointRef = sel.Chosen.ref
		}
	}
	return dto, nil
}

// freshPrecheck loads the facts in one short read transaction and applies the database-only gates in
// answer order: the feature switch, an active transfer, run eligibility, the destination and the owner's
// headroom. status is the first refusing status, or "" when evidence selection should run.
func (s *Service) freshPrecheck(ctx context.Context, userID, runID uuid.UUID) (freshFacts, string, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return freshFacts{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	f, err := loadFreshFacts(ctx, q, userID, runID)
	if err != nil {
		return f, "", err
	}
	if !s.p.FreshResume {
		return f, apitypes.FreshResumeStatusDisabled, nil
	}
	status, err := s.freshGates(ctx, q, f.run)
	return f, status, err
}

// freshGates applies the database gates that do not depend on the evidence: no active transfer, run
// eligibility, a destination worker and owner headroom. It returns the refusing status or "". It runs
// under the owner and run locks in a start, so every read is current; the preview runs it unlocked.
func (s *Service) freshGates(ctx context.Context, q *store.Queries, run store.Run) (string, error) {
	if _, err := q.GetActiveFreshResumeForRun(ctx, run.ID); err == nil {
		return apitypes.FreshResumeStatusInProgress, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	eligible, err := q.FreshResumeRunBaseEligible(ctx, store.FreshResumeRunBaseEligibleParams{
		ID: run.ID, UserID: run.UserID, GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !eligible) {
		return apitypes.FreshResumeStatusIneligible, nil
	}
	if err != nil {
		return "", err
	}
	cutoff := s.staleWorkerCutoff()
	n, err := q.CountFreshResumeDestinations(ctx, store.CountFreshResumeDestinationsParams{
		RunID: run.ID, UserID: run.UserID, HeartbeatCutoff: cutoff,
	})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return apitypes.FreshResumeStatusNoDestination, nil
	}
	limit, err := s.custodyLimit(ctx)
	if err != nil {
		return "", err
	}
	if limit > 0 {
		adm, err := q.GetCustodyAdmissionForRun(ctx, store.GetCustodyAdmissionForRunParams{
			UserID: run.UserID, RunID: run.ID, CustodyHoldLimit: limit, HeartbeatCutoff: cutoff,
		})
		if err != nil {
			return "", err
		}
		// The pending reservation is one more owner unit and the new attempt one more hold of the run.
		if adm.AdmissionCountedHolds+1 > int64(limit) || adm.OwnOpenHolds+2 > int64(limit) {
			return apitypes.FreshResumeStatusCustodyLimit, nil
		}
	}
	return "", nil
}
