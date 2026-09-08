package rosetta

import (
	"net/url"
	"strings"
)

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

// joinEndpoint appends an API path to the configured base URL. A base
// without a path (bare host) gets "/v1" inserted, matching OpenAI-style
// versioning; explicit version paths are preserved; a query string on the
// base survives at the end of the joined URL.
func joinEndpoint(base, path string) string {
	b := trimTrailingSlash(base)
	if u, err := parseURL(b); err == nil && u.Opaque == "" && u.Scheme != "" && u.Host != "" {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/v1"
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + path
		return u.String()
	}
	// Malformed or opaque bases (e.g. "localhost:11434") keep the legacy
	// string concatenation.
	if u, err := parseURL(b); err == nil && (u.Path == "" || u.Path == "/") {
		b += "/v1"
	}
	return b + path
}
