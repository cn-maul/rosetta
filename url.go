package rosetta

import (
	"errors"
	"net/url"
	"strings"
)

var errInvalidEndpoint = errors.New("rosetta: invalid endpoint URL")

func validateEndpoint(raw string) error {
	if strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errInvalidEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return errInvalidEndpoint
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return errInvalidEndpoint
	}
	return nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

func joinEndpoint(base, path string) string {
	b := trimTrailingSlash(base)
	if u, err := parseURL(b); err == nil && u.Opaque == "" && u.Scheme != "" && u.Host != "" {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/v1"
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + path
		return u.String()
	}
	if u, err := parseURL(b); err == nil && (u.Path == "" || u.Path == "/") {
		b += "/v1"
	}
	return b + path
}
