package rosetta

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
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

// Stream is a pull-based iterator over unified stream events. Next must
// be driven from a single goroutine, but Err / Usage / Partial / Close are
// safe to call concurrently (e.g. a watchdog closing the stream while the
// consumer loop runs). A typical loop:
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
//
// State transitions are mutex-guarded so Close can race a blocked Next:
// the event produced after a concurrent Close is discarded and the
// resources were already released by Close.
type streamCore struct {
	next   func() (*Event, error)
	onEnd  func(Usage, error)
	cancel context.CancelFunc
	closer io.Closer

	mu       sync.Mutex
	done     bool
	released bool
	closeErr error
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
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()

	// The producer runs outside the lock: it blocks on network reads.
	ev, err := s.next()

	s.mu.Lock()
	if s.done {
		// Close() won the race while we were blocked in next(); it already
		// released the resources and will fire onEnd after unlocking.
		s.mu.Unlock()
		return false
	}
	if err != nil {
		s.done = true
		if !errors.Is(err, io.EOF) {
			s.err = err
		}
		s.releaseLocked()
		s.mu.Unlock()
		if call, usage, terr := s.takeOnEnd(); call != nil {
			call(usage, terr)
		}
		return false
	}
	if ev == nil {
		// Defense in depth: a producer returning (nil, nil) must fail
		// the stream, never panic the consumer.
		s.done = true
		s.err = errors.New("rosetta: internal error: stream produced a nil event")
		s.releaseLocked()
		s.mu.Unlock()
		if call, usage, terr := s.takeOnEnd(); call != nil {
			call(usage, terr)
		}
		return false
	}
	s.apply(ev)
	s.cur = ev
	s.mu.Unlock()
	return true
}

func (s *streamCore) Event() *Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *streamCore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *streamCore) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *streamCore) Partial() *ChatResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.partial
	// Clone inside the lock: Next() mutates existing blocks in place, so a
	// post-unlock clone of Content would race the live stream.
	cp.Content = slices.Clone(cp.Content)
	return &cp
}

func (s *streamCore) Collect() (*ChatResponse, error) {
	for s.Next() {
	}
	return s.Partial(), s.Err()
}

func (s *streamCore) Close() error {
	s.mu.Lock()
	s.done = true
	s.releaseLocked()
	closeErr := s.closeErr
	s.mu.Unlock()
	// Fire onEnd outside the lock: user-supplied usage trackers may read
	// stream accessors (Partial/Err/Usage) or close the stream, which must
	// not deadlock against the mutex held here.
	if call, usage, err := s.takeOnEnd(); call != nil {
		call(usage, err)
	}
	return closeErr
}

// releaseLocked finalizes the stream exactly once: cancels the request
// context and closes the response body. The owner callback (onEnd) is NOT
// invoked here — callers fire it via takeOnEnd after unlocking, so user
// code can never run while mu is held. Callers hold mu.
func (s *streamCore) releaseLocked() {
	if s.released {
		return
	}
	s.released = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.closer != nil {
		s.closeErr = s.closer.Close()
		if s.err == nil {
			s.err = s.closeErr
		}
	}
}

// takeOnEnd pops the owner callback so it fires exactly once, outside mu.
// usage/err are the terminal values captured at the time of the pop. The
// returned call is nil when the callback was already consumed.
func (s *streamCore) takeOnEnd() (call func(Usage, error), usage Usage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call = s.onEnd
	s.onEnd = nil
	usage, err = s.usage, s.err
	return
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
