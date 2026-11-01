package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// crossRunDiscardBinding is the separate immutable audit shape for whole-target
// policy discard. It deliberately has no worker transport mapping.
type crossRunDiscardBinding struct {
	Basis                      string    `json:"basis"`
	HoldID                     uuid.UUID `json:"hold_id"`
	Generation                 int64     `json:"generation"`
	TargetRunID                uuid.UUID `json:"target_run_id"`
	TargetTerminalGeneration   int64     `json:"target_terminal_generation"`
	TargetTerminalStatus       string    `json:"target_terminal_status"`
	WitnessRunID               uuid.UUID `json:"witness_run_id"`
	WitnessCompletedGeneration int64     `json:"witness_completed_generation"`
	OwnerID                    uuid.UUID `json:"owner_id"`
	RepoID                     uuid.UUID `json:"repo_id"`
	IssueIID                   int64     `json:"issue_iid"`
	TargetFinishedAt           time.Time `json:"target_finished_at"`
	WitnessCreatedAt           time.Time `json:"witness_created_at"`
	ConnectionID               uuid.UUID `json:"connection_id"`
	ForgeType                  string    `json:"forge_type"`
	BaseURL                    string    `json:"base_url"`
	ProjectID                  int64     `json:"project_id"`
	WitnessMRIID               int64     `json:"witness_mr_iid"`
	VerifiedAt                 time.Time `json:"verified_at"`
}

// DiscardCrossRunHolds settles an older terminal run using a freshly verified
// same-issue witness. Locks are run UUID order, repository/connection, every
// hold, publications, then non-ready captures. Work is bounded to 1000 rows per
// inventory and 1000 non-ready captures across siblings, with one overflow row.
// There are no retries; ctx bounds lock waits and any refusal rolls back siblings.
// Consumer admission after the recheck remains an accepted policy race.
func (s *Service) DiscardCrossRunHolds(ctx context.Context, target CrossRunTargetBinding, witness VerifiedCrossRunWitnessSnapshot) (int64, error) {
	if err := target.Validate(witness); err != nil {
		return 0, err
	}
	target.FinishedAt = target.FinishedAt.UTC().Truncate(time.Microsecond)
	b := witness.binding
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	var a store.LockCrossRunTargetRow
	var w store.LockCrossRunWitnessRow
	lockTarget := func() error {
		var e error
		a, e = q.LockCrossRunTarget(ctx, target.RunID)
		return e
	}
	lockWitness := func() error {
		var e error
		w, e = q.LockCrossRunWitness(ctx, b.RunID)
		return e
	}
	first, second := lockTarget, lockWitness
	if bytes.Compare(target.RunID[:], b.RunID[:]) > 0 {
		first, second = second, first
	}
	for _, lock := range []func() error{first, second} {
		if err := lock(); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrMergeSnapshotMismatch
			}
			return 0, err
		}
	}
	if a.UserID != target.OwnerID || a.RepoID != pgconv.UUID(target.RepoID) ||
		a.Kind != target.Kind || a.Status != target.Status || a.ClaimGeneration != target.Generation ||
		!a.IssueIid.Valid || a.IssueIid.Int64 != target.IssueIID || a.MrIid.Valid ||
		(a.FailOrigin.Valid && a.FailOrigin.String == "run_timeout") ||
		!crossRunTimestamp(a.FinishedAt) || !a.FinishedAt.Time.Equal(target.FinishedAt) ||
		w.UserID != b.OwnerID || w.RepoID != pgconv.UUID(b.RepoID) || w.Kind != b.Kind ||
		w.Status != "completed" || w.ClaimGeneration != b.CompletedGeneration ||
		!w.IssueIid.Valid || w.IssueIid.Int64 != b.IssueIID || !w.MrIid.Valid || w.MrIid.Int64 != b.MRIID ||
		!crossRunTimestamp(w.CreatedAt) || !w.CreatedAt.Time.Equal(b.CreatedAt) ||
		!a.FinishedAt.Time.Before(w.CreatedAt.Time) {
		return 0, ErrMergeSnapshotMismatch
	}
	binding, err := q.LockMergedPredecessorBinding(ctx, b.RepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMergeSnapshotMismatch
	}
	if err != nil {
		return 0, err
	}
	if binding.RepoID != b.RepoID || binding.OwnerID != b.OwnerID ||
		binding.ConnectionID != b.ConnectionID || binding.ProjectID != b.ProjectID ||
		binding.ForgeType != b.ForgeType || binding.BaseUrl != b.BaseURL {
		return 0, ErrMergeSnapshotMismatch
	}
	claim, err := q.HasCrossRunLiveClaim(ctx, target.RunID)
	if err != nil {
		return 0, err
	}
	consumer, err := q.HasCrossRunLiveConsumer(ctx, store.HasCrossRunLiveConsumerParams{
		OwnerID: target.OwnerID, RepoID: pgconv.UUID(target.RepoID), IssueIid: pgconv.Int8Ptr(&target.IssueIID),
	})
	if err != nil {
		return 0, err
	}
	if claim || consumer {
		return 0, ErrMergeSnapshotMismatch
	}
	holds, err := q.LockCrossRunInventory(ctx, target.RunID)
	if err != nil {
		return 0, err
	}
	if len(holds) > maxMergedPredecessorHolds {
		return 0, ErrMergedPredecessorWorkLimit
	}
	pubs, err := q.LockCrossRunPublications(ctx, target.RunID)
	if err != nil {
		return 0, err
	}
	if len(pubs) > maxMergedPredecessorHolds {
		return 0, ErrMergedPredecessorWorkLimit
	}
	byGeneration := make(map[int64]store.RecoveryCustodyHold, len(holds))
	byID := make(map[uuid.UUID]store.RecoveryCustodyHold, len(holds))
	for _, h := range holds {
		if h.ID == uuid.Nil || h.RunID != target.RunID || h.UserID != target.OwnerID ||
			h.RepoID != pgconv.UUID(target.RepoID) || h.Generation <= 0 || h.Generation > target.Generation ||
			h.OriginalWorkerID == uuid.Nil || h.OriginalWorkerIdentity == "" {
			return 0, crossRunRefusal(h.ID, "hold identity")
		}
		if _, duplicate := byGeneration[h.Generation]; duplicate {
			return 0, crossRunRefusal(h.ID, "duplicate generation")
		}
		byGeneration[h.Generation], byID[h.ID] = h, h
	}
	publications := make(map[uuid.UUID]store.RunHeldPublication, len(pubs))
	for _, p := range pubs {
		h, ok := byID[p.HoldID]
		if !ok || !crossRunPublicationIdentity(p, h) {
			return 0, crossRunRefusal(p.HoldID, "publication identity")
		}
		switch p.State {
		case "prepared", "invoked", "create_unknown":
			return 0, crossRunRefusal(h.ID, "publication create unresolved")
		case "created", "refused", "acknowledged", "delete_unknown", "deleted", "abandoned":
		default:
			return 0, crossRunRefusal(h.ID, "unknown publication state")
		}
		publications[p.ID] = p
	}
	shared, err := q.HasCrossRunSharedFinal(ctx, target.RunID)
	if err != nil {
		return 0, err
	}
	if shared {
		return 0, crossRunRefusal(uuid.Nil, "shared FINAL")
	}
	captures, err := q.ListCrossRunFinalCaptures(ctx, target.RunID)
	if err != nil {
		return 0, err
	}
	if len(captures) > maxMergedPredecessorHolds {
		return 0, ErrMergedPredecessorWorkLimit
	}
	finalCaptures := make(map[uuid.UUID]store.RecoveryCapture, len(captures))
	for _, c := range captures {
		finalCaptures[c.ID] = c
	}
	for _, h := range holds {
		if err := classifyCrossRunHold(h, target.Generation, byGeneration, publications, finalCaptures); err != nil {
			return 0, err
		}
	}
	remaining := maxMergedPredecessorCaptures
	var changed int64
	for _, h := range holds {
		if h.State != "open" {
			continue
		}
		// Lock capture siblings only after the entire hold/publication inventory.
		captures, err := q.LockMergedPredecessorNonReadyCaptures(ctx, store.LockMergedPredecessorNonReadyCapturesParams{
			HoldID: h.ID, CaptureLimit: int32(remaining + 1),
		})
		if err != nil {
			return 0, err
		}
		if len(captures) > remaining {
			return 0, ErrMergedPredecessorWorkLimit
		}
		remaining -= len(captures)
		audit, err := json.Marshal(crossRunDiscardBinding{
			Basis: "same_issue_witness", HoldID: h.ID, Generation: h.Generation,
			TargetRunID: target.RunID, TargetTerminalGeneration: target.Generation,
			TargetTerminalStatus: target.Status, TargetFinishedAt: target.FinishedAt,
			WitnessRunID: b.RunID, WitnessCompletedGeneration: b.CompletedGeneration,
			WitnessCreatedAt: b.CreatedAt, WitnessMRIID: b.MRIID,
			OwnerID: b.OwnerID, RepoID: b.RepoID, IssueIID: b.IssueIID,
			ConnectionID: b.ConnectionID, ForgeType: b.ForgeType, BaseURL: b.BaseURL,
			ProjectID: b.ProjectID, VerifiedAt: witness.verifiedAt,
		})
		if err != nil {
			return 0, err
		}
		n, err := q.DiscardCrossRunHold(ctx, store.DiscardCrossRunHoldParams{
			HoldID: h.ID, RunID: target.RunID, OwnerID: target.OwnerID, RepoID: pgconv.UUID(target.RepoID),
			Generation: h.Generation, TerminalGeneration: target.Generation, Binding: audit,
		})
		if err != nil {
			return 0, err
		}
		if n != 1 {
			return 0, crossRunRefusal(h.ID, "discard refused")
		}
		n, err = q.DiscardNonReadyCapturesForHold(ctx, h.ID)
		if err != nil {
			return 0, err
		}
		if n != int64(len(captures)) {
			return 0, crossRunRefusal(h.ID, "capture settlement refused")
		}
		changed++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if changed > 0 {
		s.custodySettledAfterCommit(target.RunID)
	}
	return changed, nil
}

func crossRunRefusal(id uuid.UUID, reason string) error {
	return fmt.Errorf("%w: hold %s: %s", ErrMergeSnapshotMismatch, id, reason)
}

func crossRunTimestamp(at pgtype.Timestamptz) bool {
	return at.Valid && at.InfinityModifier == pgtype.Finite && finiteCrossRunTime(at.Time)
}

func crossRunPublicationIdentity(p store.RunHeldPublication, h store.RecoveryCustodyHold) bool {
	return p.ID != uuid.Nil && p.RunID == h.RunID && p.HoldID == h.ID && p.UserID == h.UserID &&
		p.RepoID == uuid.UUID(h.RepoID.Bytes) && p.WorkerID == h.OriginalWorkerID && p.Generation == h.Generation &&
		p.Ref == fmt.Sprintf("refs/uzi-held/%s/%d", h.RunID, h.Generation) &&
		crossRunFullSHA(p.Tip) && validCoverageDigest(p.CoverageDigest) &&
		(!p.LiveRunID.Valid || uuid.UUID(p.LiveRunID.Bytes) == h.RunID)
}

func crossRunFullSHA(sha string) bool {
	normalized, err := validateSha(sha, false)
	return err == nil && normalized == sha && (len(sha) == 40 || len(sha) == 64)
}

// classifyCrossRunHold recognizes stored dispositions, never renews their proofs.
// In particular, an archive is not a transfer, and live ancestry within this
// target is allowed only with its matching, strictly later successor hold.
func classifyCrossRunHold(h store.RecoveryCustodyHold, terminal int64, generations map[int64]store.RecoveryCustodyHold,
	publications map[uuid.UUID]store.RunHeldPublication, captures map[uuid.UUID]store.RecoveryCapture) error {
	refuse := func(reason string) error { return crossRunRefusal(h.ID, reason) }
	ancestry := h.ReleasePushedSha.Valid || h.ReleaseSourceSha.Valid || h.ReleaseAdoptedSha.Valid ||
		h.ReleaseFinalHeadSha.Valid || h.ReleaseSuccessorGeneration.Valid || h.ReleaseBranch.Valid || h.ReleaseTarget.Valid
	final := h.FinalDisposition.Valid || h.FinalCaptureID.Valid || h.FinalPublicationID.Valid ||
		h.FinalSourceSha.Valid || h.FinalCoverageDigest.Valid || len(h.CompletedPublicationReceipt) != 0
	if h.State == "open" {
		if !h.LiveWorkerID.Valid || uuid.UUID(h.LiveWorkerID.Bytes) != h.OriginalWorkerID ||
			!h.LiveRunID.Valid || uuid.UUID(h.LiveRunID.Bytes) != h.RunID || h.ReleasedAt.Valid ||
			h.ReleaseEvidence.Valid || ancestry || final || len(h.MergedPredecessorDiscardBinding) != 0 {
			return refuse("conflicting open evidence")
		}
		return nil
	}
	if h.LiveWorkerID.Valid || h.LiveRunID.Valid || !h.ReleaseEvidence.Valid {
		return refuse("closed live pointer or missing evidence")
	}
	ev := h.ReleaseEvidence.String
	if ev == "merged_predecessor_discard" {
		if h.State != "discarded" || !crossRunHistoricalBinding(h) {
			return refuse("discard binding")
		}
		// The guarded policy transition preserves any prior FINAL and ancestry.
		return nil
	}
	if len(h.MergedPredecessorDiscardBinding) != 0 {
		return refuse("conflicting discard binding")
	}
	if ev == "owner_discard" {
		if h.State != "discarded" || ancestry || final || (h.ReleasedAt.Valid && !crossRunTimestamp(h.ReleasedAt)) {
			return refuse("owner discard shape")
		}
		return nil
	}
	if h.State != "released" || !crossRunTimestamp(h.ReleasedAt) {
		return refuse("release state or time")
	}
	if ev == "ancestry" || ev == "live_ancestry" {
		if final || h.InventoryGuarded || !h.ReleaseSuccessorGeneration.Valid ||
			h.ReleaseSuccessorGeneration.Int64 <= h.Generation || h.ReleaseSuccessorGeneration.Int64 > terminal ||
			!h.ReleaseBranch.Valid || h.ReleaseBranch.String == "" || len(h.ReleaseBranch.String) > 255 ||
			!h.ReleasePushedSha.Valid || !crossRunFullSHA(h.ReleasePushedSha.String) ||
			!h.ReleaseSourceSha.Valid || !crossRunFullSHA(h.ReleaseSourceSha.String) ||
			!h.ReleaseAdoptedSha.Valid || !crossRunFullSHA(h.ReleaseAdoptedSha.String) ||
			!h.ReleaseFinalHeadSha.Valid || !crossRunFullSHA(h.ReleaseFinalHeadSha.String) {
			return refuse("ancestry audit")
		}
		if ev == "live_ancestry" {
			successor, ok := generations[h.ReleaseSuccessorGeneration.Int64]
			if !ok || successor.RunID != h.RunID || successor.UserID != h.UserID || successor.RepoID != h.RepoID ||
				successor.OriginalWorkerID != h.OriginalWorkerID || !h.ReleaseTarget.Valid ||
				(h.ReleaseTarget.String != "checkpoint" && h.ReleaseTarget.String != "branch") {
				return refuse("foreign or missing live ancestry successor")
			}
		} else if h.ReleaseTarget.Valid {
			return refuse("completed ancestry target")
		}
		return nil
	}
	if ancestry {
		return refuse("conflicting ancestry audit")
	}
	if !h.InventoryGuarded {
		if final {
			return refuse("legacy FINAL")
		}
		switch ev {
		case "publication", "archive", "forge_no_output", "no_adopted_source":
			return nil
		default:
			return refuse("unknown legacy evidence")
		}
	}
	if !h.FinalDisposition.Valid {
		return refuse("missing FINAL")
	}
	switch h.FinalDisposition.String {
	case "archive":
		c, ok := captures[uuid.UUID(h.FinalCaptureID.Bytes)]
		if ev != "archive" || !h.FinalCaptureID.Valid || !ok || h.FinalPublicationID.Valid ||
			!h.FinalSourceSha.Valid || !h.FinalCoverageDigest.Valid || !validCoverageDigest(h.FinalCoverageDigest.String) ||
			c.HoldID != h.ID || c.RunID != h.RunID || c.UserID != h.UserID ||
			!c.OriginalWorkerID.Valid || uuid.UUID(c.OriginalWorkerID.Bytes) != h.OriginalWorkerID ||
			c.OriginalWorkerIdentity != h.OriginalWorkerIdentity || !c.ManifestBound ||
			!crossRunSourceSHA(h.FinalSourceSha.String) || c.SourceSha != h.FinalSourceSha.String || !c.CoverageDigest.Valid || c.CoverageDigest.String != h.FinalCoverageDigest.String {
			return refuse("archive FINAL identity")
		}
	case "held_publication":
		p, ok := publications[uuid.UUID(h.FinalPublicationID.Bytes)]
		if ev != "held_publication" || !h.FinalPublicationID.Valid || !ok || h.FinalCaptureID.Valid ||
			!h.FinalSourceSha.Valid || !h.FinalCoverageDigest.Valid ||
			p.Tip != h.FinalSourceSha.String || p.CoverageDigest != h.FinalCoverageDigest.String {
			return refuse("publication FINAL identity")
		}
	case "settled":
		if (ev != "publication" && ev != "forge_no_output") || h.FinalCaptureID.Valid || h.FinalPublicationID.Valid ||
			h.FinalSourceSha.Valid || !h.FinalCoverageDigest.Valid || h.FinalCoverageDigest.String != emptyInventoryDigest {
			return refuse("settled FINAL")
		}
	case "no_adopted_source":
		if ev != "no_adopted_source" || h.FinalCaptureID.Valid || h.FinalPublicationID.Valid ||
			h.FinalSourceSha.Valid || h.FinalCoverageDigest.Valid {
			return refuse("no source FINAL")
		}
	case "completed_publication":
		if !crossRunCompletedReceipt(h) {
			return refuse("completed publication receipt")
		}
	default:
		return refuse("unknown FINAL")
	}
	if h.FinalDisposition.String != "completed_publication" && len(h.CompletedPublicationReceipt) != 0 {
		return refuse("conflicting completed receipt")
	}
	return nil
}

func crossRunCompletedReceipt(h store.RecoveryCustodyHold) bool {
	var r apitypes.CompletedPublicationReceipt
	var frozen map[string]any
	var receipt map[string]any
	if json.Unmarshal(h.CompletedPublicationReceipt, &r) != nil ||
		json.Unmarshal(h.CompletionIdentity, &frozen) != nil ||
		json.Unmarshal(h.CompletedPublicationReceipt, &receipt) != nil || frozen == nil || receipt == nil {
		return false
	}
	if len(frozen) != 13 || len(receipt) != 14 || !crossRunFullSHA(r.FinalHead) || !crossRunFullSHA(r.ObservedBranchHead) {
		return false
	}
	delete(receipt, "observed_branch_head")
	if !reflect.DeepEqual(frozen, receipt) || h.CompletedPublicationReason.Valid ||
		h.FinalCaptureID.Valid || h.FinalPublicationID.Valid || h.FinalSourceSha.Valid || h.FinalCoverageDigest.Valid {
		return false
	}
	out := apitypes.RecoveryExactHold{
		HoldID: h.ID.String(), RunID: h.RunID.String(), RepoID: uuid.UUID(h.RepoID.Bytes).String(),
		OriginalWorkerID: h.OriginalWorkerID.String(), Generation: h.Generation, State: h.State,
		ReleaseEvidence:             h.ReleaseEvidence.String,
		FinalReceipt:                &apitypes.RecoveryFinalDisposition{Kind: "completed_publication"},
		CompletedPublicationReceipt: &r,
	}
	return validExactClosedObservation(out, h.UserID)
}

func crossRunSourceSHA(sha string) bool {
	normalized, err := validateSha(sha, false)
	return err == nil && normalized == sha && sha != ""
}

func crossRunHistoricalBinding(h store.RecoveryCustodyHold) bool {
	var b map[string]json.RawMessage
	if json.Unmarshal(h.MergedPredecessorDiscardBinding, &b) != nil {
		return false
	}
	matches := func(key string, value any) bool {
		want, err := json.Marshal(value)
		return err == nil && bytes.Equal(b[key], want)
	}
	if !matches("hold_id", h.ID) || !matches("generation", h.Generation) ||
		!matches("owner_id", h.UserID) || !matches("repo_id", uuid.UUID(h.RepoID.Bytes)) {
		return false
	}
	positive := func(key string) int64 {
		var n int64
		if json.Unmarshal(b[key], &n) != nil || n <= 0 {
			return 0
		}
		return n
	}
	text := func(key string) string {
		var value string
		_ = json.Unmarshal(b[key], &value)
		return value
	}
	stamp := func(key string) time.Time {
		var at time.Time
		if json.Unmarshal(b[key], &at) != nil || !finiteCrossRunTime(at) {
			return time.Time{}
		}
		return at
	}
	at := stamp("verified_at")
	forge := text("forge_type")
	if at.IsZero() || at.After(time.Now()) || !exactUUID(text("connection_id")) ||
		text("base_url") == "" || positive("project_id") == 0 ||
		(forge != "github" && forge != "gitlab" && forge != "forgejo") {
		return false
	}
	if matches("basis", "same_issue_witness") {
		finished, created := stamp("target_finished_at"), stamp("witness_created_at")
		status := text("target_terminal_status")
		return len(b) == 19 && matches("target_run_id", h.RunID) &&
			positive("target_terminal_generation") >= h.Generation &&
			(status == "failed" || status == "cancelled") && positive("issue_iid") > 0 &&
			exactUUID(text("witness_run_id")) && text("witness_run_id") != h.RunID.String() &&
			positive("witness_completed_generation") > 0 && positive("witness_mr_iid") > 0 &&
			!finished.IsZero() && !created.IsZero() && finished.Before(created)
	}
	return len(b) == 12 && matches("run_id", h.RunID) &&
		positive("completed_generation") > h.Generation && positive("mr_iid") > 0
}
