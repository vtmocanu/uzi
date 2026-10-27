package uzicli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestManualCordonMalformedResponse(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, body := range []string{`{}`, `null`, `{"worker":null}`, `{"worker":{}}`, `{"worker":`} {
			t.Run(method+body, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				c := &HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}
				var got apitypes.WorkerDTO
				var err error
				if method == http.MethodPost {
					got, err = c.CordonWorker(context.Background(), "id", "")
				} else {
					got, err = c.UncordonWorker(context.Background(), "id")
				}
				if err == nil || got.ID != "" {
					t.Fatalf("malformed success acknowledged: worker=%+v err=%v", got, err)
				}
			})
		}
	}
}

func TestManualCordonHTTP(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, reason := range []string{"", "維持😀\n\u202e\u001b]0;hostile\a"} {
			t.Run(method+reason, func(t *testing.T) {
				id := "a/b ?😀"
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != method || r.URL.EscapedPath() != "/api/workers/"+url.PathEscape(id)+"/cordon" {
						t.Errorf("request %s %s", r.Method, r.URL.EscapedPath())
					}
					if r.Header.Get("Authorization") != "Bearer fixture" {
						t.Error("missing auth")
					}
					body, _ := io.ReadAll(r.Body)
					if method == http.MethodDelete {
						if len(body) != 0 {
							t.Errorf("DELETE body %q", body)
						}
					} else {
						var got map[string]string
						if err := json.Unmarshal(body, &got); err != nil {
							t.Error(err)
						}
						v, present := got["reason"]
						if v != reason || present != (reason != "") {
							t.Errorf("reason %q present=%v", v, present)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"worker": apitypes.WorkerDTO{ID: "updated", Status: "offline", EffectiveCordoned: true}})
				}))
				defer srv.Close()
				c := &HTTPClient{BaseURL: srv.URL, Token: "fixture", HTTP: srv.Client()}
				var got apitypes.WorkerDTO
				var err error
				if method == http.MethodPost {
					got, err = c.CordonWorker(context.Background(), id, reason)
				} else {
					got, err = c.UncordonWorker(context.Background(), id)
				}
				if err != nil || got.ID != "updated" || !got.EffectiveCordoned || got.Status != "offline" {
					t.Fatalf("worker=%+v err=%v", got, err)
				}
			})
		}
	}
}

func TestManualCordonHTTPErrors(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, status := range []int{401, 403, 404, 409, 500} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				message := "manual cordon is only supported for persistent workers"
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
				}))
				defer srv.Close()
				c := &HTTPClient{BaseURL: srv.URL, Token: "fixture", HTTP: srv.Client()}
				var err error
				if method == http.MethodPost {
					_, err = c.CordonWorker(context.Background(), "id", "")
				} else {
					_, err = c.UncordonWorker(context.Background(), "id")
				}
				if err == nil {
					t.Fatal("wanted failure")
				}
				if status == 409 && (ExitCodeFor(err) != ExitConflict || !strings.Contains(err.Error(), message)) {
					t.Fatalf("409: %v", err)
				}
				wantCode := map[int]int{401: ExitAuth, 403: ExitAuth, 404: ExitNotFound, 409: ExitConflict, 500: ExitUnreachable}[status]
				if ExitCodeFor(err) != wantCode {
					t.Fatalf("status %d exit=%d want %d: %v", status, ExitCodeFor(err), wantCode, err)
				}
			})
		}
	}
}
