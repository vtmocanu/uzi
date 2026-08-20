// Command astinert reports whether the difference between two Go source files
// is limited to comments and gofmt-absorbed formatting.
//
// It parses each file WITHOUT parser.ParseComments, reprints the resulting AST
// with go/printer, and compares the reprints byte-for-byte. A byte-identical
// reprint is a sound one-directional guarantee that only comments and
// gofmt-normalized formatting changed; anything reaching real syntax (including
// a statement that was commented out) makes the reprints differ.
//
// Exit codes mirror the repo's fmt-check:api convention:
//
//	0  INERT   — byte-identical reprints; only comments/formatting changed
//	1  DIFFERS — the reprints differ; code changed
//	2  usage error, or a file failed to parse (the instrument is broken)
package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
)

// reprintNoComments parses src with comments NOT parsed (mode 0) and returns
// the go/printer reprint of the resulting AST. A fresh FileSet per call keeps
// positions file-local so two independent files reprint comparably.
func reprintNoComments(name string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// inert reports whether a and b reprint to byte-identical source, i.e. whether
// b differs from a only in comments and formatting. Any parse error is
// propagated.
func inert(aName string, a []byte, bName string, b []byte) (bool, error) {
	ra, err := reprintNoComments(aName, a)
	if err != nil {
		return false, err
	}
	rb, err := reprintNoComments(bName, b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ra, rb), nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s OLD.go NEW.go\n", os.Args[0])
		os.Exit(2)
	}
	aName, bName := os.Args[1], os.Args[2]

	a, err := os.ReadFile(aName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	b, err := os.ReadFile(bName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	same, err := inert(aName, a, bName, b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if same {
		fmt.Printf("INERT: %s vs %s\n", aName, bName)
		os.Exit(0)
	}
	fmt.Printf("DIFFERS: %s vs %s\n", aName, bName)
	os.Exit(1)
}
