package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func resetArgs() []string {
	return []string{"run", "approve", "r1", "--reset-milestones", "--reason", "replace scope",
		"--expected-gate-revision", "7", "--expected-reset-state-digest", strings.Repeat("a", 64),
		"--expected-completion-revision", "null"}
}

func TestResetApprovalValidationNoEffects(t *testing.T) {
	for _, flag := range []string{"--reason", "--expected-gate-revision", "--expected-reset-state-digest", "--expected-completion-revision"} {
		t.Run("missing "+flag, func(t *testing.T) {
			args := resetArgs()
			for i, arg := range args {
				if arg == flag {
					args = append(args[:i], args[i+2:]...)
					break
				}
			}
			fc := gateFake("awaiting_approval", 7)
			_, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage || !strings.Contains(stderr, flag) {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			if len(fc.RunVerbCalls) != 0 {
				t.Fatalf("invalid reset made calls: %v", fc.RunVerbCalls)
			}
		})
	}
	for _, tc := range []struct{ flag, value string }{
		{"--reason", " \t"}, {"--expected-gate-revision", "0"}, {"--expected-gate-revision", "-1"},
		{"--expected-gate-revision", "oops"}, {"--expected-completion-revision", "0"},
		{"--expected-completion-revision", "-1"}, {"--expected-completion-revision", "NULL"},
		{"--expected-completion-revision", ""}, {"--expected-completion-revision", "1.5"},
		{"--expected-completion-revision", "9999999999999999999999"},
		{"--expected-reset-state-digest", "bad"}, {"--expected-reset-state-digest", strings.Repeat("g", 64)},
	} {
		t.Run(tc.flag+"="+tc.value, func(t *testing.T) {
			args := resetArgs()
			for i, arg := range args {
				if arg == tc.flag {
					args[i+1] = tc.value
				}
			}
			fc := gateFake("awaiting_approval", 7)
			_, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			if len(fc.RunVerbCalls) != 0 {
				t.Fatalf("invalid reset made calls: %v", fc.RunVerbCalls)
			}
		})
	}
	for _, flag := range []string{"--reason", "--expected-reset-state-digest", "--expected-completion-revision"} {
		fc := gateFake("awaiting_approval", 7)
		_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", flag, "x")
		if code != uzicli.ExitUsage || len(fc.RunVerbCalls) != 0 {
			t.Fatalf("orphan %s code=%d calls=%v", flag, code, fc.RunVerbCalls)
		}
	}
	for _, token := range []string{"auto", "", "prod-key"} {
		fc := gateFake("awaiting_approval", 7)
		_, stderr, code := runCLI(t, fakeEnv(fc), append(resetArgs(), "--token", token)...)
		if code != uzicli.ExitUsage || !strings.Contains(stderr, "--token") || len(fc.RunVerbCalls) != 0 || fc.LastSetTokenRunID != "" {
			t.Fatalf("reset+token code=%d calls=%v stderr=%s", code, fc.RunVerbCalls, stderr)
		}
	}
}

func TestResetApprovalRealHTTP(t *testing.T) {
	for _, revision := range []string{"null", "3"} {
		t.Run(revision, func(t *testing.T) {
			var requests []string
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"server_side":true}`)
			}))
			defer srv.Close()
			args := resetArgs()
			args[len(args)-1] = revision
			args = append(args, "--agent-source", "repo", "--exclude-agents", "tester", "--json")
			out, stderr, code := runCLI(t, fakeEnv(uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: "test"})), args...)
			if code != 0 || !strings.Contains(out, `"server_side": true`) {
				t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
			}
			if !reflect.DeepEqual(requests, []string{"POST /api/runs/r1/inputs"}) {
				t.Fatalf("requests=%v; reset must not fetch bindings", requests)
			}
			expectedRevision := any(nil)
			if revision == "3" {
				expectedRevision = float64(3)
			}
			want := map[string]any{
				"kind": "approve_plan", "body": "", "override_capabilities": false,
				"selection":              map[string]any{"source": "repo", "exclusions": []any{"tester"}},
				"expected_gate_revision": float64(7), "expected_reset_state_digest": strings.Repeat("a", 64),
				"reconciliation": map[string]any{"mode": "reset", "reason": "replace scope", "expected_completion_revision": expectedRevision},
			}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("body=%#v want=%#v", body, want)
			}
		})
	}
}

func TestResetApprovalRefusalsNeverRetry(t *testing.T) {
	for _, reason := range []string{uzicli.ReasonMilestoneReconciliationRequired, uzicli.ReasonMilestoneReconciliationConflict, uzicli.ReasonMilestoneReconciliationNotReady} {
		fc := gateFake("awaiting_approval", 7)
		fc.SubmitRunInputErr = &uzicli.ExitError{Code: uzicli.ExitConflict, Reason: reason, Err: errors.New("refused")}
		_, _, code := runCLI(t, fakeEnv(fc), resetArgs()...)
		if code != uzicli.ExitConflict || !reflect.DeepEqual(fc.RunVerbCalls, []string{"submit_run_input"}) {
			t.Fatalf("%s code=%d calls=%v", reason, code, fc.RunVerbCalls)
		}
	}
}

func TestReconciliationReviewExactBindings(t *testing.T) {
	revision := 3
	for _, tc := range []struct {
		name     string
		revision *int
		want     string
	}{
		{"legacy", nil, "null"},
		{"structural", &revision, "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest := strings.Repeat("b", 64)
			r := apitypes.RunDTO{ID: "r1", ResetStateDigest: digest, GateRevision: 7, CompletionRevision: tc.revision}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			for label, value := range map[string]string{
				"GATE_REVISION":       "7",
				"COMPLETION_REVISION": tc.want,
				"RESET_STATE_DIGEST":  digest,
			} {
				found := false
				for _, line := range strings.Split(out, "\n") {
					if strings.Join(strings.Fields(line), " ") == label+" "+value {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing exact binding %s=%s in %s", label, value, out)
				}
			}
		})
	}
}

func TestReconciliationGetPagingAndSafeOutput(t *testing.T) {
	hostile := "evil\x1b[2J\u202etext"
	cursor := int64(9)
	r := apitypes.RunDTO{ID: "r1", ResetStateDigest: strings.Repeat("b", 64), GateRevision: 10,
		Milestones: []apitypes.Milestone{{ID: "old", Title: hostile}}, MilestonesCandidate: []apitypes.Milestone{},
		MilestonesCompleted: []string{"old"}, MilestonesInProgress: []string{"active"},
		MilestoneReconciliationsNextBeforeGateRevision: &cursor}
	audit, _ := json.Marshal(map[string]any{"gate_revision": 10, "created_at": "2026-10-15", "reason": hostile,
		"reason_truncated": true, "details_omitted": true,
		"before_state": map[string]any{"milestones_frozen": []any{map[string]string{"id": "old", "title": hostile}}, "scope_ceiling": 2},
		"after_state":  map[string]any{"milestones_frozen": []any{}, "milestones_completed": []any{}, "scope_ceiling": nil}})
	r.MilestoneReconciliations = []json.RawMessage{audit}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--reconciliation-before-gate-revision", "11")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if fc.LastRunReadOptions.ReconciliationBeforeGateRevision == nil || *fc.LastRunReadOptions.ReconciliationBeforeGateRevision != 11 {
		t.Fatal("cursor not forwarded")
	}
	for _, want := range []string{"FROZEN_MILESTONES", "old", "evil", "CANDIDATE_MILESTONES", "[] (empty)", "MILESTONES_COMPLETED", "active", "COMPLETION_ACCEPTED", "COMPLETION_DEFERRED", "COMPLETION_REVISION", "null", "RESET_STATE_DIGEST", "scope ceiling", "prior decisions/waivers", "prior permits", "reason_truncated", "details_omitted", "BEFORE", "AFTER", "2026-10-15", "--reconciliation-before-gate-revision 9"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
		t.Fatalf("unsafe output: %q", out)
	}
	jsonOut, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json", "--reconciliation-before-gate-revision", "11")
	var got apitypes.RunDTO
	if code != 0 || json.Unmarshal([]byte(jsonOut), &got) != nil || got.Milestones[0].Title != hostile {
		t.Fatalf("JSON altered: %s", jsonOut)
	}
	for _, value := range []string{"0", "-1", "no"} {
		fresh := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fresh), "run", "get", "r1", "--reconciliation-before-gate-revision", value)
		if code != uzicli.ExitUsage || len(fresh.RunVerbCalls) != 0 {
			t.Fatalf("cursor %s code=%d calls=%v", value, code, fresh.RunVerbCalls)
		}
	}
}
