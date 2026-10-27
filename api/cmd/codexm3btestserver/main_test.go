package main

import (
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The live harness must exercise production's callback budgets. Client-level tests
// cannot catch an omitted option at this independent construction site.
func TestLiveSubscriptionUsesProductionRefreshBudgets(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "buildLiveSubscription" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			name, ok := assign.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "refreshClient" {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				t.Fatal("live refresh client must use codexauth.NewClient")
			}
			constructor, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				t.Fatal("live refresh client must use codexauth.NewClient")
			}
			pkg, ok := constructor.X.(*ast.Ident)
			if !ok || pkg.Name != "codexauth" || constructor.Sel.Name != "NewClient" {
				t.Fatal("live refresh client must use codexauth.NewClient")
			}
			want := map[string][2]string{
				"WithPerRequestTimeout": {"2500", "Millisecond"},
				"WithRefreshTimeout":    {"5", "Second"},
			}
			seen := make(map[string]bool)
			for _, arg := range call.Args {
				option, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				selector, ok := option.Fun.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				optionPkg, ok := selector.X.(*ast.Ident)
				if !ok || optionPkg.Name != "codexauth" {
					continue
				}
				expected, required := want[selector.Sel.Name]
				if !required {
					continue
				}
				if seen[selector.Sel.Name] || len(option.Args) != 1 {
					t.Fatalf("duplicate or malformed %s", selector.Sel.Name)
				}
				product, ok := option.Args[0].(*ast.BinaryExpr)
				if !ok || product.Op != token.MUL {
					t.Fatalf("%s must use %s*time.%s", selector.Sel.Name, expected[0], expected[1])
				}
				amount, amountOK := product.X.(*ast.BasicLit)
				unit, unitOK := product.Y.(*ast.SelectorExpr)
				if !amountOK || amount.Kind != token.INT || amount.Value != expected[0] || !unitOK {
					t.Fatalf("%s must use %s*time.%s", selector.Sel.Name, expected[0], expected[1])
				}
				timePkg, ok := unit.X.(*ast.Ident)
				if !ok || timePkg.Name != "time" || unit.Sel.Name != expected[1] {
					t.Fatalf("%s must use %s*time.%s", selector.Sel.Name, expected[0], expected[1])
				}
				seen[selector.Sel.Name] = true
			}
			if !seen["WithPerRequestTimeout"] || !seen["WithRefreshTimeout"] {
				t.Fatal("live refresh client must set both production timeout options")
			}
			found = true
			return false
		})
	}
	if !found {
		t.Fatal("live subscription refresh client construction not found")
	}
}

func TestFakeChatGPTAccessTokenCarriesPinnedExternalAuthClaims(t *testing.T) {
	const accountID = "canary-account-test"
	token, err := fakeChatGPTAccessToken(accountID, "token-a")
	if err != nil {
		t.Fatalf("fakeChatGPTAccessToken: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("token has %d non-empty JWT segments, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		TokenID string `json:"jti"`
		Auth    struct {
			PlanType string `json:"chatgpt_plan_type"`
			Account  string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.TokenID != "canary-token-a" || claims.Auth.PlanType != "pro" || claims.Auth.Account != accountID {
		t.Fatalf("unexpected external-auth claims: %+v", claims)
	}

	rotated, err := fakeChatGPTAccessToken(accountID, "token-b")
	if err != nil {
		t.Fatalf("rotated fakeChatGPTAccessToken: %v", err)
	}
	if rotated == token {
		t.Fatal("a rotated fake access token must differ from the initial token")
	}
}
