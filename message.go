package rosetta

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Role is a conversation speaker, unified across protocols.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool marks a message carrying tool results back to the model.
	// Anthropic receives these as tool_result blocks inside a user turn;
	// the adapter performs that rewrite.
	RoleTool Role = "tool"
)

// BlockType enumerates unified content block kinds.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockAudio      BlockType = "audio"       // inline audio input (OpenAI protocols only)
	BlockFile       BlockType = "file"        // document/file input (PDF etc.)
	BlockToolCall   BlockType = "tool_call"   // model requesting a tool
	BlockToolResult BlockType = "tool_result" // result fed back to the model
	BlockThinking   BlockType = "thinking"    // chain-of-thought content
	// BlockRedactedThinking is Anthropic's server-redacted reasoning: an
	// opaque payload that must be replayed unchanged to keep a thinking
	// conversation intact. It rides in a block's Thinking field.
	BlockRedactedThinking BlockType = "redacted_thinking"
)

// Block is one piece of message content. Which fields are meaningful
// depends on Type:
//
//   - BlockText: Text
//   - BlockImage: ImageURL (http(s) or data: URL)
//   - BlockAudio: AudioData (raw base64), AudioFormat ("wav"|"mp3");
//     supported by the OpenAI protocols, rejected by Anthropic
//   - BlockFile: FileData (raw base64, data: URL or http(s) URL) or
//     FileID (uploaded reference), plus FileName and MimeType
//   - BlockToolCall: ToolCallID, ToolName, Arguments (raw JSON string)
//   - BlockToolResult: ToolCallID, ToolName, Content (text payload), IsError
//   - BlockThinking: Thinking, Signature (Anthropic passthrough)
type Block struct {
	Type        BlockType
	Text        string
	ImageURL    string
	AudioData   string // base64 payload without the data: prefix
	AudioFormat string // "wav" or "mp3"
	FileName    string
	MimeType    string // e.g. "application/pdf"; derived when empty
	FileData    string // raw base64, data: URL or http(s) URL
	FileID      string // provider file id (uploaded reference)
	ToolCallID  string
	ToolName    string
	Arguments   string
	Content     string
	IsError     bool
	Thinking    string
	Signature   string
	// CacheControl marks an Anthropic prompt-cache breakpoint after this
	// block (text, image, document, tool_result, tool_use). Ignored by the
	// OpenAI protocols, which cache prefixes automatically.
	CacheControl *CacheControl
}

// Message is one conversation turn composed of content blocks.
type Message struct {
	Role   Role
	Blocks []Block
}

// System builds a system message.
func System(text string) Message {
	return Message{Role: RoleSystem, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// User builds a user text message.
func User(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// UserImage builds a user message combining optional text with an image
// referenced by URL (http(s) or data:).
func UserImage(text, imageURL string) Message {
	var blocks []Block
	if text != "" {
		blocks = append(blocks, Block{Type: BlockText, Text: text})
	}
	blocks = append(blocks, Block{Type: BlockImage, ImageURL: imageURL})
	return Message{Role: RoleUser, Blocks: blocks}
}

// UserFile builds a user message combining optional text with a document.
// data is raw base64 (no data: prefix), a data: URL or — on Anthropic — an
// http(s) URL; mediaType is e.g. "application/pdf".
func UserFile(text, name, mediaType, data string) Message {
	var blocks []Block
	if text != "" {
		blocks = append(blocks, Block{Type: BlockText, Text: text})
	}
	blocks = append(blocks, FileContent(name, mediaType, data))
	return Message{Role: RoleUser, Blocks: blocks}
}

// UserAudio builds a user message combining optional text with inline
// audio. data is raw base64; format must be "wav" or "mp3". Supported by
// the OpenAI protocols; Anthropic rejects audio input.
func UserAudio(text, base64Data, format string) Message {
	var blocks []Block
	if text != "" {
		blocks = append(blocks, Block{Type: BlockText, Text: text})
	}
	blocks = append(blocks, AudioContent(base64Data, format))
	return Message{Role: RoleUser, Blocks: blocks}
}

// AudioContent builds an inline audio block from raw base64 data.
func AudioContent(data, format string) Block {
	return Block{Type: BlockAudio, AudioData: data, AudioFormat: format}
}

// FileContent builds an inline document block. data is raw base64 (no
// data: prefix), a data: URL or an http(s) URL (the last is accepted by
// Anthropic only).
func FileContent(name, mediaType, data string) Block {
	return Block{Type: BlockFile, FileName: name, MimeType: mediaType, FileData: data}
}

// FileRef references a file previously uploaded through the provider's
// files API, instead of carrying inline content.
func FileRef(name, fileID string) Block {
	return Block{Type: BlockFile, FileName: name, FileID: fileID}
}

// Assistant builds an assistant message. Use AssistantBlocks to include
// tool calls or thinking content in a multi-turn replay.
func Assistant(text string) Message {
	return Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// AssistantBlocks builds an assistant message from raw blocks (tool calls,
// thinking replay, plain text).
func AssistantBlocks(blocks ...Block) Message {
	return Message{Role: RoleAssistant, Blocks: blocks}
}

// ToolCall builds a tool_call block for an assistant turn.
func ToolCall(id, name, arguments string) Block {
	return Block{Type: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: arguments}
}

// ToolResult builds a tool result message addressed to the given call.
func ToolResult(callID, name, content string) Message {
	return Message{Role: RoleTool, Blocks: []Block{{
		Type: BlockToolResult, ToolCallID: callID, ToolName: name, Content: content,
	}}}
}

// Thinking builds a thinking block, typically to replay prior reasoning in
// a multi-turn conversation on Anthropic (Signature preserves the
// provider-supplied signature and must be passed through unchanged).
func Thinking(text, signature string) Block {
	return Block{Type: BlockThinking, Thinking: text, Signature: signature}
}

// text returns the concatenation of the message's text blocks.
func (m Message) text() string {
	var b strings.Builder
	for i, blk := range m.Blocks {
		if blk.Type == BlockText && blk.Text != "" {
			if i > 0 && b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// roleAllowedBlocks defines which block types each role may carry.
// Combinations outside the matrix are rejected at validation time instead
// of being silently dropped by a protocol encoder — a file or audio block
// that never reaches the model must not look like a successful request.
var roleAllowedBlocks = map[Role]map[BlockType]bool{
	RoleSystem:    {BlockText: true},
	RoleUser:      {BlockText: true, BlockImage: true, BlockAudio: true, BlockFile: true},
	RoleAssistant: {BlockText: true, BlockToolCall: true, BlockThinking: true, BlockRedactedThinking: true},
	RoleTool:      {BlockToolResult: true},
}

// validate checks protocol-independent structural rules for one message:
// a known role, at least one effective content block, block types allowed
// for the role, and the required fields per block type. Empty text blocks
// are treated as placeholders and skipped — an image-only message with an
// empty text prefix is valid — but a message left with no effective
// content is an error.
func (m Message) validate() error {
	switch m.Role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("unsupported role %q", m.Role)
	}
	allowed := roleAllowedBlocks[m.Role]
	n := 0
	for _, b := range m.Blocks {
		if b.Type == BlockText && b.Text == "" {
			continue // placeholder alongside media blocks
		}
		if !allowed[b.Type] {
			return fmt.Errorf("%s role does not support %s blocks", m.Role, b.Type)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("message has no content blocks")
	}
	for i, b := range m.Blocks {
		if b.Type == BlockText && b.Text == "" {
			continue
		}
		if err := b.validate(); err != nil {
			return fmt.Errorf("Blocks[%d] (%s): %w", i, b.Type, err)
		}
	}
	return nil
}

// validate checks the required fields of one content block.
func (b Block) validate() error {
	switch b.Type {
	case BlockText:
		if b.Text == "" {
			return fmt.Errorf("text block is empty")
		}
	case BlockImage:
		if b.ImageURL == "" {
			return fmt.Errorf("image block needs ImageURL")
		}
		if err := validateImageURL(b.ImageURL); err != nil {
			return err
		}
	case BlockAudio:
		if b.AudioData == "" {
			return fmt.Errorf("audio block has no AudioData")
		}
		if b.AudioFormat != "wav" && b.AudioFormat != "mp3" {
			return fmt.Errorf("audio format %q unsupported (wav or mp3)", b.AudioFormat)
		}
		if !isBase64(b.AudioData) {
			return fmt.Errorf("audio block AudioData is not valid base64")
		}
	case BlockFile:
		if b.FileData == "" && b.FileID == "" {
			return fmt.Errorf("file block needs FileData or FileID")
		}
		if b.FileData != "" && b.FileID != "" {
			return fmt.Errorf("file block has both FileData and FileID; set exactly one source")
		}
		if b.FileData != "" {
			if err := validateFileData(b.FileData); err != nil {
				return err
			}
		}
	case BlockToolCall:
		if b.ToolCallID == "" || b.ToolName == "" {
			return fmt.Errorf("tool_call block needs ToolCallID and ToolName")
		}
		// Arguments reach the model as a tool input object; a syntactically
		// valid but non-object payload (an array or a scalar) is silently
		// coerced to {} by the adapters, corrupting the tool pairing, so the
		// object shape is required up front (audit C3).
		if b.Arguments != "" {
			trimmed := strings.TrimSpace(b.Arguments)
			if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid([]byte(trimmed)) {
				return fmt.Errorf("tool_call arguments must be a JSON object")
			}
		}
	case BlockToolResult:
		if b.ToolCallID == "" {
			return fmt.Errorf("tool_result block needs ToolCallID")
		}
	case BlockThinking:
		if b.Thinking == "" && b.Signature == "" {
			return fmt.Errorf("thinking block is empty")
		}
	case BlockRedactedThinking:
		if b.Thinking == "" {
			return fmt.Errorf("redacted_thinking block has no data")
		}
	default:
		return fmt.Errorf("unknown block type %q", b.Type)
	}
	if b.CacheControl != nil {
		if err := b.CacheControl.validate(); err != nil {
			return err
		}
		// Anthropic only accepts cache breakpoints on text, image,
		// document, tool_result and tool_use blocks; a thinking or
		// redacted_thinking block carrying one would be silently dropped, so
		// reject it locally.
		if b.Type == BlockThinking || b.Type == BlockRedactedThinking {
			return fmt.Errorf("thinking blocks cannot carry cache_control")
		}
	}
	return nil
}

// validateImageURL checks an image reference: an http(s) URL passes through,
// a data: URL must be well-formed with a non-empty (and, for base64, valid)
// payload; any other form — file://, ftp://, bare base64, an HTML data URL
// reaching a non-image sink — is rejected locally instead of being forwarded
// to the provider's image_url.url field (audit C2).
func validateImageURL(u string) error {
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		return nil
	case strings.HasPrefix(u, "data:"):
		rest := strings.TrimPrefix(u, "data:")
		head, payload, ok := strings.Cut(rest, ",")
		if !ok {
			return fmt.Errorf("image data: URL is malformed (missing comma)")
		}
		if payload == "" {
			return fmt.Errorf("image data: URL has an empty payload")
		}
		if strings.Contains(head, ";base64") && !isBase64(payload) {
			return fmt.Errorf("image data: URL payload is not valid base64")
		}
		return nil
	default:
		return fmt.Errorf("image URL must be an http(s) or data: URL")
	}
}

// validateFileData checks inline file content: a data: URL must be
// well-formed with a non-empty payload (base64 payloads must decode);
// anything else must be valid base64. http(s) URLs pass through — only
// Anthropic accepts them, which the protocol encoders enforce.
func validateFileData(data string) error {
	if strings.HasPrefix(data, "data:") {
		rest := strings.TrimPrefix(data, "data:")
		head, payload, ok := strings.Cut(rest, ",")
		if !ok {
			return fmt.Errorf("file data is a malformed data: URL (missing comma)")
		}
		if payload == "" {
			return fmt.Errorf("file data URL has an empty payload")
		}
		if strings.Contains(head, ";base64") && !isBase64(payload) {
			return fmt.Errorf("file data URL payload is not valid base64")
		}
		return nil
	}
	if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
		return nil
	}
	if !isBase64(data) {
		return fmt.Errorf("file block FileData is not valid base64, a data: URL or an http(s) URL")
	}
	return nil
}

// isBase64 reports whether s is valid standard (padded or raw) base64.
func isBase64(s string) bool {
	if s == "" {
		return false
	}
	if _, err := base64.StdEncoding.DecodeString(s); err == nil {
		return true
	}
	_, err := base64.RawStdEncoding.DecodeString(s)
	return err == nil
}
