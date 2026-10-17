package workersvc

import (
	"testing"
)

func TestCompletionScopeViewClaimParity(t *testing.T) {
	const criteria = `[{"id":"m1.c1","milestone_id":"m1","text":"One"},{"id":"m2.c1","milestone_id":"m2","text":"Two"}]`
	const first = `{"milestone_id":"m2","reason":"first","revision":2}`
	const second = `{"milestone_id":"m1","reason":"later","revision":3}`
	const accepted = `[{"id":"m1.c1","milestone_id":"m1","text":"One","reason":"accepted","revision":2}]`
	for _, tc := range []struct {
		name, contract                    string
		ids, titles, reasons, acceptedIDs []string
	}{
		{"no scope", `{"criteria":` + criteria + `}`, nil, nil, nil, nil},
		{"partial", `{"criteria":` + criteria + `,"scope":{"out":[` + first + `]}}`, []string{"m2"}, []string{"Two"}, []string{"first"}, nil},
		{"accumulating partial", `{"criteria":` + criteria + `,"scope":{"out":[` + first + "," + second + `]}}`, []string{"m2", "m1"}, []string{"Two", "One"}, []string{"first", "later"}, nil},
		{"accept only", `{"criteria":` + criteria + `,"accepted":` + accepted + `}`, nil, nil, nil, []string{"m1.c1"}},
		{"null criteria", `{"criteria":null,"scope":{"out":[` + first + `]}}`, []string{"m2"}, []string{""}, []string{"first"}, nil},
		{"missing criterion", `{"criteria":[],"scope":{"out":[` + first + `]}}`, []string{"m2"}, []string{""}, []string{"first"}, nil},
		{"undecodable", "{", nil, nil, nil, nil},
		{"JSON null", "null", nil, nil, nil, nil},
		{"SQL NULL", "", nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deferred, accepted := CompletionScopeView([]byte(tc.contract))
			claim := CompletionScopeClaim([]byte(tc.contract))
			if deferred == nil || accepted == nil {
				t.Fatal("view must carry arrays")
			}
			if len(deferred) != len(tc.ids) || len(accepted) != len(tc.acceptedIDs) {
				t.Fatalf("view deferred=%+v accepted=%+v", deferred, accepted)
			}
			if len(tc.ids)+len(tc.acceptedIDs) == 0 {
				if claim != nil {
					t.Fatalf("claim=%+v", claim)
				}
				return
			}
			if claim == nil || len(claim.Deferred) != len(deferred) || len(claim.Accepted) != len(accepted) {
				t.Fatalf("claim=%+v", claim)
			}
			for i, d := range deferred {
				c := claim.Deferred[i]
				if d.MilestoneID != tc.ids[i] || d.Reason != tc.reasons[i] || c.MilestoneID != d.MilestoneID || c.Reason != d.Reason || c.Title != tc.titles[i] {
					t.Fatalf("view=%+v claim=%+v", d, c)
				}
			}
			for i, a := range accepted {
				c := claim.Accepted[i]
				if a.ID != tc.acceptedIDs[i] || c.ID != a.ID || c.MilestoneID != a.MilestoneID || c.Text != a.Text || c.Reason != a.Reason {
					t.Fatalf("view=%+v claim=%+v", a, c)
				}
			}

		})
	}
}
