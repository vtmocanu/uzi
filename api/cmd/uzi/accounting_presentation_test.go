package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func accountingMsg(seq int32, role, instance string, at time.Time) apitypes.MessageDTO {
	m := msgDTO(seq, "status", role, instance, "accounting ghost", "", at)
	m.Payload = json.RawMessage(`{"event":"codex_response_usage","usage_response_id":"12345678-1234-4234-8234-123456789abc","usage":{"input_tokens":11,"output_tokens":12,"cache_creation_input_tokens":13,"cache_read_input_tokens":14},"model":"gpt-5"}`)
	return m
}

func accountingEvent(m apitypes.MessageDTO) apitypes.RunEventDTO {
	return apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: m.Seq, Kind: m.Kind, Agent: m.Agent, AgentInstance: m.AgentInstance, AgentLabel: m.AgentLabel, Payload: m.Payload, CreatedAt: &m.CreatedAt}
}

func TestAccountingLogsHumanJSON(t *testing.T) {
	now := time.Now().UTC()
	hidden := accountingMsg(2, "coder", "child", now)
	ordinary := []apitypes.MessageDTO{
		msgDTO(1, "status", "lead", "", "", "ordinary status", now),
		hidden,
		msgDTO(3, "text", "lead", "", "", "ordinary text", now),
		{Seq: 4, Kind: "tool_use", Payload: json.RawMessage(`{"name":"Bash","input":{"command":"pwd"}}`)},
	}
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": ordinary}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"ordinary status", "ordinary text", "Bash"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "codex_response_usage") || strings.Contains(out, "#2") {
		t.Errorf("accounting shown: %s", out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
	if code != 0 {
		t.Fatalf("JSON exit=%d", code)
	}
	var got []apitypes.MessageDTO
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m apitypes.MessageDTO
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if !reflect.DeepEqual(got, ordinary) {
		t.Fatalf("JSON did not retain complete DTOs: %+v", got)
	}
}

func TestAccountingLogsFollowCursor(t *testing.T) {
	for _, tail := range []bool{false, true} {
		t.Run(map[bool]string{false: "after", true: "tail"}[tail], func(t *testing.T) {
			now := time.Now().UTC()
			var afters []int32
			fc := &uzicli.FakeClient{
				LogsByID: map[string][]apitypes.MessageDTO{"r1": {accountingMsg(4, "lead", "", now), accountingMsg(5, "coder", "child", now)}},
				RunByID:  map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "completed"}},
				RunLogsHook: func(_ string, after int32) ([]apitypes.MessageDTO, error) {
					afters = append(afters, after)
					if after < 5 {
						return []apitypes.MessageDTO{accountingMsg(4, "lead", "", now), accountingMsg(5, "coder", "child", now)}, nil
					}
					return []apitypes.MessageDTO{msgDTO(6, "text", "lead", "", "", "later visible", now)}, nil
				},
			}
			args := []string{"run", "logs", "r1", "--follow"}
			if tail {
				args = append(args, "--tail", "2")
			} else {
				args = append(args, "--after", "3")
			}
			out, _, code := runCLI(t, fakeEnv(fc), args...)
			if code != 0 {
				t.Fatalf("exit=%d", code)
			}
			want := []int32{3, 5}
			if tail {
				want = []int32{5, 6}
			}
			if !reflect.DeepEqual(afters, want) {
				t.Errorf("afters=%v want=%v", afters, want)
			}
			if strings.Contains(out, "codex_response_usage") || !strings.Contains(out, "later visible") {
				t.Errorf("output=%s", out)
			}
		})
	}
}

func TestAccountingModelPresentation(t *testing.T) {
	now := time.Now().UTC()
	for _, dark := range []bool{false, true} {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
			t.Run(strings.Join([]string{map[bool]string{false: "light", true: "dark"}[dark], profile.String()}, "/"), func(t *testing.T) {
				m := uxModel(&uzicli.FakeClient{}, "r1", dark)
				m = step(m, tea.ColorProfileMsg{Profile: profile})
				m = applyDetail(m, apitypes.RunDTO{ID: "r1", Status: "running", Health: "ok"}, []apitypes.MessageDTO{
					{Seq: 9, Kind: "tool_use", Agent: sp("lead"), Payload: json.RawMessage(`{"name":"Bash","input":{"command":"pwd"}}`), CreatedAt: now.Add(-2 * time.Minute)},
					msgDTO(10, "status", "lead", "", "", "ordinary status", now.Add(-time.Minute)),
					msgDTO(11, "text", "coder", "child", "implementation", "ordinary text", now),
				})
				if got := stripANSI(m.backfillBadge()); got != "⇡ loading earlier" {
					t.Errorf("baseline badge=%q", got)
				}
				baseline := m.View().Content
				flush := func(content string) string {
					var output bytes.Buffer
					writer := colorprofile.Writer{Forward: &output, Profile: profile}
					if _, err := writer.WriteString(content); err != nil {
						t.Fatal(err)
					}
					return output.String()
				}
				flushedBaseline := flush(baseline)
				for _, want := range []string{"⇡ loading earlier", "ordinary status", "ordinary text", "Bash"} {
					if !strings.Contains(stripANSI(flushedBaseline), want) {
						t.Errorf("terminal profile lost plain cue %q", want)
					}
				}
				if profile == colorprofile.TrueColor && !strings.Contains(flushedBaseline, "38;2;") {
					t.Error("TrueColor positive control emitted no foreground colors")
				}
				// Ascii may retain non-color attributes such as bold; NoTTY strips all SGR.
				if profile == colorprofile.Ascii {
					for _, color := range []string{"38;2;", "48;2;", "38;5;", "48;5;"} {
						if strings.Contains(flushedBaseline, color) {
							t.Errorf("Ascii terminal retained color sequence %q", color)
						}
					}
				}
				if profile == colorprofile.NoTTY && strings.Contains(flushedBaseline, "\x1b[") {
					t.Error("NoTTY terminal retained SGR styles")
				}
				views := map[string]string{}
				for i, lane := range m.detail.lanes {
					m.detail.laneIdx = i
					views[lane.Key] = m.View().Content
				}
				m.detail.laneIdx = 0
				lanes := append([]agentLane(nil), m.detail.lanes...)
				active := activeLaneKey(m.detail.run.Status, m.detail.frames)
				activity := railActivity(m.detail.frames)
				m = step(m, streamEventsMsg{runID: "r1", gen: m.detail.gen, events: []apitypes.RunEventDTO{
					accountingEvent(accountingMsg(12, "lead", "", now.Add(time.Minute))),
					accountingEvent(accountingMsg(13, "coder", "ghost", now.Add(2*time.Minute))),
				}})
				if got := stripANSI(m.backfillBadge()); got != "⇡ loading earlier" {
					t.Errorf("badge=%q", got)
				}
				if m.View().Content != baseline {
					t.Error("accounting changed real model View")
				}
				if flush(m.View().Content) != flushedBaseline {
					t.Error("accounting changed flushed terminal output")
				}
				if len(m.detail.frames) != 5 || m.detail.lowSeq != 9 || m.detail.highSeq != 13 || len(m.detail.seen) != 5 {
					t.Errorf("raw state lost: %+v", frameSeqs(m))
				}
				if !reflect.DeepEqual(lanes, m.detail.lanes) {
					t.Error("lane counts/frames/LastActivity changed")
				}
				if activeLaneKey(m.detail.run.Status, m.detail.frames) != active {
					t.Error("active lane changed")
				}
				if !reflect.DeepEqual(railActivity(m.detail.frames), activity) {
					t.Error("rail activity changed")
				}
				for i := range m.detail.lanes {
					m.detail.laneIdx = i
					out := m.View().Content
					if out != views[m.detail.lanes[i].Key] {
						t.Errorf("lane %d transcript/cache/View changed", i)
					}
					if strings.Contains(out, "codex_response_usage") || strings.Contains(out, "accounting ghost") {
						t.Errorf("accounting transcript lane %d: %s", i, out)
					}
				}
			})
		}
	}
}

func TestAccountingOnlyPages(t *testing.T) {
	now := time.Now().UTC()
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {
		msgDTO(1, "text", "lead", "", "", "earlier visible", now),
		accountingMsg(2, "coder", "ghost", now), accountingMsg(3, "lead", "", now),
		accountingMsg(4, "lead", "", now), accountingMsg(5, "coder", "ghost", now),
		accountingMsg(6, "lead", "", now), accountingMsg(7, "coder", "ghost", now),
		msgDTO(8, "text", "lead", "", "", "later visible", now),
	}}}
	shrinkPageSize(t, 2)
	all := fc.LogsByID["r1"]
	fc.LogsByID["r1"] = all[:5]
	m := tuiTestModel(t, fc, "r1")
	m = step(m, detailRunMsg{runID: "r1", run: apitypes.RunDTO{ID: "r1", Status: "running"}})
	nm, cmd := m.Update(m.loadTailCmd("r1")())
	m = nm.(tuiModel)
	if !m.detail.tailLoaded || m.detail.lowSeq != 4 || m.detail.highSeq != 5 || len(m.detail.lanes) != 0 {
		t.Errorf("accounting-only tail raw/presentation state wrong")
	}
	empty := applyDetail(tuiTestModel(t, fc, "r1"), apitypes.RunDTO{ID: "r1", Status: "running"}, nil)
	if m.renderTranscript() != empty.renderTranscript() {
		t.Error("accounting-only loaded tail differs from normal empty transcript")
	}
	if cmd == nil {
		t.Fatal("tail stopped backfill")
	}
	nm, cmd = m.Update(cmd())
	m = nm.(tuiModel)
	if m.detail.lowSeq != 2 || cmd == nil || m.detail.backfillFailed || len(m.detail.lanes) != 0 {
		t.Fatal("accounting-only backfill did not continue raw cursor")
	}
	nm, cmd = m.Update(cmd())
	m = nm.(tuiModel)
	if m.detail.lowSeq != 1 || !m.detail.historyComplete || cmd != nil || !strings.Contains(stripANSI(m.View().Content), "earlier visible") {
		t.Fatal("earlier visible frame not loaded")
	}

	stream := uzicli.NewRunStream(m.ctx, nil)
	defer stream.Close()
	m = step(m, streamReadyMsg{runID: "r1", gen: m.detail.gen, stream: stream})
	if stream.LastSeen() != 5 {
		t.Errorf("replay floor=%d", stream.LastSeen())
	}
	fc.LogsByID["r1"] = all
	m.detail.catchupWaitID = 42
	nm, cmd = m.Update(m.catchupCmd("r1", 5, 42)())
	m = nm.(tuiModel)
	if m.detail.highSeq != 7 || stream.LastSeen() != 7 || cmd == nil || m.detail.catchupWaitID != 42 {
		t.Fatal("accounting catchup did not advance and continue")
	}
	nm, cmd = m.Update(cmd())
	m = nm.(tuiModel)
	if m.detail.highSeq != 8 || !strings.Contains(stripANSI(m.View().Content), "later visible") {
		t.Fatal("later visible frame not loaded")
	}
	if cmd == nil {
		t.Fatal("expected empty-page catchup")
	}
	nm, cmd = m.Update(cmd())
	m = nm.(tuiModel)
	if cmd != nil || m.detail.catchupWaitID != 0 {
		t.Fatal("catchup did not end")
	}
	// Replay duplicates and new live accounting both retain raw evidence without a ghost lane.
	m = step(m, streamEventsMsg{runID: "r1", gen: m.detail.gen, events: []apitypes.RunEventDTO{accountingEvent(fc.LogsByID["r1"][5]), accountingEvent(accountingMsg(9, "coder", "ghost", now))}})
	if m.detail.highSeq != 9 || len(m.detail.frames) != 9 || len(m.detail.lanes) != 1 {
		t.Fatalf("live/replay state=%v lanes=%d", frameSeqs(m), len(m.detail.lanes))
	}
}

func TestAccountingPredicateThroughPresentation(t *testing.T) {
	cases := []struct {
		name, kind, payload string
		hidden              bool
	}{
		{"exact", "status", `{"event":"codex_response_usage"}`, true},
		{"wrong kind", "text", `{"event":"codex_response_usage"}`, false},
		{"ordinary status", "status", `{"event":"usage"}`, false},
		{"prefix", "status", `{"event":"codex_response_usage_extra"}`, false},
		{"case", "status", `{"event":"CODEX_RESPONSE_USAGE"}`, false},
		{"wrong key", "status", `{"Event":"codex_response_usage"}`, false},
		{"nested", "status", `{"other":{"event":"codex_response_usage"}}`, false},
		{"nonstring", "status", `{"event":true}`, false},
		{"missing", "status", `{"text":"codex_response_usage"}`, false},
		{"malformed", "status", `{"event":"codex_response_usage"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := msgDTO(1, tc.kind, "lead", "", "", "", time.Now().UTC())
			msg.Payload = json.RawMessage(tc.payload)
			fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {msg}}}
			out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
			if code != 0 || (out == "") != tc.hidden {
				t.Errorf("human output=%q exit=%d hidden=%v", out, code, tc.hidden)
			}
			m := applyDetail(tuiTestModel(t, fc, "r1"), apitypes.RunDTO{ID: "r1", Status: "running"}, []apitypes.MessageDTO{msg})
			if (len(m.detail.lanes) == 0) != tc.hidden {
				t.Errorf("lanes=%v hidden=%v", m.detail.lanes, tc.hidden)
			}
			if len(m.detail.frames) != 1 || m.detail.highSeq != 1 {
				t.Fatal("raw evidence lost")
			}
		})
	}
}

func TestAccountingLogsOnlyWindow(t *testing.T) {
	for _, args := range [][]string{{"--tail", "2"}, {"--after", "3"}} {
		fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {
			accountingMsg(4, "lead", "", time.Now().UTC()), accountingMsg(5, "coder", "child", time.Now().UTC()),
		}}}
		out, _, code := runCLI(t, fakeEnv(fc), append([]string{"run", "logs", "r1"}, args...)...)
		if code != 0 || out != "" {
			t.Errorf("args=%v exit=%d output=%q", args, code, out)
		}
		out, _, code = runCLI(t, fakeEnv(fc), append(append([]string{"run", "logs", "r1"}, args...), "--json")...)
		if code != 0 {
			t.Fatalf("JSON exit=%d", code)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 {
			t.Fatalf("JSON lines=%q", out)
		}
		for i, line := range lines {
			var m apitypes.MessageDTO
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m, fc.LogsByID["r1"][i]) {
				t.Errorf("JSON DTO changed: %+v", m)
			}
		}
	}
}

func TestAccountingStreamRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := time.Now().UTC()
	events := []apitypes.RunEventDTO{
		accountingEvent(accountingMsg(1, "lead", "", now)),
		accountingEvent(accountingMsg(2, "coder", "ghost", now)),
		accountingEvent(msgDTO(3, "text", "lead", "", "", "visible after stream accounting", now)),
	}
	stream := uzicli.NewRunStream(ctx, events)
	defer stream.Close()
	m := applyDetail(tuiTestModel(t, &uzicli.FakeClient{}, "r1"), apitypes.RunDTO{ID: "r1", Status: "running"}, nil)
	nm, read := m.Update(streamReadyMsg{runID: "r1", gen: m.detail.gen, stream: stream})
	m = nm.(tuiModel)
	// At most one read per seeded event; the context bounds each blocking read.
	// Any failed read ends this test rather than leaving a sibling stream running.
	for attempts := 0; len(m.detail.frames) < len(events) && attempts < len(events); attempts++ {
		if read == nil {
			t.Fatal("stream read stopped")
		}
		nm, read = m.Update(read())
		m = nm.(tuiModel)
	}
	if !reflect.DeepEqual(frameSeqs(m), []int32{1, 2, 3}) || m.detail.highSeq != 3 || m.detail.lowSeq != 1 {
		t.Fatalf("raw stream frames=%v", frameSeqs(m))
	}
	if len(m.detail.lanes) != 1 || !strings.Contains(stripANSI(m.View().Content), "visible after stream accounting") {
		t.Fatal("live accounting added lanes or swallowed visible text")
	}
}

func TestAccountingUXLabScene(t *testing.T) {
	for _, dark := range []bool{true, false} {
		name := "light"
		if dark {
			name = "dark"
		}
		t.Run(name, func(t *testing.T) {
			m := detailAccountingBackfill(dark, time.Now())
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			frame := m.View().Content
			plain := stripANSI(frame)
			if len(m.detail.frames) != 4 || len(m.detail.lanes) != 3 || m.detail.lanes[0].Key != laneAllKey {
				t.Fatal("scene raw evidence or visible roster incorrect")
			}
			if !strings.Contains(plain, "⇡ loading earlier") || strings.Contains(plain, " of 13") || strings.Contains(plain, "codex_response_usage") || strings.Contains(plain, "accounting ghost") {
				t.Fatalf("scene has ghost content or wrong badge: %s", plain)
			}
			if !strings.Contains(plain, "Reviewing the implementation.") || !strings.Contains(plain, "Running focused tests.") {
				t.Fatal("scene lost ordinary transcript")
			}
			if os.Getenv("UZI_UXLAB_GEN") == "1" {
				out := os.Getenv("UZI_UXLAB_OUT_DIR")
				if out == "" {
					t.Fatal("dedicated scene generation requires UZI_UXLAB_OUT_DIR")
				}
				if err := os.MkdirAll(out, 0o750); err != nil { //nolint:gosec // G703: opt-in developer generation uses the caller's output directory, never run data.
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, "detail-accounting-backfill-"+name+".ansi"), []byte(frame), 0o600); err != nil { //nolint:gosec // G703: opt-in developer generation uses the caller's output directory, never run data.
					t.Fatal(err)
				}
			}
		})
	}
}
