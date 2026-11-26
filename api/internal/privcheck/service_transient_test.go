package privcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// classedErr is a test error carrying a forge class, read by forge.Class through
// errors.As the same way a driver's scrubbed error is.
type classedErr struct{ class forge.ErrorClass }

func (e classedErr) Error() string           { return "forge failure" }
func (e classedErr) Class() forge.ErrorClass { return e.class }

// classedWrapErr is a classed timeout that also wraps its cause (e.g. DeadlineExceeded).
// Drivers do not wrap the cause (forge's redactor keeps only a message and a class), and
// the guard reads only forge.Class, so these rows behave like the plain classed-timeout
// rows today; they guard against a future errors.Is check on the cause.
type classedWrapErr struct {
	class forge.ErrorClass
	cause error
}

func (e classedWrapErr) Error() string           { return "forge failure: " + e.cause.Error() }
func (e classedWrapErr) Unwrap() error           { return e.cause }
func (e classedWrapErr) Class() forge.ErrorClass { return e.class }

var retryableClasses = []forge.ErrorClass{
	forge.ErrorClassTimeout, forge.ErrorClassServerError,
	forge.ErrorClassRateLimited, forge.ErrorClassNetwork,
}

var nonRetryableClasses = []forge.ErrorClass{forge.ErrorClassAuth, forge.ErrorClassOther}

func guardWith(f *fakeForge, ctx context.Context) GuardResult {
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	return svc.GuardRepo(ctx, GuardInput{Repo: aGuardRepo("main")})
}

func protForge(p protResult) *fakeForge {
	return &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: p},
	}
}

func wantCause(t *testing.T, res GuardResult, want forge.ErrorClass) {
	t.Helper()
	got, ok := res.TransientForgeCause()
	if !ok || got != want {
		t.Fatalf("TransientForgeCause = (%q, %v), want (%q, true); result %+v", got, ok, want, res)
	}
}

func wantNoCause(t *testing.T, res GuardResult) {
	t.Helper()
	if got, ok := res.TransientForgeCause(); ok {
		t.Fatalf("TransientForgeCause = (%q, true), want none; result %+v", got, res)
	}
}

func TestGuardRepoTransientCauseVerifyToken(t *testing.T) {
	for _, c := range retryableClasses {
		t.Run(string(c), func(t *testing.T) {
			res := guardWith(&fakeForge{verifyErr: classedErr{c}}, context.Background())
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantCause(t, res, c)
		})
	}
	for _, c := range nonRetryableClasses {
		t.Run("none_"+string(c), func(t *testing.T) {
			res := guardWith(&fakeForge{verifyErr: classedErr{c}}, context.Background())
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantNoCause(t, res)
		})
	}
	t.Run("unclassified", func(t *testing.T) {
		wantNoCause(t, guardWith(&fakeForge{verifyErr: errors.New("boom")}, context.Background()))
	})
}

func TestGuardRepoTransientCauseBranchProtectionError(t *testing.T) {
	for _, c := range retryableClasses {
		t.Run(string(c), func(t *testing.T) {
			res := guardWith(protForge(protResult{err: classedErr{c}}), context.Background())
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantCause(t, res, c)
		})
	}
	for _, c := range nonRetryableClasses {
		t.Run("none_"+string(c), func(t *testing.T) {
			wantNoCause(t, guardWith(protForge(protResult{err: classedErr{c}}), context.Background()))
		})
	}
}

func TestGuardRepoTransientCauseContextDone(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithTimeout(context.Background(), 0)
	defer cancelExpired()
	<-expired.Done()
	for name, ctx := range map[string]context.Context{"canceled": canceled, "expired": expired} {
		t.Run(name+"_verify", func(t *testing.T) {
			res := guardWith(&fakeForge{verifyErr: classedErr{forge.ErrorClassNetwork}}, ctx)
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantNoCause(t, res)
		})
		t.Run(name+"_protection", func(t *testing.T) {
			wantNoCause(t, guardWith(protForge(protResult{err: classedErr{forge.ErrorClassNetwork}}), ctx))
		})
		// A timeout-class error is the shape a done caller context actually
		// produces; it must be suppressed the same way as a network one.
		t.Run(name+"_verify_timeout", func(t *testing.T) {
			res := guardWith(&fakeForge{verifyErr: classedErr{forge.ErrorClassTimeout}}, ctx)
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantNoCause(t, res)
		})
		// A timeout-classed error that wraps the real context.DeadlineExceeded: only the done
		// caller context can suppress it, since the class alone would make it transient.
		t.Run(name+"_verify_deadline_exceeded", func(t *testing.T) {
			res := guardWith(&fakeForge{verifyErr: classedWrapErr{forge.ErrorClassTimeout, context.DeadlineExceeded}}, ctx)
			if !res.Blocked {
				t.Fatalf("must still block: %+v", res)
			}
			wantNoCause(t, res)
		})
		t.Run(name+"_protection_timeout", func(t *testing.T) {
			wantNoCause(t, guardWith(protForge(protResult{err: classedErr{forge.ErrorClassTimeout}}), ctx))
		})
	}
	// Control: the same timeout-class errors under a live context are transient.
	t.Run("live_verify_timeout", func(t *testing.T) {
		wantCause(t, guardWith(&fakeForge{verifyErr: classedErr{forge.ErrorClassTimeout}}, context.Background()), forge.ErrorClassTimeout)
	})
	t.Run("live_verify_deadline_exceeded", func(t *testing.T) {
		wantCause(t, guardWith(&fakeForge{verifyErr: classedWrapErr{forge.ErrorClassTimeout, context.DeadlineExceeded}}, context.Background()), forge.ErrorClassTimeout)
	})
	t.Run("live_protection_timeout", func(t *testing.T) {
		wantCause(t, guardWith(protForge(protResult{err: classedErr{forge.ErrorClassTimeout}}), context.Background()), forge.ErrorClassTimeout)
	})
}

func TestGuardRepoTransientCauseProtectionUnverified(t *testing.T) {
	t.Run("with_class", func(t *testing.T) {
		bp := forge.BranchProtection{Protected: true, ProtectionUnverified: true, UnverifiedReadClass: forge.ErrorClassNetwork}
		res := guardWith(protForge(protResult{bp: bp}), context.Background())
		if !res.Blocked {
			t.Fatalf("must still block: %+v", res)
		}
		wantCause(t, res, forge.ErrorClassNetwork)
	})
	t.Run("without_class", func(t *testing.T) {
		bp := forge.BranchProtection{Protected: true, ProtectionUnverified: true}
		res := guardWith(protForge(protResult{bp: bp}), context.Background())
		if !res.Blocked {
			t.Fatalf("must still block: %+v", res)
		}
		wantNoCause(t, res)
	})
	t.Run("mixed_block", func(t *testing.T) {
		bp := forge.BranchProtection{Protected: true, ProtectionUnverified: true, UnverifiedReadClass: forge.ErrorClassNetwork, BotCanPush: true}
		res := guardWith(protForge(protResult{bp: bp}), context.Background())
		if !res.Blocked {
			t.Fatalf("must block: %+v", res)
		}
		wantNoCause(t, res)
	})
}

func TestGuardRepoTransientCauseClientBuildError(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeBuilder{buildErr: classedErr{forge.ErrorClassNetwork}})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("must block: %+v", res)
	}
	wantNoCause(t, res)
}

func TestTransientForgeCauseConstructed(t *testing.T) {
	unreadable := newFinding(CodeProtectionUnreadable, "x")
	unprotected := newFinding(CodeDefaultBranchUnprotected, "y")
	cases := []struct {
		name string
		res  GuardResult
		want bool
	}{
		{"single unreadable", GuardResult{Blocked: true, ForgeRetryClass: forge.ErrorClassTimeout, Findings: []Finding{unreadable}}, true},
		{"not blocked", GuardResult{Blocked: false, ForgeRetryClass: forge.ErrorClassTimeout, Findings: []Finding{unreadable}}, false},
		{"empty class", GuardResult{Blocked: true, Findings: []Finding{unreadable}}, false},
		{"auth class", GuardResult{Blocked: true, ForgeRetryClass: forge.ErrorClassAuth, Findings: []Finding{unreadable}}, false},
		{"other blocker", GuardResult{Blocked: true, ForgeRetryClass: forge.ErrorClassNetwork, Findings: []Finding{unreadable, unprotected}}, false},
		{"two unreadable", GuardResult{Blocked: true, ForgeRetryClass: forge.ErrorClassNetwork, Findings: []Finding{unreadable, unreadable}}, false},
		{"no blocking finding", GuardResult{Blocked: true, ForgeRetryClass: forge.ErrorClassNetwork}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := tc.res.TransientForgeCause()
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
		})
	}
}
