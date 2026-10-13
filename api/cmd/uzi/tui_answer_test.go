package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type answerFixtureStep struct {
	Question int
	Option   *int
	Text     *string
	Answers  []string
	Ready    bool
}

func TestAnswerSharedCorpus(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-question-answer.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Composition []struct {
			Name     string
			Selected []string
			Text     string
			Answer   string
		}
		Readiness []struct {
			Name    string
			Answers []string
			Count   int
			Ready   bool
		}
		Actions []struct {
			Name    string
			Payload json.RawMessage
			Steps   []answerFixtureStep
			Answers []string
			Ready   bool
		}
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Composition) == 0 || len(corpus.Readiness) == 0 || len(corpus.Actions) == 0 {
		t.Fatal("answer corpus sections must be nonempty")
	}
	for _, c := range corpus.Composition {
		t.Run("compose/"+c.Name, func(t *testing.T) {
			if got := composeAnswer(c.Selected, c.Text); got != c.Answer {
				t.Fatalf("got %q, want %q", got, c.Answer)
			}
		})
	}
	for _, c := range corpus.Readiness {
		t.Run("ready/"+c.Name, func(t *testing.T) {
			if got := answersReady(c.Answers, c.Count); got != c.Ready {
				t.Fatalf("got %v, want %v", got, c.Ready)
			}
		})
	}
	for _, c := range corpus.Actions {
		t.Run("actions/"+c.Name, func(t *testing.T) {
			p, ok := parseAnswerableQuestion(c.Payload)
			if !ok {
				t.Fatal("valid fixture rejected")
			}
			var identity struct {
				QuestionID string `json:"question_id"`
			}
			if err := json.Unmarshal(c.Payload, &identity); err != nil {
				t.Fatal(err)
			}
			selected := make([][]bool, len(p.Questions))
			texts := make([]string, len(p.Questions))
			check := func(answers []string, ready bool) {
				t.Helper()
				body := composeQuestionAnswers(p, selected, texts)
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				var wire answerBody
				if err := json.Unmarshal(encoded, &wire); err != nil {
					t.Fatal(err)
				}
				if wire.QuestionID != identity.QuestionID || !reflect.DeepEqual(wire.Answers, answers) {
					t.Fatalf("wire got %#v, want id %q answers %#v", wire, identity.QuestionID, answers)
				}
				if got := answersReady(body.Answers, len(p.Questions)); got != ready {
					t.Fatalf("readiness got %v, want %v", got, ready)
				}
			}
			if len(c.Steps) == 0 {
				check(c.Answers, c.Ready)
			}
			for _, step := range c.Steps {
				if step.Option != nil {
					qi := step.Question
					selected[qi] = toggleAnswerOption(p.Questions[qi], selected[qi], *step.Option)
				} else if step.Text != nil {
					texts[step.Question] = *step.Text
				} else {
					t.Fatal("fixture action has neither option nor text")
				}
				check(step.Answers, step.Ready)
			}
		})
	}
}

func TestQuestionAnswerableParser(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `"question"`, `{`,
		`{"questions":[{"question":"valid"}]}`,
		`{"question_id":3,"questions":[{"question":"valid"}]}`,
		`{"question_id":"\ufeff ","questions":[{"question":"valid"}]}`,
		`{"question_id":"q","questions":[]}`,
		`{"question_id":"q","questions":null}`,
		`{"question_id":"q","questions":{}}`,
		`{"question_id":"q","questions":[{"question":"valid"},null,{"question":"later"}]}`,
		`{"question_id":"q","questions":[42,{"question":"later"}]}`,
		`{"question_id":"q","questions":[{},{"question":"later"}]}`,
		`{"question_id":"q","questions":[{"question":false},{"question":"later"}]}`,
		`{"question_id":"q","questions":[{"question":"\ufeff "},{"question":"later"}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, ok := parseAnswerableQuestion(json.RawMessage(raw)); ok {
				t.Fatal("malformed payload accepted")
			}
		})
	}
	raw := json.RawMessage(`{
		"question_id":" \ufeffidentity\ufeff ",
		"questions":[
			{"question":" original prose ","header":42,"multiSelect": true,
			 "options":[null,42,{},{"label":false},{"label":"\ufeff "},
			            {"label":"\ufeff A \ufeff","description":7},{"label":"\u0085","description":"kept"}]},
			{"question":"\u0085","multiSelect":"true","options":{}}
		]
	}`)
	p, ok := parseAnswerableQuestion(raw)
	want := questionPayload{
		QuestionID: " \ufeffidentity\ufeff ",
		Questions: []answerQuestion{
			{Question: " original prose ", MultiSelect: true, Options: []answerOption{{Label: "A"}, {Label: "\u0085", Description: "kept"}}},
			{Question: "\u0085", Options: []answerOption{}},
		},
	}
	if !ok || !reflect.DeepEqual(p, want) {
		t.Fatalf("got %#v, ok %v; want %#v", p, ok, want)
	}
	// The CLI still reads multiSelect without adopting the TUI's strict parser.
	var cli questionPayload
	if err := json.Unmarshal(raw, &cli); err == nil {
		t.Fatal("typed CLI should retain its existing malformed-field rejection")
	}
	if err := json.Unmarshal([]byte(`{"question_id":"q","questions":[{"question":"Q","multiSelect":true}]}`), &cli); err != nil || !cli.Questions[0].MultiSelect {
		t.Fatalf("CLI multiSelect not read: %#v, %v", cli, err)
	}
}

func TestAnswerOptionIndexes(t *testing.T) {
	q := answerQuestion{Options: []answerOption{{Label: "same"}, {Label: "same"}}, MultiSelect: true}
	first := toggleAnswerOption(q, nil, 1)
	second := toggleAnswerOption(q, first, 0)
	if !reflect.DeepEqual(first, []bool{false, true}) || !reflect.DeepEqual(second, []bool{true, true}) {
		t.Fatalf("index selection or immutable toggle failed: %v %v", first, second)
	}
	for _, index := range []int{-1, 2} {
		if got := toggleAnswerOption(q, first, index); !reflect.DeepEqual(got, first) {
			t.Fatalf("invalid index %d changed selection: %v", index, got)
		}
	}
}
