package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// PRD #1798 D9: the staged -> bound -> acknowledged PR-description artifact.
//
// Every write runs in ONE transaction that first row-locks the run through
// GetRunOwnedByWorkerForUpdate (the run must be held by this worker, owned by the worker's
// user, non-terminal and repo-ful) and checks the request's claim_generation against the LOCKED
// row: a released claim or an advanced generation is ErrPrDescriptionStaleClaim (409), exactly
// the completion-permit fence. Locks are then taken in one order everywhere: run row, then the
// PR's pr_descriptions row, then version rows, so two runs acking the same PR cannot deadlock.

// PR-description outcome and state vocabularies (the migration's CHECK sets).
const (
	PrDescOutcomePublished = "published"

	prDescStatePending   = "pending"
	prDescStatePublished = "published"

	prDescMatchPublished = "published"
	prDescMatchPending   = "pending"
	prDescMatchNone      = "none"

	// PrDescSourceDeterministic is the D8 rung-3 source: the version carries no model or lead
	// text, so its raw fields are ignored (never validated, never a 400) and stored empty.
	PrDescSourceDeterministic = "deterministic_only"

	maxPrDescTargetBranchBytes = 255
	maxPrDescSizeLines         = int64(1) << 40

	// MaxPrDescPendingVersionsPerRun caps a run's pending versions staged under its LIVE claim
	// generation. A normal publication stages once, plus at most one D11 regeneration; a stage
	// past the cap is 409 too_many_versions. Older generations do not count: at stage their
	// unbound pending versions are abandoned, and their BOUND pending versions stay pending
	// (lost-ack recovery matches against them), so crashed attempts cannot lock a run out.
	MaxPrDescPendingVersionsPerRun = 20
	// MaxPrDescVersionsPerRun is the per-run backstop over every version in any state and
	// generation (a run reclaimed over and over): a stage at it is 409 too_many_versions.
	MaxPrDescVersionsPerRun = 200
)

var validPrDescSources = map[string]bool{"generated": true, "lead_only": true, PrDescSourceDeterministic: true}

var validPrDescOutcomes = map[string]bool{
	PrDescOutcomePublished: true, "skipped_human_edit": true, "skipped_no_region": true,
	"skipped_malformed": true, "skipped_snapshot_moved": true, "write_failed": true,
}

var prDescSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PR-description sentinel errors, mapped to HTTP status by the handler.
var (
	// ErrPrDescriptionStaleClaim: the request's claim_generation is not the run's live claim (a
	// reclaim advanced it, or a held-state switch released it), or the version was staged under
	// another generation → 409 reason stale_claim.
	ErrPrDescriptionStaleClaim = errors.New("pr description: claim generation is not the run's live claim")
	// ErrPrDescriptionLockConflict: the ack's expected_lock_version is not the PR's current
	// lock_version (another writer acked first) → 409 reason lock_conflict.
	ErrPrDescriptionLockConflict = errors.New("pr description: lock_version changed")
	// ErrPrDescriptionVersionConflict: the version cannot take this transition (already bound to
	// another PR, not bound yet, already published or abandoned), or the request names a PR other
	// than the run's own (runs.mr_iid, or the PR the run first staged/bound for) → 409 reason
	// version_conflict.
	ErrPrDescriptionVersionConflict = errors.New("pr description: version state does not allow this")
	// ErrPrDescriptionTooManyVersions: the run already holds MaxPrDescPendingVersionsPerRun
	// pending versions in its live generation, or MaxPrDescVersionsPerRun versions in all →
	// 409 reason too_many_versions.
	ErrPrDescriptionTooManyVersions = errors.New("pr description: too many pending versions for this run")
	// ErrPrDescriptionVersionNotFound: no such version for this run → 404.
	ErrPrDescriptionVersionNotFound = errors.New("pr description: version not found for this run")
	// errPrDescriptionNoTx: neither a pgx transaction source nor a transactional store is wired.
	errPrDescriptionNoTx = errors.New("pr description: no transaction source wired")
)

// PrDescQueries is the statement set one PR-description transaction uses. Exported so a
// handler-package fake can implement the transactional surface.
type PrDescQueries interface {
	GetRunOwnedByWorkerForUpdate(ctx context.Context, arg store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error)
	InsertPrDescriptionVersion(ctx context.Context, arg store.InsertPrDescriptionVersionParams) (store.PrDescriptionVersion, error)
	GetPrDescriptionVersionForRunForUpdate(ctx context.Context, arg store.GetPrDescriptionVersionForRunForUpdateParams) (store.PrDescriptionVersion, error)
	BindPrDescriptionVersion(ctx context.Context, arg store.BindPrDescriptionVersionParams) (store.PrDescriptionVersion, error)
	EnsurePrDescription(ctx context.Context, arg store.EnsurePrDescriptionParams) error
	GetPrDescriptionForUpdate(ctx context.Context, arg store.GetPrDescriptionForUpdateParams) (store.PrDescription, error)
	GetPrDescriptionVersionByID(ctx context.Context, id uuid.UUID) (store.PrDescriptionVersion, error)
	FindPrDescriptionVersionByRegionHash(ctx context.Context, arg store.FindPrDescriptionVersionByRegionHashParams) (store.PrDescriptionVersion, error)
	MarkPrDescriptionVersionPublished(ctx context.Context, id uuid.UUID) (int64, error)
	MarkPrDescriptionVersionAbandoned(ctx context.Context, id uuid.UUID) (int64, error)
	SetPrDescriptionPublished(ctx context.Context, arg store.SetPrDescriptionPublishedParams) (int64, error)
	SetPrDescriptionOutcome(ctx context.Context, arg store.SetPrDescriptionOutcomeParams) (int64, error)
	RecoverPrDescriptionLostAck(ctx context.Context, arg store.RecoverPrDescriptionLostAckParams) (int64, error)
	FirstPrDescriptionMrIidForRun(ctx context.Context, runID uuid.UUID) (int64, error)
	CountPendingPrDescriptionVersionsForRunGeneration(ctx context.Context, arg store.CountPendingPrDescriptionVersionsForRunGenerationParams) (int64, error)
	CountPrDescriptionVersionsForRun(ctx context.Context, runID uuid.UUID) (int64, error)
	AbandonStalePrDescriptionVersionsForRun(ctx context.Context, arg store.AbandonStalePrDescriptionVersionsForRunParams) (int64, error)
}

// PrDescTx is one open PR-description transaction.
type PrDescTx interface {
	PrDescQueries
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// prDescBeginner is an optional Store surface (the claimFinishBeginner idiom): an in-memory
// transactional fake implements it so unit tests drive the same fenced logic. Production opens
// the transaction on txBeginner.
type prDescBeginner interface {
	BeginPrDescTx(ctx context.Context) (PrDescTx, error)
}

// prDescReader is the optional read surface the claim, the run DTO overlay and the lookup use.
// It is deliberately NOT part of Store: a fake store that embeds Store does not satisfy it, so
// the best-effort readers skip instead of calling an unimplemented method.
type prDescReader interface {
	GetPrDescription(ctx context.Context, arg store.GetPrDescriptionParams) (store.PrDescription, error)
	GetPrDescriptionVersionByID(ctx context.Context, id uuid.UUID) (store.PrDescriptionVersion, error)
	FindPrDescriptionVersionByRegionHash(ctx context.Context, arg store.FindPrDescriptionVersionByRegionHashParams) (store.PrDescriptionVersion, error)
	LatestBoundPrDescriptionVersionForRun(ctx context.Context, runID uuid.UUID) (store.PrDescriptionVersion, error)
}

type pgxPrDescTx struct {
	*store.Queries
	tx pgx.Tx
}

func (t pgxPrDescTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t pgxPrDescTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

func (s *Service) beginPrDesc(ctx context.Context) (PrDescTx, error) {
	if s.txBeginner != nil {
		tx, err := s.txBeginner.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return pgxPrDescTx{Queries: store.New(tx), tx: tx}, nil
	}
	if b, ok := s.q.(prDescBeginner); ok {
		return b.BeginPrDescTx(ctx)
	}
	return nil, errPrDescriptionNoTx
}

// withPrDescTx runs fn in one transaction, committing only when fn succeeds.
func (s *Service) withPrDescTx(ctx context.Context, fn func(q PrDescQueries) error) error {
	tx, err := s.beginPrDesc(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// checkPrDescRun is the fence every PR-description call applies to the run row: owned by the
// worker's user, the live (unreleased) claim at this generation, non-terminal, repo-ful.
func checkPrDescRun(run store.Run, wkr store.Worker, gen int64) error {
	if run.UserID != wkr.UserID {
		return ErrRunNotOwned
	}
	if run.ClaimGeneration != gen || run.ClaimReleasedAt.Valid {
		return ErrPrDescriptionStaleClaim
	}
	if terminalStatuses[run.Status] {
		return ErrRunTerminal
	}
	if !run.RepoID.Valid {
		return ErrSummaryRepoRequired
	}
	return nil
}

// lockPrDescRun row-locks the run for this worker and applies the fence.
func lockPrDescRun(ctx context.Context, q PrDescQueries, wkr store.Worker, runID uuid.UUID, gen int64) (store.Run, error) {
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true}})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotOwned
		}
		return store.Run{}, err
	}
	return run, checkPrDescRun(run, wkr, gen)
}

// cleanPrDescSHA normalizes a worker-reported commit sha: NUL-stripped, trimmed, lowercase,
// 7-64 hex characters.
func cleanPrDescSHA(s string) (string, bool) {
	clean, _ := stripNUL(s)
	clean = strings.ToLower(strings.TrimSpace(clean))
	return clean, prDescHexSHA.MatchString(clean)
}

// cleanPrDescTargetBranch accepts a branch name that is non-empty, bounded and free of control
// and format runes (the renderer interpolates it into the provenance line).
func cleanPrDescTargetBranch(s string) (string, bool) {
	clean := strings.TrimSpace(s)
	if clean == "" || len(clean) > maxPrDescTargetBranchBytes || termsafe.Validate("target_branch", clean) != nil ||
		strings.ContainsAny(clean, " \t\n") {
		return "", false
	}
	return clean, true
}

// validPrDescSize bounds every count; an unavailable size carries all-zero buckets (they are
// never rendered, so a non-zero one is a worker bug).
func validPrDescSize(sz *apitypes.PrDescriptionSize) bool {
	if sz == nil {
		return true
	}
	if sz.Files < 0 || sz.Files > maxPrDescSizeLines {
		return false
	}
	for _, b := range []apitypes.PrDescriptionSizeBucket{sz.Code, sz.Tests, sz.Docs, sz.Config, sz.Generated, sz.Vendored} {
		if b.Added < 0 || b.Deleted < 0 || b.Added > maxPrDescSizeLines || b.Deleted > maxPrDescSizeLines {
			return false
		}
		if sz.Unavailable && (b.Added != 0 || b.Deleted != 0) {
			return false
		}
	}
	return true
}

// checkPrDescRunPR is the mr_iid fence (stage with an mr_iid, and bind): the PR a request names
// must be the run's own. When runs.mr_iid is set (an mr_rework run, or a run whose completion
// recorded its PR) it must equal that; and once the run has a version naming a PR, every later
// version names the same PR.
//
// First bind unverified; pinned thereafter. While runs.mr_iid is NULL (an issue run whose PR
// the worker has just opened), the FIRST stage-with-mr_iid or bind may name any PR number in
// the run's repo: the api cannot check it, because only the worker (holding the forge PAT)
// learns the PR iid from the forge. From then on every stage and bind of the run is pinned to
// that PR.
func checkPrDescRunPR(ctx context.Context, q PrDescQueries, run store.Run, mrIid int64) error {
	if run.MrIid.Valid && run.MrIid.Int64 != mrIid {
		return ErrPrDescriptionVersionConflict
	}
	first, err := q.FirstPrDescriptionMrIidForRun(ctx, run.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	case first != mrIid:
		return ErrPrDescriptionVersionConflict
	}
	return nil
}

// StagePrDescription validates and sanitizes the worker's RAW fields, then stores a pending
// version for the run's snapshot (D9 step 1). The returned version's Fields are the sanitized
// fields: the only text the renderer may publish. An mr_iid, when sent, must be the run's own PR
// (checkPrDescRunPR); a run at a version cap is refused (ErrPrDescriptionTooManyVersions, see
// checkPrDescStageCaps).
//
// The run/claim/worker fence and the caps are checked BEFORE the fields are sanitized, in a short
// transaction of their own (its only write is checkPrDescStageCaps' abandonment of stale unbound
// versions), so a caller that does not hold the run, or a run at its cap, cannot spend the
// sanitizer's CPU; the write transaction then re-checks both under the run row lock.
func (s *Service) StagePrDescription(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.PrDescriptionStageRequest) (apitypes.PrDescriptionVersionDTO, error) {
	if req.ClaimGeneration == nil || !validPrDescSources[req.Source] || !validPrDescSize(req.Size) {
		return apitypes.PrDescriptionVersionDTO{}, ErrPrDescriptionInvalid
	}
	if req.MrIid != nil && *req.MrIid <= 0 {
		return apitypes.PrDescriptionVersionDTO{}, ErrPrDescriptionInvalid
	}
	baseSha, okBase := cleanPrDescSHA(req.BaseSha)
	headSha, okHead := cleanPrDescSHA(req.HeadSha)
	target, okTarget := cleanPrDescTargetBranch(req.TargetBranch)
	if !okBase || !okHead || !okTarget {
		return apitypes.PrDescriptionVersionDTO{}, ErrPrDescriptionInvalid
	}
	// fence is the run/claim/worker check, the mr_iid check and the caps, applied twice: once
	// before sanitizing and again inside the write transaction.
	fence := func(q PrDescQueries) (store.Run, error) {
		run, err := lockPrDescRun(ctx, q, wkr, runID, *req.ClaimGeneration)
		if err != nil {
			return run, err
		}
		if req.MrIid != nil {
			if err := checkPrDescRunPR(ctx, q, run, *req.MrIid); err != nil {
				return run, err
			}
		}
		return run, checkPrDescStageCaps(ctx, q, run)
	}
	if err := s.withPrDescTx(ctx, func(q PrDescQueries) error {
		_, err := fence(q)
		return err
	}); err != nil {
		return apitypes.PrDescriptionVersionDTO{}, err
	}

	raw := req.Fields
	if req.Source == PrDescSourceDeterministic {
		// D8 rung 3: no model or lead text at all, whatever the worker sent. The raw fields are
		// not validated either (an over-cap or malformed one is not a 400): they are discarded.
		raw = apitypes.PrDescriptionFields{}
	}
	fields, err := SanitizePrDescriptionFields(ctx, raw)
	if err != nil {
		return apitypes.PrDescriptionVersionDTO{}, err
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return apitypes.PrDescriptionVersionDTO{}, err
	}
	var sizeJSON []byte
	if req.Size != nil {
		if sizeJSON, err = json.Marshal(req.Size); err != nil {
			return apitypes.PrDescriptionVersionDTO{}, err
		}
	}
	var mr pgtype.Int8
	if req.MrIid != nil {
		mr = pgtype.Int8{Int64: *req.MrIid, Valid: true}
	}

	var out store.PrDescriptionVersion
	err = s.withPrDescTx(ctx, func(q PrDescQueries) error {
		run, err := fence(q)
		if err != nil {
			return err
		}
		out, err = q.InsertPrDescriptionVersion(ctx, store.InsertPrDescriptionVersionParams{
			RunID: run.ID, ClaimGeneration: run.ClaimGeneration, RepoID: uuid.UUID(run.RepoID.Bytes),
			MrIid: mr, Fields: fieldsJSON, Size: sizeJSON,
			BaseSha: baseSha, HeadSha: headSha, TargetBranch: target, Source: req.Source,
		})
		return err
	})
	if err != nil {
		return apitypes.PrDescriptionVersionDTO{}, err
	}
	return prDescVersionDTO(out), nil
}

// checkPrDescStageCaps runs under the run row lock (lockPrDescRun). It first abandons the run's
// UNBOUND pending versions from older claim generations (nothing was written from them, and no
// later flight can bind or ack them); bound ones stay pending for lost-ack recovery. It then
// refuses a stage when the live generation already holds MaxPrDescPendingVersionsPerRun pending
// versions, or the run holds MaxPrDescVersionsPerRun versions in all.
func checkPrDescStageCaps(ctx context.Context, q PrDescQueries, run store.Run) error {
	if _, err := q.AbandonStalePrDescriptionVersionsForRun(ctx, store.AbandonStalePrDescriptionVersionsForRunParams{
		RunID: run.ID, ClaimGeneration: run.ClaimGeneration,
	}); err != nil {
		return err
	}
	pending, err := q.CountPendingPrDescriptionVersionsForRunGeneration(ctx, store.CountPendingPrDescriptionVersionsForRunGenerationParams{
		RunID: run.ID, ClaimGeneration: run.ClaimGeneration,
	})
	if err != nil {
		return err
	}
	if pending >= MaxPrDescPendingVersionsPerRun {
		return ErrPrDescriptionTooManyVersions
	}
	total, err := q.CountPrDescriptionVersionsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if total >= MaxPrDescVersionsPerRun {
		return ErrPrDescriptionTooManyVersions
	}
	return nil
}

// BindPrDescription binds the run's pending version to its PR and records the rendered region
// hash, creating the PR's pr_descriptions row when absent, in one transaction (D9 step 2). It
// returns the bound version and the PR's current state (lock_version, published version).
// Re-binding the same version to the same PR is idempotent (the hash is updated). The PR must
// be the run's own (checkPrDescRunPR).
func (s *Service) BindPrDescription(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.PrDescriptionBindRequest) (apitypes.PrDescriptionBindResponse, error) {
	versionID, err := uuid.Parse(req.VersionID)
	if err != nil || req.ClaimGeneration == nil || req.MrIid <= 0 || !prDescSHA256Hex.MatchString(req.RenderedRegionSha256) {
		return apitypes.PrDescriptionBindResponse{}, ErrPrDescriptionInvalid
	}
	var resp apitypes.PrDescriptionBindResponse
	err = s.withPrDescTx(ctx, func(q PrDescQueries) error {
		run, err := lockPrDescRun(ctx, q, wkr, runID, *req.ClaimGeneration)
		if err != nil {
			return err
		}
		if err := checkPrDescRunPR(ctx, q, run, req.MrIid); err != nil {
			return err
		}
		repoID := uuid.UUID(run.RepoID.Bytes)
		// Lock order: the PR row before the version row (see the file comment).
		if err := q.EnsurePrDescription(ctx, store.EnsurePrDescriptionParams{RepoID: repoID, MrIid: req.MrIid}); err != nil {
			return err
		}
		pr, err := q.GetPrDescriptionForUpdate(ctx, store.GetPrDescriptionForUpdateParams{RepoID: repoID, MrIid: req.MrIid})
		if err != nil {
			return err
		}
		v, err := lockPrDescVersion(ctx, q, run, versionID)
		if err != nil {
			return err
		}
		if v.ClaimGeneration != run.ClaimGeneration {
			return ErrPrDescriptionStaleClaim
		}
		if v.State != prDescStatePending || (v.MrIid.Valid && v.MrIid.Int64 != req.MrIid) {
			return ErrPrDescriptionVersionConflict
		}
		// The recorded hash is immutable: the region it names may already be on the forge with
		// only its ack lost, and lost-ack recovery and human-edit protection both key on it. An
		// identical rebind is an idempotent retry; a different region needs a new staged version.
		if v.RenderedRegionSha256.Valid && v.RenderedRegionSha256.String != req.RenderedRegionSha256 {
			return ErrPrDescriptionVersionConflict
		}
		bound, err := q.BindPrDescriptionVersion(ctx, store.BindPrDescriptionVersionParams{
			ID: v.ID, RunID: run.ID, MrIid: req.MrIid, RenderedRegionSha256: req.RenderedRegionSha256,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPrDescriptionVersionConflict
			}
			return err
		}
		state, err := prDescState(ctx, q, pr)
		if err != nil {
			return err
		}
		resp = apitypes.PrDescriptionBindResponse{Version: prDescVersionDTO(bound), PR: state}
		return nil
	})
	return resp, err
}

// AckPrDescription records the outcome of the forge write for a bound version, compare-and-
// swapped on expected_lock_version (D9 step 3). A published outcome points the PR at the version
// and marks it published; any other outcome records last_outcome only and abandons the version,
// leaving published_version_id untouched. A published outcome needs a BOUND version (mr_iid and
// rendered_region_sha256 set): a version staged with an mr_iid but never bound is
// version_conflict, since nothing records what it wrote. When observed_region_sha256 equals a
// PENDING version's rendered hash for the same PR, that version is acknowledged as published
// first (D9 step 4, lost-ack recovery; see recoverPrDescLostAck for when it is skipped). A retry
// of an already-applied ack returns the current state.
func (s *Service) AckPrDescription(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.PrDescriptionAckRequest) (apitypes.PrDescriptionAckResponse, error) {
	versionID, err := uuid.Parse(req.VersionID)
	if err != nil || req.ClaimGeneration == nil || !validPrDescOutcomes[req.Outcome] || req.ExpectedLockVersion < 0 {
		return apitypes.PrDescriptionAckResponse{}, ErrPrDescriptionInvalid
	}
	if req.ObservedRegionSha256 != nil && !prDescSHA256Hex.MatchString(*req.ObservedRegionSha256) {
		return apitypes.PrDescriptionAckResponse{}, ErrPrDescriptionInvalid
	}
	var resp apitypes.PrDescriptionAckResponse
	err = s.withPrDescTx(ctx, func(q PrDescQueries) error {
		run, err := lockPrDescRun(ctx, q, wkr, runID, *req.ClaimGeneration)
		if err != nil {
			return err
		}
		// Unlocked read for the PR key; the version row is locked after the PR row.
		peek, err := q.GetPrDescriptionVersionByID(ctx, versionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPrDescriptionVersionNotFound
			}
			return err
		}
		if peek.RunID != run.ID {
			return ErrPrDescriptionVersionNotFound
		}
		if !peek.MrIid.Valid {
			return ErrPrDescriptionVersionConflict // no PR named yet: there is nothing to ack against
		}
		repoID, mrIid := uuid.UUID(run.RepoID.Bytes), peek.MrIid.Int64
		pr, err := q.GetPrDescriptionForUpdate(ctx, store.GetPrDescriptionForUpdateParams{RepoID: repoID, MrIid: mrIid})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPrDescriptionVersionConflict
			}
			return err
		}
		v, err := lockPrDescVersion(ctx, q, run, versionID)
		if err != nil {
			return err
		}
		if v.ClaimGeneration != run.ClaimGeneration {
			return ErrPrDescriptionStaleClaim
		}
		if v.State != prDescStatePending {
			if prDescAckAlreadyApplied(v, pr, req) || prDescOwnRecoveryAlreadyApplied(v, pr, req) {
				state, err := prDescState(ctx, q, pr)
				resp = apitypes.PrDescriptionAckResponse{PR: state}
				return err
			}
			return ErrPrDescriptionVersionConflict
		}
		if req.Outcome == PrDescOutcomePublished && !v.RenderedRegionSha256.Valid {
			return ErrPrDescriptionVersionConflict // never bound: no record of what was written
		}
		if pr.LockVersion != req.ExpectedLockVersion {
			return ErrPrDescriptionLockConflict
		}

		if req.ObservedRegionSha256 != nil {
			recovered, err := recoverPrDescLostAck(ctx, q, pr, v, *req.ObservedRegionSha256)
			if err != nil {
				return err
			}
			resp.RecoveredVersionID = recovered
		}

		// The forge already shows THIS version's own region although the worker reports a
		// non-published outcome (e.g. a write that timed out but landed): the record must name
		// what is on the forge, so the version is published instead of abandoned, unless a newer
		// publication supersedes it.
		outcome := req.Outcome
		if outcome != PrDescOutcomePublished && req.ObservedRegionSha256 != nil && v.RenderedRegionSha256.Valid &&
			v.RenderedRegionSha256.String == *req.ObservedRegionSha256 && resp.RecoveredVersionID == nil {
			own, err := prDescOwnRegionOnForge(ctx, q, pr, v)
			if err != nil {
				return err
			}
			if own {
				outcome = PrDescOutcomePublished
				id := v.ID.String()
				resp.RecoveredVersionID = &id
			}
		}

		var rows int64
		if outcome == PrDescOutcomePublished {
			if _, err := q.MarkPrDescriptionVersionPublished(ctx, v.ID); err != nil {
				return err
			}
			rows, err = q.SetPrDescriptionPublished(ctx, store.SetPrDescriptionPublishedParams{
				PublishedVersionID: v.ID, RepoID: repoID, MrIid: mrIid, ExpectedLockVersion: req.ExpectedLockVersion,
			})
		} else {
			if _, err := q.MarkPrDescriptionVersionAbandoned(ctx, v.ID); err != nil {
				return err
			}
			rows, err = q.SetPrDescriptionOutcome(ctx, store.SetPrDescriptionOutcomeParams{
				LastOutcome: req.Outcome, RepoID: repoID, MrIid: mrIid, ExpectedLockVersion: req.ExpectedLockVersion,
			})
		}
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrPrDescriptionLockConflict // unreachable under the row lock; the CAS is the backstop
		}
		after, err := q.GetPrDescriptionForUpdate(ctx, store.GetPrDescriptionForUpdateParams{RepoID: repoID, MrIid: mrIid})
		if err != nil {
			return err
		}
		resp.PR, err = prDescState(ctx, q, after)
		return err
	})
	return resp, err
}

// recoverPrDescLostAck acknowledges, as published, the PENDING version of this PR whose rendered
// region hash equals the one observed on the forge: its write landed but its ack was lost. It
// returns that version's id, or nil when nothing needed recovering. Recovery never moves the PR
// backwards: it is skipped when the currently published version already rendered the observed
// region (the forge shows what the record says), and when the matching pending version was
// created before (or with) the currently published one (an older write whose text a later
// publication superseded, re-observed because a human restored it or two versions rendered the
// same text).
func recoverPrDescLostAck(ctx context.Context, q PrDescQueries, pr store.PrDescription, acking store.PrDescriptionVersion, observed string) (*string, error) {
	var published *store.PrDescriptionVersion
	if pr.PublishedVersionID.Valid {
		pv, err := q.GetPrDescriptionVersionByID(ctx, uuid.UUID(pr.PublishedVersionID.Bytes))
		if err != nil {
			return nil, err
		}
		if pv.RenderedRegionSha256.Valid && pv.RenderedRegionSha256.String == observed {
			return nil, nil
		}
		published = &pv
	}
	m, err := q.FindPrDescriptionVersionByRegionHash(ctx, store.FindPrDescriptionVersionByRegionHashParams{
		RepoID: pr.RepoID, MrIid: pr.MrIid, State: prDescStatePending, RenderedRegionSha256: observed,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if m.ID == acking.ID {
		// The forge already shows the acking version's own region: there is no OTHER version to
		// recover. AckPrDescription publishes the acking version itself, even on a non-published
		// outcome (a write that timed out but landed), so its record matches the forge.
		return nil, nil
	}
	if published != nil && !m.CreatedAt.Time.After(published.CreatedAt.Time) {
		return nil, nil
	}
	if _, err := q.MarkPrDescriptionVersionPublished(ctx, m.ID); err != nil {
		return nil, err
	}
	if _, err := q.RecoverPrDescriptionLostAck(ctx, store.RecoverPrDescriptionLostAckParams{
		PublishedVersionID: m.ID, RepoID: pr.RepoID, MrIid: pr.MrIid,
	}); err != nil {
		return nil, err
	}
	id := m.ID.String()
	return &id, nil
}

// prDescOwnRegionOnForge reports whether the acking version, whose own rendered region the forge
// shows, should be recorded as published: not when the currently published version already
// rendered the same region (the record is already accurate) or is newer than it (a later
// publication superseded this text; the forge showing it again is a human restore or a
// duplicate rendering).
func prDescOwnRegionOnForge(ctx context.Context, q PrDescQueries, pr store.PrDescription, v store.PrDescriptionVersion) (bool, error) {
	if !pr.PublishedVersionID.Valid {
		return true, nil
	}
	pv, err := q.GetPrDescriptionVersionByID(ctx, uuid.UUID(pr.PublishedVersionID.Bytes))
	if err != nil {
		return false, err
	}
	if pv.RenderedRegionSha256.Valid && pv.RenderedRegionSha256.String == v.RenderedRegionSha256.String {
		return false, nil
	}
	return v.CreatedAt.Time.After(pv.CreatedAt.Time), nil
}

// prDescOwnRecoveryAlreadyApplied reports whether a non-published ack is a retry of one that
// published its own version because the forge showed that version's region (see AckPrDescription).
func prDescOwnRecoveryAlreadyApplied(v store.PrDescriptionVersion, pr store.PrDescription, req apitypes.PrDescriptionAckRequest) bool {
	return req.Outcome != PrDescOutcomePublished && req.ObservedRegionSha256 != nil &&
		v.RenderedRegionSha256.Valid && v.RenderedRegionSha256.String == *req.ObservedRegionSha256 &&
		pr.LockVersion == req.ExpectedLockVersion+1 && v.State == prDescStatePublished &&
		pr.PublishedVersionID.Valid && uuid.UUID(pr.PublishedVersionID.Bytes) == v.ID
}

// prDescAckAlreadyApplied reports whether an ack for a non-pending version is a retry of the ack
// that moved it (a lost response): same outcome, and the PR's lock advanced exactly once past
// the caller's expectation.
func prDescAckAlreadyApplied(v store.PrDescriptionVersion, pr store.PrDescription, req apitypes.PrDescriptionAckRequest) bool {
	if pr.LockVersion != req.ExpectedLockVersion+1 {
		return false
	}
	if req.Outcome == PrDescOutcomePublished {
		return v.State == prDescStatePublished && pr.PublishedVersionID.Valid && uuid.UUID(pr.PublishedVersionID.Bytes) == v.ID
	}
	return v.State == "abandoned" && pr.LastOutcome.Valid && pr.LastOutcome.String == req.Outcome
}

func lockPrDescVersion(ctx context.Context, q PrDescQueries, run store.Run, versionID uuid.UUID) (store.PrDescriptionVersion, error) {
	v, err := q.GetPrDescriptionVersionForRunForUpdate(ctx, store.GetPrDescriptionVersionForRunForUpdateParams{ID: versionID, RunID: run.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.PrDescriptionVersion{}, ErrPrDescriptionVersionNotFound
		}
		return store.PrDescriptionVersion{}, err
	}
	return v, nil
}

// LookupPrDescription classifies a region hash the worker read from the forge (the renderer's
// "markers but no published version" case): "published" when a published version of this PR
// rendered it, "pending" when a pending one did (a lost ack; the next ack recovers it), "none"
// for an unknown protected region. It also returns the PR's current state (nil when the PR has
// no pr_descriptions row). Read-only; fenced like the writes.
func (s *Service) LookupPrDescription(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.PrDescriptionLookupRequest) (apitypes.PrDescriptionLookupResponse, error) {
	if req.ClaimGeneration == nil || req.MrIid <= 0 || !prDescSHA256Hex.MatchString(req.RegionSha256) {
		return apitypes.PrDescriptionLookupResponse{}, ErrPrDescriptionInvalid
	}
	reader, ok := s.q.(prDescReader)
	if !ok {
		return apitypes.PrDescriptionLookupResponse{}, errPrDescriptionNoTx
	}
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return apitypes.PrDescriptionLookupResponse{}, err
	}
	if err := checkPrDescRun(run, wkr, *req.ClaimGeneration); err != nil {
		return apitypes.PrDescriptionLookupResponse{}, err
	}
	repoID := uuid.UUID(run.RepoID.Bytes)
	resp := apitypes.PrDescriptionLookupResponse{Match: prDescMatchNone}
	for _, state := range []string{prDescStatePublished, prDescStatePending} {
		m, err := reader.FindPrDescriptionVersionByRegionHash(ctx, store.FindPrDescriptionVersionByRegionHashParams{
			RepoID: repoID, MrIid: req.MrIid, State: state, RenderedRegionSha256: req.RegionSha256,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return apitypes.PrDescriptionLookupResponse{}, err
		}
		id := m.ID.String()
		resp.MatchedVersionID = &id
		resp.Match = prDescMatchPublished
		if state == prDescStatePending {
			resp.Match = prDescMatchPending
		}
		break
	}
	pr, err := reader.GetPrDescription(ctx, store.GetPrDescriptionParams{RepoID: repoID, MrIid: req.MrIid})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return apitypes.PrDescriptionLookupResponse{}, err
	default:
		state, err := prDescState(ctx, reader, pr)
		if err != nil {
			return apitypes.PrDescriptionLookupResponse{}, err
		}
		resp.PR = &state
	}
	return resp, nil
}

// prDescVersionGetter is the one read prDescState needs, satisfied by both the transaction and
// the reader.
type prDescVersionGetter interface {
	GetPrDescriptionVersionByID(ctx context.Context, id uuid.UUID) (store.PrDescriptionVersion, error)
}

func prDescState(ctx context.Context, q prDescVersionGetter, pr store.PrDescription) (apitypes.PrDescriptionState, error) {
	st := apitypes.PrDescriptionState{MrIid: pr.MrIid, LockVersion: pr.LockVersion}
	if pr.LastOutcome.Valid {
		o := pr.LastOutcome.String
		st.LastOutcome = &o
	}
	if pr.PublishedVersionID.Valid {
		v, err := q.GetPrDescriptionVersionByID(ctx, uuid.UUID(pr.PublishedVersionID.Bytes))
		if err != nil {
			return st, err
		}
		dto := prDescVersionDTO(v)
		st.PublishedVersion = &dto
	}
	return st, nil
}

// prDescStateForRun resolves the run's PR and returns its state, or nil when the run has no PR
// with a pr_descriptions row. The PR is runs.mr_iid when set (an mr_rework run, or a run whose
// completion recorded its MR); otherwise the latest version this run bound (a re-claimed issue
// run never records mr_iid while held). A store without the reader surface yields nil.
func (s *Service) prDescStateForRun(ctx context.Context, run store.Run) (*apitypes.PrDescriptionState, error) {
	reader, ok := s.q.(prDescReader)
	if !ok || !run.RepoID.Valid {
		return nil, nil
	}
	mrIid := run.MrIid.Int64
	if !run.MrIid.Valid {
		v, err := reader.LatestBoundPrDescriptionVersionForRun(ctx, run.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		mrIid = v.MrIid.Int64
	}
	pr, err := reader.GetPrDescription(ctx, store.GetPrDescriptionParams{RepoID: uuid.UUID(run.RepoID.Bytes), MrIid: mrIid})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st, err := prDescState(ctx, reader, pr)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// prDescClaimKinds are the PR-producing run kinds whose claim carries pr_description.
var prDescClaimKinds = map[string]bool{
	runkind.Issue: true, runkind.CIFix: true, runkind.SelfImprove: true,
	runkind.Prompt: true, runkind.Task: true, runkind.MRRework: true,
}

// claimPrDescription is the claim's pr_description (D9 step 2): the existing PR's state for a
// run whose PR already exists, nil otherwise. Best-effort: a read error is logged and the field
// omitted, never failing the claim (the worker then stages without a prior version).
func (s *Service) claimPrDescription(ctx context.Context, run store.Run) *apitypes.PrDescriptionState {
	if !prDescClaimKinds[run.Kind] {
		return nil
	}
	st, err := s.prDescStateForRun(ctx, run)
	if err != nil {
		slog.Warn("workersvc: pr description for claim", "run_id", run.ID, "error", err)
		return nil
	}
	return st
}

// RunPrDescription is the GetRun overlay (PRD #1798): the published version of the run's PR and
// the PR's last write outcome. Both nil when the run has no PR record.
func (s *Service) RunPrDescription(ctx context.Context, run store.Run) (*apitypes.RunPrDescriptionDTO, *string, error) {
	st, err := s.prDescStateForRun(ctx, run)
	if err != nil || st == nil {
		return nil, nil, err
	}
	var out *apitypes.RunPrDescriptionDTO
	if v := st.PublishedVersion; v != nil {
		out = &apitypes.RunPrDescriptionDTO{
			MrIid: st.MrIid, Source: v.Source, Fields: v.Fields, Size: v.Size,
			BaseSha: v.BaseSha, HeadSha: v.HeadSha, TargetBranch: v.TargetBranch, PublishedAt: v.PublishedAt,
		}
	}
	return out, st.LastOutcome, nil
}

// prDescVersionDTO maps a stored version to the wire. The stored fields were sanitized on stage;
// a row that fails to decode (never written by this code) degrades to empty fields.
func prDescVersionDTO(v store.PrDescriptionVersion) apitypes.PrDescriptionVersionDTO {
	fields := apitypes.PrDescriptionFields{}
	if err := json.Unmarshal(v.Fields, &fields); err != nil {
		slog.Error("workersvc: decode pr description fields", "version_id", v.ID, "error", err)
		fields = apitypes.PrDescriptionFields{}
	}
	if fields.Changes == nil {
		fields.Changes = []string{}
	}
	if fields.ScopeNotes == nil {
		fields.ScopeNotes = []apitypes.PrDescriptionScopeNote{}
	}
	if fields.ReviewPointers == nil {
		fields.ReviewPointers = []string{}
	}
	if fields.Verification == nil {
		fields.Verification = []apitypes.PrDescriptionVerification{}
	}
	dto := apitypes.PrDescriptionVersionDTO{
		ID: v.ID.String(), RunID: v.RunID.String(), ClaimGeneration: v.ClaimGeneration,
		Fields: fields, BaseSha: v.BaseSha, HeadSha: v.HeadSha, TargetBranch: v.TargetBranch,
		Source: v.Source, State: v.State, CreatedAt: v.CreatedAt.Time,
	}
	if v.MrIid.Valid {
		mr := v.MrIid.Int64
		dto.MrIid = &mr
	}
	if len(v.Size) > 0 {
		var sz apitypes.PrDescriptionSize
		if err := json.Unmarshal(v.Size, &sz); err == nil {
			dto.Size = &sz
		}
	}
	if v.RenderedRegionSha256.Valid {
		h := v.RenderedRegionSha256.String
		dto.RenderedRegionSha256 = &h
	}
	if v.PublishedAt.Valid {
		t := v.PublishedAt.Time
		dto.PublishedAt = &t
	}
	return dto
}
