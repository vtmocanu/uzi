package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyResumeGuidanceAndPresentedToken(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "stale"}[conflict], func(t *testing.T) {
			gets, posts := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets++
					t.Error("unexpected GET: action token must come from caller")
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				posts++
				body, _ := io.ReadAll(r.Body)
				if r.Header.Get("If-Match") != `"presented"` || string(body) != `{"guidance":"Narrow scope"}` {
					t.Errorf("resume header=%q body=%s", r.Header.Get("If-Match"), body)
				}
				if conflict {
					w.WriteHeader(409)
					_, _ = w.Write([]byte(`{"error":"changed"}`))
					return
				}
				_, _ = w.Write([]byte(`{"run":{"status":"queued"}}`))
			}))
			defer srv.Close()
			_, err := newTestClient(srv).ResumeRunNowWithOptions(context.Background(), "r1", RunResumeOptions{Guidance: "Narrow scope", ActionIfMatch: `"presented"`})
			if (err != nil) != conflict || gets != 0 || posts != 1 {
				t.Fatalf("err=%v gets=%d posts=%d", err, gets, posts)
			}
		})
	}
}

func TestPolicyResumeLegacyBodylessCompatibility(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || strings.TrimSpace(string(body)) != "" || r.Header.Get("If-Match") != "" {
			t.Errorf("legacy resume changed: %s %q", r.Method, body)
		}
		_, _ = w.Write([]byte(`{"run":{"status":"queued"}}`))
	}))
	defer srv.Close()
	if _, err := newTestClient(srv).ResumeRunNow(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
}
