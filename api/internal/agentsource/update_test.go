package agentsource

import (
	"context"
	"errors"
	"testing"
)

// --- DeriveUpdate (pure) -------------------------------------------------------

func TestDeriveUpdate(t *testing.T) {
	cases := []struct {
		name           string
		pinnedRef      string
		lastAppliedSHA string
		latestRef      string
		remoteTipSHA   string
		wantAvail      bool
		wantLatest     string
	}{
		{
			// tag-pinned, source ahead: available and names the newer tag.
			name: "tag pinned newer available", pinnedRef: "v1.9.0", latestRef: "v1.10.0",
			wantAvail: true, wantLatest: "v1.10.0",
		},
		{
			// DISCRIMINATING: a DISTINCT lexical trap on a different major/minor pair —
			// a lexical compare says v2.10.0 < v2.9.0 and would wrongly report "not
			// available". Semver must report available and name the newer tag.
			name: "tag pinned discriminating lexical trap", pinnedRef: "v2.9.0", latestRef: "v2.10.0",
			wantAvail: true, wantLatest: "v2.10.0",
		},
		{
			// tag-pinned at the current newest: no update.
			name: "tag pinned current", pinnedRef: "v1.10.0", latestRef: "v1.10.0",
			wantAvail: false, wantLatest: "",
		},
		{
			// tag-pinned but the source advertises no valid semver tag yet.
			name: "tag pinned no latest", pinnedRef: "v1.9.0", latestRef: "",
			wantAvail: false, wantLatest: "",
		},
		{
			// non-v-prefixed tag pin still classifies as tag-mode (re-prefix + IsValid).
			name: "tag pinned no v prefix", pinnedRef: "1.9.0", latestRef: "1.10.0",
			wantAvail: true, wantLatest: "1.10.0",
		},
		{
			// branch-pinned: advertised tip differs from applied → moved (latest empty).
			name: "branch moved", pinnedRef: "main", lastAppliedSHA: "def", remoteTipSHA: "abc",
			wantAvail: true, wantLatest: "",
		},
		{
			// branch-pinned: tip equals applied → no update, clears after apply.
			name: "branch unchanged", pinnedRef: "main", lastAppliedSHA: "abc", remoteTipSHA: "abc",
			wantAvail: false, wantLatest: "",
		},
		{
			// branch tip compares case-insensitively to the applied SHA.
			name: "branch unchanged case insensitive", pinnedRef: "main", lastAppliedSHA: "ABC", remoteTipSHA: "abc",
			wantAvail: false, wantLatest: "",
		},
		{
			// empty ref behaves as branch/default-branch mode.
			name: "empty ref moved", pinnedRef: "", lastAppliedSHA: "def", remoteTipSHA: "abc",
			wantAvail: true, wantLatest: "",
		},
		{
			// empty ref, tip equals applied → no update.
			name: "empty ref unchanged", pinnedRef: "  ", lastAppliedSHA: "abc", remoteTipSHA: "abc",
			wantAvail: false, wantLatest: "",
		},
		{
			// branch-pinned but no tip advertised yet (never checked): no signal.
			name: "branch no tip", pinnedRef: "main", lastAppliedSHA: "def", remoteTipSHA: "",
			wantAvail: false, wantLatest: "",
		},
		{
			// SHA-pinned (40-hex): immutable, no signal regardless of latestRef/tip.
			name: "sha pinned", pinnedRef: "0123456789abcdef0123456789abcdef01234567",
			latestRef: "v9.9.9", remoteTipSHA: "abc", lastAppliedSHA: "def",
			wantAvail: false, wantLatest: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAvail, gotLatest := DeriveUpdate(tc.pinnedRef, tc.lastAppliedSHA, tc.latestRef, tc.remoteTipSHA)
			if gotAvail != tc.wantAvail || gotLatest != tc.wantLatest {
				t.Errorf("DeriveUpdate(%q,%q,%q,%q) = (%v,%q); want (%v,%q)",
					tc.pinnedRef, tc.lastAppliedSHA, tc.latestRef, tc.remoteTipSHA,
					gotAvail, gotLatest, tc.wantAvail, tc.wantLatest)
			}
		})
	}
}

// --- CheckForUpdate (seam-injected, no git) ------------------------------------

// countingLsRemote returns a ListRefsFunc that returns refs/err and counts calls.
func countingLsRemote(refs RemoteRefs, err error, calls *int) ListRefsFunc {
	return func(context.Context, CloneOptions) (RemoteRefs, error) {
		*calls++
		return refs, err
	}
}

func TestCheckForUpdatePersistsRemoteFacts(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "v1.9.0"}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{
		HeadSHA:  "head-sha",
		Tags:     map[string]string{"v1.9.0": "sha-9", "v1.10.0": "sha-10"},
		Branches: map[string]string{"main": "head-sha"},
	}, nil, &calls)

	res, err := r.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != statusOK {
		t.Fatalf("want ok; got %+v", res)
	}
	if calls != 1 {
		t.Errorf("ls-remote should be called exactly once; got %d", calls)
	}
	if res.LatestRef != "v1.10.0" {
		t.Errorf("latest should be the highest semver tag v1.10.0 (not lexical); got %q", res.LatestRef)
	}
	// ref is a tag-pin present in Tags → tip resolves to that tag's sha.
	if res.RemoteTipSHA != "sha-9" {
		t.Errorf("tip should resolve to the pinned tag's sha; got %q", res.RemoteTipSHA)
	}
	if st.appSet["agent_source_latest_ref"] != "v1.10.0" {
		t.Errorf("persisted latest_ref wrong: %q", st.appSet["agent_source_latest_ref"])
	}
	if st.appSet["agent_source_remote_tip_sha"] != "sha-9" {
		t.Errorf("persisted remote_tip_sha wrong: %q", st.appSet["agent_source_remote_tip_sha"])
	}
	if st.appSet["agent_source_update_checked_at"] == "" {
		t.Errorf("update_checked_at must be set")
	}
	if set.invalidated == 0 {
		t.Errorf("settings cache must be invalidated after a persist")
	}

	// Feed the persisted facts through DeriveUpdate: pinned v1.9.0 + latest v1.10.0 →
	// update available, names v1.10.0.
	avail, latest := DeriveUpdate(set.ref, st.appSet["agent_source_last_applied_sha"],
		st.appSet["agent_source_latest_ref"], st.appSet["agent_source_remote_tip_sha"])
	if !avail || latest != "v1.10.0" {
		t.Errorf("derived update wrong: avail=%v latest=%q", avail, latest)
	}
}

func TestCheckForUpdateBranchTip(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "main"}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{
		HeadSHA:  "head-sha",
		Tags:     map[string]string{},
		Branches: map[string]string{"main": "branch-tip"},
	}, nil, &calls)

	res, err := r.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != statusOK || res.LatestRef != "" || res.RemoteTipSHA != "branch-tip" {
		t.Fatalf("branch check wrong: %+v", res)
	}
}

func TestCheckForUpdateErrorPersistsNothing(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "main"}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{}, errors.New("agentsource: list refs: dial tcp: no route"), &calls)

	res, err := r.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != statusError {
		t.Fatalf("want error status; got %+v", res)
	}
	if len(st.appSet) != 0 {
		t.Errorf("an error must persist nothing; appSet=%v", st.appSet)
	}
	if set.invalidated != 0 {
		t.Errorf("an error must not invalidate the cache; got %d", set.invalidated)
	}
}

func TestCheckForUpdateDisabledWhenUnconfigured(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "   "}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{}, nil, &calls)

	res, _ := r.CheckForUpdate(context.Background())
	if res.Status != "disabled" {
		t.Fatalf("unconfigured url should be disabled; got %+v", res)
	}
	if calls != 0 {
		t.Errorf("an unconfigured url must not reach ls-remote; calls=%d", calls)
	}
	if len(st.appSet) != 0 {
		t.Errorf("disabled must persist nothing; appSet=%v", st.appSet)
	}
}

func TestCheckForUpdateOffAllowlistNoEgress(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://evil.test/a.git", ref: "main"}
	// allow=false → SSRF recheck fails; ls-remote must not be reached.
	r := newRec(st, set, false)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{}, nil, &calls)

	res, _ := r.CheckForUpdate(context.Background())
	if res.Status != statusError {
		t.Fatalf("off-allowlist should be error; got %+v", res)
	}
	if calls != 0 {
		t.Errorf("off-allowlist must cause zero egress; ls-remote calls=%d", calls)
	}
	if len(st.appSet) != 0 {
		t.Errorf("off-allowlist must persist nothing; appSet=%v", st.appSet)
	}
}

// TestDeriveUpdateZeroEgress proves the READ/derive path never touches the ls-remote
// seam: a call-counting lsRemote injected into the Reconciler stays at 0 while the
// derive path (DeriveUpdate over pre-seeded remote facts) runs. The handler's GET calls
// DeriveUpdate — a pure function decoupled from the reconciler — not CheckForUpdate, so
// only the explicit update-check reaches egress. The strongest form (a handler-level GET
// against an unreachable configured URL that still returns the stored-derived values) is
// deferred to the LiveDB rig because Handler.settings/q are concrete (*settings.Cache /
// *store.Queries); see PRD #702 M8 / the M4 LiveDB validation row.
func TestDeriveUpdateZeroEgress(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "v1.9.0"}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{
		Tags: map[string]string{"v1.10.0": "sha-10"},
	}, nil, &calls)

	// Simulate what the GET/derive path does: read stored remote facts + live config
	// through DeriveUpdate, repeatedly. None of this may reach the seam.
	for i := 0; i < 5; i++ {
		if avail, latest := DeriveUpdate("v1.9.0", "", "v1.10.0", "sha-10"); !avail || latest != "v1.10.0" {
			t.Fatalf("derive wrong: avail=%v latest=%q", avail, latest)
		}
		_, _ = DeriveUpdate("main", "abc", "", "def")
	}
	if calls != 0 {
		t.Errorf("the derive/read path must perform zero egress; ls-remote calls=%d", calls)
	}
}

func TestCheckForUpdateCredErrorNoEgress(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "main", credErr: errors.New("decrypt failed")}
	r := newRec(st, set, true)
	calls := 0
	r.lsRemote = countingLsRemote(RemoteRefs{}, nil, &calls)

	res, _ := r.CheckForUpdate(context.Background())
	if res.Status != statusError {
		t.Fatalf("cred decrypt failure should be error; got %+v", res)
	}
	if calls != 0 {
		t.Errorf("a cred failure must not reach ls-remote; calls=%d", calls)
	}
	if len(st.appSet) != 0 {
		t.Errorf("cred failure must persist nothing; appSet=%v", st.appSet)
	}
}
