package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func ciWatchReq(mutate func(*apitypes.ScheduleRequest)) apitypes.ScheduleRequest {
	r := apitypes.ScheduleRequest{Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *"}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func ciList(v ...string) apitypes.OptionalCIWorkflows {
	return apitypes.OptionalCIWorkflows{Present: true, Value: v}
}

func TestValidateCIWatchDefaultsPromptFromCatalog(t *testing.T) {
	job, ok := schedtmpl.BySlug("ci-repair")
	if !ok {
		t.Fatal("ci-repair catalog entry missing")
	}
	got, status, msg := validateScheduleConfig(ciWatchReq(nil), time.Now(), false)
	if status != 0 {
		t.Fatalf("blank-prompt ci_watch rejected: %d %s", status, msg)
	}
	if got.Prompt != job.Prompt {
		t.Fatalf("prompt = %q, want the ci-repair catalog body", got.Prompt)
	}
	got, status, _ = validateScheduleConfig(ciWatchReq(func(r *apitypes.ScheduleRequest) { r.Prompt = "  mine  " }), time.Now(), false)
	if status != 0 || got.Prompt != "  mine  " {
		t.Fatalf("explicit prompt not kept: %d %q", status, got.Prompt)
	}
	_, status, _ = validateScheduleConfig(ciWatchReq(func(r *apitypes.ScheduleRequest) { r.Prompt = strings.Repeat("x", 300*1024) }), time.Now(), false)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("oversize prompt status = %d, want 422", status)
	}
}

func TestValidateCIWatchRejectsForeignFields(t *testing.T) {
	iid, max, mode, guidance := int64(3), 4, "mr", "steer"
	for name, mutate := range map[string]func(*apitypes.ScheduleRequest){
		"issue_iid":   func(r *apitypes.ScheduleRequest) { r.IssueIID = &iid },
		"labels":      func(r *apitypes.ScheduleRequest) { r.Labels = []string{"bug"} },
		"max_issues":  func(r *apitypes.ScheduleRequest) { r.MaxIssues = &max },
		"output_mode": func(r *apitypes.ScheduleRequest) { r.OutputMode = &mode },
		"guidance":    func(r *apitypes.ScheduleRequest) { r.Guidance = &guidance },
	} {
		if _, status, _ := validateScheduleConfig(ciWatchReq(mutate), time.Now(), false); status < 400 || status >= 500 {
			t.Errorf("%s on ci_watch: status %d, want a 4xx", name, status)
		}
	}
	// Blank labels are stripped, not rejected.
	if _, status, msg := validateScheduleConfig(ciWatchReq(func(r *apitypes.ScheduleRequest) { r.Labels = []string{" "} }), time.Now(), false); status != 0 {
		t.Errorf("blank labels rejected: %d %s", status, msg)
	}
}

func TestValidateCIWorkflowsBounds(t *testing.T) {
	twenty := make([]string, 20)
	for i := range twenty {
		twenty[i] = strings.Repeat("a", 5) + string(rune('A'+i))
	}
	cases := []struct {
		name string
		list []string
		ok   bool
	}{
		{"one", []string{"nightly"}, true},
		{"twenty", twenty, true},
		{"twenty-one", append(append([]string{}, twenty...), "extra"), false},
		{"empty entry", []string{""}, false},
		{"blank entry", []string{"   "}, false},
		{"200 bytes", []string{strings.Repeat("b", 200)}, true},
		{"201 bytes", []string{strings.Repeat("b", 201)}, false},
		{"duplicate", []string{"nightly", "nightly"}, false},
		{"duplicate after trim", []string{"nightly", " nightly "}, false},
		{"control char", []string{"night\x00ly"}, false},
		{"newline", []string{"night\nly"}, false},
		{"tab", []string{"night\tly"}, false},
		{"empty list clears", []string{}, true},
	}
	for _, c := range cases {
		_, status, _ := validateScheduleConfig(ciWatchReq(func(r *apitypes.ScheduleRequest) { r.CIWorkflows = ciList(c.list...) }), time.Now(), false)
		if (status == 0) != c.ok {
			t.Errorf("%s: status %d, want ok=%v", c.name, status, c.ok)
		}
	}
	got, _, _ := validateScheduleConfig(ciWatchReq(func(r *apitypes.ScheduleRequest) { r.CIWorkflows = ciList("  nightly ") }), time.Now(), false)
	if len(got.CIWorkflows.Value) != 1 || got.CIWorkflows.Value[0] != "nightly" {
		t.Fatalf("entries not trimmed: %q", got.CIWorkflows.Value)
	}
}

func TestValidateCIWorkflowsRejectedOnOtherTargets(t *testing.T) {
	for _, target := range []string{"prompt", "sweep", "issue"} {
		req := apitypes.ScheduleRequest{Target: target, Prompt: "p", Timing: "recurring", CronExpr: "0 * * * *", CIWorkflows: ciList("nightly")}
		if target == "issue" {
			iid := int64(1)
			req.IssueIID = &iid
		}
		if _, status, _ := validateScheduleConfig(req, time.Now(), false); status != http.StatusBadRequest {
			t.Errorf("%s with ci_workflows: status %d, want 400", target, status)
		}
		// An explicit empty list is "clear", valid anywhere.
		req.CIWorkflows = ciList()
		if _, status, msg := validateScheduleConfig(req, time.Now(), false); status != 0 {
			t.Errorf("%s with empty ci_workflows: %d %s", target, status, msg)
		}
	}
}

func TestInvalidTargetMessageListsCIWatch(t *testing.T) {
	_, status, msg := validateScheduleConfig(apitypes.ScheduleRequest{Target: "nope", Timing: "recurring", CronExpr: "0 * * * *"}, time.Now(), false)
	if status != http.StatusBadRequest || !strings.Contains(msg, "ci_watch") {
		t.Fatalf("status %d msg %q, want 400 listing ci_watch", status, msg)
	}
}

func TestApplyCreateDefaultsAutoApproveByTarget(t *testing.T) {
	ci := apitypes.ScheduleRequest{Target: "ci_watch"}
	applyCreateDefaults(&ci)
	if *ci.AutoApprove {
		t.Fatal("ci_watch must default to manual approval")
	}
	on := true
	ci = apitypes.ScheduleRequest{Target: "ci_watch", AutoApprove: &on}
	applyCreateDefaults(&ci)
	if !*ci.AutoApprove {
		t.Fatal("an explicit auto_approve=true must be honored")
	}
	for _, target := range []string{"prompt", "sweep", "issue"} {
		r := apitypes.ScheduleRequest{Target: target}
		applyCreateDefaults(&r)
		if !*r.AutoApprove {
			t.Errorf("%s must keep the auto-approve default", target)
		}
	}
}

func TestMergeScheduleCIWorkflowsPresence(t *testing.T) {
	cur := store.RunSchedule{Target: "ci_watch", Timing: "recurring", Timezone: "UTC", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, CiWorkflows: []byte(`["a","b"]`), Prompt: pgtype.Text{String: "p", Valid: true}}
	if got := mergeSchedule(cur, apitypes.ScheduleRequest{}); !got.CIWorkflows.Present || len(got.CIWorkflows.Value) != 2 {
		t.Fatalf("absent must keep the stored list, got %+v", got.CIWorkflows)
	}
	var cleared apitypes.ScheduleRequest
	if err := cleared.CIWorkflows.UnmarshalJSON([]byte("null")); err != nil {
		t.Fatal(err)
	}
	if got := mergeSchedule(cur, cleared); !got.CIWorkflows.Present || len(got.CIWorkflows.Value) != 0 {
		t.Fatalf("null must clear, got %+v", got.CIWorkflows)
	}
	if got := mergeSchedule(cur, apitypes.ScheduleRequest{CIWorkflows: ciList()}); len(got.CIWorkflows.Value) != 0 {
		t.Fatalf("[] must clear, got %+v", got.CIWorkflows)
	}
	if got := mergeSchedule(cur, apitypes.ScheduleRequest{CIWorkflows: ciList("c")}); len(got.CIWorkflows.Value) != 1 || got.CIWorkflows.Value[0] != "c" {
		t.Fatalf("a list must replace, got %+v", got.CIWorkflows)
	}
	// Converting away from ci_watch drops the stored selectors instead of failing validation.
	away := mergeSchedule(cur, apitypes.ScheduleRequest{Target: "prompt"})
	if _, status, msg := validateScheduleConfig(away, time.Now(), false); status != 0 {
		t.Fatalf("convert ci_watch -> prompt rejected: %d %s", status, msg)
	}
	if col := ciWorkflowsColumn(away); col != nil {
		t.Fatalf("selectors persisted on a prompt row: %s", col)
	}
	// Converting a sweep into ci_watch does not trip over the sweep's stored labels.
	sweep := store.RunSchedule{Target: "sweep", Timing: "recurring", Timezone: "UTC", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Labels: []byte(`["bug"]`)}
	into := mergeSchedule(sweep, apitypes.ScheduleRequest{Target: "ci_watch"})
	if _, status, msg := validateScheduleConfig(into, time.Now(), false); status != 0 {
		t.Fatalf("convert sweep -> ci_watch rejected: %d %s", status, msg)
	}
}

func TestOnlyEnabledFalseWhenCIWorkflowsPresent(t *testing.T) {
	on := true
	if !onlyEnabled(apitypes.ScheduleRequest{Enabled: &on}) {
		t.Fatal("enabled alone must short-circuit")
	}
	if onlyEnabled(apitypes.ScheduleRequest{Enabled: &on, CIWorkflows: ciList()}) {
		t.Fatal("a present ci_workflows must route through the config path")
	}
}

func TestCIEpisodeCursorRoundTrip(t *testing.T) {
	for _, key := range []string{"a", "nightly e2e", "ünï/cødé", strings.Repeat("k", 200)} {
		got, ok := decodeCIEpisodeCursor(string(encodeCIEpisodeCursor(key)))
		if !ok || got != key {
			t.Errorf("key %q -> %q ok=%v", key, got, ok)
		}
	}
	for name, bad := range map[string]string{
		"not base64":    "!!!",
		"padded":        "YQ==",
		"too long":      string(encodeCIEpisodeCursor(strings.Repeat("k", 201))),
		"nul":           string(encodeCIEpisodeCursor("a\x00b")),
		"invalid utf-8": "_w",
	} {
		if _, ok := decodeCIEpisodeCursor(bad); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCatalogAutoApproveIsPerJob(t *testing.T) {
	sawCI := false
	for _, job := range schedtmpl.Catalog() {
		dto := catalogEntryDTO(job)
		if job.Slug == "ci-repair" {
			sawCI = true
			if dto.AutoApprove || dto.Target != "ci_watch" {
				t.Errorf("ci-repair entry = auto %v target %s, want off/ci_watch", dto.AutoApprove, dto.Target)
			}
		} else if !dto.AutoApprove {
			t.Errorf("%s lost its auto-approve default", job.Slug)
		}
		// The customized check compares against the job's own baseline.
		if defaultEditableDiverges(job, job.Cron, catalogTimezone(job), catalogModel(job), job.AutoApprove, schedtmpl.WaitOnLimit, pgtype.Bool{}, catalogMaxIssues(job), catalogOutputMode(job), job.RemoveLabelOnDispatch, catalogCapacity(job.CapacityLimit), catalogCapacity(job.CapacityRoomNeeded)) {
			t.Errorf("%s diverges from its own baseline", job.Slug)
		}
	}
	if !sawCI {
		t.Fatal("ci-repair missing from catalog")
	}
	job, _ := schedtmpl.BySlug("ci-repair")
	if !defaultEditableDiverges(job, job.Cron, catalogTimezone(job), pgtype.Text{}, true, schedtmpl.WaitOnLimit, pgtype.Bool{}, pgtype.Int4{}, pgtype.Text{}, false) {
		t.Error("turning auto-approve on for ci-repair must mark it customized")
	}
}
