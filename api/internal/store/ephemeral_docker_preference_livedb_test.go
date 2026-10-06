package store_test

import (
	"testing"

	"github.com/google/uuid"
)

func TestEphemeralDockerPreferenceAppliesLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	repo := fx.repoID.String()
	list := "{" + repo + "}"
	type input struct{ preference, tier, repo, kind, egress, list any }
	base := input{true, true, fx.repoID, "issue", nil, list}
	for _, tc := range []struct {
		name  string
		field string
		value any
		want  bool
	}{
		{"null egress", "egress", nil, true},
		{"preference off", "preference", false, false}, {"null preference", "preference", nil, false},
		{"tier off", "tier", false, false}, {"null tier", "tier", nil, false},
		{"null kind", "kind", nil, false}, {"job", "kind", "job", false},
		{"lane", "egress", uuid.New(), false},
		{"null repo", "repo", nil, false}, {"unallowlisted", "repo", uuid.New(), false},
		{"null allowlist", "list", nil, false}, {"empty allowlist", "list", "{}", false},
		{"null element without match", "list", "{NULL}", false},
		{"match plus null", "list", "{" + repo + ",NULL}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := base
			switch tc.field {
			case "preference":
				i.preference = tc.value
			case "tier":
				i.tier = tc.value
			case "repo":
				i.repo = tc.value
			case "kind":
				i.kind = tc.value
			case "egress":
				i.egress = tc.value
			case "list":
				i.list = tc.value
			}
			var got bool
			err := fx.pool.QueryRow(fx.ctx, `SELECT fn_ephemeral_docker_preference_applies($1::boolean,$2::boolean,$3::uuid,$4::text,$5::uuid,$6::uuid[])`, i.preference, i.tier, i.repo, i.kind, i.egress, i.list).Scan(&got)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
	var got bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_ephemeral_docker_preference_applies(true,true,NULL,'judge',NULL,$1::uuid[])`, list).Scan(&got); err != nil || got {
		t.Fatalf("repo-less judge=%v err=%v", got, err)
	}
	var strict bool
	var volatility string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT proisstrict,provolatile::text FROM pg_proc WHERE oid='fn_ephemeral_docker_preference_applies(boolean,boolean,uuid,text,uuid,uuid[])'::regprocedure`).Scan(&strict, &volatility); err != nil {
		t.Fatal(err)
	}
	if strict || volatility != "i" {
		t.Fatalf("strict=%v volatility=%s", strict, volatility)
	}
	// The previous lease signature still admits the same ordinary follow-up.
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_ephemeral_lease_admits(now(),$1::uuid,'agent/issue-1',false,interval '1 hour',now(),$1::uuid,'issue','agent/issue-1',NULL,1,NULL,NULL)`, fx.repoID).Scan(&got); err != nil || !got {
		t.Fatalf("old lease=%v err=%v", got, err)
	}
	for _, tc := range []struct {
		name, kind string
		repo       any
		want       bool
	}{
		{"allowlisted", "issue", fx.repoID, true},
		{"unallowlisted", "issue", uuid.New(), false},
		{"repo-less judge exception", "judge", nil, true},
	} {
		t.Run("claim fence "+tc.name, func(t *testing.T) {
			if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_worker_can_claim(true,$1::uuid[],$2::uuid,$3::text,'{}'::text[],'{}'::text[],true)`, list, tc.repo, tc.kind).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("claim fence=%v want=%v", got, tc.want)
			}
		})
	}

}
