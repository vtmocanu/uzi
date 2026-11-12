package issuetracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestPagingWalksEveryPage(t *testing.T) {
	y, srv := newFakeDriver(t)
	for i := 0; i < pageSize+5; i++ {
		srv.SeedIssue("i", "")
	}
	got, err := y.ListIssues(context.Background(), 1, forge.ListIssuesOptions{})
	if err != nil || len(got) != pageSize+5 {
		t.Fatalf("got %d issues, %v", len(got), err)
	}
}

// eightCharWindows lists every 8-byte substring of s.
func eightCharWindows(s string) []string {
	var out []string
	for i := 0; i+8 <= len(s); i++ {
		out = append(out, s[i:i+8])
	}
	return out
}

func assertNoTokenWindow(t *testing.T, msg string) {
	t.Helper()
	for _, w := range eightCharWindows(credential()) {
		if strings.Contains(msg, w) {
			t.Fatalf("message carries the token fragment %q: %q", w, msg)
		}
	}
}

// TestErrorTextScrubsBeforeCutting puts a non-JSON error body's token at
// offsets around and beyond the 200-byte cut.
func TestErrorTextScrubsBeforeCutting(t *testing.T) {
	var offsets []int
	for o := 0; o <= 230; o++ {
		offsets = append(offsets, o)
	}
	offsets = append(offsets, 400, 1000, 3000)
	var pad int
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("x", pad) + credential()))
	}))
	for _, o := range offsets {
		pad = o
		_, err := y.VerifyToken(context.Background())
		if err == nil {
			t.Fatal("want an error")
		}
		assertNoTokenWindow(t, err.Error())
	}
}

func TestErrorTextDescriptionOnly(t *testing.T) {
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeRaw(w, `{"error_description":"only this"}`)
	}))
	_, err := y.VerifyToken(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "HTTP 400: only this") {
		t.Fatalf("err = %v, want a message ending in \"HTTP 400: only this\"", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorIsScrubbed(t *testing.T) {
	y, err := newYouTrackOver(Options{BaseURL: "https://yt.example.com", Token: credential()},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed with %s", credential())
		}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = y.VerifyToken(context.Background())
	assertScrubbed(t, err)
}

// TestDecodeErrorIsScrubbed uses a numeric token, because the JSON decoder
// echoes an offending number in its error.
func TestDecodeErrorIsScrubbed(t *testing.T) {
	const num = "99999999999999999999"
	y, _ := tlsDriverToken(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, `{"id":`+num+`}`)
	}), num)
	_, err := y.VerifyToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want a decode error", err)
	}
	if strings.Contains(err.Error(), num) {
		t.Fatalf("decode error carries the token: %v", err)
	}
}

func TestOversizedErrorBodyKeepsItsClass(t *testing.T) {
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(credential() + strings.Repeat("x", int(errorBodyBytes))))
	}))
	_, err := y.VerifyToken(context.Background())
	assertScrubbed(t, err)
	if !errors.Is(err, forge.ErrResponseTooLarge) || forge.Class(err) != forge.ErrorClassServerError {
		t.Fatalf("err = %v (class %v), want ErrResponseTooLarge, server error", err, forge.Class(err))
	}
}

func TestListingByteCeilingCountsEveryPage(t *testing.T) {
	old := maxTotalBytes
	maxTotalBytes = 10_000
	t.Cleanup(func() { maxTotalBytes = old })
	project := `{"id":"0-1","shortName":"UZI","name":"Uzi"}`
	page := "[" + strings.TrimSuffix(strings.Repeat(project+",", pageSize), ",") + "]"
	if len(page) >= int(listPageBytes) || len(page) > maxTotalBytes {
		t.Fatalf("fixture page is %d bytes", len(page))
	}
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, page) }))
	_, err := y.ListProjects(context.Background())
	if err == nil || !strings.Contains(err.Error(), "byte ceiling") {
		t.Fatalf("err = %v, want the byte ceiling", err)
	}
}
