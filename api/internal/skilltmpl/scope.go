package skilltmpl

// The skill scope vocabulary: the four values of the skills.scope column. It mirrors the
// skills_scope_check CHECK (migration 00278_product_skills.sql; 00040_skills.sql created it with
// the first three) and is pinned to it by TestSkillsScopeConstraintNameLiveDB, and to the delivery
// precedence table workersvc.scopeRank by TestScopeRankCoversEveryScope, so a fifth scope cannot
// be added to one without the others going red.
//
// PRODUCT skills (PRD #1909 D9) are the odd one out: they are not a shared or per-user playbook
// but one product's approved skill set, delivered only to jobs started through that product. They
// take no part in precedence or allocation, and no listing a user can call returns them. Every
// SQL predicate and Go branch that decides something by scope therefore names its scopes
// explicitly (an IN list, a switch with no default that admits), never a negation such as
// `scope <> 'user'`, which would silently admit 'product'.
const (
	ScopeBuiltin = "builtin"
	ScopeGlobal  = "global"
	ScopeUser    = "user"
	ScopeProduct = "product"
)

// Scopes returns every skill scope, in a fixed order. The slice is a copy.
func Scopes() []string {
	return []string{ScopeBuiltin, ScopeGlobal, ScopeUser, ScopeProduct}
}

// IsShared reports whether scope is one of the two admin-managed, every-user scopes
// (builtin, global): the only scopes a shared allocation may reference.
func IsShared(scope string) bool {
	return scope == ScopeBuiltin || scope == ScopeGlobal
}
