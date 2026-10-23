package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitHubMRMetadataOverflowCompatibility(t *testing.T) {
	for _, summary := range []bool{false, true} {
		for _, status := range []int{200, 401, 403, 404, 429, 500, 501} {
			t.Run(fmt.Sprintf("summary_%t/status_%d", summary, status), func(t *testing.T) {
				const ceiling = 512
				srv := httptest.NewServer(oversizedHandler(status, ""))
				defer srv.Close()
				track := &trackRT{next: forgeHTTPTransport}
				installOverride(t, track)
				var state *responseLimitState
				d := assessmentDriver(t, "github", srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
					if r.URL.Path != "/api/v3/repositories/7" {
						t.Errorf("unexpected request: %s", r.URL.Path)
					}
					state = responseLimitFrom(r.Context())
					if state == nil {
						t.Fatal("unmarked metadata request")
					}
					state.ceiling = ceiling
					return next.RoundTrip(r)
				})
				var err error
				if summary {
					var s MergeRequestSummary
					s, err = d.GetMergeRequestSummary(context.Background(), 7, 9)
					if s != (MergeRequestSummary{}) {
						t.Fatalf("partial summary: %+v", s)
					}
				} else {
					var mr MergeRequest
					mr, err = d.GetMergeRequest(context.Background(), 7, 9)
					if mr != (MergeRequest{}) {
						t.Fatalf("partial MR: %+v", mr)
					}
				}
				wantRefusal(t, MergeRequest{}, err)
				var large *ResponseTooLargeError
				if !errors.As(err, &large) {
					t.Fatal("missing typed overflow")
				}
				wantProvenance := ResponseError
				if status == 200 {
					wantProvenance = ResponseSuccess
				}
				// The MR boundary must copy rather than alter shared overflow facts.
				original := state.failure.Load()
				if original == large || original.Provenance != wantProvenance {
					t.Fatal("MR normalization changed or reused the recorded overflow")
				}
				wantOriginalClass := ErrorClassOther
				switch status {
				case 401, 403:
					wantOriginalClass = ErrorClassAuth
				case 429:
					wantOriginalClass = ErrorClassRateLimited
				case 500, 501:
					wantOriginalClass = ErrorClassServerError
				}
				if original.Class() != wantOriginalClass {
					t.Fatalf("recorded class=%v want=%v", original.Class(), wantOriginalClass)
				}
				if large.Provenance != wantProvenance || errors.Is(err, ErrMergeRequestNotFound) {
					t.Fatalf("provenance=%v err=%v", large.Provenance, err)
				}
				if track.reqs.Load() != 1 || track.totalRead() > ceiling+1 || !track.allClosed() {
					t.Fatalf("requests=%d read=%d closed=%v", track.reqs.Load(), track.totalRead(), track.allClosed())
				}
			})
		}
	}
}

func TestGitLabMRSummaryOverflowCompatibility(t *testing.T) {
	lowerGitLabLimit(t, 512)
	for _, status := range []int{401, 403, 404, 429, 500, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			w := newWireMR(t, oversizedHandler(status, ""), 5*time.Second)
			s, err := w.d.GetMergeRequestSummary(context.Background(), 7, 9)
			if s != (MergeRequestSummary{}) {
				t.Fatalf("partial summary: %+v", s)
			}
			wantRefusal(t, MergeRequest{}, err)
			var large *ResponseTooLargeError
			if !errors.As(err, &large) || large.Provenance != ResponseError || errors.Is(err, ErrMergeRequestNotFound) {
				t.Fatalf("lost error provenance or oversized 404 fallback: %v", err)
			}
			if w.rt.reqs.Load() != 1 || w.rt.totalRead() > 513 || !w.rt.allClosed() {
				t.Fatalf("requests=%d read=%d closed=%v", w.rt.reqs.Load(), w.rt.totalRead(), w.rt.allClosed())
			}
		})
	}
}
