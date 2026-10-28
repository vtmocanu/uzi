package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestRenderRunDetailRepoPath(t *testing.T) {
	for _, path := range []string{"", "team/project", "team\x1b/project\u202e"} {
		out := renderDetail(t, apitypes.RunDTO{ID: "r1", Status: "completed", RepoPath: path})
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "REPO ") {
				found = true
				if strings.Join(strings.Fields(line), " ") != "REPO team/project" {
					t.Fatalf("unsafe or incorrect repo row: %q", line)
				}
			}
		}
		if found != (path != "") {
			t.Fatalf("repo row present=%v for path %q: %s", found, path, out)
		}
	}
}
