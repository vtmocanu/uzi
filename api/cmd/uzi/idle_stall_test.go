package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/uzicli"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestIdleStallConsumers(t *testing.T) {
	for _, tc := range []struct {
		name, field, word string
		held              bool
	}{
		{"omitted", "", "recovery unknown", false},
		{"null", `,"recovery_retry_not_before":null`, "recovery held", true},
		{"future", `,"recovery_retry_not_before":"2099-01-01T00:00:00Z"`, "recovery retry", false},
		{"due", `,"recovery_retry_not_before":"2000-01-01T00:00:00Z"`, "recovery retry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := `{"id":"r1","status":"recovery_wait","recovery_wait_cause":"codex_idle_stall"` + tc.field + `}`
			var full apitypes.RunDTO
			var compact apitypes.RunSummaryItemDTO
			if err := json.Unmarshal([]byte(row), &full); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(row), &compact); err != nil {
				t.Fatal(err)
			}
			for _, run := range []apitypes.RunDTO{full, compact.ListItem().RunDTO} {
				if got := runStateWord(apitypes.RunListItemDTO{RunDTO: run}); got != tc.word {
					t.Fatalf("word %q", got)
				}
				if isOwnerRequiredWorkerHold(run) != tc.held {
					t.Fatal("incorrect wait/owner attention predicate")
				}
				if (runBandOf(apitypes.RunListItemDTO{RunDTO: run}) == bandNeedsYou) != tc.held {
					t.Fatal("incorrect board band")
				}
				if got := newPalette(true).runStateToken(run, false).word; got != tc.word {
					t.Fatalf("token %q", got)
				}
				line := workerExhaustionLine(run)
				requireWorkerCopy(t, line, "Codex idle stall", "fresh wall", "resets the idle-stall count", "uzi run resume r1", "uzi run cancel r1")
				if strings.Contains(line, "owner-held") != tc.held {
					t.Fatal(line)
				}
				if tc.name == "omitted" && !strings.Contains(steerStateForRun(queuedIn(), run), "timing unknown") {
					t.Fatal("partial input rendering promised retry")
				}
			}
			polls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				polls++
				if polls > 1 {
					_, _ = fmt.Fprint(w, `{"run":{"id":"r1","status":"completed"}}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"run":%s}`, row)
			}))
			defer srv.Close()
			_, errout, code := runCLI(t, httpEnv(srv), "run", "wait", "r1", "--interval", "1ms", "--timeout", "1s")
			wantPolls := 2
			if tc.held {
				wantPolls = 1
			}
			if code != 0 || polls != wantPolls {
				t.Fatalf("exit=%d polls=%d want=%d: %s", code, polls, wantPolls, errout)
			}
		})
	}
	if got := steerState(queuedIn(), statusRecoveryWait, "codex_idle_stall"); !strings.Contains(got, "timing unknown") {
		t.Fatal(got)
	}
}

func TestIdleStallNoticeTimingTransitions(t *testing.T) {
	var keys []string
	for _, field := range []string{"", `,"recovery_retry_not_before":null`, `,"recovery_retry_not_before":"2099-01-01T00:00:00Z"`} {
		var run apitypes.RunDTO
		if err := json.Unmarshal([]byte(`{"status":"recovery_wait","recovery_wait_cause":"codex_idle_stall"`+field+`}`), &run); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, workerHoldNoticeKey(run))
	}
	if keys[0] == keys[1] || keys[0] == keys[2] || keys[1] == keys[2] {
		t.Fatal(keys)
	}
}

func TestIdleStallCLIReadsAndFollow(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	for _, timing := range []string{"held", "future", "due"} {
		t.Run(timing, func(t *testing.T) {
			run := retainedRecoveryRun(timing)
			cause := "codex_idle_stall"
			run.RecoveryWaitCause = &cause
			for _, verb := range []string{"get", "list", "inputs", "logs"} {
				polls, resumed := 0, false
				srv := workerExhaustionServer(t, []apitypes.RunDTO{run, run, {ID: "r1", Status: "completed"}}, &polls, &resumed)
				args := []string{"run", verb}
				if verb != "list" {
					args = append(args, "r1")
				}
				if verb == "logs" {
					args = append(args, "--follow", "--json")
				}
				out, errout, code := runCLI(t, httpEnv(srv), args...)
				srv.Close()
				if code != 0 || resumed {
					t.Fatalf("%s exit=%d resumed=%v: %s", verb, code, resumed, errout)
				}
				requireWorkerCopy(t, out+errout, "Codex idle stall", "fresh wall", "resets the idle-stall count")
				if verb == "logs" {
					wantPolls := 3
					if timing == "held" {
						wantPolls = 1
					}
					if out != "" || polls != wantPolls {
						t.Fatalf("follow output=%q polls=%d want=%d: %s", out, polls, wantPolls, errout)
					}
				}
			}
			row := apitypes.RunListItemDTO{RunDTO: run}
			for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
				m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
				m = applyDetail(m, apitypes.RunSummaryOf(row).ListItem().RunDTO, nil)
				m = step(m, tea.ColorProfileMsg{Profile: profile})
				m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
				requireWorkerCopy(t, stripANSI(m.View().Content), "Codex idle stall", "fresh wall", "idle-stall count")
			}
		})
	}
}
