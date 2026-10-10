package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545: heldRemote is the one door to the forge credential for held-work publication. It
// must refuse before decrypting, fail closed, and hand back only server-derived coordinates.

func TestHeldRemoteDerivesServerSideConnection(t *testing.T) {
	fs := &salvageStore{}
	svc, _ := newSalvageSvc(t, fs, nil, time.Hour)
	run := uuid.New()
	f, err := svc.HeldRemote()(context.Background(), run)
	if err != nil {
		t.Fatalf("HeldRemote: %v", err)
	}
	if f.CloneURL != "https://github.example.com/team/repo.git" || f.Username != "uzi-bot" || f.PAT != salvagePATok {
		t.Fatalf("HeldForge = %+v", f)
	}
	if fs.claimCalls != 1 {
		t.Fatalf("claim context reads = %d, want 1", fs.claimCalls)
	}

	lo := f.ListOptions()
	if lo.CloneURL != f.CloneURL || lo.Username != f.Username || lo.PAT != f.PAT {
		t.Fatalf("ListOptions = %+v", lo)
	}
	ref := pushbroker.HeldRef(run, 3)
	do := f.DeleteOptions(ref, salvageTip)
	if do.Ref != ref || do.ExpectedOldTip != salvageTip || do.CloneURL != f.CloneURL || do.PAT != f.PAT || do.Username != f.Username {
		t.Fatalf("DeleteOptions = %+v", do)
	}
	var po pushbroker.HeldPackOptions
	f.ApplyTo(&po)
	if po.CloneURL != f.CloneURL || po.Username != f.Username || po.PAT != f.PAT {
		t.Fatalf("ApplyTo = %+v", po)
	}
}

// The SSRF gate runs BEFORE the PAT is decrypted: the claim context carries a ciphertext that
// cannot be opened, so a decrypt-first order would surface "could not be decrypted" instead.
func TestHeldRemoteSSRFGateRunsBeforeDecrypt(t *testing.T) {
	for _, tc := range []struct{ name, web, base string }{
		{"base url", "https://github.example.com/team/repo", "https://evil.example.net"},
		{"clone host", "https://evil.example.net/team/repo", "https://github.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &salvageStore{}
			fs.claimCtx = store.GetRunClaimContextRow{
				RepoWebUrl: tc.web, BaseUrl: tc.base, BotUsername: "uzi-bot", TokenCiphertext: []byte("not-a-sealed-box"),
			}
			svc, _ := newSalvageSvc(t, fs, nil, time.Hour)
			f, err := svc.HeldRemote()(context.Background(), uuid.New())
			if err == nil || !strings.Contains(err.Error(), "not allowlisted") {
				t.Fatalf("HeldRemote = %+v, %v; want an allowlist refusal", f, err)
			}
			if strings.Contains(err.Error(), "decrypted") {
				t.Fatalf("the PAT was decrypted before the SSRF gate: %v", err)
			}
			if f != (HeldForge{}) {
				t.Fatalf("a refused remote returned coordinates: %+v", f)
			}
		})
	}
}

func TestHeldRemoteFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unset func(*Service)
	}{
		{"no allowlist", func(s *Service) { s.forgeBaseURLAllowed = nil }},
		{"no box", func(s *Service) { s.box = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &salvageStore{}
			svc, _ := newSalvageSvc(t, fs, nil, time.Hour)
			tc.unset(svc)
			_, err := svc.HeldRemote()(context.Background(), uuid.New())
			if err == nil || !strings.Contains(err.Error(), "not configured") || fs.claimCalls != 0 {
				t.Fatalf("HeldRemote err=%v claimReads=%d, want a fail-closed refusal before any read", err, fs.claimCalls)
			}
		})
	}
}

func TestHeldRemoteGoneAndReadErrors(t *testing.T) {
	fs := &salvageStore{claimErr: pgx.ErrNoRows}
	svc, _ := newSalvageSvc(t, fs, nil, time.Hour)
	if _, err := svc.HeldRemote()(context.Background(), uuid.New()); !errors.Is(err, ErrHeldRemoteGone) {
		t.Fatalf("a vanished run = %v, want ErrHeldRemoteGone", err)
	}
	fs = &salvageStore{claimErr: errors.New("db: connection reset")}
	svc, _ = newSalvageSvc(t, fs, nil, time.Hour)
	_, err := svc.HeldRemote()(context.Background(), uuid.New())
	if err == nil || errors.Is(err, ErrHeldRemoteGone) || !strings.Contains(err.Error(), "claim context") {
		t.Fatalf("a transient read failure = %v, want a retryable claim-context error", err)
	}
}
