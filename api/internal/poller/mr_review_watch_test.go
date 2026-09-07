package poller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
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

	upserts   []store.UpsertMRReworkLedgerParams
	haltSets  []store.SetMRReworkHaltNotifiedParams
	evicts    []store.DeleteMRReworkLedgerNotInParams
	upsertErr error
	haltErr   error

	ops *[]string
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

func (s *mrwStore) UpsertMRReworkLedger(_ context.Context, arg store.UpsertMRReworkLedgerParams) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, arg)
	if s.ledgers == nil {
		s.ledgers = map[string]store.MrReworkLedger{}
	}
	cur := s.ledgers[arg.Ref]
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.AttemptCount++ // INSERT count=1 or increment
	if arg.HighWater > cur.HighWater {
		cur.HighWater = arg.HighWater // GREATEST(existing, new): advance-only
	}
	cur.HaltNotified = false // a proceed resets the latch
	s.ledgers[arg.Ref] = cur
	if s.ops != nil {
		*s.ops = append(*s.ops, "upsert")
	}
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

func (s *mrwStore) DeleteMRReworkLedgerNotIn(_ context.Context, arg store.DeleteMRReworkLedgerNotInParams) (int64, error) {
	s.evicts = append(s.evicts, arg)
	return 0, nil
}

type mrwRunCall struct {
	userID, repoID, sourceRunID uuid.UUID
	ref                         string
	mrIID                       int64
	title, desc                 string
	snapshot                    *workersvc.ReviewCommentsSnapshot
}

type mrwRuns struct {
	err   error
	calls []mrwRunCall
	runID uuid.UUID
	ops   *[]string
}

func (r *mrwRuns) CreateAutoMRReworkRun(_ context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, desc string, snapshot *workersvc.ReviewCommentsSnapshot) (store.Run, error) {
	r.calls = append(r.calls, mrwRunCall{userID, repoID, sourceRunID, ref, mrIID, title, desc, snapshot})
	if r.ops != nil {
		*r.ops = append(*r.ops, "create")
	}
	if r.err != nil {
		return store.Run{}, r.err
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
}

type mrwNotifier struct {
	calls []mrwNotifyCall
	ops   *[]string
}

func (n *mrwNotifier) Notify(_ context.Context, note notifysvc.Notification) (store.Notification, error) {
	p, _ := note.Payload.(notifysvc.CIAutofixPayload)
	n.calls = append(n.calls, mrwNotifyCall{note.Kind, note.UserID, note.RunID, p})
	if n.ops != nil {
		*n.ops = append(*n.ops, "notify")
	}
	return store.Notification{ID: uuid.New()}, nil
}

type mrwSettings struct {
	enabled    bool
	enabledErr error
	capVal     int
	capErr     error
}

func (s mrwSettings) MrReworkEnabled(context.Context) (bool, error) { return s.enabled, s.enabledErr }
func (s mrwSettings) MrReworkCap(context.Context) (int, error)      { return s.capVal, s.capErr }

// mrwForge embeds the CI-autofix test forge (which already satisfies forge.Forge and
// captures issue-note posts in .notes) and overrides only the MR-comment read.
type mrwForge struct {
	*cfForge
	comments    []forge.MRComment
	commentsErr error
}

func (f *mrwForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
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
	return NewMRReviewWatch(st, runs, n, set, 5, 5*time.Minute)
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
	// create → upsert (create-then-record).
	if strings.Join(ops, ",") != "create,upsert" {
		t.Fatalf("op order = %v, want [create upsert]", ops)
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
	if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" || notifier.calls[0].runID != nil {
		t.Fatalf("expected one halted notification with no run anchor, got %+v", notifier.calls)
	}

	// Second tick: the latch is set → NO second comment, NO second notify.
	f2 := landedForge(mrwComment(200, landed(), mrwHeadSHA))
	d.detect(context.Background(), mrwRepoRow(), f2)
	if len(f2.notes) != 0 || len(notifier.calls) != 1 || len(st.haltSets) != 1 {
		t.Fatalf("the halt latch must be silent on the next tick: notes=%d notifs=%d halts=%d",
			len(f2.notes), len(notifier.calls), len(st.haltSets))
	}
}

func TestMRReworkHaltLatchWriteFailsNoComment(t *testing.T) {
	// RECORD-THEN-COMMENT: if the latch write fails, NO comment and NO notify.
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120}},
		haltErr:    context.DeadlineExceeded,
	}
	runs := &mrwRuns{}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

	newMRW(st, runs, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(f.notes) != 0 || len(notifier.calls) != 0 || len(runs.calls) != 0 {
		t.Fatalf("a failed latch write must post nothing: notes=%d notifs=%d runs=%d",
			len(f.notes), len(notifier.calls), len(runs.calls))
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

func TestMRReworkStopEvictsStaleLedger(t *testing.T) {
	// Stop-on-merge / stop-on-close cleanup: when a watched MR leaves the opened-only
	// candidate set (merged/closed via PRD #24's SyncMRStates, which ran FIRST this
	// tick), it produces no candidate, so the reconcile eviction clears its ledger row
	// with an empty keep-set — and nothing is acted on (no double-fire).
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
	if len(st.evicts) != 1 || len(st.evicts[0].KeepRefs) != 0 {
		t.Fatalf("expected one eviction with an empty keep-set, got %+v", st.evicts)
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
		if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" || notifier.calls[0].runID != nil {
			t.Fatalf("expected one halted notification with no run anchor, got %+v", notifier.calls)
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
