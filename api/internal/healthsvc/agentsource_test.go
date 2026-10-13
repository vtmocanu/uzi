package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

func sourceTestService(set Settings) *Service {
	s := New(Config{Store: &fakeStore{}, Settings: set, Now: func() time.Time { return fixedNow }})
	s.probeDB = func(context.Context) dbStat { return dbStat{schemaAtHead: true, maxConns: 20} }
	return s
}

func TestAgentSourceUpdateMatrix(t *testing.T) {
	sha := strings.Repeat("aB", 20)
	for _, tc := range []struct {
		name, ref, latest, tip, applied, severity, summary string
	}{
		{"newer tag", "v1.2.0", "v1.10.0", "", "", sevWarn, "newer tag v1.10.0"},
		{"equal tag", "1.2.0", "v1.2.0", "", "", sevOK, "up to date"},
		{"older tag", "v1.10.0", "1.2.0", "", "", sevOK, "up to date"},
		{"trimmed tag", " v1.2.0 ", "v1.10.0", "", "", sevWarn, "newer tag"},
		{"branch moved", "main", "", "new", "old", sevWarn, "tracked branch moved"},
		{"branch equal", "main", "", "abc", "ABC", sevOK, "up to date"},
		{"branch applied", "main", "", "new", "new", sevOK, "up to date"},
		{"default moved", "", "", "new", "old", sevWarn, "tracked branch moved"},
		{"default equal", "", "", "new", "new", sevOK, "up to date"},
		{"default unapplied", "", "", "new", "", sevWarn, "tracked branch moved"},
		{"missing tip", "main", "", "", "old", sevOK, "up to date"},
		{"SHA", " " + sha + " ", "", "new", "old", sevNA, "immutable SHA"},
		{"near SHA short", sha[:39], "", "new", "old", sevWarn, "tracked branch moved"},
		{"near SHA long", sha + "a", "", "new", "old", sevWarn, "tracked branch moved"},
		{"near SHA nonhex", sha[:39] + "g", "", "new", "old", sevWarn, "tracked branch moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &fakeSettings{healthEnabled: true, sourceEnabled: true, sourceURL: "https://configured.test/source.git", sourceRef: tc.ref,
				sourceStatus: settings.AgentSourceStatus{UpdateCheckedAt: fixedNow.Format(time.RFC3339), LatestRef: tc.latest, RemoteTipSHA: tc.tip, LastAppliedSHA: tc.applied, LastSyncStatus: "error", LastSyncError: "ignored", LastSyncSHA: "drift"}}
			assertSourceEvaluation(t, sourceTestService(set), tc.severity, tc.summary)
		})
	}
}

func assertSourceEvaluation(t *testing.T, svc *Service, severity, summary string) {
	t.Helper()
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, text := check(t, doc, "agentsource.update")
	if got != severity || !strings.Contains(text, summary) {
		t.Fatalf("got %s %q, want %s containing %q", got, text, severity, summary)
	}
	if doc.Blocking || doc.Counts.Danger != 0 {
		t.Fatalf("advisory evaluation became blocking: %+v", doc)
	}
	for _, c := range doc.Checks {
		if c.ID == "agentsource.update" && (c.Scope != "instance" || c.Group != groupHousekeeping || c.Title != "Agent source update" || c.Doc == nil || *c.Doc != "agent-source" || c.Action == nil || !strings.Contains(*c.Action, "/admin/settings#agent-source")) {
			t.Fatalf("metadata: %+v", c)
		}
	}
	if severity == sevWarn || severity == sevUnknown {
		st := &fakeEpisodeStore{}
		r := NewEpisodeReconciler(svc, st, &fakeEpisodeNotifier{}, &fakeEpisodeSettings{enabled: true}, nil)
		r.Reconcile(context.Background())
		r.Reconcile(context.Background())
		if st.opened != 0 {
			t.Fatal("advisory update opened danger episode")
		}
	}
}

func TestAgentSourceUpdateTagText(t *testing.T) {
	for _, tc := range []struct{ latest, severity, summary string }{
		{"v1.10.0+" + strings.Repeat("metadata", 30), sevWarn, "newer tag"},
		{"v1.10.0\n\x1b[31m\u202e", sevOK, "up to date"},
	} {
		set := &fakeSettings{healthEnabled: true, sourceEnabled: true, sourceURL: "https://configured.test/source.git", sourceRef: "v1.2.0",
			sourceStatus: settings.AgentSourceStatus{UpdateCheckedAt: fixedNow.Format(time.RFC3339), LatestRef: tc.latest}}
		svc := sourceTestService(set)
		assertSourceEvaluation(t, svc, tc.severity, tc.summary)
		doc, err := svc.Evaluate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, summary := check(t, doc, "agentsource.update")
		if strings.Contains(summary, strings.Repeat("metadata", 30)) || strings.ContainsAny(summary, "\n\x1b\u202e") {
			t.Fatalf("unsafe or unbounded tag text: %q", summary)
		}
	}
}

func TestAgentSourceUpdatePrecedence(t *testing.T) {
	t.Run("nil settings", func(t *testing.T) {
		assertSourceEvaluation(t, sourceTestService(nil), sevUnknown, "update status unavailable")
	})
	for _, name := range []string{"unchecked", "disabled", "unconfigured", "SHA unchecked", "enabled error", "URL error", "ref error", "status error"} {
		t.Run(name, func(t *testing.T) {
			set := &fakeSettings{healthEnabled: true, sourceEnabled: true, sourceURL: "https://configured.test/source.git"}
			severity, summary := sevUnknown, "not checked yet"
			switch name {
			case "disabled":
				set.sourceEnabled = false
				severity, summary = sevNA, "disabled"
			case "unconfigured":
				set.sourceURL = "  "
				severity, summary = sevNA, "unconfigured"
			case "SHA unchecked":
				set.sourceRef = strings.Repeat("aB", 20)
				severity, summary = sevNA, "immutable SHA"
			default:
				for i, n := range []string{"enabled error", "URL error", "ref error", "status error"} {
					if name == n {
						set.sourceErrors[i] = errors.New("read failed")
						// Read errors win even over disabled/unconfigured/SHA/unchecked.
						set.sourceEnabled = false
						set.sourceURL = ""
						set.sourceRef = strings.Repeat("a", 40)
						summary = "update status unavailable"
					}
				}
			}
			assertSourceEvaluation(t, sourceTestService(set), severity, summary)
		})
	}
}

func TestAgentSourceHealthConfiguredEndpointZeroRequests(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	packet := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
	body := packet("# service=git-upload-pack\n") + "0000" +
		packet(sha+" HEAD\x00symref=HEAD:refs/heads/main\n") +
		packet(sha+" refs/heads/main\n") + packet(sha+" refs/tags/v1.10.0\n") + "0000"
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/source.git/info/refs" || r.URL.RawQuery != "service=git-upload-pack" {
			t.Errorf("unexpected advertisement request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write advertisement: %v", err)
		}
	}))
	defer server.Close()
	url := server.URL + "/source.git"
	refs, err := agentsource.ListRemoteRefs(context.Background(), agentsource.CloneOptions{CloneURL: url, RedirectAllowed: func(string) bool { return true }})
	if err != nil || refs.HeadSHA != sha || refs.Branches["main"] != sha || refs.Tags["v1.10.0"] != sha || requests.Load() != 1 {
		t.Fatalf("calibration: refs=%+v err=%v requests=%d", refs, err, requests.Load())
	}
	requests.Store(0)
	set := &fakeSettings{healthEnabled: true, sourceEnabled: true, sourceURL: url, sourceRef: "v1.2.0", sourceStatus: settings.AgentSourceStatus{UpdateCheckedAt: fixedNow.Format(time.RFC3339), LatestRef: "v1.10.0", RemoteTipSHA: sha}}
	svc := sourceTestService(set)
	for i := 0; i < 3; i++ {
		assertSourceEvaluation(t, svc, sevWarn, "newer tag v1.10.0")
	}
	if requests.Load() != 0 {
		t.Fatalf("Health contacted configured endpoint %d times", requests.Load())
	}
}
