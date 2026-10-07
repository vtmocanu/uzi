package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// dispositionTransport delegates real list/fetch operations to a test-owned bare
// repository, intercepting only the session boundaries under test.
type dispositionTransport struct {
	transport.Transport
	stage      string
	report     *packp.ReportStatus
	receiveErr error
	cancel     context.CancelFunc
	invoked    int
}

var errDispositionFailure = errors.New("fixture transport failure")

func (d *dispositionTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	copyEP := *ep
	copyEP.Protocol = "file"
	s, err := d.Transport.NewUploadPackSession(&copyEP, auth)
	if err != nil {
		return nil, err
	}
	return &dispositionUpload{UploadPackSession: s, d: d}, nil
}

type dispositionUpload struct {
	transport.UploadPackSession
	d *dispositionTransport
}

func (s *dispositionUpload) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if s.d.stage == "list" {
		return nil, errDispositionFailure
	}
	return s.UploadPackSession.AdvertisedReferencesContext(ctx)
}
func (s *dispositionUpload) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if s.d.stage == "fetch" {
		return nil, errDispositionFailure
	}
	return s.UploadPackSession.UploadPack(ctx, req)
}
func (d *dispositionTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	if d.stage == "session" {
		return nil, errDispositionFailure
	}
	copyEP := *ep
	copyEP.Protocol = "file"
	s, err := d.Transport.NewReceivePackSession(&copyEP, auth)
	if err != nil {
		return nil, err
	}
	return &dispositionReceive{ReceivePackSession: s, d: d}, nil
}

type dispositionReceive struct {
	transport.ReceivePackSession
	d      *dispositionTransport
	Stdout io.Reader
}

func (s *dispositionReceive) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if s.d.stage == "advertisement" {
		return nil, errDispositionFailure
	}
	ar, err := s.ReceivePackSession.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	if s.d.stage == "advance" {
		s.Stdout = reflect.ValueOf(s.ReceivePackSession).Elem().FieldByName("Stdout").Interface().(io.Reader)
	} else {
		ar.Capabilities.Delete(capability.Sideband)
		ar.Capabilities.Delete(capability.Sideband64k)
		var wire bytes.Buffer
		if s.d.report != nil {
			if err := s.d.report.Encode(&wire); err != nil {
				return nil, err
			}
		}
		s.Stdout = bytes.NewReader(wire.Bytes())
	}
	if s.d.stage == "cancel before" {
		s.d.cancel()
	}
	return ar, err
}
func (s *dispositionReceive) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	s.d.invoked++
	if s.d.stage == "advance" {
		reflect.ValueOf(s.ReceivePackSession).Elem().FieldByName("Stdout").Set(reflect.ValueOf(s.Stdout))
		return s.ReceivePackSession.ReceivePack(ctx, req)
	}
	if req.Packfile != nil {
		defer func() { _ = req.Packfile.Close() }()
	}
	if s.d.stage == "cancel after" {
		s.d.cancel()
		return nil, ctx.Err()
	}
	_, _ = io.Copy(io.Discard, s.Stdout)
	return s.d.report, s.d.receiveErr
}

func TestPublishDisposition(t *testing.T) {
	ref := "refs/uzi-checkpoints/main"
	// Decode actual packet lines: even a successfully decoded report may omit the
	// requested command. A nonnil report alone is not acknowledgement of the update.
	decode := func(lines ...string) *packp.ReportStatus {
		t.Helper()
		var wire bytes.Buffer
		encoder := pktline.NewEncoder(&wire)
		for _, line := range lines {
			if err := encoder.EncodeString(line + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		if err := encoder.Flush(); err != nil {
			t.Fatal(err)
		}
		r := packp.NewReportStatus()
		if err := r.Decode(&wire); err != nil {
			t.Fatal(err)
		}
		return r
	}
	succeeded := decode("unpack ok", "ok "+ref)
	rejected := decode("unpack ok", "ng "+ref+" novel server policy")
	unpackRejected := decode("unpack invalid fixture")
	partial := decode("unpack ok")
	wrongRef := decode("unpack ok", "ok refs/heads/main")
	workflowRejected := decode("unpack ok", "ng "+ref+" missing workflow scope")
	nonFastForward := decode("unpack ok", "ng "+ref+" non-fast-forward")
	cases := []struct {
		name    string
		report  *packp.ReportStatus
		err     error
		want    pushbroker.PublishDisposition
		calls   int
		current bool
		mapped  error
	}{
		{name: "advance", want: pushbroker.PublishAdvanced, calls: 1},
		{name: "success ordinary close error", report: succeeded, err: errDispositionFailure, want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "success workflow close error", report: succeeded, err: errors.New("missing workflow scope"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "success non-fast-forward close error", report: succeeded, err: errors.New("non-fast-forward"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "already current", current: true},
		{name: "list", err: errDispositionFailure},
		{name: "fetch", err: errDispositionFailure},
		{name: "session", err: errDispositionFailure},
		{name: "advertisement", err: errDispositionFailure},
		{name: "cancel before", err: context.Canceled},
		{name: "unfamiliar rejection", report: rejected, err: rejected.Error(), calls: 1},
		{name: "unfamiliar rejection workflow close text", report: rejected, err: errors.New("missing workflow scope"), calls: 1},
		{name: "unfamiliar rejection non-fast-forward close text", report: rejected, err: errors.New("non-fast-forward"), calls: 1},
		{name: "unpack rejection", report: unpackRejected, err: unpackRejected.Error(), calls: 1},
		{name: "known workflow rejection", report: workflowRejected, err: workflowRejected.Error(), mapped: pushbroker.ErrWorkflowScopeRejected, calls: 1},
		{name: "known non-fast-forward rejection", report: nonFastForward, err: nonFastForward.Error(), mapped: pushbroker.ErrNotDescendant, calls: 1},
		{name: "unknown workflow text", err: errors.New("missing workflow scope"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "unknown non-fast-forward text", err: errors.New("non-fast-forward"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "partial workflow text", report: partial, err: errors.New("missing workflow scope"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "wrong ref non-fast-forward text", report: wrongRef, err: errors.New("non-fast-forward"), want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "missing acknowledgement", err: io.ErrUnexpectedEOF, want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "empty success response", want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "partial acknowledgement", report: partial, want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "wrong ref acknowledgement", report: wrongRef, want: pushbroker.PublishOutcomeUnknown, calls: 1},
		{name: "cancel after", err: context.Canceled, want: pushbroker.PublishOutcomeUnknown, calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitFixture(t)
			base := f.commit("base.txt", "base\n", "base")
			f.pushMain()
			tip := f.commit("tip.txt", "tip\n", "tip")
			if tc.current {
				f.git("push", "origin", tip+":"+ref)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ep, err := transport.NewEndpoint(f.cloneURL())
			if err != nil {
				t.Fatal(err)
			}
			delegate, err := client.NewClient(ep)
			if err != nil {
				t.Fatal(err)
			}
			d := &dispositionTransport{Transport: delegate, stage: tc.name, report: tc.report, receiveErr: tc.err, cancel: cancel}
			const protocol = "disposition"
			previous := client.Protocols[protocol]
			client.InstallProtocol(protocol, d)
			defer func() {
				if previous == nil {
					delete(client.Protocols, protocol)
				} else {
					client.InstallProtocol(protocol, previous)
				}
			}()
			res, err := pushbroker.Publish(ctx, pushbroker.Options{
				CloneURL: protocol + "://" + f.bare, Branch: "main", DefaultBranch: "main",
				DeclaredTip: tip, Pack: f.pack(tip, base),
			})
			wantCause := tc.err
			if tc.mapped != nil {
				wantCause = tc.mapped
			}
			if wantCause != nil && !errors.Is(err, wantCause) {
				t.Errorf("error = %v, want %v", err, wantCause)
			}
			wantError := tc.err != nil || tc.want == pushbroker.PublishOutcomeUnknown
			if (err != nil) != wantError {
				t.Errorf("error = %v, wantError %v", err, wantError)
			}
			if tc.want == pushbroker.PublishOutcomeUnknown && (errors.Is(err, pushbroker.ErrNotDescendant) || errors.Is(err, pushbroker.ErrWorkflowScopeRejected)) {
				t.Errorf("unknown outcome mapped to definitive rejection: %v", err)
			}
			if res.Disposition != tc.want {
				t.Errorf("Disposition = %v, want %v", res.Disposition, tc.want)
			}
			if d.invoked != tc.calls {
				t.Errorf("ReceivePack invocations = %d, want %d", d.invoked, tc.calls)
			}
			if res.AlreadyCurrent != tc.current {
				t.Errorf("AlreadyCurrent = %v, want %v", res.AlreadyCurrent, tc.current)
			}
			if tc.name == "advance" && f.originRef(ref) != tip {
				t.Error("actual advance did not reach origin")
			}
		})
	}
}
