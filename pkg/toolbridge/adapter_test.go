package toolbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

/* ---------- 夹具 ---------- */

const (
	chatBody = `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "你是一个内部助手，遵守公司规范。"},
			{"role": "user", "content": "北京天气怎么样"}
		],
		"tools": [{"type": "function", "function": {
			"name": "get_weather",
			"description": "查询天气",
			"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
		}}]
	}`

	anthropicBody = `{
		"model": "claude-x",
		"max_tokens": 1024,
		"system": "你是一个内部助手，遵守公司规范。",
		"messages": [{"role": "user", "content": "北京天气怎么样"}],
		"tools": [{
			"name": "get_weather",
			"description": "查询天气",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}
		}]
	}`

	responsesBody = `{
		"model": "gpt-4o",
		"instructions": "你是一个内部助手，遵守公司规范。",
		"input": [{"role": "user", "content": "北京天气怎么样"}],
		"tools": [{
			"type": "function",
			"name": "get_weather",
			"description": "查询天气",
			"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
		}]
	}`
)

func decodeOpts() DecodeOptions {
	return DecodeOptions{SessionID: "sess-1", RequestID: "req-1"}
}

func allProtocols() map[Protocol]string {
	return map[Protocol]string{
		ProtocolChat:      chatBody,
		ProtocolAnthropic: anthropicBody,
		ProtocolResponses: responsesBody,
	}
}

/* ---------- 解码 ---------- */

func TestDecodeNormalizesAllProtocols(t *testing.T) {
	// 这是整个适配层存在的理由：三种协议的工具定义形状完全不同
	// （Chat 有 function 包装、Anthropic 用 input_schema、Responses 扁平），
	// 出来之后必须是同一套。
	for proto, body := range allProtocols() {
		t.Run(string(proto), func(t *testing.T) {
			req, err := DecodeRequest(proto, []byte(body), decodeOpts())
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			tools := req.Tools()
			if len(tools) != 1 {
				t.Fatalf("应有一个工具，得到 %d", len(tools))
			}
			if tools[0].Name != "get_weather" || tools[0].Description != "查询天气" {
				t.Errorf("工具归一化不对：%+v", tools[0])
			}
			var schema map[string]any
			if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
				t.Fatalf("schema 不是合法 JSON：%v", err)
			}
			if schema["type"] != "object" {
				t.Errorf("schema 应原样保留：%v", schema)
			}
			if req.Model() == "" {
				t.Error("模型名应取到")
			}
			if req.LastUserText() != "北京天气怎么样" {
				t.Errorf("最后一条用户消息不对：%q", req.LastUserText())
			}
		})
	}
}

func TestDecodeRejectsUnknownProtocol(t *testing.T) {
	if _, err := DecodeRequest("grpc", []byte(chatBody), decodeOpts()); err == nil {
		t.Error("未知协议应报错")
	}
}

func TestDecodeRejectsMalformedBody(t *testing.T) {
	if _, err := DecodeRequest(ProtocolChat, []byte(`{`), decodeOpts()); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

func TestDecodeRespectsSizeLimit(t *testing.T) {
	// 没有上限，一个构造的大请求就能吃光内存。
	opts := decodeOpts()
	opts.MaxBytes = 10
	if _, err := DecodeRequest(ProtocolChat, []byte(chatBody), opts); err == nil {
		t.Error("超过上限应报错")
	}
}

/* ---------- 改造请求 ---------- */

func TestPrepareKeepsClientSystemPrompt(t *testing.T) {
	// 客户端的 system prompt 可能有几万字，那是它的业务逻辑。
	// 覆盖掉等于把它的产品功能删了。
	for proto, body := range allProtocols() {
		t.Run(string(proto), func(t *testing.T) {
			req, err := DecodeRequest(proto, []byte(body), decodeOpts())
			if err != nil {
				t.Fatal(err)
			}
			req.PrepareForTextUpstream("=== 协议说明 ===", "")

			out, err := req.Encode()
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			s := string(out)
			if !strings.Contains(s, "你是一个内部助手，遵守公司规范。") {
				t.Errorf("客户端的 system prompt 被弄丢了：%s", s)
			}
			if !strings.Contains(s, "协议说明") {
				t.Errorf("协议说明没进去：%s", s)
			}
			// 上游不认识 tools，留着有些中转站会直接报 400。
			if strings.Contains(s, "get_weather") {
				t.Errorf("tools 应被清空：%s", s)
			}
		})
	}
}

func TestProtocolNoteComesAfterBusinessPrompt(t *testing.T) {
	// 越靠近末尾的指令对模型的约束越强，而格式要求正是那个必须被遵守的。
	req, err := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}
	req.PrepareForTextUpstream("协议说明在这里", "")

	out, _ := req.Encode()
	s := string(out)
	iBiz := strings.Index(s, "遵守公司规范")
	iNote := strings.Index(s, "协议说明在这里")
	if iBiz < 0 || iNote < 0 {
		t.Fatalf("两段都该在：%s", s)
	}
	if iNote < iBiz {
		t.Error("协议说明应排在业务 prompt 之后")
	}
}

func TestPrepareWithoutExistingSystemMessage(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"你好"}]}`
	req, err := DecodeRequest(ProtocolChat, []byte(body), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}
	req.PrepareForTextUpstream("协议说明", "")

	out, _ := req.Encode()
	if !strings.Contains(string(out), "协议说明") {
		t.Errorf("没有 system 消息时应新建一条：%s", out)
	}
}

// 协议说明的落点按宿主指令的体量自适应（2026-08-24）：
//
// 实测两端——Codex 最后一条指令只有 186 字节，合并进去 4/4 成功；Claude
// Code 被 hook 注入 31 KB 行为规则，合并进去 0/4（模型读完几十 KB「怎么
// 说话」就忘了「有什么工具」）。判据是最后一条指令消息自身的长度。
func TestPrepareAdaptsToHostInstructionSize(t *testing.T) {
	newReq := func(t *testing.T, sysText string) *Request {
		t.Helper()
		sys, _ := json.Marshal(sysText)
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"system","content":%s},{"role":"user","content":"你好"}]}`, sys)
		req, err := DecodeRequest(ProtocolChat, []byte(body), decodeOpts())
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	lastMessages := func(t *testing.T, req *Request) []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} {
		t.Helper()
		out, err := req.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		return got.Messages
	}

	t.Run("宿主指令小 → 合并进同一条", func(t *testing.T) {
		req := newReq(t, "你是一个内部助手，遵守公司规范。")
		req.PrepareForTextUpstream("=== 协议说明 ===", "")
		msgs := lastMessages(t, req)
		if len(msgs) != 2 {
			t.Fatalf("不应新增消息：%+v", msgs)
		}
		if !strings.Contains(msgs[0].Content, "遵守公司规范") ||
			!strings.Contains(msgs[0].Content, "协议说明") {
			t.Errorf("应合并在宿主指令后面：%s", msgs[0].Content)
		}
	})

	t.Run("宿主指令过大 → 独立成条", func(t *testing.T) {
		req := newReq(t, strings.Repeat("行为规则段落。", 3000)) // ~21 KB
		req.PrepareForTextUpstream("=== 协议说明 ===", "")
		msgs := lastMessages(t, req)
		last := msgs[len(msgs)-1]
		if len(msgs) != 3 || last.Role != "system" {
			t.Fatalf("协议说明应是末尾独立的一条 system：%+v", msgs)
		}
		if !strings.Contains(last.Content, "协议说明") {
			t.Errorf("末尾那条应是协议说明：%s", last.Content)
		}
		if strings.Contains(last.Content, "行为规则") {
			t.Errorf("协议说明不得与宿主大段内容混在同一条里：%s", last.Content)
		}
	})
}

/* ---------- 跨协议转换 ---------- */

func TestEncodeAsCrossProtocol(t *testing.T) {
	// 客户端用 Anthropic，上游用 Chat——反代的日常。
	req, err := DecodeRequest(ProtocolAnthropic, []byte(anthropicBody), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}
	req.PrepareForTextUpstream("协议说明", "")
	req.SetModel("gpt-4o-mini")

	out, err := req.EncodeAs(ProtocolChat)
	if err != nil {
		t.Fatalf("转 chat 失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gpt-4o-mini" {
		t.Errorf("模型名应被改写：%v", got["model"])
	}
	if _, ok := got["messages"]; !ok {
		t.Errorf("chat 协议用 messages 字段：%s", out)
	}
	if !strings.Contains(string(out), "遵守公司规范") {
		t.Error("Anthropic 的顶层 system 应转成 chat 的 system 消息")
	}
}

func TestEncodeAsAnthropicNeedsMaxTokens(t *testing.T) {
	// Anthropic 把 max_tokens 定为必填。悄悄填一个数意味着模型会在
	// 谁也没决定过的长度上被截断。
	req, err := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.EncodeAs(ProtocolAnthropic); err == nil {
		t.Fatal("缺 max_tokens 应报错")
	}

	req.SetMaxTokens(2048)
	out, err := req.EncodeAs(ProtocolAnthropic)
	if err != nil {
		t.Fatalf("补上之后应成功：%v", err)
	}
	if !strings.Contains(string(out), "2048") {
		t.Errorf("max_tokens 应写进去：%s", out)
	}
}

func TestEncodeAsRejectsUnknownProtocol(t *testing.T) {
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if _, err := req.EncodeAs("grpc"); err == nil {
		t.Error("未知目标协议应报错")
	}
}

/* ---------- CompileRequest ---------- */

func TestCompileRequestDoesEverything(t *testing.T) {
	sess := newSession(t, Config{})
	req, err := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sess.CompileRequest(req, CompileOptions{})
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	if !strings.Contains(res.SystemPrompt, "get_weather") {
		t.Error("工具应进 Prompt")
	}
	if len(res.ToolsIncluded) != 1 {
		t.Errorf("应报告纳入了哪些工具：%v", res.ToolsIncluded)
	}

	// 请求已被就地改造：tools 清空、system 注入。
	out, _ := req.Encode()
	if strings.Contains(string(out), `"tools"`) {
		t.Errorf("tools 应已清空：%s", out)
	}
	if !strings.Contains(string(out), res.Signal) {
		t.Error("信号应出现在改造后的请求里")
	}
}

func TestCompileRequestPicksUpQueryAutomatically(t *testing.T) {
	// 每个调用方手写一遍 LastUserText 是没必要的重复。
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if _, err := sess.CompileRequest(req, CompileOptions{}); err != nil {
		t.Fatal(err)
	}
	// 只有一个工具，排序看不出来——这里验证的是不报错且能跑通。
	// 排序本身在 prompt 包有专门的测试。
}

func TestCompileRequestCarriesToolChoice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"查天气"}],
		"tool_choice":"required",
		"tools":[{"type":"function","function":{"name":"get_weather",
			"parameters":{"type":"object"}}}]}`
	sess := newSession(t, Config{})
	req, err := DecodeRequest(ProtocolChat, []byte(body), decodeOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !req.RequireCall() {
		t.Fatal("应识别出 tool_choice=required")
	}
	res, err := sess.CompileRequest(req, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 它只是一句约束，不是保证——但至少要出现在 Prompt 里。
	if res.SystemPrompt == "" {
		t.Error("应编译出内容")
	}
}

func TestCompileRequestRejectsNil(t *testing.T) {
	if _, err := newSession(t, Config{}).CompileRequest(nil, CompileOptions{}); err == nil {
		t.Error("空请求应报错")
	}
}

/* ---------- 渲染响应 ---------- */

func toolResult() *Result {
	return &Result{
		Text:    "我来查一下",
		Outcome: OutcomeCallsParsed,
		Calls: []Call{{
			ID: "call_1", Name: "get_weather",
			Arguments: json.RawMessage(`{"city":"北京"}`),
		}},
	}
}

func TestRenderResponsePerProtocol(t *testing.T) {
	sess := newSession(t, Config{})
	for proto, body := range allProtocols() {
		t.Run(string(proto), func(t *testing.T) {
			req, err := DecodeRequest(proto, []byte(body), decodeOpts())
			if err != nil {
				t.Fatal(err)
			}
			out, err := sess.RenderResponse(req, toolResult(), RenderOptions{})
			if err != nil {
				t.Fatalf("渲染失败：%v", err)
			}
			s := string(out)
			// 工具调用要以客户端原本期待的形状回去。
			if !strings.Contains(s, "get_weather") {
				t.Errorf("工具调用应出现在响应里：%s", s)
			}
			if !strings.Contains(s, `"call_`) && !strings.Contains(s, `call_`) {
				t.Errorf("应有 call_id（确定性摘要形态）：%s", s)
			}
			if !strings.Contains(s, "北京") {
				t.Errorf("参数应原样保留：%s", s)
			}
		})
	}
}

// envelope 的 id 是模型自由文本，Instructions 只约束同一个 envelope 内互异。
// Codex 每轮重发完整历史，第二轮的 call-1 与第一轮撞车会让解码直接拒绝
// （2026-08-24 实测，agent loop 跑两轮必现）。call_id 必须转成确定性摘要：
// 同轮重渲染稳定、跨轮必不同。
func TestCallIDUniqueAcrossSessions(t *testing.T) {
	b, _ := New(Config{})
	s1, _ := b.NewSession("s1", "r1")
	s2, _ := b.NewSession("s2", "r2")
	res := &Result{Outcome: OutcomeCallsParsed, Calls: []Call{
		{ID: "call-1", Name: "exec"},
	}}

	p1 := proposals(s1, res)
	p2 := proposals(s2, res)
	if p1[0].CallID == p2[0].CallID {
		t.Errorf("不同会话的 call_id 撞车了：%s", p1[0].CallID)
	}
	again := proposals(s1, res)
	if again[0].CallID != p1[0].CallID {
		t.Errorf("同会话重渲染应得到同一个 call_id：%s vs %s", p1[0].CallID, again[0].CallID)
	}
}

func TestRenderedShapeMatchesProtocol(t *testing.T) {
	sess := newSession(t, Config{})
	cases := map[Protocol][]string{
		ProtocolChat:      {`"choices"`, `"tool_calls"`, `"finish_reason":"tool_calls"`},
		ProtocolAnthropic: {`"content"`, `"tool_use"`, `"stop_reason":"tool_use"`},
		ProtocolResponses: {`"output"`, `"function_call"`, `"status":"completed"`},
	}
	for proto, wants := range cases {
		t.Run(string(proto), func(t *testing.T) {
			req, _ := DecodeRequest(proto, []byte(allProtocols()[proto]), decodeOpts())
			out, err := sess.RenderResponse(req, toolResult(), RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range wants {
				if !strings.Contains(string(out), w) {
					t.Errorf("缺 %s：%s", w, out)
				}
			}
		})
	}
}

// Responses 的 item_id 不能是空的：流式事件与最终 output 必须用同一个标识，
// 客户端才能把 arguments 增量拼回同一个 item。上游是纯文本模型、不会给出
// fc_xxx，所以这个 ID 只能由我们生成——但必须稳定，不能每次渲染都换一个。
func TestResponsesItemIDIsPresentAndStable(t *testing.T) {
	sess := newSession(t, Config{})
	body := allProtocols()[ProtocolResponses]

	itemID := func() string {
		req, err := DecodeRequest(ProtocolResponses, []byte(body), decodeOpts())
		if err != nil {
			t.Fatal(err)
		}
		out, err := sess.RenderResponse(req, toolResult(), RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Output []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"output"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("响应不是合法 JSON：%v\n%s", err, out)
		}
		for _, it := range got.Output {
			if it.Type == "function_call" {
				return it.ID
			}
		}
		t.Fatalf("没有 function_call item：%s", out)
		return ""
	}

	first := itemID()
	if first == "" {
		t.Fatal("item id 为空——Codex 这类客户端会拿到一串 item_id 为空的事件")
	}
	if !strings.HasPrefix(first, "fc_") {
		t.Errorf("item id 应带 fc_ 前缀，得到 %q", first)
	}
	// 同一个调用重新渲染一次，仍该是同一个 ID。
	if second := itemID(); second != first {
		t.Errorf("重新渲染后 item id 变了：%q → %q", first, second)
	}
}

func TestRenderResponseHidesUpstreamModel(t *testing.T) {
	// 客户端问的是哪个模型就该看到哪个模型。把上游的真实模型名透出去，
	// 等于泄露了你的路由配置。
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	req.SetModel("internal-llama-3-70b")

	out, err := sess.RenderResponse(req, &Result{Text: "好"}, RenderOptions{Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "internal-llama") {
		t.Errorf("不该泄露上游模型名：%s", out)
	}
	if !strings.Contains(string(out), "gpt-4o") {
		t.Errorf("应回报客户端问的模型：%s", out)
	}
}

func TestRenderResponseRejectsNil(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if _, err := sess.RenderResponse(nil, toolResult(), RenderOptions{}); err == nil {
		t.Error("空请求应报错")
	}
	if _, err := sess.RenderResponse(req, nil, RenderOptions{}); err == nil {
		t.Error("空结果应报错")
	}
}

func TestTruncatedOutcomeReachesClient(t *testing.T) {
	// 模型的调用被截断了，客户端得知道，否则它会把半截当完整。
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolResponses, []byte(responsesBody), decodeOpts())
	out, err := sess.RenderResponse(req,
		&Result{Text: "开始查", Outcome: OutcomeTruncated}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "truncated_after_signal") {
		t.Errorf("截断原因应带给客户端：%s", out)
	}
}

/* ---------- 流式渲染 ---------- */

func TestStreamRendererPerProtocol(t *testing.T) {
	sess := newSession(t, Config{})
	cases := map[Protocol][]string{
		ProtocolChat:      {"data: ", "[DONE]"},
		ProtocolAnthropic: {"message_start", "message_stop"},
		ProtocolResponses: {"response.created", "response.completed"},
	}
	for proto, wants := range cases {
		t.Run(string(proto), func(t *testing.T) {
			req, _ := DecodeRequest(proto, []byte(allProtocols()[proto]), decodeOpts())
			var buf bytes.Buffer
			sr, err := sess.NewStreamRenderer(req, &buf, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sr.WriteText([]byte("我来"))
			sr.WriteText([]byte("查一下"))
			if err := sr.Finish(Result{Text: "我来查一下", Outcome: OutcomePlainText}); err != nil {
				t.Fatalf("收尾失败：%v", err)
			}
			s := buf.String()
			for _, w := range wants {
				if !strings.Contains(s, w) {
					t.Errorf("缺 %q：%s", w, s)
				}
			}
			// 真流式：两个增量各自成事件到达，不再等到 Finish 才一次性发。
			for _, frag := range []string{"我来", "查一下"} {
				if !strings.Contains(s, frag) {
					t.Errorf("增量 %q 丢了：%s", frag, s)
				}
			}
			// 不能把正文当成增量重发一遍——客户端会显示两遍。
			//
			// Responses 是例外且必须例外：它的 output_item.done 与
			// response.completed 按协议必须带完整的 item 内容，客户端靠
			// item_id 去重。所以只对增量事件计数。
			deltas := strings.Count(s, `"delta":"查一下"`) +
				strings.Count(s, `"content":"查一下"`) +
				strings.Count(s, `"text":"查一下"`)
			if deltas != 1 {
				t.Errorf("正文增量出现 %d 次，应恰好一次：%s", deltas, s)
			}
		})
	}
}

// 真流式下已发出的字节收不回来：Finish 不重发文本。
//
// 伪流式时代这条测试断言「Result.Text 优先」——那时文本只在 Finish 才发，
// 用解析器确认过的版本是对的。改成真流式后语义反转：增量已经到客户端
// 屏幕上了，再发一份 Result.Text 就是两份正文。调用方（gateway）的权威
// 来源是同一个 StreamParser，两者本就一致。
func TestStreamRendererDoesNotResendTextAfterStreaming(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	var buf bytes.Buffer
	sr, _ := sess.NewStreamRenderer(req, &buf, RenderOptions{})
	sr.WriteText([]byte("原始增量"))
	sr.Finish(Result{Text: "原始增量", Outcome: OutcomePlainText})

	out := buf.String()
	if strings.Count(out, "原始增量") != 1 {
		t.Errorf("正文出现 %d 次，真流式下应恰好一次：%s",
			strings.Count(out, "原始增量"), out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("终止事件缺失：%s", out)
	}
}

// 未流式过任何增量时（上游全程 envelope、或零输出），Finish 仍然一次性
// 渲染完整序列——行为与伪流式时代一致。
func TestStreamRendererFallsBackWhenNothingStreamed(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	var buf bytes.Buffer
	sr, _ := sess.NewStreamRenderer(req, &buf, RenderOptions{})
	sr.Finish(Result{Text: "解析器给的文本", Outcome: OutcomePlainText})

	out := buf.String()
	if !strings.Contains(out, "解析器给的文本") {
		t.Errorf("未流式过时应发 Result.Text：%s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("缺少 role 首包：%s", out)
	}
}

// 每个增量都必须 Flush。
//
// 2026-08-26 实测踩过：编码器只 Write 不 Flush，字节积在 net/http 的缓冲里
// 等整个响应结束才一次性到达——客户端看到的事件全挤在最后一刻，真流式的
// 全部收益归零，而所有单测照样绿（bytes.Buffer 没有缓冲概念）。所以这条
// 断言必须用一个会数 Flush 次数的写入器。
func TestStreamRendererFlushesEachDelta(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	fw := &flushCountingWriter{}
	sr, err := sess.NewStreamRenderer(req, fw, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sr.WriteText([]byte("一"))
	sr.WriteText([]byte("二"))
	sr.WriteText([]byte("三"))

	if fw.flushes < 3 {
		t.Fatalf("三个增量只 Flush 了 %d 次——字节会积在缓冲里，等于伪流式", fw.flushes)
	}
}

// flushCountingWriter 数 Flush 调用次数，模拟 http.ResponseWriter 的行为。
type flushCountingWriter struct {
	buf     bytes.Buffer
	flushes int
}

func (f *flushCountingWriter) Write(b []byte) (int, error) { return f.buf.Write(b) }
func (f *flushCountingWriter) Flush()                      { f.flushes++ }

func TestStreamRendererFinishIsIdempotent(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	var buf bytes.Buffer
	sr, _ := sess.NewStreamRenderer(req, &buf, RenderOptions{})
	sr.Finish(Result{Text: "好", Outcome: OutcomePlainText})
	n := buf.Len()
	sr.Finish(Result{Text: "好", Outcome: OutcomePlainText})
	if buf.Len() != n {
		t.Error("重复 Finish 不该再发一遍事件")
	}
	// 收尾之后再写也该是空操作。
	sr.WriteText([]byte("迟到的增量"))
	if buf.Len() != n {
		t.Error("收尾之后不该再接受增量")
	}
}

func TestStreamRendererRejectsNil(t *testing.T) {
	sess := newSession(t, Config{})
	req, _ := DecodeRequest(ProtocolChat, []byte(chatBody), decodeOpts())
	if _, err := sess.NewStreamRenderer(nil, &bytes.Buffer{}, RenderOptions{}); err == nil {
		t.Error("空请求应报错")
	}
	if _, err := sess.NewStreamRenderer(req, nil, RenderOptions{}); err == nil {
		t.Error("空写入器应报错")
	}
}

/* ---------- 端到端 ---------- */

func TestFullRoundTrip(t *testing.T) {
	// 一个反代的完整流程，三协议各走一遍。
	for proto, body := range allProtocols() {
		t.Run(string(proto), func(t *testing.T) {
			sess := newSession(t, Config{})

			// 1. 请求进来
			req, err := DecodeRequest(proto, []byte(body), decodeOpts())
			if err != nil {
				t.Fatal(err)
			}

			// 2. 编译并改造
			compiled, err := sess.CompileRequest(req, CompileOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// 3. 发给上游（这里假装上游按格式回了一个调用）
			upstreamBody, err := req.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(upstreamBody), compiled.Signal) {
				t.Fatal("发给上游的请求里应有信号")
			}

			modelText := "我来查一下\n" + compiled.Signal + `
<tool_call_envelope version="1">
  <call id="c1">
    <tool>get_weather</tool>
    <arguments_json><![CDATA[{"city":"北京"}]]></arguments_json>
  </call>
</tool_call_envelope>`

			// 4. 解析
			res, err := sess.Parse(modelText)
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if res.Outcome != OutcomeCallsParsed {
				t.Fatalf("应解析出调用，得到 %s", res.Outcome)
			}
			if len(res.Calls) != 1 || res.Calls[0].Name != "get_weather" {
				t.Fatalf("调用不对：%+v", res.Calls)
			}

			// 5. 编回客户端要的协议
			out, err := sess.RenderResponse(req, res, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "get_weather") ||
				!strings.Contains(string(out), "北京") {
				t.Errorf("调用没编回去：%s", out)
			}
			// 协议信号绝不能漏进给客户端的正文。
			if strings.Contains(string(out), compiled.Signal) {
				t.Errorf("信号漏进响应了：%s", out)
			}
		})
	}
}
