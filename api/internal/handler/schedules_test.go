package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// fixedNow is the reference instant the pure validator is exercised against; run_at
// cases sit relative to it so the tests are deterministic (no wall clock).
var fixedNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }

func iptr(v int) *int { return &v }

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
		{
			name:       "sweep with max_issues ok",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(5), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: 0,
		},
		{
			name:       "sweep max_issues zero rejected",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(0), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "sweep max_issues negative rejected",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(-3), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "sweep max_issues at ceiling ok",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(MaxSweepIssues), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: 0,
		},
		{
			name:       "sweep max_issues above ceiling rejected (int32-wrap guard)",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(MaxSweepIssues + 1), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "sweep max_issues past int32 rejected (no negative-LIMIT wrap)",
			req:        apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(2147483648), Timing: "recurring", CronExpr: "0 9 * * 1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "issue with max_issues rejected (sweep-only)",
			req:        apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), MaxIssues: iptr(5), Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "prompt with max_issues rejected (sweep-only)",
			req:        apitypes.ScheduleRequest{Target: "prompt", Prompt: "x", MaxIssues: iptr(5), Timing: "recurring", CronExpr: "0 2 * * *"},
			wantStatus: http.StatusBadRequest,
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

// TestApplyCreateDefaultsMaxIssues pins PRD #274 M2: a new SWEEP with no explicit
// max_issues defaults to the bounded 10; an explicit value survives; and issue/prompt
// targets are left nil (max_issues is sweep-only).
func TestApplyCreateDefaultsMaxIssues(t *testing.T) {
	// New sweep, no explicit value → default 10.
	sweep := apitypes.ScheduleRequest{Target: "sweep"}
	applyCreateDefaults(&sweep)
	if sweep.MaxIssues == nil || *sweep.MaxIssues != 10 {
		t.Fatalf("new sweep max_issues default = %v, want 10", sweep.MaxIssues)
	}

	// Explicit value on a sweep is respected.
	explicit := apitypes.ScheduleRequest{Target: "sweep", MaxIssues: iptr(3)}
	applyCreateDefaults(&explicit)
	if explicit.MaxIssues == nil || *explicit.MaxIssues != 3 {
		t.Fatalf("explicit sweep max_issues = %v, want 3", explicit.MaxIssues)
	}

	// Issue and prompt targets never get a default cap.
	for _, target := range []string{"issue", "prompt"} {
		req := apitypes.ScheduleRequest{Target: target}
		applyCreateDefaults(&req)
		if req.MaxIssues != nil {
			t.Fatalf("%s target max_issues = %v, want nil (sweep-only)", target, req.MaxIssues)
		}
	}
}

// TestMergeScheduleMaxIssuesClears pins PRD #274 M2's deliberate replace-semantics for
// max_issues: a config PATCH with MaxIssues=nil CLEARS the cap to unlimited rather than
// keeping the current value (unlike the keep-on-empty fields). Without this, clearing a
// sweep back to unlimited would be impossible once a cap is set.
func TestMergeScheduleMaxIssuesClears(t *testing.T) {
	cur := store.RunSchedule{
		Target:    "sweep",
		Timing:    "recurring",
		CronExpr:  pgtype.Text{String: "0 9 * * 1", Valid: true},
		Timezone:  "UTC",
		MaxIssues: pgtype.Int4{Int32: 7, Valid: true},
	}

	// A config PATCH that omits max_issues (nil) must CLEAR it — replace, not keep.
	cleared := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "sweep", Timing: "recurring", CronExpr: "0 9 * * 1"})
	if cleared.MaxIssues != nil {
		t.Fatalf("merged max_issues = %v, want nil (a config PATCH replaces the whole row; nil clears to unlimited)", cleared.MaxIssues)
	}

	// A config PATCH that sets max_issues takes the request value.
	set := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "sweep", Timing: "recurring", CronExpr: "0 9 * * 1", MaxIssues: iptr(2)})
	if set.MaxIssues == nil || *set.MaxIssues != 2 {
		t.Fatalf("merged max_issues = %v, want 2", set.MaxIssues)
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
	// enabled + model is a config PATCH, not enabled-only: it must NOT short-circuit, or
	// the model would be silently dropped (PRD #300 M2).
	if onlyEnabled(apitypes.ScheduleRequest{Enabled: &yes, Model: sptr("fable")}) {
		t.Fatalf("enabled + model is NOT onlyEnabled (else the model is dropped)")
	}
	if onlyEnabled(apitypes.ScheduleRequest{}) {
		t.Fatalf("a patch with no enabled is not onlyEnabled")
	}
}

// sptr returns a *string, the tri-state carrier for the guidance field.
func sptr(v string) *string { return &v }

// TestValidateScheduleConfigGuidance pins PRD #274 M3 guidance validation: oversize is a
// 422; guidance on the prompt target is a 400 (out of scope — a prompt carries its own
// text); a blank/whitespace value normalizes to nil (a cleared textarea clears rather than
// stores whitespace); and a real value on issue/sweep is accepted and preserved.
func TestValidateScheduleConfigGuidance(t *testing.T) {
	// Oversize guidance → 422.
	big := strings.Repeat("g", MaxGuidanceBytes+1)
	if _, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "issue", IssueIID: i64(7), Guidance: sptr(big), Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow); status != http.StatusUnprocessableEntity {
		t.Fatalf("oversize guidance status = %d, want 422", status)
	}

	// Guidance at exactly the cap on a sweep is fine.
	atCap := strings.Repeat("g", MaxGuidanceBytes)
	if _, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "sweep", Guidance: sptr(atCap), Timing: "recurring", CronExpr: "0 9 * * 1",
	}, fixedNow); status != 0 {
		t.Fatalf("at-cap guidance status = %d, want 0", status)
	}

	// Guidance on the prompt target → 400.
	if _, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "prompt", Prompt: "do the thing", Guidance: sptr("steer me"), Timing: "recurring", CronExpr: "0 9 * * 1",
	}, fixedNow); status != http.StatusBadRequest {
		t.Fatalf("guidance-on-prompt status = %d, want 400", status)
	}

	// A blank/whitespace guidance on prompt is NOT a rejection — it normalizes to nil first.
	if n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "prompt", Prompt: "do the thing", Guidance: sptr("   \n\t "), Timing: "recurring", CronExpr: "0 9 * * 1",
	}, fixedNow); status != 0 || n.Guidance != nil {
		t.Fatalf("blank guidance on prompt: status=%d guidance=%v, want status 0 and nil guidance", status, n.Guidance)
	}

	// A real value on issue is accepted and preserved.
	if n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "issue", IssueIID: i64(7), Guidance: sptr("keep the diff small"), Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow); status != 0 || n.Guidance == nil || *n.Guidance != "keep the diff small" {
		t.Fatalf("valid issue guidance: status=%d guidance=%v, want status 0 and preserved value", status, n.Guidance)
	}
}

// TestMergeScheduleGuidanceClears pins PRD #274 M3's replace-semantics for guidance,
// mirroring max_issues: a config PATCH with Guidance=nil CLEARS stored guidance rather than
// keeping the current value, so a cleared textarea reaches the DB as NULL.
func TestMergeScheduleGuidanceClears(t *testing.T) {
	cur := store.RunSchedule{
		Target:   "issue",
		IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		Timing:   "recurring",
		CronExpr: pgtype.Text{String: "0 2 * * *", Valid: true},
		Timezone: "UTC",
		Guidance: pgtype.Text{String: "old guidance", Valid: true},
	}

	// A config PATCH that omits guidance (nil) must CLEAR it.
	cleared := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "recurring", CronExpr: "0 2 * * *"})
	if cleared.Guidance != nil {
		t.Fatalf("merged guidance = %v, want nil (a config PATCH replaces the whole row; nil clears)", cleared.Guidance)
	}

	// A config PATCH that sets guidance takes the request value.
	set := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "recurring", CronExpr: "0 2 * * *", Guidance: sptr("new guidance")})
	if set.Guidance == nil || *set.Guidance != "new guidance" {
		t.Fatalf("merged guidance = %v, want \"new guidance\"", set.Guidance)
	}
}

// TestGuidanceColumn pins the store-column mapping: a non-blank value on issue/sweep is
// Valid; a nil, blank, or non-issue/sweep target yields SQL NULL.
func TestGuidanceColumn(t *testing.T) {
	if c := guidanceColumn(apitypes.ScheduleRequest{Target: "issue", Guidance: sptr("go")}); !c.Valid || c.String != "go" {
		t.Fatalf("issue guidance column = %+v, want Valid \"go\"", c)
	}
	if c := guidanceColumn(apitypes.ScheduleRequest{Target: "sweep", Guidance: sptr("go")}); !c.Valid {
		t.Fatalf("sweep guidance column should be Valid, got %+v", c)
	}
	if c := guidanceColumn(apitypes.ScheduleRequest{Target: "issue", Guidance: nil}); c.Valid {
		t.Fatalf("nil guidance must be SQL NULL, got %+v", c)
	}
	if c := guidanceColumn(apitypes.ScheduleRequest{Target: "issue", Guidance: sptr("  ")}); c.Valid {
		t.Fatalf("blank guidance must be SQL NULL, got %+v", c)
	}
	if c := guidanceColumn(apitypes.ScheduleRequest{Target: "prompt", Guidance: sptr("go")}); c.Valid {
		t.Fatalf("prompt target must never persist guidance, got %+v", c)
	}
}

// TestScheduleDTOGuidance pins that scheduleDTO round-trips a stored guidance value into
// the DTO pointer, and maps a NULL/empty stored value to nil.
func TestScheduleDTOGuidance(t *testing.T) {
	h := &Handler{}
	base := store.RunSchedule{
		Target:   "issue",
		IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		Timing:   "once",
		Timezone: "UTC",
	}

	base.Guidance = pgtype.Text{String: "always add a failing test first", Valid: true}
	if dto := h.scheduleDTO(base, ""); dto.Guidance == nil || *dto.Guidance != "always add a failing test first" {
		t.Fatalf("DTO guidance = %v, want the stored value", dto.Guidance)
	}

	base.Guidance = pgtype.Text{}
	if dto := h.scheduleDTO(base, ""); dto.Guidance != nil {
		t.Fatalf("NULL guidance must map to nil, got %v", dto.Guidance)
	}

	base.Guidance = pgtype.Text{String: "", Valid: true}
	if dto := h.scheduleDTO(base, ""); dto.Guidance != nil {
		t.Fatalf("empty guidance must map to nil, got %v", dto.Guidance)
	}
}

// TestValidateScheduleConfigModel pins PRD #300 M2 model validation: it uses the shared
// agenttmpl.ValidateModel gate, applies to EVERY target (unlike guidance, which is rejected
// on prompt), a malformed token is a 400 with a "model:" message, and a blank/whitespace
// value normalizes to nil (inherit).
func TestValidateScheduleConfigModel(t *testing.T) {
	// A valid alias is accepted and normalized (trimmed) on issue.
	if n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "issue", IssueIID: i64(7), Model: sptr("  fable  "), Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow); status != 0 || n.Model == nil || *n.Model != "fable" {
		t.Fatalf("valid alias: status=%d model=%v, want status 0 and normalized \"fable\"", status, n.Model)
	}

	// A valid custom ID (a full model identifier) is accepted.
	if n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "sweep", Model: sptr("us.anthropic.claude-opus-4-1-20250805-v1:0"), Timing: "recurring", CronExpr: "0 9 * * 1",
	}, fixedNow); status != 0 || n.Model == nil || *n.Model != "us.anthropic.claude-opus-4-1-20250805-v1:0" {
		t.Fatalf("valid custom ID: status=%d model=%v, want status 0 and preserved value", status, n.Model)
	}

	// Model applies to EVERY target — prompt/sweep/issue all accept it (NOT target-scoped
	// like guidance, which is rejected on prompt).
	for _, tc := range []struct {
		name string
		req  apitypes.ScheduleRequest
	}{
		{"prompt", apitypes.ScheduleRequest{Target: "prompt", Prompt: "do the thing", Model: sptr("fable"), Timing: "recurring", CronExpr: "0 9 * * 1"}},
		{"sweep", apitypes.ScheduleRequest{Target: "sweep", Model: sptr("fable"), Timing: "recurring", CronExpr: "0 9 * * 1"}},
		{"issue", apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Model: sptr("fable"), Timing: "recurring", CronExpr: "0 2 * * *"}},
	} {
		if n, status, _ := validateScheduleConfig(tc.req, fixedNow); status != 0 || n.Model == nil || *n.Model != "fable" {
			t.Fatalf("model on %s target: status=%d model=%v, want status 0 and \"fable\" (model is not target-scoped)", tc.name, status, n.Model)
		}
	}

	// A malformed token (interior whitespace) → 400 with a "model:" message.
	if _, status, msg := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "issue", IssueIID: i64(7), Model: sptr("two words"), Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow); status != http.StatusBadRequest || !strings.HasPrefix(msg, "model:") {
		t.Fatalf("malformed model: status=%d msg=%q, want 400 and a \"model:\" message", status, msg)
	}

	// A model over the length cap → 400 with a "model:" message.
	tooLong := strings.Repeat("m", 101)
	if _, status, msg := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "sweep", Model: sptr(tooLong), Timing: "recurring", CronExpr: "0 9 * * 1",
	}, fixedNow); status != http.StatusBadRequest || !strings.HasPrefix(msg, "model:") {
		t.Fatalf("oversize model: status=%d msg=%q, want 400 and a \"model:\" message", status, msg)
	}

	// A blank/whitespace model normalizes to nil (inherit), status 0.
	if n, status, _ := validateScheduleConfig(apitypes.ScheduleRequest{
		Target: "issue", IssueIID: i64(7), Model: sptr("   \n\t "), Timing: "recurring", CronExpr: "0 2 * * *",
	}, fixedNow); status != 0 || n.Model != nil {
		t.Fatalf("blank model: status=%d model=%v, want status 0 and nil model", status, n.Model)
	}
}

// TestMergeScheduleModelClears pins PRD #300 M2's replace-semantics for model, mirroring
// guidance/max_issues: a config PATCH with Model=nil CLEARS a stored model, and a new value
// replaces it.
func TestMergeScheduleModelClears(t *testing.T) {
	cur := store.RunSchedule{
		Target:   "issue",
		IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		Timing:   "recurring",
		CronExpr: pgtype.Text{String: "0 2 * * *", Valid: true},
		Timezone: "UTC",
		Model:    pgtype.Text{String: "old-model", Valid: true},
	}

	// A config PATCH that omits model (nil) must CLEAR it — replace, not keep.
	cleared := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "recurring", CronExpr: "0 2 * * *"})
	if cleared.Model != nil {
		t.Fatalf("merged model = %v, want nil (a config PATCH replaces the whole row; nil clears to inherit)", cleared.Model)
	}

	// A config PATCH that sets model takes the request value.
	set := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "issue", IssueIID: i64(7), Timing: "recurring", CronExpr: "0 2 * * *", Model: sptr("fable")})
	if set.Model == nil || *set.Model != "fable" {
		t.Fatalf("merged model = %v, want \"fable\"", set.Model)
	}
}

// TestModelColumn pins the store-column mapping: a non-blank value on ANY target is Valid
// (model is NOT target-scoped); a nil or blank pointer yields SQL NULL.
func TestModelColumn(t *testing.T) {
	if c := modelColumn(apitypes.ScheduleRequest{Target: "issue", Model: sptr("fable")}); !c.Valid || c.String != "fable" {
		t.Fatalf("issue model column = %+v, want Valid \"fable\"", c)
	}
	if c := modelColumn(apitypes.ScheduleRequest{Target: "sweep", Model: sptr("fable")}); !c.Valid || c.String != "fable" {
		t.Fatalf("sweep model column = %+v, want Valid \"fable\"", c)
	}
	// Model is not target-scoped — a prompt target (and any other) persists the value too.
	if c := modelColumn(apitypes.ScheduleRequest{Target: "prompt", Model: sptr("fable")}); !c.Valid || c.String != "fable" {
		t.Fatalf("prompt model column = %+v, want Valid \"fable\" (model is not target-scoped)", c)
	}
	if c := modelColumn(apitypes.ScheduleRequest{Target: "issue", Model: nil}); c.Valid {
		t.Fatalf("nil model must be SQL NULL, got %+v", c)
	}
	if c := modelColumn(apitypes.ScheduleRequest{Target: "issue", Model: sptr("  ")}); c.Valid {
		t.Fatalf("blank model must be SQL NULL, got %+v", c)
	}
}

// TestScheduleDTOModel pins that scheduleDTO round-trips a stored model value into the DTO
// pointer, and maps a NULL/empty stored value to nil.
func TestScheduleDTOModel(t *testing.T) {
	h := &Handler{}
	base := store.RunSchedule{
		Target:   "issue",
		IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		Timing:   "once",
		Timezone: "UTC",
	}

	base.Model = pgtype.Text{String: "fable", Valid: true}
	if dto := h.scheduleDTO(base, ""); dto.Model == nil || *dto.Model != "fable" {
		t.Fatalf("DTO model = %v, want the stored value \"fable\"", dto.Model)
	}

	base.Model = pgtype.Text{}
	if dto := h.scheduleDTO(base, ""); dto.Model != nil {
		t.Fatalf("NULL model must map to nil, got %v", dto.Model)
	}

	base.Model = pgtype.Text{String: "", Valid: true}
	if dto := h.scheduleDTO(base, ""); dto.Model != nil {
		t.Fatalf("empty model must map to nil, got %v", dto.Model)
	}
}
