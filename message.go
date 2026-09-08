package rosetta

import "strings"

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
	BlockToolCall   BlockType = "tool_call"   // model requesting a tool
	BlockToolResult BlockType = "tool_result" // result fed back to the model
	BlockThinking   BlockType = "thinking"    // chain-of-thought content
)

// Block is one piece of message content. Which fields are meaningful
// depends on Type:
//
//   - BlockText: Text
//   - BlockImage: ImageURL (http(s) or data: URL)
//   - BlockToolCall: ToolCallID, ToolName, Arguments (raw JSON string)
//   - BlockToolResult: ToolCallID, ToolName, Content (text payload), IsError
//   - BlockThinking: Thinking, Signature (Anthropic passthrough)
type Block struct {
	Type       BlockType
	Text       string
	ImageURL   string
	ToolCallID string
	ToolName   string
	Arguments  string
	Content    string
	IsError    bool
	Thinking   string
	Signature  string
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
