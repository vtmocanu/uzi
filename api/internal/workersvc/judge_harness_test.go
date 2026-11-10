package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestJudgeHarnessFor(t *testing.T) {
	unset := pgtype.Text{}
	claude := pgtype.Text{String: "claude", Valid: true}
	codex := pgtype.Text{String: "codex", Valid: true}
	cases := []struct {
		name   string
		runsOn pgtype.Text
		target Harness
		want   Harness
	}{
		{"unset on a claude target follows the run", unset, HarnessClaude, HarnessClaude},
		{"unset on a codex target follows the run", unset, HarnessCodex, HarnessCodex},
		{"claude on a claude target", claude, HarnessClaude, HarnessClaude},
		{"claude on a codex target", claude, HarnessCodex, HarnessClaude},
		{"codex on a claude target", codex, HarnessClaude, HarnessCodex},
		{"codex on a codex target", codex, HarnessCodex, HarnessCodex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeHarnessFor(tc.runsOn, tc.target); got != tc.want {
				t.Fatalf("judgeHarnessFor(%+v, %q) = %q, want %q", tc.runsOn, tc.target, got, tc.want)
			}
		})
	}
}
