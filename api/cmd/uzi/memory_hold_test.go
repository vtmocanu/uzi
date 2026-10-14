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

func memoryHeldRun() apitypes.RunDTO {
	r := workerExhaustionRun(3)
	cause := "worker_memory_pressure"
	r.RecoveryWaitCause = &cause
	max, remaining := int32(2), int32(0)
	r.MemoryHold = &apitypes.MemoryHoldDTO{Episode: 7, Used: 2, Max: &max, Remaining: &remaining}
	latest := true
	r.CheckpointContainsLatest = &latest
	return r
}

func TestMemoryHoldCLIConsumers(t *testing.T) {
	for _, verb := range []string{"get", "list", "inputs"} {
		t.Run(verb, func(t *testing.T) {
			r := memoryHeldRun()
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
			defer srv.Close()
			args := []string{"run", verb}
			if verb != "list" {
				args = append(args, "r1")
			}
			out, errout, code := runCLI(t, httpEnv(srv), args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			copy := out + errout
			requireWorkerCopy(t, copy, "worker memory pressure", "2 of 2 memory interventions used", "0 remaining", "episode 7",
				"uzi run resume r1", "uzi run cancel r1", "fresh memory episode", "Cancel is terminal", "no automatic resume")
			if verb == "get" {
				requireWorkerCopy(t, out, "Historical checkpoints and captures do not verify current availability or latest edits")
			}
			if verb == "inputs" {
				requireWorkerCopy(t, out, "queued (owner Resume/Cancel)")
			}
			for _, bad := range []string{"automatic requeues used", "lifetime charged", "contains the latest work", "resumes on its own"} {
				if strings.Contains(copy, bad) {
					t.Errorf("misleading %q: %s", bad, copy)
				}
			}
			if resume {
				t.Fatal("read operation resumed hold")
			}
		})
	}
}

func TestMemoryHoldCLIUnknownAndJSON(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		hold       *apitypes.MemoryHoldDTO
	}{
		{"missing", "memory intervention allowance unknown", nil},
		{"unknown policy", "2 of unknown memory interventions used · unknown remaining · episode 7", &apitypes.MemoryHoldDTO{Episode: 7, Used: 2}},
		{"known zero", "0 of 0 memory interventions used · 0 remaining · episode 7", &apitypes.MemoryHoldDTO{Episode: 7, Max: new(int32), Remaining: new(int32)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := memoryHeldRun()
			r.MemoryHold = tc.hold
			env := fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}})
			out, errout, code := runCLI(t, env, "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			requireWorkerCopy(t, out, tc.want)
			out, errout, code = runCLI(t, env, "run", "get", "r1", "--json")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			var got apitypes.RunDTO
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if tc.hold != nil && (got.MemoryHold == nil || got.MemoryHold.Episode != 7 || (got.MemoryHold.Max == nil) != (tc.hold.Max == nil)) {
				t.Fatalf("memory JSON projection changed: %s", out)
			}
		})
	}
	// Stale memory metadata on a different cause or status must not render an owner memory hold.
	for _, tc := range []struct{ status, cause string }{{"running", "worker_memory_pressure"}, {"recovery_wait", "empty_turn"}} {
		r := memoryHeldRun()
		r.Status, r.RecoveryWaitCause = tc.status, &tc.cause
		out, errout, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}), "run", "get", "r1")
		if code != 0 || strings.Contains(out+errout, "memory interventions") {
			t.Fatalf("stale memory hold: exit %d %s %s", code, out, errout)
		}
	}
}

func TestMemoryHoldCLIWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		until []string
		polls int
	}{
		{"default", nil, 1},
		{"explicit defaults", []string{"--until", strings.Join(defaultWaitStates, ",")}, 2},
		{"explicit recovery", []string{"--until", "recovery_wait"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := memoryHeldRun()
			done := r
			done.Status = "completed"
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r, done}, &polls, &resume)
			defer srv.Close()
			args := append([]string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "1s", "--min-plan-seq", "99"}, tc.until...)
			_, errout, code := runCLI(t, httpEnv(srv), args...)
			if code != 0 || polls != tc.polls || resume {
				t.Fatalf("exit %d polls %d resume %v: %s", code, polls, resume, errout)
			}
			requireWorkerCopy(t, errout, "worker memory pressure", "uzi run resume r1", "uzi run cancel r1")
		})
	}
}

func TestMemoryHoldCLIFollow(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	held := memoryHeldRun()
	transient := held
	cause := "empty_turn"
	transient.RecoveryWaitCause = &cause
	polls, resume := 0, false
	done := held
	done.Status = "completed"
	srv := workerExhaustionServer(t, []apitypes.RunDTO{transient, held, done}, &polls, &resume)
	defer srv.Close()
	_, errout, code := runCLI(t, httpEnv(srv), "run", "logs", "r1", "--follow")
	if code != 0 || polls != 2 || resume {
		t.Fatalf("exit %d polls %d resume %v: %s", code, polls, resume, errout)
	}
	requireWorkerCopy(t, errout, "worker memory pressure", "fresh memory episode", "no automatic resume")
	for _, line := range strings.Split(errout, "\n") {
		if strings.Contains(line, "worker memory pressure") && strings.Contains(line, "still following") {
			t.Error(line)
		}
	}
}

func TestMemoryHoldTUIViews(t *testing.T) {
	for _, width := range []int{60, 80, 200} {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
			t.Run(fmt.Sprintf("%d/%v", width, profile), func(t *testing.T) {
				r := memoryHeldRun()
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m.width, m.height = width, 30
				m = step(m, tea.ColorProfileMsg{Profile: profile})
				m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{{RunDTO: r}}})
				flush := func(m tuiModel) string {
					var b bytes.Buffer
					w := colorprofile.Writer{Forward: &b, Profile: profile}
					if _, err := w.Write([]byte(m.View().Content)); err != nil {
						t.Fatal(err)
					}
					return stripANSI(b.String())
				}
				out := flush(m)
				requireWorkerCopy(t, out, "memory hold", "Resume/Cancel", "detail: enter")
				if runBandOf(apitypes.RunListItemDTO{RunDTO: r}) != bandNeedsYou {
					t.Fatal("memory hold outside NEEDS YOU")
				}
				m = tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
				m = applyDetail(m, r, nil)
				m.detail.steer.access = steerAllowed
				m.detail.steer.queue = []apitypes.SteerInputDTO{{Kind: "follow_up", CreatedAt: time.Now()}}
				m = step(m, tea.ColorProfileMsg{Profile: profile})
				m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
				out = flush(m)
				requireWorkerCopy(t, out, "memory hold", "owner Resume or Cancel", "2 of 2 memory interventions used", "episode 7",
					"uzi run resume r1", "uzi run cancel r1", "(terminal)", "fresh memory episode", "No automatic resume",
					"Source/archive availability and latest edits unverified", "owner Resume/Cancel")
				for _, line := range strings.Split(out, "\n") {
					if visualWidth(line) > width {
						t.Errorf("overflow: %q", line)
					}
				}
				if strings.Contains(out, "automatic requeues used") || strings.Contains(out, "contains the latest work") {
					t.Errorf("misleading detail: %s", out)
				}
			})
		}
	}
}

func TestMemoryHoldHostileID(t *testing.T) {
	r := memoryHeldRun()
	r.ID = "r1\nINJECT\x1b[31m"
	env := fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}})
	out, errout, code := runCLI(t, env, "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit %d %s", code, errout)
	}
	if strings.Contains(out, "r1\nINJECT") || strings.Contains(out, "\x1b[31m") {
		t.Fatalf("unsafe CLI: %q", out)
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
	m = applyDetail(m, r, nil)
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 35})
	out = stripANSI(m.View().Content)
	if strings.Contains(out, "r1\nINJECT") || strings.Contains(out, "\nINJECT") {
		t.Fatalf("unsafe TUI: %q", out)
	}
}
