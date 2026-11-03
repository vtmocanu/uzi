package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCrossCheckHistoryDisabledChildlessAvailabilityLiveDB(t *testing.T) {
	f, empty := newHistoryLiveFixture(t), newHistoryLiveFixture(t)
	// Column names are defined by migrations 00299 and 00324.
	for _, owner := range []*historyLiveFixture{f, empty} {
		cliMustExec(t, owner.pool, "UPDATE users SET plan_cross_check_enabled=false,code_cross_check_enabled=false WHERE id=$1", owner.owner)
	}
	assertAvailability := func(owner *historyLiveFixture, cli, want bool) {
		t.Helper()
		// Filters that cannot select the retained Plan must not affect discovery.
		for _, query := range []string{"", "?lifecycle=active&stage=code&outcome=passed&limit=1"} {
			rec := owner.call("/api/cross-checks/availability"+query, cli)
			if rec.Code != http.StatusOK {
				t.Fatalf("availability cli=%t: status=%d", cli, rec.Code)
			}
			var got apitypes.CrossCheckHistoryAvailabilityDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal("availability response is not valid JSON")
			}
			if got.Available != want {
				t.Fatalf("disabled childless availability cli=%t filtered=%t: got %t want %t", cli, query != "", got.Available, want)
			}
		}
	}
	for _, owner := range []*historyLiveFixture{f, empty} {
		for _, cli := range []bool{false, true} {
			assertAvailability(owner, cli, false)
			for _, lifecycle := range []string{"active", "past"} {
				if len(owner.list("lifecycle="+lifecycle, cli).Groups) != 0 {
					t.Fatalf("before evidence cli=%t lifecycle=%s: expected empty history", cli, lifecycle)
				}
			}
		}
	}
	parent := f.parent("failed", "retained childless failure")
	f.plan(parent, nil, 1, "failed", "", time.Now().UTC())
	for _, cli := range []bool{false, true} {
		assertAvailability(f, cli, true)
		assertAvailability(empty, cli, false)
		for _, lifecycle := range []string{"active", "past"} {
			if len(empty.list("lifecycle="+lifecycle, cli).Groups) != 0 {
				t.Fatalf("nonevidence owner cli=%t lifecycle=%s saw another owner's retained failure", cli, lifecycle)
			}
		}
		if len(f.list("lifecycle=active", cli).Groups) != 0 {
			t.Fatalf("childless settled failure cli=%t appeared active", cli)
		}
		got := f.list("lifecycle=past&stage=plan&outcome=didnt_finish", cli)
		if len(got.Groups) != 1 || got.Groups[0].ParentID != parent.String() || len(got.Groups[0].Attempts) != 1 {
			t.Fatalf("disabled owner cli=%t: retained childless failure missing from Past", cli)
		}
		g := got.Groups[0]
		a := g.Attempts[0]
		if g.Active || g.TotalAttempts != 1 || g.MatchingAttempts != 1 ||
			a.Stage != "plan" || a.Round != 1 || a.Result != "failed" || a.Review != "none" ||
			a.Execution != "didnt_finish" || a.Reason != "Cause not recorded for this attempt" ||
			a.Effect != "Effect not recorded" || a.ChildID != nil || a.ChildStatus != nil ||
			a.ChildContext != "No checker run available" {
			t.Fatalf("disabled childless failure cli=%t: expected recorded failure, safe execution, no child and no recorded effect", cli)
		}
	}
}

func TestCrossCheckHistoryLifecycleBeforeFiltersLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	parent := f.parent("completed", "settled Plan with active Code")
	now := time.Now().UTC().Truncate(time.Microsecond)
	planChild := f.child(parent, "completed")
	f.plan(parent, planChild, 1, "approve", "approve", now)
	codeChild := f.child(parent, "running")
	f.code(parent, codeChild, "pending", "", "[]", now.Add(time.Second))
	for _, cli := range []bool{false, true} {
		got := f.list("lifecycle=active&stage=plan", cli)
		if len(got.Groups) != 1 || got.Groups[0].ParentID != parent.String() || len(got.Groups[0].Attempts) != 1 {
			t.Fatalf("active Plan filter cli=%t: settled Plan missing while only Code is active", cli)
		}
		g := got.Groups[0]
		a := g.Attempts[0]
		if !g.Active || g.ParentStatus != "completed" || g.TotalAttempts != 2 || g.MatchingAttempts != 1 ||
			g.ActiveAttempts != 1 || g.ActiveHidden != 1 || g.ActiveContext != "Active attempts outside this slice or filter" ||
			a.Stage != "plan" || a.Active || a.Result != "approve" || a.Review != "approved" ||
			a.Execution != "finished" || a.ChildID == nil || *a.ChildID != planChild.String() ||
			a.ChildStatus == nil || *a.ChildStatus != "completed" {
			t.Fatalf("active Plan filter cli=%t: lifecycle must include hidden active Code before applying stage filter", cli)
		}
		if len(f.list("lifecycle=past&stage=plan", cli).Groups) != 0 {
			t.Fatalf("Past Plan filter cli=%t: active Code was filtered out before lifecycle classification", cli)
		}
	}
	// Only fixture evidence is settled; no user policy is changed.
	cliMustExec(t, f.pool, "UPDATE cross_checks SET outcome='completed',decided_at=$2 WHERE lead_run_id=$1 AND stage='code'", parent, now.Add(2*time.Second))
	cliMustExec(t, f.pool, "UPDATE runs SET status='completed' WHERE id=$1", codeChild)
	for _, cli := range []bool{false, true} {
		if len(f.list("lifecycle=active&stage=plan", cli).Groups) != 0 {
			t.Fatalf("settlement control cli=%t: Active Plan retained a fully settled group", cli)
		}
		got := f.list("lifecycle=past&stage=plan", cli)
		if len(got.Groups) != 1 || got.Groups[0].ParentID != parent.String() || len(got.Groups[0].Attempts) != 1 {
			t.Fatalf("settlement control cli=%t: Past Plan missing after Code and child settled", cli)
		}
		g := got.Groups[0]
		if g.Active || g.TotalAttempts != 2 || g.MatchingAttempts != 1 || g.ActiveAttempts != 0 ||
			g.ActiveHidden != 0 || g.ActiveContext != "" || g.Attempts[0].Stage != "plan" || g.Attempts[0].Active {
			t.Fatalf("settlement control cli=%t: Past Plan retained active context or lost unfiltered totals", cli)
		}
	}
}

func TestCrossCheckHistoryPersistedScrubBoundaryLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	token := "glpat-" + strings.Repeat("a", 20)
	// After normalization the Unicode prefix and folded whitespace occupy four
	// bytes. The token crosses the output cap but stays inside the SQL input cap.
	title := "\u001b\u202eé\n\t" + strings.Repeat("x", 496) + token + " tail\u001b\u202e\n\t"
	metadata := "\u001b\u202eé\n\t" + strings.Repeat("m", 112) + token + " tail\u001b\u202e\n\t"
	wantTitle := "é  " + strings.Repeat("x", 496) + "[redacted] t"
	wantMetadata := "é  " + strings.Repeat("m", 112) + "[redacted] t"
	if len(title) >= 2048 || len(metadata) >= 512 || len(wantTitle) != 512 || len(wantMetadata) != 128 {
		t.Fatal("scrub boundary fixture must cross output caps while remaining within SQL input caps")
	}
	parent := f.parent("completed", title)
	now := time.Now().UTC().Truncate(time.Microsecond)
	// Two schema-valid nonautomatic rounds make round 1 historical without
	// changing the frozen automatic revision budget.
	// The reason_class constraint rejects arbitrary strings; NULL exercises the
	// safe fallback without bypassing the schema.
	f.plan(parent, nil, 1, "failed", "", now)
	f.plan(parent, nil, 2, "approve", "approve", now.Add(time.Second))
	findings, err := json.Marshal(map[string]any{"summary": "private_secret_findings_" + token, "items": []any{}})
	if err != nil {
		t.Fatal("cannot encode scrub boundary fixture findings")
	}
	// checker_model/effort are mutable metadata, as in TestCrossCheckHistoryLimitsLiveDB.
	cliMustExec(t, f.pool, "UPDATE cross_checks SET checker_model=$2,checker_effort=$2,findings=$3::jsonb WHERE lead_run_id=$1 AND stage='plan' AND round=1", parent, metadata, string(findings))
	for _, cli := range []bool{false, true} {
		rec := f.call("/api/cross-checks?lifecycle=past&stage=plan&attempt_limit=2", cli)
		if rec.Code != http.StatusOK {
			t.Fatalf("persisted scrub cli=%t: status=%d", cli, rec.Code)
		}
		raw := rec.Body.String()
		// Never print the response or the secret-bearing fixture on failure.
		for _, forbidden := range []string{"glpat-", strings.Repeat("a", 8), "private_secret_findings_", "private raw findings"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("persisted scrub cli=%t: raw API JSON leaked secret fragments or private evidence", cli)
			}
		}
		var got apitypes.CrossCheckHistoryDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal("persisted scrub response is not valid JSON")
		}
		if len(got.Groups) != 1 || got.Groups[0].ParentID != parent.String() || len(got.Groups[0].Attempts) != 2 {
			t.Fatalf("persisted scrub cli=%t: expected one parent and two retained Plan rounds", cli)
		}
		g := got.Groups[0]
		if g.ParentTitle != wantTitle {
			t.Fatalf("persisted scrub cli=%t: title must normalize and redact the entire value before the 512-byte cap", cli)
		}
		a := g.Attempts[0]
		if a.Stage != "plan" || a.Round != 1 || !a.Historical || a.Result != "failed" ||
			a.Reason != "Cause not recorded for this attempt" || a.Effect != "Effect not recorded" {
			t.Fatalf("persisted scrub cli=%t: historical failure or safe unknown-reason fallback lost", cli)
		}
		if a.Model == nil || *a.Model != wantMetadata || a.Effort == nil || *a.Effort != wantMetadata {
			t.Fatalf("persisted scrub cli=%t: model and effort must normalize and redact before the 128-byte cap", cli)
		}
		for _, field := range []string{g.ParentTitle, *a.Model, *a.Effort, a.Reason, a.Effect, a.ChildContext} {
			if strings.ContainsFunc(field, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) {
				t.Fatalf("persisted scrub cli=%t: returned field contains a Cc/Cf control", cli)
			}
		}
		if !strings.HasPrefix(g.ParentTitle, "é  ") || !strings.Contains(g.ParentTitle, "[redacted]") ||
			!strings.HasPrefix(*a.Model, "é  ") || !strings.Contains(*a.Model, "[redacted]") {
			t.Fatalf("persisted scrub cli=%t: legal Unicode or positive redaction control missing", cli)
		}
	}
}
