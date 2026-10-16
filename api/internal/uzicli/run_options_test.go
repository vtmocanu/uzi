package uzicli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRunReadOptionsRealHTTP(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		_, _ = io.WriteString(w, `{"run":{"id":"r1","milestone_reconciliations_next_before_gate_revision":5}}`)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	if _, err := c.GetRun(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	cursor := int64(7)
	run, err := c.GetRunWithOptions(context.Background(), "r1", RunReadOptions{ReconciliationBeforeGateRevision: &cursor})
	if err != nil || run.MilestoneReconciliationsNextBeforeGateRevision == nil || *run.MilestoneReconciliationsNextBeforeGateRevision != 5 {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	for _, value := range []int64{0, -1} {
		_, err := c.GetRunWithOptions(context.Background(), "r1", RunReadOptions{ReconciliationBeforeGateRevision: &value})
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != ExitUsage {
			t.Fatalf("cursor=%d err=%v", value, err)
		}
	}
	if !reflect.DeepEqual(requests, []string{"GET /api/runs/r1", "GET /api/runs/r1?reconciliation_before_gate_revision=7"}) {
		t.Fatalf("requests=%v", requests)
	}
}

func TestReconciliationRefusalGuidanceRealHTTP(t *testing.T) {
	for _, tc := range []struct {
		reason string
		hints  []string
	}{
		{ReasonMilestoneReconciliationRequired, []string{"review uzi run get", "--reset-milestones", "--expected-completion-revision"}},
		{ReasonMilestoneReconciliationConflict, []string{"stale", "new explicit decision"}},
		{ReasonMilestoneReconciliationNotReady, []string{"plan revision", "fresh published checkpoint", "revisions are exhausted", "cancel", "new run"}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"untrusted\u001b[2J","reason":"`+tc.reason+`"}`)
			}))
			defer srv.Close()
			_, err := newTestClient(srv).SubmitRunInputWithOptions(context.Background(), "r1", RunInputOptions{Kind: "approve_plan"})
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != ExitConflict || ee.Reason != tc.reason || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			for _, hint := range tc.hints {
				if !strings.Contains(err.Error(), hint) {
					t.Errorf("missing %q: %v", hint, err)
				}
			}
			if strings.ContainsRune(err.Error(), '\x1b') {
				t.Fatalf("unsafe error: %q", err.Error())
			}
		})
	}
}
