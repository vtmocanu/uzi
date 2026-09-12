package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestGitHubRateReserveLow is the recorder unit test (PRD #1255 D4, shedding rule 1):
// gitHubRateReserveLow is true iff a record exists AND Remaining < max(500, Limit/10)
// AND Reset is still in the future. Each case uses a distinct token-hash key so it
// never collides with another test's process-wide recorder state.
func TestGitHubRateReserveLow(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	t.Run("low when remaining below the 500 floor and reset future", func(t *testing.T) {
		const key = "rate-unit-low-floor"
		recordGitHubRate(key, 100, 5000, future) // threshold max(500, 500) = 500
		low, reset := gitHubRateReserveLow(key)
		if !low {
			t.Fatalf("remaining 100 < 500 with a future reset must be low")
		}
		if !reset.Equal(future) {
			t.Errorf("reset = %v, want the recorded %v", reset, future)
		}
	})

	t.Run("low when remaining below 10 percent of a large limit", func(t *testing.T) {
		const key = "rate-unit-low-tenth"
		// Limit 100000 → threshold max(500, 10000) = 10000; 5000 < 10000 ⇒ low. This is
		// the branch where the 10%-of-Limit term dominates the 500 floor.
		recordGitHubRate(key, 5000, 100000, future)
		if low, _ := gitHubRateReserveLow(key); !low {
			t.Fatalf("remaining 5000 < 10%% of 100000 (10000) must be low")
		}
	})

	t.Run("not low when remaining healthy", func(t *testing.T) {
		const key = "rate-unit-healthy"
		recordGitHubRate(key, 5000, 5000, future)
		if low, _ := gitHubRateReserveLow(key); low {
			t.Fatalf("remaining 5000 (>= 500) must not be low")
		}
	})

	t.Run("not low when reset is in the past (window refilled)", func(t *testing.T) {
		const key = "rate-unit-expired"
		recordGitHubRate(key, 0, 5000, past)
		if low, _ := gitHubRateReserveLow(key); low {
			t.Fatalf("an expired window has already refilled and must not be low, even at remaining 0")
		}
	})

	t.Run("not low when no record exists", func(t *testing.T) {
		low, reset := gitHubRateReserveLow("rate-unit-never-recorded")
		if low {
			t.Fatalf("an unseen token must not be low")
		}
		if !reset.IsZero() {
			t.Errorf("reset for an unseen token = %v, want zero", reset)
		}
	})
}

// TestETagTransportRecordsRate proves the ETag transport records the primary
// rate-limit headers off a response, so the reserve sees the true current Remaining.
// It covers BOTH lanes the transport records on: an allowlisted read GET and a
// non-allowlisted passthrough GET.
func TestETagTransportRecordsRate(t *testing.T) {
	rateHeaders := func(w http.ResponseWriter, remaining int, reset time.Time) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	}

	t.Run("allowlisted read GET records", func(t *testing.T) {
		const token = "ghp" + "_rateRecordAllowlisted123456"
		reset := time.Now().Add(time.Hour)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rateHeaders(w, 10, reset) // 10 < 500 ⇒ the record must read low
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "[]")
		}))
		defer srv.Close()

		client := &http.Client{Transport: newETagTransportWithCache(nil, token, newETagCache())}
		if code, _ := getBody(t, client, srv.URL+"/repos/o/r/pulls"); code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", code)
		}
		low, gotReset := gitHubRateReserveLow(githubTokenHash(token))
		if !low {
			t.Fatalf("after a response reporting remaining 10, the reserve must read low")
		}
		if gotReset.Unix() != reset.Unix() {
			t.Errorf("recorded reset = %v, want %v (from X-RateLimit-Reset)", gotReset.Unix(), reset.Unix())
		}
	})

	t.Run("non-allowlisted passthrough GET also records", func(t *testing.T) {
		const token = "ghp" + "_rateRecordPassthrough1234567"
		reset := time.Now().Add(time.Hour)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rateHeaders(w, 5, reset)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}))
		defer srv.Close()

		client := &http.Client{Transport: newETagTransportWithCache(nil, token, newETagCache())}
		// /issues is NOT on the allowlist, so this exercises the passthrough recording path.
		if code, _ := getBody(t, client, srv.URL+"/repos/o/r/issues"); code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", code)
		}
		if low, _ := gitHubRateReserveLow(githubTokenHash(token)); !low {
			t.Fatalf("a passthrough response reporting remaining 5 must also record a low reserve")
		}
	})

	t.Run("response without rate headers is ignored", func(t *testing.T) {
		const token = "ghp" + "_rateRecordNoHeaders12345678"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "[]")
		}))
		defer srv.Close()

		client := &http.Client{Transport: newETagTransportWithCache(nil, token, newETagCache())}
		if code, _ := getBody(t, client, srv.URL+"/repos/o/r/pulls"); code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", code)
		}
		if low, _ := gitHubRateReserveLow(githubTokenHash(token)); low {
			t.Fatalf("a response with no X-RateLimit headers must not create a (low) record")
		}
	})
}

// seedLowReserve records a low primary-rate reserve for token so an interactive read
// through a driver built with the same token is shed. Returns the reset it recorded.
func seedLowReserve(token string) time.Time {
	reset := time.Now().Add(time.Hour)
	recordGitHubRate(githubTokenHash(token), 100, 5000, reset) // 100 < max(500, 500)
	return reset
}

// TestGitHubReserveShedsInteractiveRead is the load-bearing shed test: with a low
// reserve recorded for the driver's PAT, an INTERACTIVE forge-view read returns a
// *forge.RateLimitError and makes ZERO HTTP calls to the mock (shed BEFORE any forge
// call, including the id→slug resolution).
func TestGitHubReserveShedsInteractiveRead(t *testing.T) {
	const token = "ghp" + "_reserveShedInteractive12345"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		// Registered so a NON-shed call would succeed; a shed call must never reach it.
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "[]")
		},
	})
	d := newGitHubDriver(t, m, token)
	reset := seedLowReserve(token)

	_, err := d.ListMergeRequestRefs(WithInteractiveRead(context.Background()), 7, ListMergeRequestsOptions{})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("interactive read under a low reserve must return *RateLimitError, got %v", err)
	}
	if !rl.Reset.Equal(reset) {
		t.Errorf("RateLimitError.Reset = %v, want the recorded reset %v", rl.Reset, reset)
	}
	if n := m.reqCount.Load(); n != 0 {
		t.Fatalf("a shed interactive read made %d HTTP calls, want 0 (shed BEFORE any forge call)", n)
	}
}

// TestGitHubReserveDoesNotShedPoller is the "we don't starve the poller's lane and we
// don't shed it either" assertion: with the SAME low reserve, a NON-interactive read
// (plain ctx, the poller/worker lane) proceeds and hits the mock. It covers both a
// plain forge-view method (ListMergeRequestRefs) and the SHARED ListPipelineJobs,
// which the interactive ci-run drill-in and the poller's ci-fix both call.
func TestGitHubReserveDoesNotShedPoller(t *testing.T) {
	t.Run("ListMergeRequestRefs proceeds without the interactive flag", func(t *testing.T) {
		const token = "ghp" + "_reserveNoShedPollerRefs1234"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "[]")
			},
		})
		d := newGitHubDriver(t, m, token)
		seedLowReserve(token)

		refs, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{})
		if err != nil {
			t.Fatalf("a non-interactive read must NOT be shed even under a low reserve, got %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("expected the empty ref list the mock serves, got %d", len(refs))
		}
		if m.reqCount.Load() == 0 {
			t.Fatalf("the poller lane made zero HTTP calls — it was wrongly shed")
		}
	})

	t.Run("ListPipelineJobs proceeds without the interactive flag (ci-fix lane)", func(t *testing.T) {
		const token = "ghp" + "_reserveNoShedPollerJobs1234"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/actions/runs/42/jobs": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"total_count":0,"jobs":[]}`)
			},
		})
		d := newGitHubDriver(t, m, token)
		seedLowReserve(token)

		jobs, err := d.ListPipelineJobs(context.Background(), 7, 42)
		if err != nil {
			t.Fatalf("the poller's ci-fix ListPipelineJobs must NOT be shed, got %v", err)
		}
		if len(jobs) != 0 {
			t.Errorf("expected the empty job list the mock serves, got %d", len(jobs))
		}
		if m.reqCount.Load() == 0 {
			t.Fatalf("the ci-fix lane made zero HTTP calls — it was wrongly shed")
		}
	})

	t.Run("ListPipelineJobs sheds WITH the interactive flag (drill-in lane)", func(t *testing.T) {
		const token = "ghp" + "_reserveShedInteractiveJobs12"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/actions/runs/42/jobs": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"total_count":0,"jobs":[]}`)
			},
		})
		d := newGitHubDriver(t, m, token)
		seedLowReserve(token)

		_, err := d.ListPipelineJobs(WithInteractiveRead(context.Background()), 7, 42)
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("an interactive ci-run drill-in under a low reserve must shed, got %v", err)
		}
		if n := m.reqCount.Load(); n != 0 {
			t.Fatalf("a shed interactive ListPipelineJobs made %d HTTP calls, want 0", n)
		}
	})
}

// TestGitHubReserveHealthyDoesNotShed proves a healthy recorded reserve lets an
// interactive read proceed normally.
func TestGitHubReserveHealthyDoesNotShed(t *testing.T) {
	const token = "ghp" + "_reserveHealthyInteractive123"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "[]")
		},
	})
	d := newGitHubDriver(t, m, token)
	recordGitHubRate(githubTokenHash(token), 5000, 5000, time.Now().Add(time.Hour)) // healthy

	refs, err := d.ListMergeRequestRefs(WithInteractiveRead(context.Background()), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("an interactive read with a healthy reserve must proceed, got %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected the empty ref list the mock serves, got %d", len(refs))
	}
	if m.reqCount.Load() == 0 {
		t.Fatalf("a healthy interactive read made zero HTTP calls — it was wrongly shed")
	}
}
