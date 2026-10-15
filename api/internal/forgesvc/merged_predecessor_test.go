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
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type mergeCandidates struct {
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

type mergeSink struct {
	calls int
	err   error
}

func (r *mergeSink) DiscardMergedPredecessorHolds(context.Context, recovery.VerifiedMergeSnapshot) (int64, error) {
	r.calls++
	return 1, r.err
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
	return store.ListMergedPredecessorCandidatesRow{RunID: mergeID(n), OwnerID: mergeID(9000), RepoID: repo, ConnectionID: mergeID(9001), ProjectID: 42, MrIid: int64(n), CompletedGeneration: 2, ForgeType: "gitlab", BaseUrl: "https://forge.test", TokenCiphertext: token}
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
	for _, kind := range []string{"", "unknown", "opened", "closed", "locked", "wrong_iid", "unreadable", "bad_token", "discard_error", "merged"} {
		t.Run(kind, func(t *testing.T) {
			f := &mergeForge{read: func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				if kind == "unreadable" {
					return forge.MergeRequest{}, errors.New("secret must not be logged")
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
