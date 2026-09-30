package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1906 M3: `uzi run fetches <run>`, the owner read of a research run's source log.

func fetchesFake() *uzicli.FakeClient {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return &uzicli.FakeClient{RunFetchesResult: map[string]apitypes.RunFetchesDTO{
		"r1": {Fetches: []apitypes.RunFetchDTO{
			{
				ID: "f1", URL: "https://docs.vendor.com/guide.pdf", FinalURL: "https://docs.vendor.com/v2/guide.pdf",
				Verdict: "allowed", HTTPStatus: 200, ContentType: "application/pdf", Bytes: 12345,
				SHA256: strings.Repeat("a", 64), StartedAt: ts, FinishedAt: ts.Add(time.Second), CreatedAt: ts,
			},
			{
				// A hostile server's row: an escape sequence and a bidi override in the
				// site-controlled fields must not reach the terminal.
				ID: "f2", URL: "https://evil.example/\x1b[2Jx\u202Ey", Verdict: "refused",
				Reason: "off_list\x1b]0;pwn\x07", ContentType: "text/html\u202E",
				StartedAt: ts, FinishedAt: ts, CreatedAt: ts,
			},
		}},
	}}
}

func TestRunFetchesTable(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(fetchesFake()), "run", "fetches", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if got, want := strings.Fields(header), []string{"STARTED", "VERDICT", "REASON", "HTTP", "BYTES", "CONTENT", "TYPE", "URL", "FINAL", "URL"}; !equalStringSlices(got, want) {
		t.Fatalf("header = %v, want %v", got, want)
	}
	for _, want := range []string{"2026-09-29T12:00:00Z", "allowed", "200", "12345", "application/pdf", "https://docs.vendor.com/v2/guide.pdf", "refused", "off_list"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07\u202E") {
		t.Fatalf("a control or bidi rune reached the terminal: %q", out)
	}
}

func TestRunFetchesJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(fetchesFake()), "--json", "run", "fetches", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.RunFetchesDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(got.Fetches) != 2 || got.Fetches[0].SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("json = %+v", got)
	}
}

func TestRunFetchesEmptyJSONIsArray(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "--json", "run", "fetches", "r0")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"fetches": []`) {
		t.Fatalf("empty log = %s, want an empty array", out)
	}
}

func TestRunFetchesNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{RunFetchesErr: uzicli.Exitf(uzicli.ExitNotFound, "run not found")}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "fetches", "r9")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// The command follows next_cursor across pages and prints the whole log once, with no
// cursor left in the JSON.
func TestRunFetchesFollowsPages(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	row := func(id string) apitypes.RunFetchDTO {
		return apitypes.RunFetchDTO{ID: id, URL: "https://docs.example/" + id, Verdict: "allowed", StartedAt: ts, FinishedAt: ts, CreatedAt: ts}
	}
	fc := &uzicli.FakeClient{RunFetchesResult: map[string]apitypes.RunFetchesDTO{
		"r1":          {Fetches: []apitypes.RunFetchDTO{row("f1"), row("f2")}, NextCursor: "f2"},
		"r1?after=f2": {Fetches: []apitypes.RunFetchDTO{row("f3")}, NextCursor: "f3"},
		"r1?after=f3": {Fetches: []apitypes.RunFetchDTO{row("f4")}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "--json", "run", "fetches", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if want := []string{"r1", "r1?after=f2", "r1?after=f3"}; !equalStringSlices(fc.RunFetchesCalls, want) {
		t.Fatalf("pages requested = %q, want %q", fc.RunFetchesCalls, want)
	}
	var got apitypes.RunFetchesDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(got.Fetches))
	for _, f := range got.Fetches {
		ids = append(ids, f.ID)
	}
	if !equalStringSlices(ids, []string{"f1", "f2", "f3", "f4"}) || got.NextCursor != "" || strings.Contains(out, "next_cursor") {
		t.Fatalf("json = %s, want all four rows and no cursor", out)
	}
}

// A server whose pagination does not advance (a repeated cursor, or a cursor on an empty
// page) is an error, not an endless loop.
func TestRunFetchesRefusesStuckPagination(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	row := apitypes.RunFetchDTO{ID: "f1", Verdict: "allowed", StartedAt: ts, FinishedAt: ts, CreatedAt: ts}
	for name, pages := range map[string]map[string]apitypes.RunFetchesDTO{
		"repeated cursor": {
			"r1":          {Fetches: []apitypes.RunFetchDTO{row}, NextCursor: "f1"},
			"r1?after=f1": {Fetches: []apitypes.RunFetchDTO{row}, NextCursor: "f1"},
		},
		"empty page with cursor": {
			"r1": {Fetches: []apitypes.RunFetchDTO{}, NextCursor: "f1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RunFetchesResult: pages}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "fetches", "r1")
			if code == uzicli.ExitOK {
				t.Fatal("exit = 0 on a pagination that does not advance")
			}
			if len(fc.RunFetchesCalls) > 2 {
				t.Fatalf("kept paging: %q", fc.RunFetchesCalls)
			}
		})
	}
}
