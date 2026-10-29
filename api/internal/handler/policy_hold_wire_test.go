package handler

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPolicyHoldSharedWireCases(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/policy-hold/requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string          `json:"name"`
		Valid bool            `json:"valid"`
		Body  json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty shared policy fixture")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(string(tc.Body)))
			var body workersvc.PolicyHoldRequest
			err := httpx.DecodeJSONStrictBounded(req, &body, 17*1024)
			if err == nil {
				err = body.Validate()
			}
			if (err == nil) != tc.Valid {
				t.Fatalf("accepted=%v want %v: %v", err == nil, tc.Valid, err)
			}
		})
	}
}
