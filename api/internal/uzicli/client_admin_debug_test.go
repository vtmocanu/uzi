package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #4198: the HTTP client side of the admin run debug reads. Each test pins the request the
// server sees and how the reply (or an older server's reply) is decoded.

func serveJSON(t *testing.T, got *string, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunInputsMetaReportsAdminDebugMarker(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"inputs":[{"id":3,"kind":"follow_up","body":"hi","created_at":"2026-09-13T00:00:00Z",`+
		`"consumed_at":null,"disposition":null,"applied_at":null,"included_at":null,"inclusion_reported":false}],"admin_debug_read":true}`)
	in, debug, err := newTestClient(srv).RunInputsMeta(context.Background(), "r1")
	if err != nil {
		t.Fatalf("RunInputsMeta: %v", err)
	}
	if uri != "/api/runs/r1/inputs" || !debug || len(in) != 1 || in[0].ID != 3 {
		t.Errorf("uri=%q debug=%v inputs=%+v; want the inputs path, marker true, one input", uri, debug, in)
	}
}

func TestRunInputsMetaWithoutMarkerAndRunInputsUnchanged(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"inputs":[]}`)
	c := newTestClient(srv)
	if _, debug, err := c.RunInputsMeta(context.Background(), "r1"); err != nil || debug {
		t.Errorf("RunInputsMeta without marker: debug=%v err=%v; want false, nil", debug, err)
	}
	in, err := c.RunInputs(context.Background(), "r1")
	if err != nil || len(in) != 0 {
		t.Errorf("RunInputs = %+v, %v; want empty, nil", in, err)
	}
}

func TestRecoveryHoldsScopedSendsExactlyOneEscapedSelector(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"owner_id":"u 1","aggregate":{},"holds":[]}`)
	c := newTestClient(srv)
	out, err := c.RecoveryHoldsScoped(context.Background(), "u 1&x", "")
	if err != nil {
		t.Fatalf("user scope: %v", err)
	}
	if uri != "/api/recovery/holds?user=u+1%26x" || out.OwnerID != "u 1" {
		t.Errorf("user scope: uri=%q owner=%q", uri, out.OwnerID)
	}
	if _, err := c.RecoveryHoldsScoped(context.Background(), "", "run/1"); err != nil {
		t.Fatalf("run scope: %v", err)
	}
	if uri != "/api/recovery/holds?run=run%2F1" {
		t.Errorf("run scope: uri=%q", uri)
	}
	uri = ""
	for _, args := range [][2]string{{"", ""}, {"u", "r"}} {
		if _, err := c.RecoveryHoldsScoped(context.Background(), args[0], args[1]); ExitCodeFor(err) != ExitUsage {
			t.Errorf("selectors %q: err=%v, want a usage error", args, err)
		}
	}
	if uri != "" {
		t.Errorf("an invalid selector pair reached the server: %q", uri)
	}
}

func TestAdminListRunsDebugRequestAndDecode(t *testing.T) {
	var uri string
	srv := serveJSON(t, &uri, `{"runs":null,"truncated":true}`)
	c := newTestClient(srv)
	out, err := c.AdminListRunsDebug(context.Background(), "", "")
	if err != nil {
		t.Fatalf("AdminListRunsDebug: %v", err)
	}
	if uri != "/api/admin/runs?state=all" || !out.Truncated || out.Runs == nil || len(out.Runs) != 0 {
		t.Errorf("uri=%q out=%+v; want the bare state=all query, truncated, a non-nil empty runs", uri, out)
	}
	if _, err := c.AdminListRunsDebug(context.Background(), "0b8b6a8a-0000-4000-8000-000000000001", "2026-01-02T03:04:05Z"); err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if uri != "/api/admin/runs?since=2026-01-02T03%3A04%3A05Z&state=all&user=0b8b6a8a-0000-4000-8000-000000000001" {
		t.Errorf("filtered uri = %q", uri)
	}
}

func TestAdminListRunsDebugRefusesAnOlderServer(t *testing.T) {
	for name, body := range map[string]string{
		"bare array":        `[]`,
		"active envelope":   `{"runs":[]}`,
		"array with a row":  `[{"id":"r1"}]`,
		"envelope with row": `{"runs":[{"id":"r1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var uri string
			srv := serveJSON(t, &uri, body)
			_, err := newTestClient(srv).AdminListRunsDebug(context.Background(), "", "")
			if err == nil || !strings.Contains(err.Error(), "this server does not support admin run debug listing (needs a server with UZI_ADMIN_RUN_DEBUG support)") {
				t.Errorf("err = %v, want the unsupported-server error", err)
			}
		})
	}
}

func TestAdminListRunsDebugForbiddenIsAuthExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"admin run debug reads require the server flag"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).AdminListRunsDebug(context.Background(), "", "")
	if ExitCodeFor(err) != ExitAuth || !strings.Contains(err.Error(), "admin run debug reads require the server flag") {
		t.Errorf("err = %v (exit %d), want the server message at ExitAuth", err, ExitCodeFor(err))
	}
}
