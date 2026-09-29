package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1906 M1: `uzi admin egress-profile list|show`, the read-only CLI over the admin
// egress-profile reads.

func egressFake() *uzicli.FakeClient {
	ts := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	return &uzicli.FakeClient{EgressProfiles: []apitypes.EgressProfileDTO{
		{
			ID: "p1", Name: "vendor-docs", Description: "Vendor X official docs",
			Hosts:                  []string{"docs.vendor.com", "*.cdn.vendor.com", "github.com"},
			MultiPublisherOverride: []string{"github.com"},
			Warnings: []apitypes.EgressProfileWarningDTO{{
				Entry: "github.com", Code: "multi_publisher_override",
				Message: "github.com is code hosting: many publishers share this host",
			}},
			CreatedAt: ts, UpdatedAt: ts.Add(time.Hour),
		},
		{ID: "p2", Name: "empty-desc", Hosts: []string{"kb.example.com"}, CreatedAt: ts, UpdatedAt: ts},
	}}
}

func TestAdminEgressProfileListTable(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(egressFake()), "admin", "egress-profile", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if got, want := strings.Fields(header), []string{"NAME", "HOSTS", "OVERRIDES", "UPDATED", "DESCRIPTION"}; !equalStringSlices(got, want) {
		t.Fatalf("header = %v, want %v", got, want)
	}
	for _, want := range []string{"vendor-docs", "Vendor X official docs", "2026-09-29T11:00:00Z", "empty-desc"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	// vendor-docs row: 3 hosts, 1 override.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "vendor-docs") {
			if f := strings.Fields(line); len(f) < 3 || f[1] != "3" || f[2] != "1" {
				t.Errorf("vendor-docs row = %q, want HOSTS 3 and OVERRIDES 1", line)
			}
		}
		if strings.HasPrefix(line, "empty-desc") && !strings.HasSuffix(strings.TrimSpace(line), "-") {
			t.Errorf("empty description row = %q, want a trailing -", line)
		}
	}
}

func TestAdminEgressProfileListJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(egressFake()), "--json", "admin", "egress-profile", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got []apitypes.EgressProfileDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a profile list: %v\n%s", err, out)
	}
	if len(got) != 2 || got[0].Name != "vendor-docs" || len(got[0].Hosts) != 3 {
		t.Fatalf("--json = %+v", got)
	}
}

func TestAdminEgressProfileShow(t *testing.T) {
	fc := egressFake()
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "egress-profile", "show", "vendor-docs")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastEgressProfileName != "vendor-docs" {
		t.Fatalf("forwarded name = %q, want vendor-docs", fc.LastEgressProfileName)
	}
	for _, want := range []string{
		"Vendor X official docs", "HOST", "OVERRIDE", "docs.vendor.com", "*.cdn.vendor.com",
		"warning: github.com is code hosting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "github.com" && f[1] != "yes" {
			t.Errorf("github.com row = %q, want OVERRIDE yes", line)
		}
		if len(f) == 2 && f[0] == "docs.vendor.com" && f[1] != "-" {
			t.Errorf("docs.vendor.com row = %q, want OVERRIDE -", line)
		}
	}
}

func TestAdminEgressProfileShowJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(egressFake()), "--json", "admin", "egress-profile", "show", "vendor-docs")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.EgressProfileDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a profile: %v\n%s", err, out)
	}
	if got.Name != "vendor-docs" || len(got.Warnings) != 1 || got.MultiPublisherOverride[0] != "github.com" {
		t.Fatalf("--json = %+v", got)
	}
}

func TestAdminEgressProfileShowMissingExit4(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(egressFake()), "admin", "egress-profile", "show", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// The render site is the trust boundary: a hostile server's name, description, host and
// warning text must not put a terminal escape, a bidi override or a forged line on screen.
func TestAdminEgressProfileRendersUntrustedFieldsSafely(t *testing.T) {
	hostile := "evil\x1b]0;pwned\x07\x1b[2J\u202egnp.exe\nFORGED-ROW"
	fc := &uzicli.FakeClient{EgressProfiles: []apitypes.EgressProfileDTO{{
		Name: hostile, Description: hostile, Hosts: []string{hostile}, MultiPublisherOverride: []string{hostile},
		Warnings: []apitypes.EgressProfileWarningDTO{{Entry: hostile, Message: hostile}},
	}}}
	listOut, _, code := runCLI(t, fakeEnv(fc), "admin", "egress-profile", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("list exit = %d", code)
	}
	showOut, _, code := runCLI(t, fakeEnv(fc), "admin", "egress-profile", "show", hostile)
	if code != uzicli.ExitOK {
		t.Fatalf("show exit = %d", code)
	}
	for label, out := range map[string]string{"list": listOut, "show": showOut} {
		if strings.ContainsAny(out, "\x1b\x07\u202e") {
			t.Errorf("%s output carries a control or bidi character: %q", label, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "FORGED-ROW") {
				t.Errorf("%s output let an embedded newline start a forged line: %q", label, out)
			}
		}
	}
}

// `show` prints a host and a description up to their validated maxima (253 and 500
// characters) whole: the 200-rune summary cap of cellText would cut a legal entry, and an
// admin reviewing a site list must see the exact host. Past the maximum (only a hostile or
// broken server sends that) the cell is still bounded.
func TestAdminEgressProfileShowPrintsLongFieldsWhole(t *testing.T) {
	longHost := strings.Repeat("a", 61) + "." + strings.Repeat("b", 61) + "." + strings.Repeat("c", 61) + "." + strings.Repeat("d", 63) + ".com"
	if len(longHost) != 253 {
		t.Fatalf("fixture host is %d characters, want 253", len(longHost))
	}
	longDesc := strings.Repeat("é", 499) + "Z"
	fc := &uzicli.FakeClient{EgressProfiles: []apitypes.EgressProfileDTO{{
		Name: "long", Description: longDesc, Hosts: []string{longHost},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "egress-profile", "show", "long")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, longHost) {
		t.Errorf("show cut the 253-character host:\n%s", out)
	}
	if !strings.Contains(out, longDesc) {
		t.Errorf("show cut the 500-character description:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("show truncated a field within its maximum:\n%s", out)
	}

	tooLong := strings.Repeat("x", 600)
	if got := fullCell(tooLong, 500); len([]rune(got)) != 500 || !strings.HasSuffix(got, "…") {
		t.Errorf("fullCell(600 runes, 500) = %d runes, want 500 ending in an ellipsis", len([]rune(got)))
	}
}
