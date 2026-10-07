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
	"github.com/vtmocanu/uzi/api/internal/uzicli"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func workerExhaustionRun(limit int32) apitypes.RunDTO {
	cause := "worker_requeue_exhausted"
	retry := time.Now().Add(-time.Hour)
	return apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "recovery_wait", RecoveryWaitCause: &cause, RecoveryRetryNotBefore: &retry, RequeueCount: 9,
		WorkerRecovery: &apitypes.WorkerRecoveryDTO{Episode: 4, AutomaticRequeueLimit: int(limit), EpisodeUsed: limit, EpisodeRemaining: 0}}
}

func workerExhaustionServer(t *testing.T, runs []apitypes.RunDTO, polls *int, resume *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/r1/resume-now":
			*resume = true
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != 0 {
				t.Errorf("resume payload %q: %v", body, err)
			}
			result := workerExhaustionRun(0)
			result.Status = "queued"
			result.WorkerRecovery.Episode = 5
			_ = json.NewEncoder(w).Encode(map[string]any{"run": result})
		case r.URL.Path == "/api/runs/r1":
			i := *polls
			*polls++
			if i >= len(runs) {
				i = len(runs) - 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"run": runs[i]})
		case r.URL.Path == "/api/runs/r1/inputs":
			_ = json.NewEncoder(w).Encode(map[string]any{"inputs": []apitypes.SteerInputDTO{{Kind: "follow_up", CreatedAt: time.Now()}}})
		case r.URL.Path == "/api/runs":
			items := []apitypes.RunListItemDTO{{RunDTO: runs[0]}}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": items})
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"messages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func requireWorkerCopy(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in:\n%s", want, output)
		}
	}
}

func TestWorkerExhaustionCLIGetEvidence(t *testing.T) {
	for _, limit := range []int32{0, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			r := workerExhaustionRun(limit)
			latest := true
			r.CheckpointContainsLatest = &latest
			tip := "abc123"
			r.WorkerRecovery.Evidence = &apitypes.WorkerRecoveryEvidenceDTO{CheckpointTip: &tip, AvailableCapture: true, PublicationUncertain: true, CaptureUncertain: true, CustodyUncertain: true, Unknown: true, RecordedAt: time.Date(2026, 10, 7, 1, 2, 3, 0, time.UTC)}
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
			defer srv.Close()
			out, errout, code := runCLI(t, httpEnv(srv), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			requireWorkerCopy(t, out, "worker recovery exhausted", fmt.Sprintf("%d of %d automatic requeues used", limit, limit), "lifetime charged count 9", "uzi run resume r1", "uzi run cancel r1",
				"The server recorded checkpoint abc123 before this hold. Current availability and the latest local edits are not verified.",
				"A recovery capture was recorded as available at 2026-10-07T01:02:03Z. It may later expire or be discarded; it may not contain the latest local edits.",
				"publication pending or uncertain", "capture pending or uncertain", "retained source custody uncertain", "evidence unavailable or unknown")
			if strings.Contains(out, "contains the latest work") || strings.Contains(out, "retry at") || strings.Contains(out, "resumes on its own") {
				t.Errorf("automatic retry promise: %s", out)
			}
		})
	}
}

func TestWorkerExhaustionCLIMissingDTOAndList(t *testing.T) {
	r := workerExhaustionRun(0)
	r.WorkerRecovery = nil
	polls, resume := 0, false
	srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
	defer srv.Close()
	for _, verb := range []string{"get", "list"} {
		args := []string{"run", verb}
		if verb == "get" {
			args = append(args, "r1")
		}
		out, errout, code := runCLI(t, httpEnv(srv), args...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errout)
		}
		requireWorkerCopy(t, out, "worker recovery exhausted", "automatic requeue allowance unknown", "lifetime charged count 9")
		if strings.Contains(out, "0 of 1") {
			t.Errorf("fabricated allowance: %s", out)
		}
	}
}

func TestWorkerExhaustionCLIJSONAndResume(t *testing.T) {
	r := workerExhaustionRun(3)
	tip := "abc123"
	r.WorkerRecovery.Evidence = &apitypes.WorkerRecoveryEvidenceDTO{CheckpointTip: &tip, AvailableCapture: true, PublicationUncertain: true, CaptureUncertain: true, CustodyUncertain: true, Unknown: true, RecordedAt: time.Date(2026, 10, 7, 1, 2, 3, 0, time.UTC)}
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
	var recovery map[string]any
	if err := json.Unmarshal(got["worker_recovery"], &recovery); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"episode": 4, "automatic_requeue_limit": 3, "episode_used": 3, "episode_remaining": 0} {
		if recovery[key] != want {
			t.Errorf("%s=%v want %v", key, recovery[key], want)
		}
	}
	evidence, ok := recovery["evidence"].(map[string]any)
	if !ok {
		t.Fatal("missing typed evidence")
	}
	for key, want := range map[string]any{"checkpoint_tip": "abc123", "available_capture": true, "publication_uncertain": true, "capture_uncertain": true, "custody_uncertain": true, "unknown": true, "recorded_at": "2026-10-07T01:02:03Z"} {
		if evidence[key] != want {
			t.Errorf("evidence.%s=%v want %v", key, evidence[key], want)
		}
	}
	out, errout, code = runCLI(t, httpEnv(srv), "run", "resume", "r1")
	if code != 0 || !resume {
		t.Fatalf("resume exit %d POST=%v: %s", code, resume, errout)
	}
	requireWorkerCopy(t, out, "fresh worker recovery episode", "automatic requeue allowance 0", "lifetime charged count 9", "Zero allowance permits this explicit attempt; no automatic requeues.")
	help, _, _ := runCLI(t, httpEnv(srv), "run", "resume", "--help")
	requireWorkerCopy(t, help, "worker recovery exhausted", "zero", "Cancel")
}

func TestWorkerExhaustionCLIWait(t *testing.T) {
	for _, tc := range []struct {
		name, cause string
		until       []string
		wantPolls   int
	}{
		{"default", "worker_requeue_exhausted", nil, 1},
		{"explicit defaults", "worker_requeue_exhausted", []string{"--until", strings.Join(defaultWaitStates, ",")}, 2},
		{"explicit recovery", "worker_requeue_exhausted", []string{"--until", "recovery_wait"}, 1},
		{"ordinary transient", "empty_turn", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := workerExhaustionRun(0)
			r.RecoveryWaitCause = &tc.cause
			done := r
			done.Status = "completed"
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r, done}, &polls, &resume)
			defer srv.Close()
			args := append([]string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "1s", "--min-plan-seq", "99"}, tc.until...)
			_, errout, code := runCLI(t, httpEnv(srv), args...)
			if code != 0 || polls != tc.wantPolls {
				t.Errorf("exit %d polls %d want %d: %s", code, polls, tc.wantPolls, errout)
			}
			if tc.cause == "worker_requeue_exhausted" {
				requireWorkerCopy(t, errout, "uzi run resume r1", "uzi run cancel r1")
			}
		})
	}
}

func TestWorkerExhaustionCLIFollowCauseChange(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	exhausted := workerExhaustionRun(3)
	transient := exhausted
	cause := "empty_turn"
	transient.RecoveryWaitCause = &cause
	done := exhausted
	done.Status = "completed"
	polls, resume := 0, false
	srv := workerExhaustionServer(t, []apitypes.RunDTO{transient, exhausted, exhausted, done}, &polls, &resume)
	defer srv.Close()
	_, errout, code := runCLI(t, httpEnv(srv), "run", "logs", "r1", "--follow")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout)
	}
	requireWorkerCopy(t, errout, "worker recovery exhausted", "uzi run resume r1", "uzi run cancel r1")
	if strings.Count(errout, "worker recovery exhausted") != 1 {
		t.Errorf("notice not once per cause change: %s", errout)
	}
	for _, line := range strings.Split(errout, "\n") {
		if strings.Contains(line, "worker recovery exhausted") && strings.Contains(line, "resumes on its own") {
			t.Error(line)
		}
	}
}

func TestWorkerExhaustionCLIEvidenceFlags(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		e          *apitypes.WorkerRecoveryEvidenceDTO
	}{
		{"missing", "evidence unavailable or unknown", nil},
		{"empty", "evidence unavailable or unknown", &apitypes.WorkerRecoveryEvidenceDTO{}},
		{"publication", "publication pending or uncertain", &apitypes.WorkerRecoveryEvidenceDTO{PublicationUncertain: true}},
		{"capture", "capture pending or uncertain", &apitypes.WorkerRecoveryEvidenceDTO{CaptureUncertain: true}},
		{"custody", "retained source custody uncertain", &apitypes.WorkerRecoveryEvidenceDTO{CustodyUncertain: true}},
		{"unknown", "evidence unavailable or unknown", &apitypes.WorkerRecoveryEvidenceDTO{Unknown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := workerExhaustionRun(3)
			r.WorkerRecovery.Evidence = tc.e
			out, errout, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			requireWorkerCopy(t, strings.ToLower(out), tc.want)
			for _, other := range []string{"publication pending or uncertain", "capture pending or uncertain", "retained source custody uncertain"} {
				if other != tc.want && strings.Contains(out, other) {
					t.Errorf("invented evidence: %s", out)
				}
			}
			if strings.Contains(out, "uzi run export") {
				t.Errorf("export promise: %s", out)
			}
		})
	}
}

func TestWorkerExhaustionCLIExplicitWaitTimeout(t *testing.T) {
	r := workerExhaustionRun(0)
	polls, resume := 0, false
	srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
	defer srv.Close()
	for _, until := range []string{"completed", ""} {
		_, errout, code := runCLI(t, httpEnv(srv), "run", "wait", "r1", "--until", until, "--interval", "1ms", "--timeout", "10ms")
		if code != uzicli.ExitTimeout {
			t.Errorf("until %q exit %d: %s", until, code, errout)
		}
	}
}

func TestWorkerExhaustionCLITUIViews(t *testing.T) {
	for _, width := range []int{60, 80, 200} {
		r := workerExhaustionRun(0)
		r.WorkerRecovery.Evidence = &apitypes.WorkerRecoveryEvidenceDTO{PublicationUncertain: true}
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.width, m.height = width, 30
		m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{{RunDTO: r}}})
		out := stripANSI(m.View().Content)
		requireWorkerCopy(t, out, "Resume/Cancel", "0/0 auto used", "lifetime charged 9")
		if runBandOf(apitypes.RunListItemDTO{RunDTO: r}) != bandNeedsYou {
			t.Error("not in NEEDS YOU")
		}
		m = tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m = applyDetail(m, r, nil)
		m.detail.steer.access = steerAllowed
		m.detail.steer.queue = []apitypes.SteerInputDTO{{Kind: "follow_up", CreatedAt: time.Now()}}
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
		out = stripANSI(m.View().Content)
		requireWorkerCopy(t, out, "owner Resume/Cancel", "owner Resume or Cancel", "0 of 0 automatic requeues used", "lifetime charged count 9", "uzi run resume r1", "uzi run cancel r1", "publication pending or uncertain", "availability/latest edits unverified")
		if len(strings.Split(out, "\n")) != m.height {
			t.Errorf("width %d frame height: %d want %d", width, len(strings.Split(out, "\n")), m.height)
		}
		for _, line := range strings.Split(out, "\n") {
			if visualWidth(line) > width {
				t.Errorf("width %d overflow: %q", width, line)
			}
		}
	}
}

func TestWorkerExhaustionCLIInputs(t *testing.T) {
	for _, limit := range []int32{0, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			r := workerExhaustionRun(limit)
			polls, resume := 0, false
			srv := workerExhaustionServer(t, []apitypes.RunDTO{r}, &polls, &resume)
			defer srv.Close()
			out, errout, code := runCLI(t, httpEnv(srv), "run", "inputs", "r1")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errout)
			}
			requireWorkerCopy(t, out, "queued (owner Resume/Cancel)")
			requireWorkerCopy(t, errout, "worker recovery exhausted", fmt.Sprintf("%d of %d automatic requeues used", limit, limit), "lifetime charged count 9", "uzi run resume r1", "uzi run cancel r1")
		})
	}
}

func TestWorkerExhaustionCLITUILabels(t *testing.T) {
	glyph, word := stateGlyphWord("recovery_wait", "stalled", false, false, "", "worker_requeue_exhausted")
	if glyph == "" || word != "requeue limit" {
		t.Errorf("glyph %q word %q", glyph, word)
	}
	got := steerState(apitypes.SteerInputDTO{}, "recovery_wait", "worker_requeue_exhausted")
	requireWorkerCopy(t, got, "queued", "owner Resume/Cancel")
}
