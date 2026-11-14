package workersvc

import (
	"github.com/google/uuid"
	"testing"
)

func TestCodexSwitchSpendJoinsAuthorityEpochLiveDB(t *testing.T) {
	f, run, target := codexApplyFixture(t, false)
	generation := f.gen
	appendFrames := func(messages ...IncomingMessage) {
		t.Helper()
		if err := f.svc.AppendMessagesForClaim(f.env.ctx, f.worker(t), run, messages, &generation); err != nil {
			t.Fatal(err)
		}
	}
	appendFrames(epochInit(6), epochResult(7, i32(0), "gpt-5.4", 11, 2, 0.01))
	applied := f.apply(t, run, f.req(f.opOf(t, run)))
	if applied.Outcome != ApplyOutcomeApplied || applied.Codex == nil || applied.Ordinal != 1 || applied.CapabilityEpoch != 42 {
		t.Fatal("switch did not produce independent authority/attribution epochs")
	}
	appendFrames(epochInit(applied.MessageSeq+1), epochResult(applied.MessageSeq+2, &applied.Ordinal, "gpt-5.4", 70, 7, 0.1))
	assertJoins := func() {
		t.Helper()
		rows, err := f.env.pool.Query(f.env.ctx, `WITH epochs AS (
   SELECT run_id,claim_generation,0::integer AS ordinal,secret_id,codex_claim_epoch FROM run_credential_epochs
   UNION ALL SELECT run_id,claim_generation,ordinal,secret_id,codex_claim_epoch FROM run_credential_switch_epochs)
   SELECT u.credential_epoch_ordinal,u.input_tokens,u.output_tokens,e.secret_id,e.codex_claim_epoch
   FROM run_usage u JOIN epochs e ON e.run_id=u.run_id AND e.claim_generation=u.claim_generation
     AND e.ordinal=u.credential_epoch_ordinal
   WHERE u.run_id=$1 ORDER BY e.ordinal`, run)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var ordinal int32
			var input, output, epoch int64
			var secret uuid.UUID
			if err := rows.Scan(&ordinal, &input, &output, &secret, &epoch); err != nil {
				t.Fatal(err)
			}
			switch ordinal {
			case 0:
				if input != 11 || output != 2 || epoch != 41 || secret == target {
					t.Fatal("A spend lost baseline attribution")
				}
			case 1:
				if input != 70 || output != 7 || epoch != 42 || secret != target {
					t.Fatal("B spend did not join the exact minted B epoch")
				}
			default:
				t.Fatal("unexpected attribution ordinal")
			}
			count++
		}
		if rows.Err() != nil || count != 2 {
			t.Fatal("spend rows did not join both authority snapshots", rows.Err())
		}
	}
	assertJoins()
	if err := RefoldRunUsage(f.env.ctx, f.env.pool, f.env.q, mustRun(t, f.env, run)); err != nil {
		t.Fatal(err)
	}
	assertJoins()
	var ordinal int32
	var epoch int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT m.credential_epoch_ordinal,e.codex_claim_epoch
  FROM run_messages m JOIN run_credential_switch_epochs e ON e.run_id=m.run_id
   AND e.claim_generation=m.claim_generation AND e.ordinal=m.credential_epoch_ordinal
  WHERE m.run_id=$1 AND m.seq=$2`, run, applied.MessageSeq+2).Scan(&ordinal, &epoch); err != nil {
		t.Fatal(err)
	}
	if ordinal != 1 || epoch != 42 {
		t.Fatal("B message attribution confused ordinal and capability epoch")
	}
}
