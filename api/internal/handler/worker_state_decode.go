package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// decodeWorkerState retains the state route's 1 MiB cap and requires the whole
// body to be valid. Raw presence checks distinguish omitted authority from null.
func decodeWorkerState(r *http.Request, req *workersvc.StateRequest) error {
	var raw json.RawMessage
	if err := httpx.DecodeJSONStrictBounded(r, &raw, 1<<20); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("state must be an object")
	}
	var status string
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return err
	}
	if status == "codex_switch_stopped" {
		if len(fields) != 4 {
			return errors.New("stopped settlement requires exactly four fields")
		}
		for _, key := range []string{"status", "claim_generation", "register_nonce", "expected_epoch"} {
			value, ok := fields[key]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errors.New("missing stopped settlement authority")
			}
		}
	} else {
		for key := range fields {
			if strings.EqualFold(key, "expected_epoch") {
				return errors.New("expected_epoch requires codex_switch_stopped")
			}
		}
	}
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(raw))
	if err := httpx.DecodeJSONStrictBounded(copy, req, 1<<20); err != nil {
		return err
	}
	if status == "codex_switch_stopped" && (req.ClaimGeneration == nil || (*req.ClaimGeneration <= 0 || *req.ClaimGeneration > 9007199254740991) ||
		req.ExpectedEpoch == nil || (*req.ExpectedEpoch <= 0 || *req.ExpectedEpoch > 9007199254740991) || req.RegisterNonce == "") {
		return errors.New("invalid stopped settlement authority")
	}
	return nil
}
