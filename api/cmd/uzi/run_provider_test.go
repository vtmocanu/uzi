package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func providerRetryFixture() apitypes.RunDTO {
	cause := "provider_outage"
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	retry := start.Add(time.Minute)
	return apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "recovery_wait",
		RecoveryWaitCause: &cause, ProviderParkCount: 3, ProviderParkStreakStartedAt: &start, RecoveryRetryNotBefore: &retry}
}

func providerHoldFixture() apitypes.RunDTO {
	r := heldRun("r1", "provider_unavailable", time.Now())
	r.CanStopAtProviderHold = true
	return r
}

// providerSequenceServer uses real HTTP decoding and returns one log message exactly once.
// Tests terminate on a named target; repeated observations have no timing-dependent notices.
func providerSequenceServer(t *testing.T, runs []apitypes.RunDTO, polls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/api/runs/r1":
			i := *polls
			*polls++
			if i >= len(runs) {
				i = len(runs) - 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"run": runs[i]})
		case "/api/runs/r1/messages":
			msgs := []apitypes.MessageDTO{}
			if req.URL.Query().Get("after") == "0" {
				msgs = append(msgs, apitypes.MessageDTO{Seq: 1, Kind: "text", Payload: json.RawMessage(`{"text":"fixture message"}`)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
		default:
			http.NotFound(w, req)
		}
	}))
}

func TestProviderCLIGetRetryAndJSON(t *testing.T) {
	r := providerRetryFixture()
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	requireWorkerCopy(t, out, "PROVIDER", "provider outage", "counted parks: 3", "streak started: 2026-10-10T12:00:00Z", "next retry: 2026-10-10T12:01:00Z")
	out, stderr, code = runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	var got apitypes.RunDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(r)
	var want, actual any
	_ = json.Unmarshal(wantJSON, &want)
	_ = json.Unmarshal([]byte(out), &actual)
	if !reflect.DeepEqual(want, actual) {
		t.Fatalf("JSON changed: %s", out)
	}
}

func TestProviderCLIResumedMarkerIsNotAnOwnerHold(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "running", "completed"} {
		t.Run(status, func(t *testing.T) {
			r := providerHoldFixture()
			r.Status = status
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			if strings.Contains(out, "provider unavailable") || strings.Contains(out, "waiting for the owner") || strings.Contains(out, "uzi run scope") {
				t.Fatalf("retained Resume marker rendered as active owner hold: %s", out)
			}
		})
	}
}

func TestProviderCLINonIssueHoldRetention(t *testing.T) {
	for _, kind := range []string{"task", "prompt"} {
		t.Run(kind, func(t *testing.T) {
			r := providerHoldFixture()
			r.Kind = kind
			r.CanStopAtProviderHold = false
			r.Milestones, r.MilestonesCompleted = nil, nil
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			requireWorkerCopy(t, out, "claim and worker affinity retained", "Stop unavailable: requires a milestone issue run", "uzi run resume r1", "uzi run cancel r1")
			if strings.Contains(out, "issue lock") {
				t.Fatalf("non-issue hold claims an issue lock: %s", out)
			}
		})
	}
}

func TestProviderCLIHoldStopGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*apitypes.RunDTO)
		want   string
		scope  bool
	}{
		{"eligible", func(r *apitypes.RunDTO) {}, "uzi run scope r1 --through 4; then, only if scope succeeds, Resume: uzi run resume r1", true},
		{"server refused", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false }, "server does not permit Stop", false},
		{"no completed", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false; r.MilestonesCompleted = nil }, "no completed milestone", false},
		{"non milestone", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false; r.Milestones = nil }, "requires a milestone issue run", false},
		{"interactive refused", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false; r.Interactive = true }, "server does not permit Stop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := providerHoldFixture()
			tc.change(&r)
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			requireWorkerCopy(t, out, "provider unavailable", "waiting for the owner", "no automatic retry deadline", "issue lock, claim and worker affinity retained", "clone and session files kept on the worker; recovery depends on the worker or a verified recovery copy", "uzi run resume r1", "uzi run cancel r1", tc.want)
			if strings.Contains(out, "uzi run scope") != tc.scope {
				t.Fatalf("Stop gate ignored: %s", out)
			}
		})
	}
}

func TestProviderCLIWaitFollowEdges(t *testing.T) {
	orig := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = orig })
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "follow"}[follow], func(t *testing.T) {
			generic := runAt("recovery_wait")
			retry := providerRetryFixture()
			countUpdate := retry
			countUpdate.ProviderParkCount = 4
			hold := providerHoldFixture()
			ordinaryPause := runAt("paused")
			credential := ordinaryPause
			reason := "credential_disabled"
			credential.HoldReason = &reason
			runs := []apitypes.RunDTO{generic, retry, retry, countUpdate, hold, hold, ordinaryPause, credential, hold, runAt("running"), runAt("failed")}
			polls := 0
			srv := providerSequenceServer(t, runs, &polls)
			defer srv.Close()
			args := []string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "2s", "--json"}
			if follow {
				args = []string{"run", "logs", "r1", "--follow", "--json"}
			}
			out, stderr, code := runCLI(t, httpEnv(srv), args...)
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			requireWorkerCopy(t, stderr, "provider outage", "counted parks: 3", "provider unavailable", "paused; resume with uzi run resume r1", "credential", "running")
			if strings.Contains(stderr, "paused by") {
				t.Fatalf("pause attributed without current message evidence: %s", stderr)
			}
			if strings.Count(stderr, "provider outage") != 1 || strings.Count(stderr, "provider unavailable") != 2 {
				t.Fatalf("edge notices repeated or missing: %s", stderr)
			}
			if polls != len(runs) {
				t.Fatalf("stopped early: polls=%d stderr=%s", polls, stderr)
			}
			if follow {
				var msg apitypes.MessageDTO
				if err := json.Unmarshal([]byte(out), &msg); err != nil {
					t.Fatalf("NDJSON polluted: %q: %v", out, err)
				}
				if msg.Seq != 1 || string(msg.Payload) != `{"text":"fixture message"}` {
					t.Fatalf("message changed: %s", out)
				}
			} else {
				var result apitypes.RunDTO
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatalf("JSON polluted: %s", out)
				}
				if result.Status != "failed" {
					t.Fatalf("did not reach terminal: %s", out)
				}
			}
		})
	}
}

func TestProviderCLIDefaultPollsToEveryTerminal(t *testing.T) {
	for _, terminal := range []string{"completed", "failed", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			polls := 0
			srv := providerSequenceServer(t, []apitypes.RunDTO{providerRetryFixture(), providerHoldFixture(), runAt("running"), runAt(terminal)}, &polls)
			defer srv.Close()
			_, stderr, code := runCLI(t, httpEnv(srv), "run", "wait", "r1", "--interval", "1ms")
			if code != 0 || polls != 4 {
				t.Fatalf("default wait exit=%d polls=%d stderr=%s", code, polls, stderr)
			}
			requireWorkerCopy(t, stderr, "provider outage", "provider unavailable", "paused → running", "running → "+terminal)
		})
	}
}

func TestProviderCLIExactCauseAndMissingMetadata(t *testing.T) {
	r := providerRetryFixture()
	r.ProviderParkStreakStartedAt, r.RecoveryRetryNotBefore = nil, nil
	for _, cause := range []string{"provider_outage", "provider_outage_future", ""} {
		t.Run(cause, func(t *testing.T) {
			r.RecoveryWaitCause = &cause
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			if cause == "provider_outage" {
				requireWorkerCopy(t, out, "streak started: unknown", "next retry: unknown")
			} else if strings.Contains(out, "counted parks:") {
				t.Fatalf("untyped cause rendered as provider outage: %s", out)
			}
		})
	}
}

func TestProviderCLIWaitExplicitTargets(t *testing.T) {
	for _, r := range []apitypes.RunDTO{providerRetryFixture(), providerHoldFixture()} {
		t.Run(r.Status, func(t *testing.T) {
			polls := 0
			srv := providerSequenceServer(t, []apitypes.RunDTO{runAt("running"), r, runAt("completed")}, &polls)
			defer srv.Close()
			_, stderr, code := runCLI(t, httpEnv(srv), "run", "wait", "r1", "--until", r.Status, "--interval", "1ms")
			if code != 0 || polls != 2 {
				t.Fatalf("explicit target exit=%d polls=%d stderr=%s", code, polls, stderr)
			}
		})
	}
}

func TestProviderCLITerminalSafety(t *testing.T) {
	r := providerHoldFixture()
	r.ID = "r1\n\x1b[31m'$(inert)"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	requireWorkerCopy(t, out, "'\\''$(inert)'")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "HOLD ") && strings.ContainsAny(line, "\r\x1b") {
			t.Fatalf("unsafe hold row: %q", line)
		}
	}
	unknown := "future\n\x1b[31mreason"
	changed := r
	changed.HoldReason = &unknown
	polls := 0
	srv := providerSequenceServer(t, []apitypes.RunDTO{r, changed, runAt("completed")}, &polls)
	defer srv.Close()
	_, stderr, code = runCLI(t, httpEnv(srv), "run", "wait", "r1", "--interval", "1ms")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	requireWorkerCopy(t, stderr, "'\\''$(inert)'", "paused (future [31mreason)")
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !strings.HasPrefix(line, "run r1: ") || strings.ContainsAny(line, "\r\x1b") {
			t.Fatalf("unsafe notice: %q", line)
		}
	}
}
