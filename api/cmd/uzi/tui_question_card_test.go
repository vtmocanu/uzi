package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const cardFixture = `{"questions":[{"header":"History cap","question":"How much history?","options":[{"label":"All history","description":"Keep the complete log"},{"label":"Recent","description":""}]},{"header":"Git boundary","question":"Where should Git stop?"}]}`

func cardModel(t *testing.T, payload string) tuiModel {
	t.Helper()
	m := questionViewModel(t, payload)
	m.detail.run.Status = "awaiting_input"
	m.width, m.height = 100, 40
	return m
}

func cardUpdateZ(t *testing.T, m tuiModel) tuiModel {
	t.Helper()
	next, _ := m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
	return next.(tuiModel)
}

func cardRows(out string) []string {
	var rows []string
	for _, line := range strings.Split(stripANSI(out), "\n") {
		if strings.HasPrefix(line, "┃") {
			rows = append(rows, line)
		}
	}
	return rows
}

func TestTUIQuestionCardShapesAndToggle(t *testing.T) {
	m := cardModel(t, cardFixture)
	out := stripANSI(m.View().Content)
	for _, want := range []string{"┃ ✎ ANSWER REQUIRED · 2 questions · parked until answered", "┃ 1/2 · History cap", "How much history?", "1. All history", "Keep the complete log", "2. Recent", "┃ 2/2 · Git boundary", "CREW", "z collapse"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	rows := cardRows(out)
	if len(rows) > 12 || !strings.Contains(strings.Join(rows, "\n"), "2. Recent\n┃ 2/2") {
		t.Fatalf("card cap or empty-description spacing: %q", rows)
	}
	m = cardUpdateZ(t, m)
	const summary = "┃ ✎ ANSWER REQUIRED · 2 questions (History cap, Git boundary) · z expand"
	out = stripANSI(m.View().Content)
	if rows = cardRows(out); len(rows) != 1 || rows[0] != summary || !strings.Contains(out, "CREW") {
		t.Fatalf("collapsed View: %q\n%s", rows, out)
	}
	m = cardUpdateZ(t, m)
	if m.detail.questionCollapsed || len(cardRows(m.View().Content)) < 2 {
		t.Fatal("second Update z failed to expand")
	}
	single := cardModel(t, `{"questions":[{"question":"Single body","options":[{"label":"Only","description":""}]}]}`)
	if out = stripANSI(single.View().Content); !strings.Contains(out, "1 question · parked") || !strings.Contains(out, "┃ 1/1") || !strings.Contains(out, "1. Only") {
		t.Fatalf("singular/no-header card:\n%s", out)
	}
}

func TestTUIQuestionCardLatestRunWideEvent(t *testing.T) {
	q := func(seq int32, payload string) apitypes.MessageDTO {
		return apitypes.MessageDTO{Seq: seq, Kind: "question", Payload: json.RawMessage(payload)}
	}
	a := func(seq int32) apitypes.MessageDTO {
		return apitypes.MessageDTO{Seq: seq, Kind: "answer", Payload: json.RawMessage("malformed answer")}
	}
	for _, tc := range []struct {
		name   string
		frames []apitypes.MessageDTO
		want   string
		open   bool
	}{
		{"question then malformed answer", []apitypes.MessageDTO{q(2, cardFixture), a(3)}, "", false},
		{"answer then newest question out of order", []apitypes.MessageDTO{q(7, `{"questions":[{"question":"Newest body"}]}`), a(5), q(2, cardFixture)}, "Newest body", true},
		{"newest malformed question", []apitypes.MessageDTO{q(2, cardFixture), q(8, "raw-newest-marker")}, "raw-newest-marker", true},
		{"missing id", []apitypes.MessageDTO{q(2, cardFixture)}, "How much history?", true},
		{"malformed id", []apitypes.MessageDTO{q(2, `{"question_id":[],"questions":[{"question":"Bad id body"}]}`)}, "Bad id body", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "card-run")
			frames := append(tc.frames, msgDTO(20, "text", "coder", "other", "other", "unrelated-lane", time.Now()))
			m = applyDetail(m, apitypes.RunDTO{ID: "card-run", Status: "awaiting_input"}, frames)
			for i, lane := range m.detail.lanes {
				if lane.Role == "coder" {
					m.detail.laneIdx = i
				}
			}
			rows := cardRows(m.View().Content)
			if (len(rows) > 0) != tc.open || tc.open && !strings.Contains(strings.Join(rows, "\n"), tc.want) {
				t.Fatalf("runwide newest event card: %q", rows)
			}
			if tc.name == "newest malformed question" && strings.Contains(strings.Join(rows, "\n"), "History cap") {
				t.Fatal("malformed latest question resurrected older card")
			}
			m.detail.run.Status = "running"
			if len(cardRows(m.View().Content)) != 0 || cardUpdateZ(t, m).detail.questionCollapsed {
				t.Fatal("card/toggle survived status exit")
			}
		})
	}
}

func TestTUIQuestionCardBudgetsAndCompleteOverflow(t *testing.T) {
	// At width 100 the card body has 96 cells; this unbroken word must
	// wrap losslessly to five rows, including its marker beyond rune 200.
	body := strings.Repeat("x", 384) + "TAIL"
	payload := `{"questions":[{"question":` + quoteJSON(body) + `}]}`
	for _, height := range []int{24, 25, 30, 40, 60} {
		m := cardModel(t, payload)
		m.height = height
		out := stripANSI(m.View().Content)
		rows := cardRows(out)
		capRows := min(12, (m.transcriptBudgetWithoutCard()+1)/2, m.transcriptBudgetWithoutCard()-3)
		if len(rows) > capRows || len(strings.Split(out, "\n")) > height || m.transcriptViewport() < 3 {
			t.Fatalf("height %d cap/viewport failure: card %d cap %d vp %d\n%s", height, len(rows), capRows, m.transcriptViewport(), out)
		}
		// Complete content is one indexed header + five body rows.
		if capRows < 9 {
			visible := capRows - 4
			want := fmt.Sprintf("┃ … +%d lines", 6-visible)
			if !strings.Contains(strings.Join(rows, "\n"), want) {
				t.Fatalf("overflow must count complete prose: want %s in %q", want, rows)
			}
		} else if !strings.Contains(strings.Join(rows, "\n"), "TAIL") {
			t.Fatalf("content beyond 200 lost: %q", rows)
		}
		for _, line := range strings.Split(out, "\n") {
			if visualWidth(line) > 100 {
				t.Fatalf("overflow at height %d: %q", height, line)
			}
		}
	}
	for _, height := range []int{10, 16, 23} {
		m := cardModel(t, cardFixture)
		m.height = height
		m.detail.streamErr = fmt.Errorf("offline")
		m.detail.steer.mode = steerTyping
		out := stripANSI(m.View().Content)
		rows := cardRows(out)
		expected := 0
		if m.transcriptBudgetWithoutCard() >= 4 {
			expected = 1
		}
		if len(rows) != expected || !strings.Contains(out, "NEEDS INPUT") {
			t.Fatalf("short fallback height %d: %q\n%s", height, rows, out)
		}
		if !strings.Contains(out, stripANSI(m.transportLine())) || !strings.Contains(out, stripANSI(m.renderSteerBar())) {
			t.Fatalf("short card displaced transport/steer:\n%s", out)
		}
	}
}

func TestTUIQuestionCardLosslessFieldsAndGuards(t *testing.T) {
	for _, field := range []string{"header", "question", "label", "description"} {
		t.Run(field, func(t *testing.T) {
			long := strings.Repeat("word ", 45) + field + "-tail"
			q := map[string]any{"question": "body"}
			switch field {
			case "header", "question":
				q[field] = long
			default:
				q["options"] = []map[string]string{{"label": "choice", field: long}}
			}
			payload, err := json.Marshal(map[string]any{"questions": []any{q}})
			if err != nil {
				t.Fatal(err)
			}
			m := cardModel(t, string(payload))
			card := strings.Join(cardRows(m.View().Content), "\n")
			if !strings.Contains(card, field+"-tail") {
				t.Fatalf("long field lost beyond 200: %s", card)
			}
		})
	}
	m := cardModel(t, cardFixture)
	m.detail.runLoaded = false
	if len(cardRows(m.View().Content)) != 0 || !strings.Contains(m.View().Content, "loading") {
		t.Fatal("card escaped initial loading guard")
	}
	m.detail.runLoaded = true
	m.detail.loadErr = fmt.Errorf("load failed")
	if len(cardRows(m.View().Content)) != 0 {
		t.Fatal("card escaped error guard")
	}
	// Check the actual joined pane has its title plus at least three content rows.
	m = cardModel(t, cardFixture)
	m.height = 24
	out := stripANSI(m.View().Content)
	start := strings.Index(out, "TRANSCRIPT")
	stop := strings.Index(out, "NEEDS INPUT")
	if start < 0 || stop < start || strings.Count(out[start:stop], "▏") < 3 {
		t.Fatalf("24-row View lost transcript content rows:\n%s", out)
	}
}

func TestTUIQuestionCardViewPreservesWholeWords(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, word := range []string{
			"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789END",
			strings.Repeat("a", 245) + "END",
		} {
			for _, field := range []string{"header", "question", "label", "description"} {
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
					raw, err := json.Marshal(map[string]any{"questions": []any{q}})
					if err != nil {
						t.Fatal(err)
					}
					m := cardModel(t, string(raw))
					m.height, m.dark = 100, dark
					m.renderer, err = newTUIRenderer(m.transcriptWidth(), dark)
					if err != nil {
						t.Fatal(err)
					}
					rows := cardRows(m.View().Content)
					if len(rows) > 12 || strings.Contains(strings.Join(rows, "\n"), "… +") {
						t.Fatalf("fixture must show complete pinned card: %q", rows)
					}
					var prose strings.Builder
					for _, row := range rows {
						prose.WriteString(strings.TrimSpace(strings.TrimPrefix(row, "┃")))
					}
					if !strings.Contains(prose.String(), word) {
						t.Fatalf("%s lost characters at a wrap boundary: want %q in %q", field, word, prose.String())
					}
				})
			}
		}
	}
}

func TestTUIQuestionCardMalformedPayloadBeyondCellCap(t *testing.T) {
	m := cardModel(t, strings.Repeat("x", 245)+"raw-tail")
	rows := cardRows(m.View().Content)
	if !strings.Contains(strings.Join(rows, "\n"), "raw-tail") {
		t.Fatalf("malformed question text truncated at the cell cap: %q", rows)
	}
}

func TestTUIQuestionCardPopulatedRail(t *testing.T) {
	run, frames := autoFoldRun(true, true)
	run.Status = "awaiting_input"
	frames = append(frames, apitypes.MessageDTO{Seq: 30, Kind: "question", Payload: json.RawMessage(cardFixture)})
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 40
	m = applyDetail(m, run, frames)
	for _, collapsed := range []bool{false, true} {
		if collapsed {
			m = cardUpdateZ(t, m)
		}
		out := stripANSI(m.View().Content)
		for _, want := range []string{"CREW", "MILESTONES", "SPEND", "ACCOUNTS", "runacct"} {
			if !strings.Contains(out, want) {
				t.Fatalf("collapsed %v missing %s:\n%s", collapsed, want, out)
			}
		}
		if len(strings.Split(out, "\n")) > 40 {
			t.Fatal("populated rail overflow")
		}
	}
}

func TestTUIQuestionCardPausedContentAnchor(t *testing.T) {
	m := cardModel(t, cardFixture)
	m.height = 24
	frames := []laneFrame{}
	for i := int32(2); i <= 70; i++ {
		frames = append(frames, laneFrameFromMessage(msgDTO(i, "text", "", "", "", fmt.Sprintf("anchor-line-%03d", i), time.Now())))
	}
	m.detail.addFrames(frames)
	m.detail.rebuild()
	m.detail.focus = focusTranscript
	m.detail.follow = false
	total, _ := m.transcriptExtent()
	m.detail.scroll = total - m.transcriptViewport() - 1
	first := func(m tuiModel) string {
		out := stripANSI(m.View().Content)
		for _, line := range strings.Split(out, "\n") {
			if divider := strings.Index(line, "▏"); divider >= 0 {
				content := line[divider+len("▏"):]
				if at := strings.Index(content, "anchor-line-"); at >= 0 {
					return strings.TrimSpace(content[at:])
				}
			}
		}
		t.Fatalf("no actual transcript content:\n%s", out)
		return ""
	}
	before, offset := first(m), m.detail.scroll
	m = cardUpdateZ(t, m)
	if got := first(m); got != before || m.detail.scroll != offset || m.detail.follow {
		t.Fatalf("collapsed content anchor %q -> %q offset %d -> %d follow %v", before, got, offset, m.detail.scroll, m.detail.follow)
	}
	m = cardUpdateZ(t, m)
	if first(m) != before || m.detail.scroll != offset {
		t.Fatal("expansion moved paused content")
	}
	for _, stale := range []int{-100, total + 100} {
		m.detail.scroll = stale
		_ = m.View()
	}
	m.detail.follow = true
	m.detail.scroll = 12
	m = cardUpdateZ(t, m)
	if !m.detail.follow || m.detail.scroll != 12 || !strings.Contains(m.View().Content, "anchor-line-070") {
		t.Fatal("toggle changed follow tail")
	}
}

func TestTUIQuestionCardPaddedBottomNavigation(t *testing.T) {
	for _, status := range []string{"awaiting_input", "running"} {
		t.Run(status, func(t *testing.T) {
			m := cardModel(t, cardFixture)
			m.height = 24
			for i := int32(2); i <= 70; i++ {
				m.detail.addFrames([]laneFrame{laneFrameFromMessage(msgDTO(i, "text", "", "", "", fmt.Sprintf("anchor-line-%03d", i), time.Now()))})
			}
			m.detail.rebuild()
			m.detail.focus, m.detail.follow = focusTranscript, false
			total, vp := m.transcriptExtent()
			m.detail.scroll = total - vp - 1
			first := func(m tuiModel) string {
				for _, row := range strings.Split(stripANSI(m.View().Content), "\n") {
					if divider := strings.Index(row, "▏"); divider >= 0 {
						content := row[divider+len("▏"):]
						if at := strings.Index(content, "anchor-line-"); at >= 0 {
							return strings.TrimSpace(content[at:])
						}
					}
				}
				t.Fatal("no transcript anchor in View")
				return ""
			}
			anchor, offset := first(m), m.detail.scroll
			m = cardUpdateZ(t, m)
			m.detail.run.Status = status
			total, vp = m.transcriptExtent()
			if offset <= max(0, total-vp) || first(m) != anchor {
				t.Fatal("fixture did not preserve a padded bottom anchor")
			}
			down := press(t, m, keyDown)
			if down.detail.scroll != offset || down.detail.follow || first(down) != anchor {
				t.Errorf("Down moved padded bottom: offset %d -> %d anchor %q -> %q follow %v", offset, down.detail.scroll, anchor, first(down), down.detail.follow)
			}
			expected := m
			expected.detail.scroll = offset - 1
			up := press(t, m, keyUp)
			if up.detail.scroll != offset-1 || up.detail.follow || first(up) != first(expected) || first(up) >= anchor {
				t.Errorf("Up must move one row older from saved anchor: offset %d -> %d anchor %q -> %q", offset, up.detail.scroll, anchor, first(up))
			}
			for _, stale := range []int{-100, total + 100} {
				m.detail.scroll = stale
				for _, key := range []string{keyDown, keyUp} {
					got := press(t, m, key)
					_ = got.View()
					if got.detail.scroll < 0 || got.detail.scroll > max(0, total-1) {
						t.Errorf("stale %d after %s: offset %d", stale, key, got.detail.scroll)
					}
				}
			}
			for _, live := range []string{"running", "claimed"} {
				m.detail.run.Status, m.detail.follow = live, false
				total, vp = m.transcriptExtent()
				m.detail.scroll = max(0, total-vp) - 1
				got := press(t, m, keyDown)
				if !got.detail.follow || got.detail.scroll != max(0, total-vp) {
					t.Errorf("%s normal bottom did not rearm follow", live)
				}
			}
		})
	}
}

func TestTUIQuestionCardLongSummaryAsciiWriter(t *testing.T) {
	for _, dark := range []bool{false, true} {
		t.Run(fmt.Sprintf("dark=%v", dark), func(t *testing.T) {
			m := cardModel(t, `{"questions":[{"header":`+quoteJSON(strings.Repeat("long header ", 30))+`,"question":"body"}]}`)
			m.dark = dark
			var err error
			m.renderer, err = newTUIRenderer(m.transcriptWidth(), dark)
			if err != nil {
				t.Fatal(err)
			}
			next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			m = cardUpdateZ(t, next.(tuiModel))
			var buf bytes.Buffer
			writer := colorprofile.NewWriter(&buf, nil)
			writer.Profile = colorprofile.Ascii
			if _, err := writer.Write([]byte(m.View().Content)); err != nil {
				t.Fatal(err)
			}
			rows := cardRows(buf.String())
			if len(rows) != 1 || !strings.HasSuffix(rows[0], ") · z expand") {
				t.Fatalf("long summary lost closing parenthesis or remedy: %q", rows)
			}
			for _, row := range strings.Split(stripANSI(buf.String()), "\n") {
				if visualWidth(row) > 100 {
					t.Fatalf("row exceeds 100 cells: %q", row)
				}
			}
			for _, width := range []int{10, 20, 35} {
				m.width = width
				rows := cardRows(m.View().Content)
				if len(rows) != 1 || visualWidth(rows[0]) > width || !strings.Contains(rows[0], "z expand") {
					t.Fatalf("narrow width %d lost remedy or overflowed: %q", width, rows)
				}
			}
		})
	}
}

func TestTUIQuestionCardKeyPrecedenceAndSessionReset(t *testing.T) {
	m := cardModel(t, cardFixture)
	m.detail.review.open = true
	if cardUpdateZ(t, m).detail.questionCollapsed {
		t.Fatal("z escaped review")
	}
	if len(cardRows(m.View().Content)) != 0 {
		t.Fatal("card escaped review guard")
	}
	m.detail.review.open = false
	m.detail.steer.mode = steerTyping
	next := cardUpdateZ(t, m)
	if next.detail.questionCollapsed || next.detail.steer.input != "z" {
		t.Fatal("z escaped typing")
	}
	m.detail.steer.mode = steerIdle
	m = cardUpdateZ(t, m)
	if !m.detail.questionCollapsed {
		t.Fatal("z did not collapse")
	}
	m = press(t, m, keyEsc)
	if m.detail.questionCollapsed {
		t.Fatal("exit retained collapse preference")
	}
	m.detail = newDetailState("question-run")
	if m.detail.questionCollapsed {
		t.Fatal("constructor retained collapse")
	}
	m = cardModel(t, cardFixture)
	m.detail.frames = nil
	if cardUpdateZ(t, m).detail.questionCollapsed {
		t.Fatal("z toggled without open event")
	}
}

func TestTUIQuestionCardAsciiWriter(t *testing.T) {
	m := cardModel(t, cardFixture)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	for _, collapsed := range []bool{false, true} {
		if collapsed {
			m = cardUpdateZ(t, m)
		}
		var buf bytes.Buffer
		writer := colorprofile.NewWriter(&buf, nil)
		writer.Profile = colorprofile.Ascii
		frame := m.View().Content
		if !strings.Contains(frame, "38;") {
			t.Fatal("no input color sequences: writer downgrade not exercised")
		}
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		raw := buf.String()
		// Ascii retains bold/reset SGR; the Writer must remove color SGR.
		if strings.Contains(raw, "38;") || strings.Contains(raw, "48;") {
			t.Fatal("writer retained color sequences")
		}
		out := stripANSI(raw)
		if !strings.Contains(out, "┃ ✎ ANSWER REQUIRED") || !strings.Contains(out, "CREW") {
			t.Fatalf("lost plain signals:\n%s", out)
		}
		if collapsed && !strings.Contains(out, "┃ ✎ ANSWER REQUIRED · 2 questions (History cap, Git boundary) · z expand\n") {
			t.Fatalf("literal collapsed line changed:\n%s", out)
		}
	}
}
