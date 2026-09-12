// Package recovery implements the authenticated durable archive API for PRD #1296 M2
// (D2/D4/D6/D7). It is the server half of durable run recovery: a recovery-capable
// worker reserves a capture under its run's open custody hold, streams an encrypted Git
// bundle in ONE request that the API splits into AAD-sealed ~1 MiB chunks and commits
// atomically with the ready transition, and the strict run OWNER later lists metadata
// and streams the decrypted bytes back. The API holds UZI_SECRET_KEY and never releases
// unauthenticated plaintext; it stores opaque encrypted bytes with bounded parsing and
// never spawns git, checks out files or inflates an object graph (D4).
//
// M1 froze the store-query layer (internal/store/queries/recovery.sql) and the wire DTOs
// (internal/apitypes/recovery.go); this package consumes both without editing them.
package recovery

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Sentinel errors the handler maps to HTTP statuses. Each is a specific outcome, never
// a leaked internal detail.
var (
	// ErrNotAuthorized: the caller's worker identity does not own the capture's open
	// hold, or the hold is not open — a different/later worker cannot adopt authority.
	ErrNotAuthorized = errors.New("recovery: not authorized for this capture")
	// ErrCaptureNotFound: no capture matches the (id, run, owner) triple.
	ErrCaptureNotFound = errors.New("recovery: capture not found")
	// ErrNotAvailable: the capture exists but is not in a downloadable ('available') state.
	ErrNotAvailable = errors.New("recovery: capture is not available")
	// ErrOversize: the streamed body exceeded the max bundle ceiling; rejected, not truncated.
	ErrOversize = errors.New("recovery: bundle exceeds the maximum size")
	// ErrManifestConflict: a different byte manifest is already bound under this capture id.
	ErrManifestConflict = errors.New("recovery: a different manifest is already bound")
	// ErrIntegrity: the streamed bytes did not match the declared manifest (size/checksum/
	// chunk count), or a stored chunk failed its AAD check on download.
	ErrIntegrity = errors.New("recovery: archive integrity check failed")
	// ErrQuota: a byte or capture quota would be exceeded; the source is retained for retry.
	ErrQuota = errors.New("recovery: storage quota exceeded")
	// ErrBusy: the per-process concurrent upload/download limit is saturated.
	ErrBusy = errors.New("recovery: too many concurrent transfers")
	// ErrBadRequest: a malformed manifest or request field.
	ErrBadRequest = errors.New("recovery: invalid request")
	// ErrStreamAborted: a download failed AFTER the 200 response began, so the handler
	// must not write another response — it only logs. The bytes already sent are all
	// authenticated; the download is incomplete (the client detects the short read).
	ErrStreamAborted = errors.New("recovery: download aborted after streaming began")
)

// Store is the SUBSET of the M1 recovery.sql query layer this service calls outside a
// transaction. *store.Queries satisfies it; a fake satisfies it in unit tests. The
// transactional upload/reserve paths use the pgx pool directly (they need a row lock and
// a single bounded transaction, D4), so their queries are not on this interface.
type Store interface {
	GetCaptureForOwner(ctx context.Context, arg store.GetCaptureForOwnerParams) (store.RecoveryCapture, error)
	ListCapturesForRunOwner(ctx context.Context, arg store.ListCapturesForRunOwnerParams) ([]store.RecoveryCapture, error)
	GetRecoverySummaryForRun(ctx context.Context, arg store.GetRecoverySummaryForRunParams) (store.GetRecoverySummaryForRunRow, error)
	MarkCaptureState(ctx context.Context, arg store.MarkCaptureStateParams) (store.RecoveryCapture, error)
	DiscardCaptureForOwner(ctx context.Context, arg store.DiscardCaptureForOwnerParams) (int64, error)
}

// DB is the pgx pool surface the service needs for the streaming download cursor and the
// bounded upload/reserve transactions. *pgxpool.Pool satisfies it.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Limits are the operator-configurable archive bounds (PRD #1296 D4). The handler builds
// this from config.Config so this package need not import config.
type Limits struct {
	MaxBundleBytes         int64         // max complete bundle; an over-cap upload is rejected.
	ReadyPayloadPerOwner   int64         // ready payload byte quota per owner.
	InstanceBytes          int64         // instance-wide byte quota (scoped to the affected owner on breach).
	MaxCapturesPerClaim    int           // captures admitted under one hold.
	MaxCapturesPerOwner    int           // retained (non-discarded) captures per owner.
	ReadyRetention         time.Duration // ready-artifact TTL, begins at durable capture.
	MaxConcurrentUploads   int           // concurrent uploads per API process.
	MaxConcurrentDownloads int           // concurrent downloads per API process.
	RequestDeadline        time.Duration // per upload/download request+transaction deadline.
}

// Service is the durable archive service. It is safe for concurrent use.
type Service struct {
	store    Store
	pool     DB
	box      *secretbox.Box
	limits   Limits
	now      func() time.Time
	uploads  chan struct{}
	download chan struct{}
}

// New builds a Service. box may be nil only in tests that never touch bytes; the upload
// and download paths require it. now defaults to time.Now.
func New(st Store, pool DB, box *secretbox.Box, limits Limits, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	up := limits.MaxConcurrentUploads
	if up < 1 {
		up = 1
	}
	down := limits.MaxConcurrentDownloads
	if down < 1 {
		down = 1
	}
	if limits.RequestDeadline <= 0 {
		limits.RequestDeadline = 120 * time.Second
	}
	if limits.ReadyRetention <= 0 {
		limits.ReadyRetention = 7 * 24 * time.Hour
	}
	if limits.MaxBundleBytes <= 0 {
		limits.MaxBundleBytes = 64 << 20
	}
	return &Service{
		store:    st,
		pool:     pool,
		box:      box,
		limits:   limits,
		now:      now,
		uploads:  make(chan struct{}, up),
		download: make(chan struct{}, down),
	}
}

// acquire takes one slot from sem or fails when ctx is done (deadline/cancel). It never
// blocks unboundedly: the request deadline the caller applies bounds the wait.
func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrBusy
	}
}

// release returns one slot to sem. It must be paired with a successful acquire.
func release(sem chan struct{}) { <-sem }

// workerIdentity is the immutable, non-secret provenance label recorded on a capture,
// matching the value ClaimRun stamped on the hold (workersvc.workerIdentity, PRD #1296
// D1/D2): the worker's name, falling back to its id. Replicated here — not exported from
// workersvc — because this package must not edit or import that milestone's service; the
// value is provenance only (authorization is by the immutable original_worker_id UUID,
// and the chunk AAD binds capture_id, not this label).
func workerIdentity(wkr store.Worker) string {
	if strings.TrimSpace(wkr.Name) != "" {
		return wkr.Name
	}
	return wkr.ID.String()
}
