package rosetta

import (
	"errors"
	"net/http"
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
	// Query strings are rejected: endpoints carry their version path only,
	// and a query can hide credentials that would leak into error strings
	// and retry logs verbatim.
	if u.RawQuery != "" {
		return errInvalidEndpoint
	}
	return nil
}

// displayEndpoint renders a raw endpoint for error messages with the query
// string and any userinfo stripped: callers sometimes paste a key into the
// URL, and even a rejected endpoint should not echo that credential back into
// logs.
func displayEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// decorateURL applies the client's path prefix and query parameters to a
// fully-joined request URL. The configured endpoint itself never carries a
// query (see validateEndpoint); these are injected afterwards so a
// credential cannot hide in the endpoint string.
func (c *Client) decorateURL(raw string) string {
	if c.settings.pathPrefix == "" && len(c.settings.queryParams) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if c.settings.pathPrefix != "" {
		u.Path = c.settings.pathPrefix + u.Path
	}
	if len(c.settings.queryParams) > 0 {
		q := u.Query()
		for k, vs := range c.settings.queryParams {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// applyHeaders layers caller-supplied headers onto a built header set. A
// caller header replaces the SDK default of the same name, which is what
// makes gateway auth (x-api-key, api-key) and trace propagation work.
func (c *Client) applyHeaders(h http.Header) {
	for k, vs := range c.settings.headers {
		h.Del(k)
		for _, v := range vs {
			h.Add(k, v)
		}
	}
}

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

// modelListURLs returns the candidate /models endpoint paths for ListModels,
// most likely first. It is deliberately narrower than joinEndpoint (which
// serves every request path): only the model catalog benefits from a fallback.
//
// A domain-root endpoint (no path, or just "/") tries /v1/models first —
// the de-facto standard for OpenAI-family services — and falls back to
// /models. An endpoint that already carries a path (versioned or proxied) is
// used as-is with /models appended, exactly like joinEndpoint.
func (c *Client) modelListURLs() []string {
	base := trimTrailingSlash(c.settings.endpoint)
	if u, err := parseURL(base); err == nil && u.Scheme != "" && u.Host != "" && (u.Path == "" || u.Path == "/") {
		return []string{
			c.decorateURL(base + "/v1/models"),
			c.decorateURL(base + "/models"),
		}
	}
	return []string{c.decorateURL(base + "/models")}
}
