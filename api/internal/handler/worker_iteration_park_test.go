package handler

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIterationParkEvidence(t *testing.T) {
	valid := `{"claim_generation":4,"iteration_count":5,"max_iterations":5,"head":"","published":false}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"valid", valid, true},
		{"plus_one", strings.Replace(valid, `"iteration_count":5`, `"iteration_count":6`, 1), true},
		{"max_int32", fmt.Sprintf(`{"claim_generation":1,"iteration_count":%d,"max_iterations":%d,"head":"","published":true}`, math.MaxInt32, math.MaxInt32), true},
		{"missing_generation", strings.Replace(valid, `"claim_generation":4,`, "", 1), false},
		{"generation_null", strings.Replace(valid, `"claim_generation":4`, `"claim_generation":null`, 1), false},
		{"missing_head", strings.Replace(valid, `"head":"",`, "", 1), false},
		{"missing_published", strings.Replace(valid, `,"published":false`, "", 1), false},
		{"published_null", strings.Replace(valid, "false", "null", 1), false},
		{"fraction", strings.Replace(valid, `"iteration_count":5`, `"iteration_count":5.1`, 1), false},
		{"overshoot", strings.Replace(valid, `"iteration_count":5`, `"iteration_count":7`, 1), false},
		{"below", strings.Replace(valid, `"iteration_count":5`, `"iteration_count":4`, 1), false},
		{"zero", strings.Replace(valid, `"max_iterations":5`, `"max_iterations":0`, 1), false},
		{"overflow", strings.Replace(valid, `"max_iterations":5`, `"max_iterations":2147483648`, 1), false},
		{"generation_overflow", strings.Replace(valid, `"claim_generation":4`, `"claim_generation":9223372036854775808`, 1), false},
		{"unknown", strings.Replace(valid, "}", `,"extra":true}`, 1), false},
		{"durability_null", strings.Replace(valid, "}", `,"checkpoint_contains_latest":null}`, 1), false},
		{"durability", strings.Replace(valid, "}", `,"checkpoint_contains_latest":false}`, 1), true},
		{"utf8_bytes", strings.Replace(valid, `"head":""`, `"head":"`+strings.Repeat("é", 65)+`"`, 1), false},
		{"body_limit", valid + strings.Repeat(" ", 16<<10), false},
		{"trailing_json", valid + " {}", false},
		{"invalid_utf8", strings.Replace(valid, `"head":""`, `"head":"`+string([]byte{255})+`"`, 1), false},
	}
	// Exercise the public handler seam. Valid evidence reaches the service's
	// transaction-authority refusal (500); invalid evidence must stop at 400.
	h := &Handler{wsvc: &workersvc.Service{}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.WorkerRunIterationPark(rec, workerReq(http.MethodPost, tc.body, uuid.New()))
			want := http.StatusBadRequest
			if tc.valid {
				want = http.StatusInternalServerError
			}
			if rec.Code != want {
				t.Fatalf("status=%d want %d: %s", rec.Code, want, rec.Body.String())
			}
		})
	}
}
