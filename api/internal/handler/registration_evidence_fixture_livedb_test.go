package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Issue #3859: the shared worker-protocol fixture (the agent's test sends the same bytes) is
// posted to the real worker state route against the live database. Only VALUES are substituted,
// so a key renamed in the fixture or in the Go wire type reddens this test.

func fixtureKeys(m map[string]any) []string {
	var out []string
	for k, v := range m {
		out = append(out, k)
		if sub, ok := v.(map[string]any); ok {
			for _, s := range fixtureKeys(sub) {
				out = append(out, k+"."+s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestRegistrationEvidenceFixtureRouteLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/registration-evidence-running.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	var orig map[string]any
	if err := json.Unmarshal(raw, &orig); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	wantKeys := fixtureKeys(orig)

	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 3859, true)
	run := rmSeedRun(t, pool, owner, repo, "claimed")
	worker := uuid.New()
	token := "evidence-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	const gen, epoch = int64(4), int64(11)
	const nonce = "live-register-nonce"
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,snapshot_register_nonce) VALUES($1,$2,'evidence',$3,'online',$4,$5)",
		worker, owner, sum[:], []string{}, nonce)
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2, claim_generation=$3, claimed_at=now() WHERE id=$1", run, worker, gen)

	// body builds the request from the fixture with only the values substituted.
	body := func(extraNested bool) []byte {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		ev, ok := m["registration_evidence"].(map[string]any)
		if !ok {
			t.Fatalf("fixture registration_evidence is not an object: %T", m["registration_evidence"])
		}
		m["claim_generation"] = gen
		ev["claim_generation"] = gen
		ev["register_nonce"] = nonce
		ev["registration_epoch"] = epoch
		if !extraNested {
			if got := fixtureKeys(m); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
				t.Fatalf("key set changed: %v, want %v", got, wantKeys)
			}
		} else {
			ev["unknown_nested_key"] = 1
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The strict decoder refuses an unknown nested key before anything is written.
	rec := bearerReqBody(router, http.MethodPost, "/api/worker/runs/"+run.String()+"/state", token, string(body(true)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown nested key: code=%d %s, want 400", rec.Code, rec.Body.String())
	}
	var nGen, nEpoch *int64
	if err := pool.QueryRow(t.Context(), `SELECT registration_evidence_generation, registration_evidence_epoch FROM runs WHERE id=$1`, run).Scan(&nGen, &nEpoch); err != nil {
		t.Fatal(err)
	}
	if nGen != nil || nEpoch != nil {
		t.Fatalf("a 400 wrote evidence: gen=%v epoch=%v", nGen, nEpoch)
	}

	rec = bearerReqBody(router, http.MethodPost, "/api/worker/runs/"+run.String()+"/state", token, string(body(false)))
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("fixture post: code=%d %s", rec.Code, rec.Body.String())
	}
	var ack map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("ack is not JSON: %v", err)
	}
	if got := string(bytes.TrimSpace(ack["registration_evidence"])); got != `"recorded"` {
		t.Fatalf("ack registration_evidence = %s, want \"recorded\" (ack %s)", got, rec.Body.String())
	}

	var rGen, rEpoch int64
	var rNonce string
	if err := pool.QueryRow(t.Context(), `SELECT registration_evidence_generation, registration_evidence_nonce, registration_evidence_epoch FROM runs WHERE id=$1`, run).Scan(&rGen, &rNonce, &rEpoch); err != nil {
		t.Fatalf("read evidence columns: %v", err)
	}
	if rGen != gen || rNonce != nonce || rEpoch != epoch {
		t.Fatalf("evidence columns = (%d, %q, %d), want (%d, %q, %d)", rGen, rNonce, rEpoch, gen, nonce, epoch)
	}
}
