// Package sse implements a minimal Server-Sent Events parser over an
// io.Reader. It follows the text/event-stream wire format: LF / CRLF line
// endings, multi-line "data" fields joined with LF, comment lines starting
// with ':', and optional whitespace after the field colon.
//
// It is intentionally protocol agnostic: callers interpret event names and
// data payloads (e.g. the "[DONE]" sentinel used by OpenAI-style streams).
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Event is a single decoded SSE event.
type Event struct {
	Name string // value of the "event:" field; "" when absent
	ID   string // value of the "id:" field of this event
	Data []byte // "data:" lines joined with LF
}

// Scanner reads SSE events one by one from an io.Reader. It buffers whatever
// the transport delivers, so multi-byte UTF-8 sequences split across reads
// are handled transparently (bytes are only interpreted once a full line
// has arrived).
type Scanner struct {
	r    *bufio.Reader
	name string
	id   string
	data [][]byte
}

// New returns a Scanner reading from r.
func New(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReaderSize(r, 32<<10)}
}

// Next returns the next complete event, or io.EOF when the stream ends.
// A trailing event without a terminating blank line is still delivered
// (tolerance for misbehaving servers) before EOF is reported.
func (s *Scanner) Next() (*Event, error) {
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if ev := s.take(); ev != nil {
					return ev, nil
				}
			}
			return nil, err
		}
		switch {
		case len(line) == 0: // blank line: dispatch
			if ev := s.take(); ev != nil {
				return ev, nil
			}
		case line[0] == ':': // comment / keep-alive
		default:
			field, value := splitField(line)
			switch string(field) {
			case "event":
				s.name = string(value)
			case "data":
				s.data = append(s.data, value)
			case "id":
				s.id = string(value)
			default:
				// "retry" and unknown fields are ignored
			}
		}
	}
}

// take flushes the accumulated event, or returns nil when no data lines
// were collected (per the SSE spec an event without data is not dispatched).
func (s *Scanner) take() *Event {
	if len(s.data) == 0 {
		s.name = ""
		return nil
	}
	ev := &Event{Name: s.name, ID: s.id, Data: bytes.Join(s.data, []byte("\n"))}
	s.name, s.id, s.data = "", "", nil
	return ev
}

func (s *Scanner) readLine() ([]byte, error) {
	line, err := s.r.ReadBytes('\n')
	switch {
	case err == nil:
		return chomp(line), nil
	case errors.Is(err, io.EOF) && len(line) > 0:
		return chomp(line), nil // final line without terminator
	default:
		return nil, err
	}
}

// chomp removes one trailing LF, or CRLF.
func chomp(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// splitField splits "field: value" / "field:value" / "field".
func splitField(line []byte) (field, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return line[:i], value
}
