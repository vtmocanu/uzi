package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Bodies deliberately use raw JSON rather than the new DTO fields, so this file and the
// fake capture changes can also exercise request acceptance against the pinned baseline.
type findingSendCase struct {
	name           string
	body           string
	rows           []store.AppSetting
	storeErr       error
	ensureFail     bool
	wantStatus     int
	groupStatus    int
	filed, ensured []string
}

func findingSendCases() []findingSendCase {
	marker := findingTestMarker
	defaults := []string{marker, "uzi", "autopilot"}
	custom := []store.AppSetting{
		{Key: settings.KeyFindingLabel, Value: "reported"},
		{Key: settings.KeyUziLabel, Value: "eligible"},
		{Key: settings.KeyAutopilotLabel, Value: "hands-off"},
	}
	capped := []string{marker, "clean", strings.Repeat("a", 64)}
	raw := []string{"", "bad,separator", "clean\u0001", "clean", strings.Repeat("a", 80)}
	for i := 0; i < workersvc.MaxProposalLabels; i++ {
		name := fmt.Sprintf("explicit-%02d", i)
		raw = append(raw, name)
		if len(capped) < workersvc.MaxProposalLabels {
			capped = append(capped, name)
		}
	}
	labels, _ := json.Marshal(raw)
	return []findingSendCase{
		{name: "absent", body: "{}", filed: []string{marker}, ensured: []string{marker}},
		{name: "false", body: `{"send_to_uzi":false}`, filed: []string{marker}, ensured: []string{marker}},
		{name: "false explicit run labels", body: `{"send_to_uzi":false,"labels":["uzi","autopilot","chosen"]}`, filed: []string{marker, "uzi", "autopilot", "chosen"}, ensured: []string{marker}},
		{name: "true missing rows", body: `{"send_to_uzi":true}`, filed: defaults, ensured: defaults},
		{name: "custom settings", body: `{"send_to_uzi":true,"labels":["chosen"]}`, rows: custom,
			filed: []string{"reported", "chosen", "eligible", "hands-off"}, ensured: []string{"reported", "eligible", "hands-off"}},
		{name: "explicit defaults", body: `{"send_to_uzi":true}`, rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: "uzi"}, {Key: settings.KeyAutopilotLabel, Value: "autopilot"}},
			filed: defaults, ensured: defaults},
		{name: "exact explicit collisions", body: `{"send_to_uzi":true,"labels":["autopilot","uzi","agent-found","Uzi","uzi"]}`,
			filed: []string{marker, "autopilot", "uzi", "Uzi"}, ensured: defaults},
		// These rows bypass admin validation intentionally: legacy coincident settings must
		// deduplicate even though ValidateMerged rejects new eligibility collisions.
		{name: "legacy all coincident", body: `{"send_to_uzi":true}`, rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: marker}, {Key: settings.KeyAutopilotLabel, Value: marker}},
			filed: []string{marker}, ensured: []string{marker}},
		{name: "legacy run labels coincident", body: `{"send_to_uzi":true}`, rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: "run"}, {Key: settings.KeyAutopilotLabel, Value: "run"}},
			filed: []string{marker, "run"}, ensured: []string{marker, "run"}},
		{name: "sanitization and cap", body: `{"send_to_uzi":true,"labels":` + string(labels) + "}",
			filed: append(slices.Clone(capped), "uzi", "autopilot"), ensured: defaults},
		{name: "ensure failure", body: `{"send_to_uzi":true}`, ensureFail: true, wantStatus: http.StatusBadGateway, ensured: []string{marker}},
		{name: "cold settings error", body: `{"send_to_uzi":true}`, storeErr: errors.New("settings unavailable"),
			groupStatus: http.StatusInternalServerError, filed: defaults, ensured: defaults},
		{name: "false cold settings error", body: `{"send_to_uzi":false}`, storeErr: errors.New("settings unavailable"),
			groupStatus: http.StatusInternalServerError, filed: []string{marker}, ensured: []string{marker}},
	}
}

func TestFileFindingSendToUziLiveDB(t *testing.T) {
	for _, tc := range findingSendCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, pool, q, box, fs := fileFindingLiveDB(t)
			h.settings = settings.New(&settingsStore{rows: tc.rows, err: tc.storeErr}, time.Minute)
			f := seedFindingFileFixture(context.Background(), t, pool, q, box, fs.server.URL)
			fs.ensureFail = tc.ensureFail
			req := fileFindingReq(f.owner, f.finding.ID, nil)
			req.Body = io.NopCloser(strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			h.FileFinding(rr, req)
			want := tc.wantStatus
			if want == 0 {
				want = http.StatusCreated
			}
			if rr.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, want, rr.Body.String())
			}
			fs.mu.Lock()
			ensured := slices.Clone(fs.ensured)
			seqs := slices.Clone(fs.ensureSeqs)
			issueSeq := fs.firstIssueAt
			creates := slices.Clone(fs.creates)
			fs.mu.Unlock()
			if !slices.Equal(ensured, tc.ensured) {
				t.Errorf("ensured=%v want=%v", ensured, tc.ensured)
			}
			if tc.ensureFail {
				if len(creates) != 0 {
					t.Fatal("ensure failure reached CreateIssue")
				}
				if status, _ := dispositionStatus(context.Background(), t, pool, f.owner.ID, f.repoID, f.location); status != "open" {
					t.Fatalf("claim not reverted: %s", status)
				}
				return
			}
			if len(creates) != 1 {
				t.Fatalf("creates=%d want=1", len(creates))
			}
			if !slices.Equal(creates[0].Labels, tc.filed) {
				t.Errorf("filed=%v want=%v", creates[0].Labels, tc.filed)
			}
			for _, seq := range seqs {
				if seq <= 0 || seq >= issueSeq {
					t.Errorf("ensure sequence=%d create=%d", seq, issueSeq)
				}
			}
		})
	}
}

func TestFileFindingGroupSendToUziLiveDB(t *testing.T) {
	for _, tc := range findingSendCases() {
		t.Run(tc.name, func(t *testing.T) {
			e := newFGEnv(t, nil)
			e.h.settings = settings.New(&settingsStore{rows: tc.rows, err: tc.storeErr}, time.Minute)
			members := e.members(2)
			// Add ids to a raw JSON body without relying on FindingGroupFileRequest.
			ids, _ := json.Marshal(fgIDs(members...))
			body := `{"ids":` + string(ids) + "}"
			if tc.body != "{}" {
				body = `{"ids":` + string(ids) + "," + strings.TrimPrefix(tc.body, "{")
			}
			e.fake.ensureErr = nil
			if tc.ensureFail {
				e.fake.ensureErr = errors.New("ensure refused")
			}
			rr := httptest.NewRecorder()
			e.h.FileFindingGroup(rr, fgRequest(e.owner, http.MethodPost, "/x", json.RawMessage(body), nil))
			want := tc.wantStatus
			if want == 0 {
				want = http.StatusCreated
			}
			if tc.groupStatus != 0 {
				want = tc.groupStatus
			}
			if rr.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, want, rr.Body.String())
			}
			e.fake.mu.Lock()
			ensured := slices.Clone(e.fake.ensured)
			ensureSeq, createSeq := e.fake.ensureS, e.fake.createS
			e.fake.mu.Unlock()
			wantEnsured := tc.ensured
			if tc.ensureFail {
				wantEnsured = []string{findingTestMarker, "uzi", "autopilot"}
			}
			if tc.groupStatus != 0 {
				wantEnsured = nil
			}
			if !slices.Equal(ensured, wantEnsured) {
				t.Errorf("ensured=%v want=%v", ensured, wantEnsured)
			}
			if want != http.StatusCreated {
				if e.fake.createCount() != 0 {
					t.Fatal("failed request reached CreateIssue")
				}
				e.requireOpen(members...)
				if n, _, err := store.FindingGroupPendingStats(e.ctx, e.pool, e.owner.ID); err != nil || n != 0 {
					t.Fatalf("operation not released: pending=%d err=%v", n, err)
				}
				return
			}
			if e.fake.createCount() != 1 {
				t.Fatalf("creates=%d want=1", e.fake.createCount())
			}
			if got := e.fake.create(0).Labels; !slices.Equal(got, tc.filed) {
				t.Errorf("filed=%v want=%v", got, tc.filed)
			}
			if ensureSeq <= 0 || ensureSeq >= createSeq {
				t.Errorf("ensure sequence=%d create=%d", ensureSeq, createSeq)
			}
		})
	}
}
