package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RunFetches sends the cursor as ?after= (query-escaped) and nothing on the first page.
func TestRunFetchesSendsCursor(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fetches":null,"next_cursor":"c1"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	first, err := c.RunFetches(context.Background(), "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Fetches == nil || first.NextCursor != "c1" {
		t.Fatalf("first page = %+v, want an empty array and the cursor", first)
	}
	if _, err := c.RunFetches(context.Background(), "r1", "a b&c"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/runs/r1/fetches", "/api/runs/r1/fetches?after=a+b%26c"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}
