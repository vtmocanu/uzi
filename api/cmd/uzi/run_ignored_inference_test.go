package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestRenderRunIgnoredInference(t *testing.T) {
	for _, caps := range [][]string{nil, {"jvm"}, {"jv\x1bm\u202e"}} {
		out := renderDetail(t, apitypes.RunDTO{ID: "r1", Status: "running", IgnoredInferredCapabilities: caps})
		if len(caps) == 0 {
			if strings.Contains(out, "IGNORED_INFERRED_CAPABILITIES") {
				t.Fatalf("empty policy rendered: %s", out)
			}
		} else if !strings.Contains(out, "IGNORED_INFERRED_CAPABILITIES") || !strings.Contains(out, "jvm") || strings.ContainsAny(out, "\x1b\u202e") {
			t.Fatalf("missing or unsafe policy: %q", out)
		}
	}
}
