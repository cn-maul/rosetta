package repro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 复现：Anthropic 协议下 stop_sequences / service_tier 没有到达上游。
func TestAnthropicDropsStopSequencesAndServiceTier(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, err := rosetta.NewClient(
		rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("k"),
		rosetta.WithProtocol(rosetta.ProtoAnthropic),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := &rosetta.ChatRequest{
		Model:         "claude-sonnet-4-5",
		Messages:      []rosetta.Message{rosetta.User("hi")},
		StopSequences: []string{"END"},
		ServiceTier:   "auto",
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	t.Logf("上游实际收到的 payload: %s", b)
	if _, has := got["stop_sequences"]; !has {
		t.Errorf("BUG: stop_sequences 未到达上游（官方 API 2026-10-21 仍支持该字段）")
	}
	if _, has := got["service_tier"]; !has {
		t.Errorf("BUG: service_tier 未到达上游（官方 API 2026-10-21 仍支持该字段）")
	}
}
