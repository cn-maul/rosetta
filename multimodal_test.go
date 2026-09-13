package rosetta

import (
	"strings"
	"testing"
)

// OpenAI Chat: audio and file blocks become input_audio / file content
// parts; a text-only message still sends a plain string content.
func TestOpenAIChatMultimodalBlocks(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)
	st := p.initialState()

	req := &ChatRequest{
		Model: "gpt-4o-audio-preview",
		Messages: []Message{{
			Role: RoleUser,
			Blocks: []Block{
				{Type: BlockText, Text: "what do you hear?"},
				AudioContent("QUJD", "mp3"),
				FileContent("report.pdf", "application/pdf", "SkpL"),
				FileRef("upload.pdf", "file-123"),
			},
		}},
	}
	pl, err := p.buildPayload(req, false, st)
	if err != nil {
		t.Fatal(err)
	}
	msgs := pl["messages"].([]map[string]any)
	parts := msgs[0]["content"].([]map[string]any)
	if parts[0]["type"] != "text" {
		t.Fatalf("part0 = %v", parts[0])
	}
	audio := parts[1]["input_audio"].(map[string]any)
	if parts[1]["type"] != "input_audio" || audio["data"] != "QUJD" || audio["format"] != "mp3" {
		t.Fatalf("audio part = %v", parts[1])
	}
	file := parts[2]["file"].(map[string]any)
	if parts[2]["type"] != "file" || file["file_data"] != "data:application/pdf;base64,SkpL" || file["filename"] != "report.pdf" {
		t.Fatalf("file part = %v", parts[2])
	}
	ref := parts[3]["file"].(map[string]any)
	if ref["file_id"] != "file-123" {
		t.Fatalf("file ref = %v", parts[3])
	}

	// data: URLs pass through untouched.
	req.Messages = []Message{{Role: RoleUser, Blocks: []Block{FileContent("f.txt", "text/plain", "data:text/plain;base64,SGk=")}}}
	pl, err = p.buildPayload(req, false, st)
	if err != nil {
		t.Fatal(err)
	}
	parts = pl["messages"].([]map[string]any)[0]["content"].([]map[string]any)
	if got := parts[0]["file"].(map[string]any)["file_data"]; got != "data:text/plain;base64,SGk=" {
		t.Fatalf("data url = %v", got)
	}
}

func TestOpenAIChatMultimodalBlockErrors(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)
	st := p.initialState()
	base := func(b Block) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{b}}}}
	}
	if _, err := p.buildPayload(base(AudioContent("QQ==", "ogg")), false, st); err == nil || !strings.Contains(err.Error(), "wav or mp3") {
		t.Fatalf("bad format: err = %v", err)
	}
	if _, err := p.buildPayload(base(AudioContent("", "wav")), false, st); err == nil {
		t.Fatal("empty audio data accepted")
	}
	// http(s) URLs are not valid file_data on the OpenAI protocols.
	if _, err := p.buildPayload(base(FileContent("d.pdf", "application/pdf", "https://example.test/d.pdf")), false, st); err == nil {
		t.Fatal("http url file accepted")
	}
	// Empty file blocks are caught before a request is built.
	if _, err := p.buildPayload(base(FileContent("", "", "")), false, st); err == nil {
		t.Fatal("empty file accepted")
	}
}

// Responses: file blocks map to input_file parts (file_data for inline
// content, file_url for http(s) URLs, file_id for uploaded refs); audio is
// NOT part of the official Responses input content union and is rejected.
func TestResponsesMultimodalBlocks(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)

	// Audio must be rejected: the official input content union carries
	// text, image and file parts only.
	audioReq := &ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{{
			Role:   RoleUser,
			Blocks: []Block{{Type: BlockText, Text: "listen"}, AudioContent("QUJD", "wav")},
		}},
	}
	if _, err := p.buildPayload(audioReq, false, p.initialState("gpt-4o")); err == nil {
		t.Fatal("responses must reject audio input")
	}

	req := &ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{{
			Role: RoleUser,
			Blocks: []Block{
				{Type: BlockText, Text: "read"},
				FileContent("report.pdf", "", "SkpL"),
				FileRef("up.pdf", "file-9"),
				FileContent("web.pdf", "application/pdf", "https://example.test/w.pdf"),
			},
		}},
	}
	pl, err := p.buildPayload(req, false, p.initialState("gpt-4o"))
	if err != nil {
		t.Fatal(err)
	}
	items := pl["input"].([]map[string]any)
	parts := items[0]["content"].([]map[string]any)
	file := parts[1]
	if file["type"] != "input_file" || file["filename"] != "report.pdf" {
		t.Fatalf("file part = %v", file)
	}
	// Empty MimeType defaults to application/pdf in the data URL.
	if got := file["file_data"]; got != "data:application/pdf;base64,SkpL" {
		t.Fatalf("file_data = %v", got)
	}
	if parts[2]["file_id"] != "file-9" || parts[2]["type"] != "input_file" {
		t.Fatalf("file ref = %v", parts[2])
	}
	// http(s) URLs ride in file_url (official ResponseInputFileParam).
	if parts[3]["file_url"] != "https://example.test/w.pdf" || parts[3]["type"] != "input_file" {
		t.Fatalf("file url = %v", parts[3])
	}
}

// Anthropic: files become document blocks (base64, url or file source);
// audio is rejected outright.
func TestAnthropicDocumentBlocks(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	build := func(blocks ...Block) (map[string]any, error) {
		req := &ChatRequest{Model: "claude-sonnet-4-5", Messages: []Message{{Role: RoleUser, Blocks: blocks}}}
		pl, err := p.buildPayload(req, false, p.plan(req))
		if err != nil {
			return nil, err
		}
		return pl, nil
	}

	pl, err := build(FileContent("report.pdf", "application/pdf", "SkpL"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := pl["messages"].([]map[string]any)
	doc := msgs[0]["content"].([]map[string]any)[0]
	if doc["type"] != "document" || doc["title"] != "report.pdf" {
		t.Fatalf("doc = %v", doc)
	}
	src := doc["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "application/pdf" || src["data"] != "SkpL" {
		t.Fatalf("source = %v", src)
	}

	// URL source.
	pl, err = build(FileContent("", "application/pdf", "https://example.test/d.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	src = pl["messages"].([]map[string]any)[0]["content"].([]map[string]any)[0]["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "https://example.test/d.pdf" {
		t.Fatalf("url source = %v", src)
	}

	// Uploaded-file source.
	pl, err = build(FileRef("", "file-abc"))
	if err != nil {
		t.Fatal(err)
	}
	src = pl["messages"].([]map[string]any)[0]["content"].([]map[string]any)[0]["source"].(map[string]any)
	if src["type"] != "file" || src["file_id"] != "file-abc" {
		t.Fatalf("file source = %v", src)
	}

	// Audio never reaches the wire on this protocol.
	if _, err := build(AudioContent("QUJD", "wav")); err == nil || !strings.Contains(err.Error(), "audio") {
		t.Fatalf("audio: err = %v", err)
	}
}

// UserAudio/UserFile compose text with audio/file blocks.
func TestUserMultimodalConstructors(t *testing.T) {
	am := UserAudio("hi", "QQ==", "wav")
	if len(am.Blocks) != 2 || am.Blocks[1].Type != BlockAudio || am.Blocks[1].AudioData != "QQ==" || am.Blocks[1].AudioFormat != "wav" {
		t.Fatalf("UserAudio = %+v", am)
	}
	am = UserAudio("", "QQ==", "mp3")
	if len(am.Blocks) != 1 {
		t.Fatalf("UserAudio textless = %+v", am)
	}
	fm := UserFile("read", "d.pdf", "application/pdf", "SkpL")
	if len(fm.Blocks) != 2 || fm.Blocks[1].Type != BlockFile || fm.Blocks[1].FileName != "d.pdf" ||
		fm.Blocks[1].MimeType != "application/pdf" || fm.Blocks[1].FileData != "SkpL" {
		t.Fatalf("UserFile = %+v", fm)
	}
}

func TestEstimateMultimodalBlocks(t *testing.T) {
	textOnly := &ChatRequest{Messages: []Message{User("hello")}}
	withAudio := &ChatRequest{Messages: []Message{{Role: RoleUser, Blocks: []Block{AudioContent("QQ==", "wav")}}}}
	withFile := &ChatRequest{Messages: []Message{{Role: RoleUser, Blocks: []Block{FileContent("d.pdf", "application/pdf", "SkpL")}}}}
	if withAudio.estimateInputTokens(MultimediaTokenEstimates{}) <= textOnly.estimateInputTokens(MultimediaTokenEstimates{}) {
		t.Fatal("audio not counted")
	}
	if withFile.estimateInputTokens(MultimediaTokenEstimates{}) <= textOnly.estimateInputTokens(MultimediaTokenEstimates{}) {
		t.Fatal("file not counted")
	}
}
