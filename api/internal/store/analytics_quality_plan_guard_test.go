package store_test

import (
	"reflect"
	"testing"
)

// cleanQualityPlan is a hand-built plan satisfying every qualityShapeProblems condition:
// rw_witness read by a plain CTE Scan outside any SubPlan, and rw_sel read inside a SubPlan that
// an expression references as hashed.
func cleanQualityPlan() planNode {
	return planNode{
		NodeType: "Hash Join",
		Plans: []planNode{
			{NodeType: "CTE Scan", CTEName: "rw_witness", ParentRelationship: "Outer"},
			{
				NodeType: "CTE Scan", CTEName: "rw_seed", ParentRelationship: "Inner",
				Filter: "(hashed SubPlan 1)",
				Plans: []planNode{
					{NodeType: "CTE Scan", CTEName: "rw_sel", ParentRelationship: "SubPlan", SubplanName: "SubPlan 1"},
				},
			},
		},
	}
}

// TestQualityExecutedProblemsPlanGuard feeds qualityExecutedProblems hand-built ANALYZE plans: the
// membership must have returned rows under the hashed SubPlan.
func TestQualityExecutedProblemsPlanGuard(t *testing.T) {
	withRows := func(rows float64, outsideRows float64) planNode {
		root := cleanQualityPlan()
		root.Plans[1].Plans[0].ActualRows, root.Plans[1].Plans[0].ActualLoops = rows, 1
		root.Plans = append(root.Plans, planNode{
			NodeType: "CTE Scan", CTEName: "rw_sel", ParentRelationship: "Outer", ActualRows: outsideRows, ActualLoops: 1,
		})
		return root
	}
	t.Run("rows under the hashed SubPlan", func(t *testing.T) {
		root := withRows(3, 0)
		if got := qualityExecutedProblems(&root); len(got) != 0 {
			t.Fatalf("problems = %v, want none", got)
		}
	})
	t.Run("zero rows under the hashed SubPlan", func(t *testing.T) {
		root := withRows(0, 5)
		if got := qualityExecutedProblems(&root); !reflect.DeepEqual(got, []string{qualityNoExecutedRwSel}) {
			t.Fatalf("problems = %v, want [%s]", got, qualityNoExecutedRwSel)
		}
	})
	t.Run("rows under an un-hashed SubPlan", func(t *testing.T) {
		root := withRows(3, 0)
		root.Plans[1].Filter = "(SubPlan 1)"
		if got := qualityExecutedProblems(&root); !reflect.DeepEqual(got, []string{qualityNoExecutedRwSel}) {
			t.Fatalf("problems = %v, want [%s]", got, qualityNoExecutedRwSel)
		}
	})
	t.Run("rows only outside the SubPlan", func(t *testing.T) {
		root := cleanQualityPlan()
		root.Plans = append(root.Plans, planNode{
			NodeType: "CTE Scan", CTEName: "rw_sel", ParentRelationship: "Outer", ActualRows: 4, ActualLoops: 1,
		})
		if got := qualityExecutedProblems(&root); !reflect.DeepEqual(got, []string{qualityNoExecutedRwSel}) {
			t.Fatalf("problems = %v, want [%s]", got, qualityNoExecutedRwSel)
		}
	})
}

// TestQualityShapeProblemsPlanGuard feeds qualityShapeProblems hand-built plans, so the two
// branches no real plan trips by default (the pair-keyed nested loop and a hashed rw_witness
// SubPlan) each have a committed control that fails when the branch is removed.
func TestQualityShapeProblemsPlanGuard(t *testing.T) {
	t.Run("clean plan has no problems", func(t *testing.T) {
		root := cleanQualityPlan()
		if got := qualityShapeProblems(&root); len(got) != 0 {
			t.Fatalf("clean plan reported problems: %v", got)
		}
	})

	t.Run("nested loop joined on the repo_id and mr_iid pair", func(t *testing.T) {
		root := cleanQualityPlan()
		const jf = "((a.repo_id = b.repo_id) AND (a.mr_iid = b.mr_iid))"
		root.Plans = append(root.Plans, planNode{NodeType: "Nested Loop", JoinFilter: jf})
		want := []string{"Nested Loop with Join Filter on the (repo_id, mr_iid) pair: " + jf}
		if got := qualityShapeProblems(&root); !reflect.DeepEqual(got, want) {
			t.Fatalf("problems = %v, want %v", got, want)
		}
	})

	t.Run("rw_witness read under a hashed SubPlan", func(t *testing.T) {
		root := cleanQualityPlan()
		root.Plans = append(root.Plans, planNode{
			NodeType: "Seq Scan", Filter: "(hashed SubPlan 2)",
			Plans: []planNode{
				{NodeType: "CTE Scan", CTEName: "rw_witness", ParentRelationship: "SubPlan", SubplanName: "SubPlan 2"},
			},
		})
		want := []string{"rw_witness is read under a hashed SubPlan: the witness lookup is a membership SubPlan again"}
		if got := qualityShapeProblems(&root); !reflect.DeepEqual(got, want) {
			t.Fatalf("problems = %v, want %v", got, want)
		}
	})
}
