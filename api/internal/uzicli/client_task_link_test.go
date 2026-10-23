package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientTaskContinuedIssue(t *testing.T) {
	iid := int64(42)
	for _, link := range []*int64{nil, &iid} {
		t.Run(map[bool]string{true: "linked", false: "omitted"}[link != nil], func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/repos/repo/task-runs" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				value, ok := body["continued_issue_iid"]
				if ok != (link != nil) {
					t.Errorf("link presence=%v", ok)
				}
				if link != nil && string(value) != "42" {
					t.Errorf("link=%s", value)
				}
				writeJSON(w, map[string]any{"run": map[string]any{"id": "r", "kind": "task", "issue_iid": nil, "continued_issue_iid": link}})
			}))
			defer srv.Close()
			run, err := newTestClient(srv).CreateTaskRun(context.Background(), "repo", "task", "", false, false, false, false, link)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || run.IssueIID != nil || (run.ContinuedIssueIID != nil) != (link != nil) {
				t.Fatalf("calls=%d run=%+v", calls, run)
			}
		})
	}
}
