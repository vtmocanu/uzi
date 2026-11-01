package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func iterationFixture() apitypes.RunDTO {
	return heldRun("r1", "iteration_budget_exhausted", time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
}

func TestIterationCLIGet(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt", "self_improve", "mr_rework", "ci_fix"} {
		for _, shape := range []string{"none", "zero completed", "completed"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				r := iterationFixture()
				r.Kind = kind
				if shape != "completed" {
					r.MilestonesCompleted = nil
				}
				if shape == "none" {
					r.Milestones = nil
				}
				fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
				out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
				if code != 0 {
					t.Fatalf("exit=%d: %s", code, stderr)
				}
				requireWorkerCopy(t, out, "Iteration limit reached", "uzi run resume r1", "uzi run cancel r1")
				for _, forbidden := range []string{"uzi run stop", "uzi run scope", "uzi run extend", "time limit reached"} {
					if strings.Contains(out, forbidden) {
						t.Fatalf("unexpected %q: %s", forbidden, out)
					}
				}
			})
		}
	}
}

func TestIterationCLIWallGuidance(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, used *int
		extend      bool
	}{
		{"time left", intPtr(3600), intPtr(3599), false},
		{"spent", intPtr(3600), intPtr(3600), true},
		{"over", intPtr(3600), intPtr(3601), true},
		{"unknown total", nil, intPtr(3600), false},
		{"unknown used", intPtr(3600), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := iterationFixture()
			r.BudgetTotalSeconds, r.BudgetUsedSeconds = tc.total, tc.used
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			if strings.Contains(out, "uzi run extend") != tc.extend || strings.Contains(out, "wall time exhausted") != tc.extend {
				t.Fatalf("wrong wall guidance: %s", out)
			}
			if tc.extend {
				requireWorkerCopy(t, out, "Extend adds time only, then Resume")
			}
		})
	}
}

func TestIterationCLIWaitFollow(t *testing.T) {
	orig := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = orig })
	for _, follow := range []bool{false, true} {
		for _, initial := range []bool{false, true} {
			t.Run(map[bool]string{false: "wait", true: "follow"}[follow]+map[bool]string{false: "/transition", true: "/initial"}[initial], func(t *testing.T) {
				held := iterationFixture()
				runs := []apitypes.RunDTO{held, held, runAt("paused"), held, runAt("running"), held, runAt("completed")}
				if !initial {
					runs = append([]apitypes.RunDTO{runAt("running")}, runs...)
				}
				polls := 0
				srv := providerSequenceServer(t, runs, &polls)
				defer srv.Close()
				args := []string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "2s", "--json"}
				if follow {
					args = []string{"run", "logs", "r1", "--follow", "--json"}
				}
				out, stderr, code := runCLI(t, httpEnv(srv), args...)
				if code != 0 || polls != len(runs) {
					t.Fatalf("exit=%d polls=%d: %s", code, polls, stderr)
				}
				requireWorkerCopy(t, stderr, "Iteration limit reached", "uzi run resume r1", "uzi run cancel r1")
				if strings.Count(stderr, "Iteration limit reached") != 3 {
					t.Fatalf("hold episode repeated or missing: %s", stderr)
				}
				if strings.Contains(stderr, "credential") || strings.Contains(stderr, "time limit") {
					t.Fatalf("wrong pause dispatch: %s", stderr)
				}
				if !json.Valid([]byte(out)) {
					t.Fatalf("JSON/NDJSON polluted: %q", out)
				}
			})
		}
	}
}

func TestIterationCLIExtendDoesNotInferResume(t *testing.T) {
	r := iterationFixture()
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}, InputResp: apitypes.RunInputResponse{ExtensionSeconds: intPtr(7200)}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "2h")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	if !strings.HasPrefix(out, "Extended r1 by 2h.") || strings.Contains(out, "resumed") {
		t.Fatalf("inferred resume: %s", out)
	}
}

func TestIterationCLIExistingHoldControls(t *testing.T) {
	for _, tc := range []struct{ reason, label string }{
		{"budget_exhausted", "time limit reached"}, {"completion_blocked", "completion blocked"},
	} {
		r := heldRun("r1", tc.reason, time.Now())
		fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
		out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
		if code != 0 || !strings.Contains(out, tc.label) || strings.Contains(out, "Iteration limit reached") {
			t.Fatalf("old hold changed: %s %s", out, stderr)
		}
	}
	for _, status := range []string{"queued", "running", "completed"} {
		r := iterationFixture()
		r.Status = status
		fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
		out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
		if code != 0 || strings.Contains(out, "Iteration limit reached") {
			t.Fatalf("inactive hold rendered: %s %s", out, stderr)
		}
	}
}
