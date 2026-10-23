package handler

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerSourcePublicationStrictShape(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, report string
		valid        bool
	}{
		{"false", fmt.Sprintf(`{"version":1,"published_head":%q,"non_closing":false}`, head), true},
		{"true", fmt.Sprintf(`{"version":1,"published_head":%q,"non_closing":true}`, head), true},
		{"missing_bool", fmt.Sprintf(`{"version":1,"published_head":%q}`, head), false},
		{"null_bool", fmt.Sprintf(`{"version":1,"published_head":%q,"non_closing":null}`, head), false},
		{"bad_version", fmt.Sprintf(`{"version":2,"published_head":%q,"non_closing":false}`, head), false},
		{"bad_head", `{"version":1,"published_head":"bad","non_closing":false}`, false},
		{"unknown", fmt.Sprintf(`{"version":1,"published_head":%q,"non_closing":false,"extra":1}`, head), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"status":"completed","source_publication":` + tc.report + "}"
			req := httptest.NewRequest("POST", "/state", strings.NewReader(body))
			var state workersvc.StateRequest
			err := httpx.DecodeJSONStrict(req, &state)
			if (err == nil) != tc.valid {
				t.Fatalf("decode %s: %v", body, err)
			}
			if tc.valid && state.SourcePublication == nil {
				t.Fatal("report omitted")
			}
		})
	}
}
