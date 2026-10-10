package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Held-work publication, step A (issue #2545): a failed run's worker uploads one pack and the
// API creates refs/uzi-held/<run-id>/<generation> with the stored connection credential. The
// worker names only the tip, the generation and the coverage digest; the repository, user,
// worker, ref and credential are all derived here from the hold and the run.
//
// The create is sent AT MOST ONCE per (run, generation): a monotonic create_invoked_at marker is
// committed before the network call, and every request that does not win that marker only
// reconciles (lists the ref). An unknown outcome is never terminal on an absent ref.

const (
	// HeldServiceBudget is the one deadline the service work of a step-A request runs under (lock
	// wait, the pack pre-verify, tx1, the push and its read-back, tx2). The handler starts it
	// after the body is read and passes it in the context (see PublishHeld).
	HeldServiceBudget = 150 * time.Second

	// heldPublishMaxConcurrent caps the step-A requests in flight process-wide: each holds an
	// up-to-64 MiB pack and a forge connection (the MaxConcurrentWrites precedent).
	heldPublishMaxConcurrent = 4

	// heldListTimeout bounds one reconcile listing.
	heldListTimeout = 10 * time.Second

	// heldOutcomeWriteTimeout bounds the tx2 write on a context detached from the (possibly
	// expired) service deadline, so a create the push outlived is still recorded.
	heldOutcomeWriteTimeout = 10 * time.Second

	// heldUnknownRetryAfter is when the sweeper first reconciles a create whose outcome is unknown.
	heldUnknownRetryAfter = time.Hour

	// heldLastErrorMax bounds the stored last_error (the column CHECK is 512 characters).
	heldLastErrorMax = 400
)

// Step-A reason codes. The first group is the stored refusal_reason vocabulary (the column's
// CHECK); the second group is wire-only (never stored).
const (
	HeldReasonNotFailed          = "not_failed"
	HeldReasonExcludedOrigin     = "excluded_origin"
	HeldReasonGenerationMismatch = "generation_mismatch"
	HeldReasonIdentityChanged    = "identity_changed"
	HeldReasonCreateRefused      = "create_refused"

	HeldReasonUnsupported      = "unsupported"
	HeldReasonHoldMissing      = "hold_missing"
	HeldReasonForgeUnavailable = "forge_unavailable"
	HeldReasonPackInvalid      = "pack_invalid"
	HeldReasonPackTooLarge     = "pack_too_large"
	HeldReasonTipMissing       = "tip_missing"
)

// HeldRefusal is a step-A refusal that the worker must not retry as is. It carries a reason code
// only; the handler maps the reason to a status.
type HeldRefusal struct{ Reason string }

func (e *HeldRefusal) Error() string { return "held publication refused: " + e.Reason }

// HeldPublishRequest is the worker-controlled part of step A. Nothing else comes from the worker.
type HeldPublishRequest struct {
	Tip        string
	Generation int64
	Coverage   string
}

// HeldPublishResult is the step-A answer. State is the publication's state after the request; a
// non-empty Reason qualifies it (create_refused, or forge_unavailable when a reconcile listing
// could not be made).
type HeldPublishResult struct {
	PublicationID uuid.UUID
	Ref           string
	Tip           string
	State         string
	Reason        string
}

// heldReader is the optional query surface step A needs beyond Store, satisfied by *store.Queries.
type heldReader interface {
	GetHeldPublicationHold(context.Context, store.GetHeldPublicationHoldParams) (store.RecoveryCustodyHold, error)
	GetHeldPublicationByRunGeneration(context.Context, store.GetHeldPublicationByRunGenerationParams) (store.RunHeldPublication, error)
}

// AcquireHeldPublishSlot takes one of the process-wide step-A slots without waiting. The caller
// takes it before the gate and the body read, and calls release when the request is finished.
func (s *Service) AcquireHeldPublishSlot() (release func(), ok bool) {
	select {
	case s.heldSlots <- struct{}{}:
		return func() { <-s.heldSlots }, true
	default:
		return nil, false
	}
}

// heldHoldKinds are the run kinds ClaimRun opens a custody hold for.
var heldHoldKinds = []string{runkind.Issue, runkind.CIFix, runkind.SelfImprove, runkind.Prompt, runkind.Task, runkind.MRRework}

// heldScope is the D1 scope check, shared by the unlocked gate and tx1. hold is the zero value
// when none exists. It returns nil when step A may proceed for this run and hold.
func heldScope(wkr store.Worker, run store.Run, hold store.RecoveryCustodyHold, found bool, req HeldPublishRequest) *HeldRefusal {
	refuse := func(reason string) *HeldRefusal { return &HeldRefusal{Reason: reason} }
	if run.Status != "failed" {
		return refuse(HeldReasonNotFailed)
	}
	if run.FailOrigin.Valid && (run.FailOrigin.String == "push_secret_blocked" || run.FailOrigin.String == "worker_residue_blocked") {
		return refuse(HeldReasonExcludedOrigin)
	}
	if !slices.Contains(heldHoldKinds, run.Kind) {
		return refuse(HeldReasonUnsupported)
	}
	if run.ClaimGeneration != req.Generation {
		return refuse(HeldReasonGenerationMismatch)
	}
	if !found || hold.State != "open" || !hold.InventoryGuarded || !hold.LiveWorkerID.Valid ||
		uuid.UUID(hold.LiveWorkerID.Bytes) != wkr.ID || hold.Generation != req.Generation {
		return refuse(HeldReasonHoldMissing)
	}
	if run.WorkerID != pgconv.UUID(hold.OriginalWorkerID) {
		return refuse(HeldReasonGenerationMismatch)
	}
	if !hold.RepoID.Valid || run.RepoID != hold.RepoID || hold.UserID != wkr.UserID || run.UserID != wkr.UserID {
		return refuse(HeldReasonIdentityChanged)
	}
	return nil
}

// heldIdentityMatches reports whether an existing publication row is the one this request
// names. The row's identity is immutable, so a mismatch is a different publication (or a worker
// that changed its mind) and never overwrites it.
func heldIdentityMatches(pub store.RunHeldPublication, hold store.RecoveryCustodyHold, wkr store.Worker, runID uuid.UUID, req HeldPublishRequest) bool {
	return pub.RunID == runID && pub.Generation == req.Generation && pub.HoldID == hold.ID &&
		pub.UserID == wkr.UserID && pub.WorkerID == wkr.ID && hold.RepoID.Valid &&
		pub.RepoID == uuid.UUID(hold.RepoID.Bytes) && pub.Ref == pushbroker.HeldRef(runID, req.Generation) &&
		pub.Tip == req.Tip && pub.CoverageDigest == req.Coverage
}

func heldResult(pub store.RunHeldPublication) HeldPublishResult {
	res := HeldPublishResult{PublicationID: pub.ID, Ref: pub.Ref, Tip: pub.Tip, State: pub.State}
	if pub.State == "refused" && pub.RefusalReason.Valid {
		res.Reason = pub.RefusalReason.String
	}
	return res
}

type heldAdmission struct {
	run  store.Run
	hold store.RecoveryCustodyHold
	pub  *store.RunHeldPublication
}

// heldAdmit is the unlocked pre-body admission: no forge call and no write. It checks, in
// order, that the run is owned by this worker (ErrRunNotOwned), the switch and the worker's
// capability, the D1 scope, and then loads the publication row if one exists.
func (s *Service) heldAdmit(ctx context.Context, wkr store.Worker, runID uuid.UUID, req HeldPublishRequest) (heldAdmission, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return heldAdmission{}, err
	}
	if !s.p.HeldPublication || !slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryHeldPublicationV1) {
		return heldAdmission{}, &HeldRefusal{Reason: HeldReasonUnsupported}
	}
	rd, ok := s.q.(heldReader)
	if !ok {
		return heldAdmission{}, errors.New("held publication: queries are not wired")
	}
	hold, err := rd.GetHeldPublicationHold(ctx, store.GetHeldPublicationHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation,
	})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return heldAdmission{}, fmt.Errorf("held publication hold: %w", err)
	}
	if refusal := heldScope(wkr, run, hold, found, req); refusal != nil {
		return heldAdmission{}, refusal
	}
	adm := heldAdmission{run: run, hold: hold}
	pub, err := rd.GetHeldPublicationByRunGeneration(ctx, store.GetHeldPublicationByRunGenerationParams{RunID: runID, Generation: req.Generation})
	switch {
	case err == nil:
		adm.pub = &pub
	case !errors.Is(err, pgx.ErrNoRows):
		return heldAdmission{}, fmt.Errorf("held publication row: %w", err)
	}
	return adm, nil
}

// HeldPublishGate is the pre-body gate. It reads no body and makes no forge call unless the
// publication's create was already invoked, in which case the answer is reconcile-only and the
// returned result is non-nil: the caller returns it without reading the body. A nil result with
// a nil error means the request may upload its pack.
func (s *Service) HeldPublishGate(ctx context.Context, wkr store.Worker, runID uuid.UUID, req HeldPublishRequest) (*HeldPublishResult, error) {
	adm, err := s.heldAdmit(ctx, wkr, runID, req)
	if err != nil {
		return nil, err
	}
	if adm.pub == nil {
		return nil, nil
	}
	if !heldIdentityMatches(*adm.pub, adm.hold, wkr, runID, req) {
		return nil, &HeldRefusal{Reason: HeldReasonIdentityChanged}
	}
	if !adm.pub.CreateInvokedAt.Valid {
		return nil, nil
	}
	res := s.heldReconcile(ctx, *adm.pub)
	return &res, nil
}

// heldReconcile reports the publication's state after listing the ref, and records created when
// the ref is at the tip. It never sends a create, never deletes, and never turns an absent ref
// into a terminal state. A listing it cannot make leaves the state as it is, with a retryable
// reason.
func (s *Service) heldReconcile(ctx context.Context, pub store.RunHeldPublication) HeldPublishResult {
	res := heldResult(pub)
	if pub.State != "invoked" && pub.State != "create_unknown" {
		return res
	}
	lctx, cancel := context.WithTimeout(ctx, heldListTimeout)
	defer cancel()
	f, err := s.heldRemote(lctx, pub.RunID)
	if err != nil {
		res.Reason = HeldReasonForgeUnavailable
		return res
	}
	tips, err := s.heldListFn(lctx, f.ListOptions(), pub.Ref)
	if err != nil {
		res.Reason = HeldReasonForgeUnavailable
		return res
	}
	if tips[pub.Ref] != pub.Tip {
		return res
	}
	rec, err := s.heldRecordOutcome(ctx, pub.ID, "created", "", "", time.Time{})
	if err != nil {
		slog.Warn("held publication: recording a reconciled create failed", "run", pub.RunID, "error", secretscrub.Scrub(err.Error()))
		return res
	}
	return heldResult(rec)
}

// heldRecordOutcome is tx2: a CAS on state in (invoked, create_unknown) to the given outcome,
// on a context detached from ctx's cancellation and bounded by heldOutcomeWriteTimeout. When the
// CAS finds no row (another request already recorded an outcome) the current row is returned.
func (s *Service) heldRecordOutcome(ctx context.Context, id uuid.UUID, state, refusal, lastErr string, next time.Time) (store.RunHeldPublication, error) {
	if s.txBeginner == nil {
		return store.RunHeldPublication{}, errors.New("held publication: transactions are not wired")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), heldOutcomeWriteTimeout)
	defer cancel()
	tx, err := s.txBeginner.Begin(wctx)
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	defer func() { _ = tx.Rollback(wctx) }()
	qt := store.New(tx)
	pub, err := qt.RecordHeldCreateOutcome(wctx, store.RecordHeldCreateOutcomeParams{
		ID: id, State: state,
		RefusalReason: pgtype.Text{String: refusal, Valid: refusal != ""},
		LastError:     pgtype.Text{String: lastErr, Valid: lastErr != ""},
		NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: !next.IsZero()},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := qt.GetHeldPublication(wctx, id)
		if gerr != nil {
			return store.RunHeldPublication{}, gerr
		}
		return cur, nil
	}
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	if s.heldOutcomeHook != nil {
		if err := s.heldOutcomeHook(); err != nil {
			return store.RunHeldPublication{}, err
		}
	}
	if err := tx.Commit(wctx); err != nil {
		return store.RunHeldPublication{}, err
	}
	return pub, nil
}

// PublishHeld is step A after the body was read. ctx must carry the ONE step-A service deadline
// (HeldServiceBudget, started by the handler after the body read): it covers the pack
// pre-verify, tx1, the push and its read-back. The outcome write (tx2) runs on a detached
// context so a create that outlived ctx is still recorded.
func (s *Service) PublishHeld(ctx context.Context, wkr store.Worker, runID uuid.UUID, req HeldPublishRequest, pack []byte) (HeldPublishResult, error) {
	adm, err := s.heldAdmit(ctx, wkr, runID, req)
	if err != nil {
		return HeldPublishResult{}, err
	}
	if adm.pub != nil {
		if !heldIdentityMatches(*adm.pub, adm.hold, wkr, runID, req) {
			return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonIdentityChanged}
		}
		if adm.pub.CreateInvokedAt.Valid {
			return s.heldReconcile(ctx, *adm.pub), nil
		}
	}

	// Pack pre-verify, read-only. Every forge call goes through heldRemote (SSRF gate first).
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonForgeUnavailable}
		}
		return HeldPublishResult{}, fmt.Errorf("held publication claim context: %s", secretscrub.Scrub(err.Error()))
	}
	forgeConn, err := s.heldRemote(ctx, runID)
	if err != nil {
		slog.Warn("held publication: forge connection unavailable", "run", runID, "error", secretscrub.Scrub(err.Error()))
		return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonForgeUnavailable}
	}
	branch, _ := checkpointBranch(adm.run.Kind, runID, adm.run.IssueIid)
	opts := pushbroker.HeldPackOptions{
		RunID: runID, Generation: req.Generation, Branch: branch, DefaultBranch: rc.DefaultBranch.String,
		Tip: req.Tip, Pack: pack,
	}
	forgeConn.ApplyTo(&opts)
	prepared, err := s.heldPrepareFn(ctx, opts)
	if err != nil {
		switch {
		case errors.Is(err, pushbroker.ErrPackTooLarge):
			return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonPackTooLarge}
		case errors.Is(err, pushbroker.ErrPackInvalid), errors.Is(err, pushbroker.ErrInvalidRef):
			return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonPackInvalid}
		case errors.Is(err, pushbroker.ErrTipMissing):
			return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonTipMissing}
		}
		slog.Warn("held publication: pack pre-verify failed", "run", runID, "error", sanitizeHeldText(err.Error()))
		return HeldPublishResult{}, &HeldRefusal{Reason: HeldReasonForgeUnavailable}
	}

	pub, winner, err := s.heldInvoke(ctx, wkr, runID, req)
	if err != nil {
		return HeldPublishResult{}, err
	}
	if !winner {
		return s.heldReconcile(ctx, pub), nil
	}

	// The marker is committed; nothing below can be undone by a rollback.
	outcome, sendErr := s.heldSendFn(ctx, prepared)
	var (
		state, refusal, lastErr string
		next                    time.Time
	)
	switch outcome {
	case pushbroker.HeldCreateCreated:
		state = "created"
	case pushbroker.HeldCreateRefused:
		state, refusal = "refused", HeldReasonCreateRefused
	default:
		state, next = "create_unknown", s.now().Add(heldUnknownRetryAfter)
	}
	if sendErr != nil {
		lastErr = sanitizeHeldText(sendErr.Error())
	}
	rec, err := s.heldRecordOutcome(ctx, pub.ID, state, refusal, lastErr, next)
	if err != nil {
		// The row stays invoked: reconcile-only from here on, and never another send.
		slog.Warn("held publication: recording the create outcome failed", "run", runID, "outcome", outcome.String(), "error", secretscrub.Scrub(err.Error()))
		return heldResult(pub), nil
	}
	return heldResult(rec), nil
}

// heldInvoke is tx1: with the worker, run, hold and publication locked in that order it
// re-checks D1, inserts or loads the publication, refuses a different identity, and decides the
// single winner of the create by the rows MarkHeldCreateInvoked affects. The marker is committed
// before it returns. winner is false for every request that did not set the marker.
func (s *Service) heldInvoke(ctx context.Context, wkr store.Worker, runID uuid.UUID, req HeldPublishRequest) (pub store.RunHeldPublication, winner bool, err error) {
	if s.txBeginner == nil {
		return pub, false, errors.New("held publication: transactions are not wired")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return pub, false, fmt.Errorf("held publication begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	qt := store.New(tx)
	if _, err = qt.GetWorkerForUpdate(ctx, wkr.ID); err != nil {
		return pub, false, fmt.Errorf("held publication worker lock: %w", err)
	}
	run, err := qt.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return pub, false, ErrRunNotOwned
	}
	if err != nil {
		return pub, false, fmt.Errorf("held publication run lock: %w", err)
	}
	hold, err := qt.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation,
	})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pub, false, fmt.Errorf("held publication hold lock: %w", err)
	}
	if refusal := heldScope(wkr, run, hold, found, req); refusal != nil {
		return pub, false, refusal
	}
	// Identity comes from the hold and the run, never from the worker.
	pub, err = qt.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
		RunID: runID, Generation: req.Generation, HoldID: hold.ID, UserID: wkr.UserID,
		RepoID: uuid.UUID(hold.RepoID.Bytes), WorkerID: wkr.ID,
		Ref: pushbroker.HeldRef(runID, req.Generation), Tip: req.Tip, CoverageDigest: req.Coverage,
	})
	if err != nil {
		return pub, false, fmt.Errorf("held publication upsert: %w", err)
	}
	if !heldIdentityMatches(pub, hold, wkr, runID, req) {
		return pub, false, &HeldRefusal{Reason: HeldReasonIdentityChanged}
	}
	if !pub.CreateInvokedAt.Valid {
		marked, merr := qt.MarkHeldCreateInvoked(ctx, pub.ID)
		switch {
		case merr == nil:
			pub, winner = marked, true
		case !errors.Is(merr, pgx.ErrNoRows):
			return pub, false, fmt.Errorf("held publication marker: %w", merr)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return pub, false, fmt.Errorf("held publication commit: %w", err)
	}
	return pub, winner, nil
}

var (
	heldANSIRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-_])")
)

// sanitizeHeldText makes untrusted text (a remote's ng reason, a transport error) safe to store
// and to show: ANSI escape sequences, C0/C1 controls (newlines included), bidi and other format
// characters and line separators are removed, a recognised secret family is scrubbed, runs of
// whitespace collapse to one space, and the result is bounded. Stripping runs both before and
// after the scrub so a control character cannot split a token past the scrubber.
func sanitizeHeldText(s string) string {
	strip := func(in string) string {
		in = heldANSIRe.ReplaceAllString(in, " ")
		var b strings.Builder
		for _, r := range in {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == unicode.ReplacementChar {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r)
		}
		return strings.Join(strings.Fields(b.String()), " ")
	}
	out := strip(secretscrub.Scrub(strip(s)))
	if r := []rune(out); len(r) > heldLastErrorMax {
		out = string(r[:heldLastErrorMax])
	}
	return out
}
