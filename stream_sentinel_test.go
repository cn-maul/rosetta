package rosetta

// Regression guard for the v0.5.0 double-prefix message: the stream
// sentinels (ErrStreamTruncated, ErrStreamOverflow) already carry the
// "rosetta: " prefix, so wrapping them must use "%w: <detail>" — the
// "rosetta: %w: <detail>" form used for *external* errors double-prefixes
// the message into "rosetta: rosetta: stream truncated: ...".
//
// errors.Is was never affected; only the human-readable string regressed.
// These tests pin the exact outward text so the prefix cannot drift again.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// truncationUpstream answers with a chunked SSE stream that is cut off
// before the terminal event, without the final zero-length chunk.
func truncationUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
		rw.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// drainStream runs a stream to completion and returns its terminal error.
func drainStream(t *testing.T, s Stream) error {
	t.Helper()
	for s.Next() {
	}
	return s.Err()
}

func TestStreamSentinelMessageHasSinglePrefix(t *testing.T) {
	cases := []struct {
		name  string
		proto Protocol
		body  string
		want  string
	}{
		{
			name:  "openai-chat",
			proto: ProtoOpenAIChat,
			body:  "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
			want:  "rosetta: stream truncated: openai-chat stream ended without [DONE]",
		},
		{
			name:  "responses",
			proto: ProtoOpenAIResponses,
			body:  "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
			want:  "rosetta: stream truncated: responses stream ended without response.completed",
		},
		{
			name:  "anthropic",
			proto: ProtoAnthropic,
			body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
			want: "rosetta: stream truncated: anthropic stream ended without message_stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := truncationUpstream(t, tc.body)
			c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(tc.proto))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := c.ChatStream(ctx, &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			err = drainStream(t, s)
			if !errors.Is(err, ErrStreamTruncated) {
				t.Fatalf("err = %v, want ErrStreamTruncated", err)
			}
			msg := err.Error()
			if strings.Contains(msg, "rosetta: rosetta:") {
				t.Fatalf("double prefix survived: %q", msg)
			}
			if !strings.HasPrefix(msg, tc.want) {
				t.Fatalf("message = %q, want prefix %q", msg, tc.want)
			}
			// The detail half must still be present.
			if !strings.Contains(msg, "partial response kept in Stream.Partial") {
				t.Fatalf("detail lost: %q", msg)
			}
		})
	}
}

func TestStreamOverflowSentinelMessageHasSinglePrefix(t *testing.T) {
	big := strings.Repeat("x", (maxStreamAccumBytes>>1)+1)
	s := newStream(seqNext(
		&Event{Type: EventTextDelta, Text: big, BlockIndex: 0},
		&Event{Type: EventTextDelta, Text: big, BlockIndex: 1},
	), nil)
	defer s.Close()

	err := drainStream(t, s)
	if !errors.Is(err, ErrStreamOverflow) {
		t.Fatalf("err = %v, want ErrStreamOverflow", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "rosetta: rosetta:") {
		t.Fatalf("double prefix survived: %q", msg)
	}
	if !strings.HasPrefix(msg, "rosetta: stream accumulation exceeded safety limits: ") {
		t.Fatalf("message = %q, want the sentinel text followed by the detail", msg)
	}
}

// Every sentinel must read as a self-contained "rosetta: ..." message: that
// prefix is what lets the "%w: detail" wrappers stay prefix-free.
func TestSentinelsCarryTheRosettaPrefix(t *testing.T) {
	for _, err := range []error{
		ErrNoEndpoint, ErrNoAPIKey, ErrUnknownModel, ErrContextTooLong,
		ErrThinkingUnsupported, ErrInvalidRequest, ErrNotSupported,
		ErrStreamTruncated, ErrStreamOverflow,
	} {
		if !strings.HasPrefix(err.Error(), "rosetta: ") {
			t.Errorf("sentinel %q lacks the rosetta: prefix", err)
		}
		if strings.Count(err.Error(), "rosetta: ") != 1 {
			t.Errorf("sentinel %q repeats the prefix: %q", err, err.Error())
		}
	}
}
