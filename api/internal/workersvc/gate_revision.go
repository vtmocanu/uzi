package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1795 M1: plan-gate revision allocation.
//
// Every awaiting_approval report of a plan-gated run is classified under the run-row lock
// BEFORE SetRunAwaitingApproval runs:
//
//   - current id    the report names the run's current presentation. An unchanged effective
//                   payload is an idempotent retry that returns the current revision N and
//                   writes no new snapshot; a changed one is refused (gate_presentation_conflict).
//   - historical    the id is recorded in run_gate_presentations but is no longer current:
//                   refused (gate_presentation_historical).
//   - adopt         adopt_gate_revision N binds a fresh id to an id-less gate at N, accepted only
//                   when the current revision is N, the current id is NULL and the payload is
//                   unchanged; anything else is refused (gate_adoption_stale).
//   - unseen id     allocates N+1 and records the id.
//   - no id         an old worker's report: allocates N+1 with a NULL id and records no row.
//
// A refusal writes no report. It does its fenced accounting in the same transaction and, past
// RUN_GATE_REFUSAL_MAX counted claims, fails the run.

// Refusal sentinels. The handler maps each to 409 with the matching reason token.
var (
	ErrGatePresentationHistorical = errors.New("gate presentation id is historical")
	ErrGatePresentationConflict   = errors.New("gate presentation payload changed under the current id")
	ErrGateAdoptionStale          = errors.New("gate adoption is stale")
	// ErrClaimGenerationRequired answers an id-bearing awaiting_approval report
	// (presentation_id or adopt_gate_revision) that omitted claim_generation: refusal
	// accounting is fenced on the generation, so such a report cannot be classified (400).
	ErrClaimGenerationRequired = errors.New("claim_generation is required with presentation_id or adopt_gate_revision")
)

// GatePresentationRefusalReason maps a refusal error to its 409 reason token.
func GatePresentationRefusalReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrGatePresentationHistorical):
		return "gate_presentation_historical", true
	case errors.Is(err, ErrGatePresentationConflict):
		return "gate_presentation_conflict", true
	case errors.Is(err, ErrGateAdoptionStale):
		return "gate_adoption_stale", true
	default:
		return "", false
	}
}

// gateRefusalLabel is the human wording of a refusal kind in the cap-failure reason.
func gateRefusalLabel(err error) string {
	switch {
	case errors.Is(err, ErrGatePresentationHistorical):
		return "a historical presentation"
	case errors.Is(err, ErrGatePresentationConflict):
		return "a changed presentation"
	default:
		return "a stale adoption"
	}
}

// gateRevisionKind reports whether a run kind allocates gate revisions. Chat and judge runs
// never reach the human plan gate through this path and never allocate.
func gateRevisionKind(kind string) bool {
	return kind != runkind.Chat && kind != runkind.Judge
}

// GatePresentedRequirements is the approval-relevant requirement set of the presented gate, as
// the snapshot holds it. The gate-resume claim carries it as resume_gate_presented so a
// reclaimed worker re-presents what the human saw, not a fresh detection or the live
// requirement columns an approval's capability override may have cleared.
type GatePresentedRequirements struct {
	RequiredCapabilities []string `json:"required_capabilities"`
	RequiredTools        []string `json:"required_tools"`
	SizeClass            *string  `json:"size_class,omitempty"`
}

// gatePresentedPayload is the immutable approval-relevant payload of a gate presentation.
// plan_changed_files is deliberately absent: it is advisory evidence recomputed every round,
// not part of presentation identity (PRD #1795 A1).
type gatePresentedPayload struct {
	PlanMd               *string         `json:"plan_md"`
	Milestones           json.RawMessage `json:"milestones"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	RequiredTools        []string        `json:"required_tools"`
	SizeClass            *string         `json:"size_class"`
}

// canonicalJSON returns the payload's canonical encoding: fixed key order, the two requirement
// arrays sorted and de-duplicated (both are sets), and the milestone JSON re-encoded through a
// generic decode so object keys are sorted and whitespace is uniform. Postgres JSONB re-formats
// a stored snapshot, so a digest is only ever computed over this form, never over stored bytes.
func (p gatePresentedPayload) canonicalJSON() ([]byte, error) {
	out := struct {
		PlanMd               *string         `json:"plan_md"`
		Milestones           json.RawMessage `json:"milestones"`
		RequiredCapabilities []string        `json:"required_capabilities"`
		RequiredTools        []string        `json:"required_tools"`
		SizeClass            *string         `json:"size_class"`
	}{
		PlanMd:               p.PlanMd,
		RequiredCapabilities: sortedSet(p.RequiredCapabilities),
		RequiredTools:        sortedSet(p.RequiredTools),
		SizeClass:            p.SizeClass,
	}
	ms, err := canonicalRawJSON(p.Milestones)
	if err != nil {
		return nil, err
	}
	out.Milestones = ms
	return json.Marshal(out)
}

// digest returns the canonical encoding and its sha256.
func (p gatePresentedPayload) digest() (canonical, sum []byte, err error) {
	canonical, err = p.canonicalJSON()
	if err != nil {
		return nil, nil, err
	}
	h := sha256.Sum256(canonical)
	return canonical, h[:], nil
}

// requirements projects the requirement half of the payload for the gate-resume claim.
func (p gatePresentedPayload) requirements() *GatePresentedRequirements {
	return &GatePresentedRequirements{
		RequiredCapabilities: sortedSet(p.RequiredCapabilities),
		RequiredTools:        sortedSet(p.RequiredTools),
		SizeClass:            p.SizeClass,
	}
}

func sortedSet(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// canonicalRawJSON re-encodes raw JSON through a generic decode. Empty input and JSON null both
// become null, so an absent candidate and a stored SQL NULL compare equal.
func canonicalRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonicalize milestones: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicalize milestones: %w", err)
	}
	return out, nil
}

// decodeGatePresentedPayload reads a stored snapshot. ok is false when there is none.
func decodeGatePresentedPayload(raw []byte) (gatePresentedPayload, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return gatePresentedPayload{}, false, nil
	}
	var p gatePresentedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return gatePresentedPayload{}, false, fmt.Errorf("decode gate presented payload: %w", err)
	}
	return p, true, nil
}

// gatePayloadFromRow is the row's effective persisted approval-relevant payload: what the run
// columns hold right now. It initializes a snapshot where none exists yet (a first publication,
// or a migration-era adoption).
func gatePayloadFromRow(run store.Run) gatePresentedPayload {
	return gatePresentedPayload{
		PlanMd:               textPtr(run.PlanMd),
		Milestones:           json.RawMessage(run.MilestonesCandidate),
		RequiredCapabilities: run.RequiredCapabilities,
		RequiredTools:        run.RequiredTools,
		SizeClass:            nonEmptyPtr(run.SizeClass),
	}
}

// nonEmptyPtr maps the NOT NULL size_class column (empty string when unset) onto the payload's
// optional form, so an unset size and an absent one compare equal.
func nonEmptyPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// gateReportFields is what one awaiting_approval report contributes to the presented payload,
// already in the form SetRunAwaitingApproval writes (NUL-stripped plan_md, the resolved
// milestone candidate, the vocabulary-filtered requirement set).
type gateReportFields struct {
	planMd pgtype.Text
	// milestones is the candidate the report resolves to; milestonesReported says whether the
	// report carried a milestone list at all. An absent list falls back to the base.
	milestones         []byte
	milestonesReported bool
	caps               []string
	tools              []string
	sizeClass          pgtype.Text
}

// effectiveGatePayload computes the effective presented payload of a report against a base (the
// previous presented snapshot, or the row's persisted payload when there is none). Fields the
// report carries come from the report; every field SetRunAwaitingApproval keeps when absent
// takes the base's value:
//
//   - plan_md is a direct assignment in SetRunAwaitingApproval, so it is always the report's.
//   - milestones: the report's resolved candidate when it carried a list, else the base's.
//   - required_capabilities is union-merged (escalation-only), so it is base ∪ reported.
//   - required_tools / size_class are absent-safe replacements: reported when present.
func effectiveGatePayload(base gatePresentedPayload, f gateReportFields) gatePresentedPayload {
	out := gatePresentedPayload{
		PlanMd:               textPtr(f.planMd),
		Milestones:           base.Milestones,
		RequiredCapabilities: sortedSet(append(slices.Clone(base.RequiredCapabilities), f.caps...)),
		RequiredTools:        base.RequiredTools,
		SizeClass:            base.SizeClass,
	}
	if f.milestonesReported {
		out.Milestones = json.RawMessage(f.milestones)
	}
	if f.tools != nil {
		out.RequiredTools = f.tools
	}
	if f.sizeClass.Valid {
		out.SizeClass = textPtr(f.sizeClass)
	}
	return out
}

// gateClass is the classification of one awaiting_approval report.
type gateClass int

const (
	// gateClassUntracked: the report does not take part in revision allocation (a chat/judge
	// run, or no transaction beginner wired). SetRunAwaitingApproval runs as before.
	gateClassUntracked gateClass = iota
	// gateClassPublish allocates N+1 (an unseen id, or an id-less report).
	gateClassPublish
	// gateClassRetain re-publishes the current presentation (a current-id retry).
	gateClassRetain
	// gateClassAdopt binds a fresh id to the id-less gate at N.
	gateClassAdopt
	// gateClassDeclined: the run is in a status SetRunAwaitingApproval refuses, so the report
	// is declined before any classification and counts nothing.
	gateClassDeclined
)

// gateDecision is the outcome of classifyGateReport.
type gateDecision struct {
	class gateClass
	// refusal is set (with class unset) when the report is refused.
	refusal error
	// presentationID is the report's id (Valid=false for an id-less report).
	presentationID pgtype.UUID
	// payload/digest are the snapshot to write: the published payload for gateClassPublish,
	// the initialized snapshot for gateClassAdopt (kept by COALESCE when one exists).
	payload []byte
	digest  []byte
	// retained is the presented snapshot a retained presentation (retain, adopt) re-writes as
	// the milestone candidate, so the live candidate stays the one the human saw.
	retained gatePresentedPayload
}

// gateReportAdmissible mirrors SetRunAwaitingApproval's status predicate: a report onto a run in
// any other status is declined (0 rows) and must neither allocate nor count a refusal.
func gateReportAdmissible(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "limit_wait", "pool_wait", "recovery_wait", "paused":
		return false
	default:
		return true
	}
}

// classifyGateReport classifies an awaiting_approval report against the LOCKED run row. It reads
// run_gate_presentations through q (the locking transaction) and writes nothing.
func classifyGateReport(ctx context.Context, q Store, locked store.Run, req StateRequest, f gateReportFields) (gateDecision, error) {
	if !gateReportAdmissible(locked.Status) {
		return gateDecision{class: gateClassDeclined}, nil
	}
	rowPayload := gatePayloadFromRow(locked)
	snapshot, hasSnapshot, err := decodeGatePresentedPayload(locked.GatePresentedPayload)
	if err != nil {
		return gateDecision{}, err
	}
	base := rowPayload
	if hasSnapshot {
		base = snapshot
	}
	// sameAsPresented compares the report's effective payload against the stored digest (or,
	// with no stored digest, against the base computed the same way).
	sameAsPresented := func() (bool, error) {
		_, want, derr := base.digest()
		if derr != nil {
			return false, derr
		}
		if hasSnapshot && len(locked.GatePayloadDigest) > 0 {
			want = locked.GatePayloadDigest
		}
		_, got, derr := effectiveGatePayload(base, f).digest()
		if derr != nil {
			return false, derr
		}
		return bytes.Equal(got, want), nil
	}

	if req.PresentationID == nil {
		// An id-less report (an old worker): always a new publication.
		return publishDecision(rowPayload, f, pgtype.UUID{})
	}
	pid := pgconv.UUID(*req.PresentationID)
	if locked.GatePresentationID.Valid && uuid.UUID(locked.GatePresentationID.Bytes) == *req.PresentationID {
		// The current presentation. A retry after a lost adoption ACK lands here too.
		same, serr := sameAsPresented()
		if serr != nil {
			return gateDecision{}, serr
		}
		if !same {
			return gateDecision{refusal: ErrGatePresentationConflict}, nil
		}
		return gateDecision{class: gateClassRetain, presentationID: pid, retained: base}, nil
	}
	historical, err := q.RunGatePresentationExists(ctx, store.RunGatePresentationExistsParams{RunID: locked.ID, PresentationID: *req.PresentationID})
	if err != nil {
		return gateDecision{}, err
	}
	if historical {
		return gateDecision{refusal: ErrGatePresentationHistorical}, nil
	}
	if req.AdoptGateRevision != nil {
		if locked.GateRevision <= 0 || locked.GateRevision != *req.AdoptGateRevision || locked.GatePresentationID.Valid {
			return gateDecision{refusal: ErrGateAdoptionStale}, nil
		}
		same, serr := sameAsPresented()
		if serr != nil {
			return gateDecision{}, serr
		}
		if !same {
			return gateDecision{refusal: ErrGateAdoptionStale}, nil
		}
		canonical, sum, derr := base.digest()
		if derr != nil {
			return gateDecision{}, derr
		}
		return gateDecision{class: gateClassAdopt, presentationID: pid, payload: canonical, digest: sum, retained: base}, nil
	}
	// A previously unseen id: a new presentation.
	return publishDecision(rowPayload, f, pid)
}

// publishDecision builds a new publication: the snapshot is exactly what the row will hold once
// SetRunAwaitingApproval applies the report over the row's current persisted payload.
func publishDecision(rowPayload gatePresentedPayload, f gateReportFields, pid pgtype.UUID) (gateDecision, error) {
	f.milestonesReported = true // the written candidate IS the report's resolved candidate
	canonical, sum, err := effectiveGatePayload(rowPayload, f).digest()
	if err != nil {
		return gateDecision{}, err
	}
	return gateDecision{class: gateClassPublish, presentationID: pid, payload: canonical, digest: sum}, nil
}

// recordGateRefusal does a refusal's fenced accounting under the run-row lock (the caller holds
// it and has already verified ownership, the claim generation and an unreleased claim). It
// counts once per claim generation and, past the cap, fails the run. It returns the rows the
// transition changed (1 when the run failed) so SetState runs its terminal fan-out.
func (s *Service) recordGateRefusal(ctx context.Context, q Store, locked store.Run, wkr store.Worker, gen int64, refusal error, sessionID pgtype.Text) (int64, error) {
	if locked.GateRefusalGeneration.Valid && locked.GateRefusalGeneration.Int64 == gen {
		// Already counted for this claim (a retried lost 409, or a second refusal in the same
		// claim): answer the same refusal without counting again.
		return 0, nil
	}
	count := locked.GateRefusalCount + 1
	if err := q.SetRunGateRefusal(ctx, store.SetRunGateRefusalParams{
		RefusalCount:      count,
		RefusalGeneration: pgtype.Int8{Int64: gen, Valid: true},
		ID:                locked.ID,
	}); err != nil {
		return 0, err
	}
	maxRefusals := s.p.RunGateRefusalMax
	if maxRefusals == 0 || int(count) <= maxRefusals {
		return 0, nil
	}
	reason := fmt.Sprintf("the plan gate could not be re-presented: %s refused across %d claims", gateRefusalLabel(refusal), count)
	return q.SetRunFailed(ctx, store.SetRunFailedParams{
		FailureReason: pgconv.TextOrNull(reason),
		// SERVER-DERIVED, set directly, never through CoerceFailOrigin (it is not
		// worker-reportable).
		FailOrigin: pgconv.TextOrNull("gate_presentation_refused"),
		SessionID:  sessionID,
		ID:         locked.ID,
		WorkerID:   pgconv.UUID(wkr.ID),
		// Fenced by the caller's FOR UPDATE lock and generation check.
		ClaimGeneration: pgtype.Int8{},
	})
}

// applyGateDecision writes an accepted classification after SetRunAwaitingApproval affected the
// row, in the same transaction, and returns the revision the report is answered with.
func applyGateDecision(ctx context.Context, q Store, runID uuid.UUID, d gateDecision) (int64, error) {
	switch d.class {
	case gateClassPublish:
		rev, err := q.PublishRunGatePresentation(ctx, store.PublishRunGatePresentationParams{
			PresentationID:   d.presentationID,
			PresentedPayload: d.payload,
			PayloadDigest:    d.digest,
			ID:               runID,
		})
		if err != nil {
			return 0, err
		}
		if d.presentationID.Valid {
			if err := q.InsertRunGatePresentation(ctx, store.InsertRunGatePresentationParams{
				RunID: runID, PresentationID: uuid.UUID(d.presentationID.Bytes), Revision: rev,
			}); err != nil {
				return 0, err
			}
		}
		return rev, nil
	case gateClassAdopt:
		rev, err := q.AdoptRunGatePresentation(ctx, store.AdoptRunGatePresentationParams{
			PresentationID:   d.presentationID,
			PresentedPayload: d.payload,
			PayloadDigest:    d.digest,
			ID:               runID,
		})
		if err != nil {
			return 0, err
		}
		if err := q.InsertRunGatePresentation(ctx, store.InsertRunGatePresentationParams{
			RunID: runID, PresentationID: uuid.UUID(d.presentationID.Bytes), Revision: rev,
		}); err != nil {
			return 0, err
		}
		return rev, nil
	default:
		return 0, fmt.Errorf("applyGateDecision: unexpected class %d", d.class)
	}
}

// retainedMilestonesCandidate is the milestone candidate a retained presentation re-writes: the
// presented snapshot's list, or SQL NULL when the snapshot presented none.
func retainedMilestonesCandidate(p gatePresentedPayload) []byte {
	trimmed := bytes.TrimSpace(p.Milestones)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return trimmed
}
