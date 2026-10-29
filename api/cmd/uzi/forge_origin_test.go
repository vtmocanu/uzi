package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func forgeTimingRun(t *testing.T, timing string) apitypes.RunDTO {
	t.Helper()
	var r apitypes.RunDTO
	if err := json.Unmarshal([]byte(`{"id":"r1","kind":"issue","status":"recovery_wait","recovery_wait_cause":"forge_unreachable","forge_park_count":3,"forge_park_max":3`+timing+`}`), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestForgeTimingSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want string
		held             bool
	}{
		{"null", `,"recovery_retry_not_before":null`, "owner-held", true},
		{"omitted", "", "retry timing unknown", false},
		{"future", `,"recovery_retry_not_before":"2099-01-02T03:04:05Z"`, "retry at", false},
		{"due", `,"recovery_retry_not_before":"2000-01-02T03:04:05Z"`, "retry at", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := forgeTimingRun(t, tc.wire)
			row := apitypes.RunListItemDTO{RunDTO: r}
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m = applyDetail(m, r, nil)
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			m = step(m, tea.WindowSizeMsg{Width: 80, Height: 50})
			m.detail.steer.access = steerAllowed
			for name, output := range map[string]string{
				"get":    renderDetailString(t, r),
				"list":   runStatusCell(row),
				"detail": stripANSI(m.View().Content),
			} {
				requireWorkerCopy(t, output, tc.want)
				if strings.Contains(output, "owner-held") != tc.held {
					t.Fatalf("%s fabricated or lost hold: %s", name, output)
				}
			}
			if isForgeOwnerHold(r) != tc.held || (runBandOf(row) == bandNeedsYou) != tc.held {
				t.Fatal("hold control classification wrong")
			}
			if tc.held {
				requireWorkerCopy(t, stripANSI(m.View().Content), "uzi run resume r1", "uzi run cancel r1", "Counters, cooldown, custody and grants preserved", "Export only an available archive")
				if runStateWord(row) != "forge held" || m.pal.runStateToken(r, false).word != "forge held" {
					t.Fatal("missing textual owner attention")
				}
				requireWorkerCopy(t, stripANSI(m.boardSecondLine(row)), "Resume/Cancel")
			}
		})
	}
	// A configuration change to unlimited does not release an existing NULL hold.
	// Only the forge cause and explicit retry timing identify this policy.
	r := forgeTimingRun(t, `,"recovery_retry_not_before":null`)
	r.ForgeParkMax = 0
	if !isForgeOwnerHold(r) {
		t.Fatal("configuration change hid existing owner hold")
	}
	r.ForgeParkMax = 3
	r.Status = "running"
	if isForgeOwnerHold(r) {
		t.Fatal("running run fabricated hold")
	}
}

func TestForgeWaitAndFollowControls(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	for _, timing := range []string{"held", "timed", "omitted"} {
		for _, verb := range []string{"wait", "follow", "explicit wait"} {
			t.Run(timing+"/"+verb, func(t *testing.T) {
				wire := ""
				if timing == "held" {
					wire = `,"recovery_retry_not_before":null`
				}
				if timing == "timed" {
					wire = `,"recovery_retry_not_before":"2000-01-02T03:04:05Z"`
				}
				r := forgeTimingRun(t, wire)
				// FakeClient preserves omitted timing; marshal would add a NULL.
				polls := 0
				fc := &uzicli.FakeClient{GetRunHook: func(_ string) (apitypes.RunDTO, error) {
					polls++
					if polls == 1 {
						return r, nil
					}
					done := r
					done.Status = "completed"
					return done, nil
				}}
				args := []string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "1s"}
				if verb == "follow" {
					args = []string{"run", "logs", "r1", "--follow"}
				}
				if verb == "explicit wait" {
					args = append(args, "--until", "completed")
				}
				_, stderr, code := runCLI(t, fakeEnv(fc), args...)
				want := 2
				if timing == "held" && verb != "explicit wait" {
					want = 1
				}
				if code != 0 || polls != want {
					t.Fatalf("exit %d polls %d want %d: %s", code, polls, want, stderr)
				}
				if timing == "held" {
					requireWorkerCopy(t, stderr, "owner-held", "Resume", "Cancel")
				}
			})
		}
	}
}

func TestForgeResumeUsesOrdinaryEndpoint(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": forgeTimingRun(t, `,"recovery_retry_not_before":null`)}, ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
		if rejected {
			fc.ResumeRunNowErr = uzicli.Exitf(uzicli.ExitConflict, "ordinary admission blocked: limit 1 or saturation")
		}
		_, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
		if fc.LastResumeRunID != "r1" {
			t.Fatal("resume did not use ordinary endpoint")
		}
		if rejected && (code != uzicli.ExitConflict || !strings.Contains(stderr, "ordinary admission blocked")) {
			t.Fatalf("lost refusal: %d %s", code, stderr)
		}
		if !rejected && code != 0 {
			t.Fatalf("resume exit %d: %s", code, stderr)
		}
	}
}

func TestForgeCooldownStatesAndHostileRenderers(t *testing.T) {
	const hostile = "\x1b[2J\x1b]0;bad\x07\x01\u202e\u2066\n\r\t"
	until := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	id := "exact-half-open-run" + hostile
	for _, state := range []string{"cooling", "half_open", "retry_eligible_unverified", "legacy_serialized_until_upgrade", "reserved_unverified"} {
		t.Run(state, func(t *testing.T) {
			c := apitypes.ForgeOriginCooldownDTO{Origin: "https://forge.example" + hostile, CooledUntil: until, HalfOpenRunID: &id, State: state, Detail: "advisory-detail" + hostile}
			w := apitypes.WorkerDTO{ID: "w1", Name: "worker-one", Status: "online", ForgeOriginCooldowns: []apitypes.ForgeOriginCooldownDTO{c}}
			r := forgeTimingRun(t, `,"recovery_retry_not_before":null`)
			r.ForgeOriginCooldown = &c
			fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{w}, RunByID: map[string]apitypes.RunDTO{"r1": r}}
			for _, args := range [][]string{{"worker", "list"}, {"worker", "get", "w1"}, {"run", "get", "r1"}} {
				out, stderr, code := runCLI(t, fakeEnv(fc), args...)
				if code != 0 {
					t.Fatalf("%v exit %d: %s", args, code, stderr)
				}
				assertNoRawControls(t, strings.Join(args, " "), out)
				requireWorkerCopy(t, out, "https://forge.example", "2000-01-02T03:04:05Z", "exact-half-open-run", "advisory-detail", "recovery unverified", "does not verify forge reachability")
				if strings.Contains(out, "\nFORGED") {
					t.Fatal("injected row")
				}
			}
			m := workerRenderModel(false)
			m.width, m.height = 200, 70
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			row := workerRow{w: w}
			m.workers.rows = []workerRow{row}
			m.workerDetail = workerDetailState{workerID: w.ID}
			lines, _ := m.workerDetailLines(time.Now())
			for name, out := range map[string]string{
				"attention": strings.Join(func() []string {
					var s []string
					for _, a := range workerAttention(row, time.Now()) {
						s = append(s, a.attnDetail)
					}
					return s
				}(), "\n"),
				"readout": strings.Join(m.workerReadout(row, 200), "\n"),
				"detail":  strings.Join(lines, "\n"),
			} {
				assertNoRawControls(t, name, out)
				requireWorkerCopy(t, stripANSI(out), "https://forge.example", "2000-01-02T03:04:05Z", "exact-half-open-run", "advisory-detail", "recovery unverified")
			}
			rowOut := m.workerRowLine(row, true, 200, m.workerTableWidths([]workerRow{row}, 200), time.Now())
			assertNoRawControls(t, "worker row", rowOut)
			requireWorkerCopy(t, stripANSI(rowOut), "forge:", "https://forge.example")
			m = tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m = applyDetail(m, r, nil)
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			m = step(m, tea.WindowSizeMsg{Width: 120, Height: 60})
			out := m.View().Content
			assertNoRawControls(t, "run detail", out)
			requireWorkerCopy(t, stripANSI(out), "2000-01-02T03:04:05Z", "exact-half-open-run", "advisory-detail")
		})
	}
}
