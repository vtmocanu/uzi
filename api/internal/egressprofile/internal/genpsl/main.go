// Command genpsl writes api/internal/egressprofile/psl_ancestors_gen.go: the set of every
// domain that is a PROPER ANCESTOR of a Public Suffix List rule, derived from the PSL
// table that the pinned golang.org/x/net module ships (PRD #1906 Decision 4).
//
// Why: egressprofile refuses a wildcard "*.base" when base is itself a public suffix, and
// also when a public suffix lies BELOW base ("*.kawasaki.jp" covers the ICANN wildcard
// rule "*.kawasaki.jp"; "*.run.app" covers the private rule "a.run.app"). x/net's
// publicsuffix package answers "what is the suffix of this name" but does not export its
// rule list, so "is any rule under base" cannot be asked of it. This program decodes the
// same table x/net embeds (publicsuffix/data/{text,nodes,children}, laid out as
// publicsuffix/table.go describes and publicsuffix/list.go reads it) from the module
// directory `go list -m` reports, and writes the ancestor set as a Go map.
//
// Run it with `go generate ./internal/egressprofile/` from api/. The test in this
// directory re-derives the file from the module data and fails on any difference, so a
// bump of golang.org/x/net that changes the list forces a regeneration in the same PR.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// outFile is the generated file, relative to the egressprofile package directory (the
// working directory `go generate` runs this program in).
const outFile = "psl_ancestors_gen.go"

func main() {
	src, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genpsl:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outFile, src, 0o644); err != nil { //nolint:gosec // G306: a tracked Go source file, world-readable like every other one
		fmt.Fprintln(os.Stderr, "genpsl:", err)
		os.Exit(1)
	}
}

// module is the pinned x/net module as `go list -m -json` reports it.
type module struct {
	Version string
	Dir     string
}

// xnetModule locates the golang.org/x/net module the api build resolves.
func xnetModule() (module, error) {
	cmd := exec.Command("go", "list", "-m", "-json", "golang.org/x/net")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return module{}, fmt.Errorf("go list -m golang.org/x/net: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var m module
	if err := json.Unmarshal(out, &m); err != nil {
		return module{}, fmt.Errorf("decode go list output: %w", err)
	}
	if m.Dir == "" || m.Version == "" {
		return module{}, fmt.Errorf("go list reported no directory or version for golang.org/x/net (is the module downloaded?)")
	}
	return m, nil
}

// render produces the generated file's bytes from the pinned module's PSL table.
func render() ([]byte, error) {
	m, err := xnetModule()
	if err != nil {
		return nil, err
	}
	t, err := loadTable(filepath.Join(m.Dir, "publicsuffix"))
	if err != nil {
		return nil, err
	}
	nodes, err := t.walk()
	if err != nil {
		return nil, err
	}
	return renderFile(m.Version, t.version, ancestors(nodes))
}

// table is the decoded x/net PSL table: its layout constants, read from table.go, and the
// three data files.
type table struct {
	version  string
	consts   map[string]uint64
	text     string
	nodes    []byte
	children []byte
}

// layoutConsts are the table.go constants the decoder needs. A missing one fails the load,
// so an x/net layout change stops generation instead of decoding garbage.
var layoutConsts = []string{
	"nodesBits", "nodesBitsChildren", "nodesBitsICANN", "nodesBitsTextOffset", "nodesBitsTextLength",
	"childrenBitsWildcard", "childrenBitsNodeType", "childrenBitsHi", "childrenBitsLo",
	"nodeTypeNormal", "nodeTypeException", "nodeTypeParentOnly", "numTLD",
}

func loadTable(dir string) (table, error) {
	t := table{consts: map[string]uint64{}}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(dir, "table.go"), nil, 0)
	if err != nil {
		return t, fmt.Errorf("parse table.go: %w", err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				switch lit.Kind {
				case token.INT:
					v, err := strconv.ParseUint(lit.Value, 0, 64)
					if err != nil {
						return t, fmt.Errorf("table.go const %s: %w", n.Name, err)
					}
					t.consts[n.Name] = v
				case token.STRING:
					if n.Name == "version" {
						if t.version, err = strconv.Unquote(lit.Value); err != nil {
							return t, fmt.Errorf("table.go const version: %w", err)
						}
					}
				}
			}
		}
	}
	for _, c := range layoutConsts {
		if _, ok := t.consts[c]; !ok {
			return t, fmt.Errorf("table.go has no integer constant %s: the x/net table layout changed, update genpsl", c)
		}
	}
	if t.version == "" {
		return t, fmt.Errorf("table.go has no version constant")
	}
	if t.consts["nodesBits"] != 40 {
		return t, fmt.Errorf("table.go nodesBits = %d, genpsl decodes 40-bit nodes: update genpsl", t.consts["nodesBits"])
	}
	// os.Root confines the three reads to the module's data directory.
	root, err := os.OpenRoot(filepath.Join(dir, "data"))
	if err != nil {
		return t, err
	}
	defer func() { _ = root.Close() }()
	text, err := root.ReadFile("text")
	if err != nil {
		return t, err
	}
	t.text = string(text)
	if t.nodes, err = root.ReadFile("nodes"); err != nil {
		return t, err
	}
	if t.children, err = root.ReadFile("children"); err != nil {
		return t, err
	}
	if len(t.nodes)%5 != 0 || len(t.children)%4 != 0 {
		return t, fmt.Errorf("data/nodes (%d bytes) or data/children (%d bytes) is not a whole number of records", len(t.nodes), len(t.children))
	}
	return t, nil
}

func mask(bits uint64) uint64 { return 1<<bits - 1 }

// node is one decoded trie node: the full domain name it stands for and its rule shape.
type node struct {
	name     string
	nodeType uint64
	wildcard bool
	lo, hi   uint64
}

// decode reads node i the way publicsuffix/list.go does (nodeLabel and PublicSuffix).
func (t table) decode(i uint64, parent string) (node, error) {
	c := t.consts
	if (i+1)*5 > uint64(len(t.nodes)) {
		return node{}, fmt.Errorf("node index %d out of range", i)
	}
	b := t.nodes[i*5 : i*5+5]
	x := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	length := x & mask(c["nodesBitsTextLength"])
	x >>= c["nodesBitsTextLength"]
	offset := x & mask(c["nodesBitsTextOffset"])
	x >>= c["nodesBitsTextOffset"]
	x >>= c["nodesBitsICANN"]
	ci := x & mask(c["nodesBitsChildren"])
	if offset+length > uint64(len(t.text)) || (ci+1)*4 > uint64(len(t.children)) {
		return node{}, fmt.Errorf("node %d points outside the table", i)
	}
	label := t.text[offset : offset+length]
	cb := t.children[ci*4 : ci*4+4]
	u := uint64(cb[0])<<24 | uint64(cb[1])<<16 | uint64(cb[2])<<8 | uint64(cb[3])
	n := node{name: label}
	if parent != "" {
		n.name = label + "." + parent
	}
	n.lo = u & mask(c["childrenBitsLo"])
	u >>= c["childrenBitsLo"]
	n.hi = u & mask(c["childrenBitsHi"])
	u >>= c["childrenBitsHi"]
	n.nodeType = u & mask(c["childrenBitsNodeType"])
	u >>= c["childrenBitsNodeType"]
	n.wildcard = u&mask(c["childrenBitsWildcard"]) != 0
	return n, nil
}

// walk returns every node in the trie, depth first from the top-level domains.
func (t table) walk() ([]node, error) {
	var out []node
	var visit func(lo, hi uint64, parent string, depth int) error
	visit = func(lo, hi uint64, parent string, depth int) error {
		if depth > 16 {
			return fmt.Errorf("trie deeper than 16 labels under %q: corrupt table", parent)
		}
		for i := lo; i < hi; i++ {
			n, err := t.decode(i, parent)
			if err != nil {
				return err
			}
			out = append(out, n)
			if err := visit(n.lo, n.hi, n.name, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(0, t.consts["numTLD"], "", 0); err != nil {
		return nil, err
	}
	return out, nil
}

// ancestors is every name that has a rule strictly below it: a node with children, or a
// node carrying the wildcard bit ("*.kawasaki.jp" makes every child of kawasaki.jp a
// suffix). An exception node ("!city.kawasaki.jp") marks a registrable name, so it counts
// only when it has children of its own.
func ancestors(nodes []node) []string {
	var out []string
	for _, n := range nodes {
		if n.hi > n.lo || n.wildcard {
			out = append(out, n.name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func renderFile(xnetVersion, pslVersion string, names []string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by internal/genpsl from golang.org/x/net " + xnetVersion + "; DO NOT EDIT.\n\n")
	b.WriteString("package egressprofile\n\n")
	b.WriteString("// pslSource names the Public Suffix List snapshot pslRuleAncestors was derived from.\n")
	fmt.Fprintf(&b, "const pslSource = %q\n\n", "golang.org/x/net "+xnetVersion+": "+pslVersion)
	b.WriteString("// pslRuleAncestors is every domain with a Public Suffix List rule (ICANN or private)\n")
	b.WriteString("// strictly below it. A wildcard over one of these would cover a public suffix.\n")
	b.WriteString("var pslRuleAncestors = map[string]struct{}{\n")
	for _, n := range names {
		fmt.Fprintf(&b, "\t%q: {},\n", n)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
