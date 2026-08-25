package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

func decodeAnth(t *testing.T, body string) *MessagesRequest {
	t.Helper()
	req, err := DecodeMessagesRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v\n请求体：%s", err, body)
	}
	return req
}

func decodeAnthErr(t *testing.T, body, want string) {
	t.Helper()
	_, err := DecodeMessagesRequest([]byte(body), testOpts)
	if err == nil {
		t.Fatalf("期望报错包含 %q，却解码成功了\n请求体：%s", want, body)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误信息 %q 未包含 %q", err.Error(), want)
	}
}

const anthTool = `{"name":"get_weather","description":"查询天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`

func anthBody(inner string) string {
	return `{"model":"claude-sonnet-5","max_tokens":1024,` + inner + `}`
}

// max_tokens 在 Anthropic 是必填。缺了它上游会直接拒绝，与其把注定失败的
// 请求发出去再看错误码，不如在解码时就给出可诊断的信息。
func TestAnthropicMaxTokensRequired(t *testing.T) {
	decodeAnthErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "缺少必填的 max_tokens")
	decodeAnthErr(t, `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, "必须为正")
	decodeAnthErr(t, `{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`, "必须为正")
	decodeAnthErr(t, `{"model":"m","max_tokens":"x","messages":[{"role":"user","content":"hi"}]}`, "必须是整数")
}

// system 是顶层字段而非一条消息，这是与 Chat Completions 最直观的差异。
// 内部统一成序列首位的 system 消息，让下游对三种协议看到同一种形状。
func TestAnthropicSystemBecomesFirstMessage(t *testing.T) {
	t.Run("字符串形态", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"system":"你是助手","messages":[{"role":"user","content":"hi"}]`))
		if len(req.Messages) != 2 {
			t.Fatalf("消息数为 %d，期望 2（system + user）", len(req.Messages))
		}
		if req.Messages[0].Role != RoleSystem {
			t.Errorf("首条消息角色为 %q，期望 system", req.Messages[0].Role)
		}
		if got := req.Messages[0].Text(); got != "你是助手" {
			t.Errorf("system 内容为 %q", got)
		}
		if !req.Messages[0].Role.Instructional() {
			t.Error("system 内容应被视为指令来源")
		}
	})

	t.Run("block 数组形态", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"system":[{"type":"text","text":"你是助手"}],"messages":[{"role":"user","content":"hi"}]`))
		if req.Messages[0].Role != RoleSystem {
			t.Fatalf("首条消息角色为 %q", req.Messages[0].Role)
		}
		if !strings.Contains(string(req.Messages[0].Content), "你是助手") {
			t.Errorf("system 的 block 形态未保留：%s", req.Messages[0].Content)
		}
	})

	t.Run("无 system 时不凭空造一条", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}]`))
		if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
			t.Errorf("不应插入 system 消息：%+v", req.Messages)
		}
	})
}

// Anthropic 官方只认 user 与 assistant，但 Claude Code 等真实客户端会把
// system 直接放进 messages（CC Switch 转协议时也会），与 Chat / Responses
// 两协议已接受的形状一致。system 被容忍；tool / developer / function 仍是
// 调用方把别的协议形状发过来的明确信号，要报出来。
func TestAnthropicRejectsForeignRoles(t *testing.T) {
	for _, role := range []string{"tool", "developer", "function"} {
		t.Run(role, func(t *testing.T) {
			decodeAnthErr(t, anthBody(`"messages":[{"role":"`+role+`","content":"x"}]`),
				"只支持 user、assistant 与 system")
		})
	}
}

// messages 里的 system 消息与顶层 system 同等待遇：并入统一序列、视为指令来源。
func TestAnthropicAcceptsSystemInMessages(t *testing.T) {
	req := decodeAnth(t, anthBody(`"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"hi"}]`))
	if len(req.Messages) != 2 {
		t.Fatalf("消息数为 %d，期望 2（system + user）", len(req.Messages))
	}
	if req.Messages[0].Role != RoleSystem {
		t.Fatalf("首条消息角色为 %q，期望 system", req.Messages[0].Role)
	}
	if got := req.Messages[0].Text(); got != "你是助手" {
		t.Errorf("system 内容为 %q", got)
	}
	if !req.Messages[0].Role.Instructional() {
		t.Error("system 内容应被视为指令来源")
	}

	// system 消息不得携带工具块——那是 user/assistant 的专属形状。
	decodeAnthErr(t, anthBody(`"messages":[{"role":"system","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`),
		"system 消息不得包含 tool block")
}

// tool_use 的 input 是真正的 JSON 对象，不像 Chat Completions 那样再包一层
// 字符串。这是两个协议最容易写混的地方。
func TestAnthropicToolUseInputIsObject(t *testing.T) {
	req := decodeAnth(t, anthBody(`"messages":[
		{"role":"user","content":"旧金山天气"},
		{"role":"assistant","content":[
			{"type":"text","text":"我来查"},
			{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"SF"}}
		]}
	],"tools":[`+anthTool+`]`))

	calls := req.Messages[1].ToolCalls
	if len(calls) != 1 {
		t.Fatalf("工具调用数为 %d，期望 1", len(calls))
	}
	if got := string(calls[0].Arguments); got != `{"city":"SF"}` {
		t.Errorf("参数为 %s，期望 {\"city\":\"SF\"}", got)
	}
	if calls[0].CallID != "toolu_01" {
		t.Errorf("call id 为 %q，期望原样保留 toolu_01", calls[0].CallID)
	}
	if calls[0].Tool.Namespace != ir.NamespaceClient {
		t.Errorf("命名空间为 %q", calls[0].Tool.Namespace)
	}
	// 非工具 block 要留在 Content 里，不能被工具抽取顺手丢掉。
	if !strings.Contains(string(req.Messages[1].Content), "我来查") {
		t.Errorf("文本 block 丢失：%s", req.Messages[1].Content)
	}

	t.Run("input 写成字符串要报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"f","input":"{\"a\":1}"}]}]`),
			"不是 JSON 对象")
	})
	t.Run("input 缺失要报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"f"}]}]`),
			"缺少 input")
	})
	t.Run("缺 id 要报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"assistant","content":[
			{"type":"tool_use","name":"f","input":{}}]}]`),
			"缺少 id")
	})
	t.Run("重复 id 要报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"f","input":{}},
			{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`),
			"重复")
	})
}

// 一条 user 消息携带多个 tool_result，正是把 Message.ToolCallID 重构成
// ToolResults 切片的直接原因。单字段装不下这种形状。
func TestAnthropicMultipleToolResultsInOneMessage(t *testing.T) {
	req := decodeAnth(t, anthBody(`"messages":[
		{"role":"user","content":"两地天气"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{"city":"SF"}},
			{"type":"tool_use","id":"toolu_b","name":"get_weather","input":{"city":"Tokyo"}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_a","content":"18 度"},
			{"type":"tool_result","tool_use_id":"toolu_b","content":"24 度","is_error":false},
			{"type":"text","text":"顺便说说穿什么"}
		]}
	],"tools":[`+anthTool+`]`))

	if len(req.Messages) != 3 {
		t.Fatalf("消息数为 %d，期望 3", len(req.Messages))
	}

	results := req.Messages[2].ToolResults
	if len(results) != 2 {
		t.Fatalf("工具结果数为 %d，期望 2——这正是重构要解决的形状", len(results))
	}
	if results[0].CallID != "toolu_a" || results[1].CallID != "toolu_b" {
		t.Errorf("结果与调用的关联错乱：%q %q", results[0].CallID, results[1].CallID)
	}
	// 同一条消息里的文本内容不能被结果抽取顺手丢掉。
	if !strings.Contains(string(req.Messages[2].Content), "顺便说说穿什么") {
		t.Errorf("同消息内的文本 block 丢失：%s", req.Messages[2].Content)
	}
	// 结果消息的角色是 user，其内容不得被当成指令来源。
	if req.Messages[2].Role.Instructional() {
		t.Error("携带工具结果的 user 消息不得被视为指令来源")
	}
}

// is_error 在 Chat Completions 里没有对应字段。丢掉它，「工具报错了」和
// 「工具返回了一段恰好像错误的文本」就再也分不开。
func TestAnthropicIsErrorPreserved(t *testing.T) {
	req := decodeAnth(t, anthBody(`"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"权限不足","is_error":true}]}
	]`))

	r := req.Messages[1].ToolResults[0]
	if !r.IsError {
		t.Error("is_error 标记丢失")
	}

	encoded, err := EncodeMessagesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"is_error":true`) {
		t.Errorf("is_error 在编码中丢失：%s", encoded)
	}
}

func TestAnthropicToolResultRequiresID(t *testing.T) {
	decodeAnthErr(t, anthBody(`"messages":[{"role":"user","content":[
		{"type":"tool_result","content":"x"}]}]`), "缺少 tool_use_id")
}

// 角色与 block 类型的组合必须自洽：user 不会发起调用，assistant 不会回报结果。
// 不自洽往往意味着中间有代理改写过请求。
func TestAnthropicRejectsMismatchedBlocks(t *testing.T) {
	decodeAnthErr(t, anthBody(`"messages":[{"role":"user","content":[
		{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`),
		"user 消息不得包含 tool_use")

	decodeAnthErr(t, anthBody(`"messages":[{"role":"assistant","content":[
		{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]`),
		"assistant 消息不得包含 tool_result")
}

func TestAnthropicToolChoice(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantMode ToolChoiceMode
		wantName string
	}{
		{"auto", `{"type":"auto"}`, ToolChoiceAuto, ""},
		{"any 映射到 required", `{"type":"any"}`, ToolChoiceRequired, ""},
		{"none", `{"type":"none"}`, ToolChoiceNone, ""},
		{"具名", `{"type":"tool","name":"get_weather"}`, ToolChoiceNamed, "get_weather"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`],"tool_choice":`+tc.raw))
			if req.ToolChoice.Mode != tc.wantMode {
				t.Errorf("模式为 %q，期望 %q", req.ToolChoice.Mode, tc.wantMode)
			}
			if req.ToolChoice.Name != tc.wantName {
				t.Errorf("工具名为 %q，期望 %q", req.ToolChoice.Name, tc.wantName)
			}
		})
	}

	t.Run("缺省且有工具时是 auto", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`]`))
		if req.ToolChoice.Mode != ToolChoiceAuto {
			t.Errorf("得到 %q，期望 auto", req.ToolChoice.Mode)
		}
	})

	t.Run("缺省且无工具时是 none", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}]`))
		if req.ToolChoice.Mode != ToolChoiceNone {
			t.Errorf("得到 %q，期望 none", req.ToolChoice.Mode)
		}
	})

	t.Run("未知类型报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`],"tool_choice":{"type":"always"}`),
			"未知的 tool_choice 类型")
	})

	t.Run("字符串形态报错", func(t *testing.T) {
		decodeAnthErr(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`],"tool_choice":"auto"`),
			"必须是对象")
	})
}

// disable_parallel_tool_use 是反向语义，内部统一成正向的 ParallelToolCalls。
// 翻错方向会让「禁止并行」变成「允许并行」，后果是写操作被并发执行。
func TestAnthropicParallelToolUseIsInverted(t *testing.T) {
	tests := []struct {
		raw  string
		want *bool
	}{
		{`{"type":"auto"}`, nil},
		{`{"type":"auto","disable_parallel_tool_use":true}`, boolPtr(false)},
		{`{"type":"auto","disable_parallel_tool_use":false}`, boolPtr(true)},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`],"tool_choice":`+tc.raw))
			switch {
			case tc.want == nil && req.ParallelToolCalls != nil:
				t.Errorf("未表态时应为 nil，实际为 %v", *req.ParallelToolCalls)
			case tc.want != nil && req.ParallelToolCalls == nil:
				t.Errorf("应解析出 %v，实际为 nil", *tc.want)
			case tc.want != nil && *req.ParallelToolCalls != *tc.want:
				t.Errorf("得到 %v，期望 %v——翻转方向反了", *req.ParallelToolCalls, *tc.want)
			}
		})
	}

	// 往返必须翻回去，否则一次 Bridge 转发就会把「禁止并行」变成允许。
	t.Run("往返翻回原样", func(t *testing.T) {
		req := decodeAnth(t, anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[`+anthTool+`],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`))
		encoded, err := EncodeMessagesRequest(*req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"disable_parallel_tool_use":true`) {
			t.Errorf("并行标记未翻回原样：%s", encoded)
		}
	})
}

func boolPtr(b bool) *bool { return &b }

// Anthropic 的服务端工具由 Anthropic 自己执行，不经过本 Runtime。
// 当成普通客户端工具接收下来，会让 Runtime 以为自己该去执行它。
func TestAnthropicRejectsServerTools(t *testing.T) {
	decodeAnthErr(t, anthBody(`"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]`),
		"暂不支持 Anthropic 服务端工具")
}

func TestAnthropicRejectsMalformed(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"不是 JSON", `nope`, "不是合法的 JSON 对象"},
		{"缺 model", `{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, "缺少 model"},
		{"缺 messages", anthBody(`"model":"m"`), "缺少 messages"},
		{"messages 为空", anthBody(`"messages":[]`), "不能为空数组"},
		{"block 缺 type", anthBody(`"messages":[{"role":"user","content":[{"text":"x"}]}]`), "缺少 type"},
		{"content 是数字", anthBody(`"messages":[{"role":"user","content":42}]`), "既不是字符串也不是 block 数组"},
		{"工具名重复", anthBody(`"messages":[{"role":"user","content":"hi"}],"tools":[` + anthTool + `,` + anthTool + `]`), "重复声明"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeAnthErr(t, tc.body, tc.want)
		})
	}
}

// thinking block 只被搬运，不被读取也不被改写。规格九章要求不得为了解析
// 而泄漏或伪造隐藏思维内容。
func TestAnthropicThinkingBlockPassesThrough(t *testing.T) {
	req := decodeAnth(t, anthBody(`"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"内部推理","signature":"sig123"},
		{"type":"text","text":"结论"}
	]}]`))

	content := string(req.Messages[0].Content)
	for _, want := range []string{"thinking", "内部推理", "sig123", "结论"} {
		if !strings.Contains(content, want) {
			t.Errorf("content 中丢失了 %q：%s", want, content)
		}
	}
	if len(req.Messages[0].ToolCalls) != 0 {
		t.Error("thinking block 不应被当成工具调用")
	}
}

// 往返一致性：解码再编码，语义不变。
func TestAnthropicRoundTrip(t *testing.T) {
	original := anthBody(`"system":"你是天气助手",
		"messages":[
			{"role":"user","content":"两地天气"},
			{"role":"assistant","content":[
				{"type":"text","text":"我来查"},
				{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{"city":"SF"}},
				{"type":"tool_use","id":"toolu_b","name":"get_weather","input":{"city":"Tokyo"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_a","content":"18 度"},
				{"type":"tool_result","tool_use_id":"toolu_b","content":"24 度","is_error":true}
			]},
			{"role":"assistant","content":"旧金山 18 度，东京查询失败。"}
		],
		"tools":[` + anthTool + `],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":false},
		"temperature":0.5`)

	req := decodeAnth(t, original)
	encoded, err := EncodeMessagesRequest(*req)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	again, err := DecodeMessagesRequest(encoded, testOpts)
	if err != nil {
		t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
	}

	stripDigests(req.Messages)
	stripDigests(again.Messages)
	if !reflect.DeepEqual(req, again) {
		t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", req, again)
	}

	// 逐项确认关键信息真的活着，而不是两次都同样地丢了。
	if again.MaxTokens != 1024 {
		t.Error("max_tokens 丢失")
	}
	if again.Messages[0].Role != RoleSystem {
		t.Error("system 未还原成首条消息")
	}
	if len(again.Messages[2].ToolCalls) != 2 {
		t.Error("并行的两个 tool_use 丢失")
	}
	if len(again.Messages[3].ToolResults) != 2 {
		t.Error("同一条消息里的两个 tool_result 丢失")
	}
	if !again.Messages[3].ToolResults[1].IsError {
		t.Error("is_error 在往返中丢失")
	}
	if _, ok := again.Extra["temperature"]; !ok {
		t.Error("未知字段在往返中丢失")
	}
	// system 必须回到顶层，而不是留在 messages 里发给上游。
	if strings.Contains(string(encoded), `"role":"system"`) {
		t.Errorf("system 未还原成顶层字段：%s", encoded)
	}
}

// Anthropic 只有一个 system 位置，多条无法表达。这里报错而不是拼接合并：
// 合并不可逆，客户端发出的两段独立指令被揉成一段后既无法还原也无人知晓。
func TestAnthropicRejectsMultipleSystemMessages(t *testing.T) {
	req := MessagesRequest{
		Model:     "m",
		MaxTokens: 100,
		Messages: []Message{
			{Role: RoleSystem, Content: json.RawMessage(`"甲"`)},
			{Role: RoleSystem, Content: json.RawMessage(`"乙"`)},
			{Role: RoleUser, Content: json.RawMessage(`"hi"`)},
		},
	}
	if _, err := EncodeMessagesRequest(req); err == nil {
		t.Fatal("必须报错")
	} else if !strings.Contains(err.Error(), "只支持一个 system") {
		t.Errorf("错误信息 %q 未说明原因", err.Error())
	}
}

func TestAnthropicEncodeRejectsIncomplete(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: json.RawMessage(`"hi"`)}}
	tests := []struct {
		name string
		req  MessagesRequest
		want string
	}{
		{"缺 model", MessagesRequest{MaxTokens: 1, Messages: msgs}, "缺少 model"},
		{"缺 max_tokens", MessagesRequest{Model: "m", Messages: msgs}, "max_tokens 必须为正"},
		{"缺 messages", MessagesRequest{Model: "m", MaxTokens: 1}, "缺少 messages"},
		{
			"只有 system",
			MessagesRequest{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleSystem, Content: json.RawMessage(`"x"`)}}},
			"没有任何消息",
		},
		{
			"Extra 覆盖已建模字段",
			MessagesRequest{Model: "m", MaxTokens: 1, Messages: msgs, Extra: map[string]json.RawMessage{"messages": json.RawMessage(`"x"`)}},
			"已建模字段",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeMessagesRequest(tc.req); err == nil {
				t.Fatalf("期望报错包含 %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}
