package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestReportHibernationPostsTheTransition(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody protocol.HibernationTransition
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	want := protocol.HibernationTransition{OpID: "op1", Transition: protocol.TransitionScalingDown, DeploymentUID: "d-uid", ResourceVersion: "42", PodUID: "p-uid"}
	if err := New(srv.URL, "the-token", 5*time.Second, nil, testLogger()).ReportHibernation(context.Background(), "w1", want); err != nil {
		t.Fatalf("ReportHibernation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/controller/workers/w1/hibernation" || gotAuth != "Bearer the-token" {
		t.Fatalf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %+v, want %+v", gotBody, want)
	}
}

func TestReportHibernationMapsStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		status   int
		conflict bool
		fails    bool
	}{
		{http.StatusOK, false, false},
		{http.StatusConflict, true, true},
		{http.StatusNotFound, false, true},
		{http.StatusInternalServerError, false, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		err := New(srv.URL, "t", 5*time.Second, nil, testLogger()).ReportHibernation(context.Background(), "w1", protocol.HibernationTransition{OpID: "o"})
		srv.Close()
		if (err != nil) != tc.fails {
			t.Errorf("status %d: err = %v, want failure=%v", tc.status, err, tc.fails)
		}
		if errors.Is(err, ErrHibernationConflict) != tc.conflict {
			t.Errorf("status %d: ErrHibernationConflict match = %v, want %v", tc.status, !tc.conflict, tc.conflict)
		}
	}
}
