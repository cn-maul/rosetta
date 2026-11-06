package rosetta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RG-14: upload/list/get/delete round-trip, with multipart and metadata
// normalization.
func TestFilesAPI(t *testing.T) {
	var gotPurpose, gotFilename, gotContentType, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			gotPurpose = r.FormValue("purpose")
			file, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("form file: %v", err)
			} else {
				defer file.Close()
				gotFilename = hdr.Filename
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"file-1","object":"file","bytes":5,"created_at":1700000000,"filename":"a.txt","purpose":"batch"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			io.WriteString(w, `{"data":[{"id":"file-1","bytes":5,"filename":"a.txt"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file-1":
			io.WriteString(w, `{"id":"file-1","bytes":5,"filename":"a.txt"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/files/file-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	f, err := c.UploadFile(ctx, strings.NewReader("hello"), "a.txt", "batch")
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "file-1" || f.Bytes != 5 || f.Filename != "a.txt" || f.Purpose != "batch" {
		t.Fatalf("file = %+v", f)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/files" {
		t.Fatalf("upload request = %s %s", gotMethod, gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotPurpose != "batch" || gotFilename != "a.txt" {
		t.Fatalf("purpose=%q filename=%q", gotPurpose, gotFilename)
	}

	files, err := c.ListFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != "file-1" {
		t.Fatalf("files = %+v", files)
	}

	one, err := c.GetFile(ctx, "file-1")
	if err != nil {
		t.Fatal(err)
	}
	if one.ID != "file-1" {
		t.Fatalf("get = %+v", one)
	}

	if err := c.DeleteFile(ctx, "file-1"); err != nil {
		t.Fatal(err)
	}
}

// Anthropic requires its Files beta flag and uses size_bytes/mime_type.
func TestAnthropicFiles(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart: %v", err)
		}
		io.WriteString(w, `{"id":"file-2","type":"file","size_bytes":9,"created_at":1700000000,"filename":"b.pdf","mime_type":"application/pdf"}`)
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoAnthropic))
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.UploadFile(context.Background(), strings.NewReader("pdfbytes"), "b.pdf", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBeta, anthropicFilesBeta) {
		t.Fatalf("anthropic-beta = %q", gotBeta)
	}
	if f.Bytes != 9 || f.MimeType != "application/pdf" || f.Object != "file" {
		t.Fatalf("normalized file = %+v", f)
	}
}

func TestUploadTooLarge(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	_, err := c.UploadFile(context.Background(), strings.NewReader(strings.Repeat("x", maxUploadBytes+1)), "big.bin", "batch")
	if err == nil {
		t.Fatal("oversized upload must fail locally")
	}
}
