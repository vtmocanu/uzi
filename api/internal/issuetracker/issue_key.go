package issuetracker

import "strconv"

// IssueKey is the readable key of an issue in an issue tracker project: the project short
// name, a dash and the issue number ("ABC-12"). The claim assembly, the API payloads and
// the YouTrack driver build the key with it. The youtracktest fake keeps its own copy,
// because importing this package from it would be a test import cycle.
func IssueKey(projectShort string, number int64) string {
	return projectShort + "-" + strconv.FormatInt(number, 10)
}
