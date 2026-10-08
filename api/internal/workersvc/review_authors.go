package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Author eligibility for the MR review-comment rework lane (issue #2347). A review comment
// feeds an agent prompt, so its body is only captured when its AUTHOR may be trusted with that:
// the author has repository access (the same forge.Forge.RepositoryAuthorEligibility question
// the issue lane asks, through issueinput.Assessment), or is an allowlisted review bot
// (settings.KeyMrReviewTrustedBots). ReviewAssessor does that work for both callers, the
// poller's automatic watcher and the on-demand rework endpoint, so they cannot drift.
//
// The work is bounded and fair. One tick makes at most issueinput.MaxDistinctAuthors lookups
// inside issueinput.AssessmentTimeout, each under a per-lookup timeout, and the authors whose
// answer is still needed wait in a per-(repo, ref) FIFO queue (mr_review_author_queue) so a
// flood of unanswerable authors cannot starve the one that matters: whoever was attempted goes
// to the back, whoever was not reached keeps its place at the front.

const (
	// DefaultReviewLookupTimeout bounds one author lookup, so a single hanging forge call
	// costs one slot of the tick instead of the whole assessment deadline.
	DefaultReviewLookupTimeout = 5 * time.Second
	// reviewVerdictTTL is how long a not-eligible answer is trusted before it is asked again.
	reviewVerdictTTL = 6 * time.Hour
	// reviewQueueStaleAfter is how long an untouched queue row survives eviction.
	reviewQueueStaleAfter = 7 * 24 * time.Hour
)

// ReviewAuthorQueueOps is the set of queue statements that mutate a review-author queue.
// *store.Queries satisfies it. They only ever run through ReviewQueueMutator, which holds the
// per-(repo, ref) advisory lock around them.
type ReviewAuthorQueueOps interface {
	AdmitReviewAuthors(ctx context.Context, arg store.AdmitReviewAuthorsParams) error
	RequeueReviewAuthors(ctx context.Context, arg store.RequeueReviewAuthorsParams) error
	PruneReviewAuthorQueue(ctx context.Context, arg store.PruneReviewAuthorQueueParams) (int64, error)
	DeleteStaleReviewAuthorQueue(ctx context.Context, arg store.DeleteStaleReviewAuthorQueueParams) (int64, error)
}

// ReviewQueueMutator runs fn against the queue of one (repo, ref) inside its own short
// transaction, after taking the per-(repo, ref) advisory lock. *Service satisfies it.
type ReviewQueueMutator interface {
	MutateReviewAuthorQueue(ctx context.Context, repoID uuid.UUID, ref string, fn func(ReviewAuthorQueueOps) error) error
}

// ReviewAuthorStore is the unlocked half of the assessor's storage: the verdict cache and the
// queue reads. *store.Queries satisfies it.
type ReviewAuthorStore interface {
	ListReviewAuthorQueue(ctx context.Context, arg store.ListReviewAuthorQueueParams) ([]store.ListReviewAuthorQueueRow, error)
	ListFreshNotEligibleAuthors(ctx context.Context, arg store.ListFreshNotEligibleAuthorsParams) ([]int64, error)
	UpsertReviewAuthorVerdict(ctx context.Context, arg store.UpsertReviewAuthorVerdictParams) error
	DeleteExpiredReviewAuthorVerdicts(ctx context.Context, arg store.DeleteExpiredReviewAuthorVerdictsParams) (int64, error)
	ListStaleReviewAuthorQueueRefs(ctx context.Context, arg store.ListStaleReviewAuthorQueueRefsParams) ([]string, error)
}

// MutateReviewAuthorQueue is the only way a review-author queue is changed. It begins a
// transaction, takes the per-(repo, ref) advisory lock FIRST (store.ReviewAuthorQueueLockClass),
// runs fn on the transaction-bound queries, and commits. The lock is held for fn's few SQL
// statements only: fn must never call the forge or do a lookup. Read-then-write decisions are
// made by the caller from an unlocked read, which is why the prune is bounded by the sequence
// value observed BEFORE the caller fetched its comments (ReviewAssessParams.QueueBound).
func (s *Service) MutateReviewAuthorQueue(ctx context.Context, repoID uuid.UUID, ref string, fn func(ReviewAuthorQueueOps) error) error {
	if s.txBeginner == nil {
		return errors.New("review author queue: no transaction beginner wired")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	q := store.New(tx)
	if err := q.LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repoID, Ref: ref}); err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReviewAssessor assesses the authors of an MR's review comments. The zero Timeout and Now
// mean DefaultReviewLookupTimeout and time.Now.
type ReviewAssessor struct {
	Store   ReviewAuthorStore
	Queue   ReviewQueueMutator
	Timeout time.Duration
	Now     func() time.Time
	// MaxAttempts caps the queued candidate lookups of one assessment; zero means
	// issueinput.MaxDistinctAuthors, the most the shared Assessment allows anyway. Tests lower it
	// to exercise the fair-progress bound with a handful of authors.
	MaxAttempts int
}

func (a *ReviewAssessor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *ReviewAssessor) maxAttempts() int {
	if a.MaxAttempts > 0 {
		return min(a.MaxAttempts, issueinput.MaxDistinctAuthors)
	}
	return issueinput.MaxDistinctAuthors
}

func (a *ReviewAssessor) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return DefaultReviewLookupTimeout
}

// QueueBound reads the largest queue_seq of the (repo, ref) queue. Callers take it before they
// list the MR's comments and pass it as ReviewAssessParams.QueueBound; an error means the
// queue is unreadable and the tick should be skipped.
func (a *ReviewAssessor) QueueBound(ctx context.Context, repoID uuid.UUID, ref string) (int64, error) {
	rows, err := a.Store.ListReviewAuthorQueue(ctx, store.ListReviewAuthorQueueParams{RepoID: repoID, Ref: ref})
	if err != nil {
		return 0, err
	}
	var bound int64
	for _, row := range rows {
		bound = max(bound, row.QueueSeq)
	}
	return bound, nil
}

// ReviewAssessParams is one assessment's input.
type ReviewAssessParams struct {
	// AssessmentTimeout bounds both phases together; zero uses the 30-second
	// background default. ReviewAssessor.Timeout independently bounds each lookup.
	AssessmentTimeout time.Duration
	RepoID            uuid.UUID
	Ref               string
	ProjectID         int64
	BaseURL           string // the connection's base URL, for the trusted-bot instance match
	BotForgeUserID    int64
	Lookup            issueinput.AuthorLookup
	Trusted           []settings.TrustedBot
	// Comments is the MR's complete comment list, oldest first (the driver guarantee).
	Comments []forge.MRComment
	// HighWater and Pending come from the ledger row: a comment is NEW when its id is above
	// HighWater or is in Pending.
	HighWater int64
	Pending   []int64
	// QueueBound is the max queue_seq the caller read (QueueBound) BEFORE it fetched Comments.
	// The end-of-tick prune only deletes rows at or below it: every such row was admitted before
	// the comments were fetched, so its candidacy is decided by this very list. A row admitted
	// by a concurrent assessor after that point has a larger sequence value and survives a keep
	// set that could not have seen its comment. Zero prunes nothing.
	QueueBound int64
}

type reviewClass int

const (
	classUnknown reviewClass = iota // not assessed, or the lookup failed
	classEligible
	classNotEligible
)

// timeoutLookup bounds each lookup with its own deadline. The child context keeps the
// values of the assessment context (forge.BeginAuthorAssessment's evidence cache).
type timeoutLookup struct {
	inner issueinput.AuthorLookup
	d     time.Duration
}

func (t timeoutLookup) RepositoryAuthorEligibility(ctx context.Context, projectID, authorID int64) (forge.AuthorEligibility, error) {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.inner.RepositoryAuthorEligibility(ctx, projectID, authorID)
}

// ReviewAssessment is one assessment in flight. Begin runs the candidate phase; Snapshot
// finishes it. Close releases the deadline and must always be called.
type ReviewAssessment struct {
	assessor *ReviewAssessor
	p        ReviewAssessParams
	asmt     *issueinput.Assessment
	kept     []forge.MRComment // every non-self comment, in forge order (oldest first)
	pending  map[int64]bool
	trusted  map[int64]bool        // author ids matched by the allowlist
	class    map[int64]reviewClass // author id -> decided class
	tried    map[int64]bool        // author ids a lookup was attempted for

	// Attempted is how many queued candidate authors this tick tried to look up (the A_t of
	// the fair-progress bound); ContextAttempted counts the context-only authors Snapshot
	// tried afterwards.
	Attempted        int
	ContextAttempted int
}

// Close releases the assessment's deadline timer.
func (r *ReviewAssessment) Close() { r.asmt.Close() }

// toReviewSnapshot converts a forge comment, sanitizing the author name for display.
func toReviewSnapshot(c forge.MRComment) ReviewCommentSnapshot {
	return ReviewCommentSnapshot{
		ID:                c.ID,
		AuthorUsername:    issueinput.AuthorName(c.AuthorUsername),
		AuthorForgeUserID: c.AuthorForgeUserID,
		CreatedAt:         c.CreatedAt,
		Body:              c.Body,
		Path:              c.Path,
		Line:              c.Line,
		ReplyID:           c.ReplyID,
		ResolveID:         c.ResolveID,
		HeadSHA:           c.HeadSHA,
		ReviewState:       c.ReviewState,
	}
}

func (r *ReviewAssessment) actionable(c forge.MRComment) bool {
	if r.trusted[c.AuthorForgeUserID] && c.ReviewState == forge.ReviewCommentSummary {
		return false // an allowlisted bot's summary note is never a reason to fire
	}
	return IsActionableReviewComment(toReviewSnapshot(c))
}

func (r *ReviewAssessment) isNew(c forge.MRComment) bool {
	return c.ID > r.p.HighWater || r.pending[c.ID]
}

func commentLess(a, b forge.MRComment) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// Begin runs the candidate phase: it decides every author it can without a lookup (allowlist,
// unresolvable id, fresh not-eligible verdict), admits the remaining candidate authors to the
// queue, looks them up in queue order, records the not-eligible answers, and re-queues and
// prunes. A candidate is the author of an actionable NEW comment. It returns (nil, nil) when
// there is nothing to assess: an unknown bot id (the self-filter cannot work, so no snapshot)
// or no comment left after the self-filter. Any error means the tick should be skipped: the
// verdict or queue could not be read, or the admission write failed.
func (a *ReviewAssessor) Begin(ctx context.Context, p ReviewAssessParams) (*ReviewAssessment, error) {
	if p.BotForgeUserID <= 0 {
		return nil, nil
	}
	kept := make([]forge.MRComment, 0, len(p.Comments))
	for _, c := range p.Comments {
		if c.AuthorForgeUserID == p.BotForgeUserID {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	r := &ReviewAssessment{
		assessor: a,
		p:        p,
		asmt:     issueinput.NewAssessmentWithTimeout(ctx, timeoutLookup{p.Lookup, a.timeout()}, p.ProjectID, p.AssessmentTimeout),
		kept:     kept,
		pending:  map[int64]bool{},
		trusted:  map[int64]bool{},
		class:    map[int64]reviewClass{},
		tried:    map[int64]bool{},
	}
	for _, id := range p.Pending {
		r.pending[id] = true
	}
	if err := r.begin(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *ReviewAssessment) begin(ctx context.Context) error {
	a, p := r.assessor, r.p
	fresh, err := a.Store.ListFreshNotEligibleAuthors(ctx, store.ListFreshNotEligibleAuthorsParams{
		RepoID: p.RepoID,
		Since:  pgtype.Timestamptz{Time: a.now().Add(-reviewVerdictTTL), Valid: true},
	})
	if err != nil {
		return err
	}
	freshSet := make(map[int64]bool, len(fresh))
	for _, id := range fresh {
		freshSet[id] = true
	}
	for _, c := range r.kept {
		id := c.AuthorForgeUserID
		if _, done := r.class[id]; done {
			continue
		}
		switch {
		case settings.TrustedBotMatches(p.Trusted, p.BaseURL, id):
			r.trusted[id] = true
			r.class[id] = classEligible
		case id <= 0:
			r.class[id] = classNotEligible // unresolvable identity: never looked up
		case freshSet[id]:
			r.class[id] = classNotEligible
		}
	}

	// Candidates, oldest candidate comment first (the admission order).
	ordered := slices.Clone(r.kept)
	sort.SliceStable(ordered, func(i, j int) bool { return commentLess(ordered[i], ordered[j]) })
	var candidates []int64
	isCandidate := map[int64]bool{}
	for _, c := range ordered {
		id := c.AuthorForgeUserID
		if _, done := r.class[id]; done || isCandidate[id] || !r.isNew(c) || !r.actionable(c) {
			continue
		}
		isCandidate[id] = true
		candidates = append(candidates, id)
	}

	rows, err := r.admitAndList(ctx, candidates)
	if err != nil {
		return err
	}
	// Attempt order: ascending queue position. Candidates the read did not show (a concurrent
	// eviction) follow, in admission order.
	attemptOrder := make([]int64, 0, len(candidates))
	queued := map[int64]bool{}
	for _, row := range rows {
		queued[row.ForgeUserID] = true
		if isCandidate[row.ForgeUserID] {
			attemptOrder = append(attemptOrder, row.ForgeUserID)
		}
	}
	for _, id := range candidates {
		if !queued[id] {
			attemptOrder = append(attemptOrder, id)
		}
	}

	var attempted []int64
	for _, id := range attemptOrder {
		if r.asmt.Context().Err() != nil || r.Attempted >= a.maxAttempts() {
			break // the rest were not reached: they keep their place at the front
		}
		r.Attempted++
		r.lookup(id)
		attempted = append(attempted, id)
	}
	r.recordVerdicts(ctx, attempted)

	var requeue []int64
	for _, id := range attempted {
		if r.class[id] != classEligible {
			requeue = append(requeue, id) // unknown or not-eligible go to the back, in attempt order
		}
	}
	if len(rows) > 0 || len(candidates) > 0 {
		err := a.Queue.MutateReviewAuthorQueue(ctx, p.RepoID, p.Ref, func(q ReviewAuthorQueueOps) error {
			if len(requeue) > 0 {
				if err := q.RequeueReviewAuthors(ctx, store.RequeueReviewAuthorsParams{RepoID: p.RepoID, Ref: p.Ref, ForgeUserIds: requeue}); err != nil {
					return err
				}
			}
			keep := candidates
			if keep == nil {
				keep = []int64{}
			}
			_, err := q.PruneReviewAuthorQueue(ctx, store.PruneReviewAuthorQueueParams{
				RepoID: p.RepoID, Ref: p.Ref, ObservedMaxSeq: p.QueueBound, KeepIds: keep,
			})
			return err
		})
		if err != nil {
			// Best effort: this tick's answers stand; the next tick re-queues and prunes.
			slog.Warn("workersvc: review author queue requeue/prune", "repo", p.RepoID.String(), "ref", p.Ref, "error", err)
		}
	}
	return nil
}

// admitAndList admits the candidates under the lock and then reads the queue unlocked, for the
// attempt order. The prune is NOT bounded by this read (it follows the admission and the
// comment fetch): it is bounded by ReviewAssessParams.QueueBound, read before the fetch.
func (r *ReviewAssessment) admitAndList(ctx context.Context, candidates []int64) ([]store.ListReviewAuthorQueueRow, error) {
	a, p := r.assessor, r.p
	if len(candidates) > 0 {
		err := a.Queue.MutateReviewAuthorQueue(ctx, p.RepoID, p.Ref, func(q ReviewAuthorQueueOps) error {
			return q.AdmitReviewAuthors(ctx, store.AdmitReviewAuthorsParams{RepoID: p.RepoID, Ref: p.Ref, ForgeUserIds: candidates})
		})
		if err != nil {
			return nil, err
		}
	}
	return a.Store.ListReviewAuthorQueue(ctx, store.ListReviewAuthorQueueParams{RepoID: p.RepoID, Ref: p.Ref})
}

// lookup asks the forge about one author through the shared Assessment, which caches the
// answer, enforces the deadline and the distinct-author budget, and degrades any failure to
// unknown.
func (r *ReviewAssessment) lookup(id int64) {
	r.tried[id] = true
	name := ""
	if c, ok := r.firstComment(id); ok {
		name = c.AuthorUsername
	}
	d, _ := r.asmt.Author(forge.Issue{AuthorForgeUserID: id, Author: name})
	switch d {
	case forge.AuthorEligible:
		r.class[id] = classEligible
	case forge.AuthorNotEligible:
		r.class[id] = classNotEligible
	default:
		r.class[id] = classUnknown
	}
}

func (r *ReviewAssessment) firstComment(authorID int64) (forge.MRComment, bool) {
	for _, c := range r.kept {
		if c.AuthorForgeUserID == authorID {
			return c, true
		}
	}
	return forge.MRComment{}, false
}

// recordVerdicts stores the not-eligible answers just obtained. Best effort: a failed write
// only costs one repeat lookup next tick.
func (r *ReviewAssessment) recordVerdicts(ctx context.Context, ids []int64) {
	p := r.p
	for _, id := range ids {
		if r.class[id] != classNotEligible {
			continue
		}
		err := r.assessor.Store.UpsertReviewAuthorVerdict(ctx, store.UpsertReviewAuthorVerdictParams{
			RepoID:        p.RepoID,
			ForgeUserID:   id,
			NotEligibleAt: pgtype.Timestamptz{Time: r.assessor.now(), Valid: true},
		})
		if err != nil {
			slog.Warn("workersvc: record review author verdict", "repo", p.RepoID.String(), "error", err)
		}
	}
}

func (r *ReviewAssessment) classOf(c forge.MRComment) reviewClass {
	return r.class[c.AuthorForgeUserID] // an author never assessed reads as unknown
}

// HasTrigger reports whether an ELIGIBLE, actionable, new comment exists among the authors
// decided so far: the condition for the automatic watcher to go on and fire.
func (r *ReviewAssessment) HasTrigger() bool {
	for _, c := range r.kept {
		if r.isNew(c) && r.actionable(c) && r.classOf(c) == classEligible {
			return true
		}
	}
	return false
}

// NewestEligible is the newest eligible comment decided so far. The watcher's cheap early
// debounce check reads it: assessing more authors can only make the newest comment newer, so
// a comment still inside the quiet period stays inside it.
func (r *ReviewAssessment) NewestEligible() (forge.MRComment, bool) {
	for i := len(r.kept) - 1; i >= 0; i-- {
		if r.classOf(r.kept[i]) == classEligible {
			return r.kept[i], true
		}
	}
	return forge.MRComment{}, false
}

// UnknownNewCount counts the actionable new comments whose author is permission-unknown.
func (r *ReviewAssessment) UnknownNewCount() int {
	n := 0
	for _, c := range r.kept {
		if r.isNew(c) && r.actionable(c) && r.classOf(c) == classUnknown {
			n++
		}
	}
	return n
}

// ReviewSnapshotResult is the finished assessment: the eligible-only snapshot plus what the
// ledger needs to know about every comment.
type ReviewSnapshotResult struct {
	Snapshot *ReviewCommentsSnapshot
	// Attempted and ContextAttempted mirror the assessment's counters.
	Attempted        int
	ContextAttempted int

	eligibleActionable []int64         // ids of actionable comments kept in the (capped) snapshot
	unknownActionable  []int64         // ids of actionable comments whose author is unknown
	unknownAuthor      map[int64]int64 // comment id -> author id, for unknownActionable
	class              map[int64]reviewClass
	inSnapshot         map[int64]bool
	// highWater and pending are the ledger values the assessment was begun with
	// (ReviewAssessParams.HighWater/Pending), which callers read BEFORE listing the comments.
	// PlanAssessed plans against exactly these, never a later re-read.
	highWater int64
	pending   []int64
}

// Snapshot finishes the assessment: it looks up the context-only authors (everyone not yet
// decided, oldest comment first, never queued), classifies the whole comment list, and only
// then applies the count and byte caps to the eligible comments. Comments of authors that
// could not be assessed in time are withheld as permission_unknown.
func (r *ReviewAssessment) Snapshot(ctx context.Context) *ReviewSnapshotResult {
	ordered := slices.Clone(r.kept)
	sort.SliceStable(ordered, func(i, j int) bool { return commentLess(ordered[i], ordered[j]) })
	var ctxTried []int64
	for _, c := range ordered {
		id := c.AuthorForgeUserID
		if _, done := r.class[id]; done || r.tried[id] {
			continue
		}
		if r.asmt.Context().Err() != nil || len(r.tried) >= issueinput.MaxDistinctAuthors {
			break
		}
		r.ContextAttempted++
		r.lookup(id)
		ctxTried = append(ctxTried, id)
	}
	r.recordVerdicts(ctx, ctxTried)

	res := &ReviewSnapshotResult{
		Attempted:        r.Attempted,
		ContextAttempted: r.ContextAttempted,
		class:            make(map[int64]reviewClass, len(r.kept)),
		inSnapshot:       map[int64]bool{},
		unknownAuthor:    map[int64]int64{},
		highWater:        r.p.HighWater,
		pending:          slices.Clone(r.p.Pending),
	}
	snap := &ReviewCommentsSnapshot{Version: ReviewSnapshotVersion, Comments: []ReviewCommentSnapshot{}}
	var eligible []ReviewCommentSnapshot
	var eligibleActionable []bool
	for _, c := range r.kept {
		cl := r.classOf(c)
		res.class[c.ID] = cl
		switch cl {
		case classEligible:
			eligible = append(eligible, toReviewSnapshot(c))
			eligibleActionable = append(eligibleActionable, r.actionable(c))
		case classNotEligible:
			snap.WithheldNotEligible++
		default:
			snap.WithheldUnknown++
			if r.actionable(c) {
				res.unknownActionable = append(res.unknownActionable, c.ID)
				res.unknownAuthor[c.ID] = c.AuthorForgeUserID
			}
		}
	}
	snap.Comments, snap.Truncated = capReviewComments(eligible)
	if snap.Comments == nil {
		snap.Comments = []ReviewCommentSnapshot{} // never "comments":null on the wire
	}
	// The caps keep the newest tail, so the kept comments line up with the end of eligible.
	offset := len(eligible) - len(snap.Comments)
	for i, c := range snap.Comments {
		res.inSnapshot[c.ID] = true
		if eligibleActionable[offset+i] {
			res.eligibleActionable = append(res.eligibleActionable, c.ID)
		}
	}
	res.Snapshot = snap
	return res
}

// ReviewPendingCap is the size the ledger's pending_unknown_ids is bounded to (the CHECK and
// the LIMIT of mr_rework_merge_pending). On overflow the database keeps the OLDEST ids.
const ReviewPendingCap = 10000

// ReviewPlan is what a snapshot means for one ledger row.
type ReviewPlan struct {
	// HasNew: an eligible actionable comment in the snapshot is above the high-water mark or
	// pending, i.e. there is something to rework.
	HasNew bool
	// MaxActionableID is the high-water mark to advance to: the largest eligible actionable
	// id in the (capped) snapshot, 0 when there is none.
	MaxActionableID int64
	// UnknownNew counts actionable comments from permission-unknown authors that are new.
	UnknownNew int
	// PendingAdd and PendingRemove are the ledger's pending_unknown_ids delta.
	PendingAdd    []int64
	PendingRemove []int64
	// PendingSuperseded and PendingSupersededBy are parallel slices (never nil): an author's
	// older pending id and that author's newer representative. The ledger merge drops the older
	// id only when the representative is retained in the same atomic statement (a retained
	// representative takes the older id's place in the cap order within that merge). A stale
	// writer whose add the high-water filter rejects, or whose representative another writer
	// removed, therefore leaves the older id in place instead of losing the author.
	PendingSuperseded   []int64
	PendingSupersededBy []int64
	// PendingEvicted is the part of PendingRemove that is safe to drop even when no run is
	// created: pending ids whose author is eligible but whose comment the snapshot caps evicted.
	// They fall back to human review; left pending they would keep HasTrigger true (it ignores
	// the caps) while HasNew stays false, repeating the full assessment every tick.
	PendingEvicted []int64
}

// PlanAssessed plans against the ledger values the assessment began with, which the caller
// read before listing the comments. A pending id in that set therefore existed before the
// listing, so one absent from the listing is gone whatever the forge's id order. The DELTAS
// (pending add/remove/supersession and MaxActionableID) must come from that pre-listing row:
// a later re-read cannot tell a pending id the listing lost from one added after it. Whether
// there is still something new is a separate question, HasNewAgainst, which the create paths
// re-validate against the current row under the branch lock.
func (res *ReviewSnapshotResult) PlanAssessed() ReviewPlan {
	return res.Plan(res.highWater, res.pending)
}

// HasNewAgainst reports whether an eligible actionable comment in the snapshot is above
// highWater or in pending: the freshness test the create paths re-run against the CURRENT
// ledger row under the branch lock. It carries no deltas.
func (res *ReviewSnapshotResult) HasNewAgainst(highWater int64, pending []int64) bool {
	for _, id := range res.eligibleActionable {
		if id > highWater || slices.Contains(pending, id) {
			return true
		}
	}
	return false
}

// Plan computes the plan against a ledger row's high-water mark and pending set. The row must
// have been read before the comments were listed (see PlanAssessed).
func (res *ReviewSnapshotResult) Plan(highWater int64, pending []int64) ReviewPlan {
	pendingSet := make(map[int64]bool, len(pending))
	for _, id := range pending {
		pendingSet[id] = true
	}
	var plan ReviewPlan
	for _, id := range res.eligibleActionable {
		if id > plan.MaxActionableID {
			plan.MaxActionableID = id
		}
	}
	plan.HasNew = res.HasNewAgainst(highWater, pending)
	newMark := max(highWater, plan.MaxActionableID)
	// One representative pending id per unverified author: that author's NEWEST unknown
	// actionable id among those the ledger merge will accept (above the old mark and at or below
	// the new one, or already pending). Newest because capReviewComments keeps the newest tail:
	// when the author later resolves eligible the firing run's snapshot is rebuilt from the full
	// comment list, so one id per author is enough to trigger, and the newest is the one the
	// caps are most likely to keep. A flood of outsider comments therefore costs one slot per
	// outsider, not one per comment, and cannot push an eligible author's id out of the set.
	rep := map[int64]int64{} // author id -> newest allowed unknown actionable id
	for _, id := range res.unknownActionable {
		allowed := pendingSet[id] || (id > highWater && id <= newMark)
		if id > highWater || pendingSet[id] {
			plan.UnknownNew++
		}
		if !allowed {
			continue
		}
		if a := res.unknownAuthor[id]; id > rep[a] {
			rep[a] = id
		}
	}
	plan.PendingAdd = make([]int64, 0, len(rep))
	for _, id := range rep {
		plan.PendingAdd = append(plan.PendingAdd, id)
	}
	slices.Sort(plan.PendingAdd)
	isRep := make(map[int64]bool, len(rep))
	for _, id := range plan.PendingAdd {
		isRep[id] = true
	}
	newAdds := 0
	for _, id := range plan.PendingAdd {
		if !pendingSet[id] {
			newAdds++
		}
	}
	plan.PendingRemove = []int64{}
	plan.PendingEvicted = []int64{}
	plan.PendingSuperseded = []int64{}
	plan.PendingSupersededBy = []int64{}
	var superseded []int64 // older pending ids of authors whose newer representative is in PendingAdd
	for _, id := range pending {
		cl, present := res.class[id]
		switch {
		case !present, cl == classNotEligible:
			plan.PendingRemove = append(plan.PendingRemove, id) // gone, or its author is now known to be out
		case cl == classEligible && res.inSnapshot[id]:
			plan.PendingRemove = append(plan.PendingRemove, id) // consumed by this snapshot
		case cl == classEligible:
			plan.PendingRemove = append(plan.PendingRemove, id) // evicted by the caps: human review
			plan.PendingEvicted = append(plan.PendingEvicted, id)
		case cl == classUnknown:
			if a, ok := res.unknownAuthor[id]; ok && rep[a] != 0 && !isRep[id] {
				superseded = append(superseded, id)
			}
		}
	}
	// One id per author across ticks: an older pending id of an author whose newer representative
	// is being added (or already pending) is superseded, but only when the merged set is
	// guaranteed to fit. The supersession is NOT a removal: the ledger merge
	// (mr_rework_merge_pending) applies each (older, representative) pair only when the
	// representative is retained in the same statement, so a stale writer's rejected add cannot
	// cost the author its only id. The fit check counts the superseded ids as dropped; near the cap,
	// when the set would overflow, the pairs are withheld and both ids may briefly stay (the next
	// tick, after the removals, emits them once they fit).
	incoming := len(pending) + newAdds - len(plan.PendingRemove)
	if incoming-len(superseded) <= ReviewPendingCap {
		for _, id := range superseded {
			plan.PendingSuperseded = append(plan.PendingSuperseded, id)
			plan.PendingSupersededBy = append(plan.PendingSupersededBy, rep[res.unknownAuthor[id]])
		}
	} else {
		slog.Warn("workersvc: review pending set would exceed its cap; the ledger keeps the oldest ids",
			"incoming", incoming, "cap", ReviewPendingCap)
	}
	return plan
}

// NewestEligible is the newest eligible comment of the finished snapshot.
func (res *ReviewSnapshotResult) NewestEligible() (ReviewCommentSnapshot, bool) {
	if n := len(res.Snapshot.Comments); n > 0 {
		return res.Snapshot.Comments[n-1], true
	}
	return ReviewCommentSnapshot{}, false
}

// EvictStale is the best-effort housekeeping the watcher runs once per repo tick: expired
// verdicts, and queue rows untouched for a week. Each stale queue is evicted under its own
// lock, so the eviction cannot interleave with a running mutation of that queue.
func (a *ReviewAssessor) EvictStale(ctx context.Context, repoID uuid.UUID) error {
	now := a.now()
	var errs []error
	if _, err := a.Store.DeleteExpiredReviewAuthorVerdicts(ctx, store.DeleteExpiredReviewAuthorVerdictsParams{
		RepoID: repoID, Before: pgtype.Timestamptz{Time: now.Add(-reviewVerdictTTL), Valid: true},
	}); err != nil {
		errs = append(errs, err)
	}
	before := pgtype.Timestamptz{Time: now.Add(-reviewQueueStaleAfter), Valid: true}
	refs, err := a.Store.ListStaleReviewAuthorQueueRefs(ctx, store.ListStaleReviewAuthorQueueRefsParams{RepoID: repoID, Before: before})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, ref := range refs {
		err := a.Queue.MutateReviewAuthorQueue(ctx, repoID, ref, func(q ReviewAuthorQueueOps) error {
			_, err := q.DeleteStaleReviewAuthorQueue(ctx, store.DeleteStaleReviewAuthorQueueParams{RepoID: repoID, Ref: ref, Before: before})
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
