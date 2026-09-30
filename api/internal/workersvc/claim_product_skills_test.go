package workersvc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/skilltmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestScopeRankCoversEveryScope pins scopeRank to the scope vocabulary (and through it to the
// skills_scope_check CHECK, see TestSkillsScopeConstraintNameLiveDB in the store package): the
// three delivery scopes rank user > global > builtin, and 'product' (which never enters the
// precedence union) and anything unknown rank 0. A fifth scope added to skilltmpl.Scopes without a
// decision here fails this test.
func TestScopeRankCoversEveryScope(t *testing.T) {
	want := map[string]int{
		skilltmpl.ScopeUser:    3,
		skilltmpl.ScopeGlobal:  2,
		skilltmpl.ScopeBuiltin: 1,
		skilltmpl.ScopeProduct: 0,
	}
	for _, scope := range skilltmpl.Scopes() {
		rank, decided := want[scope]
		if !decided {
			t.Errorf("scope %q is in skilltmpl.Scopes but scopeRank has no decision for it: add it to this table and to scopeRank", scope)
			continue
		}
		if got := scopeRank(scope); got != rank {
			t.Errorf("scopeRank(%q) = %d, want %d", scope, got, rank)
		}
	}
	if len(want) != len(skilltmpl.Scopes()) {
		t.Errorf("the rank table has %d scopes, skilltmpl.Scopes has %d", len(want), len(skilltmpl.Scopes()))
	}
	if scopeRank("") != 0 || scopeRank("nope") != 0 {
		t.Error("an unknown scope must rank 0")
	}
}

func productRow(name, body string) store.ListProductSkillsForRunRow {
	return store.ListProductSkillsForRunRow{ID: uuid.New(), Name: name, Description: name + " description.", Body: body}
}

func TestAssembleProductSkills(t *testing.T) {
	t.Run("empty input yields non-nil empty results", func(t *testing.T) {
		skills, dropped := assembleProductSkills(nil, 100, 10)
		if skills == nil || dropped == nil || len(skills) != 0 || len(dropped) != 0 {
			t.Fatalf("got %#v, %#v", skills, dropped)
		}
	})
	t.Run("sorted by name, bodies and descriptions carried", func(t *testing.T) {
		skills, dropped := assembleProductSkills([]store.ListProductSkillsForRunRow{productRow("zeta", "z"), productRow("alpha", "a")}, 0, 0)
		if len(dropped) != 0 || len(skills) != 2 || skills[0].Name != "alpha" || skills[1].Name != "zeta" ||
			skills[0].Body != "a" || skills[0].Description != "alpha description." {
			t.Fatalf("got %+v / %+v", skills, dropped)
		}
	})
	t.Run("an oversized body is dropped too_large and does not count against the per-run cap", func(t *testing.T) {
		rows := []store.ListProductSkillsForRunRow{productRow("a", "ok"), productRow("b", strings.Repeat("x", 11)), productRow("c", "ok"), productRow("d", "ok")}
		skills, dropped := assembleProductSkills(rows, 10, 2)
		if got := unionNames(skills); !reflect.DeepEqual(got, []string{"a", "c"}) {
			t.Fatalf("kept %v, want [a c]", got)
		}
		want := []ClaimSkillDrop{{Name: "b", Reason: DropTooLarge}, {Name: "d", Reason: DropOverLimit}}
		if !reflect.DeepEqual(dropped, want) {
			t.Fatalf("dropped %+v, want %+v", dropped, want)
		}
	})
	t.Run("a cap of zero or less means no cap", func(t *testing.T) {
		skills, dropped := assembleProductSkills([]store.ListProductSkillsForRunRow{productRow("a", strings.Repeat("x", 500)), productRow("b", "b")}, 0, -1)
		if len(skills) != 2 || len(dropped) != 0 {
			t.Fatalf("got %+v / %+v", skills, dropped)
		}
	})
	t.Run("the input slice is not reordered", func(t *testing.T) {
		rows := []store.ListProductSkillsForRunRow{productRow("b", "b"), productRow("a", "a")}
		assembleProductSkills(rows, 0, 0)
		if rows[0].Name != "b" {
			t.Fatal("assembleProductSkills sorted its input in place")
		}
	})
}
