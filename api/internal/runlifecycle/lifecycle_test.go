package runlifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeStore struct {
	moveCtx    store.GetRunMoveContextRow
	moveCtxErr error
	issue      store.Issue
	issueErr   error
	columns    []store.BoardColumn
	pending    []uuid.UUID
	gaveUp     []store.ListGaveUpColumnMovesRow

	recorded   []store.RecordRunColumnMoveParams
	cleared    []uuid.UUID
	pendingArg *store.ListPendingColumnMovesParams
	gaveUpArg  *store.ListGaveUpColumnMovesParams
}

func (f *fakeStore) GetRunMoveContext(context.Context, uuid.UUID) (store.GetRunMoveContextRow, error) {
	return f.moveCtx, f.moveCtxErr
}
func (f *fakeStore) GetIssueByIID(context.Context, store.GetIssueByIIDParams) (store.Issue, error) {
	return f.issue, f.issueErr
}
func (f *fakeStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return f.columns, nil
}
func (f *fakeStore) RecordRunColumnMove(_ context.Context, arg store.RecordRunColumnMoveParams) (int64, error) {
	f.recorded = append(f.recorded, arg)
	return 1, nil
}
func (f *fakeStore) ClearRunMovePending(_ context.Context, id uuid.UUID) (int64, error) {
	f.cleared = append(f.cleared, id)
	return 1, nil
}
func (f *fakeStore) ListPendingColumnMoves(_ context.Context, arg store.ListPendingColumnMovesParams) ([]uuid.UUID, error) {
	f.pendingArg = &arg
	return f.pending, nil
}
func (f *fakeStore) ListGaveUpColumnMoves(_ context.Context, arg store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error) {
	f.gaveUpArg = &arg
	return f.gaveUp, nil
}

type fakeMover struct {
	forgeErr    error
	autoMoveErr error
	moves       []string
}

func (m *fakeMover) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return nil, m.forgeErr
}
func (m *fakeMover) AutoMove(_ context.Context, _ forge.Forge, _ int64, issue store.Issue, _ []store.BoardColumn, target string) (store.Issue, error) {
	m.moves = append(m.moves, target)
	if m.autoMoveErr != nil {
		return store.Issue{}, m.autoMoveErr
	}
	return issue, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func newTestLifecycle(fs *fakeStore, fm *fakeMover) *Lifecycle {
	l := New(fs, fm)
	l.now = func() time.Time { return testNow }
	return l
}

func txt(s string) pgtype.Text            { return pgtype.Text{String: s, Valid: true} }
func nullText() pgtype.Text               { return pgtype.Text{} }
func ts(t time.Time) pgtype.Timestamptz   { return pgtype.Timestamptz{Time: t, Valid: true} }
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
		IssueIid:         iid,
		RepoID:           repoID,
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

	if fs.pendingArg == nil {
		t.Fatal("ListPendingColumnMoves not queried")
	}
	if !fs.pendingArg.GraceCutoff.Time.Equal(testNow.Add(-l.grace)) {
		t.Fatalf("grace cutoff = %v, want now-grace", fs.pendingArg.GraceCutoff.Time)
	}
	if !fs.pendingArg.GiveupCutoff.Time.Equal(testNow.Add(-l.giveup)) {
		t.Fatalf("giveup cutoff = %v, want now-giveup", fs.pendingArg.GiveupCutoff.Time)
	}
	if fs.pendingArg.MaxBatch != l.batch {
		t.Fatalf("batch = %d, want %d", fs.pendingArg.MaxBatch, l.batch)
	}
}

func TestReconcileGiveUpLeavesMarker(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{
		gaveUp: []store.ListGaveUpColumnMovesRow{
			{ID: runID, RepoID: uuid.New(), IssueIid: 9, Status: "completed", MovePendingSince: ts(testNow.Add(-40 * time.Minute))},
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
	if fs.gaveUpArg == nil {
		t.Fatal("ListGaveUpColumnMoves not queried")
	}
	// The just-crossed window is (now-giveup-interval, now-giveup], so each marker
	// warns exactly once as it crosses the boundary.
	if !fs.gaveUpArg.GiveupCutoff.Time.Equal(testNow.Add(-l.giveup)) {
		t.Fatalf("giveup cutoff = %v, want now-giveup", fs.gaveUpArg.GiveupCutoff.Time)
	}
	if !fs.gaveUpArg.PriorCutoff.Time.Equal(testNow.Add(-l.giveup - l.interval)) {
		t.Fatalf("prior cutoff = %v, want now-giveup-interval", fs.gaveUpArg.PriorCutoff.Time)
	}
}
