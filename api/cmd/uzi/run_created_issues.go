package main

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func createdIssueRows(issues []apitypes.CreatedIssueDTO) [][]string {
	rows := make([][]string, 0, len(issues))
	for _, issue := range issues {
		cell := "Created issue #" + strconv.FormatInt(issue.IID, 10)
		if title := cellText(issue.Title); title != "" {
			cell += " " + title
		}
		// Validate the original URL before cellText can strip unsafe characters.
		if safeCreatedIssueURL(issue.WebURL) {
			cell += " " + cellText(issue.WebURL)
		}
		rows = append(rows, []string{"CREATED_ISSUE", cell})
	}
	return rows
}

// safeCreatedIssueURL checks supplied display metadata locally. It neither
// reconstructs an issue URL nor adds terminal hyperlink escape sequences.
func safeCreatedIssueURL(raw string) bool {
	unsafe := func(s string) bool {
		return !utf8.ValidString(s) || strings.ContainsRune(s, '\\') ||
			strings.ContainsFunc(s, func(r rune) bool {
				return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
			})
	}
	if unsafe(raw) || strings.ContainsAny(raw, "?#") || strings.ContainsFunc(raw, unicode.IsSpace) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	authority := host
	if ip := net.ParseIP(host); ip != nil {
		if strings.ContainsRune(host, ':') {
			authority = "[" + host + "]"
		}
	} else {
		dns := strings.TrimSuffix(host, ".")
		if len(dns) > 253 {
			return false
		}
		for _, label := range strings.Split(dns, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
					return false
				}
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		authority += ":" + port
	}
	// Reject malformed brackets, unbracketed IPv6, and empty ports.
	if u.Host != authority {
		return false
	}
	path := u.EscapedPath()
	if path == "" || !strings.HasPrefix(path, "/") {
		return false
	}
	// One decoding pass per segment preserves valid escaping, while rejecting
	// separators, traversal, controls, and percent recursion in the decoded text.
	for _, segment := range strings.Split(strings.TrimSuffix(path[1:], "/"), "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." ||
			strings.ContainsAny(decoded, "/\\%") || unsafe(decoded) {
			return false
		}
	}
	return true
}
