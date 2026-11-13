package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Terminal preservation (issue #4007), the worker route's service: the worker of a terminal run
// uploads one pack and the API creates or fast-forwards refs/uzi-terminal/<run-id> with the stored
// connection credential. It follows held-work publication's step A (held_publish.go): a marker is
// committed BEFORE the network call and only the request that wins the marker sends. A loser and a
// request whose commit is ambiguous never send; a retry reconciles (lists the ref) when the marker
// is durable, and otherwise may win afresh and send once. No forge call runs inside a
// transaction.
//
// Lock order wherever more than one is taken: worker, run, hold, preservation, contribution.

const (
	// terminalListTimeout bounds one reconcile listing and terminalLifetimeTimeout one lifetime read.
	terminalListTimeout     = 10 * time.Second
	terminalLifetimeTimeout = 10 * time.Second

	// terminalOutcomeWriteTimeout bounds tx2 on a context detached from the (possibly expired)
	// service deadline, so a push the request outlived is still recorded.
	terminalOutcomeWriteTimeout = 10 * time.Second

	// terminalUnknownRetryAfter is when the sweep first reconciles an outcome that is unknown, and
	// terminalLiveRecheck when it next looks at a live row.
	terminalUnknownRetryAfter = time.Hour
	terminalLiveRecheck       = time.Hour

	// terminalMaxManifestBytes mirrors the 00437 manifest CHECK (octet_length(manifest::text)).
	terminalMaxManifestBytes = 262144
)

var (
	terminalIDRe  = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,128}$`)
	terminalOIDRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Contribution dispositions, the run_terminal_contributions.disposition vocabulary.
const (
	TerminalDispositionPreserved   = "preserved"
	TerminalDispositionClosedIssue = "closed_issue_abandoned"
	TerminalDispositionClosedPR    = "closed_pr_abandoned"
)

// The authority classes, the run_terminal_contributions.authority vocabulary, and the lifetime
// classes, the run_terminal_preservations.lifetime_at_bind vocabulary.
const (
	terminalAuthorityCurrentFlight = "current_flight"
	terminalAuthorityGuardedHold   = "open_guarded_hold"

	terminalLifetimeIssue = "issue"
	terminalLifetimePR    = "pr"
	terminalLifetimeNoPR  = "no_pr"
)

// The preservation and contribution states the service branches on.
const (
	terminalStateBound              = "bound"
	terminalStatePublishing         = "publishing"
	terminalStateNotPublished       = "not_published"
	terminalContributionPrepared    = "prepared"
	terminalContributionInvoked     = "invoked"
	terminalContributionUnknown     = "publish_unknown"
	terminalContributionRefusedName = "refused"
)

// TerminalRefusal is a refusal the worker must not retry as is. It carries a reason code from the
// apitypes.TerminalReason* vocabulary only; the handler maps the reason to a status.
type TerminalRefusal struct{ Reason string }

func (e *TerminalRefusal) Error() string { return "terminal preservation refused: " + e.Reason }

func terminalRefuse(reason string) *TerminalRefusal { return &TerminalRefusal{Reason: reason} }

// TerminalPublishRequest is the worker-controlled part of the route. Nothing else comes from the
// worker: the repository, user, ref, credential and the ci-config guard are all server-derived.
type TerminalPublishRequest struct {
	Generation int64
	Tip        string
	PriorTip   string
	Manifest   apitypes.TerminalManifest
	Pack       []byte
}

// TerminalPublishResult is the answer: the contribution's state after the request, and the
// preservation's. A non-empty Reason qualifies it (a refusal reason, or forge_unavailable when a
// reconcile listing could not be made).
type TerminalPublishResult struct {
	PreservationID    uuid.UUID
	ContributionID    uuid.UUID
	Ref               string
	Tip               string
	State             string
	PreservationState string
	Disposition       string
	Reason            string
}

// terminalReader is the optional query surface the route needs beyond Store, satisfied by
// *store.Queries.
type terminalReader interface {
	GetTerminalPreservationByRun(context.Context, uuid.UUID) (store.RunTerminalPreservation, error)
	GetTerminalContributionByRunGeneration(context.Context, store.GetTerminalContributionByRunGenerationParams) (store.RunTerminalContribution, error)
	GetHeldPublicationHold(context.Context, store.GetHeldPublicationHoldParams) (store.RecoveryCustodyHold, error)
	HasOpenGuardedHoldForWorker(context.Context, store.HasOpenGuardedHoldForWorkerParams) (bool, error)
}

// terminalHooks are the route's broker seams and fault-injection points. The zero value uses the
// pushbroker functions; a test sets only the fields it needs.
type terminalHooks struct {
	prepare      func(context.Context, pushbroker.TerminalPackOptions) (*pushbroker.PreparedTerminalPack, error)
	sendCreate   func(context.Context, *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error)
	sendUpdate   func(context.Context, *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error)
	list         func(context.Context, pushbroker.ListRefsOptions, uuid.UUID) (string, bool, error)
	beforeCommit func() error      // inside tx1, after the writes and before its COMMIT
	afterCommit  func(error) error // given tx1's winner COMMIT result; may turn success into an error (a lost ACK)
	beforeBind   func()            // after the unlocked pre-checks, before tx1 begins
	beforeSend   func()            // after tx1 committed, before the push
	outcome      func() error      // inside tx2, before its COMMIT

	// The sweep's retire lane (terminal_sweep.go). retireEnabled is set only by tests: production
	// retirement is gated by the terminalRetirementEnabled constant.
	del           func(context.Context, pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error)
	retireEnabled bool
	afterObserve  func(observedAt *time.Time) // after the forge observation, before tx-R1
	afterDelete   func() error                // after the remote delete, before tx-R2; an error ends the item there
	retireMaxAge  time.Duration               // replaces retireObservationMaxAge when non-zero
}

func (h terminalHooks) prepareFn() func(context.Context, pushbroker.TerminalPackOptions) (*pushbroker.PreparedTerminalPack, error) {
	if h.prepare != nil {
		return h.prepare
	}
	return pushbroker.PrepareTerminalPack
}

func (h terminalHooks) sendCreateFn() func(context.Context, *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error) {
	if h.sendCreate != nil {
		return h.sendCreate
	}
	return pushbroker.SendTerminalCreate
}

func (h terminalHooks) sendUpdateFn() func(context.Context, *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error) {
	if h.sendUpdate != nil {
		return h.sendUpdate
	}
	return pushbroker.SendTerminalUpdate
}

func (h terminalHooks) listFn() func(context.Context, pushbroker.ListRefsOptions, uuid.UUID) (string, bool, error) {
	if h.list != nil {
		return h.list
	}
	return pushbroker.ListTerminalTip
}

func (h terminalHooks) deleteFn() func(context.Context, pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error) {
	if h.del != nil {
		return h.del
	}
	return pushbroker.DeleteTerminal
}

// TerminalLifetimeForges is the forge surface the lifetime classification reads through: the issue
// side comes from IssueTrackerForRepo and the pull request side from CodeHostForRepo, exactly the
// repo-side split *forgesvc.Service implements. workersvc cannot import forgesvc (a cycle), so the
// service type-asserts the ForgeBuilder it was given (SetForges) to this interface.
type TerminalLifetimeForges interface {
	IssueTrackerForRepo(ctx context.Context, repoID uuid.UUID, forgeType, baseURL string, tokenCiphertext []byte) (forge.IssueTracker, error)
	CodeHostForRepo(ctx context.Context, repoID uuid.UUID, forgeType, baseURL string, tokenCiphertext []byte) (forge.CodeHost, error)
}

// TerminalLifetime is a run's lifetime classification (issue #4007): which forge object bounds how
// long its terminal ref is worth keeping, and whether that object is PROVEN closed.
type TerminalLifetime struct {
	// Class is issue, pr or no_pr, the run_terminal_preservations.lifetime_at_bind vocabulary.
	Class string
	// ClosedProven is true only when the forge answered and said the governing object is closed.
	ClosedProven bool
	// Disposition is preserved, or the abandonment disposition a proven closure records.
	Disposition string
	// RepoID, IssueIid and MrIid are the association the classification was made from, as found on
	// the run passed to terminalLifetime. A decision that rests on this classification is only valid
	// while the locked run still has the same association (see matchesAssociation).
	RepoID   pgtype.UUID
	IssueIid pgtype.Int8
	MrIid    pgtype.Int8
}

// matchesAssociation reports whether run still has the repo, issue and merge request the
// classification observed.
func (l TerminalLifetime) matchesAssociation(run store.Run) bool {
	return l.RepoID == run.RepoID && l.IssueIid == run.IssueIid && l.MrIid == run.MrIid
}

// terminalLifetime classifies run's lifetime. It is the ONE classification used at bind and, in the
// sweep, at retire:
//
//   - issue_iid set: only the issue counts (its pull request, if any, does not). It is proven
//     closed when the issue tracker returns State "closed" for a real issue (not a pull request
//     answered by a shared issue endpoint).
//   - issue-less with mr_iid: only the merge request counts, proven closed when merged or closed.
//   - otherwise no_pr: it has no closure signal and never abandons; the retire lane retires it only
//     by age, terminalNoPRRetention after its first publication.
//
// A lookup error, a missing binding or forge wiring, and any other state are NOT proven: the work is
// preserved. The result records the association it classified from, so the caller can refuse a
// decision once the locked run no longer matches it. It makes forge calls, so no transaction may be
// open around it.
func (s *Service) terminalLifetime(ctx context.Context, run store.Run) TerminalLifetime {
	l := TerminalLifetime{
		Class: terminalLifetimeNoPR, Disposition: TerminalDispositionPreserved,
		RepoID: run.RepoID, IssueIid: run.IssueIid, MrIid: run.MrIid,
	}
	switch {
	case run.IssueIid.Valid:
		l.Class = terminalLifetimeIssue
	case run.MrIid.Valid:
		l.Class = terminalLifetimePR
	default:
		return l
	}
	fg, ok := s.forges.(TerminalLifetimeForges)
	if !ok || fg == nil || !run.RepoID.Valid {
		return l
	}
	rd, ok := s.q.(completedPublicationReader)
	if !ok {
		return l
	}
	lctx, cancel := context.WithTimeout(ctx, terminalLifetimeTimeout)
	defer cancel()
	repoID := uuid.UUID(run.RepoID.Bytes)
	binding, err := rd.GetCompletedPublicationBinding(lctx, store.GetCompletedPublicationBindingParams{RepoID: repoID, UserID: run.UserID})
	if err != nil {
		return l
	}
	switch l.Class {
	case terminalLifetimeIssue:
		tracker, err := fg.IssueTrackerForRepo(lctx, repoID, binding.ForgeType, binding.BaseUrl, binding.TokenCiphertext)
		if err != nil || tracker == nil {
			return l
		}
		issue, err := tracker.GetIssue(lctx, binding.ProjectID, run.IssueIid.Int64)
		if err == nil && issue.State == "closed" && !issue.IsPullRequest {
			l.ClosedProven, l.Disposition = true, TerminalDispositionClosedIssue
		}
	case terminalLifetimePR:
		host, err := fg.CodeHostForRepo(lctx, repoID, binding.ForgeType, binding.BaseUrl, binding.TokenCiphertext)
		if err != nil || host == nil {
			return l
		}
		sum, err := host.GetMergeRequestSummary(lctx, binding.ProjectID, run.MrIid.Int64)
		if err == nil && (sum.State == forge.MRStateMerged || sum.State == forge.MRStateClosed) {
			l.ClosedProven, l.Disposition = true, TerminalDispositionClosedPR
		}
	}
	return l
}

// terminalCapable reports whether the worker reported the terminal-preservation protocol feature.
func terminalCapable(w store.Worker) bool {
	return slices.Contains(capability.FilterProtocol(w.ProtocolCapabilities), capability.RecoveryTerminalPreservationV1)
}

// TerminalPreservationAdmit is the pre-body check: it reads no body, makes no forge call and writes
// nothing. A worker that did not report the capability is refused unsupported.
func (s *Service) TerminalPreservationAdmit(wkr store.Worker) error {
	if !terminalCapable(wkr) {
		return terminalRefuse(apitypes.TerminalReasonUnsupported)
	}
	return nil
}

// TerminalPreservationGate is the pre-body authorization: it reads no body, makes no forge call and
// writes nothing. The capability is required, the run must belong to the worker's user and lane, and
// the worker must hold a stake in it that needs no generation: the run is currently claimed or
// running on this worker, or an open inventory-guarded hold of the run has this worker as both
// original and live worker. The worker's own run that is not an eligible flight (claim released,
// terminal status, or a refused kind or egress profile) and has no such hold gets the typed refusal
// (stale_claim or unsupported). Anything else, including a same-user run of another worker, is
// ErrRunNotOwned, so a capable worker cannot occupy the shared upload slots with runs that are not
// its own and nothing leaks. The generation lives in the body, so the generation-bound predicates
// are applied by PublishTerminal once the body is read.
func (s *Service) TerminalPreservationGate(ctx context.Context, wkr store.Worker, runID uuid.UUID) error {
	if err := s.TerminalPreservationAdmit(wkr); err != nil {
		return err
	}
	tr, ok := s.q.(terminalReader)
	if !ok {
		return errors.New("terminal preservation: queries are not wired")
	}
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunNotOwned
	}
	if err != nil {
		return fmt.Errorf("terminal preservation run: %w", err)
	}
	if (run.Kind == "cross_check" && run.ClaimGeneration == 0) || laneMismatch(runID, run.EgressProfileID.Valid, wkr) {
		return ErrRunNotOwned
	}
	own := run.WorkerID == pgconv.UUID(wkr.ID)
	if own && !run.ClaimReleasedAt.Valid && (run.Status == "claimed" || run.Status == "running") {
		if terminalFlightKindRefused(run) {
			return terminalRefuse(apitypes.TerminalReasonUnsupported)
		}
		return nil
	}
	held, err := tr.HasOpenGuardedHoldForWorker(ctx, store.HasOpenGuardedHoldForWorkerParams{RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID})
	if err != nil {
		return fmt.Errorf("terminal preservation hold: %w", err)
	}
	if held {
		return nil
	}
	if !own {
		return ErrRunNotOwned
	}
	// The worker's own run, with no hold: the typed refusals terminalAuthority gives a request
	// that carries a generation. A released claim is stale (the worker stops silently); a status
	// the flight does not allow is unsupported.
	if run.ClaimReleasedAt.Valid {
		return ErrStaleClaim
	}
	return terminalRefuse(apitypes.TerminalReasonUnsupported)
}

// canonicalTerminalManifest validates m against the authenticated run, generation and worker,
// canonicalises it (sorted slices, fixed field order, no credentials) and returns the canonical
// JSON, its sha256 digest and the roots the pack verification must prove.
func canonicalTerminalManifest(m apitypes.TerminalManifest, runID uuid.UUID, workerID uuid.UUID, generation int64) (canon []byte, digest string, roots []pushbroker.TerminalRoot, refusal *TerminalRefusal) {
	invalid := terminalRefuse(apitypes.TerminalReasonManifestInvalid)
	if m.RunID != runID.String() || m.Generation != generation || m.WorkerID != workerID.String() {
		return nil, "", nil, terminalRefuse(apitypes.TerminalReasonManifestMismatch)
	}
	if len(m.Roots) > apitypes.TerminalManifestMaxRoots || len(m.Abandoned) > apitypes.TerminalManifestMaxAbandoned {
		return nil, "", nil, invalid
	}
	out := apitypes.TerminalManifest{
		RunID: m.RunID, Generation: m.Generation, WorkerID: m.WorkerID,
		Roots:     append([]apitypes.TerminalManifestRoot{}, m.Roots...),
		Abandoned: append([]apitypes.TerminalManifestAbandoned{}, m.Abandoned...),
	}
	for _, r := range out.Roots {
		if !terminalIDRe.MatchString(r.SourceID) || !terminalIDRe.MatchString(r.AttemptID) ||
			!terminalOIDRe.MatchString(r.Commit) || !terminalOIDRe.MatchString(r.Tree) {
			return nil, "", nil, invalid
		}
	}
	for _, a := range out.Abandoned {
		if (a.Class != apitypes.TerminalAbandonedIgnored && a.Class != apitypes.TerminalAbandonedColdDirty) ||
			!terminalIDRe.MatchString(a.SourceID) || a.Extents.Files < 0 || a.Extents.Bytes < 0 {
			return nil, "", nil, invalid
		}
	}
	slices.SortFunc(out.Roots, func(a, b apitypes.TerminalManifestRoot) int {
		return strings.Compare(a.SourceID+"\x00"+a.AttemptID+"\x00"+a.Commit, b.SourceID+"\x00"+b.AttemptID+"\x00"+b.Commit)
	})
	slices.SortFunc(out.Abandoned, func(a, b apitypes.TerminalManifestAbandoned) int {
		return strings.Compare(a.Class+"\x00"+a.SourceID, b.Class+"\x00"+b.SourceID)
	})
	for i := 1; i < len(out.Roots); i++ {
		if out.Roots[i] == out.Roots[i-1] {
			return nil, "", nil, invalid
		}
	}
	for i := 1; i < len(out.Abandoned); i++ {
		if out.Abandoned[i].Class == out.Abandoned[i-1].Class && out.Abandoned[i].SourceID == out.Abandoned[i-1].SourceID {
			return nil, "", nil, invalid
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, "", nil, invalid
	}
	canon = bytes.TrimRight(buf.Bytes(), "\n")
	if len(canon) > terminalMaxManifestBytes {
		return nil, "", nil, invalid
	}
	sum := sha256.Sum256(canon)
	for _, r := range out.Roots {
		roots = append(roots, pushbroker.TerminalRoot{Commit: r.Commit, Tree: r.Tree})
	}
	return canon, hex.EncodeToString(sum[:]), roots, nil
}

// terminalFlightEligible applies the current_flight predicates to the run row read under the run
// lock (or, unlocked, to the pre-check read): a generation is required, it must be the run's
// current claim generation, the claim must not be released, and the run must be claimed or running.
// An error is the refusal for a run that IS this worker's but fails a predicate.
func terminalFlightEligible(wkr store.Worker, run store.Run, generation int64) (bool, error) {
	if generation <= 0 {
		return false, ErrMissingClaimGeneration
	}
	if run.WorkerID != pgconv.UUID(wkr.ID) {
		return false, nil
	}
	if generation != run.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return false, ErrStaleClaim
	}
	if run.Status != "claimed" && run.Status != "running" {
		return false, terminalRefuse(apitypes.TerminalReasonUnsupported)
	}
	if terminalFlightKindRefused(run) {
		return false, terminalRefuse(apitypes.TerminalReasonUnsupported)
	}
	return true, nil
}

// terminalFlightKindRefused is the current_flight scope: the run kinds held publication supports
// (heldHoldKinds), and never a run bound to an egress profile. Both are unsupported.
func terminalFlightKindRefused(run store.Run) bool {
	return !slices.Contains(heldHoldKinds, run.Kind) || run.EgressProfileID.Valid
}

// terminalHoldScope is the open_guarded_hold predicate set, the held V2 predicates of heldScope: a
// terminal run (failed, cancelled or completed) of a kind that opens custody holds, whose claim
// generation has reached the hold's, with an exact open inventory-guarded hold of this run and
// generation whose original and live worker is the caller and whose run, repo and owner match. A
// missing or non-matching hold is checked first (hold_missing); a run that is not terminal, or is
// of another kind, is then unsupported (the current_flight status refusal), and a generation the
// run never reached has no hold.
func terminalHoldScope(wkr store.Worker, run store.Run, hold store.RecoveryCustodyHold, found bool, generation int64) *TerminalRefusal {
	if !found || hold.State != "open" || !hold.InventoryGuarded || !hold.LiveWorkerID.Valid ||
		uuid.UUID(hold.LiveWorkerID.Bytes) != wkr.ID || hold.OriginalWorkerID != wkr.ID ||
		hold.RunID != run.ID || hold.LiveRunID != pgconv.UUID(run.ID) || hold.Generation != generation {
		return terminalRefuse(apitypes.TerminalReasonHoldMissing)
	}
	if run.Status != "failed" && run.Status != "cancelled" && run.Status != "completed" {
		return terminalRefuse(apitypes.TerminalReasonUnsupported)
	}
	if !slices.Contains(heldHoldKinds, run.Kind) {
		return terminalRefuse(apitypes.TerminalReasonUnsupported)
	}
	if generation <= 0 || run.ClaimGeneration < generation {
		return terminalRefuse(apitypes.TerminalReasonHoldMissing)
	}
	if !hold.RepoID.Valid || run.RepoID != hold.RepoID || hold.UserID != wkr.UserID || run.UserID != wkr.UserID {
		return terminalRefuse(apitypes.TerminalReasonIdentityChanged)
	}
	return nil
}

// terminalExcludedOrigin refuses the failures whose work must never be published.
func terminalExcludedOrigin(run store.Run) *TerminalRefusal {
	if run.FailOrigin.Valid && (run.FailOrigin.String == "push_secret_blocked" || run.FailOrigin.String == "worker_residue_blocked") {
		return terminalRefuse(apitypes.TerminalReasonExcludedOrigin)
	}
	return nil
}

// terminalAuthority decides the authority class from the SERVER state (never the payload) and
// applies that class's predicates. current_flight wins when the run is this worker's live flight at
// exactly this generation; otherwise an exact open guarded hold gives open_guarded_hold. When neither
// holds, the reason is: stale_claim for a worker's run whose claim is not this generation or was
// released (the worker stops silently); unsupported for the worker's run in a status the flight
// does not allow and with no hold at all; otherwise the hold's own refusal (hold_missing,
// identity_changed or unsupported). getHold supplies the hold read: unlocked for the pre-check, the
// locking GetFinalInventoryHold inside tx1.
func terminalAuthority(wkr store.Worker, run store.Run, generation int64, getHold func() (store.RecoveryCustodyHold, bool, error)) (string, store.RecoveryCustodyHold, error) {
	if refusal := terminalExcludedOrigin(run); refusal != nil {
		return "", store.RecoveryCustodyHold{}, refusal
	}
	eligible, flightErr := terminalFlightEligible(wkr, run, generation)
	if eligible {
		return terminalAuthorityCurrentFlight, store.RecoveryCustodyHold{}, nil
	}
	if errors.Is(flightErr, ErrMissingClaimGeneration) {
		return "", store.RecoveryCustodyHold{}, flightErr
	}
	hold, found, err := getHold()
	if err != nil {
		return "", store.RecoveryCustodyHold{}, err
	}
	holdRefusal := terminalHoldScope(wkr, run, hold, found, generation)
	if holdRefusal == nil {
		return terminalAuthorityGuardedHold, hold, nil
	}
	// Reason taxonomy when neither class applies. A claim that is not the current one is stale_claim
	// (the worker stops silently); this covers a failed or cancelled run that still names the worker
	// with its claim released, whatever its holds. A run whose worker_id was cleared reaches the hold
	// refusal instead. A completed run still holding the worker's claim fails the status
	// predicate and is unsupported when it has no hold at all. A terminal run that has a hold which does
	// not qualify is reported by the hold (hold_missing, identity_changed or unsupported).
	var statusRefusal *TerminalRefusal
	if flightErr != nil && (!errors.As(flightErr, &statusRefusal) || !found) {
		return "", store.RecoveryCustodyHold{}, flightErr
	}
	return "", store.RecoveryCustodyHold{}, holdRefusal
}

func terminalIdentityMatches(c store.RunTerminalContribution, wkr store.Worker, runID uuid.UUID, req TerminalPublishRequest, digest string) bool {
	return c.RunID == runID && c.Generation == req.Generation && c.WorkerID == wkr.ID && c.UserID == wkr.UserID &&
		c.Tip == req.Tip && c.PriorTip.Valid == (req.PriorTip != "") && c.PriorTip.String == req.PriorTip &&
		c.CoverageDigest == digest
}

func terminalResult(p store.RunTerminalPreservation, c store.RunTerminalContribution) TerminalPublishResult {
	res := TerminalPublishResult{
		PreservationID: p.ID, ContributionID: c.ID, Ref: p.Ref, Tip: c.Tip, State: c.State,
		PreservationState: p.State, Disposition: c.Disposition,
	}
	if c.State == terminalContributionRefusedName && c.RefusalReason.Valid {
		res.Reason = c.RefusalReason.String
	}
	return res
}

// PublishTerminal is the route after the body was read. ctx must carry the one service deadline
// (the handler starts it after the body read). The sequence, with no forge I/O inside a transaction:
// authority and identity pre-checks, the lifetime read, the forge connection, the read-only pack
// preparation, tx1 (bind and, for the one winner, the invoke marker), the single send, tx2.
func (s *Service) PublishTerminal(ctx context.Context, wkr store.Worker, runID uuid.UUID, req TerminalPublishRequest) (TerminalPublishResult, error) {
	if err := s.TerminalPreservationAdmit(wkr); err != nil {
		return TerminalPublishResult{}, err
	}
	// The generation is required; checked before the manifest so a missing one is not reported as a
	// manifest mismatch.
	if req.Generation <= 0 {
		return TerminalPublishResult{}, ErrMissingClaimGeneration
	}
	canon, digest, roots, refusal := canonicalTerminalManifest(req.Manifest, runID, wkr.ID, req.Generation)
	if refusal != nil {
		return TerminalPublishResult{}, refusal
	}
	if !terminalOIDRe.MatchString(req.Tip) || (req.PriorTip != "" && !terminalOIDRe.MatchString(req.PriorTip)) {
		return TerminalPublishResult{}, terminalRefuse(apitypes.TerminalReasonPackInvalid)
	}
	tr, ok := s.q.(terminalReader)
	if !ok || s.txBeginner == nil {
		return TerminalPublishResult{}, errors.New("terminal preservation: queries or transactions are not wired")
	}

	// Pre-checks, unlocked: no write, no forge call. They keep an unauthorized request from reaching
	// the forge; tx1 repeats every one under the locks.
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalPublishResult{}, ErrRunNotOwned
	}
	if err != nil {
		return TerminalPublishResult{}, fmt.Errorf("terminal preservation run: %w", err)
	}
	if (run.Kind == "cross_check" && run.ClaimGeneration == 0) || laneMismatch(runID, run.EgressProfileID.Valid, wkr) {
		return TerminalPublishResult{}, ErrRunNotOwned
	}
	preAuthority, _, err := terminalAuthority(wkr, run, req.Generation, func() (store.RecoveryCustodyHold, bool, error) {
		h, err := tr.GetHeldPublicationHold(ctx, store.GetHeldPublicationHoldParams{
			RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return h, false, nil
		}
		return h, err == nil, err
	})
	if err != nil {
		return TerminalPublishResult{}, err
	}

	// The preservation is read BEFORE the contribution. A winner commits both rows in one
	// transaction, so a preservation read as publishing guarantees the contribution read after it is
	// visible; the opposite order could see no contribution and an already-publishing preservation
	// and refuse an identical retry that should reconcile.
	pres, perr := tr.GetTerminalPreservationByRun(ctx, runID)
	if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
		return TerminalPublishResult{}, fmt.Errorf("terminal preservation row: %w", perr)
	}
	existing, err := tr.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: runID, Generation: req.Generation})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return TerminalPublishResult{}, fmt.Errorf("terminal preservation contribution: %w", err)
	}
	if found {
		if !terminalIdentityMatches(existing, wkr, runID, req, digest) {
			return TerminalPublishResult{}, terminalRefuse(apitypes.TerminalReasonGenerationBound)
		}
		if existing.State != terminalContributionPrepared {
			return s.terminalReconcile(ctx, runID, req.Generation)
		}
	}
	// A retired preservation is final and refused here. A publication in flight and a prior tip that
	// differs from the preservation's are only snapshots when this generation has no contribution
	// yet: a concurrent identical request may be about to create it, so tx1 decides them under the
	// locks.
	if perr == nil {
		if refusal := terminalPreservationRefusal(pres, found); refusal != nil && (found || refusal.Reason != apitypes.TerminalReasonPublicationInFlight) {
			return TerminalPublishResult{}, refusal
		}
		// A prior tip that differs from a settled (not publishing) preservation's cannot become right
		// under the locks, so it is refused here, before any forge call. Only the in-flight
		// publishing case is left to tx1, where a winner's commit may still move the tip.
		if pres.State != terminalStatePublishing {
			want := req.PriorTip
			if found {
				want = existing.PriorTip.String
			}
			if want != pres.Tip.String {
				return TerminalPublishResult{}, terminalRefuse(apitypes.TerminalReasonPriorTipMismatch)
			}
		}
	}

	// The lifetime read, then the pack preparation, both before any transaction. An existing
	// prepared contribution keeps its own (preserved) disposition.
	lt := TerminalLifetime{
		Class: terminalLifetimeNoPR, Disposition: TerminalDispositionPreserved,
		RepoID: run.RepoID, IssueIid: run.IssueIid, MrIid: run.MrIid,
	}
	if !found {
		lt = s.terminalLifetime(ctx, run)
	}
	var (
		prepared *pushbroker.PreparedTerminalPack
		pat      string
	)
	if !lt.ClosedProven {
		prepared, pat, err = s.terminalPrepare(ctx, run, runID, req, roots)
		if err != nil {
			return TerminalPublishResult{}, err
		}
	}

	if s.terminal.beforeBind != nil {
		s.terminal.beforeBind()
	}
	bound, err := s.terminalBind(ctx, wkr, run, preAuthority, req, digest, canon, lt)
	if err != nil {
		return TerminalPublishResult{}, err
	}
	if !bound.winner {
		if bound.con.State == terminalContributionInvoked || bound.con.State == terminalContributionUnknown {
			return s.terminalReconcile(ctx, runID, req.Generation)
		}
		return terminalResult(bound.pres, bound.con), nil
	}

	// The marker is committed; nothing below can be undone by a rollback.
	if s.terminal.beforeSend != nil {
		s.terminal.beforeSend()
	}
	// A local decision to skip both send functions is a proven no-send, not an ambiguous
	// remote outcome. Persist it as a refusal; a crash before that write still reconciles.
	suppressed := ""
	if bound.authority == terminalAuthorityGuardedHold {
		// A settlement observed here suppresses the send. A failed read also fails closed.
		hold, herr := tr.GetHeldPublicationHold(ctx, store.GetHeldPublicationHoldParams{
			RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation,
		})
		if herr != nil || !bound.con.HoldID.Valid || hold.ID != uuid.UUID(bound.con.HoldID.Bytes) || terminalHoldScope(wkr, bound.run, hold, true, req.Generation) != nil {
			suppressed = "send suppressed: the guarded hold settled after the invoke marker"
		}
	}
	if suppressed == "" && prepared == nil {
		suppressed = "send suppressed: no prepared pack"
	}
	var (
		outcome pushbroker.TerminalOutcome
		sendErr error
	)
	switch {
	case suppressed != "":
		outcome, sendErr = pushbroker.TerminalUnknown, errors.New(suppressed)
	case req.PriorTip == "":
		outcome, sendErr = s.terminal.sendCreateFn()(ctx, prepared)
	default:
		outcome, sendErr = s.terminal.sendUpdateFn()(ctx, prepared)
	}
	rec := terminalOutcomeRecord{outcome: outcome, notSent: suppressed != ""}
	if sendErr != nil {
		// The remote's ng text is untrusted: delete unsafe runes, redact the connection PAT and any
		// recognised secret family, collapse whitespace and bound it before it reaches a log or a row.
		rec.lastErr = heldRedact(sendErr.Error(), pat)
	}
	pres2, con2, err := s.terminalRecordOutcome(ctx, runID, req.Generation, rec)
	if err != nil {
		// The row stays invoked: reconcile-only from here on, and never another send.
		slog.Warn("terminal preservation: recording the outcome failed", "run", runID, "outcome", outcome.String(), "error", secretscrub.Scrub(err.Error()))
		return terminalResult(bound.pres, bound.con), nil
	}
	return terminalResult(pres2, con2), nil
}

// terminalPreservationRefusal refuses a retired preservation, and a publication already in flight
// for another generation. existing is true when the request's own contribution already exists.
func terminalPreservationRefusal(p store.RunTerminalPreservation, existing bool) *TerminalRefusal {
	switch p.State {
	case "retiring", "deleted", "abandoned":
		return terminalRefuse(apitypes.TerminalReasonPreservationRetired)
	case terminalStatePublishing:
		if !existing {
			return terminalRefuse(apitypes.TerminalReasonPublicationInFlight)
		}
	}
	return nil
}

// terminalPrepare derives the forge connection and the pack options SERVER-SIDE (the ci-config
// guard and its paths come from the run row, never the payload) and runs the read-only
// preparation. A refusal here happens before any row is written. pat is returned only so the caller
// can redact it from the send error.
func (s *Service) terminalPrepare(ctx context.Context, run store.Run, runID uuid.UUID, req TerminalPublishRequest, roots []pushbroker.TerminalRoot) (*pushbroker.PreparedTerminalPack, string, error) {
	coordinates, err := publicationCoordinates(run)
	if err != nil {
		return nil, "", terminalRefuse(apitypes.TerminalReasonIdentityChanged)
	}
	var workBranch, checkpointKey string
	if coordinates != nil {
		workBranch, checkpointKey = coordinates.WorkBranch, coordinates.CheckpointKey
	}
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", terminalRefuse(apitypes.TerminalReasonForgeUnavailable)
		}
		return nil, "", fmt.Errorf("terminal preservation claim context: %s", secretscrub.Scrub(err.Error()))
	}
	conn, err := s.heldRemote(ctx, runID)
	if err != nil {
		slog.Warn("terminal preservation: forge connection unavailable", "run", runID, "error", secretscrub.Scrub(err.Error()))
		return nil, "", terminalRefuse(apitypes.TerminalReasonForgeUnavailable)
	}
	opts := pushbroker.TerminalPackOptions{
		CloneURL: conn.CloneURL, Username: conn.Username, PAT: conn.PAT,
		RunID: runID, WorkBranch: workBranch, CheckpointKey: checkpointKey, DefaultBranch: rc.DefaultBranch.String,
		Tip: req.Tip, PriorTip: req.PriorTip, Pack: req.Pack,
		CIConfigGuard: run.CiConfigGuard, CIConfigPaths: run.CiConfigPaths, Roots: roots,
	}
	prepared, err := s.terminal.prepareFn()(ctx, opts)
	if err != nil {
		if r := terminalPrepareRefusal(err); r != nil {
			return nil, "", r
		}
		slog.Warn("terminal preservation: pack pre-verify failed", "run", runID, "error", heldRedact(err.Error(), conn.PAT))
		return nil, "", terminalRefuse(apitypes.TerminalReasonForgeUnavailable)
	}
	return prepared, conn.PAT, nil
}

// terminalPrepareRefusal maps a PrepareTerminalPack error to its wire reason, nil for an error that
// is not a typed refusal (a forge or transport failure, retryable).
func terminalPrepareRefusal(err error) *TerminalRefusal {
	switch {
	case errors.Is(err, pushbroker.ErrPackTooLarge):
		return terminalRefuse(apitypes.TerminalReasonPackTooLarge)
	case errors.Is(err, pushbroker.ErrPackInvalid), errors.Is(err, pushbroker.ErrInvalidRef):
		return terminalRefuse(apitypes.TerminalReasonPackInvalid)
	case errors.Is(err, pushbroker.ErrTipMissing):
		return terminalRefuse(apitypes.TerminalReasonTipMissing)
	case errors.Is(err, pushbroker.ErrCIConfigGuard):
		return terminalRefuse(apitypes.TerminalReasonCIConfigGuard)
	case errors.Is(err, pushbroker.ErrNotDescendant):
		return terminalRefuse(apitypes.TerminalReasonNotDescendant)
	case errors.Is(err, pushbroker.ErrTerminalRootUnreachable):
		return terminalRefuse(apitypes.TerminalReasonRootUnreachable)
	case errors.Is(err, pushbroker.ErrTerminalTreeMismatch):
		return terminalRefuse(apitypes.TerminalReasonTreeMismatch)
	case errors.Is(err, pushbroker.ErrTerminalWalkTooLarge):
		return terminalRefuse(apitypes.TerminalReasonWalkTooLarge)
	case errors.Is(err, pushbroker.ErrTerminalPriorMoved):
		return terminalRefuse(apitypes.TerminalReasonForeignTip)
	}
	return nil
}

type terminalBinding struct {
	pres      store.RunTerminalPreservation
	con       store.RunTerminalContribution
	authority string
	run       store.Run // the run row read under the lock
	winner    bool
}

// terminalWriteMiss reports a state writer that changed no row. The caller is inside its
// transaction, holding the run lock, so the re-read is authoritative: the row the writer targets
// (the contribution of generation when onContribution, else the preservation) still in the state the
// writer expected means a guard refused the change (an illegal transition); anything else is a lost
// race. Either way the caller rolls back.
func terminalWriteMiss(ctx context.Context, qt *store.Queries, what string, runID uuid.UUID, generation int64, onContribution bool, expectedState string) error {
	var (
		cur  string
		rerr error
	)
	if onContribution {
		var c store.RunTerminalContribution
		c, rerr = qt.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: runID, Generation: generation})
		cur = c.State
	} else {
		var p store.RunTerminalPreservation
		p, rerr = qt.GetTerminalPreservationByRun(ctx, runID)
		cur = p.State
	}
	switch {
	case rerr == nil && cur == expectedState:
		slog.Error("terminal preservation: illegal transition", "run", runID, "writer", what, "state", cur)
	case rerr == nil:
		slog.Warn("terminal preservation: lost race", "run", runID, "writer", what, "expected", expectedState, "state", cur)
	default:
		slog.Warn("terminal preservation: lost race, row unreadable", "run", runID, "writer", what)
	}
	return fmt.Errorf("terminal preservation %s: no row updated", what)
}

// terminalSchedule runs the scheduling write and treats zero rows as a failure: the caller rolls
// back rather than commit a row the sweep would never look at.
func terminalSchedule(ctx context.Context, qt *store.Queries, arg store.ScheduleTerminalPreservationCheckParams) error {
	n, err := qt.ScheduleTerminalPreservationCheck(ctx, arg)
	if err != nil {
		return fmt.Errorf("terminal preservation schedule: %w", err)
	}
	if n == 0 {
		return errors.New("terminal preservation schedule: no row updated")
	}
	return nil
}

// terminalBind is tx1. With the worker, run, hold and preservation locked in that order it
// re-checks the authority, loads or inserts the preservation (refusing a retired one) and the
// contribution (refusing a different payload for the same generation), and decides the single
// winner of the push by the rows its two conditional UPDATEs affect: the preservation goes to
// publishing with its live pointer FIRST (the contribution guard needs it), then the contribution
// goes prepared -> invoked. A proven-closed lifetime instead records the abandonment and sends
// nothing. winner is true only if both rows changed AND the commit succeeded. A request whose COMMIT result
// is ambiguous returns an error and never sends; a retry then reconciles if the marker is durable,
// or may win afresh.
func (s *Service) terminalBind(ctx context.Context, wkr store.Worker, run store.Run, preAuthority string, req TerminalPublishRequest, digest string, canon []byte, lt TerminalLifetime) (b terminalBinding, err error) {
	runID := run.ID
	var (
		tx           pgx.Tx
		qt           *store.Queries
		lockedWorker store.Worker
	)
	if preAuthority == terminalAuthorityCurrentFlight {
		// A live flight: the claim-mutation locks (worker, then run), whose reconciliation fence
		// applies to a live claim. The explicit current_flight predicates are applied again below.
		// An open_guarded_hold publishes for a terminal run whose claim is already released, which
		// that fence would refuse, so it takes the plain locks below, as held publication's step B.
		g := req.Generation
		var locked store.Run
		_, locked, tx, err = s.beginClaimMutation(ctx, wkr, runID, &g)
		if errors.Is(err, ErrRunNotOwned) || errors.Is(err, ErrStaleClaim) {
			// The pre-check found the run on this worker as a live flight, so the claim moved since, or
			// the reconciliation fence (validateReconciledGeneration, when a run_milestone_reconciliations
			// row exists) now refuses it: the class changed. The retry is classified afresh (for example
			// onto open_guarded_hold).
			return b, terminalRefuse(apitypes.TerminalReasonAuthorityChanged)
		}
		if err != nil {
			return b, err
		}
		if tx == nil {
			return b, errors.New("terminal preservation: transactions are not wired")
		}
		run = locked
		qt = store.New(tx)
		// beginClaimMutation already holds the worker lock; this re-read returns the locked row.
		if lockedWorker, err = qt.GetWorkerForUpdate(ctx, wkr.ID); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return b, fmt.Errorf("terminal preservation worker lock: %w", err)
		}
	} else {
		tx, err = s.beginReadCommitted(ctx)
		if err != nil {
			return b, fmt.Errorf("terminal preservation begin: %w", err)
		}
		qt = store.New(tx)
		if lockedWorker, err = qt.GetWorkerForUpdate(ctx, wkr.ID); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return b, fmt.Errorf("terminal preservation worker lock: %w", err)
		}
		run, err = qt.GetRunByIDForUpdate(ctx, runID)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return b, ErrRunNotOwned
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return b, fmt.Errorf("terminal preservation run lock: %w", err)
		}
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if lockedWorker.UserID != wkr.UserID {
		return b, ErrRunNotOwned
	}
	wkr = lockedWorker
	if refusal := s.TerminalPreservationAdmit(wkr); refusal != nil {
		return b, refusal
	}
	if run.UserID != wkr.UserID || (run.Kind == "cross_check" && run.ClaimGeneration == 0) || laneMismatch(runID, run.EgressProfileID.Valid, wkr) {
		return b, ErrRunNotOwned
	}
	authority, hold, err := terminalAuthority(wkr, run, req.Generation, func() (store.RecoveryCustodyHold, bool, error) {
		h, err := qt.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{
			RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return h, false, nil
		}
		return h, err == nil, err
	})
	if err != nil {
		return b, err
	}
	b.run = run
	if authority != preAuthority {
		// The run's state moved between the unlocked pre-check and the locks (for example running to
		// completed with a guarded hold): the class the lock path was chosen for no longer holds. This is
		// the retryable authority_changed 409, not stale_claim (which tells the worker to stop silently):
		// the worker retries and the next request is classified afresh from the pre-check.
		return b, terminalRefuse(apitypes.TerminalReasonAuthorityChanged)
	}
	// The lifetime was observed on the unlocked run read. A run whose repo, issue or merge request
	// moved since is a different association: refuse retryably before any write or send, on every
	// path, so no preserve or abandonment decision rests on an association the run no longer has.
	if !lt.matchesAssociation(run) {
		return b, terminalRefuse(apitypes.TerminalReasonAuthorityChanged)
	}
	if !run.RepoID.Valid {
		return b, terminalRefuse(apitypes.TerminalReasonIdentityChanged)
	}
	repoID := uuid.UUID(run.RepoID.Bytes)
	b.authority = authority

	pres, err := qt.InsertTerminalPreservation(ctx, store.InsertTerminalPreservationParams{
		RunID: runID, UserID: wkr.UserID, RepoID: repoID, Ref: pushbroker.TerminalRef(runID), LifetimeAtBind: lt.Class,
	})
	if err != nil {
		return b, fmt.Errorf("terminal preservation upsert: %w", err)
	}
	if pres.UserID != wkr.UserID || pres.RepoID != repoID || pres.Ref != pushbroker.TerminalRef(runID) {
		return b, terminalRefuse(apitypes.TerminalReasonIdentityChanged)
	}
	con, err := qt.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: runID, Generation: req.Generation})
	existing := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return b, fmt.Errorf("terminal preservation contribution: %w", err)
	}
	if refusal := terminalPreservationRefusal(pres, existing); refusal != nil {
		return b, refusal
	}
	if existing {
		if !terminalIdentityMatches(con, wkr, runID, req, digest) {
			return b, terminalRefuse(apitypes.TerminalReasonGenerationBound)
		}
		if con.State != terminalContributionPrepared {
			// Decided by an earlier request: nothing to write, the caller reconciles.
			b.pres, b.con = pres, con
			return b, nil
		}
		if con.PriorTip.String != pres.Tip.String {
			return b, terminalRefuse(apitypes.TerminalReasonPriorTipMismatch)
		}
	} else {
		if pres.Tip.String != req.PriorTip {
			return b, terminalRefuse(apitypes.TerminalReasonPriorTipMismatch)
		}
		params := store.InsertTerminalContributionParams{
			PreservationID: pres.ID, RunID: runID, Generation: req.Generation, WorkerID: wkr.ID, UserID: wkr.UserID,
			Authority: authority, Tip: req.Tip, CoverageDigest: digest, Manifest: canon, Disposition: lt.Disposition,
		}
		if authority == terminalAuthorityGuardedHold {
			params.HoldID = pgtype.UUID{Bytes: hold.ID, Valid: true}
		}
		if req.PriorTip != "" {
			params.PriorTip = pgtype.Text{String: req.PriorTip, Valid: true}
		}
		con, err = qt.InsertTerminalContribution(ctx, params)
		if err != nil {
			return b, fmt.Errorf("terminal preservation contribution insert: %w", err)
		}
	}

	if con.Disposition != TerminalDispositionPreserved {
		// A proven-closed lifetime: the abandonment is recorded and nothing is sent. A preservation
		// that never had a ref is finished; one that may own a ref stays due so the sweep retires it.
		switch {
		case pres.State == terminalStateBound && !pres.Tip.Valid:
			p2, merr := qt.MarkTerminalPreservationNotPublished(ctx, pres.ID)
			if merr != nil {
				if errors.Is(merr, pgx.ErrNoRows) {
					return b, terminalWriteMiss(ctx, qt, "mark not_published", runID, req.Generation, false, pres.State)
				}
				return b, fmt.Errorf("terminal preservation not_published: %w", merr)
			}
			pres = p2
		case pres.State != terminalStateNotPublished:
			if serr := terminalSchedule(ctx, qt, store.ScheduleTerminalPreservationCheckParams{
				ID: pres.ID, NextCheckAt: pgtype.Timestamptz{Time: s.now(), Valid: true},
			}); serr != nil {
				return b, serr
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return b, fmt.Errorf("terminal preservation commit: %w", err)
		}
		b.pres, b.con = pres, con
		return b, nil
	}

	// The winner decision. Preservation FIRST: the contribution guard refuses prepared -> invoked
	// unless the preservation is publishing.
	pres2, err := qt.MarkTerminalPreservationPublishing(ctx, store.MarkTerminalPreservationPublishingParams{
		ID: pres.ID, ExpectedState: pres.State, ExpectedTip: pres.Tip,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return b, terminalWriteMiss(ctx, qt, "mark publishing", runID, req.Generation, false, pres.State)
	}
	if err != nil {
		return b, fmt.Errorf("terminal preservation publishing: %w", err)
	}
	con2, err := qt.MarkTerminalContributionInvoked(ctx, con.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, terminalWriteMiss(ctx, qt, "mark invoked", runID, req.Generation, true, terminalContributionPrepared)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_run_terminal_contributions_unresolved" {
		// The one-unresolved-push-per-preservation index: another generation's push is unresolved.
		// Any other unique violation is an internal error, handled below.
		return b, terminalRefuse(apitypes.TerminalReasonPublicationInFlight)
	}
	if err != nil {
		return b, fmt.Errorf("terminal preservation marker: %w", err)
	}
	if s.terminal.beforeCommit != nil {
		if err = s.terminal.beforeCommit(); err != nil {
			return b, err
		}
	}
	err = tx.Commit(ctx)
	if s.terminal.afterCommit != nil {
		err = s.terminal.afterCommit(err)
	}
	if err != nil {
		// The COMMIT result is unknown: the marker may or may not be durable. This request is NOT the
		// winner and never sends; a retry reads the row and either reconciles or wins afresh.
		return b, fmt.Errorf("terminal preservation commit: %w", err)
	}
	b.pres, b.con, b.winner = pres2, con2, true
	return b, nil
}

// terminalOutcomeRecord carries a send result or this request's definitive local no-send.
// notSent is never inferred from a remote error or reconstructed after a crash.
type terminalOutcomeRecord struct {
	outcome pushbroker.TerminalOutcome
	notSent bool
	lastErr string
}

// terminalRecordOutcome is tx2, on a context detached from ctx's cancellation and bounded by
// terminalOutcomeWriteTimeout. Under the run lock, then the preservation lock, every writer is a
// CAS whose zero rows rolls the whole transaction back:
//
//	applied  contribution published FIRST, then the preservation live at the new tip
//	refused  contribution refused with its reason, then the preservation back to live (it had a
//	         tip) or not_published (it had none)
//	unknown  contribution publish_unknown; the preservation stays publishing and is rechecked later
//
// When another request already decided the contribution, the current rows are returned unchanged.
func (s *Service) terminalRecordOutcome(ctx context.Context, runID uuid.UUID, generation int64, rec terminalOutcomeRecord) (store.RunTerminalPreservation, store.RunTerminalContribution, error) {
	var zeroP store.RunTerminalPreservation
	var zeroC store.RunTerminalContribution
	if s.txBeginner == nil {
		return zeroP, zeroC, errors.New("terminal preservation: transactions are not wired")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalOutcomeWriteTimeout)
	defer cancel()
	tx, err := s.beginReadCommitted(wctx)
	if err != nil {
		return zeroP, zeroC, err
	}
	defer func() { _ = tx.Rollback(wctx) }()
	qt := store.New(tx)
	if _, err = qt.GetRunByIDForUpdate(wctx, runID); err != nil {
		return zeroP, zeroC, fmt.Errorf("terminal preservation run lock: %w", err)
	}
	pres, err := qt.GetTerminalPreservationForUpdate(wctx, runID)
	if err != nil {
		return zeroP, zeroC, fmt.Errorf("terminal preservation lock: %w", err)
	}
	con, err := qt.GetTerminalContributionByRunGeneration(wctx, store.GetTerminalContributionByRunGenerationParams{RunID: runID, Generation: generation})
	if err != nil {
		return zeroP, zeroC, fmt.Errorf("terminal preservation contribution: %w", err)
	}
	if con.State != terminalContributionInvoked && con.State != terminalContributionUnknown {
		return pres, con, nil
	}
	expected := con.State
	next := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: s.now().Add(d), Valid: true} }
	switch {
	case !rec.notSent && rec.outcome == pushbroker.TerminalApplied:
		con, err = qt.RecordTerminalContributionPublished(wctx, store.RecordTerminalContributionPublishedParams{ID: con.ID, ExpectedState: con.State})
		if errors.Is(err, pgx.ErrNoRows) {
			return zeroP, zeroC, terminalWriteMiss(wctx, qt, "record published", runID, generation, true, expected)
		}
		if err != nil {
			return zeroP, zeroC, fmt.Errorf("terminal preservation published: %w", err)
		}
		pres, err = qt.SettleTerminalPreservationLive(wctx, store.SettleTerminalPreservationLiveParams{
			ID: pres.ID, Tip: pgtype.Text{String: con.Tip, Valid: true}, ExpectedTip: pres.Tip, NextCheckAt: next(terminalLiveRecheck),
		})
	case rec.notSent || rec.outcome == pushbroker.TerminalRefused:
		reason := apitypes.TerminalReasonCreateRefused
		if con.PriorTip.Valid {
			reason = apitypes.TerminalReasonUpdateRefused
		}
		if rec.notSent {
			reason = apitypes.TerminalReasonSendSuppressed
		}
		con, err = qt.RecordTerminalContributionRefused(wctx, store.RecordTerminalContributionRefusedParams{
			ID: con.ID, ExpectedState: con.State, RefusalReason: pgtype.Text{String: reason, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return zeroP, zeroC, terminalWriteMiss(wctx, qt, "record refused", runID, generation, true, expected)
		}
		if err != nil {
			return zeroP, zeroC, fmt.Errorf("terminal preservation refused: %w", err)
		}
		pres, err = qt.SettleTerminalPreservationRefused(wctx, store.SettleTerminalPreservationRefusedParams{
			ID: pres.ID, ExpectedTip: pres.Tip, NextCheckAt: next(terminalLiveRecheck),
			LastError: pgtype.Text{String: rec.lastErr, Valid: rec.lastErr != ""},
		})
	default:
		if con.State == terminalContributionInvoked {
			con, err = qt.RecordTerminalContributionUnknown(wctx, con.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				return zeroP, zeroC, terminalWriteMiss(wctx, qt, "record unknown", runID, generation, true, terminalContributionInvoked)
			}
			if err != nil {
				return zeroP, zeroC, fmt.Errorf("terminal preservation unknown: %w", err)
			}
		}
		if err = terminalSchedule(wctx, qt, store.ScheduleTerminalPreservationCheckParams{
			ID: pres.ID, NextCheckAt: next(terminalUnknownRetryAfter),
			LastError: pgtype.Text{String: rec.lastErr, Valid: rec.lastErr != ""},
		}); err != nil {
			return zeroP, zeroC, err
		}
		pres, err = qt.GetTerminalPreservationByRun(wctx, runID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return zeroP, zeroC, terminalWriteMiss(wctx, qt, "settle preservation", runID, generation, false, terminalStatePublishing)
	}
	if err != nil {
		return zeroP, zeroC, fmt.Errorf("terminal preservation settle: %w", err)
	}
	if s.terminal.outcome != nil {
		if err = s.terminal.outcome(); err != nil {
			return zeroP, zeroC, err
		}
	}
	if err = tx.Commit(wctx); err != nil {
		return zeroP, zeroC, err
	}
	return pres, con, nil
}

// terminalReconcile reports the contribution's state after listing the terminal ref, and records
// published when the ref is at its tip. It never sends, never deletes, and never turns an absent or
// different ref into a terminal state. A listing it cannot make leaves the state as it is, with the
// retryable reason forge_unavailable.
func (s *Service) terminalReconcile(ctx context.Context, runID uuid.UUID, generation int64) (TerminalPublishResult, error) {
	tr, ok := s.q.(terminalReader)
	if !ok {
		return TerminalPublishResult{}, errors.New("terminal preservation: queries are not wired")
	}
	pres, err := tr.GetTerminalPreservationByRun(ctx, runID)
	if err != nil {
		return TerminalPublishResult{}, fmt.Errorf("terminal preservation row: %w", err)
	}
	con, err := tr.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: runID, Generation: generation})
	if err != nil {
		return TerminalPublishResult{}, fmt.Errorf("terminal preservation contribution: %w", err)
	}
	res := terminalResult(pres, con)
	if con.State != terminalContributionInvoked && con.State != terminalContributionUnknown {
		return res, nil
	}
	lctx, cancel := context.WithTimeout(ctx, terminalListTimeout)
	defer cancel()
	f, err := s.heldRemote(lctx, runID)
	if err != nil {
		res.Reason = apitypes.TerminalReasonForgeUnavailable
		return res, nil
	}
	tip, present, err := s.terminal.listFn()(lctx, f.ListOptions(), runID)
	if err != nil {
		res.Reason = apitypes.TerminalReasonForgeUnavailable
		return res, nil
	}
	if !present || tip != con.Tip {
		return res, nil
	}
	pres2, con2, err := s.terminalRecordOutcome(ctx, runID, generation, terminalOutcomeRecord{outcome: pushbroker.TerminalApplied})
	if err != nil {
		slog.Warn("terminal preservation: recording a reconciled publication failed", "run", runID, "error", secretscrub.Scrub(err.Error()))
		return res, nil
	}
	return terminalResult(pres2, con2), nil
}
