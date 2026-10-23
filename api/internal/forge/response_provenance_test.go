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
	"time"
)

func TestResponseOverflowProvenanceRedaction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		permanent  bool
		class      ErrorClass
		provenance ResponseProvenance
	}{
		{"bare", ErrResponseTooLarge, false, ErrorClassOther, ResponseUnknown},
		{"empty", &ResponseTooLargeError{}, false, ErrorClassOther, ResponseUnknown},
		{"success", &ResponseTooLargeError{Provenance: ResponseSuccess}, false, ErrorClassOther, ResponseSuccess},
		{"issue", &ResponseTooLargeError{Provenance: ResponseIssueSuccess}, true, ErrorClassOther, ResponseIssueSuccess},
		{"rate", &ResponseTooLargeError{Provenance: ResponseError, Outcome: ErrorClassRateLimited}, false, ErrorClassRateLimited, ResponseError},
		{"server", &ResponseTooLargeError{Provenance: ResponseError, Outcome: ErrorClassServerError}, false, ErrorClassServerError, ResponseError},
		{"invalid", &ResponseTooLargeError{Provenance: 255, Outcome: "REMOTE https://secret.invalid"}, false, ErrorClassOther, ResponseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("REMOTE https://secret.invalid header: %w", tc.err)
			if IsIssueResponseTooLarge(wrapped) != tc.permanent || Class(wrapped) != tc.class {
				t.Fatalf("wrapped predicate/class: %v / %q", IsIssueResponseTooLarge(wrapped), Class(wrapped))
			}
			clean := (redactor{}).error(wrapped)
			if IsIssueResponseTooLarge(clean) != tc.permanent || Class(clean) != tc.class {
				t.Fatalf("redacted predicate/class: %v / %q", IsIssueResponseTooLarge(clean), Class(clean))
			}
			var large *ResponseTooLargeError
			if errors.As(clean, &large) {
				if large.Provenance != tc.provenance || large.Class() != tc.class || clean.Error() != ErrResponseTooLarge.Error() {
					t.Fatalf("redacted metadata: %+v", large)
				}
				if !errors.Is(clean, ErrResponseTooLarge) || strings.Contains(fmt.Sprintf("%+v", *large), "REMOTE") {
					t.Fatal("unsafe metadata or lost sentinel")
				}
			} else {
				t.Fatal("lost typed overflow")
			}
		})
	}
	if IsIssueResponseTooLarge(nil) {
		t.Fatal("nil classified as permanent")
	}
}

// GitLab rejects these 2xx statuses rather than decoding an issue response.
func TestGetIssueRejectedSuccessStatusOverflow(t *testing.T) {
	for _, status := range []int{203, 205, 206} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, strings.Repeat("REMOTE", 683))
			}))
			defer srv.Close()
			d, err := newGitLab(srv.URL, testToken(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.GetIssue(context.Background(), 7, 9)
			assessmentRefusal(t, err)
			if IsIssueResponseTooLarge(err) || attempts != 1 {
				t.Fatalf("rejected status became permanent: status=%d err=%v attempts=%d", status, err, attempts)
			}
		})
	}
}

// The public GetIssue seam must not promote a successful prerequisite response
// to an affirmative issue response, even though both belong to the same call.
func TestGetIssueSuccessfulPrerequisiteOverflow(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if !strings.HasSuffix(r.URL.Path, "/repositories/7") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, strings.Repeat(" ", 1048577))
	}))
	defer srv.Close()
	d, err := newGitHub(srv.URL, testToken(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := d.GetIssue(context.Background(), 7, 9)
	assessmentRefusal(t, err)
	var large *ResponseTooLargeError
	if !errors.As(err, &large) || large.Provenance != ResponseSuccess || IsIssueResponseTooLarge(err) || Class(err) != ErrorClassOther || issue.IID != 0 || attempts != 1 {
		t.Fatalf("prerequisite overflow: issue=%+v err=%v attempts=%d", issue, err, attempts)
	}
}
