package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestIntakePauseClaimGuard fails when a statement that claims a run (advances
// claim_generation or SETs status = 'claimed') lacks the PRD #3454 intake-pause conjunct.
// The matched set must be exactly ClaimRun and ClaimChatRun: a new claiming query must
// either gain the conjunct (and be added here) or justify why it cannot start new roots.
func TestIntakePauseClaimGuard(t *testing.T) {
	genRe := regexp.MustCompile(`claim_generation\s*=\s*(runs\.)?claim_generation\s*\+\s*1`)
	// The SET-list form only: WHERE guards also mention 'claimed' and must not match.
	setRe := regexp.MustCompile(`(?m)^\s*status\s*=\s*'claimed'\s*,`)
	nameRe := regexp.MustCompile(`-- name: (\w+)`)

	files, err := filepath.Glob("*.sql.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated query files: %v", err)
	}
	matched := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, v := range vs.Values {
				body, ok := constString(v)
				if !ok {
					continue
				}
				m := nameRe.FindStringSubmatch(body)
				if m == nil {
					continue
				}
				if genRe.MatchString(body) || setRe.MatchString(body) {
					matched[m[1]] = body
				}
			}
			return true
		})
	}
	var names []string
	for n := range matched {
		names = append(names, n)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "ClaimChatRun,ClaimRun" {
		t.Fatalf("claiming queries = [%s], want exactly [ClaimChatRun,ClaimRun]; a new claim path needs the intake-pause conjunct (PRD #3454)", got)
	}
	for _, n := range names {
		for _, want := range []string{"fn_run_admitted_during_intake_pause", "fn_instance_intake_paused"} {
			if !strings.Contains(matched[n], want) {
				t.Errorf("%s lacks %s", n, want)
			}
		}
	}
}

// constString folds a string literal or a "+" concatenation of literals; sqlc emits the
// latter for a query body that itself contains a backtick.
func constString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok1 := constString(x.X)
		r, ok2 := constString(x.Y)
		return l + r, ok1 && ok2
	case *ast.ParenExpr:
		return constString(x.X)
	}
	return "", false
}
