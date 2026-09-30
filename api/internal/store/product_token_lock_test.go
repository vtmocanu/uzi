package store

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The mint lock's class is a literal in queries/product_tokens.sql (sqlc cannot bind a
// Go constant into the statement). This pins the generated statement, which is what
// executes, to the named constant in migrate.go, and keeps the class distinct from
// EVERY other two-int lock class.
//
// The other classes are enumerated from source, not listed by hand: every top-level
// `const ...LockClass` in this package's non-test files is parsed and its value
// evaluated, so a class added later is compared without an edit here.
func TestProductTokenMintLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(ProductTokenMintLockClass), 10)
	if !strings.Contains(lockProductTokenMint, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockProductTokenMint does not lock class %s (ProductTokenMintLockClass):\n%s", want, lockProductTokenMint)
	}

	classes := lockClassesFromSource(t)
	// Non-vacuity: the parse found the named constant with its compiled value, plus the
	// classes that existed when this test was written.
	if got, ok := classes["ProductTokenMintLockClass"]; !ok || got != int64(ProductTokenMintLockClass) {
		t.Fatalf("source parse gave ProductTokenMintLockClass = %d (found %t), compiled value %d", got, ok, ProductTokenMintLockClass)
	}
	for name, v := range map[string]int32{
		"HostedProvisionLockClass":       HostedProvisionLockClass,
		"SecretMutationLockClass":        SecretMutationLockClass,
		"JudgeDispositionCoordLockClass": JudgeDispositionCoordLockClass,
		"RunBranchLockClass":             RunBranchLockClass,
		"CheckpointRetentionLockClass":   CheckpointRetentionLockClass,
		"StoredFilesLockClass":           StoredFilesLockClass,
	} {
		if got, ok := classes[name]; !ok || got != int64(v) {
			t.Errorf("source parse gave %s = %d (found %t), compiled value %d", name, got, ok, v)
		}
	}

	// Pairwise distinct: no two classes share a value.
	byValue := map[int64][]string{}
	for name, v := range classes {
		byValue[v] = append(byValue[v], name)
	}
	for v, names := range byValue {
		if len(names) > 1 {
			sort.Strings(names)
			t.Errorf("two-int lock classes collide on %d: %v", v, names)
		}
	}
	t.Logf("%d two-int lock classes: %v", len(classes), classes)
}

// lockClassesFromSource parses this package's non-test Go files and returns every
// top-level constant whose name ends in "LockClass", with its evaluated value.
func lockClassesFromSource(t *testing.T) map[string]int64 {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]int64{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasSuffix(name.Name, "LockClass") {
						continue
					}
					if i >= len(vs.Values) {
						t.Fatalf("%s: %s has no explicit value; this test evaluates literals only", f, name.Name)
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						t.Fatalf("%s: %s is not an integer literal; this test evaluates literals only", f, name.Name)
					}
					v, exact := constant.Int64Val(constant.MakeFromLiteral(lit.Value, lit.Kind, 0))
					if !exact {
						t.Fatalf("%s: %s = %s does not fit int64", f, name.Name, lit.Value)
					}
					if _, dup := out[name.Name]; dup {
						t.Fatalf("%s declared twice", name.Name)
					}
					out[name.Name] = v
				}
			}
		}
	}
	return out
}
