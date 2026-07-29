package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// The Slack answer body (PRD #88 M3). slacksvc hands the adapter a question id and a
// reply; the adapter is what turns them into the wire body, so this is where the two
// behavioural claims live.
//
// Deliberately NOT a field-name round-trip: the body is marshalled from
// workersvc.AnswerBody and parsed back into it, so asserting the tags match would be a
// tautology. There is exactly one declaration of this shape — that is the point of
// routing the marshal through main rather than letting slacksvc declare its own — so
// there is nothing left for a round-trip to discriminate. What CAN be wrong is the
// arity and the passthrough, and both are asserted against the raw JSON as well, since
// the worker reads the persisted text and not this struct.
func TestAnswerInputBodyShape(t *testing.T) {
	raw, err := answerInputBody("q-7", "use redis")
	if err != nil {
		t.Fatalf("encode answer body: %v", err)
	}

	var got workersvc.AnswerBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the body must parse as the type the server validates: %v (%s)", err, raw)
	}
	// Arity: one reply is one answer, at index 0. Not zero (the lead would see the
	// question as unanswered) and not repeated per question (which would claim the same
	// prose answers each of them).
	if len(got.Answers) != 1 || got.Answers[0] != "use redis" {
		t.Fatalf("the reply must land as exactly one answer at index 0: %+v (%s)", got.Answers, raw)
	}
	// Passthrough: the id is compared for equality against runs.open_question_id and
	// never parsed for meaning, so any normalisation here breaks the identity guard
	// silently — the answer would be refused as stale for a question the user answered.
	if got.QuestionID != "q-7" {
		t.Fatalf("the question id must pass through unmodified, got %q from %s", got.QuestionID, raw)
	}
	// The worker reads the persisted TEXT, so pin the wire keys too — a struct-level
	// assertion alone cannot see a tag rename that both sides share.
	for _, key := range []string{`"question_id"`, `"answers"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("the wire body must carry %s: %s", key, raw)
		}
	}
}

// An id or reply carrying JSON metacharacters is escaped by the encoder rather than
// breaking the body — the reply is untrusted free text from Slack, and a hand-built
// body would be an injection point here.
func TestAnswerInputBodyEscapesUntrustedText(t *testing.T) {
	raw, err := answerInputBody(`q"1`, "use \"redis\", not {memcached}")
	if err != nil {
		t.Fatalf("encode answer body: %v", err)
	}
	var got workersvc.AnswerBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("a reply carrying quotes must still produce parseable JSON: %v (%s)", err, raw)
	}
	if got.QuestionID != `q"1` || got.Answers[0] != "use \"redis\", not {memcached}" {
		t.Fatalf("values must survive escaping intact: %+v", got)
	}
}
