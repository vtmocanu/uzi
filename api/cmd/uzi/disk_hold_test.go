package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func diskTimingRun(t *testing.T, timing string) apitypes.RunDTO {
	t.Helper()
	field := ""
	switch timing {
	case "held":
		field = `,"recovery_retry_not_before":null`
	case "future":
		field = `,"recovery_retry_not_before":"2099-01-02T03:04:05Z"`
	case "due":
		field = `,"recovery_retry_not_before":"2000-01-02T03:04:05Z"`
	case "zero":
		field = `,"recovery_retry_not_before":"0001-01-01T00:00:00Z"`
	}
	var r apitypes.RunDTO
	if err := json.Unmarshal([]byte(`{"id":"r1","kind":"issue","status":"recovery_wait","recovery_wait_cause":"data_volume_full","disk_park_count":7`+field+"}"), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Decode real wire shapes before exercising the CLI and board classification.
// Due stamps, including year 1, remain timed; omission never implies NULL.
func TestDiskTimingSurfaces(t *testing.T) {
	for _, timing := range []string{"future", "due", "held", "omitted", "zero"} {
		t.Run(timing, func(t *testing.T) {
			r := diskTimingRun(t, timing)
			held := timing == "held"
			row := apitypes.RunListItemDTO{RunDTO: r}
			if (runBandOf(row) == bandNeedsYou) != held {
				t.Fatal("disk attention band disagrees with wire timing")
			}
			p := newPalette(true)
			token := p.runStateToken(r, false)
			if held && (token.word != "disk held" || token.color != p.amber || runStateWord(row) != "disk held") {
				t.Fatalf("held token: %+v, word %s", token, runStateWord(row))
			}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}, Runs: []apitypes.RunListItemDTO{row}, AdminRuns: []apitypes.RunListItemDTO{row}}
			for _, args := range [][]string{{"run", "get", "r1"}, {"run", "list"}, {"admin", "runs"}} {
				out, stderr, code := runCLI(t, fakeEnv(fc), args...)
				if code != 0 {
					t.Fatalf("%v: exit %d: %s", args, code, stderr)
				}
				switch timing {
				case "held":
					requireWorkerCopy(t, out, "held", "Resume/Cancel")
					if args[1] == "get" {
						requireWorkerCopy(t, out, "no automatic resume", "one further ordinary attempt", "counters persist", "admission may refuse", "Local unpublished work is not guaranteed on worker loss")
					}
				case "omitted":
					requireWorkerCopy(t, out, "timing unknown")
					if strings.Contains(out, "resumes at") || strings.Contains(out, "owner-held") {
						t.Fatalf("unknown timing promised a retry or hold: %s", out)
					}
				default:
					requireWorkerCopy(t, out, "waiting for disk space")
				}
			}
			wantSteer := "queued (run waiting for disk space)"
			if held {
				wantSteer = "queued (disk held; owner Resume/Cancel)"
			} else if timing == "omitted" {
				wantSteer = "queued (run waiting for disk space; retry timing unknown)"
			}
			if got := steerStateWithRun(queuedIn(), r.Status, []string{dataVolumeFullCause}, &r); got != wantSteer {
				t.Fatalf("steer state %q want %q", got, wantSteer)
			}
			for _, status := range []string{"queued", "running", "completed"} {
				stale := r
				stale.Status = status
				if runStateWord(apitypes.RunListItemDTO{RunDTO: stale}) == "disk held" || strings.Contains(renderDetailString(t, stale), "DISK") {
					t.Fatal("stale disk metadata rendered as a park")
				}
			}
		})
	}
}

func TestDiskWaitFollowTimingControls(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	for _, timing := range []string{"future", "due", "held", "omitted", "zero", "timed-to-held"} {
		for _, verb := range []string{"wait", "explicit wait", "follow"} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/json=%v", timing, verb, jsonOutput), func(t *testing.T) {
					polls := 0
					r := diskTimingRun(t, timing)
					if timing == "timed-to-held" {
						r = diskTimingRun(t, "due")
					}
					fc := &uzicli.FakeClient{
						LogsByID: map[string][]apitypes.MessageDTO{"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hello"}`)}}},
						GetRunHook: func(_ string) (apitypes.RunDTO, error) {
							polls++
							if polls == 1 {
								return r, nil
							}
							if timing == "timed-to-held" && polls == 2 {
								return diskTimingRun(t, "held"), nil
							}
							done := r
							done.Status = "completed"
							return done, nil
						},
					}
					args := []string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "1s"}
					if verb == "explicit wait" {
						args = append(args, "--until", "completed")
					}
					if verb == "follow" {
						args = []string{"run", "logs", "r1", "--follow"}
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					out, stderr, code := runCLI(t, fakeEnv(fc), args...)
					wantPolls := 2
					if timing == "held" && verb != "explicit wait" {
						wantPolls = 1
					}
					if timing == "timed-to-held" && verb == "explicit wait" {
						wantPolls = 3
					}
					if code != 0 || polls != wantPolls {
						t.Fatalf("exit %d polls %d want %d: %s", code, polls, wantPolls, stderr)
					}
					if timing == "held" || timing == "timed-to-held" {
						requireWorkerCopy(t, stderr, "owner-held", "uzi run resume r1", "uzi run cancel r1", "counters persist", "admission may refuse")
						if strings.Count(stderr, "automatic disk retries exhausted") != 1 {
							t.Fatalf("hold notice missing or repeated: %s", stderr)
						}
					}
					if jsonOutput && verb == "follow" {
						for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
							if !json.Valid([]byte(line)) {
								t.Fatalf("prose in NDJSON: %q", line)
							}
						}
					} else if jsonOutput && !json.Valid([]byte(out)) {
						t.Fatalf("invalid wait JSON: %s", out)
					}
					if fc.LastResumeRunID != "" || fc.LastInputKind != "" {
						t.Fatal("wait/follow performed an owner action")
					}
				})
			}
		}
	}
}

// Each explicit action makes one POST, including a refusal or a server error.
// The HTTP client and bodyless Resume interface stay unchanged.
func TestDiskOwnerActionsSinglePOST(t *testing.T) {
	for _, verb := range []string{"resume", "cancel"} {
		for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s/%d", verb, status), func(t *testing.T) {
				posts := 0
				r := diskTimingRun(t, "held")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if req.Method == http.MethodGet && req.URL.Path == "/api/runs/r1" {
						_ = json.NewEncoder(w).Encode(r)
						return
					}
					want := "/api/runs/r1/resume-now"
					if verb == "cancel" {
						want = "/api/runs/r1/inputs"
					}
					if req.Method != http.MethodPost || req.URL.Path != want {
						t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
						http.NotFound(w, req)
						return
					}
					posts++
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
					}
					if verb == "resume" && len(body) != 0 {
						t.Errorf("Resume gained a body: %s", body)
					}
					if verb == "cancel" {
						var input map[string]any
						if err := json.Unmarshal(body, &input); err != nil || input["kind"] != "cancel" {
							t.Errorf("cancel body %s: %v", body, err)
						}
					}
					w.WriteHeader(status)
					if status != http.StatusOK {
						_, _ = w.Write([]byte(`{"error":"ordinary admission refused"}`))
					} else if verb == "cancel" {
						_, _ = w.Write([]byte(`{"server_side":true}`))
					} else {
						r.Status = "queued"
						_ = json.NewEncoder(w).Encode(map[string]any{"run": r})
					}
				}))
				defer srv.Close()
				_, stderr, code := runCLI(t, httpEnv(srv), "run", verb, "r1")
				wantCode := map[int]int{http.StatusOK: 0, http.StatusNotFound: uzicli.ExitNotFound, http.StatusConflict: uzicli.ExitConflict, http.StatusInternalServerError: uzicli.ExitUnreachable}[status]
				if code != wantCode || posts != 1 {
					t.Fatalf("exit %d want %d POSTs %d: %s", code, wantCode, posts, stderr)
				}
			})
		}
	}
}

func TestDiskHoldTUILayoutAndSanitization(t *testing.T) {
	for _, timing := range []string{"held", "omitted", "zero"} {
		for _, width := range []int{40, 60, 80, 200} {
			t.Run(fmt.Sprintf("%s/%d", timing, width), func(t *testing.T) {
				r := diskTimingRun(t, timing)
				r.ID = "r1\x1b[2J\nFORGED"
				m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
				m = applyDetail(m, r, nil)
				m = step(m, tea.WindowSizeMsg{Width: width, Height: 40})
				raw := m.View().Content
				assertNoRawControls(t, "disk detail", raw)
				out := stripANSI(raw)
				for _, line := range diskHoldLines(r) {
					assertNoRawControls(t, "unwrapped disk guidance", line)
					if strings.ContainsAny(line, "\r\n") {
						t.Fatalf("unwrapped guidance contains an injected row: %q", line)
					}
				}
				switch timing {
				case "held":
					copy := strings.Join(strings.Fields(out), " ")
					requireWorkerCopy(t, copy, "Resume", "Cancel", "counters persist", "admission may refuse", "Local unpublished work is not guaranteed on worker loss.")
				case "omitted":
					requireWorkerCopy(t, out, "timing unknown")
					if strings.Contains(out, "resumes in") || strings.Contains(out, "retry 00:00") {
						t.Fatal("unknown timing got a countdown")
					}
				default:
					if !strings.Contains(out, "retry") && !strings.Contains(out, "resumes at") {
						t.Fatal("valid year-1 timing lost its retry guidance")
					}
					if strings.Contains(out, "timing unknown") {
						t.Fatal("valid year-1 timing became unknown")
					}
				}
				rows := strings.Split(out, "\n")
				if len(rows) != m.height {
					t.Fatalf("detail height %d want %d", len(rows), m.height)
				}
				for i, row := range rows {
					if visualWidth(row) > width {
						t.Fatalf("row %d exceeds width %d: %q", i, width, row)
					}
				}
				m = tuiTestModel(t, &uzicli.FakeClient{}, "")
				m.width, m.height = width, 20
				m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{{RunDTO: r}}})
				out = stripANSI(m.View().Content)
				switch timing {
				case "held":
					requireWorkerCopy(t, out, "disk held", "Resume/Cancel")
				case "omitted":
					requireWorkerCopy(t, out, "timing unknown")
				default:
					if !strings.Contains(out, "retry") && !strings.Contains(out, "resumes at") {
						t.Fatal("valid year-1 board timing lost its retry guidance")
					}
					if strings.Contains(out, "timing unknown") {
						t.Fatal("valid year-1 board timing became unknown")
					}
				}
				for _, row := range strings.Split(out, "\n") {
					if visualWidth(row) > width {
						t.Fatalf("board exceeds width %d: %q", width, row)
					}
				}
			})
		}
	}
}

func TestDiskTimingDateValidation(t *testing.T) {
	for _, stamp := range []string{"not-a-date", "2026-02-30T00:00:00Z"} {
		t.Run(stamp, func(t *testing.T) {
			r := diskTimingRun(t, "omitted")
			body := []byte(`{"status":"recovery_wait","recovery_wait_cause":"data_volume_full","recovery_retry_not_before":` + quoteJSON(stamp) + "}")
			if err := json.Unmarshal(body, &r); err == nil {
				t.Fatal("malformed retry timestamp decoded")
			}
			if got := diskWaitOf(r); got != diskWaitUnknown {
				t.Fatalf("rejected timestamp inferred state %v", got)
			}
		})
	}
	for _, year := range []int{-1, 0, 1, 9999, 10000} {
		t.Run(fmt.Sprintf("typed-year-%d", year), func(t *testing.T) {
			r := diskTimingRun(t, "held")
			stamp := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
			r.RecoveryRetryNotBefore = &stamp
			want := diskWaitTimed
			if year < 0 || year > 9999 {
				want = diskWaitUnknown
			}
			if got := diskWaitOf(r); got != want {
				t.Fatalf("typed year %d state %v want %v", year, got, want)
			}
		})
	}
}

func TestDiskHoldDetailHeightBudget(t *testing.T) {
	const id = "12345678-1234-4234-8234-123456789abc"
	for _, size := range []struct{ width, height int }{
		{40, 16}, {40, 12}, {40, 10}, {30, 16}, {30, 12}, {30, 10}, {40, 40}, {80, 40},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			r := diskTimingRun(t, "held")
			r.ID = id
			m := tuiTestModel(t, &uzicli.FakeClient{}, id)
			// Cover populated transcripts here; the hostile-ID matrix covers empty
			// transcripts. Both retain exact physical height and footer assertions.
			m = applyDetail(m, r, []apitypes.MessageDTO{{Seq: 1, Kind: "text", Payload: json.RawMessage(`{"text":"Disk recovery paused."}`)}})
			m = step(m, tea.WindowSizeMsg{Width: size.width, Height: size.height})
			raw := m.View().Content
			assertNoRawControls(t, "disk hold height", raw)
			out := stripANSI(raw)
			rows := strings.Split(out, "\n")
			if len(rows) != size.height {
				t.Fatalf("detail height %d want %d: %q", len(rows), size.height, out)
			}
			requireWorkerCopy(t, rows[len(rows)-1], "esc back", "? keys")
			requireWorkerCopy(t, out, "Resume/Cancel")
			for i, row := range rows {
				if visualWidth(row) > size.width {
					t.Fatalf("row %d exceeds width %d: %q", i, size.width, row)
				}
			}
			guidance := strings.Join(strings.Fields(out), " ")
			// Wrapping can break a UUID at a hyphen. Verify every command character
			// survives, without treating the physical line break as part of the ID.
			commandChars := strings.Join(strings.Fields(out), "")
			if size.height >= 40 || (size.width == 40 && size.height == 16) {
				requireWorkerCopy(t, commandChars, "uzirunresume"+id, "uziruncancel"+id)
			}
			switch {
			case size.height >= 40:
				requireWorkerCopy(t, guidance, "one further ordinary attempt", "counters persist", "admission may refuse", "Local unpublished work is not guaranteed on worker loss.")
			case size.width == 40 && size.height == 16:
				requireWorkerCopy(t, guidance, "Counted parks: 7", "counters persist", "One more attempt", "admission may refuse", "Worker loss may lose unpublished work.")
			default:
				requireWorkerCopy(t, guidance, "uzi run get")
			}
		})
	}
}

func TestDiskResumeHelp(t *testing.T) {
	out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "resume", "--help")
	if code != 0 {
		t.Fatalf("help exit %d: %s", code, stderr)
	}
	requireWorkerCopy(t, out, "data_volume_full", "one further ordinary attempt", "counters persist", "admission may refuse", "Local unpublished work is not guaranteed on worker loss")
}
