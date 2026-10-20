package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1230 D5: source-hash drift is a detector. Nothing in this file holds a run,
// refuses a permit, revises a contract or marks a criterion satisfied; it records
// what changed so the owner can see a contradiction.

// ErrSemanticObservationInvalid marks a request the server refuses on its merits
// (foreign source, outcome contradicting the hashes, malformed field), as opposed to
// a fence or state refusal.
var ErrSemanticObservationInvalid = errors.New("invalid semantic observation")

const (
	semanticObservationMaxPRDs    = 8
	semanticObservationMaxLinks   = 8
	semanticObservationMaxRecord  = 32 * 1024
	semanticIssueMaxBytes         = 64 * 1024
	semanticIssueReadTimeout      = 10 * time.Second
	semanticObservationRefPattern = `^(?:[0-9a-f]{40}|[0-9a-f]{64})$`
)

var (
	semanticHashRe        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	semanticPushedSHARe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	semanticObsRefRe      = regexp.MustCompile(semanticObservationRefPattern)
	semanticCheckboxRe    = regexp.MustCompile(`(?m)^([ \t]*(?:[-*+]|\d+[.)])[ \t]+)\[[ xX]\]`)
	semanticDriftReasonOK = map[string]bool{"missing": true, "truncated": true, "unreadable": true, "denied": true, "invalid": true, "timeout": true}
)

// currentIssue is the outcome of the best-effort authorized forge read. Text is
// only meaningful when Reason is empty; it never leaves this package.
type currentIssue struct {
	Text   string
	Reason string
}

// readCurrentSemanticIssue reads the run's own issue with a bounded deadline. A
// forge error never fails the observation: it becomes an explicit unknown reason.
func readCurrentSemanticIssue(ctx context.Context, f forge.Forge, projectID, iid int64) currentIssue {
	ctx, cancel := context.WithTimeout(ctx, semanticIssueReadTimeout)
	defer cancel()
	issue, err := f.GetIssue(ctx, projectID, iid)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return currentIssue{Reason: "timeout"}
		}
		return currentIssue{Reason: "unreadable"}
	}
	if len(issue.Description) > semanticIssueMaxBytes {
		return currentIssue{Reason: "truncated"}
	}
	return currentIssue{Text: issue.Description}
}

func (s *Service) currentSemanticIssue(ctx context.Context, wkr store.Worker, runID uuid.UUID) (currentIssue, error) {
	conn, err := s.ForgeConnForRun(ctx, wkr, runID)
	switch {
	case errors.Is(err, ErrRunNotOwned):
		return currentIssue{}, err
	case err != nil:
		return currentIssue{Reason: "unreadable"}, nil
	case conn.IssueIID == nil:
		return currentIssue{Reason: "denied"}, nil
	case s.forges == nil:
		return currentIssue{Reason: "unreadable"}, nil
	}
	f, err := s.forges.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		return currentIssue{Reason: "unreadable"}, nil
	}
	return readCurrentSemanticIssue(ctx, f, conn.ForgeProjectID, *conn.IssueIID), nil
}

// validateSemanticObservationRequest checks everything that needs no stored state.
func validateSemanticObservationRequest(req apitypes.SemanticObservationRequest) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrSemanticObservationInvalid, fmt.Sprintf(format, args...))
	}
	switch req.Phase {
	case apitypes.SemanticDriftPhasePresentation:
		if !semanticHashRe.MatchString(req.CandidateDigest) || req.GateRevision == nil || *req.GateRevision < 1 || req.PushedSHA != "" {
			return bad("presentation requires candidate_digest and gate_revision only")
		}
	case apitypes.SemanticDriftPhasePublication:
		if !semanticPushedSHARe.MatchString(req.PushedSHA) || req.CandidateDigest != "" || req.GateRevision != nil {
			return bad("publication requires pushed_sha only")
		}
	default:
		return bad("unknown phase")
	}
	if len(req.PRDSources) > semanticObservationMaxPRDs {
		return bad("too many prd sources")
	}
	seen := map[string]bool{}
	for _, p := range req.PRDSources {
		if !semanticIdentifier(p.SourceID, 512) || seen[p.SourceID] || !semanticObsRefRe.MatchString(p.Ref) {
			return bad("invalid prd source identity or ref")
		}
		seen[p.SourceID] = true
		if req.Phase == apitypes.SemanticDriftPhasePublication && p.Ref != req.PushedSHA {
			return bad("publication ref must be the pushed sha")
		}
		if p.ObservedHash != "" && !semanticHashRe.MatchString(p.ObservedHash) {
			return bad("invalid observed hash")
		}
		switch p.Outcome {
		case apitypes.SemanticDriftUnchanged, apitypes.SemanticDriftChanged:
			if p.ObservedHash == "" || p.Reason != "" {
				return bad("%s requires observed_hash and no reason", p.Outcome)
			}
		case apitypes.SemanticDriftUnknown:
			if p.ObservedHash != "" || !semanticDriftReasonOK[p.Reason] {
				return bad("unknown requires a reason and no hash")
			}
		default:
			return bad("unknown outcome")
		}
	}
	return nil
}

func normalizeSemanticCheckboxes(s string) string {
	return semanticCheckboxRe.ReplaceAllString(s, "${1}[ ]")
}

// driftIssueSource compares the current issue with the frozen issue source. The
// server computes the outcome itself; the record holds hashes and enums only.
func driftIssueSource(ref apitypes.SemanticSourceRef, frozenText string, cur currentIssue) apitypes.SemanticDriftSource {
	out := apitypes.SemanticDriftSource{SourceID: ref.ID, FrozenHash: ref.Hash}
	switch {
	case cur.Reason != "":
		out.Outcome, out.Reason = apitypes.SemanticDriftUnknown, cur.Reason
	case semanticHash([]byte(frozenText)) != ref.Hash:
		// The snapshot is not the frozen bytes: nothing can be compared.
		out.Outcome, out.Reason = apitypes.SemanticDriftUnknown, "invalid"
	default:
		out.ObservedHash = semanticHash([]byte(cur.Text))
		out.Outcome = apitypes.SemanticDriftUnchanged
		if out.ObservedHash != ref.Hash {
			out.Outcome = apitypes.SemanticDriftChanged
			out.CheckboxOnly = normalizeSemanticCheckboxes(frozenText) == normalizeSemanticCheckboxes(cur.Text)
		}
	}
	return out
}

// driftLinks reports PRD paths added to or removed from the current issue text
// relative to the frozen PRD source paths. The compiler captures only the first
// eight paths and gives an identifier-unsafe path a fallback id, so the comparison
// uses the same eight-path window and the same identifier predicate; paths outside it
// are omitted and flagged rather than reported as drift.
func driftLinks(frozen []apitypes.SemanticSourceRef, text string) (added, removed []string, truncated bool) {
	added, removed = []string{}, []string{}
	frozenSet, currentSet := map[string]bool{}, map[string]bool{}
	for _, ref := range frozen {
		if ref.Kind == "prd" && strings.HasPrefix(ref.ID, "prd:") && !strings.HasPrefix(ref.ID, "prd:unknown:") {
			frozenSet[strings.TrimPrefix(ref.ID, "prd:")] = true
		}
	}
	paths := FindSemanticPrdPaths(text)
	if len(paths) > semanticObservationMaxPRDs {
		paths, truncated = paths[:semanticObservationMaxPRDs], true
	}
	for _, p := range paths {
		if !semanticIdentifier("prd:"+p, 512) {
			truncated = true
			continue
		}
		currentSet[p] = true
		if !frozenSet[p] && len(added) < semanticObservationMaxLinks {
			added = append(added, p)
		}
	}
	for _, ref := range frozen {
		p := strings.TrimPrefix(ref.ID, "prd:")
		if ref.Kind == "prd" && ref.ID != p && !strings.HasPrefix(ref.ID, "prd:unknown:") && !currentSet[p] && len(removed) < semanticObservationMaxLinks {
			removed = append(removed, p)
		}
	}
	return added, removed, truncated
}

// driftPRDSources validates the worker-reported PRD reads against the frozen
// sources and recomputes changed/unchanged from the server's frozen hash. Every
// frozen PRD source must be reported once so an omission cannot hide drift.
func driftPRDSources(frozen []apitypes.SemanticSourceRef, reported []apitypes.SemanticObservedSource) ([]apitypes.SemanticDriftSource, error) {
	byID := map[string]apitypes.SemanticSourceRef{}
	for _, ref := range frozen {
		if ref.Kind == "prd" {
			byID[ref.ID] = ref
		}
	}
	if len(reported) != len(byID) {
		return nil, fmt.Errorf("%w: every frozen prd source must be reported once", ErrSemanticObservationInvalid)
	}
	out := make([]apitypes.SemanticDriftSource, 0, len(reported))
	for _, r := range reported {
		ref, ok := byID[r.SourceID]
		if !ok {
			return nil, fmt.Errorf("%w: not a frozen prd source", ErrSemanticObservationInvalid)
		}
		src := apitypes.SemanticDriftSource{SourceID: ref.ID, FrozenHash: ref.Hash, ObservedHash: r.ObservedHash, Ref: r.Ref, Outcome: r.Outcome, Reason: r.Reason}
		switch {
		case ref.Completeness != "complete":
			// A frozen hash of unavailable bytes is not a baseline.
			if r.Outcome != apitypes.SemanticDriftUnknown {
				return nil, fmt.Errorf("%w: frozen source has no complete baseline", ErrSemanticObservationInvalid)
			}
		case r.Outcome == apitypes.SemanticDriftUnchanged && r.ObservedHash != ref.Hash,
			r.Outcome == apitypes.SemanticDriftChanged && r.ObservedHash == ref.Hash:
			return nil, fmt.Errorf("%w: outcome contradicts the frozen hash", ErrSemanticObservationInvalid)
		}
		out = append(out, src)
	}
	return out, nil
}

// ObserveSemanticDrift records one drift observation for a semantic run. The forge
// read happens before the run lock; the fence, validation, comparison and the bounded
// append run under it. The returned record carries outcomes, hashes, refs, paths and
// times, never source text.
func (s *Service) ObserveSemanticDrift(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, req apitypes.SemanticObservationRequest) (apitypes.SemanticDriftObservation, error) {
	var out apitypes.SemanticDriftObservation
	if err := validateSemanticObservationRequest(req); err != nil {
		return out, err
	}
	cur, err := s.currentSemanticIssue(ctx, wkr, runID)
	if err != nil {
		return out, err
	}
	err = s.withSemanticCoverage(ctx, wkr.ID, runID, generation, func(c *semanticCoverageTx) error {
		if req.Phase == apitypes.SemanticDriftPhasePresentation &&
			(req.CandidateDigest != c.candidate.Digest || *req.GateRevision != c.run.GateRevision) {
			return ErrSemanticGateMismatch
		}
		var issueRef *apitypes.SemanticSourceRef
		for i := range c.candidate.Sources {
			if c.candidate.Sources[i].Kind == "issue" {
				issueRef = &c.candidate.Sources[i]
			}
		}
		if issueRef == nil {
			return fmt.Errorf("%w: no frozen issue source", ErrSemanticObservationInvalid)
		}
		prds, err := driftPRDSources(c.candidate.Sources, req.PRDSources)
		if err != nil {
			return err
		}
		blob, err := c.q.GetSemanticSnapshot(ctx, store.GetSemanticSnapshotParams{RunID: runID, Hash: issueRef.Hash})
		if err != nil {
			return err
		}
		issue := driftIssueSource(*issueRef, string(blob), cur)
		rec := apitypes.SemanticDriftObservation{Phase: req.Phase, CandidateDigest: req.CandidateDigest, PushedSHA: req.PushedSHA,
			ObservedAt: c.now, Issue: issue, PRDs: prds, LinksAdded: []string{}, LinksRemoved: []string{}}
		if req.GateRevision != nil {
			rec.GateRevision = *req.GateRevision
		}
		if cur.Reason == "" {
			rec.LinksAdded, rec.LinksRemoved, rec.LinksTruncated = driftLinks(c.candidate.Sources, cur.Text)
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if len(raw) > semanticObservationMaxRecord {
			return fmt.Errorf("%w: record exceeds bound", ErrSemanticObservationInvalid)
		}
		if _, err = c.q.AppendSemanticObservation(ctx, store.AppendSemanticObservationParams{RunID: runID, Observation: raw}); err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}

type storedSemanticObservations struct {
	Dropped int64                               `json:"dropped"`
	Items   []apitypes.SemanticDriftObservation `json:"items"`
}

// SemanticDriftForOwner projects the stored observations for the run owner. It
// returns nil when the run has no semantic planning row or no observation yet.
// Contradiction is true when any stored source is changed or unknown, so a later
// benign observation cannot silence an earlier one.
func (s *Service) SemanticDriftForOwner(ctx context.Context, owner uuid.UUID, run store.Run) (*apitypes.SemanticDriftDTO, error) {
	if run.UserID != owner {
		return nil, ErrRunNotFound
	}
	if !run.CompletionContractVersion.Valid || s.txBeginner == nil {
		return nil, nil
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	raw, err := store.New(tx).GetSemanticObservations(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return semanticDriftDTO(raw)
}

func semanticDriftDTO(raw []byte) (*apitypes.SemanticDriftDTO, error) {
	var stored storedSemanticObservations
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if len(stored.Items) == 0 && stored.Dropped == 0 {
		return nil, nil
	}
	dto := &apitypes.SemanticDriftDTO{Observations: stored.Items, Dropped: stored.Dropped}
	if dto.Observations == nil {
		dto.Observations = []apitypes.SemanticDriftObservation{}
	}
	for _, o := range stored.Items {
		if o.Issue.Outcome != apitypes.SemanticDriftUnchanged {
			dto.Contradiction = true
		}
		for _, p := range o.PRDs {
			if p.Outcome != apitypes.SemanticDriftUnchanged {
				dto.Contradiction = true
			}
		}
	}
	return dto, nil
}
