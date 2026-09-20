package rosetta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
	BlockIndex     int    // content-block index for text/thinking merging (Anthropic); 0 elsewhere
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

	// Accumulation uses per-block builders appended amortized, not string
	// += (which is O(n²) per delta) — audit B8. Blocks are keyed by provider
	// content-block index so out-of-order emitters merge into the right block
	// (audit C17). Content is materialized from these on read.
	acc      []*accBlock
	textPos  map[int]int
	thinkPos map[int]int
	toolPos  map[int]int
	accBytes int
	overflow bool
}

// accBlock accumulates one output block. Exactly one of the builders is
// active, determined by kind.
type accBlock struct {
	kind      BlockType
	text      strings.Builder
	thinking  strings.Builder
	signature string
	toolID    string
	toolName  string
	args      strings.Builder
}

// newStream wires a Stream from an event producer. onEnd, when non-nil,
// is invoked once when the stream finishes (cleanly or with an error).
func newStream(next func() (*Event, error), onEnd func(Usage, error)) *streamCore {
	return &streamCore{next: next, onEnd: onEnd}
}

// bufferedStream replays a fully-decoded, non-event-stream ChatResponse as a
// one-shot unified stream. Gateways that answer a stream:true request with a
// buffered 200 application/json body would otherwise have their complete
// reply mistaken for a truncated (empty) SSE stream (audit B3).
func bufferedStream(cr *ChatResponse) Stream {
	events := make([]*Event, 0, len(cr.Content)+2)
	events = append(events, &Event{Type: EventMessageStart, ID: cr.ID, Model: cr.Model})
	for _, b := range cr.Content {
		switch b.Type {
		case BlockText:
			events = append(events, &Event{Type: EventTextDelta, Text: b.Text})
		case BlockThinking:
			events = append(events, &Event{Type: EventThinkingDelta, Text: b.Thinking, Signature: b.Signature})
		case BlockRedactedThinking:
			events = append(events, &Event{Type: EventThinkingDelta, Text: b.Thinking})
		case BlockToolCall:
			events = append(events, &Event{Type: EventToolCall, ToolID: b.ToolCallID, ToolName: b.ToolName, ArgumentsDelta: b.Arguments})
		}
	}
	u := cr.Usage
	events = append(events, &Event{Type: EventMessageEnd, StopReason: cr.StopReason, Usage: &u})
	i := 0
	return newStream(func() (*Event, error) {
		if i >= len(events) {
			return nil, io.EOF
		}
		ev := events[i]
		i++
		return ev, nil
	}, nil)
}

// attachCancel lets the owner release the HTTP context when the stream
// terminates for any reason. The write is lock-guarded because releaseLocked
// reads the field under the same lock (audit C11); if the stream already
// terminated, the cancel runs immediately so it is never dropped.
func (s *streamCore) attachCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		cancel()
		return
	}
	s.cancel = cancel
}

// attachCloser registers the response body for closing when the stream
// terminates, so early Close and clean end both free the connection. As with
// attachCancel the write is lock-guarded, and an already-released stream
// closes the body on the spot rather than leaking it.
func (s *streamCore) attachCloser(c io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		_ = c.Close()
		return
	}
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
	if s.overflow {
		// Accumulation crossed a safety cap: fail the stream rather than
		// let a hostile or buggy provider drive unbounded memory growth.
		s.done = true
		s.err = fmt.Errorf("rosetta: %w: stream accumulation exceeded %d bytes / %d blocks (partial response kept in Stream.Partial)", ErrStreamOverflow, maxStreamAccumBytes, maxStreamBlocks)
		s.releaseLocked()
		s.mu.Unlock()
		if call, usage, terr := s.takeOnEnd(); call != nil {
			call(usage, terr)
		}
		return false
	}
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
	cp.Content = s.buildContent()
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
		// Preserve the Close error for Close()'s return value, but do NOT
		// surface it through s.err: a stream that ended cleanly must report
		// Err()==nil even if freeing the connection failed.
		s.closeErr = s.closer.Close()
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

// Stream accumulation caps (audit B8). The bytes cap bounds total text,
// thinking and tool-argument volume a single stream may pile up — SSE
// per-event caps do not bound the number of events. The block cap bounds
// distinct content blocks (an attacker controls ToolIndex). Both trip the
// stream with ErrStreamOverflow rather than growing without limit.
const (
	maxStreamAccumBytes = 64 << 20 // 64 MiB of accumulated content
	maxStreamBlocks     = 10_000
)

// apply folds an event into the accumulator. Deltas append into per-block
// strings.Builder instances (amortized, not O(n²) string +=) keyed by the
// provider content-block index, so out-of-order emitters never fold text or
// a signature into the wrong block. Content is materialized on read.
func (s *streamCore) apply(ev *Event) {
	switch ev.Type {
	case EventMessageStart:
		s.partial.ID = ev.ID
		s.partial.Model = ev.Model
	case EventTextDelta:
		if b := s.blockFor(&s.textPos, ev.BlockIndex, BlockText, len(ev.Text)); b != nil {
			b.text.WriteString(ev.Text)
		}
	case EventThinkingDelta:
		if b := s.blockFor(&s.thinkPos, ev.BlockIndex, BlockThinking, len(ev.Text)); b != nil {
			b.thinking.WriteString(ev.Text)
			if ev.Signature != "" {
				b.signature = ev.Signature
			}
		}
	case EventToolCall:
		if b := s.blockFor(&s.toolPos, ev.ToolIndex, BlockToolCall, len(ev.ArgumentsDelta)); b != nil {
			if ev.ToolID != "" {
				b.toolID = ev.ToolID
			}
			if ev.ToolName != "" {
				b.toolName = ev.ToolName
			}
			b.args.WriteString(ev.ArgumentsDelta)
		}
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

// blockFor returns the accumulator for a given provider index, creating it
// on first use, and accounts bytes against the caps. A nil return means the
// stream has overflowed; the caller drops the delta and Next surfaces the
// error.
func (s *streamCore) blockFor(pos *map[int]int, index int, kind BlockType, addBytes int) *accBlock {
	if i, ok := (*pos)[index]; ok {
		s.chargeBytes(addBytes)
		return s.acc[i]
	}
	if len(s.acc) >= maxStreamBlocks {
		s.overflow = true
		return nil
	}
	if addBytes > 0 {
		s.chargeBytes(addBytes)
		if s.overflow {
			return nil
		}
	}
	if *pos == nil {
		*pos = make(map[int]int)
	}
	i := len(s.acc)
	(*pos)[index] = i
	s.acc = append(s.acc, &accBlock{kind: kind})
	return s.acc[i]
}

func (s *streamCore) chargeBytes(n int) {
	if n <= 0 {
		return
	}
	s.accBytes += n
	if s.accBytes > maxStreamAccumBytes {
		s.overflow = true
	}
}

// buildContent materializes the accumulated blocks into a snapshot slice.
func (s *streamCore) buildContent() []Block {
	if len(s.acc) == 0 {
		return nil
	}
	out := make([]Block, len(s.acc))
	for i, b := range s.acc {
		switch b.kind {
		case BlockText:
			out[i] = Block{Type: BlockText, Text: b.text.String()}
		case BlockThinking:
			out[i] = Block{Type: BlockThinking, Thinking: b.thinking.String(), Signature: b.signature}
		case BlockToolCall:
			out[i] = Block{Type: BlockToolCall, ToolCallID: b.toolID, ToolName: b.toolName, Arguments: b.args.String()}
		}
	}
	return out
}
