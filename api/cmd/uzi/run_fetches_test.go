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
