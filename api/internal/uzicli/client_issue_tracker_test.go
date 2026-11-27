package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueTrackerConnectionsWire(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[{"id":"c1","kind":"youtrack","base_url":"https://yt.example.com","account_id":"1-1","account_login":"bot","created_at":"2026-11-01T09:00:00Z","last_verified_at":"2026-11-18T10:30:00Z"}]}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).ListIssueTrackerConnections(context.Background())
	if err != nil || len(got) != 1 || got[0].Kind != "youtrack" || got[0].AccountLogin != "bot" || got[0].LastVerifiedAt.IsZero() {
		t.Fatalf("decoded %+v, err %v", got, err)
	}
	if gotPath != "/api/issue-trackers/connections" || gotAuth != "Bearer uzc_test" {
		t.Errorf("path %q auth %q", gotPath, gotAuth)
	}
}

func TestIssueTrackerStatesWireAndStatus(t *testing.T) {
	var gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"mapping":{"open_state":"Open","closed_state":"Done","columns":[{"label_name":"Blocked","state":"Open"}]},"states":[{"name":"Done","resolved":true,"archived":false,"closable":true,"openable":false}]}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).GetIssueTrackerStates(context.Background(), "r/1")
	if err != nil || got.Mapping == nil || got.Mapping.ClosedState != "Done" || len(got.Mapping.Columns) != 1 || len(got.States) != 1 || !got.States[0].Closable {
		t.Fatalf("decoded %+v, err %v", got, err)
	}
	if gotPath != "/api/repos/r%2F1/issue-tracker-link/states" {
		t.Errorf("path = %q", gotPath)
	}
	for code, want := range map[int]int{http.StatusNotFound: ExitNotFound, http.StatusConflict: ExitConflict} {
		status = code
		_, err := newTestClient(srv).GetIssueTrackerStates(context.Background(), "x")
		if ee, ok := err.(*ExitError); !ok || ee.Code != want {
			t.Errorf("status %d: err = %v, want exit %d", code, err, want)
		}
	}
}
