package main

import "encoding/json"

// isAccountingMessage identifies retained messages that have no transcript presentation:
// accounting evidence (a codex_response_usage status; kind and event must both match) and the
// progress_note kind, which is surfaced on the PROGRESS block instead. Ordinary status messages
// remain visible.
func isAccountingMessage(kind string, payload json.RawMessage) bool {
	// progress_note is the model-written Now summary (PRD #2603): it is surfaced on the
	// PROGRESS block, never as a transcript row. JSON mode still carries it.
	if kind == "progress_note" {
		return true
	}
	if kind != "status" {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return false
	}
	var event string
	return json.Unmarshal(fields["event"], &event) == nil && event == "codex_response_usage"
}

// presentationFrames leaves the raw log and its sequence cursors intact. The
// returned slice contains only frames that can contribute lanes or visible activity.
func presentationFrames(frames []laneFrame) []laneFrame {
	var visible []laneFrame
	for _, f := range frames {
		if !isAccountingMessage(f.Kind, f.Payload) {
			visible = append(visible, f)
		}
	}
	return visible
}
