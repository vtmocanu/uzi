package issuetracker

import (
	"net/http"
	"time"

	"github.com/vtmocanu/uzi/api/internal/httptransport"
)

// defaultTimeout bounds one request when Options.Timeout is zero.
const defaultTimeout = 15 * time.Second

// newHTTPClient builds the client a driver uses: a finite timeout and no
// redirect following, so a Location header cannot carry the token to another
// host. A nil base means an independent httptransport.New(http.DefaultTransport);
// only the test-only constructor in export_test.go sets base.
func newHTTPClient(timeout time.Duration, base http.RoundTripper) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if base == nil {
		base = httptransport.New(http.DefaultTransport)
	}
	return &http.Client{
		Transport: base,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// isRedirect reports whether resp is any 3xx, including 304 and a response with
// no Location. None is ever followed or treated as success.
func isRedirect(resp *http.Response) bool {
	return resp.StatusCode >= 300 && resp.StatusCode < 400
}
