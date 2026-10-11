package runprogress

// MaxIssueRefs bounds how many issue references IssueRefs returns, and so how many
// values the blocked-by lookup binds per array parameter.
const MaxIssueRefs = 5

// maxIssueRefDigits drops absurd numbers before they are parsed, so a hostile
// digit run can never overflow int64 (9 digits is below 1e9).
const maxIssueRefDigits = 9

// IssueRefs extracts the issue numbers a free-text question mentions, for the
// run-detail blocked-by hint (PRD #2602). The text is untrusted model output: only
// the parsed integers leave this function, never any part of the text.
//
// A reference is a '#' immediately followed by a run of ASCII digits; the run is
// taken whole, so "#12abc" is 12. A '#' preceded by a word character still counts
// ("abc#12" is 12): the hint is advisory, so a loose match is acceptable and a
// word-boundary rule would only add edge cases. A run longer than 9 digits is
// dropped, as is zero (including "#000"), self (the run's own issue number; pass 0
// when it has none), and a repeat. Order is first-seen; at most MaxIssueRefs are
// returned across all texts.
func IssueRefs(texts []string, self int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, text := range texts {
		for i := 0; i < len(text); i++ {
			if text[i] != '#' {
				continue
			}
			j := i + 1
			var n int64
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				if j-(i+1) < maxIssueRefDigits {
					n = n*10 + int64(text[j]-'0')
				}
				j++
			}
			digits := j - (i + 1)
			i = j - 1
			if digits == 0 || digits > maxIssueRefDigits || n == 0 || n == self || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
			if len(out) == MaxIssueRefs {
				return out
			}
		}
	}
	return out
}
