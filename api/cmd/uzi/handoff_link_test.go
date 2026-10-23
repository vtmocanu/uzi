package main

import (
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"strings"
	"testing"
)

func TestHandoffIssueValidation(t *testing.T) {
	for _, value := range []string{"0", "-1", "9223372036854775808", "nope"} {
		t.Run(value, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			rec := &handoffRecorder{}
			env, _ := handoffEnv(fc, rec)
			_, stderr, code := runCLI(t, env, "handoff", "-m", "task", "--issue", value)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			if len(fc.TaskCalls) != 0 || len(rec.gitCalls) != 0 {
				t.Fatalf("invalid input caused side effects: %v %v", fc.TaskCalls, rec.gitCalls)
			}
		})
	}
}

func TestHandoffIssueForwardAndConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "linked", true: "conflict"}[conflict], func(t *testing.T) {
			fc := &uzicli.FakeClient{CreatedTaskRun: taskRun("r", "uzi/task/r"), DispatchedRun: taskRun("r", "uzi/task/r")}
			if conflict {
				fc.CreateTaskRunErr = uzicli.Exitf(uzicli.ExitConflict, "a non-terminal run already exists for this issue")
			}
			rec := &handoffRecorder{}
			env, _ := handoffEnv(fc, rec)
			_, stderr, code := runCLI(t, env, "handoff", "-m", "task", "--issue", "42")
			want := uzicli.ExitOK
			if conflict {
				want = uzicli.ExitConflict
			}
			if code != want {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			if fc.LastCreateTaskContinuedIssueIID == nil || *fc.LastCreateTaskContinuedIssueIID != 42 {
				t.Fatal("link not forwarded")
			}
			pushed := false
			for _, call := range rec.gitCalls {
				if len(call) > 0 && call[0] == "push" {
					pushed = true
				}
			}
			if pushed == conflict || (fc.LastDispatchRunID != "") == conflict {
				t.Fatalf("push=%v dispatch=%q conflict=%v", pushed, fc.LastDispatchRunID, conflict)
			}
		})
	}
}

func TestHandoffOmittedIssueKeepsBranch(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedTaskRun: taskRun("r", "uzi/task/server"), DispatchedRun: taskRun("r", "uzi/task/server")}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)
	_, stderr, code := runCLI(t, env, "handoff", "-m", "ordinary task")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if fc.LastCreateTaskContinuedIssueIID != nil {
		t.Fatal("omitted issue became a continuation link")
	}
	pushed := hasGitCall(rec, []string{"push", "origin", "HEAD:refs/heads/uzi/task/server"})
	if !pushed || fc.LastDispatchRunID != "r" {
		t.Fatalf("push=%v dispatch=%q", pushed, fc.LastDispatchRunID)
	}
}

func TestHandoffRunGetJSONUnlinked(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r": {ID: "r", Kind: "task"}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["continued_issue_iid"]) != "null" || string(body["issue_iid"]) != "null" {
		t.Fatalf("wrong JSON: %s", out)
	}
}

func TestHandoffRunGetJSONContinuedIssue(t *testing.T) {
	iid := int64(42)
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r": {ID: "r", Kind: "task", ContinuedIssueIID: &iid}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["continued_issue_iid"]) != "42" || strings.TrimSpace(string(body["issue_iid"])) != "null" {
		t.Fatalf("wrong JSON: %s", out)
	}
}
