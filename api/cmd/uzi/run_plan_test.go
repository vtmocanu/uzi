package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func planMessage(seq int32, text string) apitypes.MessageDTO {
	payload, _ := json.Marshal(map[string]string{"plan_md": text})
	return apitypes.MessageDTO{Seq: seq, Kind: "plan", Payload: payload}
}

func planFeedback(seq int32) apitypes.MessageDTO {
	return apitypes.MessageDTO{Seq: seq, Kind: "plan_feedback", Payload: json.RawMessage(`{"automatic":true}`)}
}

func planEnv(messages ...apitypes.MessageDTO) Env {
	return fakeEnv(&uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r": messages}})
}

type planFixture struct {
	Name          string                `json:"name"`
	Messages      []apitypes.MessageDTO `json:"messages"`
	TargetVersion int                   `json:"target_version"`
	TargetSeq     int32                 `json:"target_seq"`
	Expected      struct {
		Version     int     `json:"version"`
		PlanMD      string  `json:"plan_md"`
		BaseVersion *int    `json:"base_version"`
		BasePlanMD  *string `json:"base_plan_md"`
		Identical   bool    `json:"identical_after_feedback"`
	} `json:"expected"`
}

func planFixtures(t *testing.T) []planFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/plan-revision/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []planFixture
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	required := []string{"one-line edit", "pending feedback", "re-presentation", "two rounds", "earlier version with later feedback", "identical revise", "feedback is not reused", "no predecessor", "automatic feedback", "unsorted feed", "trailing CR/LF equality", "meaningful trailing spaces", "pending feedback preserves existing base", "earlier revision keeps its own base"}
	names := map[string]bool{}
	var withBase, withoutBase, identical, changed bool
	for _, c := range cases {
		if names[c.Name] {
			t.Fatalf("duplicate fixture %q", c.Name)
		}
		names[c.Name] = true
		if c.Expected.BaseVersion == nil {
			withoutBase = true
		} else {
			withBase = true
			if c.Expected.Identical {
				identical = true
			} else {
				changed = true
			}
		}
	}
	for _, name := range required {
		if !names[name] {
			t.Errorf("missing required fixture %q", name)
		}
	}
	if len(cases) != len(required) || !withBase || !withoutBase || !identical || !changed {
		t.Fatal("canonical branch coverage changed")
	}
	return cases
}

func TestPlanBaseCanonical(t *testing.T) {
	for _, c := range planFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			msgs := append([]apitypes.MessageDTO(nil), c.Messages...)
			sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
			idx := -1
			for i, m := range msgs {
				if m.Seq == c.TargetSeq {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatal("missing target")
			}
			b := planBase(msgs, idx)
			if c.Expected.BaseVersion == nil {
				if b != -1 {
					t.Fatalf("base=%d, want none", b)
				}
			} else {
				if b < 0 {
					t.Fatal("missing base")
				}
				text, err := planMarkdown(msgs[b])
				if err != nil || text != *c.Expected.BasePlanMD {
					t.Fatalf("base text=%q error=%v", text, err)
				}
				v := 0
				for i := 0; i <= b; i++ {
					if msgs[i].Kind == "plan" {
						v++
					}
				}
				if v != *c.Expected.BaseVersion {
					t.Fatalf("base version=%d", v)
				}
			}
		})
	}
}

func TestRunPlanCanonical(t *testing.T) {
	for _, c := range planFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			env := planEnv(c.Messages...)
			for _, diff := range []bool{false, true} {
				args := []string{"run", "plan", "r", "--version", strconv.Itoa(c.TargetVersion), "--json"}
				if diff {
					args = append(args, "--diff")
				}
				out, stderr, code := runCLI(t, env, args...)
				if code != 0 {
					t.Fatalf("code=%d stderr=%s", code, stderr)
				}
				var result runPlanOutput
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if result.Version != c.Expected.Version || result.PlanMD != c.Expected.PlanMD ||
					!reflect.DeepEqual(result.BaseVersion, c.Expected.BaseVersion) || result.IdenticalAfterFeedback != c.Expected.Identical {
					t.Fatalf("result=%+v expected=%+v", result, c.Expected)
				}
				if !diff || c.Expected.BaseVersion == nil {
					if result.Diff != nil {
						t.Fatalf("unexpected diff %q", *result.Diff)
					}
				} else if result.Diff == nil || (c.Expected.Identical && *result.Diff != "") || (!c.Expected.Identical && *result.Diff == "") {
					t.Fatalf("wrong diff %v", result.Diff)
				}
			}
		})
	}
}

func TestPlanBaseFeedIndexAndStrictSeq(t *testing.T) {
	m := []apitypes.MessageDTO{{Seq: 1, Kind: "text"}, planMessage(2, "a"), planFeedback(3), {Seq: 4, Kind: "text"}, planMessage(5, "b"), planFeedback(6), planMessage(7, "c")}
	if got := planBase(m, 4); got != 1 {
		t.Fatalf("index=%d", got)
	}
	for _, idx := range []int{-1, 0, 3, 8} {
		if got := planBase(m, idx); got != -1 {
			t.Fatalf("invalid index %d -> %d", idx, got)
		}
	}
	for _, seq := range []int32{2, 5} {
		m[2].Seq = seq
		if got := planBase(m, 4); got != -1 {
			t.Fatalf("boundary feedback %d -> %d", seq, got)
		}
	}
	if got := planBase(m, 6); got != 4 {
		t.Fatalf("nearest base=%d", got)
	}
}

func TestRunPlanSelectionAndVerbatim(t *testing.T) {
	first, latest := "# first\n", "\x1b[31m# %s\t**latest**\r\n\x00"
	feed := []apitypes.MessageDTO{planMessage(9, latest), planMessage(1, first)}
	env := fakeEnv(&uzicli.FakeClient{RunLogsHook: func(id string, after int32) ([]apitypes.MessageDTO, error) {
		if id != "r" || after != 0 {
			t.Fatalf("RunLogs(%q,%d)", id, after)
		}
		return feed, nil
	}})
	env.StdoutTTY = true
	out, stderr, code := runCLI(t, env, "run", "plan", "r")
	if code != 0 || stderr != "" || out != latest {
		t.Fatalf("out=%q stderr=%q code=%d", out, stderr, code)
	}
	if feed[0].Seq != 9 {
		t.Fatal("sorted client's feed in place")
	}
	out, stderr, code = runCLI(t, env, "run", "plan", "r", "--version", "1")
	if code != 0 || stderr != "" || out != first {
		t.Fatalf("historical out=%q stderr=%q code=%d", out, stderr, code)
	}
	for _, version := range []string{"0", "-1", "3"} {
		out, stderr, code = runCLI(t, env, "run", "plan", "r", "--version", version)
		if out != "" || code != uzicli.ExitUsage || !strings.Contains(stderr, "run has 2 plan version(s)") {
			t.Fatalf("version=%s out=%q err=%q code=%d", version, out, stderr, code)
		}
	}
	out, stderr, code = runCLI(t, planEnv(), "run", "plan", "r")
	if out != "" || code != uzicli.ExitNotFound || !strings.Contains(stderr, "run has no plan") {
		t.Fatalf("no plan: %q %q %d", out, stderr, code)
	}
	apiErr := uzicli.Exitf(uzicli.ExitUnreachable, "page failed")
	env = fakeEnv(&uzicli.FakeClient{RunLogsHook: func(string, int32) ([]apitypes.MessageDTO, error) { return nil, apiErr }})
	out, stderr, code = runCLI(t, env, "run", "plan", "r")
	if out != "" || code != uzicli.ExitUnreachable || !strings.Contains(stderr, "page failed") {
		t.Fatalf("API error: %q %q %d", out, stderr, code)
	}
}

func TestRunPlanDiffNotes(t *testing.T) {
	out, stderr, code := runCLI(t, planEnv(planMessage(1, "raw")), "run", "plan", "r", "--diff")
	if code != 0 || out != "raw" || stderr != "no revision base (no feedback preceded this version)\n" {
		t.Fatalf("%q %q %d", out, stderr, code)
	}
	out, stderr, code = runCLI(t, planEnv(planMessage(1, "same\n"), planFeedback(2), planMessage(3, "same\r\n\n")), "run", "plan", "r", "--diff")
	if code != 0 || out != "" || stderr != "This revision is identical to v1. Your requested changes were not applied.\n" {
		t.Fatalf("%q %q %d", out, stderr, code)
	}
	env := planEnv(planMessage(1, "alpha\nold\nomega\n"), planFeedback(2), planMessage(3, "alpha\nnew\nomega\n"))
	out, stderr, code = runCLI(t, env, "run", "plan", "r", "--diff")
	want := "--- v1\n+++ v2\n@@ -1,3 +1,3 @@\n alpha\n-old\n+new\n omega\n"
	if code != 0 || out != want || stderr != "" {
		t.Fatalf("%q %q %d", out, stderr, code)
	}
	out, stderr, code = runCLI(t, env, "run", "plan", "r", "--diff", "--json")
	var result runPlanOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || stderr != "" || result.Diff == nil || *result.Diff != want {
		t.Fatalf("%+v %q %d", result, stderr, code)
	}
}

func TestRunPlanSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		over       bool
	}{
		{"ASCII exact", strings.Repeat("a", 200*1024), false},
		{"ASCII over", strings.Repeat("a", 200*1024+1), true},
		{"Unicode exact", strings.Repeat("é", 100*1024), false},
		{"Unicode over", strings.Repeat("é", 100*1024) + "a", true},
		{"Original endings exact", strings.Repeat("a", 200*1024-2) + "\r\n", false},
		{"Original endings over", strings.Repeat("a", 200*1024) + "\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, base := range []bool{false, true} {
				msgs := []apitypes.MessageDTO{planMessage(1, tc.text)}
				if base {
					msgs = append(msgs, planFeedback(2), planMessage(3, tc.text))
				}
				for _, jsonFlag := range []bool{false, true} {
					args := []string{"run", "plan", "r", "--diff"}
					if jsonFlag {
						args = append(args, "--json")
					}
					out, stderr, code := runCLI(t, planEnv(msgs...), args...)
					if tc.over {
						if code != uzicli.ExitUsage || out != "" || strings.Count(stderr, "plan too large to compare (limit 200 KiB)") != 1 {
							t.Fatalf("%d %q %q", code, out, stderr)
						}
					} else if code != 0 {
						t.Fatalf("%d %q", code, stderr)
					}
				}
			}
			out, stderr, code := runCLI(t, planEnv(planMessage(1, tc.text)), "run", "plan", "r")
			if code != 0 || stderr != "" || out != tc.text {
				t.Fatal("plain output size/bytes changed")
			}
		})
	}
	out, stderr, code := runCLI(t, planEnv(planMessage(1, strings.Repeat("x", 200*1024+1)), planFeedback(2), planMessage(3, "small")), "run", "plan", "r", "--diff")
	if code != uzicli.ExitUsage || out != "" || !strings.Contains(stderr, "plan too large") {
		t.Fatalf("base cap: %q %q %d", out, stderr, code)
	}
}

func TestRunPlanInvalidPayload(t *testing.T) {
	for _, payload := range []string{"{", `{}`, `{"plan_md":null}`, `{"plan_md":4}`, `{"plan_md":false}`, `{"plan_md":[]}`, `{"plan_md":{}}`, "null", "[]"} {
		t.Run(payload, func(t *testing.T) {
			m := apitypes.MessageDTO{Seq: 1, Kind: "plan", Payload: json.RawMessage(payload)}
			for _, args := range [][]string{{"run", "plan", "r"}, {"run", "plan", "r", "--json"}, {"run", "plan", "r", "--diff"}} {
				out, stderr, code := runCLI(t, planEnv(m), args...)
				if code == 0 || out != "" || !strings.Contains(stderr, "invalid plan payload") {
					t.Fatalf("%q %q %d", out, stderr, code)
				}
			}
			out, stderr, code := runCLI(t, planEnv(m, planFeedback(2), planMessage(3, "valid")), "run", "plan", "r", "--json")
			if code == 0 || out != "" || !strings.Contains(stderr, "invalid plan payload") {
				t.Fatalf("base: %q %q %d", out, stderr, code)
			}
		})
	}
	out, stderr, code := runCLI(t, planEnv(planMessage(1, "")), "run", "plan", "r")
	if code != 0 || out != "" || stderr != "" {
		t.Fatalf("empty string: %q %q %d", out, stderr, code)
	}
}

type planFailWriter struct{ err error }

func (w planFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunPlanWriterErrors(t *testing.T) {
	boom := errors.New("write failed")
	for _, tc := range []struct {
		name     string
		messages []apitypes.MessageDTO
		args     []string
		stderr   bool
	}{
		{"raw", []apitypes.MessageDTO{planMessage(1, "raw")}, nil, false},
		{"json", []apitypes.MessageDTO{planMessage(1, "raw")}, []string{"--json"}, false},
		{"diff", []apitypes.MessageDTO{planMessage(1, "a"), planFeedback(2), planMessage(3, "b")}, []string{"--diff"}, false},
		{"no base", []apitypes.MessageDTO{planMessage(1, "a")}, []string{"--diff"}, true},
		{"identical", []apitypes.MessageDTO{planMessage(1, "a"), planFeedback(2), planMessage(3, "a")}, []string{"--diff", "--json"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := planEnv(tc.messages...)
			if tc.stderr {
				env.Stderr = planFailWriter{boom}
			} else {
				env.Stdout = planFailWriter{boom}
			}
			root := newRootCmd(env)
			root.SetArgs(append([]string{"run", "plan", "r"}, tc.args...))
			if err := root.Execute(); !errors.Is(err, boom) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestPlanUnifiedDiffLiterals(t *testing.T) {
	for _, tc := range []struct{ name, base, target, want string }{
		{"insert empty", "", "a\nb", "--- v1\n+++ v2\n@@ -0,0 +1,2 @@\n+a\n+b\n"},
		{"delete empty", "a\nb", "", "--- v1\n+++ v2\n@@ -1,2 +0,0 @@\n-a\n-b\n"},
		{"empty", "", "\r\n", ""},
		{"equal endings", "a\r\n", "a\n\r", ""},
		{"blank line", "a\n\nb", "a\nx\nb", "--- v1\n+++ v2\n@@ -1,3 +1,3 @@\n a\n-\n+x\n b\n"},
		{"spaces unicode", "é \n界\r\nend", "é\n界\r\nend\n", "--- v1\n+++ v2\n@@ -1,3 +1,3 @@\n-é \n+é\n 界\r\n end\n"},
		{"beginning insert", "a\nb\nc\nd\ne", "x\na\nb\nc\nd\ne", "--- v1\n+++ v2\n@@ -1,3 +1,4 @@\n+x\n a\n b\n c\n"},
		{"end delete", "a\nb\nc\nd\ne", "a\nb\nc\nd", "--- v1\n+++ v2\n@@ -2,4 +2,3 @@\n b\n c\n d\n-e\n"},
		{"touching windows", "old\n1\n2\n3\n4\n5\n6\nlast", "new\n1\n2\n3\n4\n5\n6\nend", "--- v1\n+++ v2\n@@ -1,8 +1,8 @@\n-old\n+new\n 1\n 2\n 3\n 4\n 5\n 6\n-last\n+end\n"},
		{"overlapping windows", "old\n1\n2\n3\n4\nlast", "new\n1\n2\n3\n4\nend", "--- v1\n+++ v2\n@@ -1,6 +1,6 @@\n-old\n+new\n 1\n 2\n 3\n 4\n-last\n+end\n"},
		{"separate windows", "old\n1\n2\n3\n4\n5\n6\n7\nlast", "new\n1\n2\n3\n4\n5\n6\n7\nend", "--- v1\n+++ v2\n@@ -1,4 +1,4 @@\n-old\n+new\n 1\n 2\n 3\n@@ -6,4 +6,4 @@\n 5\n 6\n 7\n-last\n+end\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				if got := planUnifiedDiff(tc.base, tc.target, 1, 2); got != tc.want {
					t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
				}
			}
		})
	}
}

// Thousands of unique lines exercise line tokens beyond a byte and beyond the
// UTF-16 surrogate range; the literal final hunk also checks decoded Unicode.
func TestPlanUnifiedDiffManyLines(t *testing.T) {
	var base strings.Builder
	for i := 0; i < 70000; i++ {
		fmt.Fprintf(&base, "%x\n", i)
	}
	target := base.String() + "界\n"
	want := "--- v1\n+++ v2\n@@ -69998,3 +69998,4 @@\n 1116d\n 1116e\n 1116f\n+界\n"
	if got := planUnifiedDiff(base.String(), target, 1, 2); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRunPlanJSONNulls(t *testing.T) {
	out, stderr, code := runCLI(t, planEnv(planMessage(1, "")), "run", "plan", "r", "--json")
	if code != 0 || stderr != "" || out != "{\"version\":1,\"base_version\":null,\"plan_md\":\"\",\"diff\":null,\"identical_after_feedback\":false}\n" {
		t.Fatalf("%q %q %d", out, stderr, code)
	}
	env := planEnv(planMessage(1, "same"), planFeedback(2), planMessage(3, "same\r\n"))
	out, stderr, code = runCLI(t, env, "run", "plan", "r", "--json")
	var result runPlanOutput
	if err := json.NewDecoder(bytes.NewBufferString(out)).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || stderr != "" || !result.IdenticalAfterFeedback || result.Diff != nil || result.BaseVersion == nil || *result.BaseVersion != 1 {
		t.Fatalf("%+v %q %d", result, stderr, code)
	}
}
