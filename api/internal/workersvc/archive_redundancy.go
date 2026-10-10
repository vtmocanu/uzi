package workersvc

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git object ids are SHA-1; this verifies them, it is not a security hash
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2625: a completed run's guarded recovery archive may expire before its retention window,
// but only when the api itself proves, through the forge, that everything the archive retains is
// already reachable from the run's published branch. The worker supplies the archive's inventory
// (roots, the captured head, the tree, and the raw bytes of the synthetic aggregate commits);
// nothing it says about publication is trusted. The flow is:
//
//  1. every database and local check (binding, digest, aggregate rebuild, strict commit parsing),
//  2. the forge proof under one deadline, holding no database lock,
//  3. one transaction that locks worker, run, hold and capture, re-verifies, and expires the
//     capture with its recorded proof.
//
// Any failure keeps the archive on its own retention window.
const (
	// RedundancyRequestMaxBytes bounds the encoded request body: 8 aggregate objects, the current
	// object and the witness at 64 KiB each, base64-inflated, plus the roots.
	RedundancyRequestMaxBytes = 2 << 20

	redundancyProofDeadline   = 30 * time.Second
	redundancyWriteDeadline   = 15 * time.Second
	redundancyCooldown        = time.Hour
	redundancyMaxHeads        = 64
	redundancyMaxObjects      = 8
	redundancyMaxObjectBytes  = 64 << 10
	redundancyMaxParents      = 32
	redundancyMaxPrerequisite = 64
)

// ErrInvalidRedundancyRequest is a malformed redundancy claim: a field that is not a lowercase
// 40-hex id, roots that are not strictly sorted and unique, or a missing member. The handler
// answers 400; every other outcome is a 200 with a bounded reason.
var ErrInvalidRedundancyRequest = errors.New("invalid recovery redundancy request")

type archiveRedundancyStore interface {
	GetArchiveRedundancyView(context.Context, store.GetArchiveRedundancyViewParams) (store.GetArchiveRedundancyViewRow, error)
	GetCompletedPublicationBinding(context.Context, store.GetCompletedPublicationBindingParams) (store.GetCompletedPublicationBindingRow, error)
	RecordArchiveRedundancyRefusal(context.Context, store.RecordArchiveRedundancyRefusalParams) (int64, error)
}

// archiveRedundancyTxStore is the locked mutation phase's slice of the store.
type archiveRedundancyTxStore interface {
	GetWorkerForUpdate(context.Context, uuid.UUID) (store.Worker, error)
	GetRunOwnedByWorkerForUpdate(context.Context, store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error)
	GetFinalInventoryHold(context.Context, store.GetFinalInventoryHoldParams) (store.RecoveryCustodyHold, error)
	GetFinalInventoryCapture(context.Context, store.GetFinalInventoryCaptureParams) (store.RecoveryCapture, error)
	GetArchiveRedundancyView(context.Context, store.GetArchiveRedundancyViewParams) (store.GetArchiveRedundancyViewRow, error)
	LockCompletedPublicationBinding(context.Context, store.LockCompletedPublicationBindingParams) (store.LockCompletedPublicationBindingRow, error)
	ExpireRedundantCapture(context.Context, store.ExpireRedundantCaptureParams) ([]uuid.UUID, error)
}

type archiveRedundancyProver struct {
	q      archiveRedundancyStore
	forges ForgeBuilder
	begin  TxBeginner
	txq    func(pgx.Tx) archiveRedundancyTxStore
	now    func() time.Time
}

func (s *Service) archiveRedundancyProver() *archiveRedundancyProver {
	q, ok := s.q.(archiveRedundancyStore)
	if !ok {
		return nil
	}
	return &archiveRedundancyProver{
		q: q, forges: s.forges, begin: s.txBeginner,
		txq: func(tx pgx.Tx) archiveRedundancyTxStore { return store.New(tx) },
		now: s.now,
	}
}

// ProveArchiveRedundancy answers one worker claim that the completed run's archive captureID is
// redundant with the published branch (issue #2625). It returns ErrInvalidRedundancyRequest for a
// malformed claim, a database error for an infrastructure failure, and otherwise a response whose
// Outcome is expired only after the proof is recorded and the bytes are gone.
func (s *Service) ProveArchiveRedundancy(ctx context.Context, w store.Worker, runID, captureID uuid.UUID, req apitypes.RecoveryArchiveRedundancyRequest) (apitypes.RecoveryArchiveRedundancyResponse, error) {
	p := s.archiveRedundancyProver()
	if p == nil {
		return retainedRedundancy(captureID, apitypes.RecoveryRedundancyAncestryUnknown), nil
	}
	return p.prove(ctx, w, runID, captureID, req)
}

func retainedRedundancy(captureID uuid.UUID, reason string) apitypes.RecoveryArchiveRedundancyResponse {
	return apitypes.RecoveryArchiveRedundancyResponse{CaptureID: captureID.String(), Outcome: apitypes.RecoveryRedundancyRetained, Reason: reason}
}

func expiredRedundancy(captureID uuid.UUID) apitypes.RecoveryArchiveRedundancyResponse {
	return apitypes.RecoveryArchiveRedundancyResponse{CaptureID: captureID.String(), Outcome: apitypes.RecoveryRedundancyExpired}
}

// redundancyInputs is the verified, decoded claim.
type redundancyInputs struct {
	roots   []string // the claim's sorted unique roots
	heads   []string // sorted unique roots plus current_sha
	current redundancyCommit
	witness *redundancyCommit
}

func (p *archiveRedundancyProver) prove(ctx context.Context, w store.Worker, runID, captureID uuid.UUID, req apitypes.RecoveryArchiveRedundancyRequest) (apitypes.RecoveryArchiveRedundancyResponse, error) {
	if !validRedundancyShape(req) {
		return apitypes.RecoveryArchiveRedundancyResponse{}, ErrInvalidRedundancyRequest
	}
	retain := func(reason string) apitypes.RecoveryArchiveRedundancyResponse {
		slog.Info("archive redundancy retained", "run", runID, "capture", captureID, "reason", reason)
		return retainedRedundancy(captureID, reason)
	}
	if reason := redundancyCaps(req); reason != "" {
		return retain(reason), nil
	}

	view, err := p.q.GetArchiveRedundancyView(ctx, store.GetArchiveRedundancyViewParams{
		CaptureID: captureID, RunID: runID, UserID: w.UserID, WorkerID: w.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return retain(apitypes.RecoveryRedundancyBindingMismatch), nil
	}
	if err != nil {
		return apitypes.RecoveryArchiveRedundancyResponse{}, err
	}
	id, replay, reason := redundancyBinding(view, w, runID, req)
	if replay {
		return expiredRedundancy(captureID), nil
	}
	if reason != "" {
		return retain(reason), nil
	}
	if len(view.PrerequisiteShas) > redundancyMaxPrerequisite {
		return retain(apitypes.RecoveryRedundancyInventoryTooLarge), nil
	}
	for _, sha := range view.PrerequisiteShas {
		if !isSettleSHA(sha) {
			return retain(apitypes.RecoveryRedundancyBadObject), nil
		}
	}

	in, reason := verifyRedundancyInputs(req, view.SourceSha)
	if reason != "" {
		return retain(reason), nil
	}

	// The memo is a database read, so it precedes the first forge call.
	if view.RedundancyRefusedAt.Valid && view.RedundancyRefusal.Valid &&
		p.timeNow().Sub(view.RedundancyRefusedAt.Time) < redundancyCooldown {
		return retain(apitypes.RecoveryRedundancyCoolingDown), nil
	}

	// Past this point every refusal is forge evidence (or its absence) and is remembered.
	refuse := func(reason string) apitypes.RecoveryArchiveRedundancyResponse {
		memoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redundancyWriteDeadline)
		defer cancel()
		if _, err := p.q.RecordArchiveRedundancyRefusal(memoCtx, store.RecordArchiveRedundancyRefusalParams{
			ID: captureID, RunID: runID, UserID: w.UserID, CoverageDigest: req.CoverageDigest, Reason: reason,
		}); err != nil {
			slog.Warn("archive redundancy refusal not recorded", "capture", captureID, "error", err)
		}
		return retain(reason)
	}
	if p.forges == nil {
		return refuse(apitypes.RecoveryRedundancyAncestryUnknown), nil
	}
	proofCtx, cancel := context.WithTimeout(ctx, redundancyProofDeadline)
	defer cancel()
	repoID, perr := uuid.Parse(id.RepoID)
	if perr != nil {
		return refuse(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	binding, err := p.q.GetCompletedPublicationBinding(proofCtx, store.GetCompletedPublicationBindingParams{RepoID: repoID, UserID: w.UserID})
	if err != nil {
		return refuse(completedPublicationError(proofCtx, err)), nil
	}
	if binding.ConnectionID.String() != id.ConnectionID || binding.ProjectID != id.ProjectID ||
		binding.ForgeType != id.ForgeType || binding.BaseUrl != id.BaseURL {
		return refuse(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	f, err := p.forges.ForgeForConnection(binding.ForgeType, binding.BaseUrl, binding.TokenCiphertext)
	if err != nil {
		return refuse(apitypes.RecoveryRedundancyAncestryUnknown), nil
	}
	head, reason := proveCompletedPublication(proofCtx, f, id)
	if reason != "" {
		return refuse(reason), nil
	}
	if reason := proveRedundantCoverage(proofCtx, f, id.ProjectID, head, in, req.CurrentSha, view.PrerequisiteShas); reason != "" {
		return refuse(reason), nil
	}

	return p.expire(ctx, w, runID, captureID, req, view, id, head)
}

func (p *archiveRedundancyProver) timeNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// expire is the locked mutation phase. It re-verifies the binding under the worker, run, hold and
// capture locks and counts the flipped rows: success is exactly one row, the requested capture.
func (p *archiveRedundancyProver) expire(ctx context.Context, w store.Worker, runID, captureID uuid.UUID, req apitypes.RecoveryArchiveRedundancyRequest,
	view store.GetArchiveRedundancyViewRow, id apitypes.CompletionIdentity, head string) (apitypes.RecoveryArchiveRedundancyResponse, error) {
	if p.begin == nil {
		return retainedRedundancy(captureID, apitypes.RecoveryRedundancyAncestryUnknown), nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redundancyWriteDeadline)
	defer cancel()
	retain := func(reason string) apitypes.RecoveryArchiveRedundancyResponse {
		slog.Info("archive redundancy retained", "run", runID, "capture", captureID, "reason", reason)
		return retainedRedundancy(captureID, reason)
	}
	tx, err := p.begin.Begin(writeCtx)
	if err != nil {
		return apitypes.RecoveryArchiveRedundancyResponse{}, err
	}
	defer func() { _ = tx.Rollback(writeCtx) }()
	qt := p.txq(tx)
	if _, err = qt.GetWorkerForUpdate(writeCtx, w.ID); err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	if _, err = qt.GetRunOwnedByWorkerForUpdate(writeCtx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(w.ID)}); err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	hold, err := qt.GetFinalInventoryHold(writeCtx, store.GetFinalInventoryHoldParams{
		RunID: runID, UserID: w.UserID, WorkerID: w.ID, Generation: req.Generation,
	})
	if err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	if _, err = qt.GetFinalInventoryCapture(writeCtx, store.GetFinalInventoryCaptureParams{
		ID: captureID, HoldID: hold.ID, RunID: runID, UserID: w.UserID, WorkerID: w.ID,
	}); err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	locked, err := qt.GetArchiveRedundancyView(writeCtx, store.GetArchiveRedundancyViewParams{
		CaptureID: captureID, RunID: runID, UserID: w.UserID, WorkerID: w.ID,
	})
	if err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	_, replay, reason := redundancyBinding(locked, w, runID, req)
	if replay {
		return expiredRedundancy(captureID), nil
	}
	if reason != "" {
		return retain(reason), nil
	}
	// The proof was made against the identity read before the forge calls; any change since is
	// a different claim, not this one.
	if !bytes.Equal(locked.CompletionIdentity, view.CompletionIdentity) ||
		locked.SourceSha != view.SourceSha || !slices.Equal(locked.PrerequisiteShas, view.PrerequisiteShas) {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	repoID, err := uuid.Parse(id.RepoID)
	if err != nil {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	current, err := qt.LockCompletedPublicationBinding(writeCtx, store.LockCompletedPublicationBindingParams{RepoID: repoID, UserID: w.UserID})
	if err != nil || current.ConnectionID.String() != id.ConnectionID || current.ProjectID != id.ProjectID ||
		current.ForgeType != id.ForgeType || current.BaseUrl != id.BaseURL {
		return retain(apitypes.RecoveryRedundancyIdentityChanged), nil
	}
	proof, err := json.Marshal(redundancyProof{
		HoldID: id.HoldID, Generation: id.Generation, CoverageDigest: req.CoverageDigest, SourceSha: view.SourceSha,
		CurrentSha: req.CurrentSha, Tree: req.Tree, MRIID: *id.MRIID, FinalHead: id.FinalHead,
		AnchorHead: head, ProvedAt: p.timeNow().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return apitypes.RecoveryArchiveRedundancyResponse{}, err
	}
	ids, err := qt.ExpireRedundantCapture(writeCtx, store.ExpireRedundantCaptureParams{
		Proof: proof, ID: captureID, HoldID: hold.ID, RunID: runID, UserID: w.UserID, WorkerID: w.ID,
		CoverageDigest: req.CoverageDigest, SourceSha: view.SourceSha, Generation: req.Generation,
		Identity: view.CompletionIdentity,
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55000" {
		// The capture guard refused the proof; nothing was deleted (the statement rolled back).
		return retain(apitypes.RecoveryRedundancyNotExpired), nil
	}
	if err != nil {
		return apitypes.RecoveryArchiveRedundancyResponse{}, err
	}
	if len(ids) != 1 || ids[0] != captureID {
		// Zero rows covers a WHERE mismatch, a lost race and the guard's silent drop of an
		// identity-tuple change. Only exactly the requested capture counts as expired.
		return retain(apitypes.RecoveryRedundancyNotExpired), nil
	}
	if err = tx.Commit(writeCtx); err != nil {
		return apitypes.RecoveryArchiveRedundancyResponse{}, err
	}
	slog.Info("archive expired as published-redundant", "run", runID, "capture", captureID, "anchor_head", head)
	return expiredRedundancy(captureID), nil
}

// redundancyProof is the evidence stored on the capture. The capture guard checks every field
// except proved_at against the capture, its released hold and the completed run.
type redundancyProof struct {
	HoldID         string `json:"hold_id"`
	Generation     int64  `json:"generation"`
	CoverageDigest string `json:"coverage_digest"`
	SourceSha      string `json:"source_sha"`
	CurrentSha     string `json:"current_sha"`
	Tree           string `json:"tree"`
	MRIID          int64  `json:"mr_iid"`
	FinalHead      string `json:"final_head"`
	AnchorHead     string `json:"anchor_head"`
	ProvedAt       string `json:"proved_at"`
}

func validRedundancyShape(req apitypes.RecoveryArchiveRedundancyRequest) bool {
	if req.Generation <= 0 || len(req.CoverageDigest) != 64 || !isLowerHex(req.CoverageDigest) ||
		!isSettleSHA(req.CurrentSha) || !isSettleSHA(req.Tree) || len(req.Roots) == 0 ||
		len(req.AggregateObjects) == 0 || req.CurrentObject == "" {
		return false
	}
	for i, r := range req.Roots {
		if !isSettleSHA(r) || (i > 0 && req.Roots[i-1] >= r) {
			return false
		}
	}
	return req.WIP == nil || req.WIP.WitnessObject != ""
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func redundancyHeads(req apitypes.RecoveryArchiveRedundancyRequest) []string {
	heads := slices.Clone(req.Roots)
	if !slices.Contains(heads, req.CurrentSha) {
		heads = append(heads, req.CurrentSha)
	}
	slices.Sort(heads)
	return heads
}

func redundancyCaps(req apitypes.RecoveryArchiveRedundancyRequest) string {
	if len(redundancyHeads(req)) > redundancyMaxHeads || len(req.AggregateObjects) > redundancyMaxObjects {
		return apitypes.RecoveryRedundancyInventoryTooLarge
	}
	encodedMax := base64.StdEncoding.EncodedLen(redundancyMaxObjectBytes)
	objs := slices.Clone(req.AggregateObjects)
	objs = append(objs, req.CurrentObject)
	if req.WIP != nil {
		objs = append(objs, req.WIP.WitnessObject)
	}
	for _, o := range objs {
		if len(o) > encodedMax {
			return apitypes.RecoveryRedundancyInventoryTooLarge
		}
	}
	return ""
}

// redundancyBinding checks the claim against the capture, its hold and the run, and returns the
// frozen completion identity. replay is true when this exact claim already expired the capture.
func redundancyBinding(v store.GetArchiveRedundancyViewRow, w store.Worker, runID uuid.UUID, req apitypes.RecoveryArchiveRedundancyRequest) (id apitypes.CompletionIdentity, replay bool, reason string) {
	digestMatches := v.CoverageDigest.Valid && v.CoverageDigest.String == req.CoverageDigest
	if v.CaptureState == "expired" && v.CaptureReason.Valid && v.CaptureReason.String == "published_redundant" &&
		len(v.RedundancyProof) > 0 && digestMatches && v.HoldGeneration == req.Generation {
		return id, true, ""
	}
	switch {
	case v.CaptureState != "available" || !v.ManifestBound:
		return id, false, apitypes.RecoveryRedundancyNotAvailable
	case !digestMatches:
		return id, false, apitypes.RecoveryRedundancyDigestMismatch
	case v.HoldGeneration != req.Generation || v.HoldWorkerID != w.ID || !v.InventoryGuarded ||
		v.RunID != runID || v.UserID != w.UserID:
		return id, false, apitypes.RecoveryRedundancyBindingMismatch
	case v.HoldState == "open" || v.RunStatus != "completed":
		return id, false, apitypes.RecoveryRedundancyNotCompleted
	case v.HoldState != "released":
		return id, false, apitypes.RecoveryRedundancyBindingMismatch
	case len(v.CompletionIdentity) == 0:
		return id, false, apitypes.RecoveryRedundancyIdentityMissing
	}
	if json.Unmarshal(v.CompletionIdentity, &id) != nil || id.HoldID != v.HoldID.String() || id.RunID != runID.String() ||
		id.OwnerID != w.UserID.String() || id.WorkerID != w.ID.String() || id.Generation != req.Generation ||
		!isSettleSHA(id.FinalHead) || !v.RepoID.Valid || id.RepoID != uuid.UUID(v.RepoID.Bytes).String() ||
		id.ConnectionID == "" || id.ProjectID <= 0 || id.ForgeType == "" || id.BaseURL == "" {
		return id, false, apitypes.RecoveryRedundancyIdentityChanged
	}
	if id.MRIID == nil || *id.MRIID <= 0 {
		return id, false, apitypes.RecoveryRedundancyMRMissing
	}
	if v.RunGeneration != v.HoldGeneration || !v.RunWorkerID.Valid || uuid.UUID(v.RunWorkerID.Bytes) != w.ID ||
		!v.CompletionFinalHead.Valid || v.CompletionFinalHead.String != id.FinalHead {
		return id, false, apitypes.RecoveryRedundancyBindingMismatch
	}
	if !v.RunMrIid.Valid || v.RunMrIid.Int64 != *id.MRIID {
		return id, false, apitypes.RecoveryRedundancyIdentityChanged
	}
	return id, false, ""
}

// redundancyDigest is sha256 over the exact bytes of JS JSON.stringify({roots, currentSha, tree}).
// Every member is lowercase hex, so no escaping is possible.
func redundancyDigest(roots []string, currentSha, tree string) string {
	var b strings.Builder
	b.WriteString(`{"roots":[`)
	for i, r := range roots {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(r)
		b.WriteByte('"')
	}
	b.WriteString(`],"currentSha":"`)
	b.WriteString(currentSha)
	b.WriteString(`","tree":"`)
	b.WriteString(tree)
	b.WriteString(`"}`)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

type redundancyCommit struct {
	sha     string
	tree    string
	parents []string
	message string
}

// gitObjectID is the SHA-1 of a loose commit object holding body.
func gitObjectID(body []byte) string {
	h := sha1.New() //nolint:gosec // git object id, see import
	_, _ = fmt.Fprintf(h, "commit %d\x00", len(body))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// parseStrictCommit parses a raw commit body the way the proof needs it and no looser: the first
// line is exactly `tree <40 lowercase hex>`, followed by contiguous `parent <40 lowercase hex>`
// lines with no duplicate, then other headers (a line starting with a space continues the previous
// header). A later tree or parent header, a NUL byte, uppercase hex, a missing header/message
// separator or an empty header line is refused, so what git would ignore cannot be smuggled past
// the checks here.
func parseStrictCommit(body []byte) (redundancyCommit, bool) {
	var c redundancyCommit
	if bytes.IndexByte(body, 0) >= 0 {
		return c, false
	}
	sep := bytes.Index(body, []byte("\n\n"))
	if sep < 0 {
		return c, false
	}
	lines := strings.Split(string(body[:sep]), "\n")
	if !strings.HasPrefix(lines[0], "tree ") || !isSettleSHA(lines[0][len("tree "):]) {
		return c, false
	}
	c.tree = lines[0][len("tree "):]
	i := 1
	for ; i < len(lines) && strings.HasPrefix(lines[i], "parent "); i++ {
		p := lines[i][len("parent "):]
		if !isSettleSHA(p) || slices.Contains(c.parents, p) {
			return c, false
		}
		c.parents = append(c.parents, p)
	}
	first := i
	for ; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			return c, false
		}
		if l[0] == ' ' {
			if i == first {
				return c, false
			}
			continue
		}
		key, _, _ := strings.Cut(l, " ")
		if key == "tree" || key == "parent" {
			return c, false
		}
	}
	c.message = string(body[sep+2:])
	c.sha = gitObjectID(body)
	return c, true
}

var redundancyMessage = regexp.MustCompile(`^uzi recovery coverage ([0-9a-f]{64}) round (0|[1-9][0-9]*) batch (0|[1-9][0-9]*)\n$`)

func decodeRedundancyObject(s string) (body []byte, reason string) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, apitypes.RecoveryRedundancyBadObject
	}
	body, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, apitypes.RecoveryRedundancyBadObject
	}
	if len(body) > redundancyMaxObjectBytes {
		return nil, apitypes.RecoveryRedundancyInventoryTooLarge
	}
	return body, ""
}

// verifyRedundancyInputs runs every local check of the claim: the digest, the aggregate rebuilt
// from its raw commits down to the archived source_sha, and the tree of the captured head.
func verifyRedundancyInputs(req apitypes.RecoveryArchiveRedundancyRequest, sourceSha string) (redundancyInputs, string) {
	var in redundancyInputs
	if redundancyDigest(req.Roots, req.CurrentSha, req.Tree) != req.CoverageDigest {
		return in, apitypes.RecoveryRedundancyDigestMismatch
	}
	in.roots = req.Roots
	in.heads = redundancyHeads(req)

	objects := make(map[string]redundancyCommit, len(req.AggregateObjects))
	for _, enc := range req.AggregateObjects {
		body, reason := decodeRedundancyObject(enc)
		if reason != "" {
			return in, reason
		}
		c, ok := parseStrictCommit(body)
		if !ok {
			return in, apitypes.RecoveryRedundancyBadObject
		}
		if _, dup := objects[c.sha]; dup {
			return in, apitypes.RecoveryRedundancyBadObject
		}
		objects[c.sha] = c
	}
	if _, ok := objects[sourceSha]; !ok {
		return in, apitypes.RecoveryRedundancySourceMismatch
	}
	var leaves []string
	visited := map[string]bool{}
	var walk func(sha string) string
	walk = func(sha string) string {
		c := objects[sha]
		if visited[sha] {
			return apitypes.RecoveryRedundancyBadObject
		}
		visited[sha] = true
		m := redundancyMessage.FindStringSubmatch(c.message)
		if m == nil || m[1] != req.CoverageDigest || len(c.parents) == 0 || len(c.parents) > redundancyMaxParents {
			return apitypes.RecoveryRedundancyBadObject
		}
		if c.tree != req.Tree {
			return apitypes.RecoveryRedundancyTreeMismatch
		}
		for _, parent := range c.parents {
			if _, internal := objects[parent]; internal {
				if reason := walk(parent); reason != "" {
					return reason
				}
				continue
			}
			leaves = append(leaves, parent)
		}
		return ""
	}
	if reason := walk(sourceSha); reason != "" {
		return in, reason
	}
	if len(visited) != len(objects) {
		return in, apitypes.RecoveryRedundancyBadObject
	}
	slices.Sort(leaves)
	if !slices.Equal(leaves, in.heads) {
		return in, apitypes.RecoveryRedundancySourceMismatch
	}

	// The captured head's own tree must be the tree the aggregate carries: the aggregate is only
	// as good as the tree it stands for, so a mismatch here is refused before any forge call.
	body, reason := decodeRedundancyObject(req.CurrentObject)
	if reason != "" {
		return in, reason
	}
	cur, ok := parseStrictCommit(body)
	if !ok || cur.sha != req.CurrentSha {
		return in, apitypes.RecoveryRedundancyBadObject
	}
	if cur.tree != req.Tree {
		return in, apitypes.RecoveryRedundancyTreeMismatch
	}
	in.current = cur
	if req.WIP != nil {
		body, reason := decodeRedundancyObject(req.WIP.WitnessObject)
		if reason != "" {
			return in, reason
		}
		wit, ok := parseStrictCommit(body)
		if !ok {
			return in, apitypes.RecoveryRedundancyBadObject
		}
		if wit.tree != req.Tree {
			return in, apitypes.RecoveryRedundancyTreeMismatch
		}
		in.witness = &wit
	}
	return in, ""
}

// proveRedundantCoverage asks the forge, once per distinct commit, whether it is an ancestor of
// head. Only an error-free Ancestor answer is proof; a NotAncestor answer names the class that
// failed, and an error, timeout or inconclusive answer is never read as coverage.
func proveRedundantCoverage(ctx context.Context, f forge.Forge, projectID int64, head string, in redundancyInputs, currentSha string, prerequisites []string) string {
	cache := map[string]forge.Ancestry{}
	ask := func(sha string) (forge.Ancestry, string) {
		if sha == head {
			return forge.AncestryAncestor, ""
		}
		if a, ok := cache[sha]; ok {
			return a, ""
		}
		a, err := f.CompareAncestry(ctx, projectID, head, sha)
		if err != nil {
			return forge.AncestryUnknown, completedPublicationError(ctx, err)
		}
		if a != forge.AncestryAncestor && a != forge.AncestryNotAncestor {
			return forge.AncestryUnknown, apitypes.RecoveryRedundancyAncestryUnknown
		}
		cache[sha] = a
		return a, ""
	}
	// covered reports "" when sha is an ancestor of head and notAncestor when the forge said it is not.
	covered := func(sha, notAncestor string) string {
		a, reason := ask(sha)
		switch {
		case reason != "":
			return reason
		case a == forge.AncestryAncestor:
			return ""
		default:
			return notAncestor
		}
	}
	// A current_sha that is also a root is an owed commit, never parked work: it takes the root rule.
	currentIsRoot := slices.Contains(in.roots, currentSha)
	for _, sha := range in.heads {
		if sha == currentSha && !currentIsRoot {
			continue
		}
		if reason := covered(sha, apitypes.RecoveryRedundancyUncoveredRoot); reason != "" {
			return reason
		}
	}
	// The captured head is covered when published; otherwise it is parked work-in-progress, whose
	// parents and a witness commit carrying the same tree must be published.
	a, reason := ask(currentSha)
	if reason != "" {
		return reason
	}
	if a != forge.AncestryAncestor {
		if in.witness == nil {
			return apitypes.RecoveryRedundancyUncoveredWIP
		}
		for _, sha := range append(slices.Clone(in.current.parents), in.witness.sha) {
			if reason := covered(sha, apitypes.RecoveryRedundancyUncoveredWIP); reason != "" {
				return reason
			}
		}
	}
	for _, sha := range prerequisites {
		if reason := covered(sha, apitypes.RecoveryRedundancyUncoveredPrereq); reason != "" {
			return reason
		}
	}
	if ctx.Err() != nil {
		return completedPublicationError(ctx, ctx.Err())
	}
	return ""
}
