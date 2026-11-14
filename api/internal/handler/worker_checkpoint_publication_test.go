package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4257 (feature checkpoint_publication_withheld_v1): the nested checkpoint_publication
// object a worker sends on a running report, and the checkpoint_publication ACK the api answers.

// cpStore scripts the fact writes independently of the status write: a fact-write error must never
// fail the report.
type cpStore struct {
	*protocolStore
	outcome     string
	err         error
	runningRows int64
	sets        []store.RecordRunCheckpointWithheldParams
	clears      []store.ClearRunCheckpointWithheldParams
}

func (s *cpStore) RecordRunCheckpointWithheld(_ context.Context, arg store.RecordRunCheckpointWithheldParams) (string, error) {
	s.sets = append(s.sets, arg)
	return s.outcome, s.err
}

func (s *cpStore) ClearRunCheckpointWithheld(_ context.Context, arg store.ClearRunCheckpointWithheldParams) (string, error) {
	s.clears = append(s.clears, arg)
	return s.outcome, s.err
}

func (s *cpStore) SetRunRunning(context.Context, store.SetRunRunningParams) (int64, error) {
	return s.runningRows, nil
}

const (
	cpReportWithheld = `{"status":"running","checkpoint_publication":{"state":"withheld","since":"2026-11-14T12:00:00Z","claim_generation":3,"register_nonce":"n-1","report_seq":4}}`
	cpReportCleared  = `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n-1","report_seq":5}}`
)

func cpAck(t *testing.T, st *cpStore, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	runID := uuid.New()
	st.ownedRun = store.Run{ID: runID, Status: "claimed", ClaimGeneration: 3}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, workerReq(http.MethodPost, body, runID))
	var ack map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack %q: %v", rec.Body.String(), err)
	}
	return rec.Code, ack
}

func cpAckValue(t *testing.T, ack map[string]json.RawMessage) string {
	t.Helper()
	raw, ok := ack["checkpoint_publication"]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("checkpoint_publication %s: %v", raw, err)
	}
	return v
}

func TestWorkerStateCheckpointPublicationAck(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		outcome     string
		storeErr    error
		runningRows int64
		wantCode    int
		wantAck     string
	}{
		{"withheld recorded", cpReportWithheld, "recorded", nil, 1, http.StatusOK, "recorded"},
		{"withheld refused", cpReportWithheld, "refused", nil, 1, http.StatusOK, "refused"},
		{"cleared", cpReportCleared, "cleared", nil, 1, http.StatusOK, "cleared"},
		{"clear refused", cpReportCleared, "refused", nil, 1, http.StatusOK, "refused"},
		{"unexpected store text is refused", cpReportWithheld, "something-else", nil, 1, http.StatusOK, "refused"},
		{"recorded although the status write did not apply", cpReportWithheld, "recorded", nil, 0, http.StatusConflict, "recorded"},
		// Failure isolation: a fact-write error is ACKed unrecorded and never fails the status write.
		{"withheld store error", cpReportWithheld, "", errors.New("boom"), 1, http.StatusOK, "unrecorded"},
		{"clear store error", cpReportCleared, "", errors.New("boom"), 1, http.StatusOK, "unrecorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &cpStore{protocolStore: &protocolStore{}, outcome: tc.outcome, err: tc.storeErr, runningRows: tc.runningRows}
			code, ack := cpAck(t, st, tc.body)
			if code != tc.wantCode || cpAckValue(t, ack) != tc.wantAck {
				t.Fatalf("code=%d checkpoint_publication=%q, want %d / %q (ack %v)", code, cpAckValue(t, ack), tc.wantCode, tc.wantAck, ack)
			}
			if len(st.sets)+len(st.clears) != 1 {
				t.Fatalf("fact writes = %d sets, %d clears, want exactly one", len(st.sets), len(st.clears))
			}
		})
	}
	t.Run("withheld write carries the report's fence and marker", func(t *testing.T) {
		st := &cpStore{protocolStore: &protocolStore{}, outcome: "recorded", runningRows: 1}
		cpAck(t, st, cpReportWithheld)
		if len(st.sets) != 1 {
			t.Fatalf("sets = %d", len(st.sets))
		}
		a := st.sets[0]
		if a.Generation != 3 || a.RegisterNonce != "n-1" || a.ReportSeq != 4 || !a.Since.Valid || a.Since.Time.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-14T12:00:00Z" {
			t.Fatalf("withheld write = %+v", a)
		}
	})
	t.Run("absent when the request carried none", func(t *testing.T) {
		st := &cpStore{protocolStore: &protocolStore{}, outcome: "recorded", runningRows: 1}
		code, ack := cpAck(t, st, `{"status":"running"}`)
		if _, has := ack["checkpoint_publication"]; has || code != http.StatusOK || len(st.sets)+len(st.clears) != 0 {
			t.Fatalf("code=%d ack=%v, want an unchanged ack and no fact write", code, ack)
		}
	})
}

func TestWorkerStateCheckpointPublicationValidation(t *testing.T) {
	for name, body := range map[string]string{
		"not a running body":         `{"status":"completed","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"unknown state":              `{"status":"running","checkpoint_publication":{"state":"blocked","claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"empty state":                `{"status":"running","checkpoint_publication":{"claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"withheld without since":     `{"status":"running","checkpoint_publication":{"state":"withheld","claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"withheld bad since":         `{"status":"running","checkpoint_publication":{"state":"withheld","since":"yesterday","claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"cleared with since":         `{"status":"running","checkpoint_publication":{"state":"cleared","since":"2026-11-14T12:00:00Z","claim_generation":3,"register_nonce":"n","report_seq":1}}`,
		"zero generation":            `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":0,"register_nonce":"n","report_seq":1}}`,
		"empty nonce":                `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"","report_seq":1}}`,
		"zero report_seq":            `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n","report_seq":0}}`,
		"missing report_seq":         `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n"}}`,
		"extra key path":             `{"status":"running","checkpoint_publication":{"state":"withheld","since":"2026-11-14T12:00:00Z","claim_generation":3,"register_nonce":"n","report_seq":1,"path":"secrets/.env"}}`,
		"extra key finding":          `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n","report_seq":1,"finding":"x"}}`,
		"report_seq is not a number": `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n","report_seq":"1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := &cpStore{protocolStore: &protocolStore{}, outcome: "recorded", runningRows: 1}
			code, _ := cpAck(t, st, body)
			if code != http.StatusBadRequest || len(st.sets)+len(st.clears) != 0 {
				t.Fatalf("code=%d writes=%d, want 400 and no fact write", code, len(st.sets)+len(st.clears))
			}
		})
	}
}

// TestWorkerStateCheckpointPublicationStrictDecoder: an api WITHOUT the field answers 400 to a
// report that carries it, which is why a worker may send it only after the register response
// advertised checkpoint_publication_withheld_v1. The pre-change shape is the CURRENT StateRequest
// minus that one field.
func TestWorkerStateCheckpointPublicationStrictDecoder(t *testing.T) {
	cur := reflect.TypeOf(workersvc.StateRequest{})
	var fields []reflect.StructField
	for i := 0; i < cur.NumField(); i++ {
		if f := cur.Field(i); f.Name != "CheckpointPublication" {
			fields = append(fields, f)
		}
	}
	if len(fields) != cur.NumField()-1 {
		t.Fatal("StateRequest no longer has a CheckpointPublication field")
	}
	decode := func(dst any, body string) error {
		return httpx.DecodeJSON(httptest.NewRequest("POST", "/state", strings.NewReader(body)), dst)
	}
	if err := decode(reflect.New(reflect.StructOf(fields)).Interface(), `{"status":"running","claim_generation":3}`); err != nil {
		t.Fatalf("a pre-change api must accept a report without the field: %v", err)
	}
	err := decode(reflect.New(reflect.StructOf(fields)).Interface(), cpReportWithheld)
	if err == nil || !strings.Contains(err.Error(), `unknown field "checkpoint_publication"`) {
		t.Fatalf("a pre-change api must reject the field, got %v", err)
	}
	var now workersvc.StateRequest
	if err := decode(&now, cpReportWithheld); err != nil {
		t.Fatalf("the current api must accept it: %v", err)
	}
	cp := now.CheckpointPublication
	if cp == nil || cp.State != "withheld" || cp.Since != "2026-11-14T12:00:00Z" || cp.ClaimGeneration != 3 || cp.RegisterNonce != "n-1" || cp.ReportSeq != 4 {
		t.Fatalf("decoded %+v", cp)
	}
	// The object itself is strict: no content-bearing extra key can ride along.
	var extra workersvc.StateRequest
	err = decode(&extra, `{"status":"running","checkpoint_publication":{"state":"cleared","claim_generation":3,"register_nonce":"n","report_seq":1,"path":"x"}}`)
	if err == nil || !strings.Contains(err.Error(), `unknown field "path"`) {
		t.Fatalf("an extra key inside checkpoint_publication must be refused, got %v", err)
	}
}

// TestCheckpointPublicationWireFixtureDecodes: the shared worker-protocol fixture (the worker's
// own test sends the same bytes) decodes through the real strict decoder and validates.
func TestCheckpointPublicationWireFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/checkpoint-publication-withheld-running.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	var req workersvc.StateRequest
	if err := httpx.DecodeJSON(httptest.NewRequest("POST", "/state", strings.NewReader(string(raw))), &req); err != nil {
		t.Fatalf("fixture refused by the strict decoder: %v", err)
	}
	cp := req.CheckpointPublication
	if req.State != "running" || req.ClaimGeneration == nil || *req.ClaimGeneration != 3 || cp == nil ||
		cp.State != "withheld" || cp.Since != "2026-11-14T12:00:00Z" || cp.ClaimGeneration != 3 || cp.RegisterNonce == "" || cp.ReportSeq != 1 {
		t.Fatalf("fixture decoded to %+v / %+v", req, cp)
	}
	st := &cpStore{protocolStore: &protocolStore{}, outcome: "recorded", runningRows: 1}
	runID := uuid.New()
	st.ownedRun = store.Run{ID: runID, Status: "claimed", ClaimGeneration: 3}
	rec := httptest.NewRecorder()
	// No top-level claim_generation fence is wired in this fake handler; only the body shape is checked.
	h := newProtocolHandler(t, st)
	h.WorkerRunState(rec, workerReq(http.MethodPost, strings.Replace(string(raw), `"claim_generation": 3,
  "checkpoint_publication"`, `"checkpoint_publication"`, 1), runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("fixture post: code=%d %s", rec.Code, rec.Body.String())
	}
}

// TestRunDTOPublicationWithheldIsContentFree: the run DTO of a run carrying the stored fact has the
// health enum and the fixed reason and NOTHING else new. The fact's own columns (since, markers,
// nonce) never reach the wire: the JSON is byte-identical to the same run without them.
func TestRunDTOPublicationWithheldIsContentFree(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	reason := apitypes.ReasonCheckpointPublicationWithheld
	base := store.Run{
		ID: uuid.New(), Status: "running", Kind: "issue",
		Health: "publication_withheld", HealthReason: pgtype.Text{String: reason, Valid: true},
		HealthSince: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}
	withFact := base
	withFact.CheckpointWithheldSince = pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
	withFact.CheckpointWithheldStatusSince = pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	withFact.CheckpointWithheldWorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	withFact.CheckpointWithheldGeneration = pgtype.Int8{Int64: 7, Valid: true}
	withFact.CheckpointWithheldNonce = pgtype.Text{String: "register-nonce-secretish", Valid: true}
	withFact.CheckpointWithheldReportSeq = pgtype.Int8{Int64: 9, Valid: true}
	a, err := json.Marshal(runToDTO(base, "", time.Hour, 0, 0, now, 3))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(runToDTO(withFact, "", time.Hour, 0, 0, now, 3))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("the stored fact changed the run DTO:\n%s\n%s", a, b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["health"] != "publication_withheld" || m["health_reason"] != reason {
		t.Fatalf("health = %v / %v, want the enum and the fixed reason", m["health"], m["health_reason"])
	}
	for k := range m {
		if strings.Contains(k, "withheld") || strings.Contains(k, "report_seq") || strings.Contains(k, "nonce") {
			t.Fatalf("DTO grew a fact-bearing key %q", k)
		}
	}
	if strings.Contains(string(b), "register-nonce-secretish") {
		t.Fatal("the nonce reached the DTO")
	}
}
