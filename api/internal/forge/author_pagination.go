package forge

import (
	"net/url"
	"strconv"
	"strings"
)

// validateAuthorNextPage rejects pagination metadata the SDK silently fails to
// parse. It affects only author evidence; existing pagination is unchanged.
func validateAuthorNextPage(header string, page, next int) error {
	if next != 0 && next <= page {
		return ErrAuthorUnknown
	}
	found := false
	for _, link := range strings.Split(header, ",") {
		link = strings.TrimSpace(link)
		if link == "" {
			continue
		}
		parts := strings.Split(link, ";")
		address := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(address, "<") || !strings.HasSuffix(address, ">") {
			return ErrAuthorUnknown
		}
		for _, param := range parts[1:] {
			pair := strings.SplitN(strings.TrimSpace(param), "=", 2)
			if len(pair) != 2 {
				return ErrAuthorUnknown
			}
			if pair[0] != "rel" {
				continue
			}
			rel := strings.Trim(pair[1], "\"")
			for _, name := range strings.Fields(rel) {
				if name != "next" {
					continue
				}
				if found {
					return ErrAuthorUnknown
				}
				found = true
				u, err := url.Parse(address[1 : len(address)-1])
				if err != nil {
					return ErrAuthorUnknown
				}
				n, err := strconv.Atoi(u.Query().Get("page"))
				if err != nil || n <= page || n != next {
					return ErrAuthorUnknown
				}
			}
		}
	}
	if next != 0 && !found {
		return ErrAuthorUnknown
	}
	return nil
}
