package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func collect(t *testing.T, r io.Reader) []*Event {
	t.Helper()
	sc := New(r)
	var out []*Event
	for {
		ev, err := sc.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("unexpected error: %v", err)
		}
		out = append(out, ev)
	}
}

func TestBasicEvents(t *testing.T) {
	evs := collect(t, strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]\n\n"))
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if string(evs[0].Data) != `{"a":1}` {
		t.Errorf("event 0 data = %q", evs[0].Data)
	}
	if string(evs[1].Data) != "[DONE]" {
		t.Errorf("event 1 data = %q", evs[1].Data)
	}
}

func TestCRLFAndEventField(t *testing.T) {
	evs := collect(t, strings.NewReader("event: delta\r\ndata: hello\r\n\r\n"))
	if len(evs) != 1 || evs[0].Name != "delta" || string(evs[0].Data) != "hello" {
		t.Fatalf("got %+v", evs)
	}
}

func TestMultilineData(t *testing.T) {
	evs := collect(t, strings.NewReader("data: line1\ndata: line2\n\n"))
	if len(evs) != 1 || string(evs[0].Data) != "line1\nline2" {
		t.Fatalf("got %+v", evs)
	}
}

func TestNoSpaceAfterColon(t *testing.T) {
	evs := collect(t, strings.NewReader("data:nospace\n\n"))
	if len(evs) != 1 || string(evs[0].Data) != "nospace" {
		t.Fatalf("got %+v", evs)
	}
}

func TestCommentsAndKeepAlives(t *testing.T) {
	evs := collect(t, strings.NewReader(": ping\n\n: ping\ndata: real\n\n"))
	if len(evs) != 1 || string(evs[0].Data) != "real" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTrailingEventWithoutBlankLine(t *testing.T) {
	evs := collect(t, strings.NewReader("data: tail"))
	if len(evs) != 1 || string(evs[0].Data) != "tail" {
		t.Fatalf("got %+v", evs)
	}
}

func TestIDField(t *testing.T) {
	evs := collect(t, strings.NewReader("id: 42\ndata: x\n\n"))
	if len(evs) != 1 || evs[0].ID != "42" {
		t.Fatalf("got %+v", evs)
	}
}

func TestEventWithoutDataNotDispatched(t *testing.T) {
	// Per the SSE spec an event with no data lines is dropped.
	evs := collect(t, strings.NewReader("event: lonely\n\ndata: ok\n\n"))
	if len(evs) != 1 || string(evs[0].Data) != "ok" {
		t.Fatalf("got %+v", evs)
	}
}

// TestMultibyteSplitAcrossReads feeds the stream one byte at a time, which
// splits multi-byte UTF-8 characters (Chinese, emoji) across reads. The
// parser buffers bytes until a full line arrives, so characters must
// survive intact.
func TestMultibyteSplitAcrossReads(t *testing.T) {
	payload := "data: 你好，世界 🌍\n\ndata: [DONE]\n\n"
	evs := collect(t, iotest.OneByteReader(strings.NewReader(payload)))
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if got := string(evs[0].Data); got != "你好，世界 🌍" {
		t.Errorf("data = %q", got)
	}
}
