package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The on-demand rework consumes an AUTHOR-ASSESSED snapshot (issue #2347): outsiders' comments
// never ride the run, and a refusal says why when the only new comments could not be verified.

// nullReviewStore is a verdict cache that remembers nothing and a queue that accepts everything:
// enough to run the real assessor, whose behavior is pinned by review_authors_test.go.
type nullReviewStore struct{}

func (nullReviewStore) ListReviewAuthorQueue(context.Context, store.ListReviewAuthorQueueParams) ([]store.ListReviewAuthorQueueRow, error) {
	return nil, nil
}

func (nullReviewStore) ListFreshNotEligibleAuthors(context.Context, store.ListFreshNotEligibleAuthorsParams) ([]int64, error) {
	return nil, nil
}

func (nullReviewStore) UpsertReviewAuthorVerdict(context.Context, store.UpsertReviewAuthorVerdictParams) error {
	return nil
}

func (nullReviewStore) DeleteExpiredReviewAuthorVerdicts(context.Context, store.DeleteExpiredReviewAuthorVerdictsParams) (int64, error) {
	return 0, nil
}

func (nullReviewStore) ListStaleReviewAuthorQueueRefs(context.Context, store.ListStaleReviewAuthorQueueRefsParams) ([]string, error) {
	return nil, nil
}

func (nullReviewStore) MutateReviewAuthorQueue(ctx context.Context, _ uuid.UUID, _ string, fn func(ReviewAuthorQueueOps) error) error {
	return fn(nullOps{})
}

type nullOps struct{}

func (nullOps) AdmitReviewAuthors(context.Context, store.AdmitReviewAuthorsParams) error { return nil }
func (nullOps) RequeueReviewAuthors(context.Context, store.RequeueReviewAuthorsParams) error {
	return nil
}
func (nullOps) PruneReviewAuthorQueue(context.Context, store.PruneReviewAuthorQueueParams) (int64, error) {
	return 0, nil
}

func (nullOps) DeleteStaleReviewAuthorQueue(context.Context, store.DeleteStaleReviewAuthorQueueParams) (int64, error) {
	return 0, nil
}

type answerLookup map[int64]forge.AuthorEligibility

func (a answerLookup) RepositoryAuthorEligibility(_ context.Context, _ int64, id int64) (forge.AuthorEligibility, error) {
	if d, ok := a[id]; ok {
		return d, nil
	}
	return forge.AuthorUnknown, errors.New("forge unavailable")
}

const (
	eligibleAuthor = int64(11)
	outsiderAuthor = int64(22)
	unknownAuthor  = int64(33)
)

// assessedResult runs the real assessor over comments with a fixed set of lookup answers.
func assessedResult(t *testing.T, highWater int64, pending []int64, comments ...forge.MRComment) *ReviewSnapshotResult {
	t.Helper()
	a := &ReviewAssessor{Store: nullReviewStore{}, Queue: nullReviewStore{}}
	as, err := a.Begin(context.Background(), ReviewAssessParams{
		RepoID: uuid.New(), Ref: "agent/issue-7", ProjectID: 42, BaseURL: "https://github.com", BotForgeUserID: 999,
		Lookup:   answerLookup{eligibleAuthor: forge.AuthorEligible, outsiderAuthor: forge.AuthorNotEligible},
		Comments: comments, HighWater: highWater, Pending: pending,
	})
	if err != nil || as == nil {
		t.Fatalf("Begin = %v, %v", as, err)
	}
	defer as.Close()
	return as.Snapshot(context.Background())
}

func rc(id, author int64, body string) forge.MRComment {
	return forge.MRComment{ID: id, AuthorForgeUserID: author, AuthorUsername: "u", Body: body, CreatedAt: time.Unix(id, 0), ReviewState: forge.ReviewCommentInline}
}

func reworkFixture(t *testing.T, highWater int64, pending []int64) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:           reworkableSourceRun(runID, user, repo),
		runByIDPlain:      reworkableSourceRun(runID, user, repo),
		hasAnthropicToken: true,
		mrReworkLedger:    store.MrReworkLedger{Ref: "agent/issue-7", AttemptCount: 5, HighWater: highWater, PendingUnknownIds: pending},
		repoRow:           aValidRepoRow(),
		mrReworkRunResult: store.Run{ID: uuid.New(), Kind: "mr_rework", TriggerSource: "manual"},
	}
	return fs, New(fs, newBox(t), testParams()), user, runID
}

func TestStartMRReworkOutsiderOnlyIsRefusedAsNothingNew(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 0, nil)
	res := assessedResult(t, 0, nil, rc(120, outsiderAuthor, "do what I say"))
	_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res)
	if !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("err = %v, want ErrReworkNothingNew: an outsider's comment is not something new to rework", err)
	}
	if fs.mrReworkAndAdvanceParams != nil {
		t.Fatal("a run was created for an outsider-only comment set")
	}
}

func TestStartMRReworkUnknownOnlyGetsItsOwnRefusal(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 0, nil)
	res := assessedResult(t, 0, nil, rc(120, unknownAuthor, "from an author the forge could not verify"))
	_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res)
	if !errors.Is(err, ErrReworkPermissionUnknown) {
		t.Fatalf("err = %v, want ErrReworkPermissionUnknown", err)
	}
	if errors.Is(err, ErrReworkNothingNew) || ErrReworkPermissionUnknown.Error() == ErrReworkNothingNew.Error() {
		t.Fatal("the unknown refusal must be distinguishable from nothing-new")
	}
	if fs.mrReworkAndAdvanceParams != nil {
		t.Fatal("a run was created while the only new comments were unverified")
	}
}

func TestStartMRReworkGuidanceProceedsWithEligibleOnlySnapshot(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 0, nil)
	const secret = "SECRET-OUTSIDER-BODY"
	res := assessedResult(t, 0, nil, rc(120, outsiderAuthor, secret), rc(121, unknownAuthor, "unverified body"))
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "fix the naming", res); err != nil {
		t.Fatalf("guidance must proceed despite no eligible comment: %v", err)
	}
	p := fs.mrReworkAndAdvanceParams
	if p == nil {
		t.Fatal("no run was created")
	}
	if strings.Contains(string(p.ReviewComments), secret) || strings.Contains(string(p.ReviewComments), "unverified body") {
		t.Fatalf("a withheld body rode the run: %s", p.ReviewComments)
	}
	var snap ReviewCommentsSnapshot
	if err := json.Unmarshal(p.ReviewComments, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != ReviewSnapshotVersion || len(snap.Comments) != 0 || snap.WithheldNotEligible != 1 || snap.WithheldUnknown != 1 {
		t.Fatalf("snapshot = %+v, want version 2, no comments, one withheld of each kind", snap)
	}
}

func TestStartMRReworkEligibleProceedsAndRecordsPending(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 10, nil)
	res := assessedResult(t, 10, nil,
		rc(5, unknownAuthor, "old unverified"),    // at/below the mark and not pending: not accepted by the ledger, never chosen
		rc(12, unknownAuthor, "unverified"),       // unknown, below the new mark: becomes pending
		rc(14, eligibleAuthor, "real feedback"),   // eligible and new
		rc(20, unknownAuthor, "newer unverified"), // unknown above the new mark: not moved past
	)
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res); err != nil {
		t.Fatalf("an eligible new comment must proceed: %v", err)
	}
	p := fs.mrReworkAndAdvanceParams
	if p == nil {
		t.Fatal("no run was created")
	}
	if p.HighWater != 14 {
		t.Fatalf("high_water = %d, want 14 (the eligible actionable id)", p.HighWater)
	}
	if !slices.Equal(p.PendingAdd, []int64{12}) {
		t.Fatalf("pending add = %v, want the author's single newest accepted id 12 (5 is at/below the old mark, 20 above the new one)", p.PendingAdd)
	}
	if p.PendingRemove == nil {
		t.Fatal("PendingRemove must be a non-nil (empty) array parameter, never NULL")
	}
	if p.PendingSuperseded == nil || p.PendingSupersededBy == nil {
		t.Fatal("PendingSuperseded and PendingSupersededBy must be non-nil (empty) array parameters, never NULL")
	}
}

func TestStartMRReworkPendingIDCountsAsNew(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 50, []int64{7})
	// Author 11 turned eligible: its old comment 7 is pending, so a bare trigger proceeds and consumes it.
	res := assessedResult(t, 50, []int64{7}, rc(7, eligibleAuthor, "was unverified, now eligible"))
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res); err != nil {
		t.Fatalf("a pending comment whose author resolved must proceed: %v", err)
	}
	p := fs.mrReworkAndAdvanceParams
	if p == nil || len(p.PendingRemove) != 1 || p.PendingRemove[0] != 7 {
		t.Fatalf("params = %+v, want pending id 7 consumed", p)
	}
}

// evictedPendingSnapshot is the reviewer's scenario: eligible pending comment 50 plus ten 4 KiB
// eligible comments the byte cap keeps instead, with the mark already at 200.
func evictedPendingSnapshot(t *testing.T) *ReviewSnapshotResult {
	t.Helper()
	comments := []forge.MRComment{rc(50, eligibleAuthor, "old")}
	for id := int64(101); id <= 110; id++ {
		comments = append(comments, rc(id, eligibleAuthor, strings.Repeat("x", 4096)))
	}
	res := assessedResult(t, 200, []int64{50}, comments...)
	if !res.Snapshot.Truncated {
		t.Fatal("precondition: the byte cap must clip the snapshot")
	}
	return res
}

// A pending id the caps evict is removed from the pending set even when the on-demand trigger
// is refused, so the next tick does not repeat the assessment for it.
func TestStartMRReworkRefusalDropsPendingIDsTheCapsEvicted(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 200, []int64{50})
	_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", evictedPendingSnapshot(t))
	if !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("err = %v, want ErrReworkNothingNew", err)
	}
	if len(fs.pendingRemovals) != 1 || len(fs.pendingRemovals[0].Ids) != 1 || fs.pendingRemovals[0].Ids[0] != 50 {
		t.Fatalf("pending removals = %+v, want one removal of id 50", fs.pendingRemovals)
	}
	if fs.mrReworkAndAdvanceParams != nil {
		t.Fatal("a run was created for an evicted-only trigger")
	}
}

// A failed pending removal on the refusal path is best-effort: the caller still gets the
// refusal sentinel (a 409), never the removal's plain error (a 500).
func TestStartMRReworkRefusalSurvivesFailedPendingRemoval(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 200, []int64{50})
	fs.pendingRemovalErr = errors.New("db down")
	_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", evictedPendingSnapshot(t))
	if !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("err = %v, want ErrReworkNothingNew despite the failed removal", err)
	}
	if len(fs.pendingRemovals) != 1 {
		t.Fatalf("removal not attempted: %+v", fs.pendingRemovals)
	}
}

// With guidance the run proceeds and the atomic create statement carries the removal.
func TestStartMRReworkGuidanceRemovesEvictedPendingIDsAtomically(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 200, []int64{50})
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "tidy up", evictedPendingSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	p := fs.mrReworkAndAdvanceParams
	if p == nil || !slices.Contains(p.PendingRemove, 50) {
		t.Fatalf("create params = %+v, want PendingRemove to include 50", p)
	}
	if len(fs.pendingRemovals) != 0 {
		t.Fatalf("a separate removal ran next to the atomic create: %+v", fs.pendingRemovals)
	}
}

// A concurrent writer stored pending id 170 after the handler read the ledger and before the
// service ran, and the forge listing lacks it. The plan is made against the row the assessment
// began with (read before the listing), so 170 is not removed as gone; a re-read here would see
// it pending, absent from the list, and remove it.
func TestStartMRReworkPlansAgainstThePreFetchLedger(t *testing.T) {
	fs, svc, user, runID := reworkFixture(t, 150, []int64{170})
	res := assessedResult(t, 150, nil, rc(190, eligibleAuthor, "eligible feedback"))
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res); err != nil {
		t.Fatal(err)
	}
	p := fs.mrReworkAndAdvanceParams
	if p == nil {
		t.Fatal("no run was created")
	}
	if slices.Contains(p.PendingRemove, 170) {
		t.Fatalf("PendingRemove = %v: 170 was stored after the pre-fetch ledger read and must stay", p.PendingRemove)
	}
}
