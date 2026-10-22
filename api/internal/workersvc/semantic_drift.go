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
	semanticObservationMaxPRDs   = 8
	semanticObservationMaxLinks  = 8
	semanticObservationMaxRecord = 32 * 1024
	semanticIssueMaxBytes        = 64 * 1024
	// semanticIssueReadTimeout bounds the forge read well inside the agent's whole 10 s observation
	// budget (up to 5 s of git reads come first), so a slow forge becomes unknown(timeout) on the
	// record rather than the client aborting and dropping the observation.
	semanticIssueReadTimeout      = 3 * time.Second
	semanticObservationRefPattern = `^(?:[0-9a-f]{40}|[0-9a-f]{64})$`
)

var (
	semanticHashRe        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	semanticObsRefRe      = regexp.MustCompile(semanticObservationRefPattern)
	semanticCheckboxRe    = regexp.MustCompile(`(?m)^([ \t]*(?:[-*+]|\d+[.)])[ \t]+)\[[ xX]\]`)
	semanticDriftReasonOK = map[string]bool{"missing": true, "truncated": true, "unreadable": true, "denied": true, "invalid": true, "timeout": true}
)

// currentIssue is the outcome of the best-effort authorized forge read. Text is
// only meaningful when Reason is empty; it never leaves this package.
type currentIssue struct {
	Text   string
	Reason string
	// Saved is the raw body captured at run creation (ForgeConn.SavedBody). It differs
	// from the frozen issue text only for a composed (scheduled guidance) description.
	Saved string
	// SavedValid is true when the run captured a raw body (issue_saved_body is not NULL), which
	// may legitimately be empty. Only then is Saved a baseline.
	SavedValid bool
}

// issueBaseline returns the text the current raw forge body must be compared with. A composed
// (scheduled guidance) run froze a text that is not the raw forge body, so its saved raw body is
// the baseline whenever one was captured and differs from the frozen text, including an empty one.
func issueBaseline(frozenText string, cur currentIssue) (baseline string, composed bool) {
	if cur.SavedValid && cur.Saved != frozenText {
		return cur.Saved, true
	}
	return frozenText, false
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
	cur := readCurrentSemanticIssue(ctx, f, conn.ForgeProjectID, *conn.IssueIID)
	cur.Saved, cur.SavedValid = conn.SavedBody, conn.SavedBodyValid
	return cur, nil
}

// semanticWorkerClaim checks live ownership before any semantic body or forge read.
// The authoritative generation fence is re-checked under the operation's run lock.
func (s *Service) semanticWorkerClaim(ctx context.Context, wkr store.Worker, runID uuid.UUID) (store.Run, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return store.Run{}, err
	}
	switch {
	case run.ClaimGeneration < 1 || run.ClaimReleasedAt.Valid:
		return store.Run{}, ErrStaleClaim
	case terminalStatuses[run.Status]:
		return store.Run{}, ErrRunTerminal
	case run.Status != "running" && run.Status != "awaiting_approval":
		return store.Run{}, ErrInvalidState
	}
	return run, nil
}

func (s *Service) requireSemanticEnrollment(ctx context.Context, runID uuid.UUID) error {
	if s.txBeginner == nil {
		return errCoverageUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = store.New(tx).GetSemanticObservations(ctx, runID); errors.Is(err, pgx.ErrNoRows) {
		return errCoverageUnavailable
	} else if err != nil {
		return err
	}
	return nil
}

// RequireSemanticWorkerAdmission rejects foreign, inactive and unenrolled writes
// before the handler reads their body. It grants no mutation authority: the body
// generation and enrollment are still checked under the operation's run lock.
func (s *Service) RequireSemanticWorkerAdmission(ctx context.Context, wkr store.Worker, runID uuid.UUID) error {
	if _, err := s.semanticWorkerClaim(ctx, wkr, runID); err != nil {
		return err
	}
	return s.requireSemanticEnrollment(ctx, runID)
}

func (s *Service) requireObservable(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) error {
	run, err := s.semanticWorkerClaim(ctx, wkr, runID)
	if err != nil {
		return err
	}
	if run.ClaimGeneration != generation {
		return ErrStaleClaim
	}
	return s.requireSemanticEnrollment(ctx, runID)
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
		if !semanticObsRefRe.MatchString(req.PushedSHA) || req.CandidateDigest != "" || req.GateRevision != nil {
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
		// A composed (scheduled guidance) frozen text is not the raw forge body; compare the
		// current raw body against the saved raw body instead, as WorkerForgeGetIssue does.
		baseline, composed := issueBaseline(frozenText, cur)
		baselineHash := ref.Hash
		if composed {
			baselineHash = semanticHash([]byte(baseline))
		}
		out.Outcome = apitypes.SemanticDriftUnchanged
		if out.ObservedHash != baselineHash {
			out.Outcome = apitypes.SemanticDriftChanged
			out.CheckboxOnly = normalizeSemanticCheckboxes(baseline) == normalizeSemanticCheckboxes(cur.Text)
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
		if !ValidSemanticPrdPath(p) || !semanticIdentifier("prd:"+p, 512) {
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
		if ref.Kind != "prd" || ref.ID == p || strings.HasPrefix(ref.ID, "prd:unknown:") || currentSet[p] {
			continue
		}
		if !ValidSemanticPrdPath(p) {
			truncated = true
		} else if len(removed) < semanticObservationMaxLinks {
			removed = append(removed, p)
		}
	}
	return added, removed, truncated
}

// boundedPrdLinks returns the valid PRD paths of text inside the compiler's eight-path window,
// flagging truncation when the window cut paths off or a path failed the shared predicates.
func boundedPrdLinks(text string) (paths []string, truncated bool) {
	found := FindSemanticPrdPaths(text)
	if len(found) > semanticObservationMaxPRDs {
		found, truncated = found[:semanticObservationMaxPRDs], true
	}
	for _, p := range found {
		if !ValidSemanticPrdPath(p) || !semanticIdentifier("prd:"+p, 512) {
			truncated = true
			continue
		}
		paths = append(paths, p)
	}
	return paths, truncated
}

// driftLinksComposed is driftLinks for a composed run: the frozen PRD sources were captured from
// the composed text (raw body plus schedule guidance), so they cannot be the baseline for the raw
// body. The links of the saved raw body are, under the same validator, window and bounds.
func driftLinksComposed(saved, text string) (added, removed []string, truncated bool) {
	savedPaths, t1 := boundedPrdLinks(saved)
	currentPaths, t2 := boundedPrdLinks(text)
	diff := func(from, not []string) []string {
		skip, out := map[string]bool{}, []string{}
		for _, p := range not {
			skip[p] = true
		}
		for _, p := range from {
			if !skip[p] && len(out) < semanticObservationMaxLinks {
				out = append(out, p)
			}
		}
		return out
	}
	return diff(currentPaths, savedPaths), diff(savedPaths, currentPaths), t1 || t2
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
	if s.txBeginner == nil {
		return out, fmt.Errorf("semantic observation requires transaction")
	}
	if err := s.requireObservable(ctx, wkr, runID, generation); err != nil {
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
			if saved, composed := issueBaseline(string(blob), cur); composed {
				rec.LinksAdded, rec.LinksRemoved, rec.LinksTruncated = driftLinksComposed(saved, cur.Text)
			} else {
				rec.LinksAdded, rec.LinksRemoved, rec.LinksTruncated = driftLinks(c.candidate.Sources, cur.Text)
			}
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if len(raw) > semanticObservationMaxRecord {
			return fmt.Errorf("%w: record exceeds bound", ErrSemanticObservationInvalid)
		}
		if _, err = c.q.AppendSemanticObservation(ctx, store.AppendSemanticObservationParams{RunID: runID, Observation: raw, Contradicted: driftRecordContradicts(rec)}); err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}

type storedSemanticObservations struct {
	Dropped      int64                               `json:"dropped"`
	Contradicted bool                                `json:"contradicted"`
	Items        []apitypes.SemanticDriftObservation `json:"items"`
}

// driftRecordContradicts is true when any source of the record is changed or unknown, or a PRD
// link was added to or removed from the issue. It mirrors the agent's hasDriftContradiction.
func driftRecordContradicts(o apitypes.SemanticDriftObservation) bool {
	if o.Issue.Outcome != apitypes.SemanticDriftUnchanged || len(o.LinksAdded) > 0 || len(o.LinksRemoved) > 0 {
		return true
	}
	for _, p := range o.PRDs {
		if p.Outcome != apitypes.SemanticDriftUnchanged {
			return true
		}
	}
	return false
}

// SemanticDriftForOwner projects the stored observations for the run owner. It
// returns nil when the run has no semantic planning row or no observation yet.
// Contradiction is the sticky stored flag or any kept source being changed or unknown, so
// neither a later benign observation nor the 32-item bound can silence an earlier one.
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
	// Sticky: a contradiction recorded in a since-dropped observation still counts.
	dto.Contradiction = stored.Contradicted
	for _, o := range stored.Items {
		dto.Contradiction = dto.Contradiction || driftRecordContradicts(o)
	}
	return dto, nil
}
