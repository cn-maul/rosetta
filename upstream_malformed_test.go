package rosetta

// ErrUpstreamMalformed 的分类契约。
//
// # 为什么必须有这组测试（这是它当初漏掉的直接原因）
//
// 修这个缺陷之前，SDK 里所有「上游响应体解不开」的失败都被裸 fmt.Errorf 包装，
// 既不是哨兵、也不是 *APIError / *TransportError。于是调用方的错误分类器
// 三段全不命中，落到兜底的 "internal gateway error"（500），并且**不可转移**
// —— 一个「接受请求、返回垃圾」的上游永远换不掉。真实进程上复现过：
//
//	上游 200 + `{"choices": [ this is not json` → 网关回 500 internal_error，
//	故障转移链的健康次目标被调用 0 次。
//
// 而当时的测试套件里**没有**「非类型化 / 畸形响应」这一档 —— 已有的错误测试
// 全部从 *APIError / *TransportError 出发，正好绕过了那个兜底分支。
// 所以本文件的核心不只是「哨兵能匹配」，而是**把这一档错误类型补进覆盖面**。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// malformedChunk 是一个「HTTP 200 + 完全无法解析的 body」的假上游响应体。
//
// 用 HTML 错误页而不是半截 JSON：它正是真实故障里最常见的那一种
// （中间代理/网关在链路出错时回一个 HTML 502 页，而状态码已经是 200，
// 或上游把 HTML 和 200 一起返回）。半截 JSON 也覆盖，见下面的分档。
const malformedHTML = `<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>`

// malformedUpstream 起一个恒回 (status, body) 的假上游。
// 不打外网：全部走 httptest。
func malformedUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMalformedClient 造一个指向假上游的客户端（显式协议，不触发探测）。
func newMalformedClient(t *testing.T, url string, proto Protocol, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithEndpoint(url),
		WithAPIKey("sk-test"),
		WithProtocol(proto),
	}
	c, err := NewClient(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------
// 一元路径：每个改动点都必须命中哨兵
// ---------------------------------------------------------------------

// 三个 chat 协议的**一元**响应体解不开 → 必须命中 ErrUpstreamMalformed。
//
// 用子测试逐协议跑：三处是各自独立的解码函数（decodeOpenAIChatResponse /
// decodeAnthropicResponse / decodeResponsesResponse），漏改任何一处，
// 对应的子测试就会红 —— 这正是「一族缺陷不能只修一处」的保障。
func TestUpstreamMalformed_UnaryChatResponses(t *testing.T) {
	cases := []struct {
		name  string
		proto Protocol
		body  string
	}{
		{"openai-chat/HTML错误页", ProtoOpenAIChat, malformedHTML},
		{"openai-chat/半截JSON", ProtoOpenAIChat, `{"choices": [ this is not json`},
		{"openai-chat/空body", ProtoOpenAIChat, ``},
		{"anthropic/HTML错误页", ProtoAnthropic, malformedHTML},
		{"anthropic/半截JSON", ProtoAnthropic, `{"content": [ {"type": "text", `},
		{"responses/HTML错误页", ProtoOpenAIResponses, malformedHTML},
		{"responses/半截JSON", ProtoOpenAIResponses, `{"output": [ {"type": "message"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := malformedUpstream(t, http.StatusOK, tc.body)
			c := newMalformedClient(t, srv.URL, tc.proto)

			var err error
			switch tc.proto {
			case ProtoAnthropic:
				_, err = c.Chat(context.Background(), &ChatRequest{
					Model: "m", MaxOutputTokens: 16, Messages: []Message{User("hi")},
				})
			default:
				_, err = c.Chat(context.Background(), &ChatRequest{
					Model: "m", Messages: []Message{User("hi")},
				})
			}
			if err == nil {
				t.Fatal("畸形的一元响应体必须报错，实际返回 nil")
			}

			t.Logf("err = %v", err)
			if !errors.Is(err, ErrUpstreamMalformed) {
				t.Errorf("未命中 ErrUpstreamMalformed：%v\n"+
					"（这正是原缺陷：调用方分类器三段全不命中，落到 500 internal_error 且不可转移）", err)
			}
			// 反向：畸形响应**不该**被误判成调用方的错或上游的 HTTP 错误。
			if errors.Is(err, ErrInvalidRequest) {
				t.Errorf("畸形上游响应被误判成 ErrInvalidRequest —— 会把上游的故障归咎于调用方")
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				t.Errorf("畸形上游响应被误判成 *APIError（%v）—— 会让调用方按 HTTP 语义处置", apiErr)
			}
			var transportErr *TransportError
			if errors.As(err, &transportErr) {
				t.Errorf("畸形上游响应被误判成 *TransportError（%v）—— 会让调用方当成网络故障重试", transportErr)
			}
		})
	}
}

// embedding / rerank 的畸形响应同样命中哨兵。
//
// 这两条是**辅助 API**，与 chat 走不同的解码函数；漏改它们的话，
// 使用 embeddings 的调用方仍会拿到不可分类的错误。
func TestUpstreamMalformed_AuxiliaryAPIs(t *testing.T) {
	t.Run("embedding", func(t *testing.T) {
		srv := malformedUpstream(t, http.StatusOK, malformedHTML)
		c := newMalformedClient(t, srv.URL, ProtoOpenAIChat, WithEmbeddingEndpoint(srv.URL))

		_, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "m", Input: []string{"x"}})
		if err == nil {
			t.Fatal("畸形的 embeddings 响应必须报错")
		}
		t.Logf("embedding err = %v", err)
		if !errors.Is(err, ErrUpstreamMalformed) {
			t.Errorf("embeddings 畸形响应未命中哨兵：%v", err)
		}
	})

	t.Run("rerank", func(t *testing.T) {
		srv := malformedUpstream(t, http.StatusOK, malformedHTML)
		c := newMalformedClient(t, srv.URL, ProtoOpenAIChat, WithEmbeddingEndpoint(srv.URL))

		_, err := c.Rerank(context.Background(), &RerankRequest{
			Model: "m", Query: "q", Documents: []string{"d"},
		})
		if err == nil {
			t.Fatal("畸形的 rerank 响应必须报错")
		}
		t.Logf("rerank err = %v", err)
		if !errors.Is(err, ErrUpstreamMalformed) {
			t.Errorf("rerank 畸形响应未命中哨兵：%v", err)
		}
	})
}

// /models 目录畸形 → 命中哨兵（OpenAI 兼容与 Anthropic 两条解码路径都测）。
//
// 这条路径容易被忽略：它不是推理调用，但同样是「解上游响应体」，
// 而且它自己还有一条 `{"data": null}` 的结构性检查（audit B11），
// 那条也必须归到同一类 —— 否则就在修复点下一行重新开了一个分类漏洞。
func TestUpstreamMalformed_ModelCatalog(t *testing.T) {
	cases := []struct {
		name   string
		proto  Protocol
		body   string
		reason string
	}{
		{"openai/HTML", ProtoOpenAIChat, malformedHTML, "目录体解不开"},
		{"openai/data为null", ProtoOpenAIChat, `{"data":null}`, "缺必需字段（结构性缺陷，与解不开同类）"},
		{"openai/空对象", ProtoOpenAIChat, `{}`, "缺必需字段"},
		{"anthropic/HTML", ProtoAnthropic, malformedHTML, "目录体解不开"},
		// 与 openai/data为null 分档对称：两条目录解码路径各有自己的
		// `list.Data == nil` 检查（provider_openai_common.go 与
		// provider_anthropic.go），两处都打了同一个哨兵。只测一条路径
		// 会让另一条路径的改动变成无覆盖 —— 而这正是本文件要防的漏改形态。
		{"anthropic/data为null", ProtoAnthropic, `{"data":null}`, "缺必需字段（结构性缺陷，与解不开同类）"},
		{"anthropic/空对象", ProtoAnthropic, `{}`, "缺必需字段"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := malformedUpstream(t, http.StatusOK, tc.body)
			c := newMalformedClient(t, srv.URL, tc.proto)

			_, err := c.ListModels(context.Background())
			if err == nil {
				t.Fatal("畸形的模型目录必须报错")
			}
			t.Logf("err = %v（%s）", err, tc.reason)
			if !errors.Is(err, ErrUpstreamMalformed) {
				t.Errorf("模型目录畸形未命中哨兵：%v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 反向：真正的上游/本地错误**不得**命中哨兵
// ---------------------------------------------------------------------
//
// 这一组比正向更重要。若哨兵被套得太宽（例如把 *APIError 也包进去），
// 调用方会把「上游明确回了 401/429/404」当成「响应畸形」——
// 而两者的处置相反：前者要换凭据/退避，后者要换上游。
// 一个过宽的哨兵会把整个错误分类体系压平，比不加哨兵更糟。
func TestUpstreamMalformed_DoesNotCatchTypedErrors(t *testing.T) {
	cases := []struct {
		name string
		// err 是各条错误路径的真实产物形态。
		err error
		why string
	}{
		{
			"上游500",
			&APIError{StatusCode: 500, Method: "POST", URL: "http://x", Message: "boom"},
			"上游明确的 HTTP 错误：应按状态码退避/转移，不是「解不开」",
		},
		{
			"上游429",
			&APIError{StatusCode: 429, Method: "POST", URL: "http://x", Retryable: true},
			"限流：应退避，与畸形响应无关",
		},
		{
			"上游401",
			&APIError{StatusCode: 401, Method: "POST", URL: "http://x"},
			"鉴权失败：应换凭据，不是换上游",
		},
		{
			"信内错误(InBand)",
			&APIError{StatusCode: 200, InBand: true, Method: "POST", URL: "http://x", Message: "overloaded"},
			"HTTP 200 但体内带 error：StatusCode 是 200，最容易被误当成畸形响应",
		},
		{
			"传输层错误",
			&TransportError{Method: "POST", URL: "http://x", Err: errors.New("dial tcp: refused")},
			"网络故障：是完全不同的类别",
		},
		{
			"调用方请求非法",
			fmt.Errorf("%w: Model is required", ErrInvalidRequest),
			"本地错误：必须保持 ErrInvalidRequest，方向与上游故障相反",
		},
		{
			"流截断",
			fmt.Errorf("%w: openai-chat stream ended without [DONE]", ErrStreamTruncated),
			"流哨兵：独立类别，且刻意不并入畸形响应（见流式测试）",
		},
		{
			"流溢出",
			fmt.Errorf("%w: accumulation exceeded", ErrStreamOverflow),
			"流哨兵：同上",
		},
		{
			"未建模的普通错误",
			errors.New("some unrelated failure"),
			"普通错误不该被误吸进哨兵",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if errors.Is(tc.err, ErrUpstreamMalformed) {
				t.Errorf("%s：%v 被误判成 ErrUpstreamMalformed —— %s", tc.name, tc.err, tc.why)
			}
		})
	}
}

// 反向的端到端版：上游回**真实 HTTP 错误**时，错误必须是 *APIError，
// 且**不该**被套上畸形哨兵。
//
// 只测构造出来的 *APIError 不够 —— 那样测的是「我构造的对象长什么样」，
// 而不是「解码路径产出什么」。这条走真实 HTTP 状态码。
func TestUpstreamMalformed_RealHTTPErrorIsNotMalformed(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status%d", status), func(t *testing.T) {
			// 一个**合法**的 OpenAI 错误体：能被 parseOpenAIError 解析。
			srv := malformedUpstream(t, status,
				`{"error":{"message":"upstream said no","type":"invalid_request_error"}}`)
			c := newMalformedClient(t, srv.URL, ProtoOpenAIChat)

			_, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
			if err == nil {
				t.Fatalf("HTTP %d 必须报错", status)
			}
			t.Logf("err = %v", err)

			if errors.Is(err, ErrUpstreamMalformed) {
				t.Errorf("HTTP %d 的合法错误体被误判成畸形响应：%v", status, err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("HTTP %d 应产出 *APIError，实际 %T: %v", status, err, err)
			}
			if apiErr.StatusCode != status {
				t.Errorf("APIError.StatusCode = %d, want %d", apiErr.StatusCode, status)
			}
		})
	}
}

// ---------------------------------------------------------------------
// errors.Is 必须穿透多层包装
// ---------------------------------------------------------------------

// 哨兵在任意层包装下都必须命中。
//
// 这条不是走形式：真实链路上错误会被装饰好几层（SDK 内部 fmt.Errorf、
// 调用方的重试包装、故障转移的归因包装）。若某一层用 %v 而不是 %w，
// 哨兵就在那里断了 —— 而 errors.Is 断链是**静默**的（不会报错，
// 只是分类悄悄变成「未知错误」），所以必须有测试钉住。
func TestUpstreamMalformed_ErrorsIsPenetratesWrapping(t *testing.T) {
	base := fmt.Errorf("%w: openai-chat response: %w", ErrUpstreamMalformed,
		errors.New("invalid character '<' looking for beginning of value"))

	layered := []struct {
		name string
		err  error
	}{
		{"原始", base},
		{"一层%w", fmt.Errorf("retry wrapper: %w", base)},
		{"两层%w", fmt.Errorf("failover: %w", fmt.Errorf("attempt 1: %w", base))},
		{
			"与其它哨兵并列",
			errors.Join(fmt.Errorf("attempt: %w", base), errors.New("noise")),
		},
	}
	for _, l := range layered {
		t.Run(l.name, func(t *testing.T) {
			if !errors.Is(l.err, ErrUpstreamMalformed) {
				t.Errorf("%s：errors.Is 未穿透 —— 上层会把它当成未知错误：%v", l.name, l.err)
			}
		})
	}

	// 反向：%v（而非 %w）会**故意**断链。这里显式钉住这个语义，
	// 因为它是「有人改用 %v 时分类会静默失效」的文档化证据。
	//
	// 措辞上刻意避开在 t.Error 的字符串里出现 %v 字面量 —— go vet 的
	// printf 检查会把它当成「本该用 Errorf 而用了 Error」而报错。
	// （实测踩过：这个坑本身与被测逻辑无关，纯属字符串巧合。）
	broken := fmt.Errorf("no wrap: %v", base)
	if errors.Is(broken, ErrUpstreamMalformed) {
		t.Error("用非 wrap 动词包装时不该保留哨兵链 —— 若这条通过，说明 Go 语义变了，本文件的其它断言需要重审")
	}
}

// 端到端版的穿透：从一个**真实**的 HTTP 往返里拿到的错误，
// 经过真实装饰路径后仍必须命中。
//
// 与上面的合成包装互补：上一条测 errors.Is 的语义，这条测 SDK 真的把
// 哨兵放进了错误链（而不是只在合成例子里成立）。
func TestUpstreamMalformed_EndToEndErrorsIs(t *testing.T) {
	srv := malformedUpstream(t, http.StatusOK, malformedHTML)
	c := newMalformedClient(t, srv.URL, ProtoOpenAIChat)

	_, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("必须报错")
	}

	// 模拟调用方的分层处理（真实网关就是这么包的）。
	consumer := fmt.Errorf("gateway attempt 1: %w", err)
	outer := fmt.Errorf("failover loop: %w", consumer)

	if !errors.Is(outer, ErrUpstreamMalformed) {
		t.Fatalf("从真实 HTTP 往返取的错误穿透两层后未命中哨兵：%v", outer)
	}
	// 端到端确认「不是其它类别」，因为调用方正是靠这个三选一来决定动作。
	if errors.Is(outer, ErrInvalidRequest) {
		t.Error("真实畸形响应被误判成 ErrInvalidRequest")
	}
	var apiErr *APIError
	if errors.As(outer, &apiErr) {
		t.Error("真实畸形响应被误判成 *APIError")
	}
}

// ---------------------------------------------------------------------
// 流式路径：**明确**不做分类（并把这个决定钉住）
// ---------------------------------------------------------------------

// 流式事件畸形 → 错误**不得**带 ErrUpstreamMalformed。
//
// # 为什么这条测试是"反向"的
//
// 初看之下，流式事件解不开也是「上游响应畸形」，理应命中同一个哨兵。
// 但两者的**处置相反**：
//
//   - 一元响应解不开时，调用方还没有任何内容，换一个上游重试是安全且
//     正确的（这正是哨兵要支撑的故障转移）。
//   - 流式事件解不开时，**先前的事件可能已经交付给调用方了**。此时
//     "errors.Is(...) → 换上游重试" 会重放已交付的输出并可能重复计费。
//
// 一个哨兵无法同时表达「可安全重试」与「不可重试」——因为安全性取决于
// 调用方走到了哪一步，而错误本身不知道。所以流式保持无类型，
// 由调用方按自己的 committed 状态决定（网关侧就是这么做的，见
// rosetta-gateway 的 TestE2E_C3：首片之后掐断**不得**触碰健康次目标）。
//
// 把它标上哨兵会让消费者最自然的映射变成正确性 bug，所以这里必须钉死。
func TestUpstreamMalformed_StreamEventIsDeliberatelyUntyped(t *testing.T) {
	// 一条件流的 SSE 事件（不是合法 JSON），前面已经有一片正常内容。
	const streamBody = "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {this is not valid json at all\n\n"

	cases := []struct {
		name  string
		proto Protocol
	}{
		{"openai-chat", ProtoOpenAIChat},
		{"anthropic", ProtoAnthropic},
		{"responses", ProtoOpenAIResponses},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl, _ := w.(http.Flusher)
				_, _ = fmt.Fprint(w, streamBody)
				if fl != nil {
					fl.Flush()
				}
			}))
			t.Cleanup(srv.Close)

			c := newMalformedClient(t, srv.URL, tc.proto)
			var req *ChatRequest
			if tc.proto == ProtoAnthropic {
				req = &ChatRequest{Model: "m", MaxOutputTokens: 16, Messages: []Message{User("hi")}}
			} else {
				req = &ChatRequest{Model: "m", Messages: []Message{User("hi")}}
			}

			s, err := c.ChatStream(context.Background(), req)
			if err != nil {
				// 建连阶段就失败也可以接受（取决于协议/上游），
				// 但那样这条测试没测到「事件解析」，必须显式说出来。
				t.Skipf("建流阶段即失败（未覆盖事件解析）：%v", err)
			}
			streamErr := drainStream(t, s)

			if streamErr == nil {
				t.Fatal("畸形的事件流必须让 Stream.Err() 非 nil（静默丢弃会损坏文本/tool 参数）")
			}
			t.Logf("stream err = %v", streamErr)

			// 核心反向断言。
			if errors.Is(streamErr, ErrUpstreamMalformed) {
				t.Errorf("流式事件解析失败被标上了 ErrUpstreamMalformed：%v\n"+
					"这会让消费者的 `errors.Is(...) -> 换上游重试` 重放已交付的输出并重复计费。\n"+
					"流式路径必须保持无类型，由调用方按 committed 状态决定。", streamErr)
			}
			// 也不该被误判成调用方的错：确实是上游的坏数据，只是不可安全重试。
			if errors.Is(streamErr, ErrInvalidRequest) {
				t.Errorf("流式事件畸形被误判成 ErrInvalidRequest：%v", streamErr)
			}
		})
	}
}

// 一元路径**必须**与流式路径区分开：同一份畸形字节，一元要哨兵、流式不要。
//
// 这条是上面两条的交叉验证 —— 防止有人「统一一下」把两边改成一样。
func TestUpstreamMalformed_UnaryVsStreamAreDeliberatelyDifferent(t *testing.T) {
	// 一元：整份 body 就是畸形 JSON。
	srvUnary := malformedUpstream(t, http.StatusOK, `{"choices": [ this is not json`)
	cUnary := newMalformedClient(t, srvUnary.URL, ProtoOpenAIChat)
	_, unaryErr := cUnary.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if unaryErr == nil {
		t.Fatal("一元畸形必须报错")
	}

	// 流式：事件畸形。
	srvStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {not json\n\n")
	}))
	t.Cleanup(srvStream.Close)
	cStream := newMalformedClient(t, srvStream.URL, ProtoOpenAIChat)
	s, serr := cStream.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	var streamErr error
	if serr != nil {
		streamErr = serr
	} else {
		streamErr = drainStream(t, s)
	}
	if streamErr == nil {
		t.Fatal("流式畸形事件必须报错")
	}

	unaryTagged := errors.Is(unaryErr, ErrUpstreamMalformed)
	streamTagged := errors.Is(streamErr, ErrUpstreamMalformed)

	t.Logf("一元命中哨兵=%v（err=%v）", unaryTagged, unaryErr)
	t.Logf("流式命中哨兵=%v（err=%v）", streamTagged, streamErr)

	if !unaryTagged {
		t.Error("一元路径必须命中哨兵（调用方才可能转移）")
	}
	if streamTagged {
		t.Error("流式路径必须**不**命中哨兵（已交付内容后重试会重复输出/重复计费）")
	}
}

// ---------------------------------------------------------------------
// 错误消息与敏感信息
// ---------------------------------------------------------------------

// 哨兵消息的方向必须**明确是上游**：不能让人读成"你的请求有问题"。
//
// 这两种处置相反（改请求 vs 换上游），一条含糊的消息会让值班的人往错方向查。
func TestUpstreamMalformed_MessageBlamesUpstream(t *testing.T) {
	msg := ErrUpstreamMalformed.Error()
	t.Logf("消息 = %q", msg)

	// 必须点明是**上游**（upstream），而不是请求/参数。
	if !strings.Contains(msg, "upstream") {
		t.Errorf("消息 %q 未点明是上游的问题 —— 会被读成调用方请求有误，处置方向相反", msg)
	}
	// 未被明确归因于请求。
	for _, wrong := range []string{"invalid request", "your request", "request is invalid"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("消息 %q 含有指向调用方的措辞 %q", msg, wrong)
		}
	}
	// 与既有哨兵风格一致：都带 "rosetta: " 前缀。
	if !strings.HasPrefix(msg, "rosetta: ") {
		t.Errorf("消息 %q 缺少既有的 \"rosetta: \" 前缀（与其它 9 个哨兵不一致）", msg)
	}
}

// 畸形的**响应体内容**不得被塞进错误消息。
//
// 上游的畸形体可能是 HTML 错误页（含内部主机名/版本）、也可能是把调用方
// 的 key 回显进去的半截 JSON。错误消息会进日志与遥测，所以只报"解不开"，
// 不报"解不开的是什么"。
//
// 注意：这条**不是**靠 %v/%w 的选择来保证的，而是靠"根本不把 body 传进
// 错误"这个结构事实。测试用「body 里放一个显眼的秘密」来验证。
func TestUpstreamMalformed_DoesNotLeakBody(t *testing.T) {
	const secret = "sk-ant-SUPERSECRETKEY1234567890"
	// 一个把 secret 放在**语法错误附近**的畸形体：如果实现把 body 片段
	// 拼进消息，这里最容易漏出来。
	body := `{"error":"` + secret + `", "choices": [ this is not json`

	srv := malformedUpstream(t, http.StatusOK, body)
	c := newMalformedClient(t, srv.URL, ProtoOpenAIChat)

	_, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("必须报错")
	}
	msg := err.Error()
	t.Logf("err = %s", msg)

	if strings.Contains(msg, secret) {
		t.Errorf("畸形响应体的内容泄漏进了错误消息：%s", msg)
	}
	// 也不该泄漏那段 body 的其它特征片段。
	if strings.Contains(msg, "this is not json") {
		t.Errorf("错误消息里带了响应体片段：%s", msg)
	}
	// 但必须仍然可分类 —— 不泄漏的前提是分类不依赖 body 内容。
	if !errors.Is(err, ErrUpstreamMalformed) {
		t.Errorf("为了不泄漏而丢了分类：%v", err)
	}
}
