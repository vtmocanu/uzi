package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func retainedRecoveryRun(timing string) apitypes.RunDTO {
	r := workerExhaustionRun(3)
	cause := "retained_recovery_exhausted"
	r.RecoveryWaitCause = &cause
	r.RecoveryRetryNotBefore = nil
	if timing != "held" {
		stamp := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
		if timing == "due" {
			stamp = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
		}
		r.RecoveryRetryNotBefore = &stamp
	}
	latest := true
	r.CheckpointContainsLatest = &latest
	return r
}

func TestRetainedRecoveryCLIConsumers(t *testing.T) {
	for _, timing := range []string{"future", "due", "held"} {
		for _, evidence := range []bool{false, true} {
			for _, verb := range []string{"get", "list", "inputs"} {
				t.Run(fmt.Sprintf("%s/%v/%s", timing, evidence, verb), func(t *testing.T) {
					r := retainedRecoveryRun(timing)
					if evidence {
						tip := "abc123"
						r.WorkerRecovery.Evidence = &apitypes.WorkerRecoveryEvidenceDTO{
							CheckpointTip: &tip, AvailableCapture: true, PublicationUncertain: true,
							CaptureUncertain: true, CustodyUncertain: true, Unknown: true,
							RecordedAt: time.Date(2026, 10, 7, 1, 2, 3, 0, time.UTC),
						}
					} else {
						r.WorkerRecovery = nil
					}
					polls, resume := 0, false
					srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
					defer srv.Close()
					args := []string{"run", verb}
					if verb != "list" {
						args = append(args, "r1")
					}
					out, errout, code := runCLI(t, httpEnv(srv), args...)
					if code != 0 || resume {
						t.Fatalf("exit %d mutation %v: %s", code, resume, errout)
					}
					copy := out + errout
					requireWorkerCopy(t, copy, "retained recovery exhausted", "uzi run resume r1", "uzi run cancel r1")
					if timing == "held" {
						requireWorkerCopy(t, copy, "owner-held", "no automatic resume")
						if strings.Contains(copy, "timed retry") {
							t.Fatalf("held run has timer: %s", copy)
						}
					} else {
						requireWorkerCopy(t, copy, "timed retry", r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339))
						if strings.Contains(copy, "owner-held") || strings.Contains(copy, "no automatic resume") {
							t.Fatalf("timed run held: %s", copy)
						}
					}
					for _, bad := range []string{"automatic requeues", "lifetime charged", "fresh allowance", "fresh archive", "contains the latest work", "resumes on its own"} {
						if strings.Contains(copy, bad) {
							t.Errorf("misleading %q: %s", bad, copy)
						}
					}
					if verb == "inputs" {
						want := "queued (timed retry)"
						if timing == "held" {
							want = "queued (owner Resume/Cancel)"
						}
						requireWorkerCopy(t, out, want)
					}
					if verb == "get" {
						requireWorkerCopy(t, copy, "availability", "latest local edits")
						if evidence {
							requireWorkerCopy(t, copy, "recorded checkpoint abc123", "2026-10-07T01:02:03Z",
								"may later expire or be discarded", "publication pending or uncertain",
								"capture pending or uncertain", "retained source custody uncertain")
						} else {
							requireWorkerCopy(t, copy, "evidence unavailable or unknown")
						}
					}
				})
			}
		}
	}
}

func TestRetainedRecoveryJSON(t *testing.T) {
	for _, timing := range []string{"future", "due", "held"} {
		t.Run(timing, func(t *testing.T) {
			r := retainedRecoveryRun(timing)
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
			defer srv.Close()
			out, errout, code := runCLI(t, httpEnv(srv), "run", "get", "r1", "--json")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			stamp, ok := got["recovery_retry_not_before"]
			if !ok || (string(stamp) == "null") != (timing == "held") {
				t.Fatalf("nullable timing lost: %s", out)
			}
			var roundtrip apitypes.RunDTO
			if err := json.Unmarshal([]byte(out), &roundtrip); err != nil {
				t.Fatal(err)
			}
			if roundtrip.WorkerRecovery == nil || roundtrip.WorkerRecovery.Episode != 4 || roundtrip.WorkerRecovery.EpisodeUsed != 3 {
				t.Fatalf("historical worker-death episode changed: %s", out)
			}
			polls = 0
			out, errout, code = runCLI(t, httpEnv(srv), "run", "inputs", "r1", "--json")
			if code != 0 || polls != 0 || !strings.HasPrefix(strings.TrimSpace(out), "[") || errout != "" {
				t.Fatalf("raw input JSON did GetRun or added copy: exit %d polls %d: %s %s", code, polls, out, errout)
			}
		})
	}
}

func TestRetainedRecoveryWait(t *testing.T) {
	for _, tc := range []struct {
		name       string
		timings    []string
		until      []string
		wantPolls  int
		heldNotice bool
	}{
		{"future to held", []string{"future", "held"}, nil, 2, true},
		{"due to held", []string{"due", "held"}, nil, 2, true},
		{"future to completed", []string{"future"}, nil, 2, false},
		{"due to completed", []string{"due"}, nil, 2, false},
		{"held default", []string{"held"}, nil, 1, true},
		{"explicit defaults", []string{"future", "held"}, []string{"--until", strings.Join(defaultWaitStates, ",")}, 3, true},
		{"explicit recovery", []string{"future"}, []string{"--until", "recovery_wait"}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs []apitypes.RunDTO
			for _, timing := range tc.timings {
				runs = append(runs, retainedRecoveryRun(timing))
			}
			done := retainedRecoveryRun("held")
			done.Status = "completed"
			runs = append(runs, done)
			polls, resume := 0, false
			srv := workerExhaustionServer(t, runs, &polls, &resume)
			defer srv.Close()
			args := append([]string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "1s", "--min-plan-seq", "99"}, tc.until...)
			_, errout, code := runCLI(t, httpEnv(srv), args...)
			if code != 0 || polls != tc.wantPolls || resume {
				t.Fatalf("exit %d polls %d want %d mutation %v: %s", code, polls, tc.wantPolls, resume, errout)
			}
			if strings.Contains(errout, "owner-held") != tc.heldNotice {
				t.Fatalf("finite-to-NULL notice lost: %s", errout)
			}
		})
	}
}

func TestRetainedRecoveryFollow(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	for _, timing := range []string{"future", "due"} {
		for _, end := range []string{"held", "completed"} {
			for _, asJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%v", timing, end, asJSON), func(t *testing.T) {
					r := retainedRecoveryRun(timing)
					last := retainedRecoveryRun("held")
					if end == "completed" {
						last.Status = end
					}
					polls, resume := 0, false
					srv := workerExhaustionServer(t, []apitypes.RunDTO{r, r, last, {ID: "r1", Status: "completed"}}, &polls, &resume)
					defer srv.Close()
					args := []string{"run", "logs", "r1", "--follow"}
					if asJSON {
						args = append(args, "--json")
					}
					out, errout, code := runCLI(t, httpEnv(srv), args...)
					if code != 0 || polls != 3 || resume || out != "" {
						t.Fatalf("exit %d polls %d mutation %v: %s %s", code, polls, resume, out, errout)
					}
					requireWorkerCopy(t, errout, "timed retry", "still following")
					if strings.Count(errout, "timed retry") != 1 || strings.Contains(errout, "owner-held") != (end == "held") {
						t.Fatalf("wrong transition notices: %s", errout)
					}
				})
			}
		}
	}
}

func TestRetainedRecoveryTUI(t *testing.T) {
	for _, timing := range []string{"future", "due", "held"} {
		for _, width := range []int{60, 80, 200} {
			for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
				t.Run(fmt.Sprintf("%s/%d/%v", timing, width, profile), func(t *testing.T) {
					r := retainedRecoveryRun(timing)
					row := apitypes.RunListItemDTO{RunDTO: r}
					word, band := "recovery retry", bandFloor
					if timing == "held" {
						word, band = "recovery held", bandNeedsYou
					}
					m := tuiTestModel(t, &uzicli.FakeClient{}, "")
					m.width, m.height = width, 30
					m = step(m, tea.ColorProfileMsg{Profile: profile})
					m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{row}})
					if tok := m.pal.runStateToken(r, false); tok.word != word || runStateWord(row) != word || runBandOf(row) != band {
						t.Fatalf("typed timing lost: token %+v word %s band %d", tok, runStateWord(row), runBandOf(row))
					}
					m.board.filter = word
					if got := m.board.visible(); len(got) != 1 || got[0].ID != r.ID {
						t.Fatal("display word filter lost run")
					}
					flush := func(m tuiModel) string {
						var b bytes.Buffer
						w := colorprofile.Writer{Forward: &b, Profile: profile}
						if _, err := w.Write([]byte(m.View().Content)); err != nil {
							t.Fatal(err)
						}
						return stripANSI(b.String())
					}
					out := flush(m)
					requireWorkerCopy(t, out, word, "Resume now/Cancel", "detail: enter", bandNames[band])
					m = tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
					m = applyDetail(m, r, nil)
					m.detail.steer.access = steerAllowed
					m.detail.steer.queue = []apitypes.SteerInputDTO{{Kind: "follow_up", CreatedAt: time.Now()}}
					m = step(m, tea.ColorProfileMsg{Profile: profile})
					m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
					out = flush(m)
					requireWorkerCopy(t, out, word, "uzi run resume r1", "uzi run cancel r1", "availability and latest edits unverified")
					if timing == "held" {
						requireWorkerCopy(t, out, "no automatic resume", "owner Resume/Cancel")
					} else {
						requireWorkerCopy(t, out, "Timed retry not before", "queued (timed retry)")
					}
					for _, bad := range []string{"automatic requeues used", "contains the latest work", "fresh allowance"} {
						if strings.Contains(out, bad) {
							t.Errorf("misleading %q: %s", bad, out)
						}
					}
					for _, line := range strings.Split(out, "\n") {
						if visualWidth(line) > width {
							t.Errorf("overflow: %q", line)
						}
					}
				})
			}
		}
	}
}

func TestRetainedRecoveryNarrowAndStale(t *testing.T) {
	if got := steerState(queuedIn(), statusRecoveryWait, "retained_recovery_exhausted"); got != "queued (retained recovery; retry timing unknown)" {
		t.Fatalf("cause-only projection inferred timing: %q", got)
	}
	if _, word := stateGlyphWord(statusRecoveryWait, "", false, false, "", "", "retained_recovery_exhausted"); word != "recovery wait" {
		t.Fatalf("cause-only glyph inferred timing: %q", word)
	}
	for _, status := range []string{"running", "completed"} {
		r := retainedRecoveryRun("held")
		r.Status = status
		row := apitypes.RunListItemDTO{RunDTO: r}
		if runBandOf(row) == bandNeedsYou || strings.Contains(runStateWord(row), "recovery") {
			t.Fatalf("stale metadata classified retained hold: %+v", row)
		}
		out, errout, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}), "run", "get", "r1")
		if code != 0 || strings.Contains(out+errout, "retained recovery exhausted") {
			t.Fatalf("stale copy: %d %s %s", code, out, errout)
		}
	}
}

func TestRetainedRecoveryOwnerActions(t *testing.T) {
	for _, timing := range []string{"future", "due", "held"} {
		for _, verb := range []string{"resume", "cancel"} {
			for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusConflict} {
				t.Run(fmt.Sprintf("%s/%s/%d", timing, verb, status), func(t *testing.T) {
					posts := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						want := "/api/runs/r1/resume-now"
						if verb == "cancel" {
							want = "/api/runs/r1/inputs"
						}
						// PRD #3522: human `run resume` first reads the run's hold to word the
						// weekly-reserve override line; serve that read, it is not an owner action.
						if verb == "resume" && req.Method == http.MethodGet && req.URL.Path == "/api/runs/r1" {
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(retainedRecoveryRun(timing))
							return
						}
						if req.Method != http.MethodPost || req.URL.Path != want {
							t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
							http.NotFound(w, req)
							return
						}
						posts++
						if verb == "cancel" {
							var body map[string]any
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["kind"] != "cancel" {
								t.Errorf("cancel body %+v: %v", body, err)
							}
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						if status != http.StatusOK {
							_, _ = w.Write([]byte(`{"error":"owner action refused"}`))
							return
						}
						r := retainedRecoveryRun(timing)
						r.Status = "queued"
						if verb == "cancel" {
							_, _ = w.Write([]byte(`{"server_side":true}`))
						} else {
							_ = json.NewEncoder(w).Encode(map[string]any{"run": r})
						}
					}))
					defer srv.Close()
					_, errout, code := runCLI(t, httpEnv(srv), "run", verb, "r1")
					wantCode := map[int]int{http.StatusOK: 0, http.StatusNotFound: uzicli.ExitNotFound, http.StatusConflict: uzicli.ExitConflict}[status]
					if code != wantCode || posts != 1 {
						t.Fatalf("exit %d want %d posts %d: %s", code, wantCode, posts, errout)
					}
					fc := &uzicli.FakeClient{}
					m := tuiTestModel(t, fc, "r1")
					m = applyDetail(m, retainedRecoveryRun(timing), nil)
					m.detail.steer.access = steerAccessFor(m.detail.run, nil)
					m, cmd, _ := m.steerKey("x")
					if cmd != nil || m.detail.steer.mode != steerConfirming {
						t.Fatal("cancel did not require confirmation")
					}
					_, cmd, _ = m.steerKey("y")
					if cmd == nil {
						t.Fatal("confirmed cancel not available")
					}
					msg := cmd().(steerResultMsg)
					if msg.kind != kindCancel || fc.LastInputKind != kindCancel {
						t.Fatal("TUI cancel did not use existing input verb")
					}
					m.detail.steer.mode = steerIdle
					m.detail.steer.access = steerAccessFor(m.detail.run, uzicli.Exitf(uzicli.ExitNotFound, "not owner"))
					_, cmd, handled := m.steerKey("x")
					if cmd != nil || handled {
						t.Fatal("non-owner gained cancel")
					}
				})
			}
		}
	}
}
