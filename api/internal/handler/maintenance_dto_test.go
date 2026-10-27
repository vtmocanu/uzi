package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// User-authored owner labels and repo paths reach the admin readout raw from the database;
// the DTO boundary must strip terminal control sequences, bidi overrides and newlines.
func TestMaintenanceDTOSanitizesUserAuthoredText(t *testing.T) {
	hostile := "Eve\x1b[2J\x1b[1;1Hroot\u202eKCAPS\nforged-row"
	repo := "evil\x1b]0;pwned\x07/re\u202epo\nx"
	item := store.MaintenanceItem{RunID: uuid.New(), Kind: "issue", Status: "running", OwnerLabel: hostile, Repo: &repo}
	section := store.MaintenanceSection{Count: 1, Items: []store.MaintenanceItem{item}}
	m := store.MaintenanceReadout{Active: section, Held: section, Custody: section, Queued: section,
		IdleBoundWorkers: store.MaintenanceWorkers{Count: 1, Items: []store.MaintenanceWorker{
			{WorkerID: uuid.New(), OwnerLabel: hostile, RunID: uuid.New()}}}}
	var st store.IntakePauseState
	st.ByLabel = pgtype.Text{String: hostile, Valid: true}
	b, err := json.Marshal(apitypes.MaintenanceAdminDTO{
		State: maintenanceStateDTO(st), Readout: maintenanceReadoutDTO(m)})
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal escapes control bytes as \u001b etc., so check the decoded strings.
	type labeled struct {
		OwnerLabel string `json:"owner_label"`
		Repo       string `json:"repo"`
	}
	var back struct {
		State struct {
			ByLabel string `json:"by_label"`
		} `json:"state"`
		Readout struct {
			Active struct {
				Items []labeled `json:"items"`
			} `json:"active"`
			Idle struct {
				Items []labeled `json:"items"`
			} `json:"idle_bound_workers"`
		} `json:"readout"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	vals := []string{back.State.ByLabel, back.Readout.Active.Items[0].OwnerLabel, back.Readout.Active.Items[0].Repo,
		back.Readout.Idle.Items[0].OwnerLabel}
	for _, v := range vals {
		if v == "" || strings.ContainsAny(v, "\x1b\x07\n\r\u202e") {
			t.Fatalf("unsanitized or empty value %q", v)
		}
	}
	if !strings.HasPrefix(back.Readout.Active.Items[0].OwnerLabel, "Eve") {
		t.Fatalf("legitimate text lost: %q", back.Readout.Active.Items[0].OwnerLabel)
	}
}
