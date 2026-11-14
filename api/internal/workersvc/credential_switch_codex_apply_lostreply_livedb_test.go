package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

var errCodexApplyLostCommitReply = errors.New("test: committed apply reply lost")

type codexApplyLostReplyBeginner struct{ tx pgx.Tx }

func (b codexApplyLostReplyBeginner) Begin(context.Context) (pgx.Tx, error) {
	return codexApplyLostReplyTx{Tx: b.tx}, nil
}

type codexApplyLostReplyTx struct{ pgx.Tx }

func (tx codexApplyLostReplyTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return errCodexApplyLostCommitReply
}

// A real committed transaction followed by a lost reply cannot reconstruct the
// hash-only capability. Replay must expose no authority and must not mint again.
func TestCodexApplyCommittedLostReplyLiveDB(t *testing.T) {
	f, run, target := codexApplyFixture(t, false)
	op := f.opOf(t, run)
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	f.svc.SetTxBeginner(codexApplyLostReplyBeginner{tx: tx})
	result, err := f.svc.ApplyCredentialSwitch(f.env.ctx, f.worker(t), run, f.req(op))
	if !errors.Is(err, errCodexApplyLostCommitReply) || result.Codex != nil || result.Outcome == ApplyOutcomeApplied {
		t.Fatal("lost commit reply released authority", err)
	}
	committed := mustRun(t, f.env, run)
	if committed.CodexSecretID.Bytes != target || committed.CredentialSwitchRequestedAt.Valid || committed.CodexClaimEpoch != 42 {
		t.Fatal("test did not commit the switch before losing its reply")
	}
	f.svc.SetTxBeginner(f.env.pool)
	before := f.snap(t, run)
	replay := f.apply(t, run, f.req(op))
	if replay.Outcome != ApplyOutcomeRefusedCommitted || replay.Codex != nil || replay.Token != "" {
		t.Fatal("committed retry returned or reminted authority")
	}
	if f.snap(t, run) != before {
		t.Fatal("committed replay wrote state")
	}
}
