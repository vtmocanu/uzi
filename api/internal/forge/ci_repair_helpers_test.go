package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCIReadLimitsSharedClonedContextsAndLookahead(t *testing.T) {
	ctx := WithCIRepairReadLimits(context.Background())
	b := ciBudget(ctx)
	b.remaining = 3
	body := &ciCloseReader{Reader: strings.NewReader("abc")}
	base := ciRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	req, _ := http.NewRequestWithContext(markCIJSON(ctx), "GET", "https://example.test", nil)
	resp, err := (ciRepairTransport{base: base}).RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != "abc" || b.remaining != 0 || !body.closed {
		t.Fatalf("exact shared allowance: %q, %v, remaining=%d, closed=%v", data, err, b.remaining, body.closed)
	}
	_ = resp.Body.Close()
	body = &ciCloseReader{Reader: strings.NewReader("d")}
	_, err = (ciRepairTransport{base: base}).RoundTrip(req.Clone(req.Context()))
	requireCIBudget(t, err)
	if b.remaining != -1 || !body.closed {
		t.Fatalf("lookahead: remaining=%d closed=%v", b.remaining, body.closed)
	}
}

func TestCIExplicitPaginationItemLimitDiscardsSnapshot(t *testing.T) {
	ctx := WithCIRepairReadLimits(context.Background())
	calls := 0
	fetch := func(page int) ([]int, int, error) {
		calls++
		if page == 1 {
			return []int{1, 2}, 2, nil
		}
		return []int{3, 4}, 0, nil
	}
	got, err := paginateLimits(func(e error) error { return newRedactor("ci-fixture-credential").error(e) }, fetch, 3, 10, ciBudget(ctx))
	requireCIBudget(t, err)
	if got != nil || calls != 2 {
		t.Fatalf("snapshot=%v calls=%d", got, calls)
	}
	got, err = paginateLimits(func(e error) error { return e }, fetch, 4, 10, ciBudget(WithCIRepairReadLimits(context.Background())))
	if err != nil || len(got) != 4 || got[3] != 4 {
		t.Fatalf("exact terminal item limit: %v, %v", got, err)
	}
}

func TestCIPartialJobHTTPErrorDiscardsAll(t *testing.T) {
	for _, provider := range ciProviders {
		t.Run(string(provider), func(t *testing.T) {
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "2" {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"message":"ci-fixture-credential denied"}`)
					return
				}
				w.Header().Set("X-Next-Page", "2")
				w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
				_, _ = io.WriteString(w, ciJobsJSON(provider, 1))
			})
			jobs, err := d.ListPipelineJobs(WithCIRepairReadLimits(context.Background()), 7, 1)
			if jobs != nil || err == nil || errors.Is(err, ErrCIReadBudget) || strings.Contains(err.Error(), "ci-fixture-credential") {
				t.Fatalf("partial HTTP failure: %v, %v", jobs, err)
			}
		})
	}
}

func TestCIGitHubLogTailExcluded(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "log tail")
	}))
	t.Cleanup(blob.Close)
	d := ciWireDriver(t, TypeGitHub, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", blob.URL+"/log")
		w.WriteHeader(http.StatusFound)
	}).(*github)
	d.allowInsecureLogHost = true
	ctx := WithCIRepairReadLimits(context.Background())
	ciBudget(ctx).remaining = 0
	log, err := d.JobLogTail(ctx, 7, 1, 100)
	if err != nil || log != "log tail" || ciBudget(ctx).remaining != 0 {
		t.Fatalf("log=%q, error=%v, remaining=%d", log, err, ciBudget(ctx).remaining)
	}
}
