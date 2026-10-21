package forge

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// GetMergeRequestSummary makes the same SDK call as GetMergeRequest, so it carries the
// same response ceilings (#3226). These reuse the #3038 wire helpers in
// gitlab_mr_limit_test.go and run serially for the same reason.

const mrSummaryApprovalsPath = mrTestPath + "/approvals"

func summaryJSON(padTo int) []byte {
	b := []byte(`{"id":1009,"iid":9,"title":"t","state":"opened","source_branch":"agent/issue-1",` +
		`"target_branch":"main","sha":"0123456789abcdef0123456789abcdef01234567",` +
		`"blocking_discussions_resolved":true,"web_url":"https://gl/mr/9"}`)
	if len(b) < padTo {
		b = append(b, bytes.Repeat([]byte(" "), padTo-len(b))...)
	}
	return b
}

func TestGitLabGetMergeRequestSummaryOverLimitRefused(t *testing.T) {
	lowerGitLabLimit(t, 4096)
	var approvalsHit atomic.Bool
	w := newWireMR(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mrSummaryApprovalsPath {
			approvalsHit.Store(true)
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(summaryJSON(20000))
	}, 5*time.Second)
	s, err := w.d.GetMergeRequestSummary(context.Background(), 7, 9)
	if !isTooLarge(err) {
		t.Fatalf("want ErrResponseTooLarge, got summary=%+v err=%v", s, err)
	}
	var typed *ResponseTooLargeError
	if !errors.As(err, &typed) {
		t.Fatalf("error %T is not *ResponseTooLargeError", err)
	}
	if errors.Is(err, ErrMergeRequestNotFound) {
		t.Fatal("a refused body must not read as not found")
	}
	if s != (MergeRequestSummary{}) {
		t.Fatalf("a refused response must return the zero summary, got %+v", s)
	}
	if approvalsHit.Load() {
		t.Fatal("approvals were requested after the merge-request body was refused")
	}
	if got := w.rt.totalRead(); got > 4096+1 {
		t.Fatalf("read %d bytes, want at most limit+1", got)
	}
	if !w.rt.allClosed() {
		t.Fatal("response body not closed")
	}
}

// An oversized 404 body overflows on the lazy non-success path, where the SDK still
// reports the status: the overflow check must win over the 404 mapping, or a refused
// body would read as "merge request not found".
func TestGitLabGetMergeRequestSummaryOversized404IsTooLarge(t *testing.T) {
	w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write(bytes.Repeat([]byte("x"), int(gitLabMaxErrorResponseBytes)+1024))
	}, 5*time.Second)
	_, err := w.d.GetMergeRequestSummary(context.Background(), 7, 9)
	if !isTooLarge(err) {
		t.Fatalf("oversized 404 = %v, want ErrResponseTooLarge", err)
	}
	if errors.Is(err, ErrMergeRequestNotFound) {
		t.Fatal("an oversized 404 body must not read as not found")
	}
}

func TestGitLabGetMergeRequestSummaryUnderLimit(t *testing.T) {
	lowerGitLabLimit(t, 4096)
	w := newWireMR(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mrSummaryApprovalsPath {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(summaryJSON(4096))
	}, 5*time.Second)
	s, err := w.d.GetMergeRequestSummary(context.Background(), 7, 9)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary at the limit: %v", err)
	}
	if s.IID != 9 || s.SourceBranch != "agent/issue-1" || s.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || s.ReviewDecision != ReviewNone {
		t.Fatalf("mapped summary = %+v", s)
	}
}
