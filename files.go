package rosetta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/cn-maul/rosetta/internal/httpx"
)

// maxUploadBytes bounds an in-memory upload. Multipart needs a known body
// length, so the file is buffered; this cap keeps a mistaken multi-GB
// reader from exhausting memory.
const maxUploadBytes = 32 << 20

// anthropicFilesBeta gates Anthropic's Files API.
const anthropicFilesBeta = "files-api-2025-04-14"

// File is a provider-side uploaded file. OpenAI and Anthropic spell the
// metadata slightly differently; File normalizes both.
type File struct {
	ID        string
	Object    string // OpenAI "object"; Anthropic "type"
	Bytes     int64
	CreatedAt time.Time
	Filename  string
	Purpose   string
	MimeType  string
}

// UploadFile uploads a file through the provider's Files API and returns
// the reference to pass as Block.FileID. purpose is OpenAI-specific
// ("assistants", "batch", "vision", "fine-tune"); Anthropic ignores it.
// The reader is buffered in memory, capped at 32 MiB.
func (c *Client) UploadFile(ctx context.Context, r io.Reader, filename, purpose string) (*File, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil file reader", ErrInvalidRequest)
	}
	if filename == "" {
		return nil, fmt.Errorf("%w: filename is required", ErrInvalidRequest)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading upload: %v", ErrInvalidRequest, err)
	}
	if len(data) > maxUploadBytes {
		return nil, fmt.Errorf("%w: upload exceeds %d bytes", ErrInvalidRequest, maxUploadBytes)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("%w: building multipart: %v", ErrInvalidRequest, err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("%w: writing multipart: %v", ErrInvalidRequest, err)
	}
	if purpose != "" {
		if err := mw.WriteField("purpose", purpose); err != nil {
			return nil, fmt.Errorf("%w: writing purpose: %v", ErrInvalidRequest, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("%w: closing multipart: %v", ErrInvalidRequest, err)
	}

	hdr := c.filesHeaders(mw.FormDataContentType())
	u := c.decorateURL(joinEndpoint(c.settings.endpoint, "/files"))
	out, err := c.doFilesJSON(ctx, http.MethodPost, u, hdr, body.Bytes())
	if err != nil {
		return nil, err
	}
	var w fileWire
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, fmt.Errorf("%w: files response: %w", ErrUpstreamMalformed, err)
	}
	f := w.file()
	return &f, nil
}

// ListFiles returns the files visible to this credential.
func (c *Client) ListFiles(ctx context.Context) ([]File, error) {
	u := c.decorateURL(joinEndpoint(c.settings.endpoint, "/files"))
	out, err := c.doFilesJSON(ctx, http.MethodGet, u, c.filesHeaders("application/json"), nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []fileWire `json:"data"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("%w: files list: %w", ErrUpstreamMalformed, err)
	}
	files := make([]File, 0, len(list.Data))
	for _, w := range list.Data {
		files = append(files, w.file())
	}
	return files, nil
}

// GetFile fetches one file's metadata.
func (c *Client) GetFile(ctx context.Context, id string) (*File, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: file id is required", ErrInvalidRequest)
	}
	u := c.decorateURL(joinEndpoint(c.settings.endpoint, "/files/"+url.PathEscape(id)))
	out, err := c.doFilesJSON(ctx, http.MethodGet, u, c.filesHeaders("application/json"), nil)
	if err != nil {
		return nil, err
	}
	var w fileWire
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, fmt.Errorf("%w: file response: %w", ErrUpstreamMalformed, err)
	}
	f := w.file()
	return &f, nil
}

// DeleteFile removes a file. A 200 or 204 both count as success; the body
// differs between providers and carries nothing the caller needs.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: file id is required", ErrInvalidRequest)
	}
	u := c.decorateURL(joinEndpoint(c.settings.endpoint, "/files/"+url.PathEscape(id)))
	call := &httpx.Call{
		Method: http.MethodDelete,
		URL:    u,
		Header: c.filesHeaders("application/json"),
	}
	resp, err := c.call(ctx, call)
	if err != nil {
		return transport(err, http.MethodDelete, u)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if rerr != nil {
		return transport(rerr, http.MethodDelete, u)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return withRetryAfter(c.parseFilesError(resp.StatusCode, body, http.MethodDelete, u, resp.Header), resp.Header)
	}
	return nil
}

// filesHeaders builds the auth headers for a Files call. The Content-Type
// is supplied by the caller (multipart for upload, JSON otherwise).
func (c *Client) filesHeaders(contentType string) http.Header {
	var h http.Header
	if p, ok := c.provider.(*anthropicProvider); ok {
		h = p.headers(false)
		// The Files API is beta on Anthropic and needs its own flag on top
		// of anything the caller configured.
		beta := anthropicFilesBeta
		if extra := p.c.settings.betas; len(extra) > 0 {
			for _, b := range extra {
				beta += "," + b
			}
		}
		h.Set("anthropic-beta", beta)
	} else {
		h = openAIHeaders(c, "application/json")
	}
	h.Set("Content-Type", contentType)
	return h
}

// doFilesJSON issues a Files call and returns the raw 2xx body.
func (c *Client) doFilesJSON(ctx context.Context, method, u string, hdr http.Header, body []byte) ([]byte, error) {
	call := &httpx.Call{Method: method, URL: u, Header: hdr}
	if body != nil {
		call.Body = func() ([]byte, error) { return body, nil }
	}
	resp, err := c.call(ctx, call)
	if err != nil {
		return nil, transport(err, method, u)
	}
	defer resp.Body.Close()
	out, rerr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if rerr != nil {
		return nil, transport(rerr, method, u)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, withRetryAfter(c.parseFilesError(resp.StatusCode, out, method, u, resp.Header), resp.Header)
	}
	return out, nil
}

func (c *Client) parseFilesError(status int, body []byte, method, url string, h http.Header) *APIError {
	if _, ok := c.provider.(*anthropicProvider); ok {
		return parseAnthropicError(status, body, method, url, requestID(h))
	}
	return parseOpenAIError(status, body, method, url, h.Get("X-Request-Id"))
}

// fileWire accepts both providers' metadata shapes.
type fileWire struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Type      string `json:"type"`
	Bytes     int64  `json:"bytes"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	MimeType  string `json:"mime_type"`
}

func (w fileWire) file() File {
	f := File{
		ID:       w.ID,
		Object:   w.Object,
		Bytes:    w.Bytes,
		Filename: w.Filename,
		Purpose:  w.Purpose,
		MimeType: w.MimeType,
	}
	if f.Object == "" {
		f.Object = w.Type
	}
	if f.Bytes == 0 {
		f.Bytes = w.SizeBytes
	}
	if w.CreatedAt > 0 {
		f.CreatedAt = time.Unix(w.CreatedAt, 0)
	}
	return f
}
