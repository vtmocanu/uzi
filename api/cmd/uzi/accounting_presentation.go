package main

import "encoding/json"

// isAccountingMessage identifies retained accounting evidence that has no human
// presentation. Kind and event must both match; ordinary status messages remain visible.
func isAccountingMessage(kind string, payload json.RawMessage) bool {
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
