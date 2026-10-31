package forgesvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type mergeCandidates struct {
	targets        func(context.Context, store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error)
	rows           map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow
	fail           map[uuid.UUID]bool
	visits         []store.ListMergedPredecessorCandidatesParams
	afterEnumerate func()
}

func (q *mergeCandidates) ListMergedPredecessorCandidates(ctx context.Context, p store.ListMergedPredecessorCandidatesParams) ([]store.ListMergedPredecessorCandidatesRow, error) {
	q.visits = append(q.visits, p)
	if q.afterEnumerate != nil {
		q.afterEnumerate()
	}
	if q.fail[p.RepoID] {
		return nil, errors.New("enumeration")
	}
	for _, row := range q.rows[p.RepoID] {
		if slices.Compare(row.RunID[:], p.AfterRunID[:]) > 0 {
			return []store.ListMergedPredecessorCandidatesRow{row}, nil
		}
	}
	return nil, nil
}

func (q *mergeCandidates) ListCrossRunTargets(ctx context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
	if q.targets != nil {
		return q.targets(ctx, p)
	}
	return nil, nil
}

type mergeSink struct {
	cross func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error)
	calls int
	err   error
}

func (r *mergeSink) DiscardMergedPredecessorHolds(context.Context, recovery.VerifiedMergeSnapshot) (int64, error) {
	r.calls++
	return 1, r.err
}

func (r *mergeSink) DiscardCrossRunHolds(ctx context.Context, target recovery.CrossRunTargetBinding, witness recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
	if r.cross != nil {
		return r.cross(ctx, target, witness)
	}
	return 0, nil
}

type mergeForge struct {
	forgetest.BaseFake
	read func(context.Context, int64, int64) (forge.MergeRequest, error)
}

func (f *mergeForge) GetMergeRequest(ctx context.Context, project, iid int64) (forge.MergeRequest, error) {
	return f.read(ctx, project, iid)
}
func mergeID(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }
func mergeTestService(t *testing.T, f forge.Forge) (*Service, []byte) {
	t.Helper()
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithForgeBuilder(nil, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return f, nil })
	token, err := s.EncryptToken("fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s, token
}
func mergeRow(repo uuid.UUID, n int, token []byte) store.ListMergedPredecessorCandidatesRow {
	return store.ListMergedPredecessorCandidatesRow{RunID: mergeID(n), OwnerID: mergeID(9000), RepoID: repo, ConnectionID: mergeID(9001), OriginalBranch: true, ProjectID: 42, MrIid: int64(n), CompletedGeneration: 2, ForgeType: "gitlab", BaseUrl: "https://forge.test", TokenCiphertext: token}
}

func TestMergedPredecessorFairnessAndRestart(t *testing.T) {
	var reads []int64
	f := &mergeForge{read: func(ctx context.Context, p, i int64) (forge.MergeRequest, error) {
		if p != 42 {
			t.Errorf("project=%d", p)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("observation budget")
		}
		reads = append(reads, i)
		return forge.MergeRequest{IID: i, State: forge.MRStateOpened}, nil
	}}
	s, token := mergeTestService(t, f)
	a, b := mergeID(1), mergeID(2)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}}
	for n := 101; n <= 250; n++ {
		q.rows[a] = append(q.rows[a], mergeRow(a, n, token))
		q.rows[b] = append(q.rows[b], mergeRow(b, n+1000, token))
	}
	s.SetMergedPredecessorRecovery(q, &mergeSink{})
	// Concurrent callers serialize the phase and cursor updates.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{b, a}) }()
	}
	wg.Wait()
	if len(reads) != 300 {
		t.Fatalf("reads=%d", len(reads))
	}
	for i := 0; i < 300; i += 2 {
		if reads[i] != int64(101+i/2) || reads[i+1] != int64(1101+i/2) {
			t.Fatalf("fair sequence at %d: %v", i, reads[i:i+2])
		}
	}
	// Exhausted cursors wrap for a later phase, with no duplicates in that phase.
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b})
	if len(reads) != 300 {
		t.Fatal("exhaustion repeated a candidate")
	}
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b})
	if len(reads) != 400 || reads[300] != 101 {
		t.Fatal("wrapped cursor did not retry refusals")
	}
	restarted, _ := mergeTestService(t, f)
	restarted.SetMergedPredecessorRecovery(q, &mergeSink{})
	restarted.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a})
	if reads[400] != 101 {
		t.Fatal("restart lost durable OPEN workset")
	}
}

func TestMergedPredecessorRefusals(t *testing.T) {
	for _, kind := range []string{"", "unknown", "opened", "closed", "locked", "wrong_iid", "unreadable", "bad_token", "discard_error", "too_large", "merged"} {
		t.Run(kind, func(t *testing.T) {
			f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				if kind == "unreadable" {
					return forge.MergeRequest{}, errors.New("secret must not be logged")
				}
				if kind == "too_large" {
					// A refused oversized response is unverified evidence even when
					// the fake also hands back a merged-looking value.
					return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, &forge.ResponseTooLargeError{}
				}
				if kind == "wrong_iid" {
					return forge.MergeRequest{IID: iid + 1, State: forge.MRStateMerged}, nil
				}
				state := kind
				if kind == "bad_token" || kind == "discard_error" {
					state = forge.MRStateMerged
				}
				return forge.MergeRequest{IID: iid, State: state}, nil
			}}
			s, token := mergeTestService(t, f)
			if kind == "bad_token" {
				token = []byte("invalid")
			}
			repo := mergeID(1)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {mergeRow(repo, 100, token)}}}
			sink := &mergeSink{}
			if kind == "discard_error" {
				sink.err = errors.New("binding changed")
			}
			s.SetMergedPredecessorRecovery(q, sink)
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			want := 0
			if kind == "merged" || kind == "discard_error" {
				want = 1
			}
			if sink.calls != want {
				t.Fatalf("discard calls=%d want=%d", sink.calls, want)
			}
			if s.merged.cursors[repo] != uuid.Nil {
				t.Fatal("exhausted cursor should wrap")
			}
		})
	}
}

func TestMergedPredecessorVisitsCancellationAndPruning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		cancel()
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}}
	s, token := mergeTestService(t, f)
	var repos []uuid.UUID
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}, fail: map[uuid.UUID]bool{}}
	for n := 1; n <= 120; n++ {
		repo := mergeID(n)
		repos = append(repos, repo)
		q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{mergeRow(repo, n+200, token)}
		q.fail[repo] = true
	}
	sink := &mergeSink{}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), repos)
	if len(q.visits) != 100 || s.merged.nextRepo != mergeID(101) {
		t.Fatal("repository visit budget/position")
	}
	q.fail = map[uuid.UUID]bool{}
	s.ReconcileMergedPredecessors(ctx, repos)
	if reads != 1 || sink.calls != 0 || s.merged.nextRepo != mergeID(102) {
		t.Fatal("cancellation did not preserve next repository")
	}
	if s.merged.cursors[mergeID(101)] != mergeID(301) {
		t.Fatal("attempted cursor not saved")
	}
	s.ReconcileMergedPredecessors(context.Background(), nil)
	if len(s.merged.cursors) != 0 || s.merged.nextRepo != uuid.Nil {
		t.Fatal("disabled repositories not pruned")
	}
}

func TestMergedPredecessorIndependentDeadlines(t *testing.T) {
	for _, phase := range []bool{true, false} {
		t.Run(fmt.Sprintf("phase_%v", phase), func(t *testing.T) {
			parent := context.Background()
			reads := 0
			f := &mergeForge{read: func(ctx context.Context, _, iid int64) (forge.MergeRequest, error) {
				reads++
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Fatal("observation child not exhausted")
				}
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}}
			s, token := mergeTestService(t, f)
			repo := mergeID(1)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {mergeRow(repo, 100, token)}}}
			sink := &mergeSink{}
			s.SetMergedPredecessorRecovery(q, sink)
			var budgets []time.Duration
			s.merged.withTimeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
				budgets = append(budgets, d)
				if (phase && d == 30*time.Second) || (!phase && d == 5*time.Second) {
					return context.WithDeadline(ctx, time.Now().Add(-time.Hour))
				}
				return context.WithTimeout(ctx, d)
			}
			s.ReconcileMergedPredecessors(parent, []uuid.UUID{repo})
			if parent.Err() != nil || sink.calls != 0 {
				t.Fatal("deadline escaped child or accepted late merged response")
			}
			wantReads := 1
			wantBudgets := []time.Duration{30 * time.Second, 5 * time.Second}
			if phase {
				wantReads = 0
				wantBudgets = wantBudgets[:1]
			}
			if reads != wantReads || !slices.Equal(budgets, wantBudgets) {
				t.Fatalf("reads=%d budgets=%v", reads, budgets)
			}
			s.merged.withTimeout = nil
			f.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}
			// The exhausted phase did not consume work; late observation wraps for retry.
			if !phase {
				s.ReconcileMergedPredecessors(parent, []uuid.UUID{repo})
			}
			s.ReconcileMergedPredecessors(parent, []uuid.UUID{repo})
			if sink.calls == 0 {
				t.Fatal("deadline refusal lost retry")
			}
		})
	}
}

type mergeDeadlineContext struct{ context.Context }

func TestMergedPredecessorEnumerationExpiresWithoutConsumingCandidate(t *testing.T) {
	for _, result := range []string{"rows", "error", "empty"} {
		t.Run(result, func(t *testing.T) {
			var reads []int64
			f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				reads = append(reads, iid)
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}}
			s, token := mergeTestService(t, f)
			sibling, repo := mergeID(1), mergeID(2)
			q := &mergeCandidates{
				rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{
					sibling: {mergeRow(sibling, 101, token)},
					repo:    {mergeRow(repo, 201, token)},
				},
				fail: map[uuid.UUID]bool{repo: result == "error"},
			}
			if result == "empty" {
				q.rows[repo] = nil
			}
			sink := &mergeSink{}
			s.SetMergedPredecessorRecovery(q, sink)
			s.merged.cursors = map[uuid.UUID]uuid.UUID{sibling: mergeID(100), repo: mergeID(200)}
			s.merged.nextRepo = repo
			var phase *mergeDeadlineContext
			s.merged.withTimeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
				if d != 30*time.Second {
					t.Fatalf("phase budget=%v", d)
				}
				phase = &mergeDeadlineContext{Context: ctx}
				return phase, func() {}
			}
			q.afterEnumerate = func() {
				expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
				t.Cleanup(stop)
				phase.Context = expired
				if !errors.Is(phase.Err(), context.DeadlineExceeded) {
					t.Fatal("phase did not expire during enumeration")
				}
			}
			repos := []uuid.UUID{sibling, repo}
			s.ReconcileMergedPredecessors(context.Background(), repos)
			if len(reads) != 0 || sink.calls != 0 {
				t.Fatal("enumeration returned after cancellation was consumed")
			}
			if len(q.visits) != 1 || q.visits[0].RepoID != repo || q.visits[0].AfterRunID != mergeID(200) {
				t.Fatalf("expired phase visits=%v", q.visits)
			}
			if s.merged.nextRepo != repo {
				t.Errorf("expired enumeration consumed repository turn: nextRepo=%s want=%s", s.merged.nextRepo, repo)
			}
			if s.merged.cursors[repo] != mergeID(200) || s.merged.cursors[sibling] != mergeID(100) {
				t.Errorf("expired enumeration changed run cursors: %v", s.merged.cursors)
			}
			q.afterEnumerate = nil
			q.fail[repo] = false
			q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{mergeRow(repo, 201, token)}
			s.merged.withTimeout = nil
			q.visits = nil
			s.ReconcileMergedPredecessors(context.Background(), repos)
			if len(q.visits) == 0 || q.visits[0].RepoID != repo || q.visits[0].AfterRunID != mergeID(200) {
				t.Errorf("next phase did not first resume interrupted repository and run cursor: visits=%v", q.visits)
			}
			if !slices.Equal(reads, []int64{201, 101}) || sink.calls != 2 {
				t.Fatalf("candidates skipped or reordered after expired enumeration: reads=%v discards=%d", reads, sink.calls)
			}
		})
	}
}

type mergedRecoveryFunc func(context.Context, recovery.VerifiedMergeSnapshot) (int64, error)

func (fn mergedRecoveryFunc) DiscardMergedPredecessorHolds(ctx context.Context, snapshot recovery.VerifiedMergeSnapshot) (int64, error) {
	return fn(ctx, snapshot)
}

func (fn mergedRecoveryFunc) DiscardCrossRunHolds(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
	return 0, nil
}

func TestMergedPredecessorCandidatePanicIsolation(t *testing.T) {
	var output bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	const privatePayload = "private forge response contents"
	a, b := mergeID(1), mergeID(2)
	var panickedContext context.Context
	var reads []int64
	f := &mergeForge{read: func(ctx context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads = append(reads, iid)
		if iid == 201 {
			panickedContext = ctx
			panic(privatePayload)
		}
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}}
	s, token := mergeTestService(t, f)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{
		a: {mergeRow(a, 201, token)}, b: {mergeRow(b, 202, token)},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s.SetMergedPredecessorRecovery(q, mergedRecoveryFunc(func(context.Context, recovery.VerifiedMergeSnapshot) (int64, error) {
		calls++
		if calls == 1 {
			// Check before parent cancellation, so removing the observation defer fails.
			if ctx.Err() != nil || panickedContext == nil || panickedContext.Err() != context.Canceled {
				t.Error("panic failed to cancel its child while the parent remained live")
			}
			cancel() // End after the successful sibling, before exhaustion resets cursors.
		}
		return 1, nil
	}))
	s.ReconcileMergedPredecessors(ctx, []uuid.UUID{a, b})
	if !slices.Equal(reads, []int64{201, 202}) || calls != 1 ||
		s.merged.cursors[a] != mergeID(201) || s.merged.nextRepo != a {
		t.Fatalf("panic lost sibling/cursor progress: reads=%v calls=%d cursors=%v next=%s", reads, calls, s.merged.cursors, s.merged.nextRepo)
	}
	if panickedContext == nil || panickedContext.Err() != context.Canceled {
		t.Fatal("panic left the forge observation context alive")
	}
	if strings.Contains(output.String(), privatePayload) || !strings.Contains(output.String(), "candidate_panicked") {
		t.Fatalf("panic logging exposed payload or omitted fixed reason: %s", output.String())
	}
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b}) // Wrap for a later phase.
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b})
	if !slices.Equal(reads, []int64{201, 202, 201, 202}) || calls != 2 {
		t.Fatalf("next tick/wrap lost retry: reads=%v calls=%d", reads, calls)
	}
}

func TestMergedPredecessorRecoveryPanicContinuesSiblings(t *testing.T) {
	a, b := mergeID(1), mergeID(2)
	f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}}
	s, token := mergeTestService(t, f)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{
		a: {mergeRow(a, 201, token)}, b: {mergeRow(b, 202, token)},
	}}
	calls := 0
	s.SetMergedPredecessorRecovery(q, mergedRecoveryFunc(func(context.Context, recovery.VerifiedMergeSnapshot) (int64, error) {
		calls++
		if calls == 1 {
			panic("settlement collaborator failed")
		}
		return 1, nil
	}))
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b})
	if calls != 2 {
		t.Fatalf("recovery panic abandoned sibling: calls=%d", calls)
	}
}

func TestMergedPredecessorExactRepositoryAttemptsAcrossPhases(t *testing.T) {
	var reads []int64
	f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads = append(reads, iid)
		if iid <= 220 {
			return forge.MergeRequest{}, errors.New("persistent early failure")
		}
		return forge.MergeRequest{IID: iid, State: forge.MRStateOpened}, nil
	}}
	s, token := mergeTestService(t, f)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}}
	var repos []uuid.UUID
	for n := 1; n <= 120; n++ {
		repo := mergeID(n)
		repos = append(repos, repo)
		q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{mergeRow(repo, n+200, token)}
	}
	s.SetMergedPredecessorRecovery(q, &mergeSink{})
	s.ReconcileMergedPredecessors(context.Background(), repos)
	want := make([]int64, 100)
	for n := range want {
		want[n] = int64(n + 201)
	}
	if !slices.Equal(reads, want) {
		t.Fatalf("first phase attempts=%v", reads)
	}
	s.ReconcileMergedPredecessors(context.Background(), repos)
	for n := 301; n <= 320; n++ {
		want = append(want, int64(n))
	}
	if !slices.Equal(reads, want) {
		t.Fatalf("queued repositories starved or duplicated: attempts=%v", reads)
	}
	// Cursors wrap over bounded later phases, preserving the durable retry workset.
	for phase := 0; phase < 3; phase++ {
		s.ReconcileMergedPredecessors(context.Background(), repos)
	}
	retries := map[int64]int{}
	for _, iid := range reads {
		retries[iid]++
	}
	for iid := int64(201); iid <= 320; iid++ {
		if retries[iid] < 2 {
			t.Fatalf("repository candidate %d never retried: counts=%v", iid, retries)
		}
	}
}

func TestMergedPredecessorEnumerationRetryAndEmptyRepo(t *testing.T) {
	reads := 0
	f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}}
	s, token := mergeTestService(t, f)
	repo, empty := mergeID(1), mergeID(2)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {mergeRow(repo, 100, token)}}, fail: map[uuid.UUID]bool{repo: true}}
	sink := &mergeSink{}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo, empty})
	if reads != 0 || sink.calls != 0 {
		t.Fatal("failed or empty enumeration contacted forge")
	}
	q.fail[repo] = false
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo, empty})
	if reads != 1 || sink.calls != 1 {
		t.Fatal("enumeration failure lost candidate")
	}
}

func crossRunRow(repo uuid.UUID, n int, token []byte) store.ListMergedPredecessorCandidatesRow {
	row := mergeRow(repo, n, token)
	row.OriginalBranch = false
	row.Kind = "issue"
	row.IssueIid = pgtype.Int8{Int64: 77, Valid: true}
	row.CreatedAt = pgtype.Timestamptz{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	return row
}

func crossRunTarget(row store.ListMergedPredecessorCandidatesRow, n int) store.ListCrossRunTargetsRow {
	return store.ListCrossRunTargetsRow{
		RunID: mergeID(n), OwnerID: row.OwnerID, RepoID: pgtype.UUID{Bytes: row.RepoID, Valid: true},
		Kind: "issue", Status: "failed", Generation: 3, IssueIid: row.IssueIid,
		FinishedAt: pgtype.Timestamptz{Time: row.CreatedAt.Time.Add(-time.Hour), Valid: true},
		CreatedAt:  pgtype.Timestamptz{Time: row.CreatedAt.Time.Add(-2 * time.Hour), Valid: true},
	}
}

func TestCrossRunContinuationRefusalAndFreshObservation(t *testing.T) {
	for _, original := range []bool{false, true} {
		t.Run(fmt.Sprint(original), func(t *testing.T) {
			reads := 0
			f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				reads++
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}}
			s, token := mergeTestService(t, f)
			repo := mergeID(1)
			row := crossRunRow(repo, 100, token)
			row.OriginalBranch = original
			first, second := crossRunTarget(row, 90), crossRunTarget(row, 80)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {row}}}
			enumerations, calls := 0, 0
			q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
				enumerations++
				if reads != enumerations || p.WitnessRunID != row.RunID || p.OwnerID != row.OwnerID ||
					p.RepoID.Bytes != row.RepoID || p.IssueIid != row.IssueIid || p.WitnessCreatedAt != row.CreatedAt {
					t.Fatalf("target enumeration did not follow fresh exact B observation: reads=%d params=%+v", reads, p)
				}
				switch enumerations {
				case 1:
					if p.AfterCreatedAt.Valid || p.AfterRunID != uuid.Nil {
						t.Fatal("first key not empty")
					}
					return []store.ListCrossRunTargetsRow{first}, nil
				case 2:
					if p.AfterCreatedAt != first.CreatedAt || p.AfterRunID != first.RunID {
						t.Fatal("refusal lost immutable key")
					}
					return []store.ListCrossRunTargetsRow{second}, nil
				default:
					if p.AfterCreatedAt != second.CreatedAt || p.AfterRunID != second.RunID {
						t.Fatal("second key lost")
					}
					return nil, nil
				}
			}
			sink := &mergeSink{cross: func(_ context.Context, target recovery.CrossRunTargetBinding, witness recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
				calls++
				want := first
				if calls == 2 {
					want = second
				}
				if target.RunID != want.RunID || target.Generation != 3 || target.Status != "failed" ||
					target.FinishedAt != want.FinishedAt.Time || target.Validate(witness) != nil {
					t.Fatalf("target binding=%+v", target)
				}
				if s.merged.targets[repo].runID != want.RunID || s.merged.cursors[repo] != uuid.Nil {
					t.Fatal("continuation not saved before transaction")
				}
				return 0, errors.New("locked refusal")
			}}
			s.SetMergedPredecessorRecovery(q, sink)
			for phase := 1; phase <= 2; phase++ {
				s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
				if reads != phase || calls != phase || s.merged.cursors[repo] != uuid.Nil || len(s.merged.targets) != 1 {
					t.Fatal("target visit repeated or skipped B")
				}
			}
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			if calls != 2 || len(s.merged.targets) != 0 {
				t.Fatal("empty enumeration retained continuation")
			}
			wantLegacy := 0
			if original {
				wantLegacy = 3
			}
			if sink.calls != wantLegacy {
				t.Fatalf("legacy calls=%d want=%d", sink.calls, wantLegacy)
			}
		})
	}
}

func TestCrossRunEnumerationFailureRetainsTuple(t *testing.T) {
	reads := 0
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}})
	repo := mergeID(1)
	row := crossRunRow(repo, 100, token)
	target := crossRunTarget(row, 90)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {row}}}
	var keys []uuid.UUID
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		keys = append(keys, p.AfterRunID)
		if len(keys) == 1 {
			return []store.ListCrossRunTargetsRow{target}, nil
		}
		if p.AfterRunID != target.RunID || p.AfterCreatedAt != target.CreatedAt {
			t.Fatal("enumeration failure lost tuple")
		}
		return nil, errors.New("enumeration failed")
	}
	calls := 0
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		calls++
		return 0, nil
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	for phase := 0; phase < 3; phase++ {
		s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
	}
	if reads != 3 || calls != 1 || len(keys) != 3 || s.merged.targets[repo].runID != target.RunID {
		t.Fatal("failed enumeration consumed target continuation")
	}
	// Candidate enumeration failures also preserve the target and witness cursor.
	q.fail = map[uuid.UUID]bool{repo: true}
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
	if s.merged.targets[repo].runID != target.RunID || reads != 3 {
		t.Fatal("witness enumeration failure consumed continuation")
	}
	s.ReconcileMergedPredecessors(context.Background(), nil)
	if len(s.merged.targets) != 0 || len(s.merged.cursors) != 0 {
		t.Fatal("disabled repo retained continuation")
	}
}

func TestCrossRunUnverifiedAndVanishedWitnessClearTuple(t *testing.T) {
	for _, mode := range []string{"unmerged", "wrong_iid", "error", "vanished", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			state, wrong, fail := forge.MRStateMerged, false, false
			s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				if fail {
					return forge.MergeRequest{}, errors.New("unreadable")
				}
				if wrong {
					iid++
				}
				return forge.MergeRequest{IID: iid, State: state}, nil
			}})
			repo := mergeID(1)
			row := crossRunRow(repo, 100, token)
			target := crossRunTarget(row, 90)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {row}}}
			var keys []uuid.UUID
			q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
				keys = append(keys, p.AfterRunID)
				return []store.ListCrossRunTargetsRow{target}, nil
			}
			s.SetMergedPredecessorRecovery(q, &mergeSink{})
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			switch mode {
			case "unmerged":
				state = forge.MRStateOpened
			case "wrong_iid":
				wrong = true
			case "error":
				fail = true
			case "vanished":
				q.rows[repo] = nil
			case "replaced":
				q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{crossRunRow(repo, 101, token)}
			}
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			if mode == "replaced" {
				if len(keys) != 2 || keys[1] != uuid.Nil || s.merged.targets[repo].witness != mergeID(101) {
					t.Fatal("vanished B transferred target key to next witness")
				}
			} else if len(s.merged.targets) != 0 || len(keys) != 1 {
				t.Fatal("unverified or vanished B retained tuple")
			}
		})
	}
}

func TestCrossRunPanicKeepsTargetProgressAndSiblings(t *testing.T) {
	a, b := mergeID(1), mergeID(2)
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}})
	row := crossRunRow(a, 100, token)
	target := crossRunTarget(row, 90)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{a: {row}, b: {mergeRow(b, 200, token)}}}
	q.targets = func(context.Context, store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		return []store.ListCrossRunTargetsRow{target}, nil
	}
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		panic("private settlement")
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{a, b})
	if sink.calls != 1 || s.merged.targets[a].runID != target.RunID || s.merged.cursors[a] != uuid.Nil {
		t.Fatal("panic lost tuple or sibling")
	}
}

func TestCrossRunSharedAttemptCap(t *testing.T) {
	reads, targets := 0, 0
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}})
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}}
	var repos []uuid.UUID
	for n := 1; n <= 100; n++ {
		repo := mergeID(n)
		repos = append(repos, repo)
		row := crossRunRow(repo, n+200, token)
		row.OriginalBranch = n < 100
		q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{row}
	}
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		targets++
		return []store.ListCrossRunTargetsRow{crossRunTarget(q.rows[uuid.UUID(p.RepoID.Bytes)][0], 150)}, nil
	}
	crossCalls := 0
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		crossCalls++
		return 0, nil
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), repos)
	if reads != 50 || targets != 50 || sink.calls != 50 || crossCalls != 50 {
		t.Fatalf("shared attempt cap: reads=%d enumerations=%d legacy=%d cross=%d", reads, targets, sink.calls, crossCalls)
	}
}

func TestCrossRunUnmergedObservationsDoNotSpendRecoveryBudget(t *testing.T) {
	var observed []int64
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		observed = append(observed, iid)
		state := forge.MRStateMerged
		if iid <= 220 {
			state = forge.MRStateOpened
		}
		return forge.MergeRequest{IID: iid, State: state}, nil
	}})
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}}
	var repos []uuid.UUID
	for n := 1; n <= 100; n++ {
		repo := mergeID(n)
		repos = append(repos, repo)
		row := crossRunRow(repo, n+200, token)
		row.OriginalBranch = true
		q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{row}
	}
	enumerations, crossCalls := 0, 0
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		enumerations++
		return []store.ListCrossRunTargetsRow{crossRunTarget(q.rows[uuid.UUID(p.RepoID.Bytes)][0], 150)}, nil
	}
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		crossCalls++
		return 0, errors.New("permanent inventory refusal")
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), repos)
	// Twenty unmerged GETs spend visits but no transaction attempts. The next
	// fifty witnesses each use both branches, including the hundredth attempt.
	if len(observed) != 70 || sink.calls != 50 || crossCalls != 50 || enumerations != 50 {
		t.Fatalf("mixed budget GETs=%d legacy=%d cross=%d enumerations=%d; want 70/50/50/50", len(observed), sink.calls, crossCalls, enumerations)
	}
	for i, iid := range observed {
		if iid != int64(201+i) {
			t.Fatalf("first observation mismatch at %d: got %d want %d", i, iid, 201+i)
		}
	}
	if len(s.merged.targets) != 50 {
		t.Fatalf("continuation slots=%d want exactly one per visited merged repo (50)", len(s.merged.targets))
	}
	s.ReconcileMergedPredecessors(context.Background(), repos)
	if observed[70] != 271 {
		t.Fatalf("budget restart GET=%d want next repository PR 271", observed[70])
	}
}

func TestCrossRunAttemptCapDefersSecondTransaction(t *testing.T) {
	reads, crossCalls := 0, 0
	phase, resumed := 1, false
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}})
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{}}
	var repos []uuid.UUID
	for n := 1; n <= 51; n++ {
		repo := mergeID(n)
		repos = append(repos, repo)
		row := crossRunRow(repo, n+200, token)
		row.OriginalBranch = n != 1
		q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{row}
	}
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		if uuid.UUID(p.RepoID.Bytes) == repos[50] {
			if phase == 1 {
				t.Fatal("cross target visited beyond shared cap")
			}
			resumed = true
		}
		return []store.ListCrossRunTargetsRow{crossRunTarget(q.rows[uuid.UUID(p.RepoID.Bytes)][0], 150)}, nil
	}
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		crossCalls++
		return 0, nil
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), repos)
	continuation, pending := s.merged.targets[repos[50]]
	if reads != 51 || sink.calls != 50 || crossCalls != 50 || !pending || continuation.runID != uuid.Nil || s.merged.cursors[repos[50]] != uuid.Nil {
		t.Fatalf("cap did not retain unvisited cross target: reads=%d legacy=%d cross=%d continuation=%+v", reads, sink.calls, crossCalls, continuation)
	}
	// Permanently refused legacy holds continue to select the same witnesses.
	// Restart rotation at the deferred repository, or its legacy transaction
	// consumes the last attempt every phase and the cross-run target starves.
	sink.err = errors.New("legacy disposition refused")
	phase = 2
	s.ReconcileMergedPredecessors(context.Background(), repos)
	if !resumed {
		t.Fatal("deferred cross-run target starved after phase cap")
	}
}

func TestCrossRunTargetEnumerationCancellationRetainsTuple(t *testing.T) {
	for _, result := range []string{"rows", "error", "empty"} {
		t.Run(result, func(t *testing.T) {
			s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}})
			repo := mergeID(1)
			row := crossRunRow(repo, 100, token)
			first, second := crossRunTarget(row, 90), crossRunTarget(row, 80)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {row}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			enumerations, calls := 0, 0
			q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
				enumerations++
				if enumerations == 1 {
					return []store.ListCrossRunTargetsRow{first}, nil
				}
				if p.AfterRunID != first.RunID || p.AfterCreatedAt != first.CreatedAt {
					t.Fatal("cancelled enumeration consumed tuple")
				}
				if enumerations == 2 {
					cancel()
					if result == "error" {
						return nil, errors.New("enumeration")
					}
					if result == "empty" {
						return nil, nil
					}
				}
				return []store.ListCrossRunTargetsRow{second}, nil
			}
			sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
				calls++
				return 0, nil
			}}
			s.SetMergedPredecessorRecovery(q, sink)
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			s.ReconcileMergedPredecessors(ctx, []uuid.UUID{repo})
			if calls != 1 || s.merged.targets[repo].runID != first.RunID {
				t.Fatal("late target enumeration was consumed")
			}
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			if calls != 2 || s.merged.targets[repo].runID != second.RunID {
				t.Fatal("interrupted target not resumed")
			}
		})
	}
}

// A witness whose target enumeration keeps failing or running out of phase
// budget must not pin the repository cursor before it: a later legacy candidate
// is reached within a bounded number of phases, and B is rediscovered after the
// cursor wraps with an empty key and a fresh failure count.
func TestCrossRunPersistentEnumerationFailureYieldsWitness(t *testing.T) {
	for _, mode := range []string{"error", "deadline_before_query", "deadline_during_query"} {
		t.Run(mode, func(t *testing.T) {
			var reads []int64
			var phase *mergeDeadlineContext
			expire := func() {
				expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
				t.Cleanup(stop)
				phase.Context = expired
			}
			s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				reads = append(reads, iid)
				if mode == "deadline_before_query" && iid == 100 {
					expire()
				}
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}})
			repo := mergeID(1)
			witness := crossRunRow(repo, 100, token)
			q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {witness, mergeRow(repo, 200, token)}}}
			enumerations := 0
			q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
				enumerations++
				if p.AfterRunID != uuid.Nil || p.AfterCreatedAt.Valid {
					t.Fatal("failed enumeration advanced the target key")
				}
				if mode == "deadline_during_query" {
					expire()
					return []store.ListCrossRunTargetsRow{crossRunTarget(witness, 90)}, nil
				}
				return nil, errors.New("enumeration failed")
			}
			crossCalls := 0
			sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
				crossCalls++
				return 0, nil
			}}
			s.SetMergedPredecessorRecovery(q, sink)
			s.merged.withTimeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
				if d == 30*time.Second {
					phase = &mergeDeadlineContext{Context: ctx}
					return phase, func() {}
				}
				// Observations stay independent so only the phase budget expires.
				return context.WithTimeout(context.Background(), d)
			}
			// A fixed bound, not the constant: the limit itself is under test.
			phases := 0
			for phases < 4 && sink.calls == 0 {
				phases++
				s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			}
			if sink.calls != 1 || !slices.Contains(reads, 200) {
				t.Fatalf("later legacy candidate starved behind failing witness: phases=%d enumerations=%d reads=%v", phases, enumerations, reads)
			}
			// A pre-query deadline exit fails before any enumeration is issued.
			perFailure := 1
			if mode == "deadline_before_query" {
				perFailure = 0
			}
			if enumerations != perFailure*crossRunEnumerationFailureLimit || crossCalls != 0 {
				t.Fatalf("enumerations=%d cross=%d; want %d and no settlement", enumerations, crossCalls, perFailure*crossRunEnumerationFailureLimit)
			}
			if _, pending := s.merged.targets[repo]; pending {
				t.Fatal("yielded witness retained its continuation")
			}
			// The phase that reached the later candidate also wrapped the cursor,
			// so the next phase rediscovers B.
			s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
			if continuation := s.merged.targets[repo]; enumerations != perFailure*(crossRunEnumerationFailureLimit+1) ||
				continuation.witness != witness.RunID || continuation.failures != 1 {
				t.Fatalf("witness not rediscovered with a fresh count after wrap: enumerations=%d continuation=%+v", enumerations, continuation)
			}
		})
	}
}

// Transient failures keep the tuple, and a successful enumeration resets the
// consecutive-failure count, so interleaved failures never yield the witness.
func TestCrossRunEnumerationSuccessResetsFailureCount(t *testing.T) {
	s, token := mergeTestService(t, &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}})
	repo := mergeID(1)
	witness := crossRunRow(repo, 100, token)
	first, second := crossRunTarget(witness, 90), crossRunTarget(witness, 80)
	q := &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {witness, mergeRow(repo, 200, token)}}}
	results := []string{"fail", "fail", "first", "fail", "fail", "second"}
	enumerations := 0
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		if enumerations >= len(results) {
			t.Fatal("unexpected enumeration")
		}
		result := results[enumerations]
		enumerations++
		switch result {
		case "first":
			return []store.ListCrossRunTargetsRow{first}, nil
		case "second":
			if p.AfterRunID != first.RunID || p.AfterCreatedAt != first.CreatedAt {
				t.Fatal("transient failure lost the target tuple")
			}
			return []store.ListCrossRunTargetsRow{second}, nil
		}
		return nil, errors.New("enumeration failed")
	}
	crossCalls := 0
	sink := &mergeSink{cross: func(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		crossCalls++
		return 0, nil
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	for range results {
		s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
	}
	if crossCalls != 2 || sink.calls != 0 || s.merged.targets[repo].runID != second.RunID || s.merged.cursors[repo] != uuid.Nil {
		t.Fatalf("interleaved transient failures yielded the witness: cross=%d legacy=%d continuation=%+v", crossCalls, sink.calls, s.merged.targets[repo])
	}
}

func TestCrossRunRestartAndWitnessSnapshot(t *testing.T) {
	var q *mergeCandidates
	reads := 0
	f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
		reads++
		// Simulate B changing during forge I/O. The service must retain its prior
		// identity and recovery must compare that snapshot against its locked rows.
		if reads == 1 {
			for repo, rows := range q.rows {
				changed := rows[0]
				changed.Kind = "job"
				changed.IssueIid.Int64++
				changed.CreatedAt.Time = changed.CreatedAt.Time.Add(time.Hour)
				q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{changed}
			}
		}
		return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
	}}
	s, token := mergeTestService(t, f)
	repo := mergeID(1)
	row := crossRunRow(repo, 100, token)
	target := crossRunTarget(row, 90)
	q = &mergeCandidates{rows: map[uuid.UUID][]store.ListMergedPredecessorCandidatesRow{repo: {row}}}
	enumerations, calls := 0, 0
	q.targets = func(_ context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
		enumerations++
		if p.IssueIid != row.IssueIid || p.WitnessCreatedAt != row.CreatedAt || p.AfterRunID != uuid.Nil || p.AfterCreatedAt.Valid {
			t.Fatal("witness reconstructed or restart reused key")
		}
		return []store.ListCrossRunTargetsRow{target}, nil
	}
	sink := &mergeSink{cross: func(_ context.Context, target recovery.CrossRunTargetBinding, witness recovery.VerifiedCrossRunWitnessSnapshot) (int64, error) {
		calls++
		if err := target.Validate(witness); err != nil {
			t.Fatalf("lost pre-I/O witness: %v", err)
		}
		return 0, errors.New("refused")
	}}
	s.SetMergedPredecessorRecovery(q, sink)
	s.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
	q.rows[repo] = []store.ListMergedPredecessorCandidatesRow{row}
	restarted, _ := mergeTestService(t, f)
	restarted.SetMergedPredecessorRecovery(q, sink)
	restarted.ReconcileMergedPredecessors(context.Background(), []uuid.UUID{repo})
	if calls != 2 || reads != 2 || enumerations != 2 {
		t.Fatal("restart lost durable OPEN workset")
	}
}
