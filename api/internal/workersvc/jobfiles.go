package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobfiles.go is the job-file store of PRD #1909 M1: bounded, AAD-sealed, short-lived files kept
// in Postgres in the recovery-archive shape (job_files + job_file_chunks). The HTTP surfaces that
// upload and download (M2, M3, M4, M5) consume only the exported API below; nothing here knows
// about routes or auth. Every method that takes an owner scopes its SQL to that owner, so a file
// that is not the caller's reads exactly like a file that does not exist.
//
// Admission is atomic. Reserve takes store.LockStoredFiles (the owner key, then the shared key),
// sums the retained bytes, applies the caps and inserts the row in state 'reserved' at the DECLARED
// size, all in one short transaction. The chunk stream (Write) then runs in its own transaction
// OUTSIDE the lock, so a slow upload never blocks admission. Recovery-archive uploads admit under
// the same lock (recovery.Service.upload), which is what keeps the two stores inside one shared
// budget.

// JobFileChunkSize is the plaintext size of one sealed chunk (~1 MiB, the recovery archive size).
const JobFileChunkSize = 1 << 20

// jobFileHeadSize is how much of a file's start Inspector.Begin sees for type detection.
const jobFileHeadSize = 512

// Job file directions and states (the values job_files' CHECK constraints allow).
const (
	JobFileInput  = "input"
	JobFileOutput = "output"

	JobFileReserved   = "reserved"
	JobFileUnattached = "unattached"
	JobFileAttached   = "attached"
	JobFileAvailable  = "available"
	JobFileExpired    = "expired"
)

// Refusal reasons: the stable, bounded strings a refused upload reports (and that
// job_output_refusals.reason records). Callers map a *JobFileRefusedError to a status by Kind.
const (
	RefusalFileTooLarge   = "file_too_large"     // over the per-file cap
	RefusalTooManyFiles   = "too_many_files"     // over the per-job file count
	RefusalJobBytes       = "job_bytes_exceeded" // over the per-job byte total
	RefusalOwnerQuota     = "owner_quota"        // over the owner's retained job-file bytes
	RefusalInstanceQuota  = "instance_quota"     // over the instance's retained job-file bytes
	RefusalStoredBudget   = "stored_budget"      // over the shared job-file + recovery budget
	RefusalEmptyFile      = "empty_file"         // a zero-byte file is never stored
	RefusalUnsupported    = "unsupported_type"   // not on the type allowlist
	RefusalSizeMismatch   = "size_mismatch"      // streamed size != declared size
	RefusalSHAMismatch    = "sha256_mismatch"    // streamed digest != declared digest
	RefusalContentInvalid = "content_invalid"    // the Inspector rejected the bytes
)

// RefusalKind classifies a refusal for the caller's status mapping: a Limit refusal is a cap on
// what one file or one job may be (an input answers 413), a Quota refusal is the owner's,
// instance's or shared budget being full (both an input upload and a worker output answer 507), an
// Invalid refusal is a malformed or mismatching upload (422, or 415 for an unsupported type).
type RefusalKind int

// The refusal kinds.
const (
	RefusalLimit RefusalKind = iota + 1
	RefusalQuota
	RefusalInvalid
)

// JobFileRefusedError is a refused reservation or upload. Reason is one of the Refusal* constants.
type JobFileRefusedError struct {
	Kind   RefusalKind
	Reason string
	// Detail is an optional fixed, server-authored sentence saying why an Inspector refused the
	// bytes. It is never built from client input.
	Detail string
}

func (e *JobFileRefusedError) Error() string { return "job file refused: " + e.Reason }

func refusal(kind RefusalKind, reason string) error {
	return &JobFileRefusedError{Kind: kind, Reason: reason}
}

// inspectorRefusal is refusal for an Inspector error: the reason it names (or def), and its
// RefusalDetailer text when it offers one.
func inspectorRefusal(err error, def string) error {
	ref := &JobFileRefusedError{Kind: RefusalInvalid, Reason: refusalReasonOf(err, def)}
	var d RefusalDetailer
	if errors.As(err, &d) {
		ref.Detail = d.RefusalDetail()
	}
	return ref
}

// JobFileBodyError is a failure reading the upload body (client disconnect, read deadline,
// malformed framing, an over-limit body), as opposed to a database or inspection failure. Unwrap
// exposes the cause, so errors.Is(err, os.ErrDeadlineExceeded) and errors.As(*http.MaxBytesError)
// see through it.
type JobFileBodyError struct{ Err error }

func (e *JobFileBodyError) Error() string { return "job file upload body: " + e.Err.Error() }
func (e *JobFileBodyError) Unwrap() error { return e.Err }

// Sentinel errors of the job-file store.
var (
	// ErrJobRunNotFound: Reserve's RunID is not a kind='job' run of the reserving owner (it does
	// not exist, belongs to someone else, or is not a job). It reads the same in every case.
	ErrJobRunNotFound = errors.New("workersvc: job run not found")
	// ErrJobFileNotFound: no file matches (id, owner), or it is still only a reservation.
	ErrJobFileNotFound = errors.New("workersvc: job file not found")
	// ErrJobFileExpired: the file existed but its bytes are gone.
	ErrJobFileExpired = errors.New("workersvc: job file expired")
	// ErrJobFileIntegrity: a stored chunk failed its AAD check, or the inventory does not match the
	// row. No unauthenticated byte is ever returned.
	ErrJobFileIntegrity = errors.New("workersvc: job file integrity check failed")
	// ErrJobFileInvalid: a malformed reservation request (bad direction, name or digest).
	ErrJobFileInvalid = errors.New("workersvc: invalid job file request")
	// ErrJobFileUploadsBusy: AcquireWrite found no free upload slot (process-wide or the owner's).
	ErrJobFileUploadsBusy = errors.New("workersvc: too many concurrent job file uploads")
	// ErrJobFilesUnavailable: the store has no encryption key or no database wired.
	ErrJobFilesUnavailable = errors.New("workersvc: job files unavailable")
)

// JobFileLimits are the operator-configurable job-file bounds (PRD #1909 D1). The server builds
// this from config.Config in cmd/server/main.go.
type JobFileLimits struct {
	InputFileMaxBytes      int64         // one input file.
	InputsMaxFiles         int           // input files per job.
	InputsMaxBytes         int64         // total input bytes per job.
	OutputFileMaxBytes     int64         // one output file.
	OutputsMaxFiles        int           // output files per job.
	OutputsMaxBytes        int64         // total output bytes per job.
	PerOwnerBytes          int64         // retained job-file bytes per owner.
	InstanceBytes          int64         // retained job-file bytes, instance-wide.
	StoredFilesBudgetBytes int64         // job-file bytes + recovery-archive bytes, instance-wide.
	Retention              time.Duration // how long a finished job's files stay downloadable.
	UploadTTL              time.Duration // how long an unattached input lives.
	RequestDeadline        time.Duration // one upload/download request+transaction deadline.
	// MaxConcurrentWrites bounds the uploads streaming at once, per API process (AcquireWrite):
	// each holds one pooled database connection for its whole body read, so this is what keeps a
	// crowd of slow clients from taking the pool. MaxConcurrentWritesPerOwner is the share one
	// owner may hold, so one caller cannot take every slot.
	MaxConcurrentWrites         int
	MaxConcurrentWritesPerOwner int
}

// withDefaults fills a zero field with the PRD #1909 D1 default.
func (l JobFileLimits) withDefaults() JobFileLimits {
	fill64 := func(v *int64, def int64) {
		if *v <= 0 {
			*v = def
		}
	}
	fillInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	fill64(&l.InputFileMaxBytes, 25<<20)
	fillInt(&l.InputsMaxFiles, 10)
	fill64(&l.InputsMaxBytes, 50<<20)
	fill64(&l.OutputFileMaxBytes, 25<<20)
	fillInt(&l.OutputsMaxFiles, 50)
	fill64(&l.OutputsMaxBytes, 100<<20)
	fill64(&l.PerOwnerBytes, 256<<20)
	fill64(&l.InstanceBytes, 1<<30)
	fill64(&l.StoredFilesBudgetBytes, 4<<30)
	if l.Retention <= 0 {
		l.Retention = 7 * 24 * time.Hour
	}
	if l.UploadTTL <= 0 {
		l.UploadTTL = time.Hour
	}
	if l.RequestDeadline <= 0 {
		l.RequestDeadline = 120 * time.Second
	}
	fillInt(&l.MaxConcurrentWrites, 4)
	fillInt(&l.MaxConcurrentWritesPerOwner, 2)
	if l.MaxConcurrentWritesPerOwner > l.MaxConcurrentWrites {
		l.MaxConcurrentWritesPerOwner = l.MaxConcurrentWrites
	}
	return l
}

// UploadMinRateBytesPerSecond is the slowest sustained upload rate the request deadline assumes
// (100 KiB/s). The worker's per-attempt upload timeout (agent/src/job-outputs.ts,
// UPLOAD_MIN_RATE_BPS) uses the same rate with a 300 s floor, so the api never cuts a body off
// before the worker gives up on it.
const UploadMinRateBytesPerSecond = 100 << 10

// MaxUploadDeadline is the absolute ceiling of one upload's wall time, whatever the declared size
// or the per-file cap: a raised per-file cap cannot stretch a stalled or trickling upload past it
// (256 MiB at 100 KiB/s would be about 44 minutes). An operator-set RequestDeadline above it is
// honoured as the floor instead (see UploadDeadline).
const MaxUploadDeadline = 10 * time.Minute

// UploadDeadline is the wall time one upload of declared bytes may take to arrive:
// max(RequestDeadline, declared / UploadMinRateBytesPerSecond), where declared is bounded by
// fileMax (the per-file cap of the direction), and the result never exceeds
// max(RequestDeadline, MaxUploadDeadline). At the default 25 MiB cap that is 256 s; a small file
// keeps the plain RequestDeadline. A non-positive declared size counts as zero.
func (l JobFileLimits) UploadDeadline(declared, fileMax int64) time.Duration {
	d := l.requestDeadlineOrDefault()
	declared = min(max(declared, 0), max(fileMax, 0))
	scaled := time.Duration((declared+UploadMinRateBytesPerSecond-1)/UploadMinRateBytesPerSecond) * time.Second
	return min(max(d, scaled), l.maxUploadDeadline())
}

func (l JobFileLimits) requestDeadlineOrDefault() time.Duration {
	if l.RequestDeadline <= 0 {
		return 120 * time.Second
	}
	return l.RequestDeadline
}

// maxUploadDeadline is the largest value UploadDeadline can return.
func (l JobFileLimits) maxUploadDeadline() time.Duration {
	return max(l.requestDeadlineOrDefault(), MaxUploadDeadline)
}

// StaleReservationCutoff is how old a 'reserved' row must be before the sweep releases it: the
// largest possible UploadDeadline plus one RequestDeadline of margin (the commit transaction and
// clock skew), so a live upload, however slow, is never swept from under its own write.
func (l JobFileLimits) StaleReservationCutoff() time.Duration {
	return l.maxUploadDeadline() + l.requestDeadlineOrDefault()
}

// ClampWriteSlots bounds the upload write slots by the database pool size. Each streaming upload
// holds one pooled connection for its whole body read, so the process-wide slots may take at most
// half the pool (at least 1), leaving the rest for auth and every other query; the per-owner share
// is at most half that bound (at least 1), so one owner cannot take every slot. Zero limit fields take their defaults first, and a caller's
// explicit smaller value is kept. A non-positive poolMax is treated as 1.
func ClampWriteSlots(poolMax int32, l JobFileLimits) JobFileLimits {
	l = l.withDefaults()
	bound := int(poolMax / 2)
	if bound < 1 {
		bound = 1
	}
	if l.MaxConcurrentWrites > bound {
		l.MaxConcurrentWrites = bound
	}
	ownerBound := bound / 2
	if ownerBound < 1 {
		ownerBound = 1
	}
	if l.MaxConcurrentWritesPerOwner > ownerBound {
		l.MaxConcurrentWritesPerOwner = ownerBound
	}
	if l.MaxConcurrentWritesPerOwner > l.MaxConcurrentWrites {
		l.MaxConcurrentWritesPerOwner = l.MaxConcurrentWrites
	}
	return l
}

// JobFilesDB is the pgx pool surface the store needs: queries plus the ability to begin the
// bounded transactions. *pgxpool.Pool satisfies it.
type JobFilesDB interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// JobFiles is the job-file store. It is safe for concurrent use.
type JobFiles struct {
	db     JobFilesDB
	box    *secretbox.Box
	limits JobFileLimits
	now    func() time.Time

	// writeMu guards writeHeld, the per-owner count of held write slots; writeSlots is the
	// process-wide semaphore (its capacity is limits.MaxConcurrentWrites).
	writeMu    sync.Mutex
	writeHeld  map[uuid.UUID]int
	writeSlots chan struct{}
}

// NewJobFiles builds the store. box is required by Write and Open (a nil box makes them return
// ErrJobFilesUnavailable); now defaults to time.Now.
func NewJobFiles(db JobFilesDB, box *secretbox.Box, limits JobFileLimits, now func() time.Time) *JobFiles {
	if now == nil {
		now = time.Now
	}
	l := limits.withDefaults()
	return &JobFiles{
		db: db, box: box, limits: l, now: now,
		writeHeld:  map[uuid.UUID]int{},
		writeSlots: make(chan struct{}, l.MaxConcurrentWrites),
	}
}

// AcquireWrite takes one upload slot for owner without waiting: it fails with
// ErrJobFileUploadsBusy when the process-wide slots are all taken or the owner already holds its
// share. The caller takes it BEFORE Reserve, so a refused request holds no reservation and no
// connection, and calls the returned release when the upload is finished (it is idempotent).
// Write streams the body while holding a pooled connection, so the slot count is the bound on how
// many connections uploads can hold.
func (j *JobFiles) AcquireWrite(owner uuid.UUID) (release func(), err error) {
	j.writeMu.Lock()
	defer j.writeMu.Unlock()
	if j.writeHeld[owner] >= j.limits.MaxConcurrentWritesPerOwner {
		return nil, ErrJobFileUploadsBusy
	}
	select {
	case j.writeSlots <- struct{}{}:
	default:
		return nil, ErrJobFileUploadsBusy
	}
	j.writeHeld[owner]++
	var once sync.Once
	return func() {
		once.Do(func() {
			j.writeMu.Lock()
			defer j.writeMu.Unlock()
			if j.writeHeld[owner]--; j.writeHeld[owner] <= 0 {
				delete(j.writeHeld, owner)
			}
			<-j.writeSlots
		})
	}, nil
}

// Limits returns the effective limits (defaults filled), for callers that size a request body.
func (j *JobFiles) Limits() JobFileLimits { return j.limits }

// SetJobFiles wires the job-file store onto the service, for the upload/download surfaces that
// reach it through JobFiles(). Call once at construction, before the service serves requests.
func (s *Service) SetJobFiles(jf *JobFiles) { s.jobFiles = jf }

// JobFiles returns the wired job-file store, or nil when none is wired (tests, and deployments
// that never call SetJobFiles): callers answer "unavailable" rather than dereference it.
func (s *Service) JobFiles() *JobFiles { return s.jobFiles }

// jobFileChunkAAD is the additional authenticated data bound into a job-file chunk's tag. It is
// domain-separated ("job_file_chunk|", distinct from the recovery archive's "recovery_chunk|")
// and binds the file id, the owner, the chunk index and the plaintext length, so a sealed chunk
// cannot be replayed into another file, another owner's file, another position or a padded
// length. The value is deterministic ASCII.
func jobFileChunkAAD(fileID, owner uuid.UUID, index, length int) []byte {
	return []byte("job_file_chunk|" + fileID.String() + "|" + owner.String() + "|" +
		strconv.Itoa(index) + "|" + strconv.Itoa(length))
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// jobFileExt maps an allowlisted content type to its on-disk extension: the storage name is
// `<sha256>.<ext>`, so the extension comes from the content and never from the uploader.
var jobFileExt = map[string]string{
	"application/pdf":  "pdf",
	"image/png":        "png",
	"image/jpeg":       "jpg",
	"text/plain":       "txt",
	"text/markdown":    "md",
	"text/csv":         "csv",
	"application/json": "json",
	"text/html":        "html",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       "xlsx",
}

// JobFileExt returns the storage extension for an allowlisted content type.
func JobFileExt(contentType string) (string, bool) {
	e, ok := jobFileExt[contentType]
	return e, ok
}

// ReserveParams describe one upload to admit.
type ReserveParams struct {
	UserID          uuid.UUID
	ProductID       *uuid.UUID // the uzp_ product a file was uploaded through, nil for a user token.
	RunID           *uuid.UUID // set for an output (and for an input already bound to a run).
	Direction       string     // JobFileInput or JobFileOutput.
	ClaimGeneration *int64     // the uploading claim generation of an output.
	DisplayName     string     // already sanitised: non-empty, at most 255 bytes, no path or control characters.
	DeclaredSize    int64      // the reservation is EXACTLY this.
	DeclaredSHA256  string     // optional lowercase hex digest, verified against the stream.
}

func (p ReserveParams) validate() error {
	if p.Direction != JobFileInput && p.Direction != JobFileOutput {
		return ErrJobFileInvalid
	}
	if p.Direction == JobFileOutput && p.RunID == nil {
		return ErrJobFileInvalid
	}
	n := len(p.DisplayName)
	if n == 0 || n > 255 || !utf8.ValidString(p.DisplayName) || strings.ContainsAny(p.DisplayName, `/\`) {
		return ErrJobFileInvalid
	}
	for _, r := range p.DisplayName {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ErrJobFileInvalid
		}
		// Format (Cf: bidi overrides and isolates, zero-width and joiner characters, the BOM),
		// line separator (Zl) and paragraph separator (Zp) characters let a name display as
		// something it is not (a right-to-left override spoofing an extension) or break a line.
		// The job_files and job_output_refusals display_name CHECKs (migration 00276) mirror the
		// ranges that matter; this is the complete test.
		if unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ErrJobFileInvalid
		}
	}
	if p.DeclaredSHA256 != "" && !sha256HexRe.MatchString(p.DeclaredSHA256) {
		return ErrJobFileInvalid
	}
	return nil
}

// Reserve admits one upload: in ONE short transaction it takes the stored-files lock, applies the
// per-file, per-job, per-owner, instance and shared-budget caps against the retained bytes (every
// state except 'expired' counts) and inserts the row in state 'reserved' at the DECLARED size. It
// never reserves more than the declared size. The refusal is a *JobFileRefusedError. The caller
// must then Write the bytes, or Release the reservation; the sweep is the backstop for a caller
// that does neither.
func (j *JobFiles) Reserve(ctx context.Context, p ReserveParams) (store.JobFile, error) {
	if err := p.validate(); err != nil {
		return store.JobFile{}, err
	}
	if p.DeclaredSize <= 0 {
		return store.JobFile{}, refusal(RefusalInvalid, RefusalEmptyFile)
	}
	fileMax, maxFiles, maxBytes := j.limits.InputFileMaxBytes, j.limits.InputsMaxFiles, j.limits.InputsMaxBytes
	if p.Direction == JobFileOutput {
		fileMax, maxFiles, maxBytes = j.limits.OutputFileMaxBytes, j.limits.OutputsMaxFiles, j.limits.OutputsMaxBytes
	}
	if p.DeclaredSize > fileMax {
		return store.JobFile{}, refusal(RefusalLimit, RefusalFileTooLarge)
	}

	if p.RunID != nil {
		// The run must be a job of THIS owner: SumRunJobFiles counts across owners, so without this
		// a caller could reserve against (and be charged into the caps of) another owner's job. It
		// is a plain read taken BEFORE the advisory keys, never a row lock inside them: a runs row
		// held FOR UPDATE elsewhere would otherwise stall every stored-files admission
		// instance-wide. runs.user_id and runs.kind are immutable. A run deleted between this read
		// and the insert below is not prevented: the job_files.run_id foreign key rejects the insert,
		// and that violation is mapped to ErrJobRunNotFound.
		if _, err := store.New(j.db).GetOwnedJobRun(ctx, store.GetOwnedJobRunParams{ID: *p.RunID, UserID: p.UserID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.JobFile{}, ErrJobRunNotFound
			}
			return store.JobFile{}, err
		}
	}

	tx, err := j.db.Begin(ctx)
	if err != nil {
		return store.JobFile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockStoredFiles(ctx, tx, p.UserID); err != nil {
		return store.JobFile{}, err
	}
	q := store.New(tx)

	// The two server-generated outputs (IsReservedJobOutputName) are not the job's own files: they
	// neither count against nor are refused by the per-job file and byte caps (SumRunJobFiles leaves
	// them out of the sums too). The owner, instance and shared-budget quotas below still apply.
	if p.RunID != nil && (p.Direction != JobFileOutput || !IsReservedJobOutputName(p.DisplayName)) {
		cur, err := q.SumRunJobFiles(ctx, store.SumRunJobFilesParams{RunID: pgconv.UUID(*p.RunID), Direction: p.Direction})
		if err != nil {
			return store.JobFile{}, err
		}
		if cur.FileCount >= int64(maxFiles) {
			return store.JobFile{}, refusal(RefusalLimit, RefusalTooManyFiles)
		}
		if cur.TotalBytes+p.DeclaredSize > maxBytes {
			return store.JobFile{}, refusal(RefusalLimit, RefusalJobBytes)
		}
	}
	sums, err := store.SumStoredFiles(ctx, q, p.UserID, uuid.Nil)
	if err != nil {
		return store.JobFile{}, err
	}
	switch {
	case sums.OwnerJobBytes+p.DeclaredSize > j.limits.PerOwnerBytes:
		return store.JobFile{}, refusal(RefusalQuota, RefusalOwnerQuota)
	case sums.InstanceJobBytes+p.DeclaredSize > j.limits.InstanceBytes:
		return store.JobFile{}, refusal(RefusalQuota, RefusalInstanceQuota)
	case sums.SharedBytes()+p.DeclaredSize > j.limits.StoredFilesBudgetBytes:
		return store.JobFile{}, refusal(RefusalQuota, RefusalStoredBudget)
	}

	row, err := q.ReserveJobFile(ctx, store.ReserveJobFileParams{
		UserID:          p.UserID,
		ProductID:       pgconv.UUIDPtr(p.ProductID),
		RunID:           pgconv.UUIDPtr(p.RunID),
		Direction:       p.Direction,
		ClaimGeneration: int8Ptr(p.ClaimGeneration),
		DisplayName:     p.DisplayName,
		ByteSize:        p.DeclaredSize,
		Sha256:          pgconv.TextOrNull(p.DeclaredSHA256),
	})
	if err != nil {
		if isRunFKViolation(err) {
			return store.JobFile{}, ErrJobRunNotFound
		}
		if isOutputDuplicateViolation(err) {
			return store.JobFile{}, errJobOutputDuplicate
		}
		return store.JobFile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.JobFile{}, err
	}
	return row, nil
}

// errJobOutputDuplicate is Reserve's answer when an output of the same run, claim generation, name
// and content is already stored or still being stored (the uq_job_files_output_content index): the
// caller looks the stored file up (StoreJobOutput, storeGeneratedOutput) instead of storing twice.
var errJobOutputDuplicate = errors.New("workersvc: output already stored")

// isOutputDuplicateViolation reports a unique violation (SQLSTATE 23505) on the output duplicate
// index.
func isOutputDuplicateViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_job_files_output_content"
}

// isRunFKViolation reports a foreign-key violation (SQLSTATE 23503) on job_files.run_id: the run
// was deleted between Reserve's ownership read and its insert.
func isRunFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "job_files_run_id_fkey"
}

func int8Ptr(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

// Inspector lets the caller (the upload surfaces of M2 and M4) classify and validate the bytes
// while they stream, without buffering the file. Write calls Begin once with the first bytes,
// Chunk for every plaintext chunk in order (the first included), and End after the last.
type Inspector interface {
	// Begin receives up to the first 512 bytes and returns the detected content type, which must
	// be on the allowlist. An error refuses the upload.
	Begin(head []byte) (contentType string, err error)
	// Chunk sees each plaintext chunk once, in order. The slice is only valid during the call.
	Chunk(p []byte) error
	// End runs after the last chunk, before the file is committed.
	End() error
}

// WriteOptions parameterise Write. Exactly one of Inspector or ContentType decides the type.
type WriteOptions struct {
	// ContentType is the caller-decided allowlisted type, used when Inspector is nil.
	ContentType string
	Inspector   Inspector
	// Deadline bounds the whole Write (the body reads and the transaction); zero means
	// Limits().RequestDeadline. The upload routes pass Limits().UploadDeadline(declared, cap), so
	// the store's bound is the same one they set on the connection's read deadline.
	Deadline time.Duration
}

// Write streams body into AAD-sealed chunks and commits them with the file's resting state in ONE
// transaction that runs outside the stored-files lock. body must yield exactly the DECLARED size:
// more, fewer, or a digest that does not match the declared one refuses the upload, and any failure
// releases the reservation in the same call (the sweep is the backstop when that release fails).
// An unattached input rests 'unattached' with the upload TTL; an output, or an input already
// bound to a run, rests 'attached' with no expiry until its run ends.
//
// The first chunk is read from body BEFORE the transaction begins, so a client that sends nothing
// holds no database connection; a later chunk is read inside it. Neither read observes ctx (a body
// read blocks in the network), so the caller must bound the body with a read deadline on the
// request (V1FileUpload does) and cap concurrent writes with AcquireWrite, which is what limits the
// connections a slow client can hold. A body read failure is returned as a *JobFileBodyError.
func (j *JobFiles) Write(ctx context.Context, id, owner uuid.UUID, body io.Reader, opt WriteOptions) (file store.JobFile, err error) {
	if j.box == nil {
		return store.JobFile{}, ErrJobFilesUnavailable
	}
	writeDeadline := opt.Deadline
	if writeDeadline <= 0 {
		writeDeadline = j.limits.RequestDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, writeDeadline)
	defer cancel()
	defer func() {
		if err != nil {
			j.releaseBestEffort(ctx, id, owner)
		}
	}()

	// Read the first chunk BEFORE taking a connection: a client that stalls before it sends data
	// never holds one. A later chunk is read inside the transaction; the caller bounds that with
	// AcquireWrite and a read deadline on the request.
	buf := make([]byte, JobFileChunkSize)
	n, readErr := io.ReadFull(body, buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return store.JobFile{}, &JobFileBodyError{Err: readErr}
	}

	tx, err := j.db.Begin(ctx)
	if err != nil {
		return store.JobFile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	row, err := q.LockReservedJobFile(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, ErrJobFileNotFound
		}
		return store.JobFile{}, err
	}
	if row.UserID != owner {
		return store.JobFile{}, ErrJobFileNotFound
	}
	declared := row.ByteSize

	contentType := opt.ContentType
	hasher := sha256.New()
	// One byte past the declared size is enough to tell an overlong body from an exact one; the
	// first chunk (already read) counts against it.
	src := io.LimitReader(body, declared+1-int64(n))
	var total int64
	index := 0
	for {
		if n > 0 {
			plain := buf[:n]
			if index == 0 && opt.Inspector != nil {
				head := plain
				if len(head) > jobFileHeadSize {
					head = head[:jobFileHeadSize]
				}
				ct, ierr := opt.Inspector.Begin(head)
				if ierr != nil {
					return store.JobFile{}, inspectorRefusal(ierr, RefusalUnsupported)
				}
				contentType = ct
			}
			if opt.Inspector != nil {
				if ierr := opt.Inspector.Chunk(plain); ierr != nil {
					return store.JobFile{}, inspectorRefusal(ierr, RefusalContentInvalid)
				}
			}
			total += int64(n)
			if total > declared {
				return store.JobFile{}, refusal(RefusalInvalid, RefusalSizeMismatch)
			}
			_, _ = hasher.Write(plain)
			sealed, serr := j.box.SealWithAAD(plain, jobFileChunkAAD(id, owner, index, n))
			if serr != nil {
				return store.JobFile{}, serr
			}
			if ierr := q.InsertJobFileChunk(ctx, store.InsertJobFileChunkParams{
				FileID: id, ChunkIndex: int32(index), Length: int32(n), Sealed: sealed, //nolint:gosec // G115: index is bounded by declared/JobFileChunkSize, n <= JobFileChunkSize
			}); ierr != nil {
				return store.JobFile{}, ierr
			}
			index++
		}
		if readErr == nil {
			n, readErr = io.ReadFull(src, buf)
			continue
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		return store.JobFile{}, &JobFileBodyError{Err: readErr}
	}
	if total != declared {
		return store.JobFile{}, refusal(RefusalInvalid, RefusalSizeMismatch)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if row.Sha256.Valid && row.Sha256.String != sum {
		return store.JobFile{}, refusal(RefusalInvalid, RefusalSHAMismatch)
	}
	if opt.Inspector != nil {
		if ierr := opt.Inspector.End(); ierr != nil {
			return store.JobFile{}, inspectorRefusal(ierr, RefusalContentInvalid)
		}
	}
	ext, ok := JobFileExt(contentType)
	if !ok {
		return store.JobFile{}, refusal(RefusalInvalid, RefusalUnsupported)
	}

	state, expires := JobFileAttached, pgtype.Timestamptz{}
	if row.Direction == JobFileInput && !row.RunID.Valid {
		state, expires = JobFileUnattached, pgconv.Time(j.now().Add(j.limits.UploadTTL))
	}
	out, err := q.FinalizeJobFile(ctx, store.FinalizeJobFileParams{
		State:       state,
		ByteSize:    total,
		Sha256:      pgconv.Text(sum),
		StorageName: pgconv.Text(sum + "." + ext),
		ContentType: pgconv.Text(contentType),
		ChunkCount:  int32(index),
		ExpiresAt:   expires,
		ID:          id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, ErrJobFileNotFound
		}
		return store.JobFile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.JobFile{}, err
	}
	return out, nil
}

// RefusalReasoner lets an Inspector name the refusal reason it wants recorded; an Inspector error
// that does not implement it is reported under the default reason.
type RefusalReasoner interface{ RefusalReason() string }

// RefusalDetailer lets an Inspector attach a fixed, server-authored sentence to its refusal; it
// must never contain client input.
type RefusalDetailer interface{ RefusalDetail() string }

func refusalReasonOf(err error, def string) string {
	var r RefusalReasoner
	if errors.As(err, &r) && r.RefusalReason() != "" {
		return r.RefusalReason()
	}
	return def
}

// releaseBestEffort deletes a still-reserved row after a failed Write. It runs on a fresh short
// context so the request's own cancelled or expired context does not prevent it; a failure is
// logged and left to the sweep.
func (j *JobFiles) releaseBestEffort(ctx context.Context, id, owner uuid.UUID) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := store.New(j.db).ReleaseJobFileReservation(rctx, store.ReleaseJobFileReservationParams{ID: id, UserID: owner}); err != nil {
		slog.Warn("job files: releasing a failed reservation; the sweep will", "file_id", id, "error", err)
	}
}

// Release deletes an owner's still-reserved file, giving its reservation back. A committed file is
// never touched; the result reports whether a reservation was released.
func (j *JobFiles) Release(ctx context.Context, id, owner uuid.UUID) (bool, error) {
	n, err := store.New(j.db).ReleaseJobFileReservation(ctx, store.ReleaseJobFileReservationParams{ID: id, UserID: owner})
	return n > 0, err
}

// Get returns the owner's file row (any state, including a tombstone).
func (j *JobFiles) Get(ctx context.Context, id, owner uuid.UUID) (store.JobFile, error) {
	row, err := store.New(j.db).GetJobFileForOwner(ctx, store.GetJobFileForOwnerParams{ID: id, UserID: owner})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, ErrJobFileNotFound
		}
		return store.JobFile{}, err
	}
	return row, nil
}

// Open returns the owner's file metadata and a reader over its decrypted bytes. The reader pulls
// one ~1 MiB chunk at a time (never the whole file), and Open itself decrypts the FIRST chunk, so
// an AAD failure surfaces as ErrJobFileIntegrity before the caller writes a response header. A
// later chunk failure, a length or digest mismatch surfaces from Read as ErrJobFileIntegrity in
// place of io.EOF: the bytes already returned are all authenticated, and the stream is incomplete.
// A reserved file reads as not found and an expired one as ErrJobFileExpired. The reader is only
// valid while ctx is.
func (j *JobFiles) Open(ctx context.Context, id, owner uuid.UUID) (store.JobFile, io.Reader, error) {
	if j.box == nil {
		return store.JobFile{}, nil, ErrJobFilesUnavailable
	}
	row, err := j.Get(ctx, id, owner)
	if err != nil {
		return store.JobFile{}, nil, err
	}
	switch row.State {
	case JobFileReserved:
		return store.JobFile{}, nil, ErrJobFileNotFound
	case JobFileExpired:
		return store.JobFile{}, nil, ErrJobFileExpired
	}
	r := &jobFileReader{ctx: ctx, j: j, file: row, hasher: sha256.New()}
	if row.ChunkCount > 0 {
		if err := r.next(); err != nil {
			return store.JobFile{}, nil, err
		}
	}
	return row, r, nil
}

// jobFileReader streams a file's decrypted chunks in order.
type jobFileReader struct {
	ctx    context.Context
	j      *JobFiles
	file   store.JobFile
	idx    int
	cur    []byte
	total  int64
	hasher hash.Hash
	done   bool
}

// next decrypts the chunk at r.idx into r.cur.
func (r *jobFileReader) next() error {
	c, err := store.New(r.j.db).GetJobFileChunk(r.ctx, store.GetJobFileChunkParams{FileID: r.file.ID, ChunkIndex: int32(r.idx)}) //nolint:gosec // G115: idx < chunk_count, an int32 column
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrJobFileIntegrity // a gap, or the chunks were reclaimed under us.
		}
		return err
	}
	plain, derr := r.j.box.OpenWithAAD(c.Sealed, jobFileChunkAAD(r.file.ID, r.file.UserID, r.idx, int(c.Length)))
	if derr != nil || len(plain) != int(c.Length) {
		return ErrJobFileIntegrity
	}
	r.cur = plain
	r.idx++
	return nil
}

// Read implements io.Reader.
func (r *jobFileReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	for len(r.cur) == 0 {
		if r.idx >= int(r.file.ChunkCount) {
			r.done = true
			if r.total != r.file.ByteSize || !r.file.Sha256.Valid ||
				hex.EncodeToString(r.hasher.Sum(nil)) != r.file.Sha256.String {
				return 0, ErrJobFileIntegrity
			}
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.done = true
			return 0, err
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	r.total += int64(n)
	_, _ = r.hasher.Write(p[:n])
	return n, nil
}

// JobFilesSweepResult counts what one Sweep pass did.
type JobFilesSweepResult struct {
	ReleasedReservations int64 // stale 'reserved' rows deleted.
	Settled              int64 // attached files of terminal jobs moved to 'available'.
	Expired              int64 // unattached/available files past expires_at tombstoned, chunks deleted.
}

// Total is the number of rows the pass changed.
func (r JobFilesSweepResult) Total() int64 { return r.ReleasedReservations + r.Settled + r.Expired }

// Sweep is the job_files_sweep pass: it releases reservations older than
// StaleReservationCutoff, the largest possible upload deadline plus one request deadline (an upload
// that died without releasing), moves attached files of terminal jobs to
// 'available' with their retention clock starting at the job's end, and expires unattached and
// available files past their expiry, deleting the chunks and keeping the row. All three run in one
// transaction under the stored-files lock, so the pass serializes with admission and with the
// recovery reclaim instead of deadlocking on rows. It is bounded by the sweeper's own tick and
// touches only rows its predicates select; a failure leaves everything for the next tick.
func (j *JobFiles) Sweep(ctx context.Context) (JobFilesSweepResult, error) {
	tx, err := j.db.Begin(ctx)
	if err != nil {
		return JobFilesSweepResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockStoredFiles(ctx, tx, uuid.Nil); err != nil {
		return JobFilesSweepResult{}, err
	}
	q := store.New(tx)
	now := j.now()
	var res JobFilesSweepResult
	if res.ReleasedReservations, err = q.ReleaseStaleJobFileReservations(ctx, pgconv.Time(now.Add(-j.limits.StaleReservationCutoff()))); err != nil {
		return JobFilesSweepResult{}, fmt.Errorf("release stale job file reservations: %w", err)
	}
	if res.Settled, err = q.SettleTerminalJobFiles(ctx, j.limits.Retention.Seconds()); err != nil {
		return JobFilesSweepResult{}, fmt.Errorf("settle terminal job files: %w", err)
	}
	if res.Expired, err = q.ExpireJobFiles(ctx, pgconv.Time(now)); err != nil {
		return JobFilesSweepResult{}, fmt.Errorf("expire job files: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobFilesSweepResult{}, err
	}
	return res, nil
}

// SweepPass adapts Sweep to the sweeper.Pass signature (a count of changed rows).
func (j *JobFiles) SweepPass(ctx context.Context) (int64, error) {
	res, err := j.Sweep(ctx)
	if err != nil {
		return 0, err
	}
	return res.Total(), nil
}
