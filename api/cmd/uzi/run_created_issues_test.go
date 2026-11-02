package main

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func createdIssuesRunGet(t *testing.T, run apitypes.RunDTO, args ...string) string {
	t.Helper()
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run", "get", "r1"}, args...)...)
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d: %s", code, stderr)
	}
	return out
}

func createdIssuesCells(out string) []string {
	var cells []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "CREATED_ISSUE" {
			cells = append(cells, strings.TrimSpace(strings.TrimPrefix(line, "CREATED_ISSUE")))
		}
	}
	return cells
}

func TestRunGetCreatedIssuesHuman(t *testing.T) {
	sourceIID, mrIID, branch := int64(19), int64(91), "uzi/source-19"
	run := apitypes.RunDTO{
		ID: "r1", Kind: "prompt", Status: "completed", ForgeType: "gitlab",
		IssueIID: &sourceIID, IssueTitle: "Original source issue", MrIID: &mrIID, Branch: &branch,
		CreatedIssues: []apitypes.CreatedIssueDTO{
			{IID: 42, Title: "First output", WebURL: "https://forge.example/prefix/group/sub/repo/-/issues/42"},
			{IID: 43, Title: "Second output", WebURL: "https://forge.example/prefix/group/sub/repo/-/issues/43"},
			{IID: 44},
		},
	}
	out := createdIssuesRunGet(t, run)
	// Keep the coordinate assertion independent of titles and URLs: disabling only
	// the created-issue render seam must fail this named regression.
	for _, coordinate := range []string{"Created issue #42", "Created issue #43", "Created issue #44"} {
		if !strings.Contains(out, coordinate) {
			t.Fatalf("missing coordinate %q in run get:\n%s", coordinate, out)
		}
	}
	want := []string{
		"Created issue #42 First output https://forge.example/prefix/group/sub/repo/-/issues/42",
		"Created issue #43 Second output https://forge.example/prefix/group/sub/repo/-/issues/43",
		"Created issue #44",
	}
	if got := createdIssuesCells(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("created issue cells = %q, want %q", got, want)
	}
	for key, want := range map[string]string{"TITLE": "Original source issue", "MR": "91", "BRANCH": branch} {
		if got := strings.Join(strings.Fields(lineWith(t, out, key))[1:], " "); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("run get fabricated a terminal escape")
	}
	var decoded apitypes.RunDTO
	if err := json.Unmarshal([]byte(createdIssuesRunGet(t, run, "--json")), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.IssueIID == nil || *decoded.IssueIID != sourceIID || decoded.IssueTitle != run.IssueTitle ||
		decoded.MrIID == nil || *decoded.MrIID != mrIID || decoded.Branch == nil || *decoded.Branch != branch {
		t.Fatalf("source issue and MR/branch changed: %+v", decoded)
	}
}

func TestRunGetCreatedIssuesAbsent(t *testing.T) {
	var oldAPI apitypes.RunDTO
	if err := json.Unmarshal([]byte(`{"id":"r1","kind":"prompt","status":"completed"}`), &oldAPI); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]apitypes.RunDTO{
		"old API": oldAPI,
		"nil":     {ID: "r1"},
		"empty":   {ID: "r1", CreatedIssues: []apitypes.CreatedIssueDTO{}},
	} {
		t.Run(name, func(t *testing.T) {
			out := createdIssuesRunGet(t, run)
			if strings.Contains(out, "CREATED_ISSUE") || strings.Contains(out, "Created issue #") {
				t.Fatalf("absent created issues rendered rows: %s", out)
			}
		})
	}
}

func TestRunGetCreatedIssuesURLs(t *testing.T) {
	valid := []string{
		"https://github.com/owner/repo/issues/42",
		"https://forge.example/prefix/group/sub/repo/-/issues/42",
		"https://forge.example:8443/a%20b/caf%C3%A9/file%2Ename/issues/42",
		"https://[2001:db8::1]:443/team/repo/issues/42",
		"https://192.0.2.1/team/repo/issues/42",
		"https://forge.example./team/repo/issues/42",
	}
	invalid := []string{
		"", "http://forge.example/a/issues/42", "//forge.example/a/issues/42",
		"https:forge.example/a/issues/42", "https:///a/issues/42",
		"https://user:pass@forge.example/a/issues/42",
		"https://forge.example/a/issues/42?", "https://forge.example/a/issues/42#",
		"https://forge.example/a/issues/42?x=y", "https://forge.example/a/issues/42#frag",
		"https://bad_host/a/issues/42", "https://-bad.example/a/issues/42",
		"https://bad-.example/a/issues/42", "https://bad..example/a/issues/42",
		"https://[bad]/a/issues/42", "https://forge.example:/a/issues/42",
		"https://forge.example:0/a/issues/42", "https://forge.example:65536/a/issues/42",
		"https://forge.example:abc/a/issues/42",
		"https://forge.example/a\\b/issues/42", "https://forge.example/a%5Cb/issues/42",
		"https://forge.example/a%2Fb/issues/42", "https://forge.example/a%2fb/issues/42",
		"https://forge.example/./issues/42", "https://forge.example/a/../issues/42",
		"https://forge.example/%2E/issues/42", "https://forge.example/.%2e/issues/42",
		"https://forge.example/a//issues/42", "https://forge.example/a%zz/issues/42",
		"https://forge.example/a%/issues/42", "https://forge.example/a%252f/issues/42",
		"https://forge.example/a%25/issues/42", "https://forge.example/a%00/issues/42",
		"https://forge.example/a%1b/issues/42", "https://forge.example/a%7f/issues/42",
		"https://forge.example/a%C2%85/issues/42", "https://forge.example/a%E2%80%AE/issues/42",
		"https://forge.example/a%FF/issues/42", "https://forge.example/a b/issues/42",
		"https://forge.example/a\nb/issues/42", "https://forge.example/a\tb/issues/42",
		"https://forge.example/a\u202Eb/issues/42", "https://forge.example/a\u200Bb/issues/42",
		"https://forge.example/a\x1b]8;;https://evil.example\a/issues/42",
		"https://" + strings.Repeat("a", 64) + ".example/issues/42",
		"https://" + strings.Repeat("abcd.", 51) + "example/issues/42",
	}
	for _, tc := range []struct {
		name string
		urls []string
		safe bool
	}{{"valid", valid, true}, {"unsafe", invalid, false}} {
		for i, raw := range tc.urls {
			t.Run(tc.name+"/"+strconv.Itoa(i), func(t *testing.T) {
				run := apitypes.RunDTO{ID: "r1", CreatedIssues: []apitypes.CreatedIssueDTO{{IID: 42, Title: "Output", WebURL: raw}}}
				out := createdIssuesRunGet(t, run)
				want := "Created issue #42 Output"
				if tc.safe {
					want += " " + raw
				}
				if got := createdIssuesCells(out); !reflect.DeepEqual(got, []string{want}) {
					t.Fatalf("URL %q: cells = %q, want %q", raw, got, want)
				}
			})
		}
	}
}

func TestRunGetCreatedIssuesSanitizesTitlesAndPreservesJSON(t *testing.T) {
	run := apitypes.RunDTO{ID: "r1", CreatedIssues: []apitypes.CreatedIssueDTO{
		{IID: 42, Title: "one\ttwo\nthree\u202E\u200B\x1b]8;;https://evil.example\aSAFE\x1b]8;;\a", WebURL: "https://forge.example/a/issues/42"},
		{IID: 43, Title: "only\x00\x7f\u2066text", WebURL: "https://forge.example/a\u202Eb/issues/43"},
	}}
	out := createdIssuesRunGet(t, run)
	want := []string{
		"Created issue #42 one two three]8;;https://evil.exampleSAFE]8;; https://forge.example/a/issues/42",
		"Created issue #43 onlytext",
	}
	if got := createdIssuesCells(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized cells = %q, want %q", got, want)
	}
	var decoded apitypes.RunDTO
	if err := json.Unmarshal([]byte(createdIssuesRunGet(t, run, "--json")), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.CreatedIssues, run.CreatedIssues) {
		t.Fatalf("--json changed created issues: got %+v, want %+v", decoded.CreatedIssues, run.CreatedIssues)
	}
}
