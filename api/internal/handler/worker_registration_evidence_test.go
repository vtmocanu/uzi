package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #3859 (feature run_registration_evidence_v1): the nested registration_evidence object a
// worker sends on a running report, and the registration_evidence ACK the api answers.

// evidenceStore is a protocol fake whose registration-evidence write and running write are scripted
// independently, which is the whole contract: the ACK reflects the evidence write whether or not
// the status write applied.
type evidenceStore struct {
	*protocolStore
	evidenceRows int64
	runningRows  int64
	evidence     []store.RecordRunRegistrationEvidenceParams
}

func (s *evidenceStore) RecordRunRegistrationEvidence(_ context.Context, arg store.RecordRunRegistrationEvidenceParams) (int64, error) {
	s.evidence = append(s.evidence, arg)
	return s.evidenceRows, nil
}

func (s *evidenceStore) SetRunRunning(context.Context, store.SetRunRunningParams) (int64, error) {
	return s.runningRows, nil
}

// No top-level claim_generation: a fenced report needs the claim transaction, which this fake
// handler does not wire; the ACK contract does not depend on the fence.
const validEvidenceBody = `{"status":"running","registration_evidence":{"register_nonce":"n-1","claim_generation":2,"registration_epoch":7}}`

func evidenceAck(t *testing.T, st *evidenceStore, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	runID := uuid.New()
	st.ownedRun = store.Run{ID: runID, Status: "claimed", ClaimGeneration: 2}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, workerReq(http.MethodPost, body, runID))
	var ack map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack %q: %v", rec.Body.String(), err)
	}
	return rec.Code, ack
}

func ackEvidence(t *testing.T, ack map[string]json.RawMessage) string {
	t.Helper()
	raw, ok := ack["registration_evidence"]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("registration_evidence %s: %v", raw, err)
	}
	return v
}

func TestWorkerStateRegistrationEvidenceAck(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		evidenceRows, runningRows int64
		wantCode                  int
		wantAck                   string
	}{
		{"recorded and applied", 1, 1, http.StatusOK, "recorded"},
		{"recorded although the status write was refused", 1, 0, http.StatusConflict, "recorded"},
		{"refused and applied", 0, 1, http.StatusOK, "refused"},
		{"refused and not applied", 0, 0, http.StatusConflict, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &evidenceStore{protocolStore: &protocolStore{}, evidenceRows: tc.evidenceRows, runningRows: tc.runningRows}
			code, ack := evidenceAck(t, st, validEvidenceBody)
			if code != tc.wantCode || ackEvidence(t, ack) != tc.wantAck {
				t.Fatalf("code=%d registration_evidence=%q, want %d / %q (ack %v)", code, ackEvidence(t, ack), tc.wantCode, tc.wantAck, ack)
			}
			if len(st.evidence) != 1 {
				t.Fatalf("evidence writes = %d, want 1", len(st.evidence))
			}
			arg := st.evidence[0]
			if arg.Generation.Int64 != 2 || arg.RegisterNonce.String != "n-1" || arg.RegistrationEpoch.Int64 != 7 {
				t.Fatalf("evidence write = %+v, want (2, n-1, 7)", arg)
			}
		})
	}
	t.Run("absent when the request carried none", func(t *testing.T) {
		st := &evidenceStore{protocolStore: &protocolStore{}, evidenceRows: 1, runningRows: 1}
		code, ack := evidenceAck(t, st, `{"status":"running"}`)
		if _, has := ack["registration_evidence"]; has || code != http.StatusOK || len(st.evidence) != 0 {
			t.Fatalf("code=%d ack=%v writes=%d, want an unchanged ack and no evidence write", code, ack, len(st.evidence))
		}
	})
}

func TestWorkerStateRegistrationEvidenceValidation(t *testing.T) {
	for name, body := range map[string]string{
		"empty nonce":        `{"status":"running","claim_generation":2,"registration_evidence":{"register_nonce":"","claim_generation":2,"registration_epoch":0}}`,
		"zero generation":    `{"status":"running","claim_generation":2,"registration_evidence":{"register_nonce":"n","claim_generation":0,"registration_epoch":0}}`,
		"negative epoch":     `{"status":"running","claim_generation":2,"registration_evidence":{"register_nonce":"n","claim_generation":2,"registration_epoch":-1}}`,
		"missing epoch":      `{"status":"running","claim_generation":2,"registration_evidence":{"register_nonce":"n","claim_generation":2}}`,
		"unknown member":     `{"status":"running","claim_generation":2,"registration_evidence":{"register_nonce":"n","claim_generation":2,"registration_epoch":0,"extra":1}}`,
		"not a running body": `{"status":"completed","claim_generation":2,"registration_evidence":{"register_nonce":"n","claim_generation":2,"registration_epoch":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := &evidenceStore{protocolStore: &protocolStore{}, evidenceRows: 1, runningRows: 1}
			code, _ := evidenceAck(t, st, body)
			if code != http.StatusBadRequest || len(st.evidence) != 0 {
				t.Fatalf("code=%d writes=%d, want 400 and no evidence write", code, len(st.evidence))
			}
		})
	}
}

// TestWorkerStateRegistrationEvidenceStrictDecoder: an api WITHOUT the field answers 400 to a
// report that carries it, which is why a worker may send it only after the register response
// advertised run_registration_evidence_v1. The pre-change request shape is built from the CURRENT
// StateRequest by dropping that one field, so it cannot drift from the real struct.
func TestWorkerStateRegistrationEvidenceStrictDecoder(t *testing.T) {
	cur := reflect.TypeOf(workersvc.StateRequest{})
	var fields []reflect.StructField
	for i := 0; i < cur.NumField(); i++ {
		if f := cur.Field(i); f.Name != "RegistrationEvidence" {
			fields = append(fields, f)
		}
	}
	if len(fields) != cur.NumField()-1 {
		t.Fatal("StateRequest no longer has a RegistrationEvidence field")
	}
	decode := func(dst any, body string) error {
		return httpx.DecodeJSON(httptest.NewRequest("POST", "/state", strings.NewReader(body)), dst)
	}
	old := reflect.New(reflect.StructOf(fields)).Interface()
	if err := decode(old, `{"status":"running","claim_generation":2}`); err != nil {
		t.Fatalf("a pre-change api must accept a report without the field: %v", err)
	}
	err := decode(reflect.New(reflect.StructOf(fields)).Interface(), validEvidenceBody)
	if err == nil || !strings.Contains(err.Error(), `unknown field "registration_evidence"`) {
		t.Fatalf("a pre-change api must reject the field, got %v", err)
	}
	var now workersvc.StateRequest
	if err := decode(&now, validEvidenceBody); err != nil {
		t.Fatalf("the current api must accept it: %v", err)
	}
	if now.RegistrationEvidence == nil || now.RegistrationEvidence.RegisterNonce != "n-1" || now.RegistrationEvidence.ClaimGeneration != 2 ||
		now.RegistrationEvidence.RegistrationEpoch == nil || *now.RegistrationEvidence.RegistrationEpoch != 7 {
		t.Fatalf("decoded %+v", now.RegistrationEvidence)
	}
}
