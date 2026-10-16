package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestPlanTranscriptExtractsFullMarkdown(t *testing.T) {
	plan := "  # Implementation plan\n\n" + strings.Repeat("- Preserve this complete step.\n", 40) + "\nUnique final acceptance line.\n  "
	payload, err := json.Marshal(map[string]string{"plan_md": plan, "text": "ordinary text"})
	if err != nil {
		t.Fatal(err)
	}
	if got := transcriptText(laneFrame{Kind: "plan", Payload: payload}); got != plan {
		t.Fatalf("plan markdown was changed or truncated: got %q, want %q", got, plan)
	}
	if got := transcriptText(laneFrame{Kind: "text", Payload: payload}); got != "ordinary text" {
		t.Fatalf("non-plan extraction changed: %q", got)
	}
}

func TestPlanTranscriptFallback(t *testing.T) {
	for _, payload := range []string{
		"", "{", "null", "[]", "{}", `{"plan_md":null}`, `{"plan_md":42}`,
		`{"plan_md":false}`, `{"plan_md":[]}`, `{"plan_md":{}}`,
		`{"plan_md":""}`, `{"plan_md":" \n\t "}`,
	} {
		t.Run(payload, func(t *testing.T) {
			got := transcriptText(laneFrame{Kind: "plan", Payload: json.RawMessage(payload)})
			want := transcriptText(laneFrame{Kind: "text", Payload: json.RawMessage(payload)})
			if got != want {
				t.Fatalf("ordinary fallback changed: got %q, want %q", got, want)
			}
		})
	}
	for _, tc := range []struct{ payload, want string }{
		{`{"plan_md":{},"text":"safe ordinary text","name":"ignored"}`, "safe ordinary text"},
		{`{"plan_md":[],"name":"safe name"}`, "`safe name`"},
		{`{"plan_md":null,"text":"null fallback"}`, "null fallback"},
		{`{"plan_md":" \n","name":"blank fallback"}`, "`blank fallback`"},
		{`{"text":"missing fallback"}`, "missing fallback"},
	} {
		if got := transcriptText(laneFrame{Kind: "plan", Payload: json.RawMessage(tc.payload)}); got != tc.want {
			t.Errorf("payload %s: got %q, want %q", tc.payload, got, tc.want)
		}
	}
}

func TestPlanTranscriptRenderedAndScrollable(t *testing.T) {
	plan := "# Plan opening marker\n\n" + strings.Repeat("- Keep each implementation step visible.\n", 55) + "\nUnique final acceptance line."
	payload, err := json.Marshal(map[string]string{"plan_md": plan})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "completed"} {
		for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
			t.Run(status+"/"+profile.String(), func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "plan-transcript")
				m = applyDetail(m, apitypes.RunDTO{ID: "plan-transcript", Status: status, Health: "ok"},
					[]apitypes.MessageDTO{leadMsg(1, "plan", string(payload), time.Now())})
				m = step(m, tea.ColorProfileMsg{Profile: profile})
				all := transcriptOf(t, m)
				for _, want := range []string{"▪ plan", "Plan opening marker", "Unique final acceptance line."} {
					if !strings.Contains(all, want) {
						t.Fatalf("transcript missing %q\n%s", want, all)
					}
				}
				m = press(t, m, "tab")
				// Each direction is bounded by 150 keypresses, beyond this fixture's rendered length.
				for range 150 {
					m = press(t, m, "up")
				}
				render := func() string {
					var out bytes.Buffer
					w := colorprofile.NewWriter(&out, nil)
					w.Profile = profile
					if _, err := w.Write([]byte(m.View().Content)); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(out.String(), "\x1b[38;") || strings.Contains(out.String(), "\x1b[48;") {
						t.Fatal("non-color frame retained color escapes")
					}
					return ansi.Strip(out.String())
				}
				top := render()
				statusCue := "running"
				if status == "completed" {
					statusCue = "✓ done"
				}
				for _, want := range []string{"▪ plan", "Plan opening marker", statusCue, "esc back", "pane"} {
					if !strings.Contains(top, want) {
						t.Fatalf("top view missing %q\n%s", want, top)
					}
				}
				if strings.Contains(top, "Unique final acceptance line.") {
					t.Fatal("long-plan fixture did not overflow the viewport")
				}
				for range 150 {
					m = press(t, m, "down")
				}
				if bottom := render(); !strings.Contains(bottom, "Unique final acceptance line.") {
					t.Fatalf("could not scroll to final plan line\n%s", bottom)
				}
			})
		}
	}
}

func TestPlanTranscriptUXLabScenes(t *testing.T) {
	for _, status := range []string{"running", "completed"} {
		for _, dark := range []bool{false, true} {
			top := ansi.Strip(detailLongPlan(dark, time.Now(), status, false).View().Content)
			bottom := ansi.Strip(detailLongPlan(dark, time.Now(), status, true).View().Content)
			for _, want := range []string{"▪ plan", "Implementation plan"} {
				if !strings.Contains(top, want) {
					t.Fatalf("%s dark=%v: top scene missing %q\n%s", status, dark, want, top)
				}
			}
			const final = "The complete plan remains readable through this final line."
			if strings.Contains(top, final) || !strings.Contains(bottom, final) {
				t.Fatalf("%s dark=%v: scenes did not navigate from opening to final line\n%s", status, dark, bottom)
			}
		}
	}
}

func TestPlanTranscriptSanitizesHostileMarkdown(t *testing.T) {
	plan := "# Safe heading\n\nBefore\x1b[2Jafter\x1b]52;c;clipboard-canary\a\nLast\x00line"
	payload, err := json.Marshal(map[string]string{"plan_md": plan})
	if err != nil {
		t.Fatal(err)
	}
	m := loadDetail(t, "hostile-plan", "ok", []apitypes.MessageDTO{
		leadMsg(1, "plan", string(payload), time.Now()),
	})
	lane, _ := m.detail.selectedLane()
	raw := strings.Join(m.buildTranscriptLines(lane), "\n")
	for _, forbidden := range []string{"\x1b[2J", "\x1b]52", "\a", "\x00"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("hostile plan retained %q", forbidden)
		}
	}
	if want := m.renderer.Markdown(plan); !strings.Contains(raw, strings.TrimLeft(want, "\n")) {
		t.Fatal("plan did not pass through the existing safe Markdown renderer")
	}
	for _, want := range []string{"▪ plan", "Safe heading", "Before", "after", "Lastline"} {
		if !strings.Contains(ansi.Strip(raw), want) {
			t.Errorf("sanitized plan missing %q\n%s", want, ansi.Strip(raw))
		}
	}
}
