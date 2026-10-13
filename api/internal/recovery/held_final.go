package recovery

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Step B of held-work publication (issue #2545): the FINAL {kind:"held_publication"} on the
// existing archive-release route. The reasons below are the retained-response vocabulary the
// worker contract pins (fixtures/api-contract/recovery_held_final.*); a retained response is a
// well-formed HTTP 200 that leaves the hold open, never a 400.
const (
	// FinalKindHeldPublication is RecoveryFinalDisposition.Kind of a held publication FINAL.
	FinalKindHeldPublication = "held_publication"

	// HeldReleaseUnsupported: the worker lacks recovery_inventory_v1 or recovery_held_publication_v1,
	// the UZI_HELD_PUBLICATION switch is off, or the retention clock cannot be set.
	HeldReleaseUnsupported = "held_publication_unsupported"
	// HeldReleaseUnavailable: the forge recheck could not be made; retry, nothing is decided.
	HeldReleaseUnavailable = "held_publication_unavailable"
	// HeldReleaseRefMissing: the forge does not advertise the held ref (definitive).
	HeldReleaseRefMissing = "held_publication_ref_missing"
	// HeldReleaseRefMismatch: the held ref is at a tip other than the declared one (definitive).
	HeldReleaseRefMismatch = "held_publication_ref_mismatch"
	// HeldReleaseNotEligible: the hold, run or publication is not in a state a held release may
	// end (cancelled or excluded run, stale generation, hold not open, publication not created), or
	// the run's repo or forge connection is gone so no listing can ever be brokered (definitive).
	HeldReleaseNotEligible = "held_publication_not_eligible"
)

// heldListTimeout bounds the one forge listing of step B (the reconcile listing's bound).
const heldListTimeout = 10 * time.Second

// ErrHeldRemoteGone is what a HeldRemoteFunc returns when the run, its repo or its forge
// connection no longer exists, so no forge call can ever be brokered for it again. The handler
// maps workersvc.ErrHeldRemoteGone onto it; step B then answers held_publication_not_eligible
// (definitive, the worker falls back to the archive) instead of a retry that never ends.
var ErrHeldRemoteGone = errors.New("recovery: held publication: the run, its repo or its forge connection is gone")

// maxHeldLogLen bounds the forge error text a held release logs, in runes.
const maxHeldLogLen = 400

// heldLogText renders a forge listing error for a log line. go-git puts the whole response
// body of the (attacker-controlled) forge in the error, so control, escape, bidi, zero-width and
// combining runes are dropped FIRST, then the text is secret-scrubbed and truncated.
func heldLogText(err error) string {
	return truncateRunes(secretscrub.Scrub(dropUnsafeRunes(err.Error())), maxHeldLogLen)
}

// ansiSeqRe matches CSI and OSC escape sequences.
var ansiSeqRe = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// dropUnsafeRunes DELETES every ANSI sequence and unsafe rune BEFORE secretscrub runs, so a token
// split by a separator is rejoined and then redacted whole (replacing the separator with a space
// would leave the token body behind, unrecognised). Unsafe is what isUnsafeLabelRune rejects plus
// the Unicode format, line and paragraph separator classes (U+2028, U+2029, U+FEFF, U+2060 to
// U+2064 and the rest of Cf), nonspacing marks (Mn: combining marks, variation selectors) and the
// zero-width Hangul fillers. A CSI's final byte goes with its sequence, so a token can lose one
// character; a real (36+ character) token body still scrubs.
func dropUnsafeRunes(s string) string {
	s = ansiSeqRe.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if isUnsafeLabelRune(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Mn) || isHangulFiller(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isHangulFiller reports the Hangul filler code points, which render with zero width.
func isHangulFiller(r rune) bool {
	return r == 0x115f || r == 0x1160 || r == 0x3164 || r == 0xffa0
}

// truncateRunes cuts s to at most max runes and trims surrounding space.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

var heldShaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// HeldRemoteFunc derives the server-side forge connection of runID's repo for the held-ref
// listing. It is injected (workersvc.Service.HeldRemote, adapted by the handler) because
// workersvc's tests import this package; the derivation applies the SSRF gate before the PAT is
// decrypted.
type HeldRemoteFunc func(ctx context.Context, runID uuid.UUID) (pushbroker.ListRefsOptions, error)

// SetHeldListAllow wires the per-worker limiter consulted immediately before the one forge
// listing of step B. A nil allow permits every listing. Call once at construction.
func (s *Service) SetHeldListAllow(allow func(workerID uuid.UUID) bool) { s.heldListAllow = allow }

// SetHeldPublication wires step B: enabled is UZI_HELD_PUBLICATION and remote the forge
// connection derivation. Call once at construction, before the service serves requests. Without
// it every held FINAL on an open hold is retained as held_publication_unsupported.
func (s *Service) SetHeldPublication(enabled bool, remote HeldRemoteFunc) {
	s.heldEnabled, s.heldRemote = enabled, remote
}

// heldRetained is the HTTP 200 answer that leaves the hold open.
func heldRetained(runID uuid.UUID, generation int64, reason string) apitypes.RecoveryReleaseResponse {
	g := generation
	return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Retained: true, Reason: reason, Generation: &g}
}

// heldReceiptMatches reports whether h is released against exactly the declared publication,
// tip and coverage digest.
func heldReceiptMatches(h store.RecoveryCustodyHold, publication uuid.UUID, f *apitypes.RecoveryFinalDisposition) bool {
	return h.State == "released" && h.FinalDisposition.Valid && h.FinalDisposition.String == FinalKindHeldPublication &&
		h.FinalPublicationID.Valid && uuid.UUID(h.FinalPublicationID.Bytes) == publication && !h.FinalCaptureID.Valid &&
		h.FinalSourceSha.Valid && h.FinalSourceSha.String == f.SourceSha &&
		h.FinalCoverageDigest.Valid && h.FinalCoverageDigest.String == f.CoverageDigest
}

// heldSupported reports whether step B may release for a worker with the given capabilities.
func (s *Service) heldSupported(caps []string) bool {
	return s.heldEnabled && slices.Contains(caps, capability.RecoveryInventoryV1) &&
		slices.Contains(caps, capability.RecoveryHeldPublicationV1)
}

// releaseHeldPublication answers a held FINAL. Order is binding:
//
//  1. stateless shape only (ErrBadRequest);
//  2. immutable ownership, then the receipt compare, with no mutable read: an exact replay of a
//     released receipt is answered with the original ACK even after the worker lost its
//     capabilities, the switch went off or the forge is down;
//  3. mutable gates (capabilities, switch), retained when unmet;
//  4. the forge recheck, outside every database lock;
//  5. one transaction in the lock order worker, run, hold, publication, which re-authenticates
//     and re-compares the receipt BEFORE any mutable re-check or definitive refusal.
func (s *Service) releaseHeldPublication(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.RecoveryReleaseRequest) (apitypes.RecoveryReleaseResponse, error) {
	f := req.FinalDisposition
	publication, err := uuid.Parse(f.PublicationID)
	if req.Generation == nil || *req.Generation <= 0 || err != nil || publication.String() != f.PublicationID ||
		!heldShaRe.MatchString(f.SourceSha) || !validCoverageDigest(f.CoverageDigest) || f.CaptureID != "" {
		return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
	}
	generation := *req.Generation
	ack := apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: &generation}
	q := store.New(s.pool)

	hold, err := q.GetHeldPublicationHold(ctx, store.GetHeldPublicationHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if hold.State == "released" && hold.FinalDisposition.String == FinalKindHeldPublication {
		if heldReceiptMatches(hold, publication, f) {
			return ack, nil
		}
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	if hold.State != "open" {
		return heldRetained(runID, generation, HeldReleaseNotEligible), nil
	}

	if !s.heldSupported(wkr.ProtocolCapabilities) {
		return heldRetained(runID, generation, HeldReleaseUnsupported), nil
	}
	// Cheap unlocked pre-check: an ineligible request must not make the API spend the owner's
	// forge quota. A failed pre-check skips the listing but still goes through the locked
	// transaction, whose receipt compare comes first.
	fp := heldForgeProof{}
	eligible, err := s.heldPrecheck(ctx, wkr, runID, generation, hold, publication, f)
	switch {
	case err != nil:
		return apitypes.RecoveryReleaseResponse{}, err
	case !eligible:
		fp.skipped = true
	case s.heldListAllow != nil && !s.heldListAllow(wkr.ID):
		// Consulted only here, after the pre-check: archive and settled FINALs and exact
		// replays never reach it. Over the limit nothing is decided, so the worker retries.
		return heldRetained(runID, generation, HeldReleaseUnavailable), nil
	default:
		fp.refDefect, err = s.heldRefDefect(ctx, runID, generation, f.SourceSha)
		switch {
		case errors.Is(err, ErrHeldRemoteGone):
			fp.gone = true
		case err != nil:
			slog.Warn("held publication release: forge recheck failed", "run", runID, "error", heldLogText(err))
			return heldRetained(runID, generation, HeldReleaseUnavailable), nil
		}
	}
	if s.heldPreLockHook != nil {
		s.heldPreLockHook()
	}
	return s.releaseHeldLocked(ctx, wkr, runID, req, publication, fp)
}

// heldForgeProof is the unlocked phase's verdict handed to the locked transaction.
type heldForgeProof struct {
	refDefect string // definitive ref reason to confirm under the locks, "" when the ref is at tip
	skipped   bool   // the pre-check failed: no listing was made, so there is no proof to release on
	gone      bool   // the forge connection is gone for good
}

// heldStateEligible is the mutable-state predicate shared by the unlocked pre-check and the
// locked re-check.
func heldStateEligible(status string, claimGen int64, runWorker pgtype.UUID, failOrigin pgtype.Text,
	hold store.RecoveryCustodyHold, wkrID uuid.UUID, havePub bool, pub store.RunHeldPublication,
	generation int64, publication uuid.UUID, f *apitypes.RecoveryFinalDisposition) bool {
	return status == "failed" && (!failOrigin.Valid || (failOrigin.String != "push_secret_blocked" && failOrigin.String != "worker_residue_blocked")) &&
		claimGen == generation && runWorker.Valid && uuid.UUID(runWorker.Bytes) == hold.OriginalWorkerID &&
		hold.LiveWorkerID.Valid && uuid.UUID(hold.LiveWorkerID.Bytes) == wkrID &&
		havePub && pub.ID == publication && pub.HoldID == hold.ID && pub.State == "created" &&
		pub.Tip == f.SourceSha && pub.CoverageDigest == f.CoverageDigest
}

// heldPrecheck reads the mutable state without locks. false means the request is ineligible as
// seen now (the locked transaction re-decides).
func (s *Service) heldPrecheck(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64,
	hold store.RecoveryCustodyHold, publication uuid.UUID, f *apitypes.RecoveryFinalDisposition) (bool, error) {
	var (
		status     string
		claimGen   int64
		runWorker  pgtype.UUID
		failOrigin pgtype.Text
	)
	if err := s.pool.QueryRow(ctx, "SELECT status, claim_generation, worker_id, fail_origin FROM runs WHERE id=$1 AND user_id=$2",
		runID, wkr.UserID).Scan(&status, &claimGen, &runWorker, &failOrigin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	pub, err := store.New(s.pool).GetHeldPublicationByRunGeneration(ctx, store.GetHeldPublicationByRunGenerationParams{RunID: runID, Generation: generation})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return heldStateEligible(status, claimGen, runWorker, failOrigin, hold, wkr.ID, err == nil, pub, generation, publication, f), nil
}

// heldRefDefect lists the held ref once and returns "" when it is at tip, or the definitive
// reason the caller must re-confirm under the locks. A listing that cannot be made is an error.
func (s *Service) heldRefDefect(ctx context.Context, runID uuid.UUID, generation int64, tip string) (string, error) {
	if s.heldRemote == nil || s.heldListFn == nil {
		return "", errors.New("held publication: forge listing is not wired")
	}
	lctx, cancel := context.WithTimeout(ctx, heldListTimeout)
	defer cancel()
	opts, err := s.heldRemote(lctx, runID)
	if err != nil {
		return "", err
	}
	ref := pushbroker.HeldRef(runID, generation)
	tips, err := s.heldListFn(lctx, opts, ref)
	if err != nil {
		return "", err
	}
	switch got, ok := tips[ref]; {
	case !ok:
		return HeldReleaseRefMissing, nil
	case got != tip:
		return HeldReleaseRefMismatch, nil
	}
	return "", nil
}

// releaseHeldLocked is step B's transaction. fp is the unlocked phase's verdict; a ref defect
// becomes a refusal only after the locked state agrees with the request, and a release needs a
// forge proof (a skipped pre-check or a mutable state that changed to eligible is retried).
func (s *Service) releaseHeldLocked(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.RecoveryReleaseRequest, publication uuid.UUID, fp heldForgeProof) (apitypes.RecoveryReleaseResponse, error) {
	f := req.FinalDisposition
	generation := *req.Generation
	ack := apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: &generation}
	retained := func(reason string) (apitypes.RecoveryReleaseResponse, error) {
		return heldRetained(runID, generation, reason), nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	var caps []string
	if err := tx.QueryRow(ctx, "SELECT id, protocol_capabilities FROM workers WHERE id=$1 AND user_id=$2 FOR UPDATE", wkr.ID, wkr.UserID).Scan(&id, &caps); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	var (
		status     string
		claimGen   int64
		runWorker  pgtype.UUID
		failOrigin pgtype.Text
	)
	if err := tx.QueryRow(ctx, "SELECT status, claim_generation, worker_id, fail_origin FROM runs WHERE id=$1 AND user_id=$2 FOR UPDATE",
		runID, wkr.UserID).Scan(&status, &claimGen, &runWorker, &failOrigin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	qt := store.New(tx)
	hold, err := qt.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if !hold.InventoryGuarded {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	// Receipt first: a concurrent release that matches is the original ACK, whatever the mutable
	// state has become since.
	if hold.State == "released" && hold.FinalDisposition.String == FinalKindHeldPublication {
		if heldReceiptMatches(hold, publication, f) {
			return ack, nil
		}
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	if hold.State != "open" {
		return retained(HeldReleaseNotEligible)
	}
	pub, err := qt.LockHeldPublicationByRunGeneration(ctx, store.LockHeldPublicationByRunGenerationParams{RunID: runID, Generation: generation})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	havePub := err == nil

	// Mutable re-checks, now under the locks.
	if !s.heldSupported(caps) || s.limits.ReadyRetention < time.Second {
		return retained(HeldReleaseUnsupported)
	}
	if !heldStateEligible(status, claimGen, runWorker, failOrigin, hold, wkr.ID, havePub, pub, generation, publication, f) {
		return retained(HeldReleaseNotEligible)
	}
	if fp.gone {
		return retained(HeldReleaseNotEligible)
	}
	if fp.skipped {
		// Ineligible at the pre-check, eligible now: no forge proof was taken, so do not release.
		return retained(HeldReleaseUnavailable)
	}
	if fp.refDefect != "" {
		return retained(fp.refDefect)
	}

	n, err := qt.ReleaseHeldPublicationHold(ctx, store.ReleaseHeldPublicationHoldParams{
		ID: hold.ID, RunID: runID, UserID: wkr.UserID, Generation: generation, WorkerID: wkr.ID,
		PublicationID: publication, Tip: f.SourceSha, CoverageDigest: f.CoverageDigest,
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if n != 1 {
		return retained(HeldReleaseNotEligible)
	}
	if s.heldPreAckHook != nil {
		s.heldPreAckHook(tx)
	}
	// A hold released without its publication acknowledged would leave the retention clock
	// unset, so a lost acknowledge rolls the release back with it.
	if _, err := qt.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{
		ID: pub.ID, RetentionSeconds: s.limits.ReadyRetention.Seconds(),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, errHeldAcknowledgeLost
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	s.custodySettledAfterCommit(runID)
	return ack, nil
}

// errHeldAcknowledgeLost: the publication acknowledge matched no row after the hold release
// succeeded in the same transaction. The transaction rolls back; the worker retries.
var errHeldAcknowledgeLost = errors.New("recovery: held publication acknowledge matched no row")
