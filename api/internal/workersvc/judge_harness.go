package workersvc

import "github.com/jackc/pgx/v5/pgtype"

// judgeHarnessFor resolves the harness a judge run is created on (PRD #3257): the owner's
// explicit "Judge runs on" choice (users.judge_runs_on) when set, else the harness of the
// run being judged. It is pure; both maybeEnqueueJudge and RerunJudge call it, so the
// automatic and the manual judge cannot disagree. The result is passed to createRunResolved
// as an explicit selection, so an explicit choice with no usable credential is refused
// (ErrNoCredentialForHarness) rather than moved to the other harness. The column is held to
// claude|codex by users_judge_runs_on_check, so a Valid value is a Harness as is.
func judgeHarnessFor(judgeRunsOn pgtype.Text, target Harness) Harness {
	if judgeRunsOn.Valid {
		return Harness(judgeRunsOn.String)
	}
	return target
}
