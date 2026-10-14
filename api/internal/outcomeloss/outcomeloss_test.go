package outcomeloss

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const fixtureDir = "../../../fixtures/outcome-discard/"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtureDir + name) //nolint:gosec // G304: a test reading its own fixture directory by constant name
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureManifest(t *testing.T) (*apitypes.OutcomeLossManifest, []byte) {
	t.Helper()
	raw := bytes.TrimSuffix(readFixture(t, "canonical.json"), []byte("\n"))
	var m apitypes.OutcomeLossManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return &m, raw
}

// The shared fixture pins the canonical bytes and the digest on both the Go and TS sides.
func TestFixtureCanonicalBytesAndDigest(t *testing.T) {
	m, raw := fixtureManifest(t)
	got, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("canonical bytes drifted from fixtures/outcome-discard/canonical.json:\n got %s\nwant %s", got, raw)
	}
	want := strings.TrimSuffix(string(readFixture(t, "digest.txt")), "\n")
	if d := Digest(got); d != want {
		t.Fatalf("digest = %s, want %s", d, want)
	}
}

func TestFixtureEscapeCases(t *testing.T) {
	var cases []struct{ Hex, Escaped string }
	if err := json.Unmarshal(readFixture(t, "escape-cases.json"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		b, err := hex.DecodeString(c.Hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := Escape(b); got != c.Escaped {
			t.Errorf("Escape(%s) = %q, want %q", c.Hex, got, c.Escaped)
		}
		if !ValidEscaped(c.Escaped, 1024) {
			t.Errorf("ValidEscaped(%q) = false", c.Escaped)
		}
	}
}

func TestValidEscapedGrammar(t *testing.T) {
	for _, s := range []string{"a\\", "\\x4", "\\xAB", "\\x41", "\\x5c", "\\n", "café", "tab\t", "del\x7f", "\\xg0", "\\X41"} {
		if ValidEscaped(s, 100) {
			t.Errorf("ValidEscaped(%q) = true, want false", s)
		}
	}
	for _, s := range []string{"", "a\\\\b", "\\x00", "\\x1f", "\\x7f", "\\x80", "\\xff", "\\\\x41"} {
		if !ValidEscaped(s, 100) {
			t.Errorf("ValidEscaped(%q) = false, want true", s)
		}
	}
	if ValidEscaped(strings.Repeat("a", 11), 10) {
		t.Error("length cap not enforced")
	}
}

func TestEscapeRoundTripsEveryByteValue(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	e := Escape(all)
	if !ValidEscaped(e, 4096) {
		t.Fatalf("Escape output is not valid escaped ASCII: %q", e)
	}
	if strings.Contains(e, "\\x5c") || strings.Contains(e, "\\x41") {
		t.Fatalf("over-escaped: %q", e)
	}
}

func TestValidateRejections(t *testing.T) {
	base, _ := fixtureManifest(t)
	clone := func() *apitypes.OutcomeLossManifest {
		b, _ := json.Marshal(base)
		var m apitypes.OutcomeLossManifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	cases := map[string]func(m *apitypes.OutcomeLossManifest){
		"version":            func(m *apitypes.OutcomeLossManifest) { m.Version = 2 },
		"raw non ascii path": func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[0].Path = "café" },
		"uppercase escape":   func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[0].Path = "\\xC3" },
		"bad sha": func(m *apitypes.OutcomeLossManifest) {
			m.Clones[0].Paths[0].SHA256 = strings.ToUpper(m.Clones[0].Paths[0].SHA256)
		},
		"negative size": func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[0].Size = -1 },
		"unsafe size":   func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[0].Size = MaxSafeInt + 1 },
		"unsorted paths": func(m *apitypes.OutcomeLossManifest) {
			p := m.Clones[0].Paths
			p[0], p[1] = p[1], p[0]
		},
		"duplicate path":    func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[1].Path = m.Clones[0].Paths[0].Path },
		"unsorted commits":  func(m *apitypes.OutcomeLossManifest) { c := m.Clones[0].UnpublishedCommits; c[0], c[1] = c[1], c[0] },
		"dup sibling":       func(m *apitypes.OutcomeLossManifest) { m.Siblings[1].Name = m.Siblings[0].Name },
		"null paths":        func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths = nil },
		"deleted with sha":  func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[4].SHA256 = m.Clones[0].Paths[0].SHA256 },
		"symlink no target": func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[5].LinkTarget = "" },
		"bad reason":        func(m *apitypes.OutcomeLossManifest) { m.BlockedReason = "other" },
		"bad status":        func(m *apitypes.OutcomeLossManifest) { m.Clones[0].Paths[0].Status = "Z?" },
		"too many commits": func(m *apitypes.OutcomeLossManifest) {
			c := make([]string, MaxCommits+1)
			for i := range c {
				c[i] = strings.Repeat("0", 32) + hex.EncodeToString([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
			}
			m.Clones[0].UnpublishedCommits = c
		},
		"hashed cap": func(m *apitypes.OutcomeLossManifest) {
			m.Clones[0].Paths[0].Size = MaxSafeInt
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := clone()
			mutate(m)
			if _, err := Canonical(m); err == nil {
				t.Fatal("manifest accepted")
			}
		})
	}
	if _, err := Canonical(clone()); err != nil {
		t.Fatalf("control: %v", err)
	}
}

func TestCanonicalSizeCap(t *testing.T) {
	base, _ := fixtureManifest(t)
	m := *base
	big := base.Clones[0]
	big.Paths = nil
	for i := 0; i < MaxPathEntries; i++ {
		big.Paths = append(big.Paths, apitypes.OutcomeLossPathItem{
			Path: strings.Repeat("p", 250) + fmt.Sprintf("%04x", i), Status: "??", Kind: "file", Size: 1,
			SHA256: strings.Repeat("a", 64), OrigPath: strings.Repeat("o", 100),
		})
	}
	m.Clones = []apitypes.OutcomeLossClone{big}
	if _, err := Canonical(&m); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversize manifest accepted or wrong error: %v", err)
	}
}

func TestValidateReport(t *testing.T) {
	m, _ := fixtureManifest(t)
	wid := uuid.MustParse(m.WorkerID)
	_, digest, err := Sum(m)
	if err != nil {
		t.Fatal(err)
	}
	good := func() *apitypes.BlockedOutcomeReport {
		return &apitypes.BlockedOutcomeReport{
			RunID: m.RunID, ClaimGeneration: m.ClaimGeneration, JournalSHA256: m.JournalSHA256, BlockedReason: m.BlockedReason,
			LossComplete: true, LossSHA256: digest, Loss: m,
		}
	}
	if got, err := ValidateReport(wid, good()); err != nil || got != digest {
		t.Fatalf("good report: %q %v", got, err)
	}
	for name, mutate := range map[string]func(r *apitypes.BlockedOutcomeReport){
		"digest mismatch": func(r *apitypes.BlockedOutcomeReport) { r.LossSHA256 = strings.Repeat("0", 64) },
		"journal sha":     func(r *apitypes.BlockedOutcomeReport) { r.JournalSHA256 = strings.Repeat("1", 64) },
		"generation":      func(r *apitypes.BlockedOutcomeReport) { r.ClaimGeneration = 4 },
		"complete no loss": func(r *apitypes.BlockedOutcomeReport) {
			r.Loss = nil
		},
		"reason set while complete": func(r *apitypes.BlockedOutcomeReport) { r.IncompleteReason = "x" },
		"foreign worker":            nil,
	} {
		r := good()
		w := wid
		if mutate == nil {
			w = uuid.New()
		} else {
			mutate(r)
		}
		if _, err := ValidateReport(w, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	inc := &apitypes.BlockedOutcomeReport{RunID: m.RunID, ClaimGeneration: 3, JournalSHA256: m.JournalSHA256, BlockedReason: "run_not_owned", IncompleteReason: "clone_unreadable"}
	if got, err := ValidateReport(wid, inc); err != nil || got != "" {
		t.Fatalf("incomplete report: %q %v", got, err)
	}
	inc.IncompleteReason = ""
	if _, err := ValidateReport(wid, inc); err == nil {
		t.Error("incomplete report without a reason accepted")
	}
}

// Canonical output must not HTML-escape: json.Marshal would write < > & as < etc.
func TestCanonicalDoesNotHTMLEscape(t *testing.T) {
	m, _ := fixtureManifest(t)
	got, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("a=1&b=<2>")) || bytes.Contains(got, []byte("\\u00")) {
		t.Fatalf("canonical bytes are HTML-escaped: %s", got)
	}
	if got[len(got)-1] == '\n' {
		t.Fatal("trailing newline not stripped")
	}
}
