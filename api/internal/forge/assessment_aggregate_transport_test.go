package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssessmentLedgerErrorBodies(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, status := range []int{401, 404, 501, 503} {
			for _, size := range []int{64, 65} {
				t.Run(fmt.Sprintf("%s/%d/%d", kind, status, size), func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, code := assessmentDefault(r.URL.Path)
						if strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/notes") {
							calls++
							code, body = status, "{}"+strings.Repeat(" ", size-2)
						}
						w.WriteHeader(code)
						w.(http.Flusher).Flush()
						_, _ = io.WriteString(w, body)
					}))
					defer srv.Close()
					track := &trackRT{next: forgeHTTPTransport}
					installOverride(t, track)
					var ledger *responseLedger
					var request *http.Request
					var transport http.RoundTripper
					d := assessmentDriver(t, kind, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
						s := responseLimitFrom(r.Context())
						if s.ledger != nil {
							if ledger == nil {
								ledger = s.ledger
								ledger.remaining = 64
							}
							request, transport = r, next
						}
						return next.RoundTrip(r)
					})
					comments, err := d.ListIssueComments(context.Background(), 7, 9)
					if err == nil || comments != nil {
						t.Fatalf("error supplied evidence: %v %v", comments, err)
					}
					if size == 65 {
						assessmentRefusal(t, err)
					}
					if ledger == nil || ledger.remaining != int64(64-size) {
						t.Fatalf("uncharged error body: %+v", ledger)
					}
					// Exact error-body exhaustion must also stop another network attempt,
					// including SDK retries, without falling through to the zero default cap.
					resp, guardErr := transport.RoundTrip(request)
					if resp != nil {
						t.Fatal("exhaustion guard returned response")
					}
					assessmentRefusal(t, guardErr)
					var large *ResponseTooLargeError
					if !errors.As(guardErr, &large) || large.Provenance != ResponseError || IsIssueResponseTooLarge(guardErr) {
						t.Fatalf("unsafe error provenance: %v", guardErr)
					}
					if calls != 1 || track.maxRead() > 65 || !track.allClosed() {
						t.Fatalf("calls=%d read=%d closed=%v", calls, track.maxRead(), track.allClosed())
					}
				})
			}
		}
	}
}

func TestAssessmentLedgerChargesFailedRead(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, code := assessmentDefault(r.URL.Path)
				if strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/notes") {
					calls++
					w.Header().Set("Content-Length", "128")
					body = "[]" + strings.Repeat(" ", 62)
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			var ledger *responseLedger
			d := assessmentDriver(t, kind, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
				s := responseLimitFrom(r.Context())
				if s.ledger != nil {
					ledger = s.ledger
					ledger.remaining = 512
				}
				return next.RoundTrip(r)
			})
			comments, err := d.ListIssueComments(context.Background(), 7, 9)
			if err == nil || comments != nil || ledger == nil || ledger.remaining != 448 || calls != 1 {
				t.Fatalf("failed read: comments=%v err=%v ledger=%+v calls=%d", comments, err, ledger, calls)
			}
		})
	}
}
