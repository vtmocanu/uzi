package pushbroker

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/uuid"
)

// PrereqRefPrefix contains capture-scoped prerequisite pins. CreateRef cannot write here.
const PrereqRefPrefix = "refs/uzi-prereq/"

func PrereqRef(captureID uuid.UUID) string { return PrereqRefPrefix + captureID.String() }

func validPrereqRef(ref string) bool {
	if !validRefUnder(ref, PrereqRefPrefix) {
		return false
	}
	id, err := uuid.Parse(ref[len(PrereqRefPrefix):])
	return err == nil && id != uuid.Nil && PrereqRef(id) == ref
}

// PrereqCreateOptions is entirely server-derived. DefaultBranch is a short branch name.
type PrereqCreateOptions struct {
	CloneURL      string
	Username      string
	PAT           string
	DefaultBranch string
	CaptureID     uuid.UUID
}

// PreparedPrereqCreate is opaque, read-only preparation evidence and a single-use send capability.
// Value copies share the private consumed flag; copying cannot authorize another send.
type PreparedPrereqCreate struct {
	o    PrereqCreateOptions
	ref  string
	tip  plumbing.Hash
	sent *atomic.Bool
}

func (p *PreparedPrereqCreate) Ref() string { return p.ref }
func (p *PreparedPrereqCreate) Tip() string { return p.tip.String() }

type PrereqCreateOutcome uint8

const (
	// Unknown means bounded readback did not establish the expected or a foreign tip.
	PrereqCreateUnknown PrereqCreateOutcome = iota
	// Created means bounded readback observed the prepared tip, even after a lost or ng report.
	PrereqCreateCreated
	// Refused requires explicit ng and confirmed absence, never just a remote error.
	PrereqCreateRefused
	// ForeignTip means readback proved a different tip; it must never be adopted.
	PrereqCreateForeignTip
)

var ErrPrereqCreateRefused = errors.New("pushbroker: prerequisite create refused")
var ErrPrereqCreateUnknown = errors.New("pushbroker: prerequisite create outcome unknown")

// PreparePrereqCreate observes default D and target absence in ONE bounded advertisement.
// It sends nothing. A preexisting target is an error even when it already points at D.
func PreparePrereqCreate(ctx context.Context, o PrereqCreateOptions) (*PreparedPrereqCreate, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ref := PrereqRef(o.CaptureID)
	source := "refs/heads/" + o.DefaultBranch
	if !validPrereqRef(ref) || !validRefUnder(source, "refs/heads/") {
		return nil, ErrInvalidRef
	}
	tips, err := boundedRefTips(ctx, o.CloneURL, authFor(Options{Username: o.Username, PAT: o.PAT}),
		map[plumbing.ReferenceName]bool{plumbing.ReferenceName(ref): true, plumbing.ReferenceName(source): true})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := tips[ref]; ok {
		return nil, ErrRefExists
	}
	tip := tips[source]
	if !isObjectID(tip) || plumbing.NewHash(tip).IsZero() {
		return nil, ErrSourceMissing
	}
	return &PreparedPrereqCreate{o: o, ref: ref, tip: plumbing.NewHash(tip), sent: new(atomic.Bool)}, nil
}

// SendPrereqCreate never resends. A bounded readback at the prepared tip establishes
// Created regardless of the command report, matching S3 and S4 reconciliation.
func SendPrereqCreate(ctx context.Context, p *PreparedPrereqCreate) (PrereqCreateOutcome, error) {
	if p == nil || !validPrereqRef(p.ref) || p.tip.IsZero() || p.sent == nil || !p.sent.CompareAndSwap(false, true) {
		return PrereqCreateUnknown, ErrInvalidRef
	}
	ctx, cancel := context.WithTimeout(ctx, maxCreateRefDuration)
	defer cancel()
	auth := authFor(Options{Username: p.o.Username, PAT: p.o.PAT})
	raw := &rawReport{ref: p.ref}
	sendCtx, sendCancel := context.WithTimeout(ctx, time.Second)
	defer sendCancel()
	sess, ar, err := openPrereqReceive(sendCtx, p.o.CloneURL, auth, raw)
	if err != nil {
		return PrereqCreateUnknown, err
	}
	res, pushErr := receiveObservedPack(sendCtx, sess, createRefRequest(ar.Capabilities, plumbing.ReferenceName(p.ref), p.tip, emptyPack()), raw)
	pushErr = errors.Join(pushErr, sess.Close())
	sendCancel()
	readCtx, readCancel := context.WithTimeout(ctx, time.Second)
	defer readCancel()
	tips, listErr := boundedRefTips(readCtx, p.o.CloneURL, auth, map[plumbing.ReferenceName]bool{plumbing.ReferenceName(p.ref): true})
	if listErr != nil {
		return PrereqCreateUnknown, errors.Join(ErrPrereqCreateUnknown, pushErr, listErr)
	}
	if err := ctx.Err(); err != nil {
		return PrereqCreateUnknown, errors.Join(ErrPrereqCreateUnknown, pushErr, err)
	}
	current, present := tips[p.ref]
	switch {
	case present && current != p.tip.String():
		return PrereqCreateForeignTip, ErrRefExists
	case present:
		return PrereqCreateCreated, nil
	case !present && res.ng:
		return PrereqCreateRefused, ErrPrereqCreateRefused
	default:
		return PrereqCreateUnknown, errors.Join(ErrPrereqCreateUnknown, pushErr)
	}
}

// openPrereqReceive bounds discovery before parsing, retaining report observation and
// the broker's authentication and redirect policy. The caller must close the session.
func openPrereqReceive(ctx context.Context, url string, auth transport.AuthMethod, raw *rawReport) (transport.ReceivePackSession, *packp.AdvRefs, error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, nil, err
	}
	var c transport.Transport
	isHTTP := ep.Protocol == "http" || ep.Protocol == "https"
	if isHTTP {
		copied := *brokerHTTPClient
		copied.CheckRedirect = func(req *http.Request, via []*http.Request) error { return checkRedirect(ctx, req, via) }
		copied.Transport = &rawReportRoundTripper{original: refAdvertisementTransport{original: brokerHTTPClient.Transport}, report: raw}
		c = githttp.NewClient(&copied)
	} else {
		c, err = transportFor(ep)
		if err != nil {
			return nil, nil, err
		}
	}
	sess, err := c.NewReceivePackSession(ep, auth)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (transport.ReceivePackSession, *packp.AdvRefs, error) {
		return nil, nil, errors.Join(err, sess.Close())
	}
	if !isHTTP {
		if err := boundRefStdout(sess); err != nil {
			return fail(err)
		}
	}
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return fail(err)
	}
	if !isHTTP {
		if err := attachRawStdout(sess, raw); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return sess, ar, nil
}

// deletePrereq requires exact nonzero persisted ownership evidence. Every advertisement,
// including post-command readback, is bounded. A changed tip is preserved; no retry occurs.
func deletePrereq(ctx context.Context, o DeleteOptions) error {
	if !isObjectID(o.ExpectedOldTip) || plumbing.NewHash(o.ExpectedOldTip).IsZero() {
		return ErrInvalidRef
	}
	auth := authFor(Options{Username: o.Username, PAT: o.PAT})
	want := map[plumbing.ReferenceName]bool{plumbing.ReferenceName(o.Ref): true}
	listCtx, listCancel := context.WithTimeout(ctx, time.Second)
	tips, err := boundedRefTips(listCtx, o.CloneURL, auth, want)
	listCancel()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tips[o.Ref] != o.ExpectedOldTip {
		return nil
	}
	raw := &rawReport{ref: o.Ref}
	receiveCtx, receiveCancel := context.WithTimeout(ctx, time.Second)
	sess, ar, err := openPrereqReceive(receiveCtx, o.CloneURL, auth, raw)
	receiveCancel()
	if err != nil {
		return err
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: plumbing.ReferenceName(o.Ref), Old: plumbing.NewHash(o.ExpectedOldTip), New: plumbing.ZeroHash}}
	sendCtx, sendCancel := context.WithTimeout(ctx, time.Second)
	_, pushErr := receiveObservedPack(sendCtx, sess, req, raw)
	sendCancel()
	pushErr = errors.Join(pushErr, sess.Close())
	readCtx, readCancel := context.WithTimeout(ctx, time.Second)
	tips, err = boundedRefTips(readCtx, o.CloneURL, auth, want)
	readCancel()
	if err != nil {
		return errors.Join(pushErr, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(pushErr, err)
	}
	if tips[o.Ref] != o.ExpectedOldTip {
		return nil
	}
	return errors.Join(errors.New("pushbroker: prerequisite delete not confirmed"), pushErr)
}
