package workersvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestSemanticCandidateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	worker := e.seedWorker(t, nil)
	run := e.seedActiveRunOwnedBy(t, worker)
	e.exec(t, `UPDATE runs SET claim_generation=7, completion_contract_version=1 WHERE id=$1`, run)
	e.exec(t, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"reserved","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run)
	svc := &Service{txBeginner: e.pool}
	c, p, s := semanticFixture()
	raw := semanticJSON(t, p)
	submit := func() (apitypes.SemanticCandidate, error) {
		return svc.submitSemanticCandidate(e.ctx, worker, run, 7, c, raw, s)
	}
	first, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	contractRaw, contractErr := buildSemanticCompletionContract(first, 1)
	if contractErr != nil {
		t.Fatal(contractErr)
	}
	contract, contractErr := parseCompletionContract(contractRaw)
	if contractErr != nil || contract.Profile != "semantic" || contract.Criteria[0].ID != first.Criteria[0].ID {
		t.Fatalf("canonical contract: %v", contractErr)
	}
	if first.Revision != 1 || uuid.Validate(first.Criteria[0].ID) != nil || first.Digest == "" {
		t.Fatalf("candidate: %+v", first)
	}
	var stored []byte
	if err = e.pool.QueryRow(e.ctx, `SELECT raw FROM semantic_snapshots WHERE run_id=$1 AND hash=$2`, run, first.Sources[0].Hash).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var provenance, completeness string
	if err = e.pool.QueryRow(e.ctx, `SELECT provenance,completeness FROM semantic_source_snapshots WHERE run_id=$1 AND source_id=$2 AND hash=$3`, run, s[0].ID, first.Sources[0].Hash).Scan(&provenance, &completeness); err != nil {
		t.Fatal(err)
	}
	if provenance != s[0].Provenance || completeness != s[0].Completeness {
		t.Fatal("snapshot metadata roundtrip")
	}
	if !bytes.Equal(stored, s[0].Bytes) {
		t.Fatalf("source roundtrip: %q", stored)
	}
	var persisted []byte
	if err = e.pool.QueryRow(e.ctx, `SELECT candidate FROM semantic_planning WHERE run_id=$1`, run).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var decoded apitypes.SemanticCandidate
	if err = json.Unmarshal(persisted, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != first.Digest {
		t.Fatal("write/read mismatch")
	}
	const siblings = 4
	results := make([]apitypes.SemanticCandidate, siblings)
	errs := make([]error, siblings)
	var wg sync.WaitGroup
	for i := range siblings {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], errs[i] = submit() }()
	}
	wg.Wait()
	for i := range siblings {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].Criteria[0].ID != first.Criteria[0].ID || results[i].Revision != 1 || results[i].Digest != first.Digest {
			t.Fatal("lost-response retry reminted identity")
		}
	}
	if _, err = svc.submitSemanticCandidate(e.ctx, worker, run, 6, c, raw, s); err == nil {
		t.Fatal("stale generation accepted")
	}
	if _, err = svc.submitSemanticCandidate(e.ctx, uuid.New(), run, 7, c, raw, s); err == nil {
		t.Fatal("foreign worker accepted")
	}
	p.Criteria[0].Text = "B"
	raw = semanticJSON(t, p)
	second, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 {
		t.Fatal("revision B")
	}
	p.Criteria[0].Text = "behavior"
	raw = semanticJSON(t, p)
	third, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	if third.Revision != 3 || third.Criteria[0].ID == first.Criteria[0].ID || third.Digest == first.Digest {
		t.Fatal("A-B-A restored old authorization identity")
	}
	var blobCount, historyCount int
	if err = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM semantic_snapshots WHERE run_id=$1`, run).Scan(&blobCount); err != nil {
		t.Fatal(err)
	}
	if err = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM semantic_candidate_history WHERE run_id=$1`, run).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if blobCount != 2 || historyCount != 3 {
		t.Fatalf("dedup/history: %d/%d", blobCount, historyCount)
	}
	// Fill byte capacity with distinct synthetic historical blobs, without evicting.
	remaining := 16*1024*1024 - len(s[0].Bytes) - len(c.PlanMd)
	for i := 0; remaining > 0; i++ {
		size := min(remaining, 256*1024)
		blob := bytes.Repeat([]byte("x"), size)
		blob[0] = byte(i + 32)
		e.exec(t, `INSERT INTO semantic_snapshots(run_id,hash,raw) VALUES($1,$2,$3)`, run, semanticHash(blob), blob)
		remaining -= size
	}
	p.Criteria[0].Text = "behavior"
	raw = semanticJSON(t, p)
	if _, err = submit(); err != nil {
		t.Fatalf("identical retry at byte cap: %v", err)
	}
	originalPlan := c.PlanMd
	c.PlanMd = "new plan"
	if _, err = submit(); err == nil {
		t.Fatal("snapshot capacity accepted")
	}
	var finalRevision int64
	if err = e.pool.QueryRow(e.ctx, `SELECT revision FROM semantic_planning WHERE run_id=$1`, run).Scan(&finalRevision); err != nil {
		t.Fatal(err)
	}
	if finalRevision != 3 {
		t.Fatal("capacity refusal mutated planning state")
	}
	c.PlanMd = originalPlan
	var current apitypes.SemanticCandidate
	// Exactly 125 further submissions reach the 128-candidate ceiling, using
	// identical snapshots. Each failure stops the test; there is no retry loop.
	for revision := 4; revision <= 128; revision++ {
		p.Criteria[0].Text = fmt.Sprintf("candidate %d", revision)
		raw = semanticJSON(t, p)
		current, err = submit()
		if err != nil {
			t.Fatal(err)
		}
		if current.Revision != int64(revision) {
			t.Fatalf("revision %d: %d", revision, current.Revision)
		}
	}
	retry, err := submit()
	if err != nil || retry.Digest != current.Digest || retry.Criteria[0].ID != current.Criteria[0].ID {
		t.Fatalf("retry at both caps: %v", err)
	}
	p.Criteria[0].Text = "over capacity"
	raw = semanticJSON(t, p)
	if _, err = submit(); err == nil {
		t.Fatal("candidate capacity accepted")
	}
	if err = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM semantic_candidate_history WHERE run_id=$1`, run).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 128 {
		t.Fatal("candidate refusal changed history")
	}
}

func TestSemanticEnrollmentDormantLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	worker := e.seedWorker(t, nil)
	run := e.seedActiveRunOwnedBy(t, worker)
	e.exec(t, `UPDATE runs SET completion_contract_version=1 WHERE id=$1`, run)
	c, p, s := semanticFixture()
	if _, err := (&Service{txBeginner: e.pool}).submitSemanticCandidate(e.ctx, worker, run, 0, c, semanticJSON(t, p), s); err == nil {
		t.Fatal("real unenrolled run admitted")
	}
}
