package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_promote_test.go pins PRD #1590 M4's classification of a held
// codex_account_unavailable run (classifyCodexAccountHold) without a database: which states are
// terminal (a definite binding mismatch on a linked alias), which stay held (transient), and that
// promotion still requires the unchanged release predicate.

// heldReleasableInputs is a held subscription run whose linked account passes the release
// predicate: same identity, same credential revision, material current, account idle.
func heldReleasableInputs(t *testing.T) codexReleaseInputs {
	t.Helper()
	key, err := codexAccountKey("user-1", "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	return codexReleaseInputs{
		authMode: codexAuthModeSubscription, boundKind: store.KindCodexAuth, status: "recovery_wait",
		frozenMaterialRev: 4, frozenMaterialRevValid: true, currentMaterialRev: 4, currentMaterialRevValid: true,
		frozenAccountKey: key, frozenAccountKeyValid: true,
		currentProviderUserID: "user-1", currentProviderUserIDValid: true,
		currentWorkspaceAccountID: "ws-1", currentWorkspaceAccountIDValid: true,
		frozenAccountRev: 7, frozenAccountRevValid: true, currentCredentialRev: 7, currentCredentialRevValid: true,
		coordState: "idle", coordStateValid: true,
	}
}

func TestClassifyCodexAccountHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(in *codexReleaseInputs)
		want    codexHoldOutcome
		wantErr error
	}{
		{"releasable idle", func(*codexReleaseInputs) {}, codexHoldPromote, nil},
		{"releasable committed", func(in *codexReleaseInputs) { in.coordState = "committed" }, codexHoldPromote, nil},

		// Transient: the run stays held and nothing is written.
		{"same identity quarantined", func(in *codexReleaseInputs) { in.coordState = codexCoordQuarantined },
			codexHoldStay, ErrCodexAccountQuarantined},
		{"same identity quarantined, material ahead", func(in *codexReleaseInputs) {
			in.coordState, in.currentMaterialRev = codexCoordQuarantined, 5
		}, codexHoldStay, ErrCodexMaterialRevisionStale},
		{"in_progress, material ahead (re-admission not applied)", func(in *codexReleaseInputs) {
			in.coordState, in.currentMaterialRev = "in_progress", 5
		}, codexHoldStay, ErrCodexMaterialRevisionStale},
		{"idle, material ahead (CAS lost to a concurrent change)", func(in *codexReleaseInputs) { in.currentMaterialRev = 6 },
			codexHoldStay, ErrCodexMaterialRevisionStale},
		{"no account columns read", func(in *codexReleaseInputs) {
			in.currentProviderUserIDValid, in.currentWorkspaceAccountIDValid, in.currentCredentialRevValid = false, false, false
			in.coordStateValid = false
		}, codexHoldStay, ErrCodexAccountTupleMismatch},
		{"no current credential revision", func(in *codexReleaseInputs) { in.currentCredentialRevValid = false },
			codexHoldStay, ErrCodexAccountTupleMismatch},
		{"not an actively-claimed status", func(in *codexReleaseInputs) { in.status = "queued" },
			codexHoldStay, ErrCodexRunNotActivelyClaimed},

		// Terminal: a definite binding mismatch on the linked alias.
		{"different identity", func(in *codexReleaseInputs) { in.currentProviderUserID = "user-2" },
			codexHoldFail, ErrCodexAccountTupleMismatch},
		{"different identity while quarantined", func(in *codexReleaseInputs) {
			in.currentWorkspaceAccountID, in.coordState = "ws-2", codexCoordQuarantined
		}, codexHoldFail, ErrCodexAccountTupleMismatch},
		{"different identity during a lease", func(in *codexReleaseInputs) {
			in.currentProviderUserID, in.coordState = "user-2", "in_progress"
		}, codexHoldFail, ErrCodexAccountTupleMismatch},
		{"undecodable frozen key", func(in *codexReleaseInputs) { in.frozenAccountKey = "not-json" },
			codexHoldFail, ErrCodexAccountTupleMismatch},
		{"unfrozen identity", func(in *codexReleaseInputs) { in.frozenAccountKeyValid = false },
			codexHoldFail, ErrCodexAccountKeyUnfrozen},
		{"credential revision bumped", func(in *codexReleaseInputs) { in.currentCredentialRev = 8 },
			codexHoldFail, ErrCodexAccountRevisionStale},
		{"credential revision bumped while quarantined", func(in *codexReleaseInputs) {
			in.currentCredentialRev, in.coordState = 8, codexCoordQuarantined
		}, codexHoldFail, ErrCodexAccountRevisionStale},
		{"no frozen credential revision", func(in *codexReleaseInputs) { in.frozenAccountRevValid = false },
			codexHoldFail, ErrCodexAccountRevisionStale},
		{"alias kind changed", func(in *codexReleaseInputs) { in.boundKind = store.KindOpenAIAPIKey },
			codexHoldFail, ErrCodexKindModeMismatch},
		{"auth mode changed", func(in *codexReleaseInputs) { in.authMode = codexAuthModeAPIKey },
			codexHoldFail, ErrCodexKindModeMismatch},
		{"auth mode and kind both changed", func(in *codexReleaseInputs) {
			in.authMode, in.boundKind = codexAuthModeAPIKey, store.KindOpenAIAPIKey
		}, codexHoldFail, ErrCodexKindModeMismatch},
		{"unknown auth mode", func(in *codexReleaseInputs) { in.authMode = "" },
			codexHoldFail, ErrCodexKindModeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := heldReleasableInputs(t)
			tc.mutate(&in)
			got, err := classifyCodexAccountHold(in)
			if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("classify = (%d, %v), want (%d, %v)", got, err, tc.want, tc.wantErr)
			}
			// Promotion is never decided around the release predicate.
			if got == codexHoldPromote && evalCodexReleasePredicate(in) != nil {
				t.Fatal("promote decided while the release predicate refuses")
			}
		})
	}
}

// TestCodexReadmitPayloadCarriesNoSecrets: the D5 feed line names the alias label and the revision
// change, and is built only from those, so it can hold no token or identity material.
func TestCodexReadmitPayloadCarriesNoSecrets(t *testing.T) {
	run := store.Run{}
	run.CodexSecretLabel.String, run.CodexSecretLabel.Valid = "work login", true
	run.CodexAccountKey.String, run.CodexAccountKey.Valid = `["user-secret-id","ws-secret-id"]`, true
	raw, err := codexReadmitPayload(run, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"alias_label":"work login"`, `"from_material_revision":3`, `"to_material_revision":4`,
		`\"work login\"`, "3 to 4", `"event":"` + codexReadmitEvent + `"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("payload %s lacks %s", s, want)
		}
	}
	for _, leak := range []string{"user-secret-id", "ws-secret-id"} {
		if strings.Contains(s, leak) {
			t.Fatalf("payload %s carries identity material %q", s, leak)
		}
	}
}
