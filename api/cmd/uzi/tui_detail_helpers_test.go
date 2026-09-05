package main

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// applyDetail reproduces the retired one-shot detail load (run + full transcript in a single
// message) via the two messages that replaced it (PRD #1137 M4 / D11), so a migrated test keeps
// the exact channel it had: the run DTO AND the transcript frames both land, tailLoaded is set,
// and the pane leaves loading…. Pass the same run/msgs the test used to load; runID is taken
// from run.ID, which every construction site set to the open run's id (a site that deliberately
// mismatched — a stale-run drop test — keeps that mismatch, so the guard still drops the load
// exactly as before).
//
// It takes no *testing.T so the non-test uxlab fixtures (detailBase et al., which have no t in
// scope) can call it too; it never asserts, it only drives the two Update calls.
func applyDetail(m tuiModel, run apitypes.RunDTO, msgs []apitypes.MessageDTO) tuiModel {
	nm, _ := m.Update(detailRunMsg{runID: run.ID, run: run})
	m = nm.(tuiModel)
	nm, _ = m.Update(detailPageMsg{runID: run.ID, kind: pageTail, msgs: msgs})
	return nm.(tuiModel)
}
