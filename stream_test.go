package rosetta

import (
	"errors"
	"io"
	"testing"
)

// seqNext builds a next func returning the given events in order, then
// io.EOF.
func seqNext(evs ...*Event) func() (*Event, error) {
	i := 0
	return func() (*Event, error) {
		if i < len(evs) {
			ev := evs[i]
			i++
			return ev, nil
		}
		return nil, io.EOF
	}
}

func TestStreamCoreAccumulates(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventMessageStart, ID: "r1", Model: "m"},
		&Event{Type: EventTextDelta, Text: "Hel"},
		&Event{Type: EventTextDelta, Text: "lo"},
		&Event{Type: EventThinkingDelta, Text: "think"},
		&Event{Type: EventThinkingDelta, Text: "ing", Signature: "sig"},
		&Event{Type: EventToolCall, ToolIndex: 0, ToolID: "t1", ToolName: "fn"},
		&Event{Type: EventToolCall, ToolIndex: 0, ArgumentsDelta: `{"a":`},
		&Event{Type: EventToolCall, ToolIndex: 0, ArgumentsDelta: `1}`},
		&Event{Type: EventMessageEnd, StopReason: StopEnd, Usage: &Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}},
	), nil)
	defer s.Close()

	var texts []string
	for s.Next() {
		if ev := s.Event(); ev.Type == EventTextDelta {
			texts = append(texts, ev.Text)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if len(texts) != 2 || texts[0]+texts[1] != "Hello" {
		t.Fatalf("texts = %v", texts)
	}

	p := s.Partial()
	if p.ID != "r1" || p.Model != "m" || p.StopReason != StopEnd {
		t.Fatalf("partial header wrong: %+v", p)
	}
	if p.Text() != "Hello" {
		t.Fatalf("Text = %q", p.Text())
	}
	if p.ThinkingText() != "thinking" {
		t.Fatalf("ThinkingText = %q", p.ThinkingText())
	}
	tc := p.ToolCalls()
	if len(tc) != 1 || tc[0].ToolCallID != "t1" || tc[0].ToolName != "fn" || tc[0].Arguments != `{"a":1}` {
		t.Fatalf("tool call wrong: %+v", tc)
	}
	if u := s.Usage(); u != (Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}) {
		t.Fatalf("usage = %+v", u)
	}
	if p.Usage != s.Usage() {
		t.Fatalf("partial usage = %+v", p.Usage)
	}
}

func TestStreamCoreCollect(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventTextDelta, Text: "a"},
		&Event{Type: EventTextDelta, Text: "b"},
	), nil)
	resp, err := s.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "ab" {
		t.Fatalf("Text = %q", resp.Text())
	}
	if s.Next() {
		t.Fatal("Next after Collect must be false")
	}
}

// Partial snapshots must be point-in-time: later events may not mutate a
// snapshot the caller already holds.
func TestStreamCorePartialSnapshotIsolated(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventTextDelta, Text: "hel"},
		&Event{Type: EventTextDelta, Text: "lo"},
		&Event{Type: EventTextDelta, Text: "!"},
	), nil)
	defer s.Close()

	if !s.Next() {
		t.Fatal("Next must be true")
	}
	snap := s.Partial()
	if snap.Text() != "hel" {
		t.Fatalf("snapshot = %q, want %q", snap.Text(), "hel")
	}
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if snap.Text() != "hel" {
		t.Fatalf("snapshot mutated by later events: %q", snap.Text())
	}
	if got := s.Partial().Text(); got != "hello!" {
		t.Fatalf("live partial = %q, want %q", got, "hello!")
	}

	// Collect's result is isolated from the live stream too.
	s2 := newStream(seqNext(
		&Event{Type: EventTextDelta, Text: "x"},
		&Event{Type: EventTextDelta, Text: "y"},
	), nil)
	resp, err := s2.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Partial().Text(); got != "xy" {
		t.Fatalf("live partial after Collect = %q", got)
	}
	if resp.Text() != "xy" {
		t.Fatalf("collect result = %q", resp.Text())
	}
}

func TestStreamCoreErrorSurfaced(t *testing.T) {
	boom := errors.New("boom")
	var endCalls int
	var endErr error
	s := newStream(func() (*Event, error) { return nil, boom }, func(u Usage, err error) { endCalls++; endErr = err })
	for s.Next() {
		t.Fatal("Next must be false on immediate error")
	}
	if !errors.Is(s.Err(), boom) {
		t.Fatalf("Err = %v", s.Err())
	}
	if endCalls != 1 || !errors.Is(endErr, boom) {
		t.Fatalf("onEnd calls=%d err=%v", endCalls, endErr)
	}
}

func TestStreamCoreOnEndOnce(t *testing.T) {
	var endCalls int
	s := newStream(seqNext(&Event{Type: EventMessageEnd, StopReason: StopEnd}), func(u Usage, err error) { endCalls++ })
	for s.Next() {
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if endCalls != 1 {
		t.Fatalf("onEnd called %d times, want 1", endCalls)
	}
}

func TestStreamCoreCancelOnEarlyClose(t *testing.T) {
	var cancels int
	s := newStream(func() (*Event, error) { return &Event{Type: EventTextDelta, Text: "x"}, nil }, nil)
	s.attachCancel(func() { cancels++ })
	if !s.Next() {
		t.Fatal("Next must be true")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if cancels != 1 {
		t.Fatalf("cancel called %d times, want 1", cancels)
	}
	if s.Next() {
		t.Fatal("Next after Close must be false")
	}
	if cancels != 1 {
		t.Fatalf("cancel called %d times after Close, want 1", cancels)
	}
}

// A producer returning (nil, nil) must fail the stream, never panic the
// consumer (defense in depth behind the provider-level guards).
func TestStreamCoreNilEventFailsNotPanics(t *testing.T) {
	s := newStream(func() (*Event, error) { return nil, nil }, nil)
	defer s.Close()
	for s.Next() {
		t.Fatal("Next must be false for a nil event")
	}
	if s.Err() == nil {
		t.Fatal("nil event must surface as an error")
	}
}

// The attached closer releases the response body exactly once, on early
// Close and on natural end alike — the connection must never leak.
type closerRecorder struct{ n int }

func (c *closerRecorder) Close() error { c.n++; return nil }

func TestStreamCoreCloserCalledOnce(t *testing.T) {
	// Early Close mid-stream.
	cl := &closerRecorder{}
	s := newStream(func() (*Event, error) { return &Event{Type: EventTextDelta, Text: "x"}, nil }, nil)
	s.attachCloser(cl)
	if !s.Next() {
		t.Fatal("Next must be true")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if cl.n != 1 {
		t.Fatalf("closer called %d times after early Close, want 1", cl.n)
	}

	// Natural end also releases.
	cl = &closerRecorder{}
	s2 := newStream(seqNext(&Event{Type: EventMessageEnd, StopReason: StopEnd}), nil)
	s2.attachCloser(cl)
	for s2.Next() {
	}
	if err := s2.Err(); err != nil {
		t.Fatal(err)
	}
	if cl.n != 1 {
		t.Fatalf("closer called %d times after natural end, want 1", cl.n)
	}

	// Error end releases too.
	cl = &closerRecorder{}
	boom := errors.New("boom")
	s3 := newStream(func() (*Event, error) { return nil, boom }, nil)
	s3.attachCloser(cl)
	for s3.Next() {
	}
	if cl.n != 1 {
		t.Fatalf("closer called %d times after error end, want 1", cl.n)
	}
}
