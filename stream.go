package rosetta

import (
	"context"
	"errors"
	"io"
	"slices"
)

// EventType enumerates unified stream event kinds.
type EventType string

const (
	EventMessageStart  EventType = "message_start"
	EventTextDelta     EventType = "text_delta"
	EventThinkingDelta EventType = "thinking_delta"
	EventToolCall      EventType = "tool_call" // incremental tool-call fragment
	EventMessageEnd    EventType = "message_end"
)

// Event is one unified streaming event. Meaningful fields depend on Type:
//
//   - EventMessageStart: ID, Model
//   - EventTextDelta / EventThinkingDelta: Text
//   - EventToolCall: ToolIndex, ToolID, ToolName, ArgumentsDelta (fragments
//     concatenate into a complete JSON arguments string)
//   - EventMessageEnd: StopReason, Usage
type Event struct {
	Type           EventType
	ID             string
	Model          string
	Text           string
	Signature      string // thinking signature passthrough (Anthropic)
	ToolIndex      int
	ToolID         string
	ToolName       string
	ArgumentsDelta string
	Usage          *Usage
	StopReason     StopReason
}

// Stream is a pull-based iterator over unified stream events. It is not
// safe for concurrent use; drive it from one goroutine. A typical loop:
//
//	stream, err := client.ChatStream(ctx, req)
//	if err != nil { return err }
//	defer stream.Close()
//	for stream.Next() {
//		switch ev := stream.Event(); ev.Type {
//		case rosetta.EventTextDelta:
//			fmt.Print(ev.Text)
//		}
//	}
//	if err := stream.Err(); err != nil { return err }
type Stream interface {
	// Next advances to the next event, returning false at the end of the
	// stream (clean end or error — check Err).
	Next() bool
	// Event returns the current event; valid only after Next returned true.
	Event() *Event
	// Err returns the stream failure, or nil after a clean end.
	Err() error
	// Usage returns the token usage reported by the provider (zero until
	// an end event carrying usage has been seen).
	Usage() Usage
	// Partial returns the response accumulated so far, useful when a
	// stream is interrupted mid-flight.
	Partial() *ChatResponse
	// Collect drains the remaining events and returns the full response.
	// It returns the same error as Err on failure.
	Collect() (*ChatResponse, error)
	// Close releases underlying resources (HTTP connection, context).
	// Calling it after a clean end is a no-op; calling it mid-stream
	// discards the rest of the response.
	Close() error
}

// streamCore is the shared Stream implementation. Protocol adapters supply
// a next func producing unified events; this type accumulates the partial
// response, tracks usage and finalizes exactly once.
type streamCore struct {
	next   func() (*Event, error)
	onEnd  func(Usage, error)
	cancel context.CancelFunc
	closer io.Closer

	done     bool
	released bool
	err      error
	cur      *Event
	partial  ChatResponse
	usage    Usage
	toolPos  map[int]int
}

// newStream wires a Stream from an event producer. onEnd, when non-nil,
// is invoked once when the stream finishes (cleanly or with an error).
func newStream(next func() (*Event, error), onEnd func(Usage, error)) *streamCore {
	return &streamCore{next: next, onEnd: onEnd}
}

// attachCancel lets the owner release the HTTP context when the stream
// terminates for any reason.
func (s *streamCore) attachCancel(cancel context.CancelFunc) {
	s.cancel = cancel
}

// attachCloser registers the response body for closing when the stream
// terminates, so early Close and clean end both free the connection.
func (s *streamCore) attachCloser(c io.Closer) {
	s.closer = c
}

func (s *streamCore) Next() bool {
	if s.done {
		return false
	}
	ev, err := s.next()
	if err != nil {
		s.done = true
		if !errors.Is(err, io.EOF) {
			s.err = err
		}
		s.release()
		return false
	}
	if ev == nil {
		// Defense in depth: a producer returning (nil, nil) must fail
		// the stream, never panic the consumer.
		s.done = true
		s.err = errors.New("rosetta: internal error: stream produced a nil event")
		s.release()
		return false
	}
	s.apply(ev)
	s.cur = ev
	return true
}

func (s *streamCore) Event() *Event { return s.cur }

func (s *streamCore) Err() error { return s.err }

func (s *streamCore) Usage() Usage { return s.usage }

func (s *streamCore) Partial() *ChatResponse {
	cp := s.partial
	cp.Content = slices.Clone(cp.Content) // snapshots must not mutate with the live stream
	return &cp
}

func (s *streamCore) Collect() (*ChatResponse, error) {
	for s.Next() {
	}
	resp := s.partial
	resp.Content = slices.Clone(resp.Content)
	return &resp, s.err
}

func (s *streamCore) Close() error {
	s.done = true
	s.release()
	return nil
}

// release finalizes the stream exactly once: cancels the request context,
// closes the response body (freeing the connection on early Close) and
// notifies the owner.
func (s *streamCore) release() {
	if s.released {
		return
	}
	s.released = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.closer != nil {
		s.closer.Close()
	}
	if s.onEnd != nil {
		s.onEnd(s.usage, s.err)
	}
}

// apply folds an event into the accumulated partial response.
func (s *streamCore) apply(ev *Event) {
	switch ev.Type {
	case EventMessageStart:
		s.partial.ID = ev.ID
		s.partial.Model = ev.Model
	case EventTextDelta:
		appendBlockText(&s.partial.Content, ev.Text)
	case EventThinkingDelta:
		appendBlockThinking(&s.partial.Content, ev.Text, ev.Signature)
	case EventToolCall:
		if s.toolPos == nil {
			s.toolPos = make(map[int]int)
		}
		appendToolDelta(&s.partial.Content, s.toolPos, ev)
	case EventMessageEnd:
		if ev.StopReason != "" {
			s.partial.StopReason = ev.StopReason
		}
		if ev.Usage != nil {
			s.usage = *ev.Usage
			s.partial.Usage = *ev.Usage
		}
	}
}

func appendBlockText(blocks *[]Block, text string) {
	if text == "" {
		return
	}
	if n := len(*blocks); n > 0 && (*blocks)[n-1].Type == BlockText {
		(*blocks)[n-1].Text += text
		return
	}
	*blocks = append(*blocks, Block{Type: BlockText, Text: text})
}

func appendBlockThinking(blocks *[]Block, text, signature string) {
	if n := len(*blocks); n > 0 && (*blocks)[n-1].Type == BlockThinking {
		(*blocks)[n-1].Thinking += text
		if signature != "" {
			(*blocks)[n-1].Signature = signature
		}
		return
	}
	*blocks = append(*blocks, Block{Type: BlockThinking, Thinking: text, Signature: signature})
}

func appendToolDelta(blocks *[]Block, pos map[int]int, ev *Event) {
	idx, ok := pos[ev.ToolIndex]
	if !ok {
		idx = len(*blocks)
		*blocks = append(*blocks, Block{Type: BlockToolCall})
		pos[ev.ToolIndex] = idx
	}
	b := &(*blocks)[idx]
	if ev.ToolID != "" {
		b.ToolCallID = ev.ToolID
	}
	if ev.ToolName != "" {
		b.ToolName = ev.ToolName
	}
	b.Arguments += ev.ArgumentsDelta
}
