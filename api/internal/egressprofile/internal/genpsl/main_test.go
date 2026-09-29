package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/publicsuffix"
)

// TestGeneratedFileUpToDate re-derives psl_ancestors_gen.go from the golang.org/x/net
// module the build resolves and fails on any difference. A bump of x/net that changes the
// Public Suffix List therefore reddens here until `go generate ./internal/egressprofile/`
// is re-run, so the wildcard rule never checks against an older list than PublicSuffix.
func TestGeneratedFileUpToDate(t *testing.T) {
	want, err := render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", outFile))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("api/internal/egressprofile/%s is stale against the pinned golang.org/x/net; "+
			"run `go generate ./internal/egressprofile/` from api/ and commit the result", outFile)
	}
}

// TestDecoderAgreesWithXNet proves the decoder reads the table the way x/net does: every
// decoded rule is a public suffix by x/net's own PublicSuffix, every wildcard rule makes
// its children suffixes, and every exception is NOT a suffix. A decoder that misread a
// bit field would produce names x/net disagrees with.
func TestDecoderAgreesWithXNet(t *testing.T) {
	m, err := xnetModule()
	if err != nil {
		t.Fatal(err)
	}
	tb, err := loadTable(filepath.Join(m.Dir, "publicsuffix"))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := tb.walk()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) < 5000 {
		t.Fatalf("decoded %d nodes, want the whole list (thousands)", len(nodes))
	}
	var normal, wild, except int
	for _, n := range nodes {
		switch n.nodeType {
		case tb.consts["nodeTypeNormal"]:
			normal++
			if ps, _ := publicsuffix.PublicSuffix(n.name); ps != n.name {
				t.Errorf("decoded rule %q: x/net PublicSuffix = %q", n.name, ps)
			}
		case tb.consts["nodeTypeException"]:
			except++
			if ps, _ := publicsuffix.PublicSuffix(n.name); ps == n.name {
				t.Errorf("decoded exception %q: x/net says it is a public suffix", n.name)
			}
		}
		if n.wildcard {
			wild++
			probe := "zz-genpsl-probe." + n.name
			if ps, _ := publicsuffix.PublicSuffix(probe); ps != probe {
				t.Errorf("decoded wildcard *.%s: x/net PublicSuffix(%q) = %q", n.name, probe, ps)
			}
		}
	}
	if normal == 0 || wild == 0 || except == 0 {
		t.Fatalf("decoded %d rules, %d wildcards, %d exceptions: want all three kinds", normal, wild, except)
	}
	// The cases the ancestor set exists for (PRD #1906 review): a rule BELOW the base.
	set := map[string]bool{}
	for _, a := range ancestors(nodes) {
		set[a] = true
	}
	for _, want := range []string{"kawasaki.jp", "sch.uk", "run.app", "digitaloceanspaces.com", "amazonaws.com", "com"} {
		if !set[want] {
			t.Errorf("ancestor set lacks %q", want)
		}
	}
	for _, not := range []string{"example.com", "zz-genpsl-probe.com"} {
		if set[not] {
			t.Errorf("ancestor set holds %q, which has no rule below it", not)
		}
	}
}
