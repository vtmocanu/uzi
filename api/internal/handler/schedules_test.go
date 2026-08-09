package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// fixedNow is the reference instant the pure validator is exercised against; run_at
// cases sit relative to it so the tests are deterministic (no wall clock).
var fixedNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }

func TestValidateScheduleConfig(t *testing.T) {
	future := fixedNow.Add(24 * time.Hour)
	past := fixedNow.Add(-1 * time.Hour)

	cases := []struct {
		name       string
		req        apitypes.ScheduleRequest
		wantStatus int
	}{
		{
			name:       "recurring issue ok",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "recurring", CronExpr: "0 2 * * *", Timezone: "Europe/Bucharest"},
			wantStatus: 0,
		},
		{
			name:       "once issue ok (empty tz defaults to UTC)",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "once", RunAt: &future},
			wantStatus: 0,
		},
		{
			name:       "sweep with labels ok",
			req:        apitypes.ScheduleRequest{Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: 0,
		},
		{
			name:       "sweep empty labels ok",
			req:        apitypes.ScheduleRequest{Target: "sweep", Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: 0,
		},
		{
			name:       "prompt ok",
			req:        apitypes.ScheduleRequest{Target: "prompt", Prompt: "find flaky tests", Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: 0,
		},
		{
			name:       "bad target",
			req:        apitypes.ScheduleRequest{Target: "nope", Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "bad timing",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(1), Timing: "hourly", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "issue missing iid",
			req:        apitypes.ScheduleRequest{Target: "issue", Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "issue non-positive iid",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(0), Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "recurring bad cron",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(1), Timing: "recurring", CronExpr: "not a cron"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "recurring bad tz",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(1), Timing: "recurring", CronExpr: "0 2 * * *", Timezone: "Mars/Phobos"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "once missing run_at",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(1), Timing: "once"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "once past run_at rejected",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(1), Timing: "once", RunAt: &past},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "prompt missing text",
			req:        apitypes.ScheduleRequest{Target: "prompt", Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "prompt oversize",
			req:        apitypes.ScheduleRequest{Target: "prompt", Prompt: strings.Repeat("x", workersvc.MaxIssueDescriptionBytes+1), Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusUnprocessableEntity,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, status, msg := validateScheduleConfig(tc.req, fixedNow)
			if status != tc.wantStatus {
				t.Fatalf("status = %d (%q), want %d", status, msg, tc.wantStatus)
			}
		})
	}
}

// TestValidateScheduleConfigNormalizes proves the normalizer defaults a blank timezone
// to UTC and strips blank sweep labels.
func TestValidateScheduleConfigNormalizes(t *testing.T) {
	n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "sweep", Labels: []string{"  ", "bug", ""}, Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow)
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
	if n.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", n.Timezone)
	}
	if len(n.Labels) != 1 || n.Labels[0] != "bug" {
		t.Fatalf("labels = %v, want [bug]", n.Labels)
	}
}

// TestOnlyEnabled pins the "PATCH carries only enabled" detection the handler uses to
// skip the config UPDATE.
// TestApplyCreateDefaults pins the create-time tri-state defaults. PRD #274 Decision 1a
// flips wait_on_limit ON (auto_approve and enabled stay ON); a present pointer — even to
// false — is always respected so an explicit opt-out survives.
func TestApplyCreateDefaults(t *testing.T) {
	// All omitted: every flag defaults on.
	req := apitypes.ScheduleRequest{}
	applyCreateDefaults(&req)
	if req.AutoApprove == nil || !*req.AutoApprove {
		t.Fatalf("auto_approve default = %v, want true", req.AutoApprove)
	}
	if req.WaitOnLimit == nil || !*req.WaitOnLimit {
		t.Fatalf("wait_on_limit default = %v, want true (PRD #274 Decision 1a)", req.WaitOnLimit)
	}
	if req.Enabled == nil || !*req.Enabled {
		t.Fatalf("enabled default = %v, want true", req.Enabled)
	}

	// Explicit false is preserved, not overwritten by the new ON default.
	no := false
	req = apitypes.ScheduleRequest{WaitOnLimit: &no, AutoApprove: &no}
	applyCreateDefaults(&req)
	if req.WaitOnLimit == nil || *req.WaitOnLimit {
		t.Fatalf("explicit wait_on_limit=false must be respected, got %v", req.WaitOnLimit)
	}
	if req.AutoApprove == nil || *req.AutoApprove {
		t.Fatalf("explicit auto_approve=false must be respected, got %v", req.AutoApprove)
	}
}

func TestOnlyEnabled(t *testing.T) {
	yes := true
	if !onlyEnabled(apitypes.ScheduleRequest{Enabled: &yes}) {
		t.Fatalf("a bare enabled patch should be onlyEnabled")
	}
	if onlyEnabled(apitypes.ScheduleRequest{Enabled: &yes, Target: "issue"}) {
		t.Fatalf("enabled + a config field is NOT onlyEnabled")
	}
	if onlyEnabled(apitypes.ScheduleRequest{}) {
		t.Fatalf("a patch with no enabled is not onlyEnabled")
	}
}
