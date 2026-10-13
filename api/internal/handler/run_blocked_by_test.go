package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// blockedByStore wraps runsStore with scripted answers for the three queries behind the
// run-detail blocked-by hint (PRD #2602 M2), recording every call and its params.
type blockedByStore struct {
	*runsStore

	question    []byte
	questionErr error
	questionN   int

	answered  bool
	answerErr error
	answerN   int
	answerArg store.RunQuestionAnswerExistsParams

	blocker    uuid.UUID
	blockerErr error
	blockN     int
	blockArg   store.GetMaybeBlockingRunParams
}

func (s *blockedByStore) GetLatestRunQuestion(_ context.Context, _ uuid.UUID) ([]byte, error) {
	s.questionN++
	if s.questionErr != nil {
		return nil, s.questionErr
	}
	return s.question, nil
}

func (s *blockedByStore) RunQuestionAnswerExists(_ context.Context, arg store.RunQuestionAnswerExistsParams) (bool, error) {
	s.answerN++
	s.answerArg = arg
	return s.answered, s.answerErr
}

func (s *blockedByStore) GetMaybeBlockingRun(_ context.Context, arg store.GetMaybeBlockingRunParams) (uuid.UUID, error) {
	s.blockN++
	s.blockArg = arg
	if s.blockerErr != nil {
		return uuid.Nil, s.blockerErr
	}
	return s.blocker, nil
}

// forgeLookupFailsDB is the DBTX behind h.q for these tests: GetRun resolves a repo-ful run's
// forge type through h.q (best-effort), which these tests do not exercise, so that one
// lookup fails with a logged error rather than needing a database.
type forgeLookupFailsDB struct{ store.DBTX }

type blockedByErrRow struct{}

func (blockedByErrRow) Scan(...any) error { return errors.New("no database in this test") }

func (forgeLookupFailsDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return blockedByErrRow{}
}

func (s *blockedByStore) totalCalls() int { return s.questionN + s.answerN + s.blockN }

// questionPayloadJSON builds a `question` message payload with the given question id and
// question texts.
func questionPayloadJSON(t *testing.T, questionID string, texts ...string) []byte {
	t.Helper()
	type item struct {
		Question string `json:"question"`
	}
	items := make([]item, 0, len(texts))
	for _, x := range texts {
		items = append(items, item{Question: x})
	}
	b, err := json.Marshal(map[string]any{"question_id": questionID, "questions": items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// blockedByFixture is a parked awaiting_input issue run (#10) with open question "q1",
// owned by owner, plus a store scripted to find blocker for a question mentioning #2512.
type blockedByFixture struct {
	owner   store.User
	repoID  uuid.UUID
	run     store.Run
	blocker uuid.UUID
	st      *blockedByStore
}

func newBlockedByFixture(t *testing.T) *blockedByFixture {
	t.Helper()
	owner := store.User{ID: uuid.New()}
	repoID := uuid.New()
	run := store.Run{
		ID:             uuid.New(),
		UserID:         owner.ID,
		RepoID:         pgtype.UUID{Bytes: repoID, Valid: true},
		Kind:           "issue",
		IssueIid:       pgtype.Int8{Int64: 10, Valid: true},
		Status:         "awaiting_input",
		OpenQuestionID: pgtype.Text{String: "q1", Valid: true},
	}
	blocker := uuid.New()
	return &blockedByFixture{
		owner:   owner,
		repoID:  repoID,
		run:     run,
		blocker: blocker,
		st: &blockedByStore{
			runsStore: &runsStore{ownerID: owner.ID, run: run},
			question:  questionPayloadJSON(t, "q1", "Should I wait for #2512 to merge first?"),
			blocker:   blocker,
		},
	}
}

// get serves GET /api/runs/{id} as viewer and returns progress.maybe_blocked_by_run_id
// (nil when the key is absent) after asserting the progress object is present.
func (f *blockedByFixture) get(t *testing.T, viewer store.User) *string {
	t.Helper()
	f.st.run = f.run
	h := newRunsHandler(t, f.st)
	h.q = store.New(forgeLookupFailsDB{})
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(viewer, f.run.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRun = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run struct {
			Progress *struct {
				MaybeBlockedByRunID *string `json:"maybe_blocked_by_run_id"`
			} `json:"progress"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Run.Progress == nil {
		t.Fatalf("progress = null for a non-terminal run: %s", rec.Body.String())
	}
	return body.Run.Progress.MaybeBlockedByRunID
}

func TestGetRunMaybeBlockedBy(t *testing.T) {
	t.Run("found: names the blocker and scopes the lookup to the run's owner and repo", func(t *testing.T) {
		f := newBlockedByFixture(t)
		got := f.get(t, f.owner)
		if got == nil || *got != f.blocker.String() {
			t.Fatalf("maybe_blocked_by_run_id = %v, want %s", got, f.blocker)
		}
		arg := f.st.blockArg
		if arg.Owner != f.owner.ID || arg.RepoID != f.repoID || arg.RunID != f.run.ID {
			t.Errorf("params owner/repo/run = %s/%s/%s, want %s/%s/%s", arg.Owner, arg.RepoID, arg.RunID, f.owner.ID, f.repoID, f.run.ID)
		}
		if !reflect.DeepEqual(arg.Iids, []int64{2512}) || !reflect.DeepEqual(arg.Refs, []string{"agent/issue-2512"}) {
			t.Errorf("params iids/refs = %v/%v, want [2512]/[agent/issue-2512]", arg.Iids, arg.Refs)
		}
		if f.st.answerArg.RunID != f.run.ID || f.st.answerArg.QuestionID.String != "q1" {
			t.Errorf("answer lookup = %+v, want run %s question q1", f.st.answerArg, f.run.ID)
		}
	})

	t.Run("admin viewing another user's run: lookup is scoped to the run's owner, not the viewer", func(t *testing.T) {
		f := newBlockedByFixture(t)
		admin := store.User{ID: uuid.New(), IsAdmin: true}
		got := f.get(t, admin)
		if got == nil {
			t.Fatalf("hint absent for an admin viewer; the owner's blocker should still resolve")
		}
		if f.st.blockArg.Owner != f.owner.ID {
			t.Errorf("Owner = %s, want the run's owner %s (viewer %s)", f.st.blockArg.Owner, f.owner.ID, admin.ID)
		}
		if f.st.blockArg.Owner == admin.ID {
			t.Errorf("lookup used the viewer id")
		}
	})

	t.Run("not found: no matching run leaves the field absent", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.blockerErr = pgx.ErrNoRows
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.blockN != 1 {
			t.Errorf("blocking-run lookups = %d, want 1", f.st.blockN)
		}
	})

	t.Run("lookup error is best-effort: the read still succeeds without the field", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.blockerErr = errors.New("boom")
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
	})

	t.Run("newest question id differs from open_question_id: absent, no further queries", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.question = questionPayloadJSON(t, "q2", "wait for #2512?")
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.answerN != 0 || f.st.blockN != 0 {
			t.Errorf("answer/block lookups = %d/%d, want 0/0", f.st.answerN, f.st.blockN)
		}
	})

	t.Run("no question message: absent", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.questionErr = pgx.ErrNoRows
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.blockN != 0 {
			t.Errorf("blocking-run lookups = %d, want 0", f.st.blockN)
		}
	})

	t.Run("answer submitted but not yet consumed: absent, no blocking lookup", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.answered = true
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.blockN != 0 {
			t.Errorf("blocking-run lookups = %d, want 0", f.st.blockN)
		}
	})

	t.Run("mr_rework match is by pipeline_ref: the refs carry agent/issue-N", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.question = questionPayloadJSON(t, "q1", "Is the rework of #77 finished, and what about #78?")
		if got := f.get(t, f.owner); got == nil {
			t.Fatalf("hint absent")
		}
		want := []string{"agent/issue-77", "agent/issue-78"}
		if !reflect.DeepEqual(f.st.blockArg.Refs, want) || !reflect.DeepEqual(f.st.blockArg.Iids, []int64{77, 78}) {
			t.Errorf("refs/iids = %v/%v, want %v/[77 78]", f.st.blockArg.Refs, f.st.blockArg.Iids, want)
		}
	})

	t.Run("question mentioning only the run's own issue: absent and no blocking lookup", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.question = questionPayloadJSON(t, "q1", "Is #10 what you meant?")
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.blockN != 0 {
			t.Errorf("blocking-run lookups = %d, want 0", f.st.blockN)
		}
	})

	t.Run("over-long and zero references are dropped before the lookup", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.st.question = questionPayloadJSON(t, "q1", "see #0, #1234567890 and #42")
		if got := f.get(t, f.owner); got == nil {
			t.Fatalf("hint absent")
		}
		if !reflect.DeepEqual(f.st.blockArg.Iids, []int64{42}) || !reflect.DeepEqual(f.st.blockArg.Refs, []string{"agent/issue-42"}) {
			t.Errorf("iids/refs = %v/%v, want only 42", f.st.blockArg.Iids, f.st.blockArg.Refs)
		}

		f = newBlockedByFixture(t)
		f.st.question = questionPayloadJSON(t, "q1", "#0 and #1234567890 only")
		if got := f.get(t, f.owner); got != nil {
			t.Errorf("maybe_blocked_by_run_id = %s, want absent", *got)
		}
		if f.st.blockN != 0 {
			t.Errorf("blocking-run lookups = %d, want 0 when no usable reference remains", f.st.blockN)
		}
	})

	t.Run("a run without an issue number keeps every reference", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.run.IssueIid = pgtype.Int8{}
		f.run.Kind = "task"
		f.st.question = questionPayloadJSON(t, "q1", "wait for #10?")
		if got := f.get(t, f.owner); got == nil {
			t.Fatalf("hint absent")
		}
		if !reflect.DeepEqual(f.st.blockArg.Iids, []int64{10}) {
			t.Errorf("iids = %v, want [10]", f.st.blockArg.Iids)
		}
	})

	t.Run("status other than awaiting_input: no queries at all", func(t *testing.T) {
		for _, status := range []string{"running", "queued", "awaiting_approval", "limit_wait", "completed"} {
			f := newBlockedByFixture(t)
			f.run.Status = status
			f.st.run = f.run
			h := newRunsHandler(t, f.st)
			h.q = store.New(forgeLookupFailsDB{})
			rec := httptest.NewRecorder()
			h.GetRun(rec, runReq(f.owner, f.run.ID))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: GetRun = %d", status, rec.Code)
			}
			if f.st.totalCalls() != 0 {
				t.Errorf("%s: hint queries = %d, want 0", status, f.st.totalCalls())
			}
		}
	})

	t.Run("run without a repo or an open question id: no queries", func(t *testing.T) {
		f := newBlockedByFixture(t)
		f.run.RepoID = pgtype.UUID{}
		f.get(t, f.owner)
		if f.st.totalCalls() != 0 {
			t.Errorf("no repo: hint queries = %d, want 0", f.st.totalCalls())
		}
		f = newBlockedByFixture(t)
		f.run.OpenQuestionID = pgtype.Text{}
		f.get(t, f.owner)
		if f.st.totalCalls() != 0 {
			t.Errorf("no open question: hint queries = %d, want 0", f.st.totalCalls())
		}
	})
}

// TestListRunsNeverRunsBlockedByLookup pins "the list endpoint never runs it": a page
// holding an awaiting_input run reads no hint queries and carries no hint field.
func TestListRunsNeverRunsBlockedByLookup(t *testing.T) {
	f := newBlockedByFixture(t)
	f.st.userRuns = []store.ListRunsForUserRow{{Run: f.run}}
	h := newRunsHandler(t, f.st)
	rec := httptest.NewRecorder()
	req := runReq(f.owner, uuid.Nil)
	h.ListRuns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d: %s", rec.Code, rec.Body.String())
	}
	if f.st.totalCalls() != 0 {
		t.Errorf("hint queries on a list read = %d, want 0", f.st.totalCalls())
	}
	var body struct {
		Runs []struct {
			Progress map[string]any `json:"progress"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 || body.Runs[0].Progress == nil {
		t.Fatalf("runs = %+v, want one run with progress", body.Runs)
	}
	if _, ok := body.Runs[0].Progress["maybe_blocked_by_run_id"]; ok {
		t.Errorf("list progress carries maybe_blocked_by_run_id")
	}
}
