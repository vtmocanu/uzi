package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfirmImageRollDrainHTTP(t *testing.T) {
	for _, mode := range []string{"present", "missing", "duplicate", "poll-error", "write-error"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing auth")
				}
				switch r.URL.Path {
				case "/api/controller/workers/11111111-1111-1111-1111-111111111111/drain":
					body, _ := io.ReadAll(r.Body)
					if r.Method != http.MethodPost || string(body) != `{"reason":"image_roll"}` {
						t.Error("incorrect drain request")
					}
					writes++
					if mode == "write-error" {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(204)
				case "/api/controller/poll":
					if writes != 1 || r.Method != http.MethodGet {
						t.Error("poll before confirmed write")
					}
					if mode == "poll-error" {
						w.WriteHeader(500)
						return
					}
					body := `{"workers":[{"id":"11111111-1111-1111-1111-111111111111","generation":7,"size":"M","custody_held":true}]}`
					if mode == "missing" {
						body = `{"workers":[]}`
					}
					if mode == "duplicate" {
						body = `{"workers":[{"id":"11111111-1111-1111-1111-111111111111"},{"id":"11111111-1111-1111-1111-111111111111"}]}`
					}
					_, _ = io.WriteString(w, body)
				default:
					t.Error("unexpected path")
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			got, err := New(srv.URL, "test-token", time.Second, nil, testLogger()).ConfirmImageRollDrain(context.Background(), "11111111-1111-1111-1111-111111111111")
			if mode != "present" {
				if err == nil {
					t.Fatal("uncertain confirmation accepted")
				}
				return
			}
			if err != nil || got.ID != "11111111-1111-1111-1111-111111111111" || got.Generation != 7 || !got.CustodyHeld {
				t.Fatalf("fresh desired shape not returned: %v", err)
			}
		})
	}
}
