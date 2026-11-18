package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The first stop and its replay preserve the run's session, canonical gate
// (runs.gate_* and its run_gate_presentations row) and open question
// (runs.open_question_id and its question message). The fixture populates each
// so that loss or rewriting of any of them changes the compared snapshot.
func TestWorkerCodexStoppedStatePreservationRouteLiveDB(t *testing.T) {
	e, w := setupStoppedGuardsRoute(t)
	withdrawStoppedRoute(t, e, w)
	presentation := uuid.New()
	stoppedRouteExec(t, e, `UPDATE runs SET gate_revision=2, gate_presentation_id=$2,
 gate_presented_payload='{"plan_md":"fixture"}'::jsonb, gate_payload_digest='\x01',
 open_question_id='stopped-question' WHERE id=$1`, e.run, presentation)
	stoppedRouteExec(t, e, `INSERT INTO run_gate_presentations(run_id,presentation_id,revision) VALUES($1,$2,2)`, e.run, presentation)
	stoppedRouteExec(t, e, `INSERT INTO run_messages(run_id,seq,kind,payload,claim_generation)
 VALUES($1,(SELECT last_seq+1 FROM runs WHERE id=$1),'question','{"id":"stopped-question"}',$2)`, e.run, e.gen)
	stoppedRouteExec(t, e, `UPDATE runs SET last_seq=last_seq+1 WHERE id=$1`, e.run)
	var populated bool
	if err := e.pool.QueryRow(context.Background(), `SELECT r.session_id IS NOT NULL AND r.gate_presentation_id=$2
 AND r.open_question_id='stopped-question'
 AND EXISTS (SELECT 1 FROM run_gate_presentations g WHERE g.run_id=r.id AND g.presentation_id=$2)
 AND EXISTS (SELECT 1 FROM run_messages m WHERE m.run_id=r.id AND m.kind='question')
 FROM runs r WHERE r.id=$1`, e.run, presentation).Scan(&populated); err != nil || !populated {
		t.Fatal("fixture did not populate session, gate and question state", err)
	}
	reportStoppedRoute(t, e, w, true, true)
	reportStoppedRoute(t, e, w, true, false)
}
