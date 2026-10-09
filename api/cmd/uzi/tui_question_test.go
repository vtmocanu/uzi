package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func questionViewModel(t *testing.T, payload string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "question-run")
	return applyDetail(m, apitypes.RunDTO{ID: "question-run", Status: "running", IssueTitle: "Question transcript"}, []apitypes.MessageDTO{
		{Seq: 1, Kind: "question", Payload: json.RawMessage(payload), CreatedAt: time.Now()},
	})
}

func TestTUIQuestionViewDisplayShapes(t *testing.T) {
	for _, id := range []string{`"question_id":"q",`, "", `"question_id":{"invalid":true},`} {
		t.Run(id, func(t *testing.T) {
			m := questionViewModel(t, `{`+id+`"questions":[null,4,{"question":" "},{"question":3},{"question":"First body","header":3,"options":[null,{"label":" "},{"label":2},{"label":"  First choice  ","description":7},{"label":"Second choice","description":""}]},{"header":"Second header","question":"Second body","options":"invalid"}]}`)
			out := stripANSI(m.View().Content)
			for _, marker := range []string{"2 questions", "First body", "1. First choice", "2. Second choice", "Second header", "Second body"} {
				if !strings.Contains(out, marker) {
					t.Fatalf("missing %q:\n%s", marker, out)
				}
			}
			if strings.Contains(out, `"questions":[`) {
				t.Fatalf("structured content rendered as raw JSON:\n%s", out)
			}
			// Empty/non-string descriptions add no blank line between consecutive options.
			lane, _ := m.detail.selectedLane()
			transcript := stripANSI(strings.Join(m.transcriptLines(lane), "\n"))
			if !strings.Contains(transcript, "1. First choice\n  2. Second choice") {
				t.Fatalf("empty description added spacing:\n%s", transcript)
			}
		})
	}
}

func TestTUIQuestionViewRawFallback(t *testing.T) {
	for _, payload := range []string{
		`{"questions":[{"question":false}],"fallback":"fallback-marker"}`,
		`{"questions":"wrong","fallback":"fallback-marker"}`,
		`{"questions":[broken fallback-marker`,
	} {
		t.Run(payload, func(t *testing.T) {
			m := questionViewModel(t, payload)
			out := stripANSI(m.View().Content)
			if !strings.Contains(out, "fallback-marker") || !strings.Contains(out, "▪ question") {
				t.Fatalf("unusable question disappeared instead of raw fallback:\n%s", out)
			}
		})
	}
}

func TestTUIQuestionViewLosslessNarrow(t *testing.T) {
	long := strings.Repeat("word ", 48)
	payload := `{"questions":[{"header":` + quoteJSON(long+"header-tail") + `,"question":` + quoteJSON(long+"body-tail") + `,"options":[{"label":` + quoteJSON(long+"label-tail") + `,"description":` + quoteJSON(long+"\n\n"+"description-tail") + `}]}]}`
	m := questionViewModel(t, payload)
	m.width, m.height = 100, 100
	out := stripANSI(m.View().Content)
	for _, marker := range []string{"1 question", "header-tail", "body-tail", "label-tail", "description-tail", "CREW"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing lossless/narrow marker %q:\n%s", marker, out)
		}
	}
	lane, _ := m.detail.selectedLane()
	lines := m.transcriptLines(lane)
	for i, line := range lines {
		if visualWidth(line) > m.transcriptWidth() {
			t.Fatalf("transcript line %d exceeds inner width: %q", i, stripANSI(line))
		}
		if i > 0 && strings.TrimSpace(stripANSI(line)) != "" && !strings.HasPrefix(stripANSI(line), "  ") {
			t.Fatalf("untrusted physical line escaped chrome: %q", stripANSI(line))
		}
	}
	for i, line := range strings.Split(out, "\n") {
		if visualWidth(line) > 100 {
			t.Fatalf("100-column View line %d overflows: %q", i, line)
		}
	}
	before := buildsOf(m)
	_ = m.View()
	if buildsOf(m) != before {
		t.Fatal("unchanged structured question View missed transcript cache")
	}
}

func TestTUIAnswerViewOrderedWithoutAssociations(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "answer-run")
	now := time.Now()
	m = applyDetail(m, apitypes.RunDTO{ID: "answer-run", Status: "running"}, []apitypes.MessageDTO{
		{Seq: 1, Kind: "question", Agent: ptr("reviewer"), Payload: json.RawMessage(`{"questions":[{"header":"Earlier header","question":"Earlier question"}]}`), CreatedAt: now},
		{Seq: 2, Kind: "answer", Payload: json.RawMessage(`{"answers":["First answer",7,null,"Second answer"],"author":"invented-author"}`), CreatedAt: now},
	})
	// Multiple lanes default to the aggregated transcript.
	laneBefore, _ := m.detail.selectedLane()
	if laneBefore.Key != laneAllKey {
		t.Fatal("answer test must exercise aggregated transcript")
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "First answer · Second answer") {
		t.Fatalf("answer order not readable:\n%s", out)
	}
	if strings.Contains(out, `"answers":`) || strings.Contains(out, "invented-author") {
		t.Fatalf("answer payload metadata leaked:\n%s", out)
	}
	lane, _ := m.detail.selectedLane()
	text := stripANSI(strings.Join(m.transcriptLines(lane), "\n"))
	_, answer, found := strings.Cut(text, "✎ answer")
	if !found {
		t.Fatalf("missing answer eyebrow:\n%s", text)
	}
	for _, invented := range []string{"reviewer", "lead", "Earlier header", "Earlier question"} {
		if strings.Contains(answer, invented) {
			t.Fatalf("answer borrowed %q from question:\n%s", invented, answer)
		}
	}
}

func TestTUIAnswerViewLosslessAndFallback(t *testing.T) {
	for _, payload := range []string{
		`{"answers":[` + quoteJSON(strings.Repeat("answer ", 45)+"answer-tail") + `]}`,
		`{"answers":[false,null],"fallback":"answer-fallback"}`,
	} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "answer-run")
		m = applyDetail(m, apitypes.RunDTO{ID: "answer-run", Status: "running"}, []apitypes.MessageDTO{
			{Seq: 1, Kind: "answer", Payload: json.RawMessage(payload), CreatedAt: time.Now()},
		})
		out := stripANSI(m.View().Content)
		marker := "answer-tail"
		if strings.Contains(payload, "answer-fallback") {
			marker = "answer-fallback"
		}
		if !strings.Contains(out, marker) {
			t.Fatalf("answer content disappeared/truncated:\n%s", out)
		}
	}
}

func TestTUIAnswerViewMalformedPayloadDoesNotInheritActor(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "malformed-actor")
	m = applyDetail(m, apitypes.RunDTO{ID: "malformed-actor", Status: "running"}, []apitypes.MessageDTO{
		{Seq: 1, Kind: "question", Agent: ptr("reviewer"), AgentLabel: ptr("old-label"), AgentInstance: ptr("shared-instance"), Payload: json.RawMessage(`{"questions":[{"question":"Earlier question"}]}`), CreatedAt: time.Now()},
		{Seq: 2, Kind: "answer", AgentInstance: ptr("shared-instance"), Payload: json.RawMessage(`{"answers":false,"fallback":"answer-fallback"}`), CreatedAt: time.Now()},
		msgDTO(3, "text", "coder", "", "", "Other lane", time.Now()),
	})
	out := stripANSI(m.View().Content)
	_, answer, ok := strings.Cut(out, "▪ answer")
	if !ok || !strings.Contains(answer, "answer-fallback") {
		t.Fatalf("malformed answer lost raw fallback:\n%s", out)
	}
	if header := strings.SplitN(answer, "\n", 2)[0]; strings.Contains(header, "old-label") || strings.Contains(header, "reviewer") {
		t.Fatalf("malformed answer inferred an actor from its lane: %q", header)
	}
}

func TestTUIQuestionAnswerViewFrameActor(t *testing.T) {
	for _, actor := range []string{"", "answer-actor"} {
		t.Run(actor, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "actor-run")
			m = applyDetail(m, apitypes.RunDTO{ID: "actor-run", Status: "running"}, []apitypes.MessageDTO{
				{Seq: 1, Kind: "question", Agent: ptr("reviewer"), AgentLabel: ptr("old-label"), AgentInstance: ptr("shared-instance"), Payload: json.RawMessage(`{"questions":[{"question":"Actor question"}]}`), CreatedAt: time.Now()},
				{Seq: 2, Kind: "answer", AgentLabel: ptr(actor), AgentInstance: ptr("shared-instance"), Payload: json.RawMessage(`{"answers":["Actor answer"]}`), CreatedAt: time.Now()},
				msgDTO(3, "text", "coder", "", "", "Other lane", time.Now()),
			})
			out := stripANSI(m.View().Content)
			if !strings.Contains(out, "✎ question · old-label") {
				t.Fatalf("question frame actor missing:\n%s", out)
			}
			if !strings.Contains(out, "Actor answer") {
				t.Fatalf("answer missing:\n%s", out)
			}
			_, answer, ok := strings.Cut(out, "✎ answer")
			if !ok {
				t.Fatalf("answer eyebrow missing:\n%s", out)
			}
			head := strings.SplitN(answer, "\n", 2)[0]
			if strings.Contains(head, "old-label") || strings.Contains(head, "reviewer") || strings.Contains(head, "lead") {
				t.Fatalf("answer actor inherited from lane: %q", head)
			}
			if actor == "" && strings.Contains(head, "·") {
				t.Fatalf("invented answer actor: %q", head)
			}
			if actor != "" && !strings.Contains(head, "· "+actor) {
				t.Fatalf("frame-proven answer actor missing: %q", head)
			}
		})
	}
}

func TestTUIQuestionViewMinimumRendererWidth(t *testing.T) {
	m := questionViewModel(t, `{"questions":[{"question":"minimum-width-body","options":[{"label":"Choice","description":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa END"}]}]}`)
	m.width, m.height = 52, 100 // transcriptWidth clamps at 20, as does newTUIRenderer.
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "Choice") || !strings.Contains(out, "END") {
		t.Fatalf("small renderer lost content:\n%s", out)
	}
	lane, _ := m.detail.selectedLane()
	for _, line := range m.transcriptLines(lane) {
		if visualWidth(line) > m.transcriptWidth() {
			t.Fatalf("minimum-width transcript overflow: %q", stripANSI(line))
		}
	}
}

// Reconstruct prose from View's transcript column, so a surviving tail marker
// cannot conceal characters lost where Glamour wraps an unbroken word.
func TestTUIQuestionAnswerViewPreservesWholeWords(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, word := range []string{
			"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789END",
			strings.Repeat("a", 245) + "END",
		} {
			for _, field := range []string{"header", "question", "label", "description", "answer"} {
				t.Run(fmt.Sprintf("dark=%t/%s/%d", dark, field, len(word)), func(t *testing.T) {
					q := map[string]any{"header": "Header", "question": "Choose"}
					option := map[string]any{"label": "Choice", "description": "Details"}
					switch field {
					case "header", "question":
						q[field] = word
					case "label", "description":
						option[field] = word
					}
					q["options"] = []any{option}
					kind := "question"
					payload := map[string]any{"questions": []any{q}}
					if field == "answer" {
						kind = "answer"
						payload = map[string]any{"answers": []string{word}}
					}
					raw, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					m := tuiTestModel(t, &uzicli.FakeClient{}, "whole-word")
					m.width, m.height, m.dark = 100, 100, dark
					m.renderer, err = newTUIRenderer(m.transcriptWidth(), dark)
					if err != nil {
						t.Fatal(err)
					}
					m = applyDetail(m, apitypes.RunDTO{ID: "whole-word", Status: "running"}, []apitypes.MessageDTO{
						{Seq: 1, Kind: kind, Payload: raw, CreatedAt: time.Now()},
					})
					var prose strings.Builder
					for _, row := range strings.Split(stripANSI(m.View().Content), "\n") {
						if _, transcript, ok := strings.Cut(row, " ▏ "); ok {
							prose.WriteString(strings.TrimSpace(transcript))
						}
					}
					if !strings.Contains(prose.String(), word) {
						t.Fatalf("%s lost characters at a wrap boundary: want %q in %q", field, word, prose.String())
					}
				})
			}
		}
	}
}

func TestTUIQuestionViewStructured(t *testing.T) {
	m := questionViewModel(t, `{"question_id":"q1","questions":[{"header":"Cache","question":"Which cache TTL?","options":[{"label":"Short TTL","description":"Refresh often"},{"label":"Long TTL","description":""}]}]}`)
	out := stripANSI(m.View().Content)
	for _, marker := range []string{"✎ question", "Cache", "Which cache TTL?", "1. Short TTL", "2. Long TTL", "Refresh often"} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing structured question marker %q in View:\n%s", marker, out)
		}
	}
	if strings.Contains(out, `"questions":[`) {
		t.Errorf("View still renders raw question JSON:\n%s", out)
	}
}
