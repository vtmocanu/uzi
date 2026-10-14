package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func ciCursor(s string) *apitypes.CIEpisodeCursor {
	c := apitypes.CIEpisodeCursor(s)
	return &c
}

func TestScheduleCreateCIWatch(t *testing.T) {
	base := []string{"schedule", "create", "--repo", "r1", "--cron", "*/15 * * * *"}
	t.Run("payload", func(t *testing.T) {
		fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "s1", Timing: "recurring", CronExpr: "*/15 * * * *"}}
		_, errOut, code := runCLI(t, fakeEnv(fc), append(base, "--ci-watch", "--workflow", "build", "--workflow", " Lint ")...)
		if code != 0 {
			t.Fatalf("exit=%d err=%s", code, errOut)
		}
		r := fc.LastCreateSchedReq
		if r.Target != "ci_watch" || r.Prompt != "" {
			t.Fatalf("target/prompt = %q/%q", r.Target, r.Prompt)
		}
		if !r.CIWorkflows.Present || strings.Join(r.CIWorkflows.Value, ",") != "build,Lint" {
			t.Fatalf("workflows = %+v", r.CIWorkflows)
		}
		if r.AutoApprove != nil {
			t.Fatalf("auto_approve must be omitted for ci_watch, got %v", *r.AutoApprove)
		}
	})
	t.Run("prompt override and explicit auto-approve", func(t *testing.T) {
		fc := &uzicli.FakeClient{}
		_, errOut, code := runCLI(t, fakeEnv(fc), append(base, "--ci-watch", "--prompt", "fix it", "--auto-approve")...)
		if code != 0 {
			t.Fatalf("exit=%d err=%s", code, errOut)
		}
		r := fc.LastCreateSchedReq
		if r.Prompt != "fix it" || r.AutoApprove == nil || !*r.AutoApprove || r.CIWorkflows.Present {
			t.Fatalf("req = %+v", r)
		}
	})
	for name, args := range map[string][]string{
		"workflow without ci-watch": {"--prompt", "x", "--workflow", "build"},
		"issue":                     {"--ci-watch", "--issue", "3"},
		"sweep":                     {"--ci-watch", "--sweep"},
		"label":                     {"--ci-watch", "--label", "bug"},
		"max-issues":                {"--ci-watch", "--max-issues", "3"},
		"output":                    {"--ci-watch", "--output", "mr"},
		"guidance":                  {"--ci-watch", "--guidance", "g"},
		"blank workflow":            {"--ci-watch", "--workflow", "  "},
		"blank prompt":              {"--ci-watch", "--prompt", " "},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, _, code := runCLI(t, fakeEnv(fc), append(append([]string{}, base...), args...)...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit=%d want usage", code)
			}
			if fc.LastCreateSchedReq.Target != "" {
				t.Fatalf("request was sent: %+v", fc.LastCreateSchedReq)
			}
		})
	}
}

func TestScheduleEditCIWorkflows(t *testing.T) {
	ci := apitypes.ScheduleDTO{ID: "s", Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *", Prompt: "p", CIWorkflows: []string{"old"}}
	wire := func(r apitypes.ScheduleRequest) map[string]json.RawMessage {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, origin := range []string{"user", "default"} {
		d := ci
		d.Origin = origin
		t.Run(origin+"/clear sends null", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--clear-workflows")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if v, ok := wire(fc.LastPatchSchedReq)["ci_workflows"]; !ok || string(v) != "null" {
				t.Fatalf("ci_workflows wire = %s (present=%v)", v, ok)
			}
		})
		t.Run(origin+"/set", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--workflow", "a", "--workflow", "b")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if v := wire(fc.LastPatchSchedReq)["ci_workflows"]; string(v) != `["a","b"]` {
				t.Fatalf("ci_workflows wire = %s", v)
			}
		})
		t.Run(origin+"/absent sends nothing", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--cron", "5 * * * *")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if _, ok := wire(fc.LastPatchSchedReq)["ci_workflows"]; ok {
				t.Fatal("ci_workflows must be absent when no workflow flag is passed")
			}
		})
		t.Run(origin+"/exclusive", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--workflow", "a", "--clear-workflows")
			if code != uzicli.ExitUsage {
				t.Fatalf("exit=%d", code)
			}
		})
	}
	t.Run("wrong target", func(t *testing.T) {
		sw := apitypes.ScheduleDTO{ID: "s", Target: "sweep", Timing: "recurring", CronExpr: "0 * * * *"}
		for _, flag := range []string{"--clear-workflows", "--workflow=a"} {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": sw}}
			_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", flag)
			if code != uzicli.ExitUsage {
				t.Fatalf("%s exit=%d", flag, code)
			}
		}
	})
}

// ciEpisodeClient serves ListScheduleCIEpisodes through the real HTTPClient against an
// httptest server, leaving every other call to the embedded fake.
type ciEpisodeClient struct {
	*uzicli.FakeClient
	base string
}

func (c *ciEpisodeClient) ListScheduleCIEpisodes(ctx context.Context, id, cursor string) (apitypes.CIEpisodePage, error) {
	return uzicli.NewHTTPClient(uzicli.Settings{URL: c.base, Token: "test"}).ListScheduleCIEpisodes(ctx, id, cursor)
}

func TestScheduleGetCIWatchCursorRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	halted := "same failure twice"
	run := int64(42)
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		cursors = append(cursors, c)
		var page apitypes.CIEpisodePage
		switch c {
		case "c2":
			page = apitypes.CIEpisodePage{Episodes: []apitypes.CIEpisodeDTO{{WorkflowKey: "deploy", Attempts: 1, UpdatedAt: now}}}
		case "c1":
			page = apitypes.CIEpisodePage{
				Episodes:   []apitypes.CIEpisodeDTO{{WorkflowKey: "lint", Attempts: 0, UpdatedAt: now}},
				NextCursor: ciCursor("c2"),
			}
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	first := apitypes.CIEpisodePage{
		Episodes: []apitypes.CIEpisodeDTO{{
			WorkflowKey: "build\x1b[31m", Attempts: 2, HaltedReason: &halted, ObservedRunID: &run, UpdatedAt: now,
		}},
		NextCursor: ciCursor("c1"),
	}
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"s": {ID: "s", Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *", CIEpisodes: &first},
	}}
	env := fakeEnv(&ciEpisodeClient{FakeClient: fc, base: srv.URL})

	out, errOut, code := runCLI(t, env, "schedule", "get", "s")
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut)
	}
	for _, want := range []string{
		"all workflows in the listed window (newest 100 executions; 50 on Forgejo)",
		"Auto-approve is off by default for CI repair: CI logs can contain text from dependencies, tests or outside contributors. Review the plan before the run changes code.",
		"halted: same failure twice", "next cursor: c1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape byte from forge text reached the terminal:\n%q", out)
	}

	out, errOut, code = runCLI(t, env, "schedule", "get", "s", "--episodes-cursor", "c1")
	if code != 0 || !strings.Contains(out, "lint") || !strings.Contains(out, "next cursor: c2") {
		t.Fatalf("page 2 exit=%d err=%s out=%s", code, errOut, out)
	}
	out, errOut, code = runCLI(t, env, "schedule", "get", "s", "--episodes-cursor", "c2")
	if code != 0 || !strings.Contains(out, "deploy") || strings.Contains(out, "next cursor") {
		t.Fatalf("page 3 exit=%d err=%s out=%s", code, errOut, out)
	}
	out, _, code = runCLI(t, env, "schedule", "get", "s", "--episodes-cursor", "c1", "--json")
	if code != 0 {
		t.Fatalf("json exit=%d", code)
	}
	var page apitypes.CIEpisodePage
	if err := json.Unmarshal([]byte(out), &page); err != nil || page.NextCursor == nil || *page.NextCursor != "c2" {
		t.Fatalf("json page = %s (%v)", out, err)
	}
	if strings.Join(cursors, ",") != "c1,c2,c1" {
		t.Fatalf("server saw cursors %v", cursors)
	}
}

func TestScheduleGetCIWatchWorkflowsListed(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"s": {ID: "s", Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *", CIWorkflows: []string{"build", "lint"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "s")
	if code != 0 || !strings.Contains(out, "Watching: build, lint") || strings.Contains(out, "newest 100") {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "Auto-approve is off by default for CI repair") {
		t.Fatalf("note missing: %s", out)
	}
}
