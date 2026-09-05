package rosetta

import "net/url"

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}
