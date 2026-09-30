package workersvc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestJobClaimCarriesOnlyItsOwnAttachedInputsLiveDB (PRD #1909 D8): the claim's files manifest
// lists exactly the run's attached INPUT files, in (created_at, id) order: not another run's
// attached input, not an unattached upload, not the run's own output. A job with no files carries
// "files": [] (never null).
func TestJobClaimCarriesOnlyItsOwnAttachedInputsLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	// Another run of the same owner, parked so it is never claimed here.
	other := e.seedRawJob(t, u, "awaiting_approval", nil, 0, 600)

	runA := v.ID
	gen := int64(1)
	first := e.put(t, ReserveParams{UserID: u, RunID: &runA, DisplayName: "first.txt"}, []byte("first file"))
	second := e.put(t, ReserveParams{UserID: u, RunID: &runA, DisplayName: "second.txt"}, []byte("second file"))
	e.put(t, ReserveParams{UserID: u, RunID: &other, DisplayName: "foreign.txt"}, []byte("another run's file"))
	e.put(t, ReserveParams{UserID: u, DisplayName: "loose.txt"}, []byte("never attached"))
	e.put(t, ReserveParams{UserID: u, RunID: &runA, Direction: JobFileOutput, ClaimGeneration: &gen, DisplayName: "out.txt"}, []byte("an output"))
	// Pin the order: created_at can tie inside one transaction-less run of inserts.
	e.exec(`UPDATE job_files SET created_at = now() - interval '2 minutes' WHERE id = $1`, first.ID)
	e.exec(`UPDATE job_files SET created_at = now() - interval '1 minute' WHERE id = $1`, second.ID)

	capID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap, capability.RecoveryArchiveV1)
	capable := store.Worker{ID: capID, UserID: u, Name: "capable", Status: "online", ProtocolCapabilities: []string{capability.RecoveryArchiveV1, jobCap, jobFilesCap}}
	pl, err := e.svc.Claim(e.ctx, capable, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if pl == nil || pl.RunID != runA.String() || pl.Job == nil {
		t.Fatalf("Claim = %+v, want job %s", pl, runA)
	}
	if len(pl.Job.Files) != 2 {
		t.Fatalf("files = %+v, want exactly the run's 2 attached inputs", pl.Job.Files)
	}
	for i, want := range []struct {
		row  store.JobFile
		body string
		disp string
	}{{first, "first file", "first.txt"}, {second, "second file", "second.txt"}} {
		sum := sha256.Sum256([]byte(want.body))
		digest := hex.EncodeToString(sum[:])
		got := pl.Job.Files[i]
		wantFile := ClaimJobFile{
			ID: want.row.ID.String(), Name: digest + ".txt", DisplayName: want.disp,
			Size: int64(len(want.body)), SHA256: digest, ContentType: "text/plain",
		}
		if got != wantFile {
			t.Fatalf("files[%d] = %+v, want %+v", i, got, wantFile)
		}
	}
}

// TestJobClaimFilesEmptyLiveDB: a job with no files claims with a non-nil empty manifest.
func TestJobClaimFilesEmptyLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	capID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	capable := store.Worker{ID: capID, UserID: u, Name: "capable", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}
	pl, err := e.svc.Claim(e.ctx, capable, nil)
	if err != nil || pl == nil || pl.Job == nil {
		t.Fatalf("Claim = %+v, %v", pl, err)
	}
	if pl.Job.Files == nil || len(pl.Job.Files) != 0 {
		t.Fatalf("files = %#v, want a non-nil empty slice", pl.Job.Files)
	}
}

// TestJobClaimCarriesInputLimitsLiveDB (PRD #1909 D1): a job claim carries the server's input caps
// so the worker can refuse an oversized manifest before downloading, and a claim on a service with
// no job-file store omits them (the worker then uses its fixed ceilings alone).
func TestJobClaimCarriesInputLimitsLiveDB(t *testing.T) {
	l := wide()
	l.InputFileMaxBytes, l.InputsMaxFiles, l.InputsMaxBytes = 4<<20, 3, 9<<20
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	t.Cleanup(func() { e.svc.SetJobFiles(nil) })
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	capID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	capable := store.Worker{ID: capID, UserID: u, Name: "capable", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}
	pl, err := e.svc.Claim(e.ctx, capable, nil)
	if err != nil || pl == nil || pl.Job == nil {
		t.Fatalf("Claim = %+v, %v", pl, err)
	}
	if got := (pl.Config.JobInputFileMaxBytes); got != 4<<20 {
		t.Fatalf("job_input_file_max_bytes = %d, want %d", got, 4<<20)
	}
	if pl.Config.JobInputsMaxFiles != 3 || pl.Config.JobInputsMaxBytes != 9<<20 {
		t.Fatalf("inputs caps = %d files / %d bytes, want 3 / %d", pl.Config.JobInputsMaxFiles, pl.Config.JobInputsMaxBytes, 9<<20)
	}

	// With no job-file store the claim omits the limits: the keys are absent from the wire JSON.
	e.svc.SetJobFiles(nil)
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
		t.Fatalf("CreateJobRun (no store): %v", err)
	}
	capID2 := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	capable2 := store.Worker{ID: capID2, UserID: u, Name: "capable-2", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}
	pl2, err := e.svc.Claim(e.ctx, capable2, nil)
	if err != nil || pl2 == nil || pl2.Job == nil {
		t.Fatalf("Claim (no store) = %+v, %v", pl2, err)
	}
	raw, err := json.Marshal(pl2.Config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	for _, key := range []string{"job_input_file_max_bytes", "job_inputs_max_files", "job_inputs_max_bytes"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("a claim on a service with no job-file store carries %q: %s", key, raw)
		}
	}
}
