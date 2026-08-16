package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #88 M6 — SubmitInput's `answer` branch (submitAnswer).
//
// This is the server half of the identity-keyed clarification round trip, and every
// rule it enforces exists because the alternative was measured to be wrong:
//
//   - It is the ONLY steering kind rejected on a non-parked run (D-P C3). Every other
//     kind is accepted on any non-terminal run, so without this an `answer` posted
//     before the lead ever asked would sit in the queue, be consumed by the steering
//     poll, and auto-resolve the first question the instant it opened — the user never
//     sees the question and the feed shows it answered by text written before it existed.
//   - A malformed body FAILS SAFE (D-P C2), deliberately opposite to
//     parseAgentSelection's malformed→"own" fallback. There a default is genuinely
//     safe; here the payload's entire job is to say WHICH question is being answered,
//     so accepting an unidentifiable answer IS the harm.
//   - The named question must be the OPEN one, keyed on identity rather than a clock or
//     an arrival ordinal, because a worker death requeues and re-parks the run and every
//     when-based key rejects an answer the user correctly submitted before the death.
//   - Answer bodies are scrubbed (D-G). #88 is the feature that makes the agent ASK the
//     user for information — precisely the prompt that elicits a credential paste — and
//     the question text is attacker-influenceable, so an injected repo file can make the
//     lead ask for a PAT "to continue". Slack's inbound path already scrubbed; web and
//     CLI did not.

// answerStore is a minimal Store for the submitAnswer path: the run lookup GetRun
// performs, plus the answer enqueue. Deliberately separate from service_test.go's
// fakeStore so these tests own their fixture. Both embed the Store interface, so any
// other method panics rather than silently returning a zero value — a test that
// reaches one has left the path under test.
type answerStore struct {
	Store
	run       store.Run
	runErr    error
	created   *store.CreateRunAnswerInputParams
	createErr error
}

func (a *answerStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if a.runErr != nil {
		return store.Run{}, a.runErr
	}
	if arg.ID != a.run.ID || arg.UserID != a.run.UserID {
		return store.Run{}, errors.New("fixture: run lookup not scoped to the owner")
	}
	return a.run, nil
}

func (a *answerStore) CreateRunAnswerInput(_ context.Context, arg store.CreateRunAnswerInputParams) (store.RunUserInput, error) {
	a.created = &arg
	if a.createErr != nil {
		return store.RunUserInput{}, a.createErr
	}
	return store.RunUserInput{ID: 77, RunID: arg.RunID, Kind: "answer"}, nil
}

// parkedRun is a run waiting on question `qid`.
func parkedRun(user, runID uuid.UUID, qid string) store.Run {
	return store.Run{
		ID:             runID,
		UserID:         user,
		Status:         "awaiting_input",
		OpenQuestionID: pgtype.Text{String: qid, Valid: qid != ""},
	}
}

func answerBody(t *testing.T, qid string, answers ...string) string {
	t.Helper()
	b, err := json.Marshal(AnswerBody{QuestionID: qid, Answers: answers})
	if err != nil {
		t.Fatalf("marshal answer body: %v", err)
	}
	return string(b)
}

// The happy path, and the shape of what is persisted: the server re-encodes from its
// OWN validated values rather than storing the caller's raw text (the rule
// submitApproval already follows), and it records the question id in the column the
// SetRunRunning guard compares against — not only inside the JSON body, which is bare
// `text` shared with prose follow_up bodies and cannot be safely cast in a predicate.
func TestSubmitAnswerPersistsIdentityAndBody(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	qid := uuid.NewString()
	fs := &answerStore{run: parkedRun(user, runID, qid)}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, qid, "use pgx", "yes"), nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("an answer is never a server-side transition — the worker consumes it")
	}
	if fs.created == nil {
		t.Fatal("answer was not enqueued")
	}
	if fs.created.QuestionID.String != qid {
		t.Fatalf("question_id column = %q, want %q — the SetRunRunning guard compares "+
			"this column, so an answer that only carries the id inside the JSON body "+
			"can never satisfy it", fs.created.QuestionID.String, qid)
	}
	var got AnswerBody
	if err := json.Unmarshal([]byte(fs.created.Body.String), &got); err != nil {
		t.Fatalf("persisted body is not the JSON contract: %v (%q)", err, fs.created.Body.String)
	}
	if got.QuestionID != qid {
		t.Fatalf("persisted body question_id = %q, want %q", got.QuestionID, qid)
	}
	if len(got.Answers) != 2 || got.Answers[0] != "use pgx" || got.Answers[1] != "yes" {
		t.Fatalf("persisted answers = %#v, want the two the user wrote, index-aligned", got.Answers)
	}
}

// D-P C3. The one kind rejected on a non-parked run. `running` is the discriminating
// status: it is non-terminal, so every other kind is accepted on it, and the shipped
// terminal guard cannot be what rejects here.
func TestSubmitAnswerRejectedWhenNotParked(t *testing.T) {
	for _, status := range []string{"running", "queued", "claimed", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			user, runID := uuid.New(), uuid.New()
			qid := uuid.NewString()
			run := parkedRun(user, runID, qid)
			run.Status = status
			fs := &answerStore{run: run}
			svc := New(fs, newBox(t), testParams())

			_, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, qid, "sure"), nil)
			if !errors.Is(err, ErrRunNotAwaitingInput) {
				t.Fatalf("err = %v, want ErrRunNotAwaitingInput for a %s run", err, status)
			}
			if fs.created != nil {
				t.Fatalf("nothing may be enqueued for a %s run, got %+v", status, fs.created)
			}
		})
	}
}

// The terminal guard still runs first and returns its own error, so a rejected answer
// on a finished run is distinguishable from one on a live-but-unparked run.
func TestSubmitAnswerOnTerminalRunIsTerminalError(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	run := parkedRun(user, runID, uuid.NewString())
	run.Status = "completed"
	fs := &answerStore{run: run}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, "x", "y"), nil); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("err = %v, want ErrRunTerminal", err)
	}
}

// D-P C2 — malformed FAILS SAFE. Each case is a different way the id can fail to
// identify a question, and all four must reject rather than fall back to "the question
// that happens to be open".
func TestSubmitAnswerMalformedBodyRejects(t *testing.T) {
	qid := uuid.NewString()
	cases := []struct {
		name string
		body string
	}{
		{"bare prose, the shape every OTHER kind uses", "just answer it"},
		{"empty", ""},
		{"valid JSON, no question_id", `{"answers":["yes"]}`},
		{"question_id present but blank", `{"question_id":"   ","answers":["yes"]}`},
		{"truncated JSON", `{"question_id":"` + qid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, runID := uuid.New(), uuid.New()
			fs := &answerStore{run: parkedRun(user, runID, qid)}
			svc := New(fs, newBox(t), testParams())

			_, err := svc.SubmitInput(context.Background(), user, runID, "answer", tc.body, nil)
			if !errors.Is(err, ErrInvalidAnswer) {
				t.Fatalf("err = %v, want ErrInvalidAnswer", err)
			}
			if fs.created != nil {
				t.Fatalf("an unidentifiable answer must never be enqueued, got %+v", fs.created)
			}
		})
	}
}

// The stale-answer guard, and the mirror of the worker-side discard. A reply written
// against Q1 that arrives after Q2 opened names a question that is no longer open.
func TestSubmitAnswerStaleQuestionIDRejects(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	openQID, staleQID := uuid.NewString(), uuid.NewString()
	fs := &answerStore{run: parkedRun(user, runID, openQID)}
	svc := New(fs, newBox(t), testParams())

	_, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, staleQID, "answer to Q1"), nil)
	if !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("err = %v, want ErrStaleAnswer", err)
	}
	if fs.created != nil {
		t.Fatalf("a stale answer must never be enqueued, got %+v", fs.created)
	}
}

// A run parked with NO open question id cannot be resumed by any answer (the
// SetRunRunning guard is unsatisfiable), so an equality test against "" must never
// pass. The empty-vs-empty case is the one a naive `qid != run.OpenQuestionID.String`
// comparison gets wrong, and the client controls both sides of it.
func TestSubmitAnswerEmptyOpenQuestionIDNeverMatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(user, runID uuid.UUID) store.Run
	}{
		{"NULL open_question_id", func(u, r uuid.UUID) store.Run { return parkedRun(u, r, "") }},
		{"present but empty", func(u, r uuid.UUID) store.Run {
			run := parkedRun(u, r, "")
			run.OpenQuestionID = pgtype.Text{String: "", Valid: true}
			return run
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, runID := uuid.New(), uuid.New()
			fs := &answerStore{run: tc.run(user, runID)}
			svc := New(fs, newBox(t), testParams())

			// A body whose question_id is blank is rejected earlier as malformed, so the
			// only way to probe the equality is with a non-empty id — which must also
			// fail, since nothing is open to answer.
			_, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, uuid.NewString(), "hi"), nil)
			if !errors.Is(err, ErrStaleAnswer) {
				t.Fatalf("err = %v, want ErrStaleAnswer", err)
			}
			if fs.created != nil {
				t.Fatalf("nothing may be enqueued, got %+v", fs.created)
			}
		})
	}
}

// D-G — the finding this feature would least want to ship without. The answer body is
// what the user types INTO a prompt the agent authored, so it is the likeliest place in
// the whole product for a credential to be pasted. It is persisted, injected into agent
// context, and mirrored to Slack.
//
// Each secret here is a DIFFERENT pattern in secretscrub, so a scrub wired to only one
// of them cannot pass this test; and each is embedded in surrounding prose, so a test
// asserting the whole body were replaced would not be measuring redaction.
func TestSubmitAnswerScrubsSecretsInEveryAnswer(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	qid := uuid.NewString()
	fs := &answerStore{run: parkedRun(user, runID, qid)}
	svc := New(fs, newBox(t), testParams())

	secrets := []string{
		"glpat-AAAAAAAAAAAAAAAAAAAA",
		"sk-ant-api03-BBBBBBBBBBBBBBBBBBBB",
		"xoxb-111111111111-CCCCCCCCCCCC",
		"uzc_DDDDDDDDDDDDDDDDDDDD",
	}
	answers := make([]string, 0, len(secrets))
	for _, s := range secrets {
		answers = append(answers, "sure, the token is "+s+" — use that one")
	}
	if _, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, qid, answers...), nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.created == nil {
		t.Fatal("answer was not enqueued")
	}
	persisted := fs.created.Body.String
	for _, s := range secrets {
		if strings.Contains(persisted, s) {
			t.Fatalf("secret %q survived into the persisted answer body: %s", s, persisted)
		}
	}
	if !strings.Contains(persisted, "[redacted]") {
		t.Fatalf("expected redaction markers in %s", persisted)
	}
	// Scrubbing must not eat the answer: the surrounding prose is the part the lead
	// actually reads, and a scrub that blanked the field would pass the checks above.
	var got AnswerBody
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("persisted body is not valid JSON after scrubbing: %v", err)
	}
	if len(got.Answers) != len(secrets) {
		t.Fatalf("answers dropped: got %d, want %d", len(got.Answers), len(secrets))
	}
	for i, a := range got.Answers {
		if !strings.HasPrefix(a, "sure, the token is ") || !strings.HasSuffix(a, " — use that one") {
			t.Fatalf("answer %d lost its surrounding prose: %q", i, a)
		}
	}
}

// The length bound (D-G's second half). Runes, not bytes: a cap applied to bytes would
// cut a multi-byte character in half and persist invalid UTF-8.
func TestSubmitAnswerBoundsAnswerLength(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	qid := uuid.NewString()
	fs := &answerStore{run: parkedRun(user, runID, qid)}
	svc := New(fs, newBox(t), testParams())

	long := strings.Repeat("é", maxAnswerBodyRunes+500)
	if _, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, qid, long), nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	var got AnswerBody
	if err := json.Unmarshal([]byte(fs.created.Body.String), &got); err != nil {
		t.Fatalf("persisted body is not valid JSON: %v", err)
	}
	if n := len([]rune(got.Answers[0])); n > maxAnswerBodyRunes {
		t.Fatalf("answer kept %d runes, want at most %d", n, maxAnswerBodyRunes)
	}
	if !strings.ContainsRune(got.Answers[0], 'é') || strings.ContainsRune(got.Answers[0], '�') {
		t.Fatal("truncation must land on a rune boundary, not mid-character")
	}
}

// An `answer` never carries an agent selection — that is approve_plan's field, and
// SubmitInput rejects the combination before the answer branch is reached.
func TestSubmitAnswerRejectsAgentSelection(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	qid := uuid.NewString()
	fs := &answerStore{run: parkedRun(user, runID, qid)}
	svc := New(fs, newBox(t), testParams())

	sel := &AgentSelection{Source: "own"}
	if _, err := svc.SubmitInput(context.Background(), user, runID, "answer", answerBody(t, qid, "yes"), sel); !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("err = %v, want ErrInvalidSelection", err)
	}
	if fs.created != nil {
		t.Fatalf("nothing may be enqueued, got %+v", fs.created)
	}
}
