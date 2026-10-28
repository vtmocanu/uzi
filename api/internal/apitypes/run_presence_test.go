package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoveryTimingPresence(t *testing.T) {
	for _, tc := range []struct {
		name, field    string
		present, timed bool
	}{
		{"omitted", "", false, false},
		{"null", `,"recovery_retry_not_before":null`, true, false},
		{"timed", `,"recovery_retry_not_before":"2099-01-01T00:00:00Z"`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"id":"r1","status":"recovery_wait","recovery_wait_cause":"codex_idle_stall","repo_path":"group/repo","worker_name":"worker","owner_email":"owner@example.com","judge_verdict":"ok","judge_todo_count":2` + tc.field + `}`)
			var full RunDTO
			var list RunListItemDTO
			var summary RunSummaryItemDTO
			for _, target := range []any{&full, &list, &summary} {
				if err := json.Unmarshal(body, target); err != nil {
					t.Fatal(err)
				}
			}
			projected := RunSummaryOf(list)
			for _, run := range []RunDTO{full, list.RunDTO, summary.ListItem().RunDTO, projected.ListItem().RunDTO} {
				if run.RecoveryRetryNotBeforePresent != tc.present || (run.RecoveryRetryNotBefore != nil) != tc.timed {
					t.Fatalf("lost timing presence: present=%v stamp=%v", run.RecoveryRetryNotBeforePresent, run.RecoveryRetryNotBefore)
				}
			}
			if list.RepoPath != "group/repo" || list.WorkerName == nil || *list.WorkerName != "worker" || list.OwnerEmail == nil || *list.OwnerEmail != "owner@example.com" || list.JudgeVerdict == nil || *list.JudgeVerdict != "ok" || list.JudgeTodoCount != 2 {
				t.Fatalf("list context lost: %+v", list)
			}
			for _, target := range []any{full, list, summary} {
				out, err := json.Marshal(target)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(out), "Present") || !strings.Contains(string(out), `"recovery_retry_not_before":`) {
					t.Fatalf("wire changed: %s", out)
				}
			}
		})
	}
	for _, target := range []any{&RunDTO{}, &RunListItemDTO{}, &RunSummaryItemDTO{}} {
		if err := json.Unmarshal([]byte(`{"recovery_retry_not_before":"invalid"}`), target); err == nil {
			t.Fatal("invalid date accepted")
		}
	}
}
