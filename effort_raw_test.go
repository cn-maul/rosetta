package rosetta

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// EffortRaw lets a caller forward a thinking level this package does not
// know about. The pre-existing suite passing unchanged is the
// backward-compatibility proof; these cases pin the new behavior itself.

// planRaw builds a payload through one request against a stub upstream,
// returning the decoded wire body.
//
// The captured body crosses a goroutine boundary (the handler writes it, the
// test reads it), and socket ordering is not something the race detector
// models — so the handoff goes through a buffered channel, which gives a
// happens-before edge the detector does understand. Without it the helpers
// are latent flakes under -race rather than under normal load.
func planRaw(t *testing.T, req *ChatRequest, proto Protocol) map[string]any {
	t.Helper()
	captured := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- body
		if proto == ProtoAnthropic {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant",
				"content":[{"type":"text","text":"ok"}],"model":"m",
				"stop_reason":"end_turn",
				"usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(proto))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.Chat(t.Context(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	select {
	case got := <-captured:
		if got == nil {
			t.Fatal("上游收到���空请求体")
		}
		return got
	default:
		t.Fatal("上游没收到请求体")
		return nil
	}
}

// assertNoEffortAnywhere fails when any key anywhere in body mentions
// "effort" (case-insensitive), at any depth.
//
// A top-level-only scan is what the Anthropic isolation test originally did,
// and it is too narrow: a regression that leaked the value at the payload
// root, or under a differently-named key like "level"/"effort_level", would
// sail straight through it. Walking the whole tree is the only assertion
// that actually pins "EffortRaw never reaches this protocol".
func assertNoEffortAnywhere(t *testing.T, body map[string]any) {
	t.Helper()
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, inner := range t2 {
				if strings.Contains(strings.ToLower(k), "effort") {
					t.Errorf("%s%s 不该存在（EffortRaw 泄漏）：%v", prefix, k, inner)
				}
				walk(prefix+k+".", inner)
			}
		case []any:
			for _, inner := range t2 {
				walk(prefix+"[].", inner)
			}
		}
	}
	walk("", body)
}

func TestEffortRaw_Chat原样送达(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{EffortRaw: "xhigh"},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v，期望原样 xhigh", body["reasoning_effort"])
	}
}

// TestEffortRaw_优先级高于Effort 钉住「显式原样值胜出」。若这里改成
// Effort 优先，一个同时设了两者的调用方会被静默降级 —— 而那正是本字段
// 要消除的那类行为。
func TestEffortRaw_优先级高于Effort(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortMedium, EffortRaw: "xhigh"},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("EffortRaw 应压过 Effort，实际 %v", body["reasoning_effort"])
	}
}

func TestEffortRaw_Responses原样送达(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{EffortRaw: "minimal"},
	}, ProtoOpenAIResponses)

	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("payload 缺 reasoning：%v", body)
	}
	if reasoning["effort"] != "minimal" {
		t.Fatalf("reasoning.effort = %v，期望 minimal", reasoning["effort"])
	}
}

// TestEffortRaw_Anthropic不发送 是本字段最重要的一条边界：
// Anthropic 没有 effort 字段，把原样值塞进去（或折算成预算）都是发明
// 协议。语义只能来自 BudgetTokens。
func TestEffortRaw_Anthropic不发送(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:           "claude",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 4096,
		Thinking:        &ThinkingConfig{EffortRaw: "xhigh", BudgetTokens: 8192},
	}, ProtoAnthropic)

	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("payload 缺 thinking：%v", body)
	}
	// 预算原样通过。
	if b, _ := thinking["budget_tokens"].(float64); int(b) != 8192 {
		t.Fatalf("budget_tokens = %v，期望 8192 原样通过", thinking["budget_tokens"])
	}
	// Anthropic 的 thinking 对象只有 type 与 budget_tokens 两个合法成员，
	// 多一个都是协议外的发明。断言精确形状比扫描键名更强：它连
	// "level"/"thinking_level" 这类不含 effort 字样的泄漏也能抓住。
	if len(thinking) != 2 || thinking["type"] != "enabled" {
		t.Fatalf("thinking 应恰为 {type:enabled, budget_tokens:…}，实际 %v", thinking)
	}
	// 再叠一层全树扫描，覆盖泄漏到 payload 根部的情况。
	assertNoEffortAnywhere(t, body)
}

// TestEffortRaw_未设置时行为不变 是零回归的正面证明：不设 EffortRaw 时
// 走的就是既有的 effort() 路径。
func TestEffortRaw_未设置时行为不变(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortHigh},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v，期望 high", body["reasoning_effort"])
	}
}

// TestEffortRaw_空Thinking仍发medium 守住最容易改坏的一格。
//
// Thinking 非 nil 但三个字段都没设时，effort() 的既定行为是「请求思考但
// 不给档位 → 按 medium」。引入 effortWire 时若顺手把它写成「算不出档位就
// 不发 reasoning_effort」，这个请求会安静地变成非思考请求 —— 上游照样返回
// 200 和一段答案，没有任何症状，而客户端的思考意图消失了。
//
// 同理 temperature/top_p：emitReasoning 由「有没有档位」驱动，而这两个参数
// 又由 emitReasoning 驱动，所以这一格错了会连带把采样参数也放行。
func TestEffortRaw_空Thinking仍发medium(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "medium" {
		t.Fatalf("空 Thinking 应按既定行为发 medium，实际 %v（该值不存在即退化成非思考请求）", body["reasoning_effort"])
	}
}

// TestEffortRaw_思考时丢弃采样参数 钉住 emitReasoning → 丢弃 temperature/top_p
// 这条联动。自定义档位恰恰是最需要触发它的一条路径（发出去的值不再是 SDK
// 认识的枚举，任何回归都可能让 reasoning 状态算错），所以不能只靠既有用例
// 间接覆盖。
func TestEffortRaw_思考时丢弃采样参数(t *testing.T) {
	temp := 0.3
	topP := 0.9
	body := planRaw(t, &ChatRequest{
		Model:       "gpt-5",
		Messages:    []Message{User("hi")},
		Temperature: &temp,
		TopP:        &topP,
		Thinking:    &ThinkingConfig{EffortRaw: "xhigh"},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	if _, ok := body["temperature"]; ok {
		t.Fatalf("思考请求不该带 temperature：%v", body["temperature"])
	}
	if _, ok := body["top_p"]; ok {
		t.Fatalf("思考请求不该带 top_p：%v", body["top_p"])
	}
}

// TestEffortRaw_原样值压过预算 覆盖文档承诺的三级优先级的**最下一级**：
// EffortRaw > Effort > BudgetTokens。原本只测了中间一级（压过 Effort），而
// 这里的降级恰恰是这个字段要防的那一类静默丢失。
func TestEffortRaw_原样值压过预算(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{EffortRaw: "xhigh", BudgetTokens: 20000},
	}, ProtoOpenAIChat)

	// 20000 的预算本会映射成 high；原样值必须赢。
	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("EffortRaw 应压过 BudgetTokens，实际 %v", body["reasoning_effort"])
	}
}

// TestEffortRaw_空串视为未设置 证明 "" 不被当成一个「空的档位」发出去，
// 而是回落到既有的预算映射。
func TestEffortRaw_空串视为未设置(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{EffortRaw: "", BudgetTokens: 20000},
	}, ProtoOpenAIChat)

	if body["reasoning_effort"] != "high" {
		t.Fatalf("空 EffortRaw 应回落预算映射得 high，实际 %v", body["reasoning_effort"])
	}
}

// TestEffortRaw_预算仍映射到档位 守住 BudgetTokens 这条老路径没有被
// effortRaw 的引入挤掉：它仍然经 effort() 反查成三档发往 OpenAI 系上游。
func TestEffortRaw_预算仍映射到档位(t *testing.T) {
	body := planRaw(t, &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{BudgetTokens: 20000},
	}, ProtoOpenAIChat)

	// 20000 > 16384 → 最高档 high
	if body["reasoning_effort"] != "high" {
		t.Fatalf("预算应映射到 high，实际 %v", body["reasoning_effort"])
	}
}

func TestEffortRaw_校验拒绝畸形值(t *testing.T) {
	// 本包刻意**不**校验取值是否已知（原样透传正是它的用途），但必须
	// 拒绝上不了线的形状 —— 否则失败会以一个看不懂的上游 400 出现。
	// 断言的是错误**类别**（ErrInvalidRequest）而不只是非 nil：只判非 nil
	// 的话，一个把这些输入变成传输错误或上游畸形错误的回归照样能过。
	cases := []struct {
		name string
		raw  string
	}{
		{"仅空白", "   "},
		{"首尾空白", " xhigh "},
		{"含引号", `x"high`},
		{"含反斜杠", `x\high`},
		{"含控制字符", "xh\x00igh"},
		{"过长", strings.Repeat("x", 65)},
		// 非法 UTF-8：ContainsFunc 按 rune 遍历，坏字节解码成 RuneError，
		// 匹配不上任何被拒的字符，于是会一路放行并被 json.Marshal 悄悄
		// 改写成 U+FFFD —— 上游收到的是调用方从未写过的值。
		{"非法UTF8", "xh\xffigh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &ChatRequest{
				Model:    "m",
				Messages: []Message{User("hi")},
				Thinking: &ThinkingConfig{EffortRaw: c.raw},
			}
			err := r.validate()
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("EffortRaw=%q 应被 ErrInvalidRequest 拒绝，实际 %v", c.raw, err)
			}
		})
	}
}

// TestEffortRaw_校验接受真实取值 是「不过度收紧」的防线。
//
// 收紧校验看起来总像是改进，但它会在无人察觉的情况下挡住某个厂商的合法
// 档位 —— 而这个字段存在的全部意义就是能原样送出没见过的值。所以必须把
// 真实世界的取值钉进用例。
func TestEffortRaw_校验接受真实取值(t *testing.T) {
	for _, raw := range []string{
		"none", "minimal", "low", "medium", "high", "xhigh",
		"max", "ultra-deep", "max_output_tokens", "GPT-5-HIGH", // 大小写原样
		"openai/gpt-5:xhigh",
		strings.Repeat("x", 64), // 恰好等于上限
		"\x7f",                  // DEL：JSON 合法且 encoding/json 不转义，放行是对的
	} {
		t.Run(raw, func(t *testing.T) {
			r := &ChatRequest{
				Model:    "m",
				Messages: []Message{User("hi")},
				Thinking: &ThinkingConfig{EffortRaw: raw},
			}
			if err := r.validate(); err != nil {
				t.Fatalf("真实取值 %q 被误拒：%v", raw, err)
			}
		})
	}
}

// TestEffortRaw_畸形值在发出请求前就失败 把校验钉在数据面上：畸形值必须
// 变成一个本地 ErrInvalidRequest，且**一个字节都不能发到上游**。
//
// 只在 validate() 层面断言不够 —— 真正要保证的是「不会花钱、不会在上游
// 侧留下垃圾请求」。服务端因此直接判定「收到过请求」。
// atomic 而非普通 bool：handler 在另一个 goroutine 上写、测试在当前 goroutine
// 上读，两者之间的顺序只由 socket 保证，而竞态检测器不建模 socket —— 用普通
// bool 会在 -race 下报出真实存在的竞态（无它在普通运行下也只是「碰巧」安全）。
func TestEffortRaw_畸形值在发出请求前就失败(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIChat))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, err = c.Chat(t.Context(), &ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{EffortRaw: "x\"high"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("期望 ErrInvalidRequest，实际 %v", err)
	}
	if hit.Load() {
		t.Fatal("畸形值不该发出任何请求")
	}
}

// TestModelInfo_EffortLevels并入能力 钉住「配了挡位就等于声明能思考」：
// 少了这条，档案会出现「有挡位但不支持思考」这种调用方无法处理的状态。
func TestModelInfo_EffortLevels并入能力(t *testing.T) {
	var r Registry
	if err := r.SetManual([]ModelInfo{{ID: "gpt-5", EffortLevels: []string{"low", "xhigh"}}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	m, ok := r.Lookup("gpt-5")
	if !ok {
		t.Fatal("查不到 gpt-5")
	}
	if !m.SupportsThinking {
		t.Fatal("声明了挡位却没声明支持思考 —— 两者必须一致")
	}
	if len(m.EffortLevels) != 2 || m.EffortLevels[1] != "xhigh" {
		t.Fatalf("挡位 = %v", m.EffortLevels)
	}
}

// TestModelInfo_DisableThinking压过挡位 是上一条的反向：显式撤销必须赢。
// 否则一份「关掉了思考」的档案会被自己附的挡位复活。
//
// 同时钉住**撤销会清掉挡位**：留着它们就等于对外发布「这个模型不能思考，
// 但可拨的档位有 high」——一个自相矛盾的条目，而它对下游是实害的：前端
// 网关正是读这份列表来告诉客户端「可选哪几档」。
func TestModelInfo_DisableThinking压过挡位(t *testing.T) {
	var r Registry
	if err := r.SetManual([]ModelInfo{{
		ID: "gpt-5", DisableThinking: true, EffortLevels: []string{"high"},
	}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	m, ok := r.Lookup("gpt-5")
	if !ok {
		t.Fatal("Lookup(\"gpt-5\") 未命中")
	}
	if m.SupportsThinking {
		t.Fatal("DisableThinking 应压过 EffortLevels")
	}
	if len(m.EffortLevels) != 0 {
		t.Fatalf("撤销后不该残留挡位（会向调用方发布用不了的选项）：%v", m.EffortLevels)
	}
}

// TestModelInfo_撤销清掉继承来的挡位 覆盖跨层继承路径：低优先级层配了挡位、
// 高优先级层撤销思考，合并结果同样不能残留挡位。
//
// 这条曾经是真实缺陷 —— 撤销只清了布尔，挡位原样留下，产出的正是注释里
// 声称「不该出现」的那个自相矛盾条目。
func TestModelInfo_撤销清掉继承来的挡位(t *testing.T) {
	var r Registry
	if err := r.SetRemote([]ModelInfo{{ID: "m", EffortLevels: []string{"low", "medium"}}}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if err := r.SetManual([]ModelInfo{{ID: "m", DisableThinking: true}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	m, ok := r.Lookup("m")
	if !ok {
		t.Fatal("Lookup(\"m\") 未命中")
	}
	if m.SupportsThinking {
		t.Fatal("高优先级层的撤销应生效")
	}
	if len(m.EffortLevels) != 0 {
		t.Fatalf("撤销后不该残留从低层继承来的挡位：%v", m.EffortLevels)
	}
}

// TestModelInfo_仅声明挡位也算主张思考 覆盖 C23 规则里「声明支持思考」的两种
// 写法。旧读法只看裸布尔，于是「只用挡位声明」这种最自然的写法会被低优先级层
// 的撤销压掉 —— 而它与直接写 SupportsThinking=true 是同一个主张。
func TestModelInfo_仅声明挡位也算主张思考(t *testing.T) {
	var r Registry
	if err := r.SetRemote([]ModelInfo{{ID: "m", DisableThinking: true}}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	// 手工层只用挡位声明思考，没有写 SupportsThinking。
	if err := r.SetManual([]ModelInfo{{ID: "m", EffortLevels: []string{"high"}}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	m, ok := r.Lookup("m")
	if !ok {
		t.Fatal("Lookup(\"m\") 未命中")
	}
	if !m.SupportsThinking {
		t.Fatal("手工层声明挡位即为主张思考，不该被低层的撤销压掉")
	}
	if m.DisableThinking {
		t.Fatal("手工层没有撤销，DisableThinking 不应为真")
	}
	if len(m.EffortLevels) != 1 || m.EffortLevels[0] != "high" {
		t.Fatalf("挡位应保留：%v", m.EffortLevels)
	}
}

// TestModelInfo_EffortLevels深拷贝 守住快照契约：返回给调用方的切片
// 不得与注册表共享，否则调用方一改就把整个注册表改了（与 Aliases 同款）。
func TestModelInfo_EffortLevels深拷贝(t *testing.T) {
	var r Registry
	src := []string{"low", "high"}
	if err := r.SetManual([]ModelInfo{{ID: "m", EffortLevels: src}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	got, ok := r.Lookup("m")
	if !ok {
		t.Fatal(`Lookup("m") 未命中`)
	}
	got.EffortLevels[0] = "MUTATED"

	again, ok := r.Lookup("m")
	if !ok {
		t.Fatal(`Lookup("m") 未命中`)
	}
	if again.EffortLevels[0] != "low" {
		t.Fatalf("注册表状态被调用方改写了：%v", again.EffortLevels)
	}
}

// TestModelInfo_挡位随低优先级层回落 与 Aliases 同语义：高优先级条目
// 没提挡位时，沿用低优先级的。
func TestModelInfo_挡位随低优先级层回落(t *testing.T) {
	var r Registry
	if err := r.SetRemote([]ModelInfo{{ID: "m", EffortLevels: []string{"low", "medium"}}}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if err := r.SetManual([]ModelInfo{{ID: "m", DisplayName: "M"}}); err != nil {
		t.Fatalf("SetManual: %v", err)
	}
	m, ok := r.Lookup("m")
	if !ok {
		t.Fatal("Lookup(\"m\") 未命中")
	}
	if len(m.EffortLevels) != 2 {
		t.Fatalf("挡位没有从低优先级层回落：%+v", m)
	}
	if m.DisplayName != "M" {
		t.Fatalf("高优先级字段应胜出：%+v", m)
	}
}
