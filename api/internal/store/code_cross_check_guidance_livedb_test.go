package store_test

import (
	"context"
	"strings"
	"testing"
)

func TestCodeCrossCheckGuidanceBoundsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		guidance, instructions string
		valid                  bool
	}{
		{"exact multibyte guidance", strings.Repeat("é", 4096), "", true},
		{"over guidance", strings.Repeat("é", 4096) + "a", "", false},
		{"exact multibyte instructions", "", strings.Repeat("é", 32768), true},
		{"over instructions", "", strings.Repeat("é", 32768) + "a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := codeCrossCheckFixture(t)
			ctx := context.Background()
			_, err := fx.f.pool.Exec(ctx, `INSERT INTO cross_checks
 (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
 head_commit,base_commit,candidate_digest,outcome,code_context,guidance_snapshot,
 guidance_text,guidance_digest,repo_instructions_enabled,repo_instructions_text,
 repo_instructions_digest,checker_harness,deadline_at)
 VALUES($1,$2,$3,'code',1,1,'failed',$4,$5,$6,'pending','{}',$7,$7,
 sha256(convert_to($7,'UTF8')),true,$8,sha256(convert_to($8,'UTF8')),'codex',now()+interval '5 minutes')`,
				fx.crossCheckID, fx.f.runID, fx.checkerID, codeFixtureHead, codeFixtureBase, make([]byte, 32), tc.guidance, tc.instructions)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireCodeCheckViolation(t, err)
			}
		})
	}
}

func TestRepoCodeCrossCheckGuidanceBoundsLiveDB(t *testing.T) {
	fx := codeCrossCheckFixture(t)
	ctx := context.Background()
	guidance := strings.Repeat("é", 4096)
	_, err := fx.f.pool.Exec(ctx, "UPDATE repos SET code_cross_check_guidance=$2 WHERE id=(SELECT repo_id FROM runs WHERE id=$1)", fx.f.runID, guidance)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.f.pool.Exec(ctx, "UPDATE repos SET code_cross_check_guidance=$2 WHERE id=(SELECT repo_id FROM runs WHERE id=$1)", fx.f.runID, guidance+"a")
	requireCodeCheckViolation(t, err)
}
