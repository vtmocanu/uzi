package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Issue #4700 enforcement: run_usage fold freshness (migration 00543) holds only while EVERY
// run_usage writer first calls fn_run_usage_fold_lock for the run it writes. There are no
// triggers on run_usage, so this test is the guard. It checks, without a database:
//
//	(i)   every SQL statement in queries/*.sql, and in migrations numbered above 00543, that
//	      writes run_usage carries an fn_run_usage_fold_lock CTE the statement reads from;
//	(ii)  no non-test Go file outside the sqlc output embeds raw SQL writing run_usage;
//	(iii) the generated UpsertRunUsage / DeleteRunUsage methods are called only from the
//	      three vetted functions, never taken as method values or named by string;
//	(iv)  RefoldRunUsage takes the run lock (LockRunForUsageRefold) before DeleteRunUsage.

var (
	// run_usage itself, never run_usage_legs / _messages / _tail_state / _model_totals: each
	// pattern requires whitespace or "(" right after the table name.
	runUsageWriteRE = regexp.MustCompile(`(?is)\b(?:INSERT\s+INTO|MERGE\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(?:\s+TABLE)?)\s+(?:ONLY\s+)?run_usage[\s(;]`)
	runUsageLockRE  = regexp.MustCompile(`(?is)fn_run_usage_fold_lock\s*\(`)
	runUsageFromLk  = regexp.MustCompile(`(?is)\bFROM\s+lk\b`)
	usageSQLComment = regexp.MustCompile(`(?m)--[^\n]*`)
)

// unguardedRunUsageWrite reports whether stmt writes run_usage without the lock CTE.
func unguardedRunUsageWrite(stmt string) bool {
	stmt = usageSQLComment.ReplaceAllString(stmt, "") + " "
	if !runUsageWriteRE.MatchString(stmt) {
		return false
	}
	return !runUsageLockRE.MatchString(stmt) || !runUsageFromLk.MatchString(stmt)
}

func writesRunUsage(s string) bool {
	return runUsageWriteRE.MatchString(usageSQLComment.ReplaceAllString(s, "") + " ")
}

func TestRunUsageWriterClassifier(t *testing.T) {
	good := []string{
		"WITH lk AS (SELECT fn_run_usage_fold_lock(@run_id) AS ok) DELETE FROM run_usage WHERE run_id = @run_id AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok)\nINSERT INTO run_usage (a)\nSELECT 1 FROM lk WHERE lk.ok",
		"INSERT INTO run_usage_legs (a) VALUES (1)",
		"DELETE FROM run_usage_messages WHERE x",
		"UPDATE run_usage_tail_state SET a = 1",
		"SELECT * FROM run_usage",
		"-- DELETE FROM run_usage WHERE x\nSELECT 1",
	}
	for _, s := range good {
		if unguardedRunUsageWrite(s) {
			t.Errorf("classified as unguarded writer, want clean: %q", s)
		}
	}
	bad := []string{
		"INSERT INTO run_usage (a) VALUES (1)",
		"INSERT INTO run_usage\n(a) VALUES (1)",
		"UPDATE run_usage SET a = 1",
		"DELETE FROM run_usage WHERE run_id = $1",
		"DELETE FROM run_usage",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE run_id = $1", // CTE never read
		"DELETE FROM run_usage WHERE run_id = $1 -- fn_run_usage_fold_lock( FROM lk",                   // lock only in a comment
		"TRUNCATE run_usage;",
	}
	for _, s := range bad {
		if !unguardedRunUsageWrite(s) {
			t.Errorf("classified as clean, want unguarded writer: %q", s)
		}
	}
}

// sqlStatements splits SQL text into statements: sqlc query files at each "-- name:" marker,
// anything else at semicolons.
func sqlStatements(text string, perName bool) []string {
	if perName {
		parts := regexp.MustCompile(`(?m)^-- name:`).Split(text, -1)
		return parts
	}
	return strings.Split(usageSQLComment.ReplaceAllString(text, ""), ";")
}

func TestRunUsageWritersTakeFoldLock_SQL(t *testing.T) {
	var scanned, writers int
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries glob: %v (%d files)", err, len(files))
	}
	guardedQueries := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries/migrations dirs, not from input.
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range sqlStatements(string(b), true) {
			scanned++
			if writesRunUsage(st) {
				writers++
				name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(st), "\n", 2)[0])
				if unguardedRunUsageWrite(st) {
					t.Errorf("%s: statement %q writes run_usage without an fn_run_usage_fold_lock CTE it reads (FROM lk)", f, name)
				} else {
					guardedQueries[strings.Fields(name)[0]] = true
				}
			}
		}
	}
	for _, want := range []string{"UpsertRunUsage", "DeleteRunUsage"} {
		if !guardedQueries[want] {
			t.Errorf("non-vacuity: guarded writer %s not found in queries/*.sql (found %v)", want, guardedQueries)
		}
	}
	if scanned < 100 || writers == 0 {
		t.Errorf("non-vacuity: scanned %d statements, %d run_usage writers", scanned, writers)
	}

	migs, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(migs) == 0 {
		t.Fatalf("migrations glob: %v", err)
	}
	var later int
	for _, f := range migs {
		base := filepath.Base(f)
		n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s has no numeric prefix", base)
		}
		if n <= 543 {
			continue
		}
		later++
		b, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries/migrations dirs, not from input.
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range sqlStatements(string(b), false) {
			if unguardedRunUsageWrite(st) {
				t.Errorf("%s: statement writes run_usage without calling fn_run_usage_fold_lock (see the manual DML rule in migration 00543): %q", base, strings.TrimSpace(st))
			}
		}
	}
	t.Logf("%d query statements (%d run_usage writers), %d migrations above 00543", scanned, writers, later)
}

// apiGoFiles lists the non-test Go files under api/ outside the sqlc output.
func apiGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.HasSuffix(p, ".sql.go") && filepath.Base(filepath.Dir(p)) == "store" {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 200 {
		t.Fatalf("non-vacuity: walked only %d Go files", len(out))
	}
	sort.Strings(out)
	return out
}

func TestRunUsageWritersTakeFoldLock_Go(t *testing.T) {
	allowed := map[string]bool{"foldUsageFrames": true, "foldPrefixedModelUsage": true, "RefoldRunUsage": true}
	guarded := map[string]bool{"UpsertRunUsage": true, "DeleteRunUsage": true}
	files := apiGoFiles(t)
	fset := token.NewFileSet()
	seenCalls := map[string]int{}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		// (ii) raw SQL in string literals, and (iii) the guarded names as strings.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if writesRunUsage(s) {
				t.Errorf("%s: raw SQL writing run_usage in a Go string; route it through the guarded sqlc queries", fset.Position(lit.Pos()))
			}
			if guarded[s] {
				t.Errorf("%s: string literal %q (reflection or by-name dispatch bypasses the call-site check)", fset.Position(lit.Pos()), s)
			}
			return true
		})
		// (iii) call sites.
		for _, decl := range file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			name := ""
			if isFn {
				name = fn.Name.Name
			}
			callFuns := map[ast.Node]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					callFuns[x.Fun] = true
				case *ast.SelectorExpr:
					if !guarded[x.Sel.Name] {
						return true
					}
					pos := fset.Position(x.Pos())
					if !callFuns[x] {
						t.Errorf("%s: .%s used as a value, not called directly", pos, x.Sel.Name)
						return true
					}
					if !allowed[name] {
						t.Errorf("%s: .%s called from %q; only foldUsageFrames, foldPrefixedModelUsage and RefoldRunUsage may write run_usage", pos, x.Sel.Name, name)
					}
					seenCalls[name+"."+x.Sel.Name]++
				}
				return true
			})
		}
	}
	for _, want := range []string{"foldUsageFrames.UpsertRunUsage", "foldPrefixedModelUsage.UpsertRunUsage", "RefoldRunUsage.DeleteRunUsage"} {
		if seenCalls[want] == 0 {
			t.Errorf("non-vacuity: call site %s not found (found %v)", want, seenCalls)
		}
	}
}

func TestRefoldRunUsageLocksRunBeforeDelete(t *testing.T) {
	const path = "../workersvc/usage_refold.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lockPos, delPos token.Pos
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RefoldRunUsage" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "LockRunForUsageRefold":
				if lockPos == token.NoPos {
					lockPos = sel.Pos()
				}
			case "DeleteRunUsage":
				if delPos == token.NoPos {
					delPos = sel.Pos()
				}
			}
			return true
		})
	}
	if lockPos == token.NoPos || delPos == token.NoPos {
		t.Fatalf("RefoldRunUsage must call LockRunForUsageRefold (found %v) and DeleteRunUsage (found %v)", lockPos != token.NoPos, delPos != token.NoPos)
	}
	if lockPos > delPos {
		t.Errorf("RefoldRunUsage calls DeleteRunUsage at %s before LockRunForUsageRefold at %s", fset.Position(delPos), fset.Position(lockPos))
	}
}
