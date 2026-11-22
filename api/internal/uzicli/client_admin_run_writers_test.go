package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const writersRepo = "0b8b6a8a-0000-4000-8000-000000000009"

func TestAdminRunWritersRequestAndDecode(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"writers":[{"run_id":"r1","kind":"ci_fix","status":"running","owner_id":"u1","repo_id":"`+writersRepo+`",`+
		`"pipeline_ref":"agent/x","mr_iid":null,"created_at":"2026-01-02T03:04:05Z"}],"complete":true}`)
	c := newTestClient(srv)
	out, err := c.AdminRunWriters(context.Background(), writersRepo, "a/b;c&d", 0)
	if err != nil {
		t.Fatalf("AdminRunWriters: %v", err)
	}
	want := "/api/admin/run-writers?branch=a%2Fb%3Bc%26d&repo=" + writersRepo
	if uri != want {
		t.Errorf("uri = %q, want %q", uri, want)
	}
	if !out.Complete || len(out.Writers) != 1 || out.Writers[0].RunID != "r1" || out.Writers[0].MRIID != nil {
		t.Errorf("out = %+v", out)
	}
	if _, err := c.AdminRunWriters(context.Background(), writersRepo, "main", 42); err != nil {
		t.Fatal(err)
	}
	if want := "/api/admin/run-writers?branch=main&mr_iid=42&repo=" + writersRepo; uri != want {
		t.Errorf("uri = %q, want %q", uri, want)
	}
}

func TestAdminRunWritersEmptyAndIncomplete(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"writers":[],"complete":false}`)
	out, err := newTestClient(srv).AdminRunWriters(context.Background(), writersRepo, "main", 0)
	if err != nil || out.Complete || out.Writers == nil || len(out.Writers) != 0 {
		t.Errorf("out=%+v err=%v; want empty non-nil writers, complete=false", out, err)
	}
}

func TestAdminRunWritersStrictDecode(t *testing.T) {
	for name, body := range map[string]string{
		"array":            `[]`,
		"garbage":          `not json`,
		"null body":        `null`,
		"missing complete": `{"writers":[]}`,
		"complete string":  `{"writers":[],"complete":"true"}`,
		"complete null":    `{"writers":[],"complete":null}`,
		"complete number":  `{"writers":[],"complete":1}`,
		"writers null":     `{"writers":null,"complete":true}`,
		"writers missing":  `{"complete":true}`,
		"writers object":   `{"writers":{},"complete":true}`,
		"bad element":      `{"writers":[{"run_id":5}],"complete":true}`,
		// Case-variant duplicates must not shadow or override the exact keys.
		"writers case dup":  `{"writers":[{"run_id":"r1","kind":"ci_fix","status":"running","owner_id":"u1","repo_id":"x","pipeline_ref":"b","created_at":"2026-01-02T03:04:05Z"}],"complete":true,"WRITERS":[]}`,
		"complete case dup": `{"writers":[],"complete":false,"Complete":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			var uri string
			srv := serveJSON(t, &uri, body)
			_, err := newTestClient(srv).AdminRunWriters(context.Background(), writersRepo, "main", 0)
			if ExitCodeFor(err) != ExitGeneric {
				t.Errorf("err=%v code=%d, want ExitGeneric", err, ExitCodeFor(err))
			}
		})
	}
}

func TestAdminRunWritersStatusMapping(t *testing.T) {
	for status, want := range map[int]int{
		401: ExitAuth, 403: ExitAuth, 404: ExitNotFound, 400: ExitUsage, 422: ExitUsage, 500: ExitUnreachable,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		_, err := newTestClient(srv).AdminRunWriters(context.Background(), writersRepo, "main", 0)
		srv.Close()
		if got := ExitCodeFor(err); got != want {
			t.Errorf("status %d: exit %d, want %d (%v)", status, got, want, err)
		}
	}
}
