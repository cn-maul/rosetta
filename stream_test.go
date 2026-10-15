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

// #11b: a response-body Close error must not masquerade as a stream failure.
// After a clean end, Err() stays nil even when freeing the connection fails;
// the close error is still surfaced through Close()'s return value.
type errCloser struct{ err error }

func (c *errCloser) Close() error { return c.err }

func TestStreamCoreCloseErrorNotFailure(t *testing.T) {
	closeErr := errors.New("connection close failed")
	s := newStream(seqNext(&Event{Type: EventMessageEnd, StopReason: StopEnd}), nil)
	s.attachCloser(&errCloser{err: closeErr})
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err after clean end = %v, want nil", err)
	}
	if err := s.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close = %v, want %v", err, closeErr)
	}
}

// C09: buffered replay is 1:1 with the decoded content. Adjacent same-kind
// blocks used to fold into one accumulator slot because the events carried
// no index; each one now keeps its own identity and order. Sealed /
// ReasoningID replay is asserted separately once those Event fields exist
// (plan C10).
func TestBufferedStreamKeepsBlockIdentity(t *testing.T) {
	cr := &ChatResponse{ID: "r1", Content: []Block{
		{Type: BlockText, Text: "A"},
		{Type: BlockThinking, Thinking: "S1", Signature: "sig1"},
		{Type: BlockThinking, Thinking: "S2", Signature: "sig2"},
		{Type: BlockToolCall, ToolCallID: "c1", ToolName: "fn1", Arguments: `{"a":1}`},
		{Type: BlockToolCall, ToolCallID: "c2", ToolName: "fn2", Arguments: `{"b":2}`},
		{Type: BlockText, Text: "B"},
	}}
	got, err := bufferedStream(cr).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got.Content) != len(cr.Content) {
		t.Fatalf("got %d blocks, want %d", len(got.Content), len(cr.Content))
	}
	want := []Block{
		{Type: BlockText, Text: "A"},
		{Type: BlockThinking, Thinking: "S1", Signature: "sig1"},
		{Type: BlockThinking, Thinking: "S2", Signature: "sig2"},
		{Type: BlockToolCall, ToolCallID: "c1", ToolName: "fn1", Arguments: `{"a":1}`},
		{Type: BlockToolCall, ToolCallID: "c2", ToolName: "fn2", Arguments: `{"b":2}`},
		{Type: BlockText, Text: "B"},
	}
	if got.ID != cr.ID {
		t.Errorf("ID = %q, want %q", got.ID, cr.ID)
	}
	for i := range want {
		if got.Content[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, got.Content[i], want[i])
		}
	}
}

// C11: an event that exceeds the remaining budget is rejected whole. The
// accumulator must not keep a partial write from a rejected event, so the
// existing block and any state already held in it survive untouched.
func TestBlockForOverflowLeavesBlockUnchanged(t *testing.T) {
	s := newStream(seqNext(), nil)
	if b := s.blockFor(&s.textPos, 0, BlockText, 0); b == nil {
		t.Fatal("blockFor returned nil for a fresh block")
	} else {
		b.text.WriteString("keep")
	}
	before := s.buildContent()[0].Text
	s.accBytes = maxStreamAccumBytes - 1
	if b := s.blockFor(&s.textPos, 0, BlockText, 16); b != nil {
		t.Fatal("blockFor admitted an event that exceeds the budget")
	}
	if !s.overflow {
		t.Error("overflow flag not set")
	}
	if after := s.buildContent()[0].Text; after != before {
		t.Errorf("text = %q after rejected event, want %q", after, before)
	}
}

// C12: the same rejection applies to a block that does not exist yet, and
// must not leave a half-created block behind.
func TestBlockForOverflowDoesNotCreateBlock(t *testing.T) {
	s := newStream(seqNext(), nil)
	s.accBytes = maxStreamAccumBytes - 1
	if b := s.blockFor(&s.textPos, 7, BlockText, 16); b != nil {
		t.Fatal("blockFor admitted a new block that exceeds the budget")
	}
	if len(s.acc) != 0 {
		t.Fatalf("acc has %d blocks, want 0", len(s.acc))
	}
	if _, ok := s.textPos[7]; ok {
		t.Error("rejected index was registered")
	}
	if s.Partial().Content != nil {
		t.Errorf("Partial has content %+v, want none", s.Partial().Content)
	}
}

// C01/C02/C04: a thinking block accumulates text, signature and sealed
// payload side by side. The sealed payload may arrive as an event with an
// empty Text, which must still land in the same block.
func TestStreamCoreThinkingSealAccumulation(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventThinkingDelta, Text: "step one ", BlockIndex: 0},
		&Event{Type: EventThinkingDelta, Text: "step two", BlockIndex: 0, Signature: "sig"},
		&Event{Type: EventThinkingDelta, BlockIndex: 0, Sealed: "BLOB", SealedBy: "openai-responses", ReasoningID: "rs_1"},
	), nil)
	defer s.Close()
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	got := s.Partial().Content
	if len(got) != 1 {
		t.Fatalf("got %d blocks, want 1", len(got))
	}
	if got[0].Thinking != "step one step two" {
		t.Errorf("Thinking = %q", got[0].Thinking)
	}
	if got[0].Signature != "sig" {
		t.Errorf("Signature = %q, want sig", got[0].Signature)
	}
	if got[0].Sealed != "BLOB" || got[0].SealedBy != "openai-responses" || got[0].ReasoningID != "rs_1" {
		t.Errorf("seal = %+v", got[0])
	}
}

// C03: the public accumulator stores sealed state regardless of its
// protocol family; filtering foreign payloads is the outbound adapter's
// job, not the accumulator's.
func TestStreamCoreKeepsForeignSealedState(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventThinkingDelta, BlockIndex: 0, Sealed: "FUTURE", SealedBy: "future-protocol"},
	), nil)
	defer s.Close()
	for s.Next() {
	}
	got := s.Partial().Content
	if len(got) != 1 || got[0].Sealed != "FUTURE" || got[0].SealedBy != "future-protocol" {
		t.Fatalf("sealed state not kept: %+v", got)
	}
}

// C05: Partial is a snapshot. Updating the sealed state after a Partial
// call must not mutate the slice the caller already holds, and the next
// Partial must show the new state.
func TestStreamCorePartialSnapshotAfterSealUpdate(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventThinkingDelta, Text: "t", BlockIndex: 0},
		&Event{Type: EventThinkingDelta, BlockIndex: 0, Sealed: "FIRST", SealedBy: "openai-responses"},
	), nil)
	defer s.Close()
	if !s.Next() {
		t.Fatal("no first event")
	}
	if !s.Next() {
		t.Fatal("no second event")
	}
	first := s.Partial().Content
	if len(first) != 1 || first[0].Sealed != "FIRST" {
		t.Fatalf("first snapshot = %+v", first)
	}
	for s.Next() {
	}
	second := s.Partial().Content
	if len(second) != 1 || second[0].Sealed != "FIRST" {
		t.Fatalf("second snapshot = %+v", second)
	}
	if first[0].Thinking != "t" {
		t.Errorf("first snapshot mutated: %+v", first[0])
	}
}

// C06: distinct block indices never merge, so payloads cannot cross
// between two reasoning items that share a provider output index.
func TestStreamCoreThinkingBlocksDoNotMerge(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventThinkingDelta, Text: "first", BlockIndex: 0},
		&Event{Type: EventThinkingDelta, Text: "second", BlockIndex: 1, Sealed: "B2", SealedBy: "openai-responses"},
		&Event{Type: EventThinkingDelta, BlockIndex: 0, Sealed: "B1", SealedBy: "openai-responses"},
	), nil)
	defer s.Close()
	for s.Next() {
	}
	got := s.Partial().Content
	if len(got) != 2 {
		t.Fatalf("got %d blocks, want 2", len(got))
	}
	if got[0].Thinking != "first" || got[0].Sealed != "B1" {
		t.Errorf("block 0 = %+v", got[0])
	}
	if got[1].Thinking != "second" || got[1].Sealed != "B2" {
		t.Errorf("block 1 = %+v", got[1])
	}
}

// C13: an idempotent redelivery of the same sealed state costs nothing
// extra against the byte budget.
func TestStreamCoreSealRedeliveryIsFree(t *testing.T) {
	s := newStream(seqNext(), nil)
	seal := &Event{Type: EventThinkingDelta, BlockIndex: 0, Sealed: "BLOB", SealedBy: "openai-responses", ReasoningID: "rs_1"}
	s.apply(seal)
	afterFirst := s.accBytes
	s.apply(seal)
	if s.accBytes != afterFirst {
		t.Errorf("redelivery charged %d bytes", s.accBytes-afterFirst)
	}
}
