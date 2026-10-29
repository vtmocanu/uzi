package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminHealthAttentionCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		want       bool
	}{
		{name: "old document"},
		{name: "explicit false", flag: `,"attention_banner":false`},
		{name: "attention", flag: `,"attention_banner":true`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"status":"warn","blocking":false,"counts":{"ok":0,"warn":1,"danger":0,"unknown":0,"na":0},"future_field":true,"checks":[{"id":"recovery.storage","scope":"owner","severity":"warn","future_check_field":42` + tc.flag + `}]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/admin/health" {
					t.Errorf("request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			doc, err := newTestClient(srv).AdminHealth(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if doc.Status != "warn" || doc.Blocking || doc.Counts.Warn != 1 || len(doc.Checks) != 1 {
				t.Fatalf("document changed: %+v", doc)
			}
			c := doc.Checks[0]
			if c.ID != "recovery.storage" || c.Scope != "owner" || c.Severity != "warn" || c.AttentionBanner != tc.want {
				t.Fatalf("decoded check: %+v", c)
			}
		})
	}
}
