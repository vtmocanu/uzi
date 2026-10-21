package forge

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestGitLabGetMergeRequestOversizedBodyRegression is the red/green regression for
// #3038: GetMergeRequest must refuse a merge request response past the 32 MiB
// ceiling instead of decoding it into memory. It uses only helpers that exist on
// the base commit and asserts nothing that needs the fix's own symbols, so it
// compiles and fails (rather than failing to build) before the fix.
func TestGitLabGetMergeRequestOversizedBodyRegression(t *testing.T) {
	const total = 33 << 20
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/9": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"id":1009,"iid":9,"state":"merged","description":"`)); err != nil {
				return
			}
			chunk := bytes.Repeat([]byte("a"), 64<<10)
			for written := 0; written < total; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
			_, _ = w.Write([]byte(`"}`))
		},
	})
	d := newTestDriver(t, m, "glpat-"+"Zq7xKm"+"Wp3NrT"+"5vBc9Y"+"dLh2")

	mr, err := d.GetMergeRequest(context.Background(), 7, 9)
	if err == nil {
		t.Fatalf("GetMergeRequest decoded a ~33 MiB response, want a size refusal; got %+v", mr)
	}
	if !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("error = %q, want the size-limit refusal", err.Error())
	}
	if mr != (MergeRequest{}) {
		t.Fatalf("a refused response must return the zero MergeRequest, got %+v", mr)
	}
}
