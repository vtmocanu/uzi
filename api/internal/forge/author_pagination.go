package forge

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// completeAuthorResponse applies only to author evidence reads.
func completeAuthorResponse(resp *http.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusOK && len(resp.Header.Values("Content-Range")) == 0
}

// validateAuthorNextPage rejects pagination metadata the SDK silently fails to
// parse. It affects only author evidence; existing pagination is unchanged.
func validateAuthorNextPage(header string, page, next int) error {
	if page <= 0 || next < 0 || (next != 0 && next <= page) {
		return ErrAuthorUnknown
	}
	pages := make(map[string]int)
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
		seenRel := false
		for _, param := range parts[1:] {
			pair := strings.SplitN(strings.TrimSpace(param), "=", 2)
			if len(pair) != 2 {
				return ErrAuthorUnknown
			}
			if pair[0] != "rel" {
				continue
			}
			if seenRel {
				return ErrAuthorUnknown
			}
			seenRel = true
			rel := strings.TrimSpace(pair[1])
			if strings.HasPrefix(rel, "\"") {
				if len(rel) < 2 || !strings.HasSuffix(rel, "\"") {
					return ErrAuthorUnknown
				}
				rel = rel[1 : len(rel)-1]
			} else if strings.Contains(rel, "\"") {
				return ErrAuthorUnknown
			}
			for _, name := range strings.Fields(rel) {
				switch name {
				case "next", "last", "first", "prev":
				default:
					continue
				}
				if _, exists := pages[name]; exists {
					return ErrAuthorUnknown
				}
				u, err := url.Parse(address[1 : len(address)-1])
				if err != nil {
					return ErrAuthorUnknown
				}
				query, err := url.ParseQuery(u.RawQuery)
				if err != nil || len(query["page"]) != 1 {
					return ErrAuthorUnknown
				}
				raw := query.Get("page")
				if raw == "" || strings.Trim(raw, "0123456789") != "" {
					return ErrAuthorUnknown
				}
				n, err := strconv.Atoi(raw)
				if err != nil || n <= 0 {
					return ErrAuthorUnknown
				}
				pages[name] = n
			}
		}
	}
	if pages["next"] != next {
		return ErrAuthorUnknown
	}
	if last, ok := pages["last"]; ok && (last < page || last < next || (next == 0 && last != page)) {
		return ErrAuthorUnknown
	}
	if first, ok := pages["first"]; ok && first != 1 {
		return ErrAuthorUnknown
	}
	if prev, ok := pages["prev"]; ok && prev >= page {
		return ErrAuthorUnknown
	}
	return nil
}
