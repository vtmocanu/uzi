package ciartifacts

// validReason is the closed source-refusal vocabulary. Upstream errors never
// become persisted strings.
func validReason(reason string) bool {
	switch reason {
	case "missing", "expired", "denied", "unsupported", "malformed", "download_budget",
		"download_unknown", "deadline", "association", "attempt_provenance_unavailable",
		"scrubber_unavailable", "artifact_limit", "archive_safety", "inflated_budget":
		return true
	}
	return false
}

// Unavailable records a bounded archive/source refusal. Summaries do not count
// as skipped files. Essential reasons survive ordinary row and render clipping.
func (c *Collector) Unavailable(id, name, reason string) {
	if !validReason(reason) {
		reason = "malformed"
	}
	found := false
	for _, r := range c.manifest.Reasons {
		if r == reason {
			found = true
		}
	}
	if !found && len(c.manifest.Reasons) < 20 {
		c.manifest.Reasons = append(c.manifest.Reasons, reason)
	}
	c.manifest.Uninspected = true
	if len(c.artifacts) >= 1000 {
		return
	}
	if id == "" {
		id = "source"
	}
	// Normalize validates IDs without rewriting them. Invalid metadata carries
	// only the source refusal, never an invalid durable identity.
	name, short := boundedMetadata(c.scrub(name), 200)
	e := Entry{ArtifactID: id, ArtifactName: name, EntryOrdinal: 1001, Shortened: short, Status: "skipped", Reason: reason, Summary: true}
	if _, err := (Manifest{Entries: []Entry{e}}).Normalize(); err != nil {
		return
	}
	if c.artifacts[id] {
		return
	}
	c.artifacts[id] = true
	c.record(e)
}
