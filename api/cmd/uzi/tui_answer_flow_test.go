package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const answerFlowPayload = `{"question_id":" original id ","questions":[{"header":"HISTORY CAP","question":"How much history?","multiSelect":true,"options":[{"label":"All","description":"Keep everything"},{"label":"Recent","description":"Keep recent events"},{"label":"None"}]},{"header":"Boundary","question":"Where should Git stop?"}]}`

// These helpers only deliver real Update messages. Commands are explicitly
// executed by each test when needed, so delayed HTTP completions are deterministic.
func answerFlowModel(t *testing.T, payload string) (tuiModel, *uzicli.FakeClient) {
	t.Helper()
	f := &uzicli.FakeClient{}
	m := tuiTestModel(t, f, "answer-run")
	m = answerUpdate(t, m, detailRunMsg{runID: m.detail.runID, gen: m.detail.gen, run: apitypes.RunDTO{ID: m.detail.runID, Kind: "issue", Status: "awaiting_input"}})
	m = answerUpdate(t, m, detailPageMsg{runID: m.detail.runID, gen: m.detail.gen, kind: pageTail, msgs: []apitypes.MessageDTO{{Seq: 1, Kind: "question", Payload: json.RawMessage(payload)}}})
	m = answerUpdate(t, m, runInputsMsg{runID: m.detail.runID, gen: m.detail.gen})
	return m, f
}
func answerUpdate(t *testing.T, m tuiModel, msg tea.Msg) tuiModel {
	t.Helper()
	n, _ := m.Update(msg)
	return n.(tuiModel)
}
func answerPress(t *testing.T, m tuiModel, key string) (tuiModel, tea.Cmd) {
	t.Helper()
	k := tea.KeyPressMsg{}
	switch key {
	case "enter":
		k.Code = tea.KeyEnter
	case "esc":
		k.Code = tea.KeyEscape
	case "tab":
		k.Code = tea.KeyTab
	case "shift+tab":
		k.Code = tea.KeyTab
		k.Mod = tea.ModShift
	case "up":
		k.Code = tea.KeyUp
	case "down":
		k.Code = tea.KeyDown
	case "left":
		k.Code = tea.KeyLeft
	case "right":
		k.Code = tea.KeyRight
	case "pgup":
		k.Code = tea.KeyPgUp
	case "pgdown":
		k.Code = tea.KeyPgDown
	case "backspace":
		k.Code = tea.KeyBackspace
	case "ctrl+c":
		k.Code = 'c'
		k.Mod = tea.ModCtrl
	default:
		k.Text = key
		if r := []rune(key); len(r) > 0 {
			k.Code = r[0]
		}
	}
	n, cmd := m.Update(k)
	return n.(tuiModel), cmd
}
func answerKeys(t *testing.T, m tuiModel, keys ...string) tuiModel {
	t.Helper()
	for _, k := range keys {
		m, _ = answerPress(t, m, k)
	}
	return m
}
func answerFeed(t *testing.T, m tuiModel, seq int32, kind, payload string) tuiModel {
	t.Helper()
	return answerUpdate(t, m, streamEventsMsg{runID: m.detail.runID, gen: m.detail.gen, events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeMessage, Seq: seq, Kind: kind, Agent: ptr("reviewer"), Payload: json.RawMessage(payload)}}})
}
func answerStateEvent(t *testing.T, m tuiModel, status string) tuiModel {
	t.Helper()
	return answerUpdate(t, m, streamEventsMsg{runID: m.detail.runID, gen: m.detail.gen, events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeState, Status: status}}})
}
func answerReview(t *testing.T, m tuiModel) tuiModel {
	t.Helper()
	return answerKeys(t, m, "i", "2", "1", " ", "1", "enter", "é", "界", "backspace", "tab", " ", "enter")
}
func answerAssertView(t *testing.T, m tuiModel, markers ...string) {
	t.Helper()
	view := stripANSI(m.View().Content)
	for _, s := range markers {
		if !strings.Contains(view, s) {
			t.Fatalf("missing %q:\n%s", s, view)
		}
	}
}

func TestAnswerComposerClosedDefersToModals(t *testing.T) {
	m, f := answerFlowModel(t, answerFlowPayload)
	m = answerKeys(t, m, "f", "h", "i")
	if m.detail.steer.input != "hi" || m.detail.answer.open {
		t.Fatal("answer shortcut intercepted follow-up input")
	}
	m, cmd := answerPress(t, m, "enter")
	if cmd == nil {
		t.Fatal("missing follow-up command")
	}
	m = answerUpdate(t, m, cmd())
	if f.LastInputBody != "hi" {
		t.Fatalf("follow-up body %q", f.LastInputBody)
	}
	m = answerKeys(t, m, "x", "i")
	if m.detail.steer.mode != steerIdle || m.detail.steer.pending != "" || m.detail.answer.open {
		t.Fatal("i failed to cancel destructive confirmation")
	}
	m.detail.review.open = true
	m = answerKeys(t, m, "i")
	if !m.detail.review.open || m.detail.answer.open {
		t.Fatal("i escaped review")
	}
}

func TestAnswerComposerUncertaintyResolvedByClosure(t *testing.T) {
	for _, closure := range []string{"answer", "replacement"} {
		t.Run(closure, func(t *testing.T) {
			m, f := answerFlowModel(t, answerFlowPayload)
			f.SubmitRunInputErr = errors.New("connection lost")
			m = answerReview(t, m)
			m, cmd := answerPress(t, m, "enter")
			m = answerUpdate(t, m, cmd())
			answerAssertView(t, m, "delivery not confirmed")
			next := `{"question_id":"next","questions":[{"question":"Next?"}]}`
			if closure == "answer" {
				m = answerFeed(t, m, 2, "answer", `{"answers":["echo","echo"]}`)
			} else {
				m = answerFeed(t, m, 2, "question", next)
			}
			if m.detail.answer.answerNotice != "" {
				t.Fatal("resolved uncertainty notice persisted")
			}
			old := m.detail.answer.draft()
			if !old.closed || !old.uncertain {
				t.Fatal("closure removed old delivery guard")
			}
			m = answerKeys(t, m, "esc", "i", "enter")
			if !old.closed || len(f.RunVerbCalls) != 1 {
				t.Fatal("old identity resubmitted")
			}
			if closure == "answer" {
				m = answerFeed(t, m, 3, "question", next)
			}
			if m.detail.answer.open {
				m = answerKeys(t, m, "esc")
			}
			m = answerKeys(t, m, "i", "n", "enter")
			if m.detail.answer.draft().snapshot.QuestionID != "next" || !m.detail.answer.draft().review {
				t.Fatal("replacement was not independent")
			}
			m.detail.answer.answerNotice = "replacement-specific notice"
			m = answerFeed(t, m, 4, "question", next)
			if m.detail.answer.answerNotice != "replacement-specific notice" {
				t.Fatal("cleared replacement notice")
			}
		})
	}
}

func TestAnswerComposerEditPagingReachability(t *testing.T) {
	for _, options := range []bool{false, true} {
		t.Run(fmt.Sprint(options), func(t *testing.T) {
			q := map[string]any{"question": "QUESTIONBEGIN " + strings.Repeat("explain ", 80) + " REQUIREDTAIL"}
			if options {
				q["options"] = []any{map[string]any{"label": "Current", "description": "DESCRIPTIONBEGIN " + strings.Repeat("describe ", 80) + " DESCRIPTIONTAIL"}}
			}
			wire, _ := json.Marshal(map[string]any{"question_id": "paging", "questions": []any{q}})
			m, _ := answerFlowModel(t, string(wire))
			m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 80, Height: 16})
			m = answerKeys(t, m, "i")
			var seen strings.Builder
			// Manual mode pins the option, adding one row to the paging limit.
			_, limit, _ := m.answerLayout(m.answerRoom())
			for _, key := range []string{"pgup", "pgdown", "pgup"} {
				// One-row paging must reach the full prose even with a compact viewport.
				for range limit + 2 {
					m = answerKeys(t, m, key)
					view := stripANSI(m.View().Content)
					seen.WriteString(view)
					for _, marker := range []string{"detail (optional)", "esc cancel", "pgup"} {
						if !strings.Contains(view, marker) {
							t.Fatalf("paging hid %s:\n%s", marker, view)
						}
					}
					if options && !strings.Contains(view, "› 1 [ ] Current") {
						t.Fatalf("paging hid current option:\n%s", view)
					}
					lines := strings.Split(view, "\n")
					if len(lines) > 16 {
						t.Fatal("paging overflowed height")
					}
					for _, line := range lines {
						if visualWidth(line) > 80 {
							t.Fatal("paging overflowed width")
						}
					}
				}
			}
			for _, marker := range []string{"QUESTIONBEGIN", "REQUIREDTAIL"} {
				if !strings.Contains(seen.String(), marker) {
					t.Fatalf("unreachable %s", marker)
				}
			}
			if options {
				for _, marker := range []string{"DESCRIPTIONBEGIN", "DESCRIPTIONTAIL"} {
					if !strings.Contains(seen.String(), marker) {
						t.Fatalf("unreachable %s", marker)
					}
				}
				m = answerKeys(t, m, "1")
				answerAssertView(t, m, "› 1 [x] Current")
				if m.detail.answer.draft().manualScroll || m.detail.answer.draft().editScroll != 0 {
					t.Fatal("selection did not restore automatic focus")
				}
			}
			answerAssertView(t, answerKeys(t, m, "pgup"), "detail (optional)")
		})
	}
}

// Read the actual View indicator only in tests; production uses layout metadata.
func answerViewOffset(t *testing.T, m tuiModel) int {
	t.Helper()
	view := stripANSI(m.View().Content)
	for _, line := range strings.Split(view, "\n") {
		var offset, remaining int
		if n, err := fmt.Sscanf(line, "┃ … ↑ %d lines · ↓ %d lines", &offset, &remaining); err == nil && n == 2 {
			return offset
		}
	}
	t.Fatalf("missing paging indicator:\n%s", view)
	return 0
}

func TestAnswerComposerFirstPagingPreservesVisibleOffset(t *testing.T) {
	wire, _ := json.Marshal(map[string]any{
		"question_id": "first-page",
		"questions": []any{map[string]any{
			"question": "QUESTIONBEGIN " + strings.Repeat("explain ", 200) + " QUESTIONTAIL",
			"options": []any{map[string]any{
				"label":       "Current",
				"description": "DESCRIPTIONBEGIN " + strings.Repeat("describe ", 80) + " DESCRIPTIONTAIL",
			}},
		}},
	})
	for _, dark := range []bool{false, true} {
		for _, key := range []string{"pgdown", "pgup"} {
			t.Run(fmt.Sprintf("dark=%v/%s", dark, key), func(t *testing.T) {
				m, _ := answerFlowModel(t, string(wire))
				m.dark = dark
				var err error
				m.renderer, err = newTUIRenderer(80, dark)
				if err != nil {
					t.Fatal(err)
				}
				m = answerKeys(t, m, "i", "1")
				for _, size := range [][2]int{{80, 16}, {40, 10}} {
					// Picker navigation restores auto-focus before resizing the viewport.
					m = answerKeys(t, m, "down")
					m = answerUpdate(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					if m.answerTooSmall() || m.detail.answer.draft().manualScroll {
						t.Fatal("expected accepted size with automatic focus")
					}
					before := answerViewOffset(t, m)
					if before < 2 {
						t.Fatalf("fixture did not auto-scroll: offset=%d", before)
					}
					m = answerKeys(t, m, key)
					want := before + 1
					if key == "pgup" {
						want = before - 1
					}
					if after := answerViewOffset(t, m); after != want {
						t.Fatalf("%s at %v: visible offset %d → %d, want %d", key, size, before, after, want)
					}
					if !m.detail.answer.draft().manualScroll || m.detail.answer.draft().editScroll != want {
						t.Fatal("manual paging state disagrees with visible offset")
					}
					answerAssertView(t, m, "› 1 [x] Current", "detail (optional)", "esc cancel")
					view := stripANSI(m.View().Content)
					if len(strings.Split(view, "\n")) > size[1] {
						t.Fatalf("paging exceeded height at %v", size)
					}
					for _, line := range strings.Split(view, "\n") {
						if visualWidth(line) > size[0] {
							t.Fatalf("paging exceeded width at %v: %s", size, line)
						}
					}
				}
			})
		}
	}
}

func TestAnswerComposerVisualInputSuffix(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("界", 100) + "Z",
		strings.Repeat("😀a界", 100) + "😀Z",
		strings.Repeat("a", 500) + "TAILZ",
	} {
		m, _ := answerFlowModel(t, `{"question_id":"suffix","questions":[{"question":"Explain"}]}`)
		m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 80, Height: 16})
		m = answerKeys(t, m, "i", text)
		for _, hostile := range []bool{false, true} {
			if hostile {
				m.detail.answer.draft().answerText[0] += "\x1b[31m\x1b]52;c;inject\a\r\x00ENDZ"
			}
			raw := m.View().Content
			assertNoRawControls(t, "input suffix", raw)
			view := stripANSI(raw)
			final := "Z"
			if strings.HasSuffix(text, "😀Z") {
				final = "😀Z"
			}
			if strings.HasSuffix(text, "TAILZ") {
				final = "TAILZ"
			}
			if hostile {
				final = "ENDZ"
			}
			answerAssertView(t, m, final, "detail (optional)", "esc cancel")
			found := false
			for _, line := range strings.Split(view, "\n") {
				if visualWidth(line) > 80 {
					t.Fatal("input overflowed width")
				}
				if strings.Contains(line, "detail (optional)") {
					found = strings.HasSuffix(line, final)
				}
			}
			if !found {
				t.Fatalf("final input glyph hidden:\n%s", view)
			}
			if len(strings.Split(view, "\n")) > 16 {
				t.Fatal("input overflowed height")
			}
		}
	}
}

func TestAnswerComposerOwnerAndMalformed(t *testing.T) {
	for _, access := range []steerAccess{steerUnknown, steerNotOwner, steerAllowed} {
		m, _ := answerFlowModel(t, answerFlowPayload)
		m.detail.steer.access = access
		m, _ = answerPress(t, m, "i")
		if m.detail.answer.open != (access == steerAllowed) {
			t.Fatalf("access %v opened=%v", access, m.detail.answer.open)
		}
		if access == steerAllowed {
			answerAssertView(t, m, "✎ ANSWER 1/2", "HISTORY CAP", "detail (optional)", "esc cancel")
		} else {
			answerAssertView(t, m, "the web, or Slack")
		}
	}
	for _, p := range []string{`{"questions":[{"question":"Choose"}]}`, `{"question_id":"q","questions":[null,{"question":"Choose"}]}`, `{"question_id":"q","questions":[]}`, `{"question_id":42,"questions":[{"question":"Choose"}]}`} {
		m, _ := answerFlowModel(t, p)
		m, _ = answerPress(t, m, "i")
		if m.detail.answer.open {
			t.Fatal("unusable payload opened")
		}
		answerAssertView(t, m, "cannot answer")
	}
	m, _ := answerFlowModel(t, answerFlowPayload)
	answerAssertView(t, m, "the agent asked 2 questions; answer with i")
	m.detail.review.open = true
	m, _ = answerPress(t, m, "i")
	if m.detail.answer.open {
		t.Fatal("opened over review")
	}
	m.detail.review.open = false
	m.detail.steer.mode = steerTyping
	m, _ = answerPress(t, m, "i")
	if m.detail.answer.open {
		t.Fatal("opened over steer")
	}
}

func TestAnswerComposerKeysReviewAndWire(t *testing.T) {
	m, f := answerFlowModel(t, answerFlowPayload)
	m = answerKeys(t, m, "i", "2", "1", " ", "1") // reverse order, deselect/reselect All
	m = answerKeys(t, m, "tab")
	for _, r := range "q?iz123 space fvxymcWrg[]hjkl" {
		m = answerKeys(t, m, string(r))
	}
	m = answerKeys(t, m, "界", "backspace", "enter")
	if m.detail.answer.draft().position != 1 {
		t.Fatal("did not advance")
	}
	m = answerKeys(t, m, "é", "界", "backspace", " ", "enter")
	answerAssertView(t, m, "✎ SEND ANSWER?", "HISTORY CAP → All, Recent — q?iz123 space", "Boundary → é")
	if m.showHelp || m.detail.questionCollapsed || m.view != viewDetail {
		t.Fatal("shortcut escaped composer")
	}
	m = answerKeys(t, m, "shift+tab", "backspace", "z", "enter")
	m, cmd := answerPress(t, m, "enter")
	if cmd == nil {
		t.Fatal("no send command")
	}
	m = answerUpdate(t, m, cmd())
	var body answerBody
	if err := json.Unmarshal([]byte(f.LastInputBody), &body); err != nil {
		t.Fatal(err)
	}
	if body.QuestionID != " original id " || len(body.Answers) != 2 || body.Answers[1] != "éz" || !strings.HasPrefix(body.Answers[0], "All, Recent — q?iz123") {
		t.Fatalf("wrong wire: %+v", body)
	}
	if f.LastInputKind != kindAnswer || f.LastInputSelection != nil || f.LastInputDiscardPendingOutcome || f.LastInputExpectedGateRevision != nil {
		t.Fatal("wrong request arguments")
	}
	answerAssertView(t, m, "answer sent")
}

func TestAnswerComposerProseFocusNavigationAndCancel(t *testing.T) {
	m, _ := answerFlowModel(t, answerFlowPayload)
	m = answerKeys(t, m, "i")
	for _, r := range "use option 2 please" {
		m = answerKeys(t, m, string(r))
	}
	if d := m.detail.answer.draft(); !d.detailFocus || d.answerText[0] != "use option 2 please" {
		t.Fatalf("prose turned into selections: %+v", d)
	}
	m = answerKeys(t, m, "tab", "down", " ", "up", " ", "enter", "enter")
	if m.detail.answer.draft().review {
		t.Fatal("empty second question advanced")
	}
	answerAssertView(t, m, "choose an option or enter an answer")
	m = answerKeys(t, m, "x", "enter", "shift+tab", "shift+tab")
	if m.detail.answer.draft().position != 0 {
		t.Fatal("back did not return to first question")
	}
	m = answerKeys(t, m, "esc", "i")
	if m.detail.answer.draft().answerText[0] != "use option 2 please" {
		t.Fatal("cancel lost draft")
	}
	m = answerKeys(t, m, "ctrl+c")
	if !m.quitting {
		t.Fatal("ctrl+c no longer confirms")
	}
}

func TestAnswerComposerPendingAndSuccessEcho(t *testing.T) {
	for _, echoFirst := range []bool{false, true} {
		m, f := answerFlowModel(t, answerFlowPayload)
		m = answerReview(t, m)
		m, cmd := answerPress(t, m, "enter")
		if cmd == nil {
			t.Fatal("no send")
		}
		result := cmd() // accepted, but delivery held
		m, second := answerPress(t, m, "enter")
		if second != nil {
			t.Fatal("double send")
		}
		m = answerKeys(t, m, "esc", "i")
		if m.detail.answer.open {
			t.Fatal("pending reopened")
		}
		if echoFirst {
			m = answerFeed(t, m, 2, "answer", `{"answers":["echo","echo"]}`)
		}
		m = answerUpdate(t, m, result)
		if m.detail.answer.draft().pending != 0 {
			t.Fatal("pending did not retire")
		}
		if !echoFirst {
			m = answerStateEvent(t, m, "running")
			answerAssertView(t, m, "answer sent")
			m = answerStateEvent(t, m, "awaiting_input")
			m = answerKeys(t, m, "i", "enter")
			if m.detail.answer.open {
				t.Fatal("sent draft reopened")
			}
			m = answerFeed(t, m, 2, "answer", `{"answers":["echo","echo"]}`)
		}
		if strings.Contains(m.View().Content, "answer sent") || strings.Contains(m.View().Content, "sending answer") {
			t.Fatal("echo did not retire success")
		}
		if len(f.RunVerbCalls) != 1 {
			t.Fatalf("duplicate accepted request %v", f.RunVerbCalls)
		}
	}
}

func TestAnswerComposerRejectedUncertainAndConflict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		copy  string
		retry bool
	}{
		{"definite", uzicli.Exitf(uzicli.ExitUsage, "invalid answer"), "answer rejected", true},
		{"auth", uzicli.Exitf(uzicli.ExitAuth, "unauthorized"), "answer rejected", true},
		{"not-found", uzicli.Exitf(uzicli.ExitNotFound, "missing"), "answer rejected", true},
		{"malformed-success", uzicli.Exitf(uzicli.ExitGeneric, "decode HTTP 200 response"), "delivery not confirmed", false},
		{"typed-timeout", uzicli.Exitf(uzicli.ExitTimeout, "request timed out"), "delivery not confirmed", false},
		{"transport", errors.New("connection lost"), "delivery not confirmed", false},
		{"server", uzicli.Exitf(uzicli.ExitUnreachable, "server unavailable"), "delivery not confirmed", false},
		{"conflict", uzicli.Exitf(uzicli.ExitConflict, "stale"), "question no longer open", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := answerFlowModel(t, answerFlowPayload)
			f.SubmitRunInputErr = tc.err
			m = answerReview(t, m)
			m, cmd := answerPress(t, m, "enter")
			if cmd == nil {
				t.Fatal("no send")
			}
			n, reload := m.Update(cmd())
			m = n.(tuiModel)
			answerAssertView(t, m, tc.copy)
			if tc.name == "conflict" && (reload == nil || m.detail.metaWaitID == 0 || m.detail.catchupWaitID == 0) {
				t.Fatal("missing guarded reload")
			}
			m = answerStateEvent(t, m, "running")
			m = answerKeys(t, m, "esc")
			answerAssertView(t, m, tc.copy)
			m = answerStateEvent(t, m, "awaiting_input")
			m.detail.polling = true
			m.detail.metaWaitID = 99
			m = answerUpdate(t, m, detailMetaMsg{runID: m.detail.runID, gen: m.detail.gen, reqID: 99, run: m.detail.run})
			m = answerKeys(t, m, "i")
			f.SubmitRunInputErr = nil
			m, cmd = answerPress(t, m, "enter")
			if (cmd != nil) != tc.retry {
				t.Fatalf("retry=%v expected=%v", cmd != nil, tc.retry)
			}
			if cmd != nil {
				m = answerUpdate(t, m, cmd())
				answerAssertView(t, m, "answer sent")
			}
			calls := 1
			if tc.retry {
				calls++
			}
			if len(f.RunVerbCalls) != calls {
				t.Fatalf("submission calls=%d, want %d", len(f.RunVerbCalls), calls)
			}
			if tc.name == "transport" {
				answerAssertView(t, m, "local to this TUI session")
			}
		})
	}
}

func TestAnswerComposerReplacementAndDelayedCompletion(t *testing.T) {
	for _, err := range []error{nil, errors.New("lost"), uzicli.Exitf(uzicli.ExitUsage, "rejected"), uzicli.Exitf(uzicli.ExitConflict, "closed")} {
		for _, delayCommand := range []bool{false, true} {
			m, f := answerFlowModel(t, answerFlowPayload)
			m = answerReview(t, m)
			m, cmd := answerPress(t, m, "enter")
			if cmd == nil {
				t.Fatal("no command")
			}
			f.SubmitRunInputErr = err
			var result tea.Msg
			if !delayCommand {
				result = cmd()
			}
			m = answerFeed(t, m, 2, "question", `{"question_id":"new-id","questions":[{"question":"New one"}]}`)
			answerAssertView(t, m, "question no longer open")
			m = answerKeys(t, m, "esc", "i", "n", "enter")
			if !m.detail.answer.draft().review || m.detail.answer.draft().answerText[0] != "n" {
				t.Fatal("replacement copied old draft")
			}
			if delayCommand {
				result = cmd()
			}
			var old answerBody
			_ = json.Unmarshal([]byte(f.LastInputBody), &old)
			if old.QuestionID != " original id " || len(old.Answers) != 2 {
				t.Fatalf("command changed wire %+v", old)
			}
			m = answerUpdate(t, m, result)
			if m.detail.answer.drafts[0].pending != 0 {
				t.Fatal("old pending stuck")
			}
			if m.detail.answer.answerNotice != "" {
				t.Fatal("old notice applied to new identity")
			}
			f.SubmitRunInputErr = nil
			m, cmd = answerPress(t, m, "enter")
			if cmd == nil {
				t.Fatal("new identity stuck")
			}
			m = answerUpdate(t, m, cmd())
			var body answerBody
			_ = json.Unmarshal([]byte(f.LastInputBody), &body)
			if body.QuestionID != "new-id" || len(body.Answers) != 1 || body.Answers[0] != "n" {
				t.Fatalf("bad replacement wire %+v", body)
			}
		}
	}
	// Replacement before generating a command blocks the old review.
	m, _ := answerFlowModel(t, answerFlowPayload)
	m = answerReview(t, m)
	m = answerFeed(t, m, 2, "question", `{"question_id":"new-id","questions":[{"question":"New one"}]}`)
	_, cmd := answerPress(t, m, "enter")
	if cmd != nil {
		t.Fatal("sent replaced draft")
	}
}

func TestAnswerComposerSameIDReparkAndDrift(t *testing.T) {
	m, _ := answerFlowModel(t, answerFlowPayload)
	m = answerKeys(t, m, "i", "2", "tab", "x")
	m = answerStateEvent(t, m, "running")
	answerAssertView(t, m, "draft suspended")
	m = answerKeys(t, m, "z")
	m = answerStateEvent(t, m, "awaiting_input")
	m = answerFeed(t, m, 2, "question", answerFlowPayload)
	if d := m.detail.answer.draft(); d.closed || d.drift || d.answerText[0] != "x" || !d.selected[0][1] {
		t.Fatal("same-id repark lost draft")
	}
	for _, replacement := range []string{
		strings.Replace(answerFlowPayload, "How much history?", "Different?", 1),
		strings.Replace(answerFlowPayload, "HISTORY CAP", "Other header", 1),
		strings.Replace(answerFlowPayload, "Keep everything", "Different description", 1),
		strings.Replace(answerFlowPayload, `"multiSelect":true`, `"multiSelect":false`, 1),
		strings.Replace(answerFlowPayload, `{"label":"None"}`, `{"label":"Fourth"}`, 1),
		strings.Replace(answerFlowPayload, `,{"label":"None"}`, "", 1),
		strings.Replace(answerFlowPayload, `{"label":"All","description":"Keep everything"},{"label":"Recent","description":"Keep recent events"}`, `{"label":"Recent","description":"Keep recent events"},{"label":"All","description":"Keep everything"}`, 1),
		`{"question_id":" original id ","questions":[{"question":"Just one"}]}`,
	} {
		d, _ := answerFlowModel(t, answerFlowPayload)
		d = answerKeys(t, d, "i", "1")
		d = answerFeed(t, d, 2, "question", replacement)
		answerAssertView(t, d, "question changed", "blocked")
		d = answerKeys(t, d, "esc", "i", "enter", "enter")
		if !d.detail.answer.draft().drift {
			t.Fatal("reopen reset drift")
		}
	}
}

func TestAnswerComposerReconcileAllAcceptedPathsAndStaleSession(t *testing.T) {
	for _, lane := range []string{"tail", "backfill", "catchup", "stream"} {
		t.Run(lane, func(t *testing.T) {
			m, _ := answerFlowModel(t, answerFlowPayload)
			m = answerKeys(t, m, "i", "1")
			echo := apitypes.MessageDTO{Seq: 3, Kind: "answer", Agent: ptr("another-lane"), Payload: json.RawMessage(`{"answers":["external"]}`)}
			switch lane {
			case "tail":
				m = answerUpdate(t, m, detailPageMsg{runID: m.detail.runID, gen: m.detail.gen, kind: pageTail, msgs: []apitypes.MessageDTO{echo}})
			case "backfill":
				m.detail.lowSeq = 4
				m = answerUpdate(t, m, detailPageMsg{runID: m.detail.runID, gen: m.detail.gen, kind: pageBackfill, msgs: []apitypes.MessageDTO{echo}})
			case "catchup":
				m.detail.catchupWaitID = 7
				m = answerUpdate(t, m, detailPageMsg{runID: m.detail.runID, gen: m.detail.gen, kind: pageCatchup, reqID: 7, msgs: []apitypes.MessageDTO{echo}})
			case "stream":
				m = answerFeed(t, m, 3, "answer", string(echo.Payload))
			}
			if !m.detail.answer.draft().closed {
				t.Fatal("accepted path missed reconciliation")
			}
			answerAssertView(t, m, "question no longer open")
		})
	}
	m, _ := answerFlowModel(t, answerFlowPayload)
	m = answerReview(t, m)
	m, cmd := answerPress(t, m, "enter")
	result := cmd().(answerResultMsg)
	for _, stale := range []answerResultMsg{
		{runID: "other", gen: result.gen, questionID: result.questionID, requestID: result.requestID},
		{runID: result.runID, gen: result.gen + 1, questionID: result.questionID, requestID: result.requestID},
		{runID: result.runID, gen: result.gen, questionID: "other", requestID: result.requestID},
		{runID: result.runID, gen: result.gen, questionID: result.questionID, requestID: result.requestID + 1},
	} {
		m = answerUpdate(t, m, stale)
		if m.detail.answer.draft().pending != result.requestID {
			t.Fatal("stale result retired request")
		}
	}
	m = answerUpdate(t, m, result)
	answerAssertView(t, m, "answer sent")
	// Ownership and run/meta results suspend without dropping the snapshot.
	d, _ := answerFlowModel(t, answerFlowPayload)
	d = answerKeys(t, d, "i", "1")
	d = answerUpdate(t, d, runInputsMsg{runID: d.detail.runID, gen: d.detail.gen, err: uzicli.Exitf(uzicli.ExitNotFound, "not owner")})
	answerAssertView(t, d, "ownership is not confirmed")
	d = answerUpdate(t, d, runInputsMsg{runID: d.detail.runID, gen: d.detail.gen})
	d.detail.polling = true
	d.detail.metaWaitID = 4
	run := d.detail.run
	run.Status = "running"
	d = answerUpdate(t, d, detailMetaMsg{runID: d.detail.runID, gen: d.detail.gen, reqID: 4, run: run})
	answerAssertView(t, d, "draft suspended")
	d = answerUpdate(t, d, detailRunMsg{runID: d.detail.runID, gen: d.detail.gen, run: run})
	if d.detail.answer.draft().closed {
		t.Fatal("DTO status interpreted as closure")
	}
}

func TestAnswerComposerReviewAllWrappedAnswersReachable(t *testing.T) {
	var questions []map[string]any
	var answers []string
	var words []string
	for qi := 0; qi < 4; qi++ {
		questions = append(questions, map[string]any{"header": fmt.Sprintf("Row%d", qi), "question": "Explain"})
		var row []string
		for wi := 0; wi < 30; wi++ {
			word := fmt.Sprintf("answer%dword%02d", qi, wi)
			row = append(row, word)
			words = append(words, word)
		}
		answers = append(answers, strings.Join(row, " "))
	}
	wire, _ := json.Marshal(map[string]any{"question_id": "wrapped", "questions": questions})
	m, _ := answerFlowModel(t, string(wire))
	m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	m = answerKeys(t, m, "i")
	for _, answer := range answers {
		m = answerKeys(t, m, answer, "enter")
	}
	answerAssertView(t, m, "SEND ANSWER?", "esc cancel", "↓")
	var seen strings.Builder
	// One step per word is an upper bound on wrapped rows for these fixtures.
	for range words {
		seen.WriteString(stripANSI(m.View().Content))
		m = answerKeys(t, m, "down")
	}
	for _, word := range words {
		if !strings.Contains(seen.String(), word) {
			t.Fatalf("wrapped answer unreachable: %s", word)
		}
	}
	last := stripANSI(m.View().Content)
	for range words {
		m = answerKeys(t, m, "pgdown")
	}
	if stripANSI(m.View().Content) != last {
		t.Fatal("scroll past the end changed the last page")
	}
	m = answerKeys(t, m, "up")
	if stripANSI(m.View().Content) == last {
		t.Fatal("up after repeated pgdown remained stuck")
	}
	for range words {
		m = answerKeys(t, m, "pgup")
	}
	answerAssertView(t, m, "answer0word00", "↑ 0 lines")
}

func TestAnswerComposerOverflowKeepsCurrentOptionAndOneRowControls(t *testing.T) {
	wire, _ := json.Marshal(map[string]any{
		"question_id": "overflow",
		"questions": []any{map[string]any{
			"header": "**literal**", "question": strings.Repeat("long question ", 100),
			"options": []any{
				map[string]any{"label": "First", "description": strings.Repeat("description ", 50)},
				map[string]any{"label": "Current"},
			},
		}},
	})
	for _, width := range []int{40, 80, 100} {
		m, _ := answerFlowModel(t, string(wire))
		m = answerUpdate(t, m, tea.WindowSizeMsg{Width: width, Height: 10})
		m = answerKeys(t, m, "i", "2", "tab", "draft")
		answerAssertView(t, m, "› 2 [x] Current", "detail (optional)", "↑", "esc cancel", "**LITERAL**")
		lines := strings.Split(stripANSI(m.View().Content), "\n")
		footer := lines[len(lines)-1]
		for _, key := range []string{"tab", "esc cancel", "1-9", "↑↓"} {
			// The narrow alternative puts 1-9 directly after the arrows.
			if !strings.Contains(footer, key) {
				t.Fatalf("width %d hid control %s in footer: %s", width, key, footer)
			}
		}
		if len(lines) > 10 {
			t.Fatalf("width %d overflowed height: %d", width, len(lines))
		}
	}
}

func TestAnswerComposerLongHostileOutcomeFitsCompactLayout(t *testing.T) {
	hostile := "safe\x1b[31m\x1b]52;c;inject\a\r\x00"
	for _, code := range []int{uzicli.ExitUsage, uzicli.ExitGeneric} {
		m, f := answerFlowModel(t, answerFlowPayload)
		f.SubmitRunInputErr = uzicli.Exitf(code, "%s", strings.Repeat("long rejection "+hostile, 100))
		m = answerReview(t, m)
		m, cmd := answerPress(t, m, "enter")
		if cmd == nil {
			t.Fatal("no send command")
		}
		m = answerUpdate(t, m, cmd())
		for _, size := range [][2]int{{40, 10}, {80, 16}, {100, 24}} {
			m = answerUpdate(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			raw := m.View().Content
			assertNoRawControls(t, "hostile answer outcome", raw)
			var buf bytes.Buffer
			writer := colorprofile.NewWriter(&buf, nil)
			writer.Profile = colorprofile.Ascii
			if _, err := writer.Write([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "38;") || strings.Contains(buf.String(), "48;") {
				t.Fatal("Ascii writer retained colors")
			}
			out := stripANSI(buf.String())
			if len(strings.Split(out, "\n")) > size[1] {
				t.Fatalf("outcome exceeded rows at %v:\n%s", size, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if visualWidth(line) > size[0] {
					t.Fatalf("outcome exceeded width at %v: %s", size, line)
				}
			}
			if code == uzicli.ExitUsage {
				answerAssertView(t, m, "answer rejected", "web/Slack", "esc cancel")
			} else {
				answerAssertView(t, m, "delivery not confirmed", "esc close")
				if !strings.Contains(out, "local resend guard") && !strings.Contains(out, "local to this TUI session") {
					t.Fatalf("lost session guard: %s", out)
				}
			}
		}
		if len(f.RunVerbCalls) != 1 {
			t.Fatal("outcome caused extra submission")
		}
	}
}

func TestAnswerComposerLayoutResizeAsciiAndHostile(t *testing.T) {
	hostile := "safe\x1b[31m\x1b]52;c;inject\a\r\x00"
	p := map[string]any{"question_id": "q", "questions": []any{map[string]any{"header": "HISTORY CAP " + hostile, "question": strings.Repeat("wrapped question ", 30) + hostile, "options": []any{map[string]any{"label": "Choice " + hostile, "description": strings.Repeat("description ", 20) + hostile}}}}}
	wire, _ := json.Marshal(p)
	for _, size := range [][2]int{{100, 24}, {80, 24}, {80, 16}} {
		for _, review := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d-review=%v", size[0], size[1], review), func(t *testing.T) {
				m, _ := answerFlowModel(t, string(wire))
				m = answerKeys(t, m, "i", "1", "tab")
				// This is a printable event followed by hostile draft bytes supplied
				// as a paste/state fixture, exercising the actual draw boundary.
				m = answerKeys(t, m, "draft")
				m.detail.answer.draft().answerText[0] += hostile
				if review {
					m = answerKeys(t, m, "enter", "pgdown", "up", "pgup")
				}
				m.detail.follow = false
				m.detail.scroll = 7
				m.detail.polling = true
				m.detail.streamErr = errors.New("degraded")
				m = answerUpdate(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				answerAssertView(t, m, "esc cancel")
				m = answerUpdate(t, m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
				raw := m.View().Content
				assertNoRawControls(t, "answer composer", raw)
				var buf bytes.Buffer
				writer := colorprofile.NewWriter(&buf, nil)
				writer.Profile = colorprofile.Ascii
				if _, err := writer.Write([]byte(raw)); err != nil {
					t.Fatal(err)
				}
				out := buf.String()
				if strings.Contains(out, "38;") || strings.Contains(out, "48;") {
					t.Fatalf("Ascii writer retained color SGR: %q", out)
				}
				assertNoRawControls(t, "Ascii answer composer", out)
				out = stripANSI(out)
				if !strings.Contains(out, "┃ ✎") || !strings.Contains(out, "esc cancel") {
					t.Fatalf("lost visible controls: %s", out)
				}
				for _, line := range strings.Split(out, "\n") {
					if visualWidth(line) > size[0] {
						t.Fatalf("overflow %q", line)
					}
				}
				if rows := strings.Count(out, "\n") + 1; rows > size[1] {
					t.Fatalf("clipped hints: rows=%d:\n%s", rows, out)
				}
				if m.detail.scroll != 7 || m.detail.follow {
					t.Fatal("composition jumped transcript")
				}
				if !review {
					answerAssertView(t, m, "detail (optional)")
				}
				m = answerUpdate(t, m, tea.WindowSizeMsg{Width: 30, Height: 8})
				answerAssertView(t, m, "resize required", "esc cancel")
				_, cmd := answerPress(t, m, "enter")
				if cmd != nil {
					t.Fatal("small terminal sent")
				}
				m = answerUpdate(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				if !m.detail.answer.open {
					t.Fatal("resize lost composer")
				}
				m = answerKeys(t, m, "esc")
				if m.detail.answer.open {
					t.Fatal("small esc failed")
				}
			})
		}
	}
}
