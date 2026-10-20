package main

import (
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"strings"
	"testing"
)

func TestScheduleEditCIArtifacts(t *testing.T) {
	ci := apitypes.ScheduleDTO{ID: "s", Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *", Prompt: "p", CIArtifacts: []string{"old"}}
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
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--clear-artifacts")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if v, ok := wire(fc.LastPatchSchedReq)["ci_artifacts"]; !ok || string(v) != "null" {
				t.Fatalf("ci_artifacts wire = %s (present=%v)", v, ok)
			}
		})
		t.Run(origin+"/set", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--artifact", "a", "--artifact", "b")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if v := wire(fc.LastPatchSchedReq)["ci_artifacts"]; string(v) != `["a","b"]` {
				t.Fatalf("ci_artifacts wire = %s", v)
			}
		})
		t.Run(origin+"/absent sends nothing", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--cron", "5 * * * *")
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut)
			}
			if _, ok := wire(fc.LastPatchSchedReq)["ci_artifacts"]; ok {
				t.Fatal("ci_artifacts must be absent when no artifact flag is passed")
			}
		})
		t.Run(origin+"/exclusive", func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": d}}
			_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", "--artifact", "a", "--clear-artifacts")
			if code != uzicli.ExitUsage {
				t.Fatalf("exit=%d", code)
			}
		})
	}
	t.Run("wrong target", func(t *testing.T) {
		sw := apitypes.ScheduleDTO{ID: "s", Target: "sweep", Timing: "recurring", CronExpr: "0 * * * *"}
		for _, flag := range []string{"--clear-artifacts", "--artifact=a"} {
			fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": sw}}
			_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "s", flag)
			if code != uzicli.ExitUsage {
				t.Fatalf("%s exit=%d", flag, code)
			}
		}
	})
}

func TestScheduleCreateCIArtifacts(t *testing.T) {
	for _, args := range [][]string{{"--ci-watch", "--artifact", " report-* "}, {"--prompt", "x", "--artifact", "x"}, {"--ci-watch", "--artifact", "["}, {"--ci-watch", "--artifact", " "}} {
		fc := &uzicli.FakeClient{}
		base := []string{"schedule", "create", "--repo", "r1", "--cron", "0 * * * *"}
		_, out, code := runCLI(t, fakeEnv(fc), append(base, args...)...)
		if args[1] == "--artifact" && args[2] == " report-* " {
			if code != 0 || !fc.LastCreateSchedReq.CIArtifacts.Present || fc.LastCreateSchedReq.CIArtifacts.Value[0] != "report-*" {
				t.Fatalf("%d %s", code, out)
			}
		} else if code != uzicli.ExitUsage {
			t.Fatalf("accepted %v: %d %s", args, code, out)
		}
	}
}
func TestScheduleCIArtifactHelpAndRender(t *testing.T) {
	for _, sub := range []string{"create", "edit"} {
		out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "schedule", sub, "--help")
		if code != 0 || !strings.Contains(out, "--artifact") || (sub == "edit" && !strings.Contains(out, "--clear-artifacts")) {
			t.Fatal(out)
		}
	}
	for _, selectors := range [][]string{nil, {"reports-*"}} {
		fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": {ID: "s", Target: "ci_watch", Timing: "recurring", CronExpr: "0 * * * *", CIArtifacts: selectors}}}
		out, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "get", "s")
		want := "Artifacts: none"
		if len(selectors) > 0 {
			want = "Artifacts: reports-*"
		}
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("%d %s %s", code, out, errOut)
		}
	}
}
