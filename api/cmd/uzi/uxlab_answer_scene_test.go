package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Each builder starts a fresh shipped model. Only fixture data changes before
// opening; key events create all drafts, review and delivery states.
type uxAnswerFrame struct {
	model   tuiModel
	client  *uzicli.FakeClient
	pending tea.Cmd
}

type uxAnswerScene struct {
	name    string
	build   func(bool, time.Time) uxAnswerFrame
	markers []string
}

func uxAnswerBase(dark bool, now time.Time, multi bool) uxAnswerFrame {
	m := detailInputScene(dark, now, "")
	if multi {
		question := strings.Replace(uxGitBoundaryQuestion, `"header": "Git boundary",`, `"header": "Git boundary", "multiSelect": true,`, 1)
		m = step(m, detailPageMsg{runID: detailRunID, gen: m.detail.gen, kind: pageCatchup,
			msgs: []apitypes.MessageDTO{{Seq: 7, Kind: "question", CreatedAt: now,
				Payload: json.RawMessage(`{"question_id":"git-verification-multi","questions":[` + uxHistoryCapQuestion + "," + question + "]}")}}})
	}
	return uxAnswerFrame{model: m, client: m.client.(*uzicli.FakeClient)}
}

// The generator has no testing.T. Keep the event seam explicit rather than
// calling the old key helper, which dispatches directly to handleKey.
func uxAnswerKey(f uxAnswerFrame, k tea.KeyPressMsg) uxAnswerFrame {
	n, cmd := f.model.Update(k)
	f.model, f.pending = n.(tuiModel), cmd
	return f
}

func uxAnswerKeys(f uxAnswerFrame, keys ...tea.KeyPressMsg) uxAnswerFrame {
	for _, k := range keys {
		f = uxAnswerKey(f, k)
	}
	return f
}

func uxAnswerRune(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func uxAnswerFirst(dark bool, now time.Time) uxAnswerFrame {
	return uxAnswerKeys(uxAnswerBase(dark, now, false), uxAnswerRune('i'), uxAnswerRune('1'))
}

func uxAnswerSecond(dark bool, now time.Time) uxAnswerFrame {
	return uxAnswerKeys(uxAnswerFirst(dark, now), tea.KeyPressMsg{Code: tea.KeyEnter}, uxAnswerRune('1'))
}

func uxAnswerMulti(dark bool, now time.Time) uxAnswerFrame {
	f := uxAnswerKeys(uxAnswerBase(dark, now, true), uxAnswerRune('i'), uxAnswerRune('1'),
		tea.KeyPressMsg{Code: tea.KeyEnter}, uxAnswerRune('2'), uxAnswerRune('1'), tea.KeyPressMsg{Code: tea.KeyTab})
	for _, r := range "retain custody" {
		f = uxAnswerKey(f, uxAnswerRune(r))
	}
	return f
}

func uxAnswerReview(dark bool, now time.Time) uxAnswerFrame {
	f := uxAnswerKey(uxAnswerSecond(dark, now), tea.KeyPressMsg{Code: tea.KeyTab})
	for _, r := range "use a cgroup memory limit" {
		f = uxAnswerKey(f, uxAnswerRune(r))
	}
	return uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
}

func uxAnswerPending(dark bool, now time.Time) uxAnswerFrame {
	return uxAnswerKey(uxAnswerReview(dark, now), tea.KeyPressMsg{Code: tea.KeyEnter})
}

func uxAnswerOutcome(dark bool, now time.Time, err error) uxAnswerFrame {
	f := uxAnswerReview(dark, now)
	f.client.SubmitRunInputErr = err
	f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
	// Execute the immutable submit command against FakeClient; follow-up reload
	// commands are deliberately left offline.
	f.model = step(f.model, f.pending())
	f.pending = nil
	return f
}

func uxAnswerResize(f uxAnswerFrame, width, height int) uxAnswerFrame {
	f.model = step(f.model, tea.WindowSizeMsg{Width: width, Height: height})
	return f
}

func uxAnswerScenes() []uxAnswerScene {
	return []uxAnswerScene{
		{"detail-answer-owner", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerBase(d, n, false) }, []string{"NEEDS INPUT", "answer with i"}},
		{"detail-answer-nonowner", func(d bool, n time.Time) uxAnswerFrame {
			f := uxAnswerBase(d, n, false)
			f.model = step(f.model, runInputsMsg{runID: detailRunID, gen: f.model.detail.gen, err: uzicli.Exitf(uzicli.ExitNotFound, "not owner")})
			return uxAnswerKey(f, uxAnswerRune('i'))
		}, []string{"NEEDS INPUT", "web, or Slack"}},
		{"detail-answer-question1", uxAnswerFirst, []string{"✎ ANSWER 1/2", "HISTORY CAP", "[x] Allow 1 GiB cap", "esc cancel", "CREW"}},
		{"detail-answer-question2", uxAnswerSecond, []string{"✎ ANSWER 2/2", "GIT BOUNDARY", "[x] Stronger resource boundary", "esc cancel"}},
		{"detail-answer-multiselect-detail", uxAnswerMulti, []string{"✎ ANSWER 2/2", "[x] Stronger resource boundary", "[x] Preserve existing boundary", "retain custody", "esc cancel"}},
		{"detail-answer-review", uxAnswerReview, []string{"✎ SEND ANSWER?", "History cap → Allow 1 GiB cap", "Git boundary → Stronger resource boundary", "enter send", "esc cancel"}},
		{"detail-answer-pending", uxAnswerPending, []string{"✎ SEND ANSWER?", "sending answer", "send/edit disabled"}},
		{"detail-answer-sent", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerOutcome(d, n, nil) }, []string{"answer sent"}},
		{"detail-answer-rejection", func(d bool, n time.Time) uxAnswerFrame {
			return uxAnswerOutcome(d, n, uzicli.Exitf(uzicli.ExitUsage, "invalid answer"))
		}, []string{"answer rejected", "invalid answer", "esc cancel"}},
		{"detail-answer-conflict", func(d bool, n time.Time) uxAnswerFrame {
			return uxAnswerOutcome(d, n, uzicli.Exitf(uzicli.ExitConflict, "stale question"))
		}, []string{"question no longer open", "send/edit disabled"}},
		{"detail-answer-uncertain", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerOutcome(d, n, errors.New("connection lost")) }, []string{"delivery not confirmed", "web or Slack", "local to this TUI session", "send/edit disabled"}},
		{"detail-answer-100x24", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerResize(uxAnswerFirst(d, n), 100, 24) }, []string{"✎ ANSWER 1/2", "[x]", "esc cancel"}},
		{"detail-answer-80x24", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerResize(uxAnswerSecond(d, n), 80, 24) }, []string{"✎ ANSWER 2/2", "[x]", "esc cancel"}},
		{"detail-answer-80x16", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerResize(uxAnswerMulti(d, n), 80, 16) }, []string{"✎ ANSWER 2/2", "retain custody", "esc cancel"}},
		{"detail-answer-review-80x16", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerResize(uxAnswerReview(d, n), 80, 16) }, []string{"✎ SEND ANSWER?", "History cap → Allow 1 GiB cap", "Git boundary → Stronger resource boundary", "esc cancel"}},
		{"detail-answer-resize-too-small", func(d bool, n time.Time) uxAnswerFrame { return uxAnswerResize(uxAnswerReview(d, n), 80, 8) }, []string{"resize required to answer", "esc cancel"}},
	}
}

func TestUXLabAnswerScenesViewAndAscii(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, scene := range uxAnswerScenes() {
		for _, dark := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dark=%v", scene.name, dark), func(t *testing.T) {
				f := scene.build(dark, now)
				raw := f.model.View().Content
				assertNoRawControls(t, scene.name, raw)
				f.model = step(f.model, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
				var buf bytes.Buffer
				writer := colorprofile.NewWriter(&buf, nil)
				writer.Profile = colorprofile.Ascii
				if _, err := writer.Write([]byte(f.model.View().Content)); err != nil {
					t.Fatal(err)
				}
				ascii := buf.String()
				if strings.Contains(ascii, "38;") || strings.Contains(ascii, "48;") {
					t.Fatalf("retained color SGR: %q", ascii)
				}
				assertNoRawControls(t, scene.name+" Ascii", ascii)
				for _, out := range []string{stripANSI(raw), stripANSI(ascii)} {
					for _, marker := range scene.markers {
						if !strings.Contains(out, marker) {
							t.Fatalf("missing %q:\n%s", marker, out)
						}
					}
					if rows := strings.Count(out, "\n") + 1; rows > f.model.height {
						t.Fatalf("rows=%d height=%d", rows, f.model.height)
					}
					for _, line := range strings.Split(out, "\n") {
						if visualWidth(line) > f.model.width {
							t.Fatalf("width overflow: %q", line)
						}
					}
				}
			})
		}
	}
}

func TestUXLabAnswerSceneWireAndGuards(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("multi=%v", multi), func(t *testing.T) {
			f := uxAnswerSecond(true, now)
			want := answerBody{QuestionID: "git-verification", Answers: []string{"Allow 1 GiB cap", "Stronger resource boundary"}}
			if multi {
				f = uxAnswerMulti(true, now)
				want = answerBody{QuestionID: "git-verification-multi", Answers: []string{"Allow 1 GiB cap", "Stronger resource boundary, Preserve existing boundary — retain custody"}}
			}
			f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
			if f.pending != nil || f.client.LastInputBody != "" {
				t.Fatal("review submitted early")
			}
			f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
			if f.pending == nil {
				t.Fatal("missing immutable submit command")
			}
			cmd := f.pending
			f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
			if f.pending != nil || f.client.LastInputBody != "" {
				t.Fatal("pending duplicated or executed request")
			}
			f.model = step(f.model, cmd())
			var got answerBody
			if err := json.Unmarshal([]byte(f.client.LastInputBody), &got); err != nil {
				t.Fatal(err)
			}
			if got.QuestionID != want.QuestionID || len(got.Answers) != 2 || got.Answers[0] != want.Answers[0] || got.Answers[1] != want.Answers[1] {
				t.Fatalf("wire=%+v want=%+v", got, want)
			}
			if f.client.LastInputRunID != detailRunID || f.client.LastInputKind != kindAnswer || f.client.LastInputSelection != nil || f.client.LastInputDiscardPendingOutcome || f.client.LastInputExpectedGateRevision != nil {
				t.Fatal("wrong request arguments")
			}
		})
	}
	for _, scene := range uxAnswerScenes() {
		t.Run(scene.name, func(t *testing.T) {
			f := scene.build(true, now)
			calls := len(f.client.RunVerbCalls)
			wantCalls := 0
			switch scene.name {
			case "detail-answer-sent", "detail-answer-rejection", "detail-answer-conflict", "detail-answer-uncertain":
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("submission calls=%d want=%d", calls, wantCalls)
			}
			switch scene.name {
			case "detail-answer-pending", "detail-answer-sent", "detail-answer-conflict", "detail-answer-uncertain", "detail-answer-nonowner", "detail-answer-resize-too-small":
				f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
				if f.pending != nil || len(f.client.RunVerbCalls) != calls {
					t.Fatal("guard allowed send")
				}
			}
			if scene.name == "detail-answer-nonowner" {
				if f.model.detail.answer.open || strings.Contains(stripANSI(f.model.View().Content), "answer with i") {
					t.Fatal("nonowner offered composer")
				}
			}
		})
	}
}

func TestUXLabAnswerSceneDraftResize(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := uxAnswerMulti(true, now)
	f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEscape})
	f = uxAnswerKey(f, uxAnswerRune('i'))
	answerAssertView(t, f.model, "retain custody", "[x] Stronger resource boundary", "[x] Preserve existing boundary")
	position, review := f.model.detail.answer.draft().position, f.model.detail.answer.draft().review
	f = uxAnswerResize(f, 30, 8)
	f = uxAnswerKey(f, tea.KeyPressMsg{Code: tea.KeyEnter})
	draft := f.model.detail.answer.draft()
	if f.pending != nil || draft.position != position || draft.review != review {
		t.Fatal("small terminal advanced")
	}
	f = uxAnswerResize(f, 80, 16)
	answerAssertView(t, f.model, "retain custody", "esc cancel")
}
