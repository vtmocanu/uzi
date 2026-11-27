package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
)

// The state-machine refusal is also what forgesvc.ColumnMoveRefused matches, so its case must
// come first: a 409 with the project named, never the "map this column" hint.
func TestWriteLinkedMoveErrorStateMachineIs409WithTheProjectNamed(t *testing.T) {
	err := fmt.Errorf("move: %w", issuetracker.ErrStateMachineField)
	if !forgesvc.ColumnMoveRefused(err) {
		t.Fatal("precondition: ColumnMoveRefused must also match the state-machine refusal")
	}
	rr := httptest.NewRecorder()
	if !writeLinkedMoveError(rr, err, "UZI") {
		t.Fatal("the error was not answered")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal(rr.Body.Bytes(), &body); jerr != nil {
		t.Fatal(jerr, rr.Body.String())
	}
	want := "project UZI uses a state-machine workflow on its State field; uzi needs a plain State field. " +
		"Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack."
	if body.Error != want || body.Error != issuetrackersvc.StateMachineRefusal("UZI") {
		t.Fatalf("message = %q, want %q", body.Error, want)
	}
}

// The unreadable State field refusal is also what forgesvc.ColumnMoveRefused matches, so its
// case must come before it: a 409 with its own text, never the state-machine or "map this
// column" text.
func TestWriteLinkedMoveErrorUnreadableStateFieldIs409WithItsOwnText(t *testing.T) {
	err := fmt.Errorf("move: %w", fmt.Errorf("%w: issue %q", issuetracker.ErrStateFieldUnreadable, "UZI-3"))
	if !forgesvc.ColumnMoveRefused(err) {
		t.Fatal("precondition: ColumnMoveRefused must also match the unreadable State field refusal")
	}
	rr := httptest.NewRecorder()
	if !writeLinkedMoveError(rr, err, "UZI") {
		t.Fatal("the error was not answered")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal(rr.Body.Bytes(), &body); jerr != nil {
		t.Fatal(jerr, rr.Body.String())
	}
	if body.Error != issuetrackersvc.StateFieldUnreadableRefusal("UZI", err) {
		t.Fatalf("message = %q", body.Error)
	}
	if body.Error == issuetrackersvc.StateMachineRefusal("UZI") || strings.Contains(body.Error, "map this column") {
		t.Fatalf("answered with another refusal's text: %q", body.Error)
	}
	if !strings.Contains(body.Error, "could not read project UZI") || !strings.Contains(body.Error, "UZI-3") {
		t.Fatalf("message = %q", body.Error)
	}
}
