package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const acceptanceMACCopy = "terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody"

func acceptanceRunState(t *testing.T, e *settleEnv) {
	t.Helper()
	rejectionAssertPark(t, e)
	var status string
	var origin, got *string
	var generation int64
	var requeues int
	var allowanceUnused, noLease bool
	if err := e.pool.QueryRow(e.ctx, `SELECT status, fail_origin, failure_reason,
		claim_generation, requeue_count, finalize_resume_generation IS NULL,
		NOT EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.run_id=r.id
			AND a.worker_id=r.worker_id AND a.claim_generation=r.claim_generation)
		FROM runs r WHERE id=$1`, e.run).
		Scan(&status, &origin, &got, &generation, &requeues, &allowanceUnused, &noLease); err != nil {
		t.Fatal(err)
	}
	if status != "recovery_wait" || origin != nil || got != nil ||
		generation != 3 || requeues != 0 || !allowanceUnused || !noLease {
		t.Fatalf("exhaustion hold = %s/%v %v G%d requeues=%d unused=%t noLease=%t",
			status, origin, got, generation, requeues, allowanceUnused, noLease)
	}
}

func acceptanceRegister(t *testing.T, e *settleEnv) {
	t.Helper()
	rec := rejectionHTTP(e, http.MethodPost, "/api/worker/register", e.tokenA, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d", rec.Code)
	}
	var registration struct {
		Features []string `json:"protocol_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, feature := range registration.Features {
		found = found || feature == "terminal_rejection_report"
	}
	if !found {
		t.Fatal("registration did not advertise terminal_rejection_report")
	}
	acceptanceRunState(t, e)
}

func acceptanceCustody(t *testing.T, e *settleEnv, id uuid.UUID, outcome, state string) {
	t.Helper()
	rec := rejectionHTTP(e, http.MethodGet,
		fmt.Sprintf("/api/worker/runs/%s/terminal-rejection-custody?generation=3", e.run), e.tokenA, "")
	var snapshot workersvc.TerminalRejectionCustody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("custody GET = %d %s", rec.Code, rec.Body.String())
	}
	var exact []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(snapshot.ExactHolds, &exact); err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != e.run.String() || snapshot.WorkerID != e.workerA.String() ||
		snapshot.Generation != 3 || snapshot.ExactCount != 1 || !snapshot.Complete ||
		snapshot.Outcome != outcome || len(exact) != 1 || exact[0].State != state ||
		exact[0].ID != id.String() {
		t.Fatalf("custody = %+v exact=%+v", snapshot, exact)
	}
}

func acceptanceOwnerHold(t *testing.T, e *settleEnv, token string, id uuid.UUID) apitypes.RecoveryCustodyHoldDTO {
	t.Helper()
	rec := bearerReq(e.router, http.MethodGet, "/api/recovery/holds", token)
	var dto apitypes.RecoveryCustodyHoldsDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dto) != nil {
		t.Fatalf("owner holds = %d %s", rec.Code, rec.Body.String())
	}
	for _, hold := range dto.Holds {
		if hold.ID == id.String() {
			return hold
		}
	}
	t.Fatalf("owner GET missing exact hold %s", id)
	return apitypes.RecoveryCustodyHoldDTO{}
}

func TestTerminalRejectionAcceptanceLiveDB(t *testing.T) {
	e := newSettleEnv(t)
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Minute)
	defer cancel()
	e.ctx = ctx
	e.wsvc.SetTxBeginner(e.pool)
	// This POST body is recorded by the actual worker; never synthesize it here.
	body, err := os.ReadFile("../../../fixtures/terminal-rejection/mac-failure.json")
	if err != nil {
		t.Fatalf("actual-worker fixture must be produced before the live acceptance run: %v", err)
	}
	var fixture struct {
		Rejections []workersvc.TerminalRejection `json:"rejections"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Rejections) != 1 || workersvc.ValidateTerminalRejections(fixture.Rejections) != nil ||
		*fixture.Rejections[0].ClaimGeneration != 3 {
		t.Fatal("fixture must contain one valid G3 mac_failure rejection")
	}
	e.run = uuid.MustParse(fixture.Rejections[0].RunID)
	e.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
		status,worker_id,claim_generation,requeue_count,branch,started_at)
		VALUES ($1,$2,$3,'issue',99,'acceptance','fixture replay','running',$4,3,0,'agent/issue-99',now())`,
		e.run, e.user, e.repo, e.workerA)
	exact := e.insertHold(e.run, 3, e.workerA)
	sibling := e.insertHold(e.run, 2, e.workerA)
	// The fixture UUID is stable across executions; retire only this test's rows.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, statement := range []string{
			"DELETE FROM recovery_custody_holds WHERE run_id=$1 AND user_id=$2",
			"DELETE FROM runs WHERE id=$1 AND user_id=$2",
		} {
			if _, err := e.pool.Exec(cleanupCtx, statement, e.run, e.user); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
	})
	acceptanceRegister(t, e)
	ownerToken := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	if h := acceptanceOwnerHold(t, e, ownerToken, exact); h.TerminalRecordRejection != "" {
		t.Fatalf("registration manufactured a diagnostic: %+v", h)
	}
	acceptanceCustody(t, e, exact, "retained", "open")
	for attempt := 0; attempt < 2; attempt++ {
		assertRejectionDisposition(t, rejectionHTTP(e, http.MethodPost,
			"/api/worker/terminal-rejections", e.tokenA, string(body)), e.run, 3, "recorded")
	}
	acceptanceRunState(t, e)
	e.assertOpen(exact, sibling)
	h := acceptanceOwnerHold(t, e, ownerToken, exact)
	if h.RunID != e.run.String() || h.Generation != 3 || h.State != "open" ||
		h.Attention != "source_only" || h.TerminalRecordRejection != "mac_failure" ||
		h.HasAvailableCapture || h.CaptureState != "" {
		t.Fatalf("owner source custody = %+v", h)
	}
	if siblingHold := acceptanceOwnerHold(t, e, ownerToken, sibling); siblingHold.TerminalRecordRejection != "" {
		t.Fatalf("report annotated sibling: %+v", siblingHold)
	}
	rec := bearerReq(e.router, http.MethodGet, "/api/runs/"+e.run.String()+"/archives", ownerToken)
	var archives apitypes.RecoveryArchiveSummaryDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &archives) != nil ||
		len(archives.Archives) != 0 || archives.Counts.Available != 0 {
		t.Fatalf("archives = %d %s", rec.Code, rec.Body.String())
	}

	if err := os.MkdirAll("../../../.uzi/scratch", 0o700); err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp("../../../.uzi/scratch", "terminal-acceptance.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	scratch, err = filepath.Abs(scratch)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(scratch, "uzi")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "./cmd/uzi") //nolint:gosec // G204: fixed command and build args; test-owned fixture output path, no shell
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CLI build: %v %s", err, output)
	}
	server := httptest.NewServer(e.router)
	defer server.Close()
	cli := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // G204: generated test-owned binary; fixed test args and fixture IDs/paths, no shell
		// Replace inherited credentials/config; the token never enters argv.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "UZI_") && !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "UZI_URL="+server.URL, "UZI_TOKEN="+ownerToken, "XDG_CONFIG_HOME="+scratch)
		output, err := cmd.CombinedOutput()
		return []byte(strings.ReplaceAll(string(output), ownerToken, "[redacted]")), err
	}
	text, err := cli("run", "recovery", e.run.String())
	if err != nil {
		t.Fatalf("CLI recovery: %v %s", err, text)
	}
	for _, want := range []string{acceptanceMACCopy, "source_only", "export unavailable", exact.String(),
		fmt.Sprintf("hold %s gen 3:", exact)} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("CLI recovery missing %q: %s", want, text)
		}
	}
	raw, err := cli("run", "recovery", e.run.String(), "--json")
	if err != nil {
		t.Fatalf("CLI JSON: %v %s", err, raw)
	}
	var rows []struct {
		apitypes.RecoveryCustodyHoldDTO
		TerminalRejection string            `json:"terminal_rejection"`
		Captures          []json.RawMessage `json:"captures"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("CLI JSON decode: %v %s", err, raw)
	}
	found := false
	for _, row := range rows {
		if row.ID == exact.String() {
			found = row.Generation == 3 && row.TerminalRecordRejection == "mac_failure" &&
				row.TerminalRejection == acceptanceMACCopy && row.Attention == "source_only" &&
				!row.HasAvailableCapture && row.Captures != nil && len(row.Captures) == 0
		}
	}
	if !found {
		t.Fatalf("CLI JSON lost raw diagnostic or exact source custody: %s", raw)
	}
	destination := filepath.Join(scratch, "unavailable.bundle")
	if output, err := cli("run", "export", e.run.String(), "--output", destination); err == nil ||
		!strings.Contains(string(output), "no recovery archive to export") {
		t.Fatalf("CLI export should refuse absent archive: %v %s", err, output)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("refused export created a file: %v", err)
	}

	discard := func(id uuid.UUID) {
		t.Helper()
		rec := bearerReq(e.router, http.MethodDelete,
			fmt.Sprintf("/api/runs/%s/recovery-holds/%s?confirm=discard", e.run, id), ownerToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("owner discard = %d %s", rec.Code, rec.Body.String())
		}
	}
	discard(exact)
	closed := e.hold(exact)
	if closed.state != "discarded" || closed.evidence != "owner_discard" ||
		!closed.liveWorkerNull || !closed.liveRunNull {
		t.Fatalf("discarded exact row = %+v", closed)
	}
	if h := acceptanceOwnerHold(t, e, ownerToken, exact); h.State != "discarded" ||
		h.Attention != "discarded" || h.TerminalRecordRejection != "mac_failure" {
		t.Fatalf("authoritative discarded owner row = %+v", h)
	}
	e.assertOpen(sibling)
	acceptanceCustody(t, e, exact, "unknown", "discarded")
	discard(sibling)
	acceptanceCustody(t, e, exact, "settled", "discarded")
	assertRejectionDisposition(t, rejectionHTTP(e, http.MethodPost,
		"/api/worker/terminal-rejections", e.tokenA, string(body)), e.run, 3, "skipped")
	if after := e.hold(exact); after != closed {
		t.Fatalf("closed row changed on replay: before=%+v after=%+v", closed, after)
	}
	var provenance bool
	if err := e.pool.QueryRow(e.ctx, `SELECT run_id=$2 AND generation=3 AND original_worker_id=$3
		AND user_id=$4 AND terminal_record_rejection='mac_failure'
		FROM recovery_custody_holds WHERE id=$1`, exact, e.run, e.workerA, e.user).Scan(&provenance); err != nil || !provenance {
		t.Fatalf("closed immutable provenance lost: %t %v", provenance, err)
	}
}

// An older worker ignores the additive feature and sends no diagnostic report.
func TestTerminalRejectionOldWorkerNewAPILiveDB(t *testing.T) {
	e := newSettleEnv(t)
	e.wsvc.SetTxBeginner(e.pool)
	e.exec("UPDATE runs SET status='running', claim_generation=3, requeue_count=0, started_at=now() WHERE id=$1", e.run)
	exact := e.insertHold(e.run, 3, e.workerA)
	acceptanceRegister(t, e)
	token := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	if h := acceptanceOwnerHold(t, e, token, exact); h.TerminalRecordRejection != "" ||
		h.Attention != "source_only" || h.HasAvailableCapture {
		t.Fatalf("old worker custody changed by feature advertisement: %+v", h)
	}
	e.assertOpen(exact, e.pred, e.sibGen, e.sibWork)
	acceptanceRunState(t, e)
}
