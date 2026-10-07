package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/reviewauthortest"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Shared identity for the fake repo/owner/MR every mr_rework test detects against.
var (
	mrwRepoID      = uuid.New()
	mrwUserID      = uuid.New()
	mrwSourceRunID = uuid.New()
)

const (
	mrwRef     = "agent/issue-7"
	mrwHeadSHA = "headsha01"
	mrwMrIID   = int64(55)
	mrwBotID   = int64(999)
)

// ── fakes ────────────────────────────────────────────────────────────────────

type mrwStore struct {
	candidates []store.ListMRReworkCandidatesRow
	candErr    error

	ledgers map[string]store.MrReworkLedger
	getErr  error

	upserts []store.UpsertMRReworkLedgerParams
	// pendingRemovals records the pending-only removals (no run created).
	pendingRemovals []store.RemoveMRReworkPendingIDsParams
	haltSets        []store.SetMRReworkHaltNotifiedParams
	evicts          []uuid.UUID
	haltErr         error

	ops *[]string

	// clock feeds the review-author model (admitted_at / last_attempt_at); nil means time.Now.
	clock func() time.Time
	rat   *reviewauthortest.Store
}

// ras is the in-memory review-author verdict cache and queue (issue #2347), created on first
// use so the many tests that build a bare &mrwStore{} need no setup.
func (s *mrwStore) ras() *reviewauthortest.Store {
	if s.rat == nil {
		s.rat = reviewauthortest.New(s.clock)
	}
	return s.rat
}

func (s *mrwStore) ListReviewAuthorQueue(ctx context.Context, arg store.ListReviewAuthorQueueParams) ([]store.ListReviewAuthorQueueRow, error) {
	return s.ras().ListReviewAuthorQueue(ctx, arg)
}

func (s *mrwStore) ListFreshNotEligibleAuthors(ctx context.Context, arg store.ListFreshNotEligibleAuthorsParams) ([]int64, error) {
	return s.ras().ListFreshNotEligibleAuthors(ctx, arg)
}

func (s *mrwStore) UpsertReviewAuthorVerdict(ctx context.Context, arg store.UpsertReviewAuthorVerdictParams) error {
	return s.ras().UpsertReviewAuthorVerdict(ctx, arg)
}

func (s *mrwStore) DeleteExpiredReviewAuthorVerdicts(ctx context.Context, arg store.DeleteExpiredReviewAuthorVerdictsParams) (int64, error) {
	return s.ras().DeleteExpiredReviewAuthorVerdicts(ctx, arg)
}

func (s *mrwStore) ListStaleReviewAuthorQueueRefs(ctx context.Context, arg store.ListStaleReviewAuthorQueueRefsParams) ([]string, error) {
	return s.ras().ListStaleReviewAuthorQueueRefs(ctx, arg)
}

func (s *mrwStore) ListMRReworkCandidates(context.Context, uuid.UUID) ([]store.ListMRReworkCandidatesRow, error) {
	return s.candidates, s.candErr
}

func (s *mrwStore) GetMRReworkLedger(_ context.Context, arg store.GetMRReworkLedgerParams) (store.MrReworkLedger, error) {
	if s.getErr != nil {
		return store.MrReworkLedger{}, s.getErr
	}
	if l, ok := s.ledgers[arg.Ref]; ok {
		return l, nil
	}
	// Mirror the generated :one — a zero-value row alongside ErrNoRows.
	return store.MrReworkLedger{}, pgx.ErrNoRows
}

// applyUpsert mirrors UpsertMRReworkLedger. The detector no longer writes the ledger itself
// (the create does, in its own transaction), so only the mrwRuns fake reaches it.
func (s *mrwStore) applyUpsert(arg store.UpsertMRReworkLedgerParams) {
	s.upserts = append(s.upserts, arg)
	if s.ledgers == nil {
		s.ledgers = map[string]store.MrReworkLedger{}
	}
	cur := s.ledgers[arg.Ref]
	priorHighWater := cur.HighWater
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.AttemptCount++ // INSERT count=1 or increment
	if arg.HighWater > cur.HighWater {
		cur.HighWater = arg.HighWater // GREATEST(existing, new): advance-only
	}
	cur.HaltNotified = false // a proceed resets the latch
	cur.PendingUnknownIds = mergePending(cur.PendingUnknownIds, arg.PendingAdd, arg.PendingRemove, arg.PendingSuperseded, arg.PendingSupersededBy, priorHighWater)
	s.ledgers[arg.Ref] = cur
}

// mergePending mirrors mr_rework_merge_pending: (existing UNION added ids above the prior
// high-water mark) minus removed ids; each (superseded, superseded-by) pair applies only when
// both ids are in that set, dropping the older id unless it is itself a replacement; then the
// OLDEST ReviewPendingCap slots are kept, a replacement taking the slot of the smallest id it
// replaces. Ascending.
func mergePending(existing, added, removed, superseded, supersededBy []int64, priorHighWater int64) []int64 {
	base := map[int64]bool{}
	for _, id := range existing {
		base[id] = true
	}
	for _, id := range added {
		if id > priorHighWater {
			base[id] = true
		}
	}
	for _, id := range removed {
		delete(base, id)
	}
	dropped := map[int64]bool{}
	replacement := map[int64]bool{}
	slot := map[int64]int64{}
	for i, o := range superseded {
		if i >= len(supersededBy) {
			break
		}
		n := supersededBy[i]
		if o == n || !base[o] || !base[n] {
			continue
		}
		dropped[o] = true
		replacement[n] = true
		if cur, ok := slot[n]; !ok || o < cur {
			slot[n] = o
		}
	}
	for n := range replacement {
		delete(dropped, n)
	}
	type kept struct{ x, slot int64 }
	var ks []kept
	for x := range base {
		if dropped[x] {
			continue
		}
		sl := x
		if s, ok := slot[x]; ok {
			sl = min(s, x)
		}
		ks = append(ks, kept{x, sl})
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].slot != ks[j].slot {
			return ks[i].slot < ks[j].slot
		}
		return ks[i].x < ks[j].x
	})
	if len(ks) > workersvc.ReviewPendingCap {
		ks = ks[:workersvc.ReviewPendingCap]
	}
	out := make([]int64, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RemoveMRReworkPendingIDs mirrors the SQL: array subtraction on the pending set of an
// existing row, leaving every other column alone.
func (s *mrwStore) RemoveMRReworkPendingIDs(_ context.Context, arg store.RemoveMRReworkPendingIDsParams) error {
	cur, ok := s.ledgers[arg.Ref]
	if !ok {
		return nil
	}
	s.pendingRemovals = append(s.pendingRemovals, arg)
	drop := map[int64]bool{}
	for _, id := range arg.Ids {
		drop[id] = true
	}
	kept := []int64{}
	for _, id := range cur.PendingUnknownIds {
		if !drop[id] {
			kept = append(kept, id)
		}
	}
	cur.PendingUnknownIds = kept
	s.ledgers[arg.Ref] = cur
	return nil
}

func (s *mrwStore) SetMRReworkHaltNotified(_ context.Context, arg store.SetMRReworkHaltNotifiedParams) error {
	if s.haltErr != nil {
		return s.haltErr
	}
	s.haltSets = append(s.haltSets, arg)
	if s.ledgers == nil {
		s.ledgers = map[string]store.MrReworkLedger{}
	}
	cur := s.ledgers[arg.Ref]
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.HaltNotified = true
	s.ledgers[arg.Ref] = cur
	if s.ops != nil {
		*s.ops = append(*s.ops, "halt")
	}
	return nil
}

func (s *mrwStore) DeleteMRReworkLedgerNotIn(_ context.Context, repoID uuid.UUID) (int64, error) {
	s.evicts = append(s.evicts, repoID)
	return 0, nil
}

type mrwRunCall struct {
	userID, repoID, sourceRunID uuid.UUID
	ref                         string
	mrIID                       int64
	title, desc                 string
	snapshot                    *workersvc.ReviewCommentsSnapshot
	capLimit                    int
	plan                        workersvc.ReviewPlan // the advance that rode the create
}

type mrwRuns struct {
	err   error
	calls []mrwRunCall
	runID uuid.UUID
	ops   *[]string
	// st is the ledger the fake create re-validates and advances, standing in for
	// workersvc's in-transaction recheck and upsert; newMRW binds it.
	st *mrwStore
}

func (r *mrwRuns) CreateAutoMRReworkRunAndAdvance(_ context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, desc string, res *workersvc.ReviewSnapshotResult, capLimit int) (store.Run, error) {
	plan := res.PlanAssessed()
	r.calls = append(r.calls, mrwRunCall{userID, repoID, sourceRunID, ref, mrIID, title, desc, res.Snapshot, capLimit, plan})
	if r.ops != nil {
		*r.ops = append(*r.ops, "create")
	}
	if r.err != nil {
		return store.Run{}, r.err
	}
	if r.st != nil {
		// The same re-validation the real create runs under the branch lock.
		cur := r.st.ledgers[ref]
		if int(cur.AttemptCount) >= capLimit {
			return store.Run{}, workersvc.ErrMRReworkCapReached
		}
		if !res.HasNewAgainst(cur.HighWater, cur.PendingUnknownIds) {
			return store.Run{}, workersvc.ErrReworkNothingNew
		}
		r.st.applyUpsert(store.UpsertMRReworkLedgerParams{
			RepoID:              repoID,
			Ref:                 ref,
			HighWater:           plan.MaxActionableID,
			PendingAdd:          plan.PendingAdd,
			PendingRemove:       plan.PendingRemove,
			PendingSuperseded:   plan.PendingSuperseded,
			PendingSupersededBy: plan.PendingSupersededBy,
		})
	}
	if r.runID == (uuid.UUID{}) {
		r.runID = uuid.New()
	}
	return store.Run{ID: r.runID}, nil
}

type mrwNotifyCall struct {
	kind    string
	userID  uuid.UUID
	runID   *uuid.UUID
	payload notifysvc.CIAutofixPayload
	slack   *notifysvc.SlackRender
	durable bool // Notification.DurableSlack (issue #1675)
}

type mrwNotifier struct {
	calls []mrwNotifyCall
	ops   *[]string
	err   error
}

func (n *mrwNotifier) Notify(_ context.Context, note notifysvc.Notification) (store.Notification, error) {
	p, _ := note.Payload.(notifysvc.CIAutofixPayload)
	n.calls = append(n.calls, mrwNotifyCall{note.Kind, note.UserID, note.RunID, p, note.Slack, note.DurableSlack})
	if n.ops != nil {
		*n.ops = append(*n.ops, "notify")
	}
	if n.err != nil {
		return store.Notification{}, n.err
	}
	return store.Notification{ID: uuid.New()}, nil
}

type mrwSettings struct {
	enabled    bool
	enabledErr error
	capVal     int
	capErr     error
	baseURL    string
	baseErr    error
	bots       []settings.TrustedBot
	botsErr    error
}

func (s mrwSettings) MrReviewTrustedBots(context.Context) ([]settings.TrustedBot, error) {
	return s.bots, s.botsErr
}

func (s mrwSettings) MrReworkEnabled(context.Context) (bool, error) { return s.enabled, s.enabledErr }
func (s mrwSettings) MrReworkCap(context.Context) (int, error)      { return s.capVal, s.capErr }
func (s mrwSettings) PublicBaseURL(context.Context) (string, error) { return s.baseURL, s.baseErr }

// mrwForge embeds the CI-autofix test forge (which already satisfies forge.Forge and
// captures issue-note posts in .notes) and overrides only the MR-comment read.
type mrwForge struct {
	*cfForge
	comments    []forge.MRComment
	commentsErr error

	// eligibility answers a repository-access lookup (issue #2347). Nil means every author is
	// eligible, so the pre-existing tests keep their meaning; lookups records each call.
	eligibility func(ctx context.Context, authorID int64) (forge.AuthorEligibility, error)
	lookups     []int64

	// onList, when set, runs while ListMergeRequestComments is "fetching": it stands for a
	// concurrent writer that updates the ledger during the listing.
	onList func()
	// listCalls counts ListMergeRequestComments calls.
	listCalls int
}

func (f *mrwForge) RepositoryAuthorEligibility(ctx context.Context, _ int64, authorID int64) (forge.AuthorEligibility, error) {
	f.lookups = append(f.lookups, authorID)
	if f.eligibility == nil {
		return forge.AuthorEligible, nil
	}
	return f.eligibility(ctx, authorID)
}

func (f *mrwForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	f.listCalls++
	if f.onList != nil {
		f.onList()
	}
	return f.comments, f.commentsErr
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mrwRepoRow() store.ListEnabledReposWithConnectionsRow {
	return store.ListEnabledReposWithConnectionsRow{
		ID:                mrwRepoID,
		ForgeProjectID:    42,
		PathWithNamespace: "grp/proj",
	}
}

// mrwCand builds a candidate with a GREEN head pipeline whose SHA is mrwHeadSHA.
// status overrides the pipeline status ("" → a candidate with no cached pipeline).
func mrwCand(status string) store.ListMRReworkCandidatesRow {
	c := store.ListMRReworkCandidatesRow{
		Ref:            pgtype.Text{String: mrwRef, Valid: true},
		MrIid:          pgtype.Int8{Int64: mrwMrIID, Valid: true},
		UserID:         mrwUserID,
		SourceRunID:    mrwSourceRunID,
		BotForgeUserID: mrwBotID,
		PipelineID:     pgtype.Int8{Int64: 7001, Valid: true},
		PipelineSha:    pgtype.Text{String: mrwHeadSHA, Valid: true},
		PipelineWebUrl: pgtype.Text{String: "https://forge/grp/proj/-/pipelines/7001", Valid: true},
	}
	if status != "" {
		c.PipelineStatus = pgtype.Text{String: status, Valid: true}
	}
	return c
}

// mrwComment is a human review comment (author != the bot) written against headSHA.
func mrwComment(id int64, created time.Time, headSHA string) forge.MRComment {
	return forge.MRComment{
		ID:                id,
		AuthorForgeUserID: 12, // a human, not the connection bot
		AuthorUsername:    "human",
		Body:              "please tighten this",
		CreatedAt:         created,
		HeadSHA:           headSHA,
		ReviewState:       forge.ReviewCommentInline,
	}
}

func newMRW(st *mrwStore, runs *mrwRuns, notifier *mrwNotifier, set mrwSettings) *MRReviewWatch {
	var n MRReviewNotifier
	if notifier != nil {
		n = notifier
	}
	runs.st = st
	return NewMRReviewWatch(st, runs, st.ras(), n, set, 5, 5*time.Minute)
}

func landedForge(comments ...forge.MRComment) *mrwForge {
	return &mrwForge{cfForge: &cfForge{}, comments: comments}
}

// A comment old enough to have "landed" (past the 5-minute quiet period).
func landed() time.Time { return time.Now().Add(-30 * time.Minute) }

// ── tests ────────────────────────────────────────────────────────────────────

func TestMRReworkProceedStartsRun(t *testing.T) {
	var ops []string
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}, ops: &ops}
	runs := &mrwRuns{ops: &ops}
	notifier := &mrwNotifier{ops: &ops}
	// Two kept comments; max id 120 > high_water 0 → fire, advance to 120.
	f := landedForge(mrwComment(100, landed(), mrwHeadSHA), mrwComment(120, landed(), mrwHeadSHA))

	newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("CreateAutoMRReworkRun calls = %d, want 1", len(runs.calls))
	}
	c := runs.calls[0]
	if c.userID != mrwUserID || c.repoID != mrwRepoID || c.ref != mrwRef || c.mrIID != mrwMrIID || c.sourceRunID != mrwSourceRunID {
		t.Fatalf("run call = %+v, want owner/repo/ref/mr/source", c)
	}
	if c.snapshot == nil || len(c.snapshot.Comments) != 2 {
		t.Fatalf("expected the built review snapshot to ride the create, got %+v", c.snapshot)
	}
	if len(st.upserts) != 1 || st.upserts[0].HighWater != 120 {
		t.Fatalf("expected one ledger upsert advancing high_water to 120, got %+v", st.upserts)
	}
	if got := st.ledgers[mrwRef].AttemptCount; got != 1 {
		t.Fatalf("attempt_count = %d, want 1", got)
	}
	// The advance rides the create (one transaction in the real service): the poller makes no
	// ledger write of its own after it, and the create carries the plan and the cap.
	if strings.Join(ops, ",") != "create" {
		t.Fatalf("op order = %v, want [create] only", ops)
	}
	if c.capLimit != 5 || c.plan.MaxActionableID != 120 {
		t.Fatalf("create carried capLimit=%d plan.MaxActionableID=%d, want 5/120", c.capLimit, c.plan.MaxActionableID)
	}
	if len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("a proceed posts no halt comment/notify, got notes=%d notifs=%d", len(f.notes), len(notifier.calls))
	}
}

func TestMRReworkPipelineRedNoFire(t *testing.T) {
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("failed")}}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(120, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a red head pipeline must not fire: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
}

func TestMRReworkNoCachedPipelineNoFire(t *testing.T) {
	// A candidate whose branch has no cached pipeline row (LEFT JOIN → NULL status).
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("")}}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(120, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("an absent head pipeline must not fire, got %d runs", len(runs.calls))
	}
}

func TestMRReworkReviewNotLandedYoungComment(t *testing.T) {
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	// Comment created NOW → inside the 5-minute quiet period → not landed.
	f := landedForge(mrwComment(120, time.Now(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a comment still inside the quiet period must not fire, got %d runs", len(runs.calls))
	}
}

func TestMRReworkStaleHeadSHANoFire(t *testing.T) {
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	// Landed, but written against a SUPERSEDED head SHA (!= the green pipeline's SHA).
	f := landedForge(mrwComment(120, landed(), "oldsha00"))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a comment on a stale head SHA must not fire, got %d runs", len(runs.calls))
	}
}

func TestMRReworkCommentAtOrBelowHighWaterNoFire(t *testing.T) {
	// high_water already at the max kept id (120): SC3 — a comment at/below the mark is
	// not re-acted.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 1, HighWater: 120}},
	}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(100, landed(), mrwHeadSHA), mrwComment(120, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 {
		t.Fatalf("no comment past the high-water must not fire: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
}

// TestMRReworkPostManualLedgerStateNoRefire proves the automatic watcher's view of the
// ledger AFTER an on-demand (manual) cycle (PRD #1202): the manual path advanced high_water
// to its max actionable id, left attempt_count at the cap, and reset halt_notified=false.
// On the SAME comment set the automatic detector does NOT re-fire and does NOT re-halt
// (nothing is past the mark); only a genuinely-new comment re-halts, and because the manual
// cycle reset the latch it comments once more (Decision 9). The poller is not in the live-DB
// sweep, so this exercises the detector directly against the seeded ledger — it never calls
// StartMRReworkForRun.
func TestMRReworkPostManualLedgerStateNoRefire(t *testing.T) {
	const cap = 5
	// Post-manual ledger: high_water = the max actionable id the manual cycle consumed (200),
	// attempt_count still at the cap (the manual cycle did NOT spend an automatic one), latch
	// reset.
	postManual := store.MrReworkLedger{Ref: mrwRef, AttemptCount: cap, HighWater: 200, HaltNotified: false}

	t.Run("same comment set does not fire or halt", func(t *testing.T) {
		st := &mrwStore{
			candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
			ledgers:    map[string]store.MrReworkLedger{mrwRef: postManual},
		}
		runs := &mrwRuns{}
		notifier := &mrwNotifier{}
		f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

		newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: cap}).detect(context.Background(), mrwRepoRow(), f)

		if len(runs.calls) != 0 {
			t.Fatalf("no comment past the manual high-water must not fire, got %d runs", len(runs.calls))
		}
		if len(f.notes) != 0 || len(notifier.calls) != 0 || len(st.haltSets) != 0 {
			t.Fatalf("nothing past the mark must not re-halt: notes=%d notifs=%d halts=%d",
				len(f.notes), len(notifier.calls), len(st.haltSets))
		}
	})

	t.Run("a genuinely-new comment re-halts and comments once", func(t *testing.T) {
		st := &mrwStore{
			candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
			ledgers:    map[string]store.MrReworkLedger{mrwRef: postManual},
		}
		runs := &mrwRuns{}
		notifier := &mrwNotifier{}
		// id 300 > high_water 200: past the mark, so GATE 3 clears; attempt_count == cap →
		// GATE 4 halts; halt_notified was reset by the manual cycle → it comments once more.
		f := landedForge(mrwComment(200, landed(), mrwHeadSHA), mrwComment(300, landed(), mrwHeadSHA))

		newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: cap}).detect(context.Background(), mrwRepoRow(), f)

		if len(runs.calls) != 0 {
			t.Fatalf("a capped MR must not start a run, got %d", len(runs.calls))
		}
		if len(st.haltSets) != 1 {
			t.Fatalf("expected one halt latch write after the reset, got %d", len(st.haltSets))
		}
		if len(f.notes) != 1 {
			t.Fatalf("expected one cap-halt comment on the re-halt, got %d", len(f.notes))
		}
		if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" {
			t.Fatalf("expected one halted notification on the re-halt, got %+v", notifier.calls)
		}
	})
}

func TestMRReworkStrictlyAboveHighWaterFires(t *testing.T) {
	// The other half of SC3: one comment STRICTLY ABOVE the mark fires and advances it.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 1, HighWater: 100}},
	}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(100, landed(), mrwHeadSHA), mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("a comment above the high-water must fire, got %d runs", len(runs.calls))
	}
	if len(st.upserts) != 1 || st.upserts[0].HighWater != 140 {
		t.Fatalf("expected high_water advance to 140, got %+v", st.upserts)
	}
	if got := st.ledgers[mrwRef].AttemptCount; got != 2 {
		t.Fatalf("attempt_count = %d, want 2 (incremented)", got)
	}
}

func TestMRReworkAtCapHaltsOnceThenSilent(t *testing.T) {
	// attempt_count (5) >= cap (5): halt. A new comment (id 200 > high_water 120) is the
	// trigger, so the cap gate is actually reached.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120}},
	}
	runs := &mrwRuns{}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	d := newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5})

	d.detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a capped MR must not start a run, got %d", len(runs.calls))
	}
	if len(st.haltSets) != 1 {
		t.Fatalf("expected one SetMRReworkHaltNotified latch, got %d", len(st.haltSets))
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "rework-cycle limit (5)") {
		t.Fatalf("expected one cap-halt comment naming the limit, got %+v", f.notes)
	}
	// PRD #1202 D10: the halt notification now anchors to the SOURCE run so the inbox row
	// links to the run page (where the owner can press "Rework now").
	if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" ||
		notifier.calls[0].runID == nil || *notifier.calls[0].runID != mrwSourceRunID {
		t.Fatalf("expected one halted notification anchored to the source run, got %+v", notifier.calls)
	}
	if !notifier.calls[0].durable {
		t.Errorf("the halt DM must be durable (issue #1675)")
	}

	// Second tick: the latch is set → NO second comment, NO second notify.
	f2 := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	d.detect(context.Background(), mrwRepoRow(), f2)
	if len(f2.notes) != 0 || len(notifier.calls) != 1 || len(st.haltSets) != 1 {
		t.Fatalf("the halt latch must be silent on the next tick: notes=%d notifs=%d halts=%d",
			len(f2.notes), len(notifier.calls), len(st.haltSets))
	}
}

func TestMRReworkHaltedAndNotifiedSkipsListingAndLookups(t *testing.T) {
	// At the cap with the halt already notified, no path can start a run, so the tick must not
	// spend a comment listing or author lookups on the MR (#2347 review). A new comment from an
	// eligible author would otherwise reach the assessment every tick.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120, HaltNotified: true}},
	}
	runs := &mrwRuns{}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	d := newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5})

	d.detect(context.Background(), mrwRepoRow(), f)

	if f.listCalls != 0 || len(f.lookups) != 0 {
		t.Fatalf("a halted, notified MR must skip the listing and lookups: list=%d lookups=%d", f.listCalls, len(f.lookups))
	}
	if len(runs.calls) != 0 || len(f.notes) != 0 || len(notifier.calls) != 0 || len(st.haltSets) != 0 {
		t.Fatalf("a halted, notified MR must stay silent: runs=%d notes=%d notifs=%d halts=%d",
			len(runs.calls), len(f.notes), len(notifier.calls), len(st.haltSets))
	}

	// Raising the cap un-halts it: the next tick lists again and proceeds.
	d2 := newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 6})
	f2 := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	d2.detect(context.Background(), mrwRepoRow(), f2)
	if f2.listCalls != 1 || len(runs.calls) != 1 {
		t.Fatalf("under a raised cap the MR must be listed and reworked: list=%d runs=%d", f2.listCalls, len(runs.calls))
	}
}

func TestMRReworkHaltLatchWriteFailsNoComment(t *testing.T) {
	// NOTIFY-THEN-LATCH-THEN-COMMENT (issue #1675): the durable DM is recorded before the
	// latch, so a failed latch write has already notified once (the next tick repeats it:
	// at-least-once), but it must NOT comment or start a run.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120}},
		haltErr:    context.DeadlineExceeded,
	}
	runs := &mrwRuns{}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

	newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(f.notes) != 0 || len(runs.calls) != 0 {
		t.Fatalf("a failed latch write must post no comment and start no run: notes=%d runs=%d", len(f.notes), len(runs.calls))
	}
	if len(notifier.calls) != 1 || !notifier.calls[0].durable {
		t.Fatalf("a failed latch write must have notified once, durably, got %+v", notifier.calls)
	}
}

func TestMRReworkHaltNotifyFailureRetriesNextTick(t *testing.T) {
	// A notify that fails to persist leaves no latch and no comment; the next tick
	// (notify healthy again) halts once: notify, latch, comment, in that order.
	var ops []string
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120}},
		ops:        &ops,
	}
	runs := &mrwRuns{ops: &ops}
	notifier := &mrwNotifier{err: context.DeadlineExceeded, ops: &ops}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	f.ops = &ops
	d := newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5})

	d.detect(context.Background(), mrwRepoRow(), f)
	if len(st.haltSets) != 0 || len(f.notes) != 0 {
		t.Fatalf("a failed notify must set no latch and post no comment, got latches=%d notes=%d", len(st.haltSets), len(f.notes))
	}
	if got := strings.Join(ops, ","); got != "notify" {
		t.Fatalf("first tick ops = %q, want notify only", got)
	}

	notifier.err = nil
	ops = ops[:0]
	d.detect(context.Background(), mrwRepoRow(), f)
	if got := strings.Join(ops, ","); got != "notify,halt,comment" {
		t.Fatalf("retry tick ops = %q, want notify,halt,comment", got)
	}
	if len(st.haltSets) != 1 || len(f.notes) != 1 {
		t.Fatalf("retry tick: latches=%d notes=%d, want 1/1", len(st.haltSets), len(f.notes))
	}
}

func TestMRReworkBranchInUseSwallows(t *testing.T) {
	// SC4: CreateAutoMRReworkRun loses the cross-kind branch race (an active ci_fix on
	// the ref) → ErrBranchInUse. Swallow: no comment, no notify, and the ledger is NOT
	// advanced (retry next tick).
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 1, HighWater: 100}},
	}
	runs := &mrwRuns{err: workersvc.ErrBranchInUse}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected one create attempt, got %d", len(runs.calls))
	}
	if len(st.upserts) != 0 {
		t.Fatalf("a swallowed create must not advance the ledger, upserts=%d", len(st.upserts))
	}
	if len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("a swallowed create must not comment/notify: notes=%d notifs=%d", len(f.notes), len(notifier.calls))
	}
}

// This test stays serial because it captures the process-wide slog default.
func TestMRReworkNoCredentialForHarnessSkipsWithoutErrorDetail(t *testing.T) {
	const detail = "arbitrary fixture resolver detail"
	ledger := store.MrReworkLedger{
		RepoID: mrwRepoID, Ref: mrwRef, AttemptCount: 1, HighWater: 100,
	}
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: ledger},
	}
	runs := &mrwRuns{err: fmt.Errorf("%s: %w", detail, workersvc.ErrNoCredentialForHarness)}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected one create attempt, got %+v", runs.calls)
	}
	call := runs.calls[0]
	if call.userID != mrwUserID || call.repoID != mrwRepoID || call.sourceRunID != mrwSourceRunID ||
		call.ref != mrwRef || call.mrIID != mrwMrIID {
		t.Fatalf("create attempt targeted wrong source: %+v", call)
	}
	if !reflect.DeepEqual(st.ledgers, map[string]store.MrReworkLedger{mrwRef: ledger}) ||
		len(st.upserts) != 0 || len(st.haltSets) != 0 {
		t.Fatalf("credential refusal changed ledger: ledgers=%+v upserts=%+v halts=%+v", st.ledgers, st.upserts, st.haltSets)
	}
	if len(notifier.calls) != 0 || len(f.notes) != 0 {
		t.Fatalf("credential refusal must not notify/comment: notifications=%+v notes=%+v", notifier.calls, f.notes)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("expected one JSON skip log: %v; logs=%q", err, logs.String())
	}
	want := map[string]any{
		"level":      "WARN",
		"msg":        "poller: mr-rework skipped: source-run harness has no usable credential",
		"repo":       mrwRepoRow().PathWithNamespace,
		"ref":        mrwRef,
		"source_run": mrwSourceRunID.String(),
	}
	delete(record, "time")
	if !reflect.DeepEqual(record, want) {
		t.Fatalf("skip log = %+v, want static fields %+v", record, want)
	}
	if strings.Contains(logs.String(), detail) || strings.Contains(logs.String(), runs.err.Error()) {
		t.Fatalf("skip log contains resolver error detail: %q", logs.String())
	}
}

func TestMRReworkActiveExistsSwallows(t *testing.T) {
	// A concurrent rework on this MR → ErrActiveMRReworkExists, swallowed like ErrBranchInUse.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{err: workersvc.ErrActiveMRReworkExists}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 || len(st.upserts) != 0 {
		t.Fatalf("ErrActiveMRReworkExists must swallow with no ledger advance: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
}

func TestMRReworkAdminGateOffNoFire(t *testing.T) {
	// The admin kill-switch is off: the detector returns before listing candidates.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: false, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 || len(st.evicts) != 0 {
		t.Fatalf("admin gate off must be a full no-op (no candidates listed): runs=%d evicts=%d", len(runs.calls), len(st.evicts))
	}
}

func TestMRReworkAdminGateErrorFailsClosed(t *testing.T) {
	// Decision 5: a genuine settings READ ERROR disables rather than enables.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, enabledErr: context.DeadlineExceeded, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a settings read error must fail closed (no fire), got %d runs", len(runs.calls))
	}
}

func TestMRReworkEmptyCandidatesReconcilesRepo(t *testing.T) {
	// Wiring only: even an empty candidate list reconciles the correct repo.
	// Real structural eviction and eligibility retention are covered by live-DB tests.
	st := &mrwStore{
		candidates: nil, // the merged/closed MR is excluded by the candidate query
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 3, HighWater: 200}},
	}
	runs := &mrwRuns{}
	f := landedForge()

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a merged/closed MR must not be acted on, got %d runs", len(runs.calls))
	}
	if len(st.evicts) != 1 || st.evicts[0] != mrwRepoID {
		t.Fatalf("expected one reconciliation for the repo, got %+v", st.evicts)
	}
}

func TestMRReworkIssuelessBranchDecoupled(t *testing.T) {
	// PRD #908 M2: an mr_rework candidate on a scheduled (self_improve) branch does not
	// parse to an issue iid. The rework still fires below the cap, and at the cap the halt
	// still notifies once — only the halt ISSUE COMMENT is suppressed (nothing to comment
	// on). The existing agent/issue-N cap-halt test (TestMRReworkAtCapHaltsOnceThenSilent)
	// remains the control that the comment DOES post for an issue branch.
	issuelessRef := "uzi/self-improve/" + uuid.NewString()
	issuelessCand := func() store.ListMRReworkCandidatesRow {
		c := mrwCand("success")
		c.Ref = pgtype.Text{String: issuelessRef, Valid: true}
		return c
	}

	t.Run("below cap fires without a comment", func(t *testing.T) {
		st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{issuelessCand()}}
		runs := &mrwRuns{}
		notifier := &mrwNotifier{}
		// One kept comment, id 120 > high_water 0 → fire.
		f := landedForge(mrwComment(120, landed(), mrwHeadSHA))

		newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

		if len(runs.calls) != 1 {
			t.Fatalf("an issueless branch below cap must still fire the rework, got %d runs", len(runs.calls))
		}
		if runs.calls[0].ref != issuelessRef {
			t.Fatalf("rework created for ref %q, want %q", runs.calls[0].ref, issuelessRef)
		}
		if len(st.upserts) != 1 || st.upserts[0].HighWater != 120 {
			t.Fatalf("expected one ledger upsert advancing high_water to 120, got %+v", st.upserts)
		}
		if len(f.notes) != 0 {
			t.Fatalf("an issueless branch must post NO issue comment, got %+v", f.notes)
		}
	})

	t.Run("at cap halts once, no comment, still notifies", func(t *testing.T) {
		st := &mrwStore{
			candidates: []store.ListMRReworkCandidatesRow{issuelessCand()},
			ledgers:    map[string]store.MrReworkLedger{issuelessRef: {Ref: issuelessRef, AttemptCount: 5, HighWater: 120}},
		}
		runs := &mrwRuns{}
		notifier := &mrwNotifier{}
		// A new comment (id 200 > high_water 120) reaches the cap gate.
		f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

		newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

		if len(runs.calls) != 0 {
			t.Fatalf("a capped issueless MR must not start a run, got %d", len(runs.calls))
		}
		if len(st.haltSets) != 1 {
			t.Fatalf("expected one SetMRReworkHaltNotified latch, got %d", len(st.haltSets))
		}
		if len(f.notes) != 0 {
			t.Fatalf("an issueless cap-halt must post NO issue comment, got %+v", f.notes)
		}
		// PRD #1202 D10: even an issueless halt (no MR comment posted) anchors the inbox row
		// to the source run so it links to the run page.
		if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" ||
			notifier.calls[0].runID == nil || *notifier.calls[0].runID != mrwSourceRunID {
			t.Fatalf("expected one halted notification anchored to the source run, got %+v", notifier.calls)
		}
		if !notifier.calls[0].durable {
			t.Errorf("the issueless halt DM must be durable (issue #1675)")
		}
	})
}

func TestMRReworkBotOnlyCommentsNoFire(t *testing.T) {
	// The only comment is uzi's OWN bot note (author == bot id): the snapshot filter
	// drops it, leaving nothing → no fire.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	botComment := mrwComment(300, landed(), mrwHeadSHA)
	botComment.AuthorForgeUserID = mrwBotID
	f := landedForge(botComment)

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("a bot-only comment set must not fire, got %d runs", len(runs.calls))
	}
}

// mrwSummaryComment builds a TOP-LEVEL / summary review note (ReviewState summary,
// no diff anchor, no head SHA — the shape github_mr.go's Source A produces). It uses a
// THIRD-PARTY author id (77, != mrwBotID 999) so the D1 self-filter keeps it: a
// non-actionable fixture must reach detectOne, not be dropped upstream, or the #1142
// regression would pass vacuously.
func mrwSummaryComment(id int64, created time.Time, login, body string) forge.MRComment {
	return forge.MRComment{
		ID:                id,
		AuthorForgeUserID: 77,
		AuthorUsername:    login,
		Body:              body,
		CreatedAt:         created,
		ReviewState:       forge.ReviewCommentSummary,
	}
}

const coderabbitSummaryMarker = "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->"

func TestMRReworkBotWalkthroughSummaryNoFire(t *testing.T) {
	// Issue #1142: the ONLY comment is a third-party bot's walkthrough/summary note
	// (CodeRabbit "No actionable comments were generated"). It is KEPT by the snapshot
	// (third-party, not uzi's own bot) but is NON-actionable, so it must neither fire
	// nor advance the ledger. Fails on unfixed code (which counts it as a new comment).
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	body := coderabbitSummaryMarker + "\n\nNo actionable comments were generated in the recent review."
	f := landedForge(mrwSummaryComment(400, landed(), "coderabbitai[bot]", body))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a bot walkthrough/summary-only set must not fire or advance the ledger: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
}

func TestMRReworkHumanCodeRabbitControlCommentNoFire(t *testing.T) {
	// Issue #1407: a maintainer's standalone CodeRabbit command controls that bot; it is
	// not review feedback for uzi and must neither start a run nor consume a ledger attempt.
	for _, body := range []string{"@coderabbitai rate limit", "@coderabbitai review"} {
		t.Run(body, func(t *testing.T) {
			st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
			runs := &mrwRuns{}
			f := landedForge(mrwSummaryComment(420, landed(), "maintainer", body))

			newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

			if len(runs.calls) != 0 || len(st.upserts) != 0 {
				t.Fatalf("a CodeRabbit control command must not fire or advance the ledger: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
			}
		})
	}
}

func TestMRReworkInlineBotFindingFires(t *testing.T) {
	// A third-party review bot's INLINE finding is exactly what mr_rework exists for —
	// the "[bot]" login must NOT suppress it (only top-level/summary bot notes filter).
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	inline := mrwComment(120, landed(), mrwHeadSHA) // mrwComment is ReviewCommentInline
	inline.AuthorForgeUserID = 77                   // a third party
	inline.AuthorUsername = "coderabbitai[bot]"
	f := landedForge(inline)

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("a bot INLINE finding must fire, got %d runs", len(runs.calls))
	}
	if len(st.upserts) != 1 || st.upserts[0].HighWater != 120 {
		t.Fatalf("expected one ledger upsert advancing high_water to 120, got %+v", st.upserts)
	}
}

func TestMRReworkHumanTopLevelNoteFires(t *testing.T) {
	// A human top-level note ("please also rename X") is a real request and stays
	// actionable even though it is a summary-state comment (non-bot login, no marker).
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	f := landedForge(mrwSummaryComment(130, landed(), "maintainer", "please also rename X before we merge"))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("a human top-level note must fire, got %d runs", len(runs.calls))
	}
	if len(st.upserts) != 1 || st.upserts[0].HighWater != 130 {
		t.Fatalf("expected one ledger upsert advancing high_water to 130, got %+v", st.upserts)
	}
}

func TestMRReworkGitLabMarkerNonBotSummaryNoFire(t *testing.T) {
	// Forge-agnostic marker rule: on GitLab/Forgejo CodeRabbit posts as an ordinary
	// user (login has no "[bot]" suffix), so the summary marker in the body is the only
	// signal that classifies the walkthrough as non-actionable.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	body := coderabbitSummaryMarker + "\n\nNo actionable comments were generated."
	f := landedForge(mrwSummaryComment(410, landed(), "coderabbit", body))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a marker-carrying summary from a non-bot login must not fire: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
}

func TestMRReworkSummaryThenLowerIdInlineFires(t *testing.T) {
	// Option B (issue #1142): a summary-only tick does NOT advance the high-water, so a
	// later inline finding with a LOWER forge id than the summary still fires — the exact
	// case the naive "advance to the max kept id" would have hidden.
	st := &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}}
	runs := &mrwRuns{}
	walkthrough := "<!-- walkthrough_start -->\n\nWalkthrough of the change."
	d := newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5})

	// Tick 1: only a bot walkthrough with a HIGH id (300). No fire, no ledger advance.
	f1 := landedForge(mrwSummaryComment(300, landed(), "coderabbitai[bot]", walkthrough))
	d.detect(context.Background(), mrwRepoRow(), f1)
	if len(runs.calls) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a summary-only tick must not fire or advance the ledger: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}

	// Tick 2: the same walkthrough (300) plus a real inline finding with a LOWER id
	// (200). Because tick 1 left high_water at 0, the id-200 inline still clears GATE 3.
	f2 := landedForge(mrwSummaryComment(300, landed(), "coderabbitai[bot]", walkthrough), mrwComment(200, landed(), mrwHeadSHA))
	d.detect(context.Background(), mrwRepoRow(), f2)
	if len(runs.calls) != 1 {
		t.Fatalf("a later inline finding with a lower id than the summary must still fire, got %d runs", len(runs.calls))
	}
	if len(st.upserts) != 1 || st.upserts[0].HighWater != 200 {
		t.Fatalf("expected the fire to advance high_water to the actionable id 200 (not the summary 300), got %+v", st.upserts)
	}
}
