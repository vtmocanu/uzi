package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PRD #3454: run-create replies carry the server's optional notice, and the admin maintenance
// readout is a plain GET.
func TestCreateRunAndTaskRunDecodeNotice(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"with notice", `{"run":{"id":"run-1","status":"queued","kind":"issue"},"notice":"queued by maintenance"}`, "queued by maintenance"},
		{"without notice", `{"run":{"id":"run-1","status":"queued","kind":"issue"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			run, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil, nil, false, nil, nil, "")
			if err != nil || run.ID != "run-1" || run.Notice != tc.want {
				t.Fatalf("CreateRun = %+v, %v; want notice %q", run, err, tc.want)
			}
			task, err := newTestClient(srv).CreateTaskRun(context.Background(), "p1", "ctx", "", false, false, false, false, nil)
			if err != nil || task.ID != "run-1" || task.Notice != tc.want {
				t.Fatalf("CreateTaskRun = %+v, %v; want notice %q", task, err, tc.want)
			}
		})
	}
}

func TestAdminMaintenanceRequest(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = io.WriteString(w, `{"state":{"active":true,"since":"2026-10-26T10:00:00Z","reason":"drain","by_label":"Ada"},`+
			`"readout":{"as_of":"2026-10-26T10:30:00Z","clear":false,"active":{"count":1,"items":[]},"held":{"count":0,"items":[]},`+
			`"custody":{"count":0,"items":[]},"queued":{"count":0,"items":[]},"idle_bound_workers":{"count":0,"items":[]}}}`)
	}))
	defer srv.Close()
	doc, err := newTestClient(srv).AdminMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/admin/maintenance" {
		t.Errorf("request = %s %s", method, path)
	}
	if !doc.State.Active || doc.Readout.Clear || doc.Readout.Active.Count != 1 || doc.State.ByLabel == nil || *doc.State.ByLabel != "Ada" {
		t.Errorf("decoded = %+v", doc)
	}
}
