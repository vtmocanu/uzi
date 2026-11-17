package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// writeNeverEnabled answers the typed refusal for an operation on a repo that has
// never been enabled (PRD 4173 D13): 409 with a message that says what to do. The
// body is the fixed sentinel text, never a database error.
func writeNeverEnabled(w http.ResponseWriter) {
	httpx.Error(w, http.StatusConflict, store.ErrRepoNeverEnabled.Error())
}

// refuseNeverEnabled writes the 409 and returns true when the repo row's
// AwaitingFirstEnable marker is set. Handlers call it right after loading the repo
// row, before any forge or issue tracker call, so the database guard never strands
// an issue the forge already created.
func refuseNeverEnabled(w http.ResponseWriter, awaitingFirstEnable bool) bool {
	if store.RefuseNeverEnabled(awaitingFirstEnable) == nil {
		return false
	}
	writeNeverEnabled(w)
	return true
}

// writeIfNeverEnabled writes the 409 and returns true when err is the typed refusal
// or carries the issue-number guard's SQLSTATE (store.MapNeverEnabled), so a handler
// that would otherwise answer 500 answers 409.
func writeIfNeverEnabled(w http.ResponseWriter, err error) bool {
	if !errors.Is(store.MapNeverEnabled(err), store.ErrRepoNeverEnabled) {
		return false
	}
	writeNeverEnabled(w)
	return true
}
