package fetchctl

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestGenerateCredential(t *testing.T) {
	a, ha, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, CredentialPrefix) || a == b {
		t.Fatalf("credentials %q %q: want distinct uzf_ tokens", a, b)
	}
	if len(a) != len(CredentialPrefix)+43 {
		t.Fatalf("credential length %d, want prefix + 43 (256 bits, raw base64url)", len(a))
	}
	sum := sha256.Sum256([]byte(a))
	if !bytes.Equal(ha, sum[:]) || !bytes.Equal(HashCredential(a), sum[:]) {
		t.Fatal("hash is not sha256 of the token")
	}
	if CredentialPrefix == "uzw_" {
		t.Fatal("the fetch credential must not share the worker join token prefix")
	}
}

func TestParseSnapshot(t *testing.T) {
	if _, err := ParseSnapshot(nil); err == nil {
		t.Fatal("a missing snapshot parsed")
	}
	if _, err := ParseSnapshot([]byte("{")); err == nil {
		t.Fatal("a malformed snapshot parsed")
	}
	s, err := ParseSnapshot([]byte(`{"profile":"p"}`))
	if err != nil || s.Profile != "p" || s.Entries == nil || len(s.Entries) != 0 {
		t.Fatalf("snapshot = %+v, %v; want profile p and an empty, non-nil entry list", s, err)
	}
}

func TestCredentialLive(t *testing.T) {
	live := store.LockFetchCredentialByHashRow{Status: "running", ClaimGeneration: 3, RunClaimGeneration: 3}
	if !credentialLive(live) {
		t.Fatal("a running, current, unrevoked credential is not live")
	}
	for name, mut := range map[string]func(*store.LockFetchCredentialByHashRow){
		"revoked":     func(c *store.LockFetchCredentialByHashRow) { c.RevokedAt.Valid = true },
		"stale gen":   func(c *store.LockFetchCredentialByHashRow) { c.ClaimGeneration = 2 },
		"claimed":     func(c *store.LockFetchCredentialByHashRow) { c.Status = "claimed" },
		"awaiting":    func(c *store.LockFetchCredentialByHashRow) { c.Status = "awaiting_input" },
		"terminal":    func(c *store.LockFetchCredentialByHashRow) { c.Status = "completed" },
		"newer claim": func(c *store.LockFetchCredentialByHashRow) { c.RunClaimGeneration = 4 },
	} {
		c := live
		mut(&c)
		if credentialLive(c) {
			t.Errorf("%s: credential is live", name)
		}
	}
}

func TestRefusalReason(t *testing.T) {
	caps := settings.FetchCaps{MaxFileBytes: 10, MaxRunBytes: 100, MaxRunFiles: 5, MaxConcurrentPerRun: 2, MaxRunAttempts: 50}
	cases := []struct {
		c    store.GetFetchCountersRow
		want string
	}{
		{store.GetFetchCountersRow{Inflight: 2, Files: 5}, AdmissionConcurrency},
		{store.GetFetchCountersRow{Inflight: 1, Files: 5}, AdmissionRunFiles},
		{store.GetFetchCountersRow{Inflight: 1, Files: 3, UsedBytes: 95}, AdmissionRunBytes},
	}
	for _, c := range cases {
		if got := refusalReason(c.c, caps); got != c.want {
			t.Errorf("refusalReason(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

func goodComplete() apitypes.FetcherCompleteRequest {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return apitypes.FetcherCompleteRequest{
		Credential: "uzf_x", ReservationID: "00000000-0000-0000-0000-000000000001",
		URL: "https://docs.example.com/a", FinalURL: "https://docs.example.com/a",
		Verdict: VerdictAllowed, HTTPStatus: 200, ContentType: "text/html", Bytes: 10,
		SHA256: strings.Repeat("0", 64), StartedAt: ts, FinishedAt: ts,
	}
}

func TestValidateComplete(t *testing.T) {
	if err := validateComplete(goodComplete()); err != nil {
		t.Fatalf("a good allowed record: %v", err)
	}
	refused := goodComplete()
	refused.Verdict, refused.Reason, refused.SHA256, refused.Bytes = VerdictRefused, "cancelled", "", 0
	if err := validateComplete(refused); err != nil {
		t.Fatalf("a good refused record (reason cancelled): %v", err)
	}
	for name, mut := range map[string]func(*apitypes.FetcherCompleteRequest){
		"verdict":          func(r *apitypes.FetcherCompleteRequest) { r.Verdict = "maybe" },
		"allowed reason":   func(r *apitypes.FetcherCompleteRequest) { r.Reason = "off_list" },
		"allowed no sha":   func(r *apitypes.FetcherCompleteRequest) { r.SHA256 = "" },
		"upper sha":        func(r *apitypes.FetcherCompleteRequest) { r.SHA256 = strings.Repeat("A", 64) },
		"refused unknown":  func(r *apitypes.FetcherCompleteRequest) { r.Verdict, r.Reason, r.SHA256 = VerdictRefused, "nope", "" },
		"refused with sha": func(r *apitypes.FetcherCompleteRequest) { r.Verdict, r.Reason = VerdictRefused, "off_list" },
		"no url":           func(r *apitypes.FetcherCompleteRequest) { r.URL = " " },
		"status":           func(r *apitypes.FetcherCompleteRequest) { r.HTTPStatus = 1000 },
		"negative bytes":   func(r *apitypes.FetcherCompleteRequest) { r.Bytes = -1 },
		"no started_at":    func(r *apitypes.FetcherCompleteRequest) { r.StartedAt = time.Time{} },
		"no finished_at":   func(r *apitypes.FetcherCompleteRequest) { r.FinishedAt = time.Time{} },
	} {
		r := goodComplete()
		mut(&r)
		var inv *InvalidRequestError
		if err := validateComplete(r); !errors.As(err, &inv) {
			t.Errorf("%s: validateComplete = %v, want an InvalidRequestError", name, err)
		}
	}
}
