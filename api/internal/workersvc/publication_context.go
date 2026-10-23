package workersvc

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type PublicationContextRequest struct {
	ClaimGeneration int64  `json:"claim_generation"`
	Branch          string `json:"branch"`
	CandidateHead   string `json:"candidate_head"`
}

// PublicationContextResponse has exactly one of reason or binding.
// No source record is projected through RunDTO.
type PublicationContextResponse struct {
	Version int                        `json:"version"`
	Status  string                     `json:"status"`
	Reason  string                     `json:"reason,omitempty"`
	Binding *PublicationContextBinding `json:"binding,omitempty"`
}

type PublicationContextBinding struct {
	RunID             uuid.UUID                `json:"run_id"`
	ClaimGeneration   int64                    `json:"claim_generation"`
	Branch            string                   `json:"branch"`
	CandidateHead     string                   `json:"candidate_head"`
	MRIID             int64                    `json:"mr_iid"`
	SourceRunID       uuid.UUID                `json:"source_run_id"`
	SourcePublication *SourcePublicationIntent `json:"source_publication"`
	IssueIID          *int64                   `json:"issue_iid"`
	Intent            string                   `json:"intent"`
}

type publicationAuthority struct {
	ID                  uuid.UUID       `json:"id"`
	UserID              uuid.UUID       `json:"user_id"`
	RepoID              *uuid.UUID      `json:"repo_id"`
	WorkerID            *uuid.UUID      `json:"worker_id"`
	Kind                string          `json:"kind"`
	Status              string          `json:"status"`
	ClaimGeneration     int64           `json:"claim_generation"`
	ClaimReleasedAt     *string         `json:"claim_released_at"`
	Branch              *string         `json:"branch"`
	PipelineRef         *string         `json:"pipeline_ref"`
	MRIID               *int64          `json:"mr_iid"`
	TargetRunID         *uuid.UUID      `json:"target_run_id"`
	IssueIID            *int64          `json:"issue_iid"`
	ReportOnly          bool            `json:"report_only"`
	StopKind            *string         `json:"stop_kind"`
	NonClosingReason    *string         `json:"completion_non_closing_reason"`
	ContractVersion     *int32          `json:"completion_contract_version"`
	ContractRevision    *int32          `json:"contract_revision"`
	Contract            json.RawMessage `json:"completion_contract"`
	MilestonesFrozen    json.RawMessage `json:"milestones_frozen"`
	MilestonesCompleted json.RawMessage `json:"milestones_completed"`
	Stamp               json.RawMessage `json:"source_publication_intent"`
}

type publicationNode struct {
	Run   publicationAuthority `json:"run"`
	Seed  uuid.UUID            `json:"seed"`
	Depth int                  `json:"depth"`
	Cycle bool                 `json:"cycle"`
}
type publicationSnapshot struct {
	Request            publicationAuthority `json:"request"`
	ConnectionID       *uuid.UUID           `json:"connection_id"`
	ProjectID          int64                `json:"project_id"`
	ForgeType          string               `json:"forge_type"`
	BaseURL            string               `json:"base_url"`
	TokenCiphertext    []byte               `json:"token_ciphertext"`
	Nodes              []publicationNode    `json:"nodes"`
	AncestryUnknown    bool                 `json:"ancestry_unknown"`
	AssociationUnknown bool                 `json:"association_unknown"`
	CIMRConflict       bool                 `json:"ci_mr_conflict"`
}

func publicationUnknown(reason string) PublicationContextResponse {
	return PublicationContextResponse{Version: 1, Status: "unknown", Reason: reason}
}

func publicationSafeID(id int64) bool { return id > 0 && id <= 9007199254740991 }

func (r PublicationContextRequest) Valid() bool {
	return publicationSafeID(r.ClaimGeneration) && len(r.Branch) <= 1024 && safePublicationIdentifier(r.Branch) && isSettleSHA(r.CandidateHead)
}

func publicationLive(a publicationAuthority, w store.Worker, id uuid.UUID, req PublicationContextRequest) bool {
	return a.ID == id && a.UserID == w.UserID && a.WorkerID != nil && *a.WorkerID == w.ID &&
		a.ClaimGeneration > 0 && a.ClaimGeneration == req.ClaimGeneration && a.ClaimReleasedAt == nil &&
		(a.Status == "claimed" || a.Status == "running") && a.PipelineRef != nil && *a.PipelineRef == req.Branch &&
		(a.Kind == "mr_rework" || a.Kind == "ci_fix") && a.RepoID != nil
}

func rawPublicationValue(b json.RawMessage) []byte {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	return b
}

func (a publicationAuthority) restriction() (bool, error) {
	r := store.Run{
		CompletionContract:         rawPublicationValue(a.Contract),
		MilestonesFrozen:           rawPublicationValue(a.MilestonesFrozen),
		MilestonesCompleted:        rawPublicationValue(a.MilestonesCompleted),
		CompletionNonClosingReason: pgconv.TextPtr(a.NonClosingReason),
		StopKind:                   pgconv.TextPtr(a.StopKind),
	}
	if a.ContractVersion != nil {
		r.CompletionContractVersion.Int32 = *a.ContractVersion
		r.CompletionContractVersion.Valid = true
	}
	if a.ContractRevision != nil {
		r.ContractRevision.Int32 = *a.ContractRevision
		r.ContractRevision.Valid = true
	}
	return sourcePublicationRestriction(r, false)
}

// Each ancestry is at most 16 edges. A bad sibling refuses the complete answer;
// no newest-row heuristic can hide ambiguity or an unresolved association.
func resolvePublicationSnapshot(s publicationSnapshot, w store.Worker, id uuid.UUID, req PublicationContextRequest) PublicationContextResponse {
	if !publicationLive(s.Request, w, id, req) {
		return publicationUnknown("claim_changed")
	}
	if s.AncestryUnknown {
		return publicationUnknown("ancestry_unknown")
	}
	if s.ConnectionID == nil || !publicationSafeID(s.ProjectID) || s.CIMRConflict || s.AssociationUnknown {
		return publicationUnknown("association_unknown")
	}
	seeds := map[uuid.UUID]bool{}
	nodes := map[uuid.UUID]publicationAuthority{}
	for _, n := range s.Nodes {
		seeds[n.Seed] = true
		if n.Cycle || n.Depth >= 16 {
			return publicationUnknown("ancestry_unknown")
		}
		a := n.Run
		if a.UserID != w.UserID || a.RepoID == nil || *a.RepoID != *s.Request.RepoID ||
			a.MRIID == nil || !publicationSafeID(*a.MRIID) {
			return publicationUnknown("association_unknown")
		}
		if a.Kind == "mr_rework" {
			if a.PipelineRef == nil || *a.PipelineRef != req.Branch || (a.Branch != nil && *a.Branch != req.Branch) ||
				a.TargetRunID == nil || n.Depth == 15 {
				return publicationUnknown("ancestry_unknown")
			}
		} else if a.Branch == nil || *a.Branch != req.Branch {
			return publicationUnknown("association_unknown")
		}
		nodes[a.ID] = a
	}
	if len(seeds) == 0 {
		return publicationUnknown("association_unknown")
	}
	roots := map[uuid.UUID]publicationAuthority{}
	for seed := range seeds {
		current := seed
		seen := map[uuid.UUID]bool{}
		for hop := 0; ; hop++ {
			a, ok := nodes[current]
			if !ok || seen[current] || hop >= 16 {
				return publicationUnknown("ancestry_unknown")
			}
			seen[current] = true
			if a.Kind != "mr_rework" {
				roots[a.ID] = a
				break
			}
			child, ok := nodes[*a.TargetRunID]
			if !ok || *child.MRIID != *a.MRIID {
				return publicationUnknown("association_unknown")
			}
			current = *a.TargetRunID
		}
	}
	if len(roots) != 1 {
		return publicationUnknown("ambiguous_source")
	}
	var source publicationAuthority
	for _, a := range roots {
		source = a
	}
	if s.Request.MRIID != nil && *s.Request.MRIID != *source.MRIID {
		return publicationUnknown("association_unknown")
	}
	binding := &PublicationContextBinding{RunID: id, ClaimGeneration: req.ClaimGeneration, Branch: req.Branch,
		CandidateHead: req.CandidateHead, MRIID: *source.MRIID, SourceRunID: source.ID, IssueIID: source.IssueIID}
	if source.Kind == "prompt" || source.Kind == "self_improve" {
		binding.Intent = "not_applicable"
		// A self-improvement run carries its tracking issue, which it never closes; the wire
		// contract sends no source issue for a not-applicable source.
		binding.IssueIID = nil
		return PublicationContextResponse{Version: 1, Status: "not_applicable", Binding: binding}
	}
	if source.Kind != "issue" || source.IssueIID == nil || !publicationSafeID(*source.IssueIID) || source.ReportOnly {
		return publicationUnknown("source_unknown")
	}
	restricted, err := source.restriction()
	// Positive restrictions remain evidence on failed/noncompleted historical sources.
	// Invalid required contract authority never establishes closing.
	if err != nil && !restricted {
		return publicationUnknown("authority_unknown")
	}
	if len(rawPublicationValue(source.Stamp)) > 0 {
		var stamp SourcePublicationIntent
		d := json.NewDecoder(bytes.NewReader(source.Stamp))
		d.DisallowUnknownFields()
		if d.Decode(&stamp) != nil || stamp.Version != 1 || stamp.ClaimGeneration <= 0 ||
			stamp.Branch != req.Branch || stamp.MRIID != *source.MRIID || !isSettleSHA(stamp.PublishedHead) ||
			(stamp.Intent != "closing" && stamp.Intent != "non_closing") || (stamp.ReportedNonClosing && stamp.Intent != "non_closing") ||
			source.Status != "completed" {
			return publicationUnknown("authority_unknown")
		}
		binding.SourcePublication = &stamp
		if stamp.Intent == "non_closing" {
			restricted = true
		}
		if !restricted && err != nil {
			return publicationUnknown("authority_unknown")
		}
	} else if !restricted {
		return publicationUnknown("publication_unproven")
	}
	binding.Intent = "closing"
	if restricted {
		binding.Intent = "non_closing"
	}
	return PublicationContextResponse{Version: 1, Status: "resolved", Binding: binding}
}

type publicationSnapshotReader interface {
	PublicationContextSnapshot(context.Context, store.PublicationContextSnapshotParams) ([]byte, error)
}

// ResolvePublicationContext performs bounded reads and one forge summary call with no
// database locks. A short final transaction locks worker -> requesting run, then
// rechecks the entire authority graph in one READ COMMITTED statement.
func (s *Service) ResolvePublicationContext(ctx context.Context, w store.Worker, id uuid.UUID, req PublicationContextRequest) PublicationContextResponse {
	if !req.Valid() || id == uuid.Nil {
		return publicationUnknown("invalid_request")
	}
	if s.txBeginner == nil || s.forges == nil {
		return publicationUnknown("authority_unknown")
	}
	q, ok := s.q.(publicationSnapshotReader)
	if !ok {
		return publicationUnknown("authority_unknown")
	}
	params := store.PublicationContextSnapshotParams{RunID: id, UserID: w.UserID}
	readCtx, cancel := context.WithTimeout(ctx, completedPublicationDeadline)
	defer cancel()
	before, err := q.PublicationContextSnapshot(readCtx, params)
	if err != nil {
		return publicationUnknown("association_unknown")
	}
	var snapshot publicationSnapshot
	if json.Unmarshal(before, &snapshot) != nil {
		return publicationUnknown("authority_unknown")
	}
	answer := resolvePublicationSnapshot(snapshot, w, id, req)
	if answer.Binding == nil {
		return answer
	}
	f, err := s.forges.ForgeForConnection(snapshot.ForgeType, snapshot.BaseURL, snapshot.TokenCiphertext)
	if err != nil {
		return publicationUnknown("forge_unknown")
	}
	summary, err := f.GetMergeRequestSummary(readCtx, snapshot.ProjectID, answer.Binding.MRIID)
	if err != nil {
		return publicationUnknown("forge_unknown")
	}
	if summary.IID != answer.Binding.MRIID || summary.State != forge.MRStateOpened ||
		!safePublicationIdentifier(summary.SourceBranch) || summary.SourceBranch != req.Branch || !isSettleSHA(summary.HeadSHA) {
		return publicationUnknown("forge_mismatch")
	}
	txCtx, txCancel := context.WithTimeout(ctx, completedPublicationDeadline)
	defer txCancel()
	tx, err := s.beginReadCommitted(txCtx)
	if err != nil {
		return publicationUnknown("authority_unknown")
	}
	defer func() { _ = tx.Rollback(txCtx) }()
	qt := store.New(tx)
	worker, err := qt.GetWorkerForUpdate(txCtx, w.ID)
	if err != nil || worker.UserID != w.UserID {
		return publicationUnknown("claim_changed")
	}
	run, err := qt.GetRunOwnedByWorkerForUpdate(txCtx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(w.ID)})
	if err != nil || run.UserID != w.UserID || run.ClaimGeneration <= 0 || run.ClaimGeneration != req.ClaimGeneration ||
		run.ClaimReleasedAt.Valid || (run.Status != "claimed" && run.Status != "running") {
		return publicationUnknown("claim_changed")
	}
	after, err := qt.PublicationContextSnapshot(txCtx, params)
	if err != nil || !bytes.Equal(before, after) {
		return publicationUnknown("authority_changed")
	}
	// This successful read grants no lease; claim loss after this point remains a race.
	return answer
}
