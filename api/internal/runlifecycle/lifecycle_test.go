package runlifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// testOrigin is the frontend origin the terminal-comment hook builds run links from.
const testOrigin = "https://uzi.test"

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeStore struct {
	moveCtx    store.GetRunMoveContextRow
	moveCtxErr error
	issueReads int // GetIssueByIID calls (the superseded fence must make none)
	issue      store.Issue
	issueErr   error
	columns    []store.BoardColumn
	pending    []uuid.UUID
	gaveUp     []store.ListGaveUpColumnMovesRow

	recorded   []store.RecordRunColumnMoveParams
	cleared    []uuid.UUID
	pendingArg *store.ListPendingColumnMovesParams // the LAST call
	gaveUpArg  *store.ListGaveUpColumnMovesParams  // the LAST call

	// Every list call, in order (the two-tier reconcile issues several per tick).
	pendingArgs []store.ListPendingColumnMovesParams
	gaveUpArgs  []store.ListGaveUpColumnMovesParams

	// lc is the Lifecycle under test (set by newTestLifecycle). Legacy mode (markers
	// == nil) keys on its fastWindow/giveup so f.pending is returned only to the fast
	// lane call and f.gaveUp only to the give-up threshold call.
	lc *Lifecycle

	// Marker mode (markers != nil) models the SQL: a per-run pending_since, listed
	// oldest first under the cutoffs and the LIMIT, deleted by record/clear.
	markers    map[uuid.UUID]time.Time
	ctxs       map[uuid.UUID]store.GetRunMoveContextRow
	gaveUpRows []store.ListGaveUpColumnMovesRow

	// gaveUpHook, when set, may answer a ListGaveUpColumnMoves call (n counts calls
	// from 1) instead of the default model.
	gaveUpHook func(ctx context.Context, arg store.ListGaveUpColumnMovesParams, n int) gaveUpResult
	// clock orders mover and gave-up calls against each other. Optional.
	clock        *orderClock
	gaveUpOrders []int

	// Terminal-comment claim (M5). claimErr forces the record step to fail; else the
	// first caller wins the row (returns 1) and every later caller reads 0, modelling
	// the atomic once-only UPDATE.
	claimCalls int
	claimErr   error
	claimed    bool
}

// gaveUpResult is what a gaveUpHook returns; handled=false falls through to the
// default model.
type gaveUpResult struct {
	rows    []store.ListGaveUpColumnMovesRow
	err     error
	handled bool
}

// orderClock hands out increasing ticks. Reconcile is single-goroutine, so no lock.
type orderClock struct{ n int }

func (c *orderClock) next() int {
	c.n++
	return c.n
}

func (f *fakeStore) GetRunMoveContext(_ context.Context, id uuid.UUID) (store.GetRunMoveContextRow, error) {
	if f.markers != nil {
		mc, ok := f.ctxs[id]
		if !ok {
			return store.GetRunMoveContextRow{}, pgx.ErrNoRows
		}
		if since, ok := f.markers[id]; ok {
			mc.MovePendingSince = ts(since)
		} else {
			mc.MovePendingSince = pgtype.Timestamptz{}
		}
		return mc, nil
	}
	return f.moveCtx, f.moveCtxErr
}
func (f *fakeStore) GetIssueByIID(_ context.Context, arg store.GetIssueByIIDParams) (store.Issue, error) {
	f.issueReads++
	if f.markers != nil {
		return issueWith(arg.RepoID, arg.ForgeIssueIid, "opened", board.ColumnInProgress), nil
	}
	return f.issue, f.issueErr
}
func (f *fakeStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return f.columns, nil
}
func (f *fakeStore) RecordRunColumnMove(_ context.Context, arg store.RecordRunColumnMoveParams) (int64, error) {
	f.recorded = append(f.recorded, arg)
	delete(f.markers, arg.ID)
	return 1, nil
}
func (f *fakeStore) ClearRunMovePending(_ context.Context, id uuid.UUID) (int64, error) {
	f.cleared = append(f.cleared, id)
	delete(f.markers, id)
	return 1, nil
}
func (f *fakeStore) ListPendingColumnMoves(_ context.Context, arg store.ListPendingColumnMovesParams) ([]uuid.UUID, error) {
	f.pendingArg = &arg
	f.pendingArgs = append(f.pendingArgs, arg)
	if f.markers != nil {
		var ids []uuid.UUID
		for id, since := range f.markers {
			if !since.After(arg.GraceCutoff.Time) && since.After(arg.GiveupCutoff.Time) {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return f.markers[ids[i]].Before(f.markers[ids[j]]) })
		if len(ids) > int(arg.MaxBatch) {
			ids = ids[:arg.MaxBatch]
		}
		return ids, nil
	}
	// Legacy mode: only the fast lane (the cutoff now-fastWindow) sees f.pending.
	if f.lc != nil && !arg.GiveupCutoff.Time.Equal(f.lc.now().Add(-f.lc.fastWindow)) {
		return nil, nil
	}
	return f.pending, nil
}
func (f *fakeStore) ListGaveUpColumnMoves(ctx context.Context, arg store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error) {
	f.gaveUpArg = &arg
	f.gaveUpArgs = append(f.gaveUpArgs, arg)
	if f.clock != nil {
		f.gaveUpOrders = append(f.gaveUpOrders, f.clock.next())
	}
	if f.gaveUpHook != nil {
		if r := f.gaveUpHook(ctx, arg, len(f.gaveUpArgs)); r.handled {
			return r.rows, r.err
		}
	}
	if f.markers != nil {
		var rows []store.ListGaveUpColumnMovesRow
		for _, row := range f.gaveUpRows {
			since := row.MovePendingSince.Time
			if !since.After(arg.GiveupCutoff.Time) && since.After(arg.PriorCutoff.Time) {
				rows = append(rows, row)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].MovePendingSince.Time.Before(rows[j].MovePendingSince.Time) })
		if len(rows) > 100 {
			rows = rows[:100]
		}
		return rows, nil
	}
	if f.lc != nil && !arg.GiveupCutoff.Time.Equal(f.lc.now().Add(-f.lc.giveup)) {
		return nil, nil
	}
	return f.gaveUp, nil
}
func (f *fakeStore) ClaimAutopilotTerminalComment(context.Context, uuid.UUID) (int64, error) {
	f.claimCalls++
	if f.claimErr != nil {
		return 0, f.claimErr
	}
	if f.claimed {
		return 0, nil // a peer invocation already claimed it
	}
	f.claimed = true
	return 1, nil
}

type fakeMover struct {
	forgeErr    error
	autoMoveErr error
	moves       []string
	// calls records every AutoMove; autoMoveHook, when set, runs first (it may block
	// until ctx is done). clock orders these calls against the store's gave-up calls.
	calls        []moveCall
	autoMoveHook func(ctx context.Context, issue store.Issue, target string)
	clock        *orderClock
	// forge is returned by ForgeForConnection (lazily created), so the
	// terminal-comment hook's CreateIssueNote calls are observable.
	forge *fakeForge
}

func (m *fakeMover) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	if m.forgeErr != nil {
		return nil, m.forgeErr
	}
	if m.forge == nil {
		m.forge = &fakeForge{}
	}
	return m.forge, nil
}

// moveCall is one AutoMove observed by fakeMover.
type moveCall struct {
	target            string
	issueIID          int64
	ctxErrAtEntry     error
	ctxErrAfterHook   error
	deadlineRemaining time.Duration // 0 when ctx has no deadline
	order             int
}

func (m *fakeMover) AutoMove(ctx context.Context, _ forge.IssueTracker, _ int64, issue store.Issue, _ []store.BoardColumn, target string) (store.Issue, error) {
	m.moves = append(m.moves, target)
	call := moveCall{target: target, issueIID: issue.ForgeIssueIid, ctxErrAtEntry: ctx.Err()}
	if m.clock != nil {
		call.order = m.clock.next()
	}
	if dl, ok := ctx.Deadline(); ok {
		call.deadlineRemaining = time.Until(dl)
	}
	if m.autoMoveHook != nil {
		m.autoMoveHook(ctx, issue, target)
		call.ctxErrAfterHook = ctx.Err()
	}
	m.calls = append(m.calls, call)
	if call.ctxErrAfterHook != nil {
		return store.Issue{}, call.ctxErrAfterHook
	}
	if m.autoMoveErr != nil {
		return store.Issue{}, m.autoMoveErr
	}
	return issue, nil
}

// fakeForge records the issue comments the terminal-comment hook posts. Only
// CreateIssueNote is exercised, so the rest of forge.Forge is left embedded-nil.
type fakeNote struct {
	projectID, issueIID int64
	body                string
}

type fakeForge struct {
	forge.Forge
	notes   []fakeNote
	noteErr error
}

func (f *fakeForge) CreateIssueNote(_ context.Context, projectID, issueIID int64, body string) (forge.IssueNote, error) {
	f.notes = append(f.notes, fakeNote{projectID, issueIID, body})
	return forge.IssueNote{}, f.noteErr
}

// ── helpers ───────────────────────────────────────────────────────────────────

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func newTestLifecycle(fs *fakeStore, fm *fakeMover) *Lifecycle {
	l := New(fs, fm, testOrigin)
	l.now = func() time.Time { return testNow }
	fs.lc = l
	return l
}

func txt(s string) pgtype.Text          { return pgtype.Text{String: s, Valid: true} }
func nullText() pgtype.Text             { return pgtype.Text{} }
func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func defaultCols() []store.BoardColumn {
	names := []string{board.ColumnInProgress, "Upcoming", "Later", board.ColumnHumanReview}
	cols := make([]store.BoardColumn, len(names))
	for i, n := range names {
		cols[i] = store.BoardColumn{LabelName: n, Position: int32(i)}
	}
	return cols
}

func issueWith(repoID uuid.UUID, iid int64, state string, labels ...string) store.Issue {
	b, _ := json.Marshal(labels)
	return store.Issue{RepoID: repoID, ForgeIssueIid: iid, State: state, Labels: b}
}

func moveCtx(repoID uuid.UUID, iid int64, status string, origin, boardCol pgtype.Text) store.GetRunMoveContextRow {
	return store.GetRunMoveContextRow{
		Status:           status,
		IssueIid:         pgtype.Int8{Int64: iid, Valid: true},
		RepoID:           pgtype.UUID{Bytes: repoID, Valid: true},
		OriginColumn:     origin,
		BoardColumn:      boardCol,
		MovePendingSince: ts(testNow.Add(-time.Minute)), // set + inside the window
		ForgeProjectID:   77,
		ForgeType:        "gitlab",
		BaseUrl:          "https://gitlab.example.com",
		TokenCiphertext:  []byte("sealed"),
	}
}

// ── decision maps ─────────────────────────────────────────────────────────────

func TestNotifierDecisionPartialMap(t *testing.T) {
	later := txt("Later")
	tests := []struct {
		status string
		origin pgtype.Text
		want   decision
	}{
		{"queued", later, decision{act: true, target: board.ColumnInProgress}},
		{"completed", later, decision{act: true, target: board.ColumnHumanReview}},
		{"failed", later, decision{act: true, target: "Later"}},
		{"cancelled", later, decision{act: true, target: "Later"}},
		{"failed", nullText(), decision{act: false, clear: true}}, // NULL-origin restore skipped
		// deliberately ignored by the notifier (Decision #1): no move, marker left.
		{"running", later, decision{act: false, clear: false}},
		{"awaiting_approval", later, decision{act: false, clear: false}},
		{"claimed", later, decision{act: false, clear: false}},
		// PRD #35: a park moves no card, so the notifier stays partial here even though
		// the RECONCILER's total map gained limit_wait. Asserted rather than left to the
		// default arm, because the two maps disagreeing is the whole design and a reader
		// who sees limit_wait added to one will reach for the other.
		{"limit_wait", later, decision{act: false, clear: false}},
	}
	for _, tc := range tests {
		if got := notifierDecision(tc.status, tc.origin); got != tc.want {
			t.Errorf("notifierDecision(%q) = %+v, want %+v", tc.status, got, tc.want)
		}
	}
}

func TestReconcilerDecisionTotalMap(t *testing.T) {
	later := txt("Later")
	tests := []struct {
		status string
		origin pgtype.Text
		want   decision
	}{
		// The whole point of the total map: a queued run that has advanced to
		// running by retry time still lands In Progress (the notifier's partial map
		// would no-op it forever).
		{"queued", later, decision{act: true, target: board.ColumnInProgress}},
		{"claimed", later, decision{act: true, target: board.ColumnInProgress}},
		{"running", later, decision{act: true, target: board.ColumnInProgress}},
		{"awaiting_approval", later, decision{act: true, target: board.ColumnInProgress}},
		// PRD #35: a parked run is working — it holds its issue, its session and its
		// branch, and it resumes on its own. The default arm would be
		// {act:false, clear:false}, which leaves a pending move marker UNHEALED and
		// trips the 30-minute pending warn; a park routinely outlasts 30 minutes by design,
		// so that warn would be the normal case rather than an exception.
		{"limit_wait", later, decision{act: true, target: board.ColumnInProgress}},
		{"recovery_wait", later, decision{act: true, target: board.ColumnInProgress}},
		{"completed", later, decision{act: true, target: board.ColumnHumanReview}},
		{"failed", later, decision{act: true, target: "Later"}},
		{"cancelled", later, decision{act: true, target: "Later"}},
		{"failed", nullText(), decision{act: false, clear: true}},
	}
	for _, tc := range tests {
		if got := reconcilerDecision(tc.status, tc.origin); got != tc.want {
			t.Errorf("reconcilerDecision(%q) = %+v, want %+v", tc.status, got, tc.want)
		}
	}
}

// ── inline notifier ───────────────────────────────────────────────────────────

func TestNotifyForgeDownLeavesMarkerSet(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{autoMoveErr: errors.New("forge unreachable")}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fm.moves) != 1 || fm.moves[0] != board.ColumnInProgress {
		t.Fatalf("expected an In Progress move attempt, got %v", fm.moves)
	}
	if len(fs.cleared) != 0 {
		t.Fatal("a forge failure must LEAVE the pending marker for the reconcile loop")
	}
	if len(fs.recorded) != 0 {
		t.Fatal("board_column must not be recorded when the forge move failed")
	}
}

func TestNotifyQueuedMovesToInProgress(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fm.moves) != 1 || fm.moves[0] != board.ColumnInProgress {
		t.Fatalf("queued must move to In Progress, got %v", fm.moves)
	}
	if len(fs.recorded) != 1 || fs.recorded[0].BoardColumn.String != board.ColumnInProgress || !fs.recorded[0].BoardColumn.Valid {
		t.Fatalf("board_column must be recorded as In Progress, got %+v", fs.recorded)
	}
	if fs.recorded[0].ID != runID {
		t.Fatalf("recorded move for wrong run: %v", fs.recorded[0].ID)
	}
}

func TestNotifyNullOriginRestoreSkipped(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		// origin AND board both NULL → unknown baseline; a restore on a guess is
		// forbidden (never strip a human's label).
		moveCtx: moveCtx(repoID, 4, "failed", nullText(), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "failed")

	if len(fm.moves) != 0 {
		t.Fatal("a NULL-origin restore must never move the card")
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatal("a NULL-origin restore is definitive and must clear the marker")
	}
}

func TestNotifyClosedIssueSkipped(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "closed", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fm.moves) != 0 {
		t.Fatal("a closed issue must never be auto-moved")
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatal("the closed-issue skip clears the marker (Closed is a terminal placement)")
	}
}

func TestNotifyMissingContextIsNoop(t *testing.T) {
	fs := &fakeStore{moveCtxErr: pgx.ErrNoRows}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)
	l.notifyOnce(context.Background(), uuid.New(), "queued")
	if len(fm.moves) != 0 || len(fs.cleared) != 0 || len(fs.recorded) != 0 {
		t.Fatal("a vanished run must be a clean no-op")
	}
}

// ── reconcile loop ────────────────────────────────────────────────────────────

func TestReconcileSuccessAfterAdvanceToRunning(t *testing.T) {
	// The queued→In Progress inline move failed (forge was down); by retry time the
	// run is running. The reconciler's total map maps running→In Progress, so the
	// card still lands In Progress and the marker is recorded+cleared.
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "running", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
		pending: []uuid.UUID{runID},
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.Reconcile(context.Background())

	if len(fm.moves) != 1 || fm.moves[0] != board.ColumnInProgress {
		t.Fatalf("running run must be reconciled to In Progress, got %v", fm.moves)
	}
	if len(fs.recorded) != 1 || fs.recorded[0].BoardColumn.String != board.ColumnInProgress {
		t.Fatalf("board_column must be recorded, got %+v", fs.recorded)
	}
	if len(fs.cleared) != 0 {
		t.Fatal("a successful move records+clears in one statement, not a separate clear")
	}
}

func TestReconcileRespectsManualDragFailedFirstWrite(t *testing.T) {
	// board_column is still NULL (the In Progress write never succeeded), origin is
	// Later, but a human dragged the card to Upcoming. The guard compares current
	// against COALESCE(board_column, origin_column) = Later — NOT the never-written
	// In Progress target — sees Upcoming ≠ Later, and backs off.
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "running", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Upcoming"),
		columns: defaultCols(),
		pending: []uuid.UUID{runID},
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.Reconcile(context.Background())

	if len(fm.moves) != 0 {
		t.Fatalf("a manual drag must win — no forge move, got %v", fm.moves)
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatal("a detected manual drag clears the pending marker")
	}
}

// supersededFixture is an older failed run whose card still sits where the automation
// left it (In Progress), on an autopilot run so the terminal-comment hook is observable.
func supersededFixture(superseded bool) (*fakeStore, *fakeMover, uuid.UUID) {
	runID, repoID := uuid.New(), uuid.New()
	mc := autopilotMoveCtx(repoID, 4, "failed", 0, "")
	mc.OriginColumn = txt("Upcoming")
	mc.BoardColumn = txt(board.ColumnInProgress)
	mc.Superseded = superseded
	fs := &fakeStore{
		moveCtx: mc,
		issue:   issueWith(repoID, 4, "opened", "PRD", board.ColumnInProgress),
		columns: defaultCols(),
		pending: []uuid.UUID{runID},
	}
	return fs, &fakeMover{}, runID
}

func TestReconcileSupersededRunMovesNothing(t *testing.T) {
	fs, fm, runID := supersededFixture(true)
	l := newTestLifecycle(fs, fm)

	l.reconcileOne(context.Background(), runID)

	if len(fm.moves) != 0 {
		t.Fatalf("a superseded run must not move the card, got %v", fm.moves)
	}
	if len(fs.recorded) != 0 {
		t.Fatalf("a superseded run must not record a board column, got %+v", fs.recorded)
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatalf("the pending marker must be cleared, got %v", fs.cleared)
	}
	// The fence is apply's first statement: no issue read precedes it.
	if fs.issueReads != 0 {
		t.Fatalf("a superseded run must not read the issue, GetIssueByIID calls=%d", fs.issueReads)
	}
	// The fence covers the move only: the terminal-comment hook still runs.
	if fs.claimCalls < 1 {
		t.Fatalf("the terminal comment hook must still run, claimCalls=%d", fs.claimCalls)
	}
}

func TestReconcileNotSupersededStillMoves(t *testing.T) {
	fs, fm, runID := supersededFixture(false)
	l := newTestLifecycle(fs, fm)

	l.reconcileOne(context.Background(), runID)

	if len(fm.moves) != 1 || fm.moves[0] != "Upcoming" {
		t.Fatalf("a run with no successor restores its origin column, got %v", fm.moves)
	}
}

func TestNotifySupersededRunMovesNothing(t *testing.T) {
	fs, fm, runID := supersededFixture(true)
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "failed")

	if len(fm.moves) != 0 || len(fs.recorded) != 0 {
		t.Fatalf("a superseded run must not move the card, got moves=%v recorded=%+v", fm.moves, fs.recorded)
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatalf("the pending marker must be cleared, got %v", fs.cleared)
	}
}

func TestReconcileSkipsHealedMarker(t *testing.T) {
	// The candidate scan returned a run, but by the re-read its marker was already
	// cleared (inline move or manual drag won the race): no move, no clear.
	runID, repoID := uuid.New(), uuid.New()
	mc := moveCtx(repoID, 4, "running", txt("Later"), nullText())
	mc.MovePendingSince = pgtype.Timestamptz{} // healed since the scan
	fs := &fakeStore{moveCtx: mc, pending: []uuid.UUID{runID}}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.Reconcile(context.Background())

	if len(fm.moves) != 0 || len(fs.cleared) != 0 || len(fs.recorded) != 0 {
		t.Fatal("a marker healed since the scan must be a clean no-op")
	}
}

func TestReconcileWindowCutoffs(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLifecycle(fs, &fakeMover{})

	l.Reconcile(context.Background())

	// The fast lane is the call whose upper age bound is the fast window.
	var fast *store.ListPendingColumnMovesParams
	for i := range fs.pendingArgs {
		if fs.pendingArgs[i].GiveupCutoff.Time.Equal(testNow.Add(-l.fastWindow)) {
			fast = &fs.pendingArgs[i]
		}
	}
	if fast == nil {
		t.Fatalf("no fast-lane ListPendingColumnMoves call among %+v", fs.pendingArgs)
	}
	if !fast.GraceCutoff.Time.Equal(testNow.Add(-15 * time.Second)) {
		t.Fatalf("grace cutoff = %v, want now-15s", fast.GraceCutoff.Time)
	}
	if fast.MaxBatch != l.batch {
		t.Fatalf("batch = %d, want %d", fast.MaxBatch, l.batch)
	}
}

func TestReconcileGiveUpLeavesMarker(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{
		gaveUp: []store.ListGaveUpColumnMovesRow{
			{ID: runID, RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, IssueIid: pgtype.Int8{Int64: 9, Valid: true}, Status: "completed", MovePendingSince: ts(testNow.Add(-25 * time.Hour))},
		},
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.Reconcile(context.Background())

	if len(fs.cleared) != 0 {
		t.Fatal("a given-up marker must be LEFT set (visible drift), never cleared")
	}
	if len(fm.moves) != 0 {
		t.Fatal("given-up runs are not retried")
	}
	// The first scan of the give-up threshold covers (now-giveup-interval, now-giveup]
	// (the cursor starts one interval back), so each marker warns exactly once as it
	// crosses the boundary.
	var gu *store.ListGaveUpColumnMovesParams
	for i := range fs.gaveUpArgs {
		if fs.gaveUpArgs[i].GiveupCutoff.Time.Equal(testNow.Add(-l.giveup)) {
			gu = &fs.gaveUpArgs[i]
		}
	}
	if gu == nil {
		t.Fatalf("no give-up-threshold ListGaveUpColumnMoves call among %+v", fs.gaveUpArgs)
	}
	if !gu.PriorCutoff.Time.Equal(testNow.Add(-l.giveup - l.interval)) {
		t.Fatalf("prior cutoff = %v, want now-giveup-interval", gu.PriorCutoff.Time)
	}
}

// ── two-tier reconcile (#4704) ────────────────────────────────────────────────

// captureSlog swaps the default logger for a text handler over the returned buffer.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

const (
	// Both crossings must log at WARN: the level is part of the needle.
	fastCrossMsg = `level=WARN msg="run lifecycle: column move still pending after the fast retry window`
	giveUpMsg    = `level=WARN msg="run lifecycle: column move gave up after the retry window`
)

// logLinesWith counts log lines containing every needle.
func logLinesWith(logs *bytes.Buffer, needles ...string) int {
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		ok := line != ""
		for _, nd := range needles {
			if !strings.Contains(line, nd) {
				ok = false
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// markerFixture is a marker-mode store (SQL-like listing) over failed runs whose
// cards still sit In Progress, so a retry moves each one back to Upcoming.
type markerFixture struct {
	fs     *fakeStore
	fm     *fakeMover
	l      *Lifecycle
	clock  *orderClock
	repoID uuid.UUID
	nextIx int64
	ids    map[int64]uuid.UUID
}

func newMarkerFixture() *markerFixture {
	fs := &fakeStore{
		markers: map[uuid.UUID]time.Time{},
		ctxs:    map[uuid.UUID]store.GetRunMoveContextRow{},
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	clock := &orderClock{}
	fs.clock, fm.clock = clock, clock
	return &markerFixture{fs: fs, fm: fm, l: newTestLifecycle(fs, fm), clock: clock, repoID: uuid.New(), ids: map[int64]uuid.UUID{}}
}

// add stamps a marker pending since testNow-age and returns the run's issue iid.
func (x *markerFixture) add(age time.Duration) int64 {
	x.nextIx++
	id := uuid.New()
	x.fs.ctxs[id] = moveCtx(x.repoID, x.nextIx, "failed", txt("Upcoming"), txt(board.ColumnInProgress))
	x.fs.markers[id] = testNow.Add(-age)
	x.ids[x.nextIx] = id
	return x.nextIx
}

func (x *markerFixture) tickAt(cur *time.Time) { x.l.now = func() time.Time { return *cur } }

func callsWithMaxBatch(fs *fakeStore, n int32) []store.ListPendingColumnMovesParams {
	var out []store.ListPendingColumnMovesParams
	for _, a := range fs.pendingArgs {
		if a.MaxBatch == n {
			out = append(out, a)
		}
	}
	return out
}

func TestReconcileSlowLaneCadence(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLifecycle(fs, &fakeMover{})
	cur := testNow
	l.now = func() time.Time { return cur }

	checkFast := func(wantCalls int) {
		t.Helper()
		fast := callsWithMaxBatch(fs, defaultBatch)
		if len(fast) != wantCalls {
			t.Fatalf("fast calls = %d, want %d", len(fast), wantCalls)
		}
		last := fast[len(fast)-1]
		if !last.GraceCutoff.Time.Equal(cur.Add(-15*time.Second)) || !last.GiveupCutoff.Time.Equal(cur.Add(-l.fastWindow)) {
			t.Fatalf("fast args = %+v at %v", last, cur)
		}
	}

	l.Reconcile(context.Background())
	slow := callsWithMaxBatch(fs, 25)
	if len(slow) != 1 {
		t.Fatalf("first tick must run the slow lane once, got %d", len(slow))
	}
	if !slow[0].GraceCutoff.Time.Equal(cur.Add(-30*time.Minute)) || !slow[0].GiveupCutoff.Time.Equal(cur.Add(-24*time.Hour)) {
		t.Fatalf("slow args = %+v", slow[0])
	}
	checkFast(1)

	cur = cur.Add(9*time.Minute + 59*time.Second)
	l.Reconcile(context.Background())
	if n := len(callsWithMaxBatch(fs, 25)); n != 1 {
		t.Fatalf("slow lane ran again after 9m59s, calls=%d", n)
	}
	checkFast(2)

	cur = testNow.Add(10 * time.Minute)
	l.Reconcile(context.Background())
	slow = callsWithMaxBatch(fs, 25)
	if len(slow) != 2 {
		t.Fatalf("slow lane must run again at +10m, calls=%d", len(slow))
	}
	if !slow[1].GraceCutoff.Time.Equal(cur.Add(-30*time.Minute)) || !slow[1].GiveupCutoff.Time.Equal(cur.Add(-24*time.Hour)) {
		t.Fatalf("second slow args = %+v", slow[1])
	}
	checkFast(3)
}

func TestReconcileSlowLaneNeverListsPastGiveup(t *testing.T) {
	x := newMarkerFixture()
	old := x.add(25 * time.Hour)
	young := x.add(2 * time.Hour)

	x.l.Reconcile(context.Background())

	if len(x.fm.calls) != 1 || x.fm.calls[0].issueIID != young {
		t.Fatalf("only the 2h marker may be retried, got %+v", x.fm.calls)
	}
	if _, left := x.fs.markers[x.ids[old]]; !left {
		t.Fatal("a marker past the give-up must be left set")
	}
}

func TestReconcileSlowLaneRunsBeforeSaturatedFastLane(t *testing.T) {
	x := newMarkerFixture()
	var slowIIDs []int64
	for _, age := range []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour} {
		slowIIDs = append(slowIIDs, x.add(age))
	}
	for i := 0; i < 100; i++ {
		x.add(time.Minute + time.Duration(i)*11*time.Second)
	}

	x.l.Reconcile(context.Background())

	if len(x.fm.calls) != 103 {
		t.Fatalf("calls = %d, want 3 slow + 100 fast", len(x.fm.calls))
	}
	for i, iid := range slowIIDs {
		if x.fm.calls[i].issueIID != iid {
			t.Fatalf("call %d = iid %d, want slow iid %d (oldest first)", i, x.fm.calls[i].issueIID, iid)
		}
	}
}

func TestReconcileStalledSlowRequestLeavesFastBudget(t *testing.T) {
	x := newMarkerFixture()
	x.l.slowTimeout = 50 * time.Millisecond
	slow1 := x.add(3 * time.Hour)
	slow2 := x.add(2 * time.Hour)
	fast1 := x.add(2 * time.Minute)
	fast2 := x.add(time.Minute)
	isSlow := func(iid int64) bool { return iid == slow1 || iid == slow2 }
	x.fm.autoMoveHook = func(ctx context.Context, issue store.Issue, _ string) {
		if isSlow(issue.ForgeIssueIid) {
			<-ctx.Done() // a stalled forge: only the slow lane's own deadline ends it
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	x.l.Reconcile(ctx)

	calls := x.fm.calls
	if len(calls) == 0 || calls[0].issueIID != slow1 {
		t.Fatalf("the first attempt must be the oldest slow marker, got %+v", calls)
	}
	if !errors.Is(calls[0].ctxErrAfterHook, context.DeadlineExceeded) {
		t.Fatalf("the slow lane ctx must end with DeadlineExceeded, got %v", calls[0].ctxErrAfterHook)
	}
	fastSeen := map[int64]bool{}
	for _, c := range calls[1:] {
		if isSlow(c.issueIID) {
			t.Fatalf("the slow lane must stop at its deadline, got %+v", c)
		}
		if c.ctxErrAtEntry != nil {
			t.Fatalf("a fast attempt started on a dead ctx: %+v", c)
		}
		fastSeen[c.issueIID] = true
	}
	if !fastSeen[fast1] || !fastSeen[fast2] {
		t.Fatalf("every fast marker must be attempted, got %+v", calls)
	}
}

func TestReconcileNoSlowTickGivesFastFullBudget(t *testing.T) {
	x := newMarkerFixture()
	cur := testNow
	x.tickAt(&cur)
	x.l.Reconcile(context.Background()) // priming tick runs the slow lane
	cur = cur.Add(30 * time.Second)
	fast := x.add(time.Minute)
	// A parent longer than slowTimeout, so a fast lane wrongly given any child
	// deadline (slowTimeout, interval-slowTimeout, warnTimeout) is caught.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	x.l.Reconcile(ctx)

	if len(x.fm.calls) != 1 || x.fm.calls[0].issueIID != fast {
		t.Fatalf("calls = %+v", x.fm.calls)
	}
	if rem := x.fm.calls[0].deadlineRemaining; rem <= 25*time.Second {
		t.Fatalf("a tick without a slow pass must leave the fast lane the full budget, remaining %v", rem)
	}
}

func TestReconcileStalledWarnScanDoesNotShortenFastPass(t *testing.T) {
	x := newMarkerFixture()
	x.l.warnTimeout = 50 * time.Millisecond
	cur := testNow
	x.tickAt(&cur)
	fast := x.add(time.Minute)
	row := store.ListGaveUpColumnMovesRow{
		ID: uuid.New(), RepoID: pgtype.UUID{Bytes: x.repoID, Valid: true}, IssueIid: pgtype.Int8{Int64: 77, Valid: true},
		Status: "failed", MovePendingSince: ts(testNow.Add(-x.l.fastWindow - 10*time.Second)),
	}
	x.fs.gaveUpRows = []store.ListGaveUpColumnMovesRow{row}
	blocking := true
	x.fs.gaveUpHook = func(ctx context.Context, _ store.ListGaveUpColumnMovesParams, _ int) gaveUpResult {
		if !blocking {
			return gaveUpResult{}
		}
		<-ctx.Done()
		return gaveUpResult{err: ctx.Err(), handled: true}
	}
	logs := captureSlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	x.l.Reconcile(ctx)
	// The stalled scan is bounded by warnTimeout (50ms), not by the 30s parent.
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("a stalled warn scan must be cut off by warnTimeout, Reconcile took %v", el)
	}

	if len(x.fm.calls) != 1 || x.fm.calls[0].issueIID != fast {
		t.Fatalf("calls = %+v", x.fm.calls)
	}
	if len(x.fs.gaveUpOrders) == 0 {
		t.Fatal("the warn scan never ran")
	}
	for _, c := range x.fm.calls {
		if c.order >= x.fs.gaveUpOrders[0] {
			t.Fatalf("fast attempt order %d is not before the first warn scan %d", c.order, x.fs.gaveUpOrders[0])
		}
		if c.deadlineRemaining <= 25*time.Second {
			t.Fatalf("the fast attempt must see the full parent budget, remaining %v", c.deadlineRemaining)
		}
	}
	if n := logLinesWith(logs, fastCrossMsg); n != 0 {
		t.Fatalf("tick 1 must log no crossing, got %d", n)
	}

	blocking = false
	cur = cur.Add(30 * time.Second)
	x.l.Reconcile(context.Background())

	if n := logLinesWith(logs, fastCrossMsg, row.ID.String()); n != 1 {
		t.Fatalf("tick 2 must log the fast-threshold crossing exactly once, got %d", n)
	}
}

func TestReconcileFirstTickExhaustionRecoversCrossing(t *testing.T) {
	x := newMarkerFixture()
	cur := testNow
	x.tickAt(&cur)
	x.add(time.Minute)
	fastRow := store.ListGaveUpColumnMovesRow{ID: uuid.New(), Status: "failed", MovePendingSince: ts(testNow.Add(-x.l.fastWindow - 10*time.Second))}
	giveRow := store.ListGaveUpColumnMovesRow{ID: uuid.New(), Status: "failed", MovePendingSince: ts(testNow.Add(-x.l.giveup - 10*time.Second))}
	x.fs.gaveUpRows = []store.ListGaveUpColumnMovesRow{fastRow, giveRow}
	blocking := true
	x.fm.autoMoveHook = func(ctx context.Context, _ store.Issue, _ string) {
		if blocking {
			<-ctx.Done()
		}
	}
	logs := captureSlog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	x.l.Reconcile(ctx)

	if len(x.fs.gaveUpArgs) != 0 {
		t.Fatalf("an expired parent must skip the warn scans, got %d calls", len(x.fs.gaveUpArgs))
	}

	blocking = false
	cur = cur.Add(x.l.interval)
	x.l.Reconcile(context.Background())

	if n := logLinesWith(logs, fastCrossMsg, fastRow.ID.String()); n != 1 {
		t.Fatalf("the fast-threshold crossing must be logged once on tick 2, got %d", n)
	}
	if n := logLinesWith(logs, giveUpMsg, giveRow.ID.String()); n != 1 {
		t.Fatalf("the give-up crossing must be logged once on tick 2, got %d", n)
	}

	logs.Reset()
	cur = cur.Add(x.l.interval)
	x.l.Reconcile(context.Background())

	if n := logLinesWith(logs, fastCrossMsg) + logLinesWith(logs, giveUpMsg); n != 0 {
		t.Fatalf("tick 3 must log no crossing, got %d", n)
	}
}

func TestWarnCrossingWindows(t *testing.T) {
	crossingRow := func(pending time.Time) store.ListGaveUpColumnMovesRow {
		return store.ListGaveUpColumnMovesRow{ID: uuid.New(), Status: "failed", MovePendingSince: ts(pending)}
	}

	t.Run("first scan cutoffs", func(t *testing.T) {
		fs := &fakeStore{}
		l := newTestLifecycle(fs, &fakeMover{})
		l.Reconcile(context.Background())
		if len(fs.gaveUpArgs) != 2 {
			t.Fatalf("want one scan per threshold, got %d", len(fs.gaveUpArgs))
		}
		for i, x := range []time.Duration{l.fastWindow, l.giveup} {
			a := fs.gaveUpArgs[i]
			if !a.GiveupCutoff.Time.Equal(testNow.Add(-x)) || !a.PriorCutoff.Time.Equal(testNow.Add(-l.interval-x)) {
				t.Fatalf("threshold %v first scan = %+v", x, a)
			}
		}
	})

	t.Run("delayed ticks abut", func(t *testing.T) {
		x := newMarkerFixture()
		cur := testNow
		x.tickAt(&cur)
		var rows []store.ListGaveUpColumnMovesRow
		for _, th := range []time.Duration{x.l.fastWindow, x.l.giveup} {
			// 10s and 60s sit in the gaps a fixed one-interval window would leave
			// between the delayed ticks (now, +45s, +105s).
			for _, off := range []time.Duration{0, 10 * time.Second, 20 * time.Second, 45 * time.Second, 60 * time.Second, 100 * time.Second, 105 * time.Second} {
				rows = append(rows, crossingRow(testNow.Add(off-th)))
			}
		}
		x.fs.gaveUpRows = rows
		logs := captureSlog(t)

		x.l.Reconcile(context.Background())
		cur = testNow.Add(45 * time.Second)
		x.l.Reconcile(context.Background())
		cur = testNow.Add(105 * time.Second)
		x.l.Reconcile(context.Background())

		for _, r := range rows {
			if n := logLinesWith(logs, r.ID.String()); n != 1 {
				t.Fatalf("row pending %v logged %d times, want exactly once", r.MovePendingSince.Time, n)
			}
		}
		if n := logLinesWith(logs, fastCrossMsg); n != 7 {
			t.Fatalf("fast-threshold lines = %d, want 7", n)
		}
		if n := logLinesWith(logs, giveUpMsg); n != 7 {
			t.Fatalf("give-up lines = %d, want 7", n)
		}
	})

	t.Run("failed scan keeps its cursor", func(t *testing.T) {
		x := newMarkerFixture()
		cur := testNow
		x.tickAt(&cur)
		fastRow := crossingRow(testNow.Add(-x.l.fastWindow - 5*time.Second))
		giveRow := crossingRow(testNow.Add(-x.l.giveup - 10*time.Second))
		x.fs.gaveUpRows = []store.ListGaveUpColumnMovesRow{fastRow, giveRow}
		x.fs.gaveUpHook = func(_ context.Context, _ store.ListGaveUpColumnMovesParams, n int) gaveUpResult {
			if n == 2 { // tick 1's give-up scan
				return gaveUpResult{err: errors.New("db down"), handled: true}
			}
			return gaveUpResult{}
		}
		logs := captureSlog(t)

		x.l.Reconcile(context.Background())
		if n := logLinesWith(logs, fastCrossMsg, fastRow.ID.String()); n != 1 {
			t.Fatalf("tick 1 must log the fast row once, got %d", n)
		}
		if n := logLinesWith(logs, "list pending-move crossings"); n != 1 {
			t.Fatalf("the failed scan must be logged once, got %d", n)
		}
		cur = cur.Add(x.l.interval)
		x.l.Reconcile(context.Background())
		cur = cur.Add(x.l.interval)
		x.l.Reconcile(context.Background())

		if n := logLinesWith(logs, giveUpMsg, giveRow.ID.String()); n != 1 {
			t.Fatalf("the give-up row must be logged exactly once after the retry, got %d", n)
		}
		if n := logLinesWith(logs, fastCrossMsg, fastRow.ID.String()); n != 1 {
			t.Fatalf("the fast row must not be logged again, got %d", n)
		}
	})

	t.Run("capped scan advances to the last row", func(t *testing.T) {
		x := newMarkerFixture()
		cur := testNow
		x.tickAt(&cur)
		base := testNow.Add(-x.l.fastWindow - 30*time.Second + 100*time.Millisecond)
		var rows []store.ListGaveUpColumnMovesRow
		for i := 0; i <= warnScanCap; i++ { // one more than the cap
			rows = append(rows, crossingRow(base.Add(time.Duration(i)*200*time.Millisecond)))
		}
		x.fs.gaveUpRows = rows
		logs := captureSlog(t)

		x.l.Reconcile(context.Background())
		if n := logLinesWith(logs, fastCrossMsg); n != warnScanCap {
			t.Fatalf("tick 1 must log the capped 100 rows, got %d", n)
		}
		cur = cur.Add(x.l.interval)
		x.l.Reconcile(context.Background())

		// The fast-threshold scan of tick 2 starts exactly at the 100th row.
		var second store.ListGaveUpColumnMovesParams
		for _, a := range x.fs.gaveUpArgs {
			if a.GiveupCutoff.Time.Equal(cur.Add(-x.l.fastWindow)) {
				second = a
			}
		}
		if !second.PriorCutoff.Time.Equal(rows[warnScanCap-1].MovePendingSince.Time) {
			t.Fatalf("tick 2 prior cutoff = %v, want the 100th row's pending_since %v", second.PriorCutoff.Time, rows[warnScanCap-1].MovePendingSince.Time)
		}
		for _, r := range rows {
			if n := logLinesWith(logs, r.ID.String()); n != 1 {
				t.Fatalf("row %v logged %d times, want once", r.MovePendingSince.Time, n)
			}
		}
	})

	t.Run("rows outside the windows do not log", func(t *testing.T) {
		x := newMarkerFixture()
		var rows []store.ListGaveUpColumnMovesRow
		for _, th := range []time.Duration{x.l.fastWindow, x.l.giveup} {
			rows = append(rows,
				crossingRow(testNow.Add(-th-x.l.interval-time.Second)), // already past: before the window
				crossingRow(testNow.Add(-th+time.Second)),              // not crossed yet
			)
		}
		x.fs.gaveUpRows = rows
		logs := captureSlog(t)

		x.l.Reconcile(context.Background())

		if n := logLinesWith(logs, fastCrossMsg) + logLinesWith(logs, giveUpMsg); n != 0 {
			t.Fatalf("no row is inside a window, got %d lines", n)
		}
	})
}

func TestLifecycleTierConstants(t *testing.T) {
	if defaultFastWindow != 30*time.Minute || defaultGiveup != 24*time.Hour ||
		defaultSlowInterval != 10*time.Minute || defaultSlowBatch != 25 ||
		defaultSlowTimeout != 10*time.Second || defaultWarnTimeout != 2*time.Second ||
		warnScanCap != 100 {
		t.Fatalf("tier constants drifted: fast=%v giveup=%v slowInterval=%v slowBatch=%d slowTimeout=%v warnTimeout=%v cap=%d",
			defaultFastWindow, defaultGiveup, defaultSlowInterval, defaultSlowBatch, defaultSlowTimeout, defaultWarnTimeout, warnScanCap)
	}
}

// ── autopilot terminal comments (PRD #19 M5) ───────────────────────────────────

// autopilotMoveCtx is moveCtx with the autopilot facts the terminal-comment hook
// reads (auto_approve, and the MR link parts on success).
func autopilotMoveCtx(repoID uuid.UUID, iid int64, status string, mrIID int64, webURL string) store.GetRunMoveContextRow {
	mc := moveCtx(repoID, iid, status, txt("Later"), nullText())
	mc.AutoApprove = true
	if mrIID > 0 {
		mc.MrIid = pgtype.Int8{Int64: mrIID, Valid: true}
	}
	mc.RepoWebUrl = webURL
	return mc
}

func TestTerminalCommentWorkerFailurePostsRunLink(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "failed", 0, ""),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "failed")

	if fs.claimCalls != 1 {
		t.Fatalf("expected one atomic claim, got %d", fs.claimCalls)
	}
	if fm.forge == nil || len(fm.forge.notes) != 1 {
		t.Fatalf("expected one failure comment, got %v", fm.forge)
	}
	note := fm.forge.notes[0]
	if note.issueIID != 4 || note.projectID != 77 {
		t.Fatalf("comment posted to wrong issue/project: %+v", note)
	}
	if !strings.Contains(note.body, "could not complete") {
		t.Fatalf("failure comment missing fixed reason: %q", note.body)
	}
	if !strings.Contains(note.body, testOrigin+"/runs/"+runID.String()) {
		t.Fatalf("failure comment missing run link: %q", note.body)
	}
	// Fixed template only — the untrusted issue text must never be echoed.
	if strings.Contains(note.body, "PRD") {
		t.Fatalf("failure comment must not interpolate issue labels/text: %q", note.body)
	}
}

func TestTerminalCommentSweeperFailurePostsViaReconcile(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "failed", 0, ""),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	// The sweeper never calls the inline notify; it stamps move_pending_since and the
	// reconcile loop is the funnel. reconcileOne stands in for one reconcile pass.
	l.reconcileOne(context.Background(), runID)

	if fs.claimCalls != 1 || fm.forge == nil || len(fm.forge.notes) != 1 {
		t.Fatalf("sweeper path must post exactly one comment via reconcile, claims=%d forge=%v", fs.claimCalls, fm.forge)
	}
}

// A card-less autopilot run with a repo (a scheduled prompt/task, or ci_fix) carries a
// NULL issue_iid. If a stale marker brings it to the reconcile loop, reconcileOne must
// clear the marker and neither move a card nor post a terminal comment to issue 0, the
// way notifyOnce already skips it (issue #1482).
func TestReconcileCardlessRunClearsMarkerWithoutComment(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	mc := autopilotMoveCtx(repoID, 0, "failed", 0, "")
	mc.IssueIid = pgtype.Int8{}
	fs := &fakeStore{moveCtx: mc, columns: defaultCols()}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.reconcileOne(context.Background(), runID)

	if fs.claimCalls != 0 || (fm.forge != nil && len(fm.forge.notes) != 0) {
		t.Fatalf("card-less run must not claim or post a terminal comment, claims=%d forge=%v", fs.claimCalls, fm.forge)
	}
	if len(fm.moves) != 0 || len(fs.recorded) != 0 {
		t.Fatalf("card-less run must not move a card: moves=%v recorded=%v", fm.moves, fs.recorded)
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatalf("card-less run's stale marker must be cleared once, got %v", fs.cleared)
	}
}

func TestTerminalCommentSuccessPostsMRLink(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "completed", 42, "https://gitlab.example.com/group/proj"),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "completed")

	if fm.forge == nil || len(fm.forge.notes) != 1 {
		t.Fatalf("expected one success comment, got %v", fm.forge)
	}
	body := fm.forge.notes[0].body
	if !strings.Contains(body, "merge request") || !strings.Contains(body, "!42") ||
		!strings.Contains(body, "https://gitlab.example.com/group/proj/-/merge_requests/42") {
		t.Fatalf("success comment missing the MR link: %q", body)
	}
}

func TestTerminalCommentNonAutopilotSilent(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		// auto_approve defaults false → a manual run.
		moveCtx: moveCtx(repoID, 4, "completed", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "completed")

	if fs.claimCalls != 0 {
		t.Fatalf("a manual run must never claim a terminal comment, got %d", fs.claimCalls)
	}
	if fm.forge != nil && len(fm.forge.notes) != 0 {
		t.Fatalf("a manual run must never comment, got %d", len(fm.forge.notes))
	}
}

func TestTerminalCommentCancelledSilent(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "cancelled", 0, ""),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "cancelled")

	if fs.claimCalls != 0 {
		t.Fatalf("cancelled is a human stop, not an autopilot outcome — no comment, got %d", fs.claimCalls)
	}
}

func TestTerminalCommentClaimedOnceAcrossPaths(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "failed", 0, ""),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	// Worker path claims and posts; the reconcile retry then finds the claim taken.
	l.notifyOnce(context.Background(), runID, "failed")
	l.reconcileOne(context.Background(), runID)

	if fs.claimCalls != 2 {
		t.Fatalf("both paths attempt the atomic claim, got %d", fs.claimCalls)
	}
	if len(fm.forge.notes) != 1 {
		t.Fatalf("exactly one comment across both paths, got %d", len(fm.forge.notes))
	}
}

func TestTerminalCommentRecordFailsNoComment(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx:  autopilotMoveCtx(repoID, 4, "failed", 0, ""),
		issue:    issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns:  defaultCols(),
		claimErr: errors.New("db unavailable"),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)

	l.notifyOnce(context.Background(), runID, "failed")

	// Record-then-comment: a failed record must never post (a comment without its
	// record could double-post on a later pass).
	if fm.forge != nil && len(fm.forge.notes) != 0 {
		t.Fatalf("a failed claim must not post a comment, got %d", len(fm.forge.notes))
	}
}

// ── projector (PRD #364 M5) ───────────────────────────────────────────────────

type fakeProjector struct {
	calls []projectorCall
	err   error
}

type projectorCall struct {
	repoID   uuid.UUID
	issueIID int64
	target   string
}

func (p *fakeProjector) ForwardMove(_ context.Context, repoID uuid.UUID, issueIID int64, target string) error {
	p.calls = append(p.calls, projectorCall{repoID: repoID, issueIID: issueIID, target: target})
	return p.err
}

// A successful apply calls the injected projector with the move's (repo, issue,
// target).
func TestNotifyCallsProjectorOnSuccess(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)
	proj := &fakeProjector{}
	l.SetProjector(proj)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(proj.calls) != 1 {
		t.Fatalf("want projector called once, got %d", len(proj.calls))
	}
	got := proj.calls[0]
	if got.repoID != repoID || got.issueIID != 4 || got.target != board.ColumnInProgress {
		t.Errorf("projector call = %+v, want repo=%v issue=4 target=%q", got, repoID, board.ColumnInProgress)
	}
}

// A projector error is logged and never fails the move (board_column still
// recorded).
func TestNotifyProjectorErrorNonFatal(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)
	l.SetProjector(&fakeProjector{err: errors.New("sync boom")})

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.recorded) != 1 || fs.recorded[0].BoardColumn.String != board.ColumnInProgress {
		t.Fatalf("a projector error must not prevent recording the move, got %+v", fs.recorded)
	}
}

// A nil projector is safe: apply proceeds and never panics.
func TestNotifyNilProjectorSafe(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm) // no SetProjector

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fm.moves) != 1 {
		t.Fatalf("nil projector must not block the move, got %v", fm.moves)
	}
}
