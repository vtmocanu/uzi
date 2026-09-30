package workersvc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobfiles_livedb_test.go covers the PRD #1909 M1 job-file store (workersvc/jobfiles.go) against a
// live Postgres: the D1 cap edges, owner scoping, the shared budget, AAD-bound chunks, expiry and
// reservation release, and atomic admission under concurrency. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway database; run via ./e2e/run-store-it.sh.
//
// The quota sums are instance-wide, and the live-DB packages share one database, so every test
// here first empties the two file stores (resetStoredFiles). That is safe only because the sweep
// runs the packages with -p 1 against a throwaway database.

// jfEnv is one test's job-file harness.
type jfEnv struct {
	jobEnv
	jf *JobFiles
}

// resetStoredFiles empties job_files (chunks cascade) and recovery_captures so instance-wide
// sums start from zero. Other tests seed their own rows at their own start.
func resetStoredFiles(e codexTestEnv) {
	e.exec(`DELETE FROM job_files`)
	e.exec(`DELETE FROM recovery_captures`)
}

func newJFEnv(t *testing.T, limits JobFileLimits) jfEnv {
	t.Helper()
	e := setupJobLiveDB(t, 0)
	resetStoredFiles(e.codexTestEnv)
	return jfEnv{jobEnv: e, jf: NewJobFiles(e.pool, e.box, limits, nil)}
}

// jobFor seeds a repo-less job run for u, the parent of output files.
func (e jfEnv) jobFor(t *testing.T, u uuid.UUID) uuid.UUID {
	t.Helper()
	return e.seedRawJob(t, u, "queued", nil, 0, 600)
}

func (e jfEnv) sums(t *testing.T, owner uuid.UUID) store.StoredFilesSums {
	t.Helper()
	s, err := store.SumStoredFiles(e.ctx, e.q, owner, uuid.Nil)
	if err != nil {
		t.Fatalf("SumStoredFiles: %v", err)
	}
	return s
}

func (e jfEnv) fileState(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM job_files WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read file state: %v", err)
	}
	return s
}

func (e jfEnv) fileExists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_files WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func (e jfEnv) chunkCount(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_file_chunks WHERE file_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// put reserves and writes one text file, returning its committed row.
func (e jfEnv) put(t *testing.T, p ReserveParams, body []byte) store.JobFile {
	t.Helper()
	if p.DisplayName == "" {
		p.DisplayName = "f.txt"
	}
	if p.Direction == "" {
		p.Direction = JobFileInput
	}
	p.DeclaredSize = int64(len(body))
	row, err := e.jf.Reserve(e.ctx, p)
	if err != nil {
		t.Fatalf("Reserve(%d bytes): %v", len(body), err)
	}
	out, err := e.jf.Write(e.ctx, row.ID, p.UserID, bytes.NewReader(body), WriteOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return out
}

func refusedWith(err error, kind RefusalKind, reason string) bool {
	var r *JobFileRefusedError
	return errors.As(err, &r) && r.Kind == kind && r.Reason == reason
}

func wide() JobFileLimits {
	return JobFileLimits{
		InputFileMaxBytes: 1 << 30, InputsMaxFiles: 1000, InputsMaxBytes: 1 << 40,
		OutputFileMaxBytes: 1 << 30, OutputsMaxFiles: 1000, OutputsMaxBytes: 1 << 40,
		PerOwnerBytes: 1 << 40, InstanceBytes: 1 << 40, StoredFilesBudgetBytes: 1 << 40,
	}
}

// TestJobFilesReserveCapEdgesLiveDB: every D1 cap admits a request exactly AT the cap and refuses
// one byte (or one file) over, with the stated reason, and the reservation is exactly the declared
// size (a small file fits the room left below the per-file cap).
func TestJobFilesReserveCapEdgesLiveDB(t *testing.T) {
	t.Run("per-file cap: at accepted, one over refused", func(t *testing.T) {
		l := wide()
		l.InputFileMaxBytes, l.OutputFileMaxBytes = 100, 60
		e := newJFEnv(t, l)
		u := e.seedUser(t)
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 100}); err != nil {
			t.Fatalf("input at the cap: %v", err)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 101}); !refusedWith(err, RefusalLimit, RefusalFileTooLarge) {
			t.Fatalf("input one over the cap = %v, want limit/file_too_large", err)
		}
		run := e.jobFor(t, u)
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, RunID: &run, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: 60}); err != nil {
			t.Fatalf("output at the cap: %v", err)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, RunID: &run, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: 61}); !refusedWith(err, RefusalLimit, RefusalFileTooLarge) {
			t.Fatalf("output one over the cap = %v, want limit/file_too_large", err)
		}
	})

	t.Run("per-job output file count and bytes: at accepted, over refused", func(t *testing.T) {
		l := wide()
		l.OutputsMaxFiles, l.OutputsMaxBytes = 3, 30
		e := newJFEnv(t, l)
		u := e.seedUser(t)
		run := e.jobFor(t, u)
		out := func(size int64) error {
			_, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, RunID: &run, Direction: JobFileOutput, DisplayName: "o.txt", DeclaredSize: size})
			return err
		}
		for i := 0; i < 2; i++ {
			if err := out(10); err != nil {
				t.Fatalf("output %d: %v", i, err)
			}
		}
		if err := out(11); !refusedWith(err, RefusalLimit, RefusalJobBytes) {
			t.Fatalf("output pushing the job to 31 bytes = %v, want limit/job_bytes_exceeded", err)
		}
		if err := out(10); err != nil { // third file, exactly 30 bytes in total
			t.Fatalf("third output at both caps: %v", err)
		}
		if err := out(1); !refusedWith(err, RefusalLimit, RefusalTooManyFiles) {
			t.Fatalf("fourth output = %v, want limit/too_many_files", err)
		}
	})

	t.Run("owner quota: at accepted, one over refused, small file fits the remainder", func(t *testing.T) {
		l := wide()
		l.PerOwnerBytes = 100
		e := newJFEnv(t, l)
		u := e.seedUser(t)
		row, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 90})
		if err != nil {
			t.Fatal(err)
		}
		if row.ByteSize != 90 || row.State != JobFileReserved {
			t.Fatalf("reservation = %d bytes in state %s, want exactly the declared 90 reserved", row.ByteSize, row.State)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "b.txt", DeclaredSize: 11}); !refusedWith(err, RefusalQuota, RefusalOwnerQuota) {
			t.Fatalf("11 bytes into 10 remaining = %v, want quota/owner_quota", err)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "b.txt", DeclaredSize: 10}); err != nil {
			t.Fatalf("10 bytes into 10 remaining (below the per-file cap): %v", err)
		}
	})

	t.Run("instance quota and shared budget", func(t *testing.T) {
		l := wide()
		l.InstanceBytes = 100
		e := newJFEnv(t, l)
		a, b := e.seedUser(t), e.seedUser(t)
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: a, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 100}); err != nil {
			t.Fatalf("instance at the cap: %v", err)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: b, Direction: JobFileInput, DisplayName: "b.txt", DeclaredSize: 1}); !refusedWith(err, RefusalQuota, RefusalInstanceQuota) {
			t.Fatalf("one byte over the instance cap = %v, want quota/instance_quota", err)
		}

		l = wide()
		l.StoredFilesBudgetBytes = 100
		e = newJFEnv(t, l)
		a = e.seedUser(t)
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: a, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 100}); err != nil {
			t.Fatalf("budget at the cap: %v", err)
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: a, Direction: JobFileInput, DisplayName: "b.txt", DeclaredSize: 1}); !refusedWith(err, RefusalQuota, RefusalStoredBudget) {
			t.Fatalf("one byte over the shared budget = %v, want quota/stored_budget", err)
		}
	})

	t.Run("malformed requests", func(t *testing.T) {
		e := newJFEnv(t, wide())
		u := e.seedUser(t)
		for name, p := range map[string]ReserveParams{
			"bad direction":        {UserID: u, Direction: "sideways", DisplayName: "a.txt", DeclaredSize: 1},
			"path in name":         {UserID: u, Direction: JobFileInput, DisplayName: "../a.txt", DeclaredSize: 1},
			"control in name":      {UserID: u, Direction: JobFileInput, DisplayName: "a\x00.txt", DeclaredSize: 1},
			"empty name":           {UserID: u, Direction: JobFileInput, DisplayName: "", DeclaredSize: 1},
			"output without run":   {UserID: u, Direction: JobFileOutput, DisplayName: "a.txt", DeclaredSize: 1},
			"non-hex declared sha": {UserID: u, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 1, DeclaredSHA256: "XYZ"},
		} {
			if _, err := e.jf.Reserve(e.ctx, p); !errors.Is(err, ErrJobFileInvalid) {
				t.Errorf("%s: err = %v, want ErrJobFileInvalid", name, err)
			}
		}
		if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "a.txt", DeclaredSize: 0}); !refusedWith(err, RefusalInvalid, RefusalEmptyFile) {
			t.Errorf("zero-byte declared size = %v, want invalid/empty_file", err)
		}
	})
}

// TestJobFilesOwnerScopingLiveDB: a file is only ever the owner's, and one owner's usage never
// counts against another owner's per-owner quota.
func TestJobFilesOwnerScopingLiveDB(t *testing.T) {
	l := wide()
	l.PerOwnerBytes = 50
	e := newJFEnv(t, l)
	a, b := e.seedUser(t), e.seedUser(t)
	fileA := e.put(t, ReserveParams{UserID: a}, []byte(strings.Repeat("a", 50))) // A is now at the owner quota

	if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: a, Direction: JobFileInput, DisplayName: "more.txt", DeclaredSize: 1}); !refusedWith(err, RefusalQuota, RefusalOwnerQuota) {
		t.Fatalf("owner A over its quota = %v, want owner_quota", err)
	}
	if _, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: b, Direction: JobFileInput, DisplayName: "b.txt", DeclaredSize: 50}); err != nil {
		t.Fatalf("owner B must not be charged for A's bytes: %v", err)
	}

	if _, err := e.jf.Get(e.ctx, fileA.ID, b); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("Get as another owner = %v, want ErrJobFileNotFound", err)
	}
	if _, _, err := e.jf.Open(e.ctx, fileA.ID, b); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("Open as another owner = %v, want ErrJobFileNotFound", err)
	}
	if released, err := e.jf.Release(e.ctx, fileA.ID, b); err != nil || released {
		t.Fatalf("Release of A's committed file as B = %v, %v; want (false, nil)", released, err)
	}
	if _, err := e.jf.Write(e.ctx, fileA.ID, b, strings.NewReader("x"), WriteOptions{ContentType: "text/plain"}); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("Write to A's file as B = %v, want ErrJobFileNotFound", err)
	}
	if e.fileState(t, fileA.ID) != JobFileUnattached {
		t.Fatal("B's attempts must not have touched A's file")
	}
}

// TestJobFilesWriteOpenRoundTripLiveDB: a multi-chunk file round-trips through Write and Open with
// its content identity, resting state and expiry, and the stored chunks are ciphertext.
func TestJobFilesWriteOpenRoundTripLiveDB(t *testing.T) {
	l := wide()
	l.UploadTTL = 30 * time.Minute
	e := newJFEnv(t, l)
	u := e.seedUser(t)
	body := make([]byte, 2*JobFileChunkSize+12345)
	for i := range body {
		body[i] = byte('a' + i%26)
	}

	in := e.put(t, ReserveParams{UserID: u, DisplayName: "notes.txt"}, body)
	if in.State != JobFileUnattached || in.ChunkCount != 3 || in.ByteSize != int64(len(body)) {
		t.Fatalf("input = state %s chunks %d size %d, want unattached/3/%d", in.State, in.ChunkCount, in.ByteSize, len(body))
	}
	if !in.StorageName.Valid || in.StorageName.String != in.Sha256.String+".txt" || in.DisplayName != "notes.txt" {
		t.Fatalf("storage_name = %+v (sha %+v), display %q; want <sha256>.txt with the display name kept apart", in.StorageName, in.Sha256, in.DisplayName)
	}
	if want := time.Now().Add(30 * time.Minute); in.ExpiresAt.Time.Before(want.Add(-time.Minute)) || in.ExpiresAt.Time.After(want.Add(time.Minute)) {
		t.Fatalf("unattached expires_at = %v, want ~%v (the upload TTL)", in.ExpiresAt.Time, want)
	}
	meta, r, err := e.jf.Open(e.ctx, in.ID, u)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, body) || meta.ID != in.ID {
		t.Fatalf("Open round trip: err=%v equal=%v", err, bytes.Equal(got, body))
	}
	var sealed []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT sealed FROM job_file_chunks WHERE file_id = $1 AND chunk_index = 0`, in.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, body[:64]) {
		t.Fatal("a stored chunk contains plaintext; chunks must be sealed")
	}

	run := e.jobFor(t, u)
	gen := int64(1)
	out := e.put(t, ReserveParams{UserID: u, RunID: &run, Direction: JobFileOutput, ClaimGeneration: &gen, DisplayName: "report.md"}, []byte("# report"))
	if out.State != JobFileAttached || out.ExpiresAt.Valid || !out.RunID.Valid || !out.ClaimGeneration.Valid {
		t.Fatalf("output = state %s expires %v run %v gen %v, want attached, no expiry until the run ends", out.State, out.ExpiresAt.Valid, out.RunID.Valid, out.ClaimGeneration.Valid)
	}
	// A reservation is not readable, and a Write needs one.
	rsv, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "r.txt", DeclaredSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.jf.Open(e.ctx, rsv.ID, u); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("Open of a reservation = %v, want ErrJobFileNotFound", err)
	}
	if _, err := e.jf.Write(e.ctx, in.ID, u, strings.NewReader("data"), WriteOptions{ContentType: "text/plain"}); !errors.Is(err, ErrJobFileNotFound) {
		t.Fatalf("Write over a committed file = %v, want ErrJobFileNotFound", err)
	}
}

type rejectInspector struct{}

func (r rejectInspector) Begin([]byte) (string, error) { return "text/plain", nil }
func (r rejectInspector) Chunk([]byte) error           { return nil }
func (r rejectInspector) End() error                   { return errors.New("looks wrong") }

// TestJobFilesWriteRefusalReleasesReservationLiveDB: a body that is not the declared file (longer,
// shorter, wrong digest, rejected by the inspector, unsupported type) refuses the upload AND gives
// the reservation back in the same call: the row is gone and the sums drop.
func TestJobFilesWriteRefusalReleasesReservationLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	u := e.seedUser(t)
	sha := strings.Repeat("0", 64)

	cases := []struct {
		name     string
		declared int64
		body     string
		sha      string
		opt      WriteOptions
		reason   string
	}{
		{"longer than declared", 4, "abcdef", "", WriteOptions{ContentType: "text/plain"}, RefusalSizeMismatch},
		{"shorter than declared", 8, "abc", "", WriteOptions{ContentType: "text/plain"}, RefusalSizeMismatch},
		{"digest mismatch", 3, "abc", sha, WriteOptions{ContentType: "text/plain"}, RefusalSHAMismatch},
		{"inspector rejects", 3, "abc", "", WriteOptions{Inspector: rejectInspector{}}, RefusalContentInvalid},
		{"type not on the allowlist", 3, "abc", "", WriteOptions{ContentType: "application/x-msdownload"}, RefusalUnsupported},
	}
	for _, c := range cases {
		row, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "f.txt", DeclaredSize: c.declared, DeclaredSHA256: c.sha})
		if err != nil {
			t.Fatalf("%s: Reserve: %v", c.name, err)
		}
		if got := e.sums(t, u).OwnerJobBytes; got != c.declared {
			t.Fatalf("%s: reserved bytes = %d, want the declared %d", c.name, got, c.declared)
		}
		_, err = e.jf.Write(e.ctx, row.ID, u, strings.NewReader(c.body), c.opt)
		if !refusedWith(err, RefusalInvalid, c.reason) {
			t.Errorf("%s: Write err = %v, want invalid/%s", c.name, err, c.reason)
		}
		if e.fileExists(t, row.ID) {
			t.Errorf("%s: the reservation was not released", c.name)
		}
		if got := e.sums(t, u).OwnerJobBytes; got != 0 {
			t.Errorf("%s: owner bytes after the refusal = %d, want 0", c.name, got)
		}
	}
	// The right digest passes, so the mismatch above is the digest and not the harness.
	sum := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // sha256("abc")
	row, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "f.txt", DeclaredSize: 3, DeclaredSHA256: sum})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.jf.Write(e.ctx, row.ID, u, strings.NewReader("abc"), WriteOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Write with the matching digest: %v", err)
	}
}

// TestJobFilesOpenRejectsAADMismatchLiveDB: each chunk is sealed with an AAD binding file id,
// owner, index and length, so a chunk replayed into another file, another position, another owner
// or under a different length fails the tag check and no plaintext is released.
func TestJobFilesOpenRejectsAADMismatchLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	u, other := e.seedUser(t), e.seedUser(t)
	body := bytes.Repeat([]byte("x"), JobFileChunkSize+100) // two chunks
	a := e.put(t, ReserveParams{UserID: u, DisplayName: "a.txt"}, body)
	b := e.put(t, ReserveParams{UserID: u, DisplayName: "b.txt"}, bytes.Repeat([]byte("y"), JobFileChunkSize+100))

	readAll := func(id, owner uuid.UUID) error {
		_, r, err := e.jf.Open(e.ctx, id, owner)
		if err != nil {
			return err
		}
		_, err = io.ReadAll(r)
		return err
	}
	if err := readAll(a.ID, u); err != nil {
		t.Fatalf("untampered read: %v", err)
	}

	t.Run("chunk replayed from another file", func(t *testing.T) {
		e.exec(`UPDATE job_file_chunks SET sealed = (SELECT sealed FROM job_file_chunks WHERE file_id = $2 AND chunk_index = 0) WHERE file_id = $1 AND chunk_index = 0`, a.ID, b.ID)
		if err := readAll(a.ID, u); !errors.Is(err, ErrJobFileIntegrity) {
			t.Fatalf("read after replay from another file = %v, want ErrJobFileIntegrity", err)
		}
	})

	t.Run("chunks swapped within one file", func(t *testing.T) {
		c := e.put(t, ReserveParams{UserID: u, DisplayName: "c.txt"}, bytes.Repeat([]byte("z"), 2*JobFileChunkSize))
		e.exec(`UPDATE job_file_chunks SET chunk_index = chunk_index + 10 WHERE file_id = $1`, c.ID)
		e.exec(`UPDATE job_file_chunks SET chunk_index = CASE chunk_index WHEN 10 THEN 1 WHEN 11 THEN 0 END WHERE file_id = $1`, c.ID)
		if err := readAll(c.ID, u); !errors.Is(err, ErrJobFileIntegrity) {
			t.Fatalf("read after swapping chunk positions = %v, want ErrJobFileIntegrity", err)
		}
	})

	t.Run("file re-owned", func(t *testing.T) {
		d := e.put(t, ReserveParams{UserID: u, DisplayName: "d.txt"}, []byte("owned by u"))
		e.exec(`UPDATE job_files SET user_id = $2 WHERE id = $1`, d.ID, other)
		if err := readAll(d.ID, other); !errors.Is(err, ErrJobFileIntegrity) {
			t.Fatalf("read by the new owner = %v, want ErrJobFileIntegrity (the owner is bound into the AAD)", err)
		}
	})

	t.Run("length column altered", func(t *testing.T) {
		f := e.put(t, ReserveParams{UserID: u, DisplayName: "f.txt"}, []byte("0123456789"))
		e.exec(`UPDATE job_file_chunks SET length = 9 WHERE file_id = $1`, f.ID)
		if err := readAll(f.ID, u); !errors.Is(err, ErrJobFileIntegrity) {
			t.Fatalf("read with a forged length = %v, want ErrJobFileIntegrity", err)
		}
	})

	t.Run("dropped trailing chunk", func(t *testing.T) {
		g := e.put(t, ReserveParams{UserID: u, DisplayName: "g.txt"}, bytes.Repeat([]byte("g"), JobFileChunkSize+5))
		e.exec(`DELETE FROM job_file_chunks WHERE file_id = $1 AND chunk_index = 1`, g.ID)
		if err := readAll(g.ID, u); !errors.Is(err, ErrJobFileIntegrity) {
			t.Fatalf("read with a dropped chunk = %v, want ErrJobFileIntegrity", err)
		}
	})
}

// TestJobFilesSweepLiveDB: stale reservations are released, a finished job's attached files become
// available with the retention clock, expired files lose their bytes but keep their row, and an
// expired file stops counting toward the quotas.
func TestJobFilesSweepLiveDB(t *testing.T) {
	l := wide()
	l.RequestDeadline = time.Minute
	l.Retention = 24 * time.Hour
	l.PerOwnerBytes = 1000
	e := newJFEnv(t, l)
	u := e.seedUser(t)

	// Reservations: one past the stale cutoff (the max upload deadline plus a RequestDeadline), one
	// within it (older than 2x the request deadline: a slow live upload is not swept).
	stale, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "stale.txt", DeclaredSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "fresh.txt", DeclaredSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE job_files SET created_at = now() - make_interval(secs => $2) WHERE id = $1`, stale.ID, (l.StaleReservationCutoff() + time.Minute).Seconds())
	e.exec(`UPDATE job_files SET created_at = now() - interval '5 minutes' WHERE id = $1`, fresh.ID)

	// Outputs: one on a live run, one on a run that finished an hour ago.
	liveRun := e.jobFor(t, u)
	doneRun := e.seedRawJob(t, u, "completed", nil, 0, 600)
	e.exec(`UPDATE runs SET finished_at = now() - interval '1 hour' WHERE id = $1`, doneRun)
	liveOut := e.put(t, ReserveParams{UserID: u, RunID: &liveRun, Direction: JobFileOutput, DisplayName: "live.txt"}, []byte("live"))
	doneOut := e.put(t, ReserveParams{UserID: u, RunID: &doneRun, Direction: JobFileOutput, DisplayName: "done.txt"}, []byte("done"))

	// An unattached input already past its expiry.
	old := e.put(t, ReserveParams{UserID: u, DisplayName: "old.txt"}, []byte("expires"))
	e.exec(`UPDATE job_files SET expires_at = now() - interval '1 minute' WHERE id = $1`, old.ID)
	before := e.sums(t, u).OwnerJobBytes

	res, err := e.jf.Sweep(e.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ReleasedReservations != 1 || res.Settled != 1 || res.Expired != 1 {
		t.Fatalf("sweep = %+v, want 1 released, 1 settled, 1 expired", res)
	}
	if e.fileExists(t, stale.ID) || !e.fileExists(t, fresh.ID) {
		t.Fatal("only the stale reservation may be released")
	}
	if e.fileState(t, liveOut.ID) != JobFileAttached {
		t.Fatal("an output of a live run must stay attached")
	}
	var state string
	var expires time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT state, expires_at FROM job_files WHERE id = $1`, doneOut.ID).Scan(&state, &expires); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().Add(23 * time.Hour); state != JobFileAvailable || expires.Before(want.Add(-2*time.Minute)) || expires.After(want.Add(2*time.Minute)) {
		t.Fatalf("finished job's output = %s expiring %v, want available at finished_at + retention (~%v)", state, expires, want)
	}
	if e.fileState(t, old.ID) != JobFileExpired || e.chunkCount(t, old.ID) != 0 {
		t.Fatalf("expired file = %s with %d chunks, want an expired tombstone with no bytes", e.fileState(t, old.ID), e.chunkCount(t, old.ID))
	}
	if _, _, err := e.jf.Open(e.ctx, old.ID, u); !errors.Is(err, ErrJobFileExpired) {
		t.Fatalf("Open of an expired file = %v, want ErrJobFileExpired", err)
	}
	// stale (100) and old (7) stopped counting; a settled file still counts.
	if after := e.sums(t, u).OwnerJobBytes; after != before-100-int64(len("expires")) {
		t.Fatalf("owner bytes = %d, want %d after releasing the reservation and expiring the file", after, before-100-int64(len("expires")))
	}
	// A second pass is a no-op.
	if res, err := e.jf.Sweep(e.ctx); err != nil || res.Total() != 0 {
		t.Fatalf("second sweep = %+v, %v; want nothing to do", res, err)
	}
}

// TestJobFilesParallelReservationsAdmitExactlyAsManyAsFitLiveDB: N concurrent reservations that
// together exceed a quota admit exactly as many as fit, whatever the interleaving. Under READ
// COMMITTED without the stored-files lock each would sum against its own snapshot and all would
// pass.
//
// CALIBRATION: delete the store.LockStoredFiles call from JobFiles.Reserve; the admitted count
// then exceeds the quota and this test goes red.
func TestJobFilesParallelReservationsAdmitExactlyAsManyAsFitLiveDB(t *testing.T) {
	const size, fits, attempts = 100, 5, 24
	for name, limits := range map[string]func(JobFileLimits) JobFileLimits{
		"per-owner quota": func(l JobFileLimits) JobFileLimits { l.PerOwnerBytes = size * fits; return l },
		"instance quota":  func(l JobFileLimits) JobFileLimits { l.InstanceBytes = size * fits; return l },
		"shared budget":   func(l JobFileLimits) JobFileLimits { l.StoredFilesBudgetBytes = size * fits; return l },
	} {
		t.Run(name, func(t *testing.T) {
			e := newJFEnv(t, limits(wide()))
			u := e.seedUser(t)
			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				admitted int
				refused  int
			)
			start := make(chan struct{})
			for i := 0; i < attempts; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := e.jf.Reserve(context.Background(), ReserveParams{UserID: u, Direction: JobFileInput, DisplayName: "p.txt", DeclaredSize: size})
					mu.Lock()
					defer mu.Unlock()
					var r *JobFileRefusedError
					switch {
					case err == nil:
						admitted++
					case errors.As(err, &r) && r.Kind == RefusalQuota:
						refused++
					default:
						t.Errorf("unexpected error: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			if admitted != fits || refused != attempts-fits {
				t.Fatalf("admitted %d and refused %d of %d, want exactly %d admitted", admitted, refused, attempts, fits)
			}
			if got := e.sums(t, u).OwnerJobBytes; got != size*fits {
				t.Fatalf("reserved bytes = %d, want %d", got, size*fits)
			}
		})
	}
}
