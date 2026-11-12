package issuetracker

import "strings"

// Kind names an issue tracker driver. The string is the stored value.
type Kind string

// KindYouTrack is the JetBrains YouTrack driver.
const KindYouTrack Kind = "youtrack"

// SupportedKinds lists every driver, in the order a connect UI would offer them.
func SupportedKinds() []Kind {
	return []Kind{KindYouTrack}
}

// ParseKind returns the driver named by s, or false when s names none.
// Matching is exact.
func ParseKind(s string) (Kind, bool) {
	for _, k := range SupportedKinds() {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}

// SupportedKindsList renders SupportedKinds for an error message.
func SupportedKindsList() string {
	kinds := SupportedKinds()
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}
