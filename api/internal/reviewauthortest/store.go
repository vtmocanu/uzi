// Package reviewauthortest is an in-memory model of the review-author verdict cache and
// per-(repo, ref) queue behind workersvc.ReviewAssessor (issue #2347), for unit tests of its
// callers. It follows the SQL statements' semantics: queue_seq comes from one shared sequence
// that never repeats, every mutation must run inside Mutate (it fails loudly when a queue
// statement is called without the (repo, ref) lock), and a prune only deletes rows at or below
// the observed sequence. The LiveDB tests in internal/store pin the real statements.
package reviewauthortest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type key struct {
	repo uuid.UUID
	ref  string
}

type row struct {
	id        int64
	seq       int64
	admitted  time.Time
	attempted time.Time
}

// Store models both the unlocked reads and the locked mutations.
type Store struct {
	mu       sync.Mutex
	now      func() time.Time
	seq      int64
	queues   map[key][]row
	verdicts map[uuid.UUID]map[int64]time.Time
	held     map[key]bool

	// ListErr, AdmitErr and VerdictListErr force the corresponding call to fail.
	ListErr        error
	AdmitErr       error
	VerdictListErr error
	// Locks counts Mutate calls per (repo, ref), in call order.
	Locks []string
}

// New builds a Store reading time from now (nil means time.Now).
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:      now,
		queues:   map[key][]row{},
		verdicts: map[uuid.UUID]map[int64]time.Time{},
		held:     map[key]bool{},
	}
}

// Order returns the authors queued for (repo, ref) in attempt order.
func (s *Store) Order(repo uuid.UUID, ref string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := append([]row(nil), s.queues[key{repo, ref}]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out
}

// Verdicts returns the not-eligible author ids recorded for repo.
func (s *Store) Verdicts(repo uuid.UUID) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for id := range s.verdicts[repo] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Store) ListReviewAuthorQueue(_ context.Context, arg store.ListReviewAuthorQueueParams) ([]store.ListReviewAuthorQueueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	rows := append([]row(nil), s.queues[key{arg.RepoID, arg.Ref}]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	out := make([]store.ListReviewAuthorQueueRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.ListReviewAuthorQueueRow{ForgeUserID: r.id, QueueSeq: r.seq})
	}
	return out, nil
}

func (s *Store) ListFreshNotEligibleAuthors(_ context.Context, arg store.ListFreshNotEligibleAuthorsParams) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.VerdictListErr != nil {
		return nil, s.VerdictListErr
	}
	out := []int64{}
	for id, at := range s.verdicts[arg.RepoID] {
		if !at.Before(arg.Since.Time) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Store) UpsertReviewAuthorVerdict(_ context.Context, arg store.UpsertReviewAuthorVerdictParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verdicts[arg.RepoID] == nil {
		s.verdicts[arg.RepoID] = map[int64]time.Time{}
	}
	if cur, ok := s.verdicts[arg.RepoID][arg.ForgeUserID]; !ok || arg.NotEligibleAt.Time.After(cur) {
		s.verdicts[arg.RepoID][arg.ForgeUserID] = arg.NotEligibleAt.Time
	}
	return nil
}

func (s *Store) DeleteExpiredReviewAuthorVerdicts(_ context.Context, arg store.DeleteExpiredReviewAuthorVerdictsParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, at := range s.verdicts[arg.RepoID] {
		if at.Before(arg.Before.Time) {
			delete(s.verdicts[arg.RepoID], id)
			n++
		}
	}
	return n, nil
}

func (s *Store) ListStaleReviewAuthorQueueRefs(_ context.Context, arg store.ListStaleReviewAuthorQueueRefsParams) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var refs []string
	for k, rows := range s.queues {
		if k.repo != arg.RepoID {
			continue
		}
		for _, r := range rows {
			if r.touched().Before(arg.Before.Time) {
				refs = append(refs, k.ref)
				break
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

func (r row) touched() time.Time {
	if r.attempted.After(r.admitted) {
		return r.attempted
	}
	return r.admitted
}

// Mutate runs fn holding the (repo, ref) lock: the queue statements fail outside it.
func (s *Store) Mutate(ctx context.Context, repo uuid.UUID, ref string, fn func(workersvc.ReviewAuthorQueueOps) error) error {
	k := key{repo, ref}
	s.mu.Lock()
	s.Locks = append(s.Locks, ref)
	s.held[k] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.held[k] = false
		s.mu.Unlock()
	}()
	return fn(locked{s})
}

// MutateReviewAuthorQueue lets *Store satisfy workersvc.ReviewQueueMutator directly.
func (s *Store) MutateReviewAuthorQueue(ctx context.Context, repo uuid.UUID, ref string, fn func(workersvc.ReviewAuthorQueueOps) error) error {
	return s.Mutate(ctx, repo, ref, fn)
}

type locked struct{ s *Store }

var errNotLocked = errors.New("reviewauthortest: queue statement run without the (repo, ref) lock")

func (l locked) check(repo uuid.UUID, ref string) error {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if !l.s.held[key{repo, ref}] {
		return errNotLocked
	}
	return nil
}

func (l locked) AdmitReviewAuthors(_ context.Context, arg store.AdmitReviewAuthorsParams) error {
	if err := l.check(arg.RepoID, arg.Ref); err != nil {
		return err
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.AdmitErr != nil {
		return l.s.AdmitErr
	}
	k := key{arg.RepoID, arg.Ref}
	for _, id := range arg.ForgeUserIds {
		l.s.seq++ // nextval is drawn per input row, conflicting or not
		exists := false
		for _, r := range l.s.queues[k] {
			if r.id == id {
				exists = true
			}
		}
		if !exists {
			l.s.queues[k] = append(l.s.queues[k], row{id: id, seq: l.s.seq, admitted: l.s.now()})
		}
	}
	return nil
}

func (l locked) RequeueReviewAuthors(_ context.Context, arg store.RequeueReviewAuthorsParams) error {
	if err := l.check(arg.RepoID, arg.Ref); err != nil {
		return err
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	k := key{arg.RepoID, arg.Ref}
	for _, id := range arg.ForgeUserIds {
		l.s.seq++
		for i := range l.s.queues[k] {
			if l.s.queues[k][i].id == id {
				l.s.queues[k][i].seq = l.s.seq
				l.s.queues[k][i].attempted = l.s.now()
			}
		}
	}
	return nil
}

func (l locked) PruneReviewAuthorQueue(_ context.Context, arg store.PruneReviewAuthorQueueParams) (int64, error) {
	if err := l.check(arg.RepoID, arg.Ref); err != nil {
		return 0, err
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	k := key{arg.RepoID, arg.Ref}
	keep := map[int64]bool{}
	for _, id := range arg.KeepIds {
		keep[id] = true
	}
	var kept []row
	var n int64
	for _, r := range l.s.queues[k] {
		if r.seq <= arg.ObservedMaxSeq && !keep[r.id] {
			n++
			continue
		}
		kept = append(kept, r)
	}
	l.s.queues[k] = kept
	return n, nil
}

func (l locked) DeleteStaleReviewAuthorQueue(_ context.Context, arg store.DeleteStaleReviewAuthorQueueParams) (int64, error) {
	if err := l.check(arg.RepoID, arg.Ref); err != nil {
		return 0, err
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	k := key{arg.RepoID, arg.Ref}
	var kept []row
	var n int64
	for _, r := range l.s.queues[k] {
		if r.touched().Before(arg.Before.Time) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	l.s.queues[k] = kept
	return n, nil
}
