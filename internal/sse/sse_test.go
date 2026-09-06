package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// collect drains all events from input; the stream must end with io.EOF.
func collect(t *testing.T, input string, r io.Reader) []*Event {
	t.Helper()
	if r == nil {
		r = strings.NewReader(input)
	}
	var evs []*Event
	sc := New(r)
	for {
		ev, err := sc.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected error: %v", err)
			}
			return evs
		}
		evs = append(evs, ev)
	}
}

func TestBasicEvent(t *testing.T) {
	evs := collect(t, "data: hello\n\n", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if string(evs[0].Data) != "hello" {
		t.Fatalf("data = %q", evs[0].Data)
	}
	if evs[0].Name != "" || evs[0].ID != "" {
		t.Fatalf("unexpected name/id: %q %q", evs[0].Name, evs[0].ID)
	}
}

func TestMultiLineData(t *testing.T) {
	evs := collect(t, "data: line1\ndata: line2\n\n", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if string(evs[0].Data) != "line1\nline2" {
		t.Fatalf("data = %q", evs[0].Data)
	}
}

func TestCRLF(t *testing.T) {
	evs := collect(t, "data: a\r\n\r\ndata: b\r\n\r\n", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	if string(evs[0].Data) != "a" || string(evs[1].Data) != "b" {
		t.Fatalf("data = %q %q", evs[0].Data, evs[1].Data)
	}
}

func TestCommentsAndFields(t *testing.T) {
	input := ": keep-alive\nevent: message\nid: 42\nretry: 100\ndata: x\n\n"
	evs := collect(t, input, nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Name != "message" || evs[0].ID != "42" || string(evs[0].Data) != "x" {
		t.Fatalf("event = %+v", evs[0])
	}
}

func TestTrailingEventWithoutBlankLine(t *testing.T) {
	evs := collect(t, "data: final", nil)
	if len(evs) != 1 || string(evs[0].Data) != "final" {
		t.Fatalf("trailing event lost: %+v", evs)
	}
}

// An event without data lines is not dispatched; its event name must not
// leak into the next dispatched event.
func TestNoDataEventNotDispatched(t *testing.T) {
	evs := collect(t, "event: phantom\n\ndata: real\n\n", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Name != "" || string(evs[0].Data) != "real" {
		t.Fatalf("name leaked or data wrong: %+v", evs[0])
	}
}

// Event.ID is per-event: an id field applies only to the event it was
// declared for, and an id on a discarded (no-data) event must not leak
// into the next dispatched one.
func TestIDPerEvent(t *testing.T) {
	evs := collect(t, "id: 1\ndata: a\n\ndata: b\n\n", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].ID != "1" || evs[1].ID != "" {
		t.Fatalf("ids = %q %q, want \"1\", \"\"", evs[0].ID, evs[1].ID)
	}

	evs = collect(t, "id: 9\nevent: phantom\n\ndata: a\n\n", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].ID != "" {
		t.Fatalf("orphaned id leaked: %q", evs[0].ID)
	}
}

func TestNoColonLine(t *testing.T) {
	evs := collect(t, "data\n\n", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if string(evs[0].Data) != "" {
		t.Fatalf("data = %q, want empty", evs[0].Data)
	}
}

// Multi-byte sequences split across reads must survive buffering.
func TestSplitReads(t *testing.T) {
	input := "data: 你好世界\n\n"
	evs := collect(t, input, iotest.OneByteReader(strings.NewReader(input)))
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if string(evs[0].Data) != "你好世界" {
		t.Fatalf("data = %q", evs[0].Data)
	}
}

func TestEmptyStream(t *testing.T) {
	if evs := collect(t, "", nil); len(evs) != 0 {
		t.Fatalf("got %d events", len(evs))
	}
}
