package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ciFixBindingMaxCandidates caps how many recorded pull requests one ci_fix create
// will ask the forge about. The candidate query returns one extra row, so a ninth
// means "more than the cap" and the binding is refused as unresolved without a forge
// call.
const ciFixBindingMaxCandidates = 8

// maxSafeMRIID is the largest iid a ci_fix can be bound to; it matches the upper
// bound of the runs_ci_fix_mr_iid_check constraint (2^53 - 1, the largest integer a
// JSON number carries exactly).
const maxSafeMRIID = 9007199254740991

// CIFixBindingLookupBudget bounds the whole candidate read plus every forge call of one
// binding resolution. Exported so the manual Fix CI handler can size its response write
// deadline from it; a var so a test can shorten it.
var CIFixBindingLookupBudget = 15 * time.Second

// ErrCIFixPRUnresolved is matched (errors.Is) by *CIFixPRUnresolvedError: the branch's
// pull-request association cannot be determined unambiguously. It is a verdict about
// the branch, not a transient failure.
var ErrCIFixPRUnresolved = errors.New("could not determine which pull request this branch belongs to")

// ErrCIFixPRUnavailable wraps a failure to verify the branch's pull requests (database
// read, missing forge, transport, auth, rate limit, timeout, or a PR in the transient
// locked state). Nothing was decided; a retry may succeed.
var ErrCIFixPRUnavailable = errors.New("could not verify the branch's pull request")

// ErrCIFixBindingChanged is returned when the branch's candidate pull requests changed
// between the forge check and the insert.
var ErrCIFixBindingChanged = errors.New("the branch's pull-request association changed while creating the CI fix")

// CIFixPRUnresolvedError carries a fixed uzi-authored reason (never forge text) why
// the association is unresolved.
type CIFixPRUnresolvedError struct{ Reason string }

func (e *CIFixPRUnresolvedError) Error() string {
	return ErrCIFixPRUnresolved.Error() + ": " + e.Reason
}

// Is makes errors.Is(err, ErrCIFixPRUnresolved) true for every unresolved error.
func (e *CIFixPRUnresolvedError) Is(target error) bool { return target == ErrCIFixPRUnresolved }

func ciFixUnavailable(err error) error { return fmt.Errorf("%w: %w", ErrCIFixPRUnavailable, err) }

// errCIFixLookupFailed is the fixed uzi-authored cause shown for a failure that is not a
// forge driver error (database read, forge construction, missing forge client).
var errCIFixLookupFailed = errors.New("the pull-request lookup could not run")

// ciFixLookupFailed logs a non-driver lookup failure and returns it as unavailable with a
// fixed cause, so a database or construction error (which can carry host, user or
// database names) never reaches an HTTP body. A deadline or cancellation carries no such
// data and stays reachable through errors.Is.
func ciFixLookupFailed(op string, err error) error {
	slog.Warn("ci-fix: pull-request lookup failed", "op", op, "error", err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ciFixUnavailable(context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return ciFixUnavailable(context.Canceled)
	}
	return ciFixUnavailable(errCIFixLookupFailed)
}

// resolveCIFixBinding decides which pull request a ci_fix on ref binds to.
//
// The candidates are only the pull requests uzi recorded (runs.mr_iid) on its own runs
// for this branch. A head-branch match alone is never proof: no driver's head-branch
// field identifies the head repository, so asking the forge "which PRs use this branch
// name" could match a fork's PR. On GitLab the worker's duplicate-create recovery
// (GitLabClient.findOpenMr in agent/src/forge.ts) can adopt a same-named fork MR into
// runs.mr_iid, so the recorded set is only as fork-safe as that recording (tracked
// separately). Each candidate is read from the forge and classified:
//
//	forge answer                                   class
//	opened, source branch == ref                   open match
//	opened, different source branch                unresolvable
//	merged / closed                                confirmed not open
//	locked                                         unavailable (transient during merge)
//	404 (forge.ErrMergeRequestNotFound)            unresolvable
//	iid mismatch, empty state/branch, unknown state unresolvable
//	any other error (auth 401/403, rate limit,
//	  transport, deadline, cancel)                 unavailable
//
// 401/403 are unavailable, not unresolvable: a bad or rotated token says nothing about
// the PR, and the owner can fix it and retry. Precedence: any unavailable wins; else any
// unresolvable or two or more open matches is unresolved; else exactly one open match
// binds; else the fix is unbound (Valid=false). checked is the candidate set the
// decision was made on, for the in-transaction recheck.
func (s *Service) resolveCIFixBinding(ctx context.Context, row store.GetRepoForUserRow, userID, repoID uuid.UUID, ref string) (bound pgtype.Int8, checked []int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, CIFixBindingLookupBudget)
	defer cancel()

	cands, err := s.q.ListCIFixBindingCandidates(ctx, store.ListCIFixBindingCandidatesParams{
		UserID: userID, RepoID: repoID, Ref: pgtype.Text{String: ref, Valid: true},
	})
	if err != nil {
		return pgtype.Int8{}, nil, ciFixLookupFailed("list candidates", err)
	}
	if len(cands) > ciFixBindingMaxCandidates {
		return pgtype.Int8{}, nil, &CIFixPRUnresolvedError{Reason: "too many pull requests were recorded on this branch"}
	}
	for _, iid := range cands {
		if iid <= 0 || iid > maxSafeMRIID {
			return pgtype.Int8{}, nil, &CIFixPRUnresolvedError{Reason: "an invalid pull request id was recorded on this branch"}
		}
	}
	if len(cands) == 0 {
		return pgtype.Int8{}, []int64{}, nil
	}
	if s.forges == nil {
		return pgtype.Int8{}, nil, ciFixLookupFailed("forge client", errors.New("no forge client is configured"))
	}
	f, err := s.forges.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		return pgtype.Int8{}, nil, ciFixLookupFailed("forge for connection", err)
	}
	var ch forge.CodeHost = f

	var (
		open        []int64
		unavailable error
		reason      string
	)
	for _, iid := range cands {
		sum, gerr := ch.GetMergeRequestSummary(ctx, row.ForgeProjectID, iid)
		switch {
		case gerr != nil && errors.Is(gerr, forge.ErrMergeRequestNotFound):
			if reason == "" {
				reason = fmt.Sprintf("pull request %d was not found", iid)
			}
		case gerr != nil:
			if unavailable == nil {
				unavailable = ciFixUnavailable(gerr)
			}
		case sum.IID != iid || sum.State == "" || sum.SourceBranch == "" || !forge.IsKnownMRState(sum.State):
			if reason == "" {
				reason = fmt.Sprintf("pull request %d returned an unexpected answer", iid)
			}
		case sum.State == forge.MRStateLocked:
			if unavailable == nil {
				unavailable = ciFixUnavailable(fmt.Errorf("pull request %d is being merged", iid))
			}
		case sum.State == forge.MRStateOpened && sum.SourceBranch == ref:
			open = append(open, iid)
		case sum.State == forge.MRStateOpened:
			if reason == "" {
				reason = fmt.Sprintf("pull request %d is open on a different branch", iid)
			}
		}
		if ctx.Err() != nil {
			// The budget (or the caller) is spent: every remaining candidate would fail the
			// same way, and that is unavailable, not a verdict.
			if unavailable == nil {
				unavailable = ciFixUnavailable(ctx.Err())
			}
			break
		}
	}
	switch {
	case unavailable != nil:
		return pgtype.Int8{}, nil, unavailable
	case reason != "":
		return pgtype.Int8{}, nil, &CIFixPRUnresolvedError{Reason: reason}
	case len(open) > 1:
		return pgtype.Int8{}, nil, &CIFixPRUnresolvedError{Reason: "more than one open pull request uses this branch"}
	case len(open) == 1:
		return pgtype.Int8{Int64: open[0], Valid: true}, slices.Clone(cands), nil
	}
	return pgtype.Int8{}, slices.Clone(cands), nil
}

// sameCIFixCandidateSet compares two candidate id lists as sets (order-insensitive).
func sameCIFixCandidateSet(a, b []int64) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
