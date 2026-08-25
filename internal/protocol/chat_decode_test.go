package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
)

var testOpts = DecodeOptions{
	SessionID: "sess-1",
	RequestID: "req-1",
	Now:       time.Unix(1700000000, 0),
}

func decode(t *testing.T, body string) *ChatRequest {
	t.Helper()
	req, err := DecodeChatRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v\n请求体：%s", err, body)
	}
	return req
}

func decodeErr(t *testing.T, body, want string) {
	t.Helper()
	_, err := DecodeChatRequest([]byte(body), testOpts)
	if err == nil {
		t.Fatalf("期望报错包含 %q，却解码成功了\n请求体：%s", want, body)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误信息 %q 未包含 %q", err.Error(), want)
	}
}

// stripDigests 清空所有调用提案的原始候选摘要，供往返比较使用。
//
// 摘要记录的是「本次解码所见的参数原文」的指纹。一轮编解码后原文的排版被
// 归一化，摘要因此改变——这是它该有的行为，不是缺陷。往返测试要验证的是
// 语义是否保真，所以把这个刻意随原文变化的字段排除在外。
func stripDigests(msgs []Message) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].RawCandidateDigest = ""
		}
	}
}

const weatherTool = `{
  "type": "function",
  "function": {
    "name": "get_weather",
    "description": "查询天气",
    "parameters": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
  }
}`

// tool_choice 的缺省语义是有讲究的：没有工具时必须是 none 而不是 auto。
// 报 auto 会让下游误以为「模型本可以调用工具却没调」，从而把
// client_capability_missing 误判成 persona_refusal——查错方向整个跑偏。
func TestToolChoiceDefaults(t *testing.T) {
	t.Run("有工具时默认 auto", func(t *testing.T) {
		req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`+weatherTool+`]}`)
		if req.ToolChoice.Mode != ToolChoiceAuto {
			t.Errorf("得到 %q，期望 auto", req.ToolChoice.Mode)
		}
	})

	t.Run("无工具时默认 none", func(t *testing.T) {
		req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if req.ToolChoice.Mode != ToolChoiceNone {
			t.Errorf("得到 %q，期望 none（tools=0 不应报成 auto）", req.ToolChoice.Mode)
		}
		if len(req.Tools) != 0 {
			t.Errorf("不应解析出工具，实际 %d 个", len(req.Tools))
		}
	})

	t.Run("tools 为 null 等同于无工具", func(t *testing.T) {
		req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":null}`)
		if req.ToolChoice.Mode != ToolChoiceNone {
			t.Errorf("得到 %q，期望 none", req.ToolChoice.Mode)
		}
	})
}

func TestToolChoiceExplicit(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantMode ToolChoiceMode
		wantName string
	}{
		{"auto", `"auto"`, ToolChoiceAuto, ""},
		{"none", `"none"`, ToolChoiceNone, ""},
		{"required", `"required"`, ToolChoiceRequired, ""},
		{"any 映射到 required", `"any"`, ToolChoiceRequired, ""},
		{"具名", `{"type":"function","function":{"name":"get_weather"}}`, ToolChoiceNamed, "get_weather"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`+weatherTool+`],"tool_choice":`+tc.raw+`}`)
			if req.ToolChoice.Mode != tc.wantMode {
				t.Errorf("模式为 %q，期望 %q", req.ToolChoice.Mode, tc.wantMode)
			}
			if req.ToolChoice.Name != tc.wantName {
				t.Errorf("工具名为 %q，期望 %q", req.ToolChoice.Name, tc.wantName)
			}
		})
	}
}

// 未知取值必须报错。规格八章：不能静默转成 auto——静默降级会把一个
// 「客户端要求必须调用」的请求变成「随便你调不调」，而客户端无从知道。
func TestToolChoiceUnknownIsRejected(t *testing.T) {
	bad := []string{`"AUTO"`, `"always"`, `"require"`, `123`, `["auto"]`,
		`{"type":"tool","function":{"name":"x"}}`,
		`{"type":"function","function":{"name":"bad name"}}`}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			decodeErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`+weatherTool+`],"tool_choice":`+raw+`}`, "")
		})
	}
}

func TestRequiresCall(t *testing.T) {
	tests := map[ToolChoiceMode]bool{
		ToolChoiceAuto:     false,
		ToolChoiceNone:     false,
		ToolChoiceRequired: true,
		ToolChoiceNamed:    true,
	}
	for mode, want := range tests {
		if got := (ToolChoice{Mode: mode}).RequiresCall(); got != want {
			t.Errorf("%q.RequiresCall() = %v，期望 %v", mode, got, want)
		}
	}
}

// arguments 在 Chat Completions 里是「一个内含 JSON 的字符串」，不是对象。
// 这是最容易写错的一处，也是解析失败必须显式报错的地方。
func TestDecodeArguments(t *testing.T) {
	msg := func(args string) string {
		return `{"model":"m","messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function",
			 "function":{"name":"get_weather","arguments":` + args + `}}]}
		]}`
	}

	t.Run("合法的字符串参数", func(t *testing.T) {
		req := decode(t, msg(`"{\"city\":\"SF\"}"`))
		calls := req.Messages[1].ToolCalls
		if len(calls) != 1 {
			t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
		}
		if got := string(calls[0].Arguments); got != `{"city":"SF"}` {
			t.Errorf("参数为 %s，期望 {\"city\":\"SF\"}", got)
		}
	})

	t.Run("空对象字符串合法", func(t *testing.T) {
		req := decode(t, msg(`"{}"`))
		if got := string(req.Messages[1].ToolCalls[0].Arguments); got != `{}` {
			t.Errorf("参数为 %s，期望 {}", got)
		}
	})

	rejects := []struct {
		name string
		args string
		want string
	}{
		// 直接写成对象是最常见的误实现：官方 SDK 会因此解析失败。
		{"写成对象", `{"city":"SF"}`, "必须是字符串"},
		{"空字符串", `""`, "空字符串"},
		{"缺失", `null`, "缺失"},
		{"字符串里是残缺 JSON", `"{\"city\":"`, "不是 JSON 对象"},
		{"字符串里是数组", `"[1,2]"`, "不是 JSON 对象"},
		{"字符串里是纯文本", `"我不能执行命令"`, "不是 JSON 对象"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			decodeErr(t, msg(tc.args), tc.want)
		})
	}
}

// 解析失败绝不能产出一个「能用」的提案。这条测试盯的是规格三章那句
// 「不允许把解析失败的参数塞进 _raw 后继续执行」。
func TestParseFailureYieldsNoProposal(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function",
		 "function":{"name":"get_weather","arguments":"随便一段不是 JSON 的话"}}]}
	]}`
	req, err := DecodeChatRequest([]byte(body), testOpts)
	if err == nil {
		t.Fatal("参数无法解析时必须报错")
	}
	if req != nil {
		t.Error("报错时不得同时返回可用的请求对象")
	}
}

func TestDecodeRejectsAmbiguity(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"重复的 call id",
			`{"model":"m","messages":[{"role":"user","content":"hi"},
			 {"role":"assistant","tool_calls":[
			   {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},
			   {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}]}`,
			"重复",
		},
		{
			"重复的工具名",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` + weatherTool + `,` + weatherTool + `]}`,
			"重复声明",
		},
		{
			"n 大于 1",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`,
			"多候选语义不明确",
		},
		{
			"非 function 类型的工具",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","function":{"name":"x","parameters":{}}}]}`,
			"只支持 function",
		},
		{
			"非 function 类型的调用",
			`{"model":"m","messages":[{"role":"user","content":"hi"},
			 {"role":"assistant","tool_calls":[{"id":"c1","type":"custom","function":{"name":"x","arguments":"{}"}}]}]}`,
			"只支持 function",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeErr(t, tc.body, tc.want)
		})
	}
}

// 角色与字段的组合不自洽，往往意味着路由或代理改写过请求——
// 而 router_mutation 正是需要被诊断出来的一类根因。
func TestMessageRoleCombinations(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"user 消息带 tool_calls",
			`{"model":"m","messages":[{"role":"user","content":"hi","tool_calls":[
			  {"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`,
			"只有 assistant",
		},
		{
			"tool 消息缺 tool_call_id",
			`{"model":"m","messages":[{"role":"tool","content":"result"}]}`,
			"缺少 tool_call_id",
		},
		{
			"user 消息带 tool_call_id",
			`{"model":"m","messages":[{"role":"user","content":"hi","tool_call_id":"c1"}]}`,
			"只有 tool 消息",
		},
		{
			"未知角色",
			`{"model":"m","messages":[{"role":"root","content":"hi"}]}`,
			"未知的消息角色",
		},
		{
			// 空消息本身现在会被丢掉（真实客户端会发这种噪声），但整个请求
			// 因此一条不剩时仍要报错——不能发一个没有对话的请求给上游。
			"user 消息缺 content",
			`{"model":"m","messages":[{"role":"user"}]}`,
			"没有一条带内容的消息",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeErr(t, tc.body, tc.want)
		})
	}

	// assistant 可以只给工具调用而没有正文，这是最常见的形态，不能误拒。
	t.Run("assistant 只有工具调用是合法的", func(t *testing.T) {
		decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"},
		 {"role":"assistant","tool_calls":[{"id":"c1","type":"function",
		  "function":{"name":"get_weather","arguments":"{}"}}]}]}`)
	})
}

func TestDecodeRejectsMalformedTop(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"不是 JSON", `not json`, "不是合法的 JSON 对象"},
		{"是数组", `[]`, "不是合法的 JSON 对象"},
		{"缺 model", `{"messages":[{"role":"user","content":"hi"}]}`, "缺少 model"},
		{"缺 messages", `{"model":"m"}`, "缺少 messages"},
		{"messages 为空数组", `{"model":"m","messages":[]}`, "不能为空数组"},
		{"model 不是字符串", `{"model":1,"messages":[{"role":"user","content":"hi"}]}`, "必须是字符串"},
		{"stream 不是布尔", `{"model":"m","stream":"yes","messages":[{"role":"user","content":"hi"}]}`, "必须是布尔值"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeErr(t, tc.body, tc.want)
		})
	}
}

// 没有上限的请求体是一条内存耗尽路径，规格三章把它列为必须纠正的问题。
func TestRequestSizeLimit(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 1000) + `"}]}`
	opts := testOpts
	opts.MaxBytes = 100
	if _, err := DecodeChatRequest([]byte(body), opts); err == nil {
		t.Fatal("超出上限的请求体必须被拒绝")
	} else if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误信息 %q 未说明是大小超限", err.Error())
	}
}

func TestDecodeRequiresIdentifiers(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for _, opts := range []DecodeOptions{
		{RequestID: "req-1"},
		{SessionID: "sess-1"},
		{},
	} {
		if _, err := DecodeChatRequest([]byte(body), opts); err == nil {
			t.Errorf("缺少标识时必须报错：%+v", opts)
		}
	}
}

// 未知的顶层字段要原样保留，路由层才能把它们透传给上游。
// 静默吞掉会让 temperature、seed 之类的参数在经过 Bridge 后神秘失效。
func TestExtraFieldsArePreserved(t *testing.T) {
	req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
	 "temperature":0.7,"seed":42,"custom_vendor_field":{"a":1}}`)

	for _, k := range []string{"temperature", "seed", "custom_vendor_field"} {
		if _, ok := req.Extra[k]; !ok {
			t.Errorf("未知字段 %q 未被保留", k)
		}
	}
	// 已建模的字段不应重复出现在 Extra 里。
	for _, k := range []string{"model", "messages", "tools", "tool_choice"} {
		if _, ok := req.Extra[k]; ok {
			t.Errorf("已建模字段 %q 不应出现在 Extra 中", k)
		}
	}
}

func TestDecodedProposalFields(t *testing.T) {
	req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"},
	 {"role":"assistant","tool_calls":[{"id":"call_xyz","type":"function",
	  "function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}]}`)

	p := req.Messages[1].ToolCalls[0]

	// 原始 call ID 必须原样保留：重新生成会让客户端无法把结果关联回自己
	// 发出的那次调用。规格三章把这列为必须纠正的问题。
	if p.CallID != "call_xyz" {
		t.Errorf("call_id 为 %q，期望原样保留 call_xyz", p.CallID)
	}
	if p.SessionID != "sess-1" || p.RequestID != "req-1" {
		t.Errorf("会话标识未写入：%+v", p)
	}
	if p.Source != ir.SourceNative {
		t.Errorf("来源为 %q，期望 native", p.Source)
	}
	// 客户端声明的工具落在专用命名空间，下游据此知道它不该走 executor。
	if p.Tool.Namespace != ir.NamespaceClient {
		t.Errorf("命名空间为 %q，期望 %q", p.Tool.Namespace, ir.NamespaceClient)
	}
	if p.Tool.Name != "get_weather" {
		t.Errorf("工具名为 %q", p.Tool.Name)
	}
	if p.RawCandidateDigest == "" {
		t.Error("应记录原始候选的摘要以备审计")
	}
	if strings.Contains(p.RawCandidateDigest, "SF") {
		t.Error("摘要中不应出现原文")
	}
	if !p.CreatedAt.Equal(testOpts.Now) {
		t.Errorf("时间戳为 %v，期望注入的 %v", p.CreatedAt, testOpts.Now)
	}
}

// 解码结果必须与书写排版无关。
//
// 这条测试来自一次真实失败：fixture 里的 JSON 带缩进，而 json.RawMessage
// 保留原始字节，于是「解码→编码→解码」两次的结果字节不同，往返不再幂等。
// 更要紧的是参数的幂等键按字节计算——同一次重试换个排版就会算出不同的键，
// 账本会把它当成一次新调用放行，副作用因此重复发生。
func TestDecodingIsFormatIndependent(t *testing.T) {
	pretty := `{
	  "model": "m",
	  "messages": [
	    {
	      "role": "assistant",
	      "tool_calls": [
	        {
	          "id": "call_1",
	          "type": "function",
	          "function": {
	            "name": "get_weather",
	            "arguments": "{  \"city\" :  \"SF\"  }"
	          }
	        }
	      ]
	    }
	  ],
	  "tools": [
	    {
	      "type": "function",
	      "function": {
	        "name": "get_weather",
	        "parameters": {
	          "type": "object",
	          "properties": {}
	        }
	      }
	    }
	  ]
	}`
	compact := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}`

	a := decode(t, pretty)
	b := decode(t, compact)

	callA := a.Messages[0].ToolCalls[0]
	callB := b.Messages[0].ToolCalls[0]

	// 参数在解码后必须归到同一形态，否则后续的 Schema 校验、日志和
	// 幂等键都会随客户端的排版风格漂移。
	if string(callA.Arguments) != string(callB.Arguments) {
		t.Errorf("参数未归一化：%s vs %s", callA.Arguments, callB.Arguments)
	}
	if callA.IdempotencyKey() != callB.IdempotencyKey() {
		t.Errorf("排版差异改变了幂等键：%s vs %s——重试会被误判为新调用",
			callA.IdempotencyKey(), callB.IdempotencyKey())
	}
	if string(a.Tools[0].InputSchema) != string(b.Tools[0].InputSchema) {
		t.Errorf("schema 未归一化：%s vs %s", a.Tools[0].InputSchema, b.Tools[0].InputSchema)
	}

	// 反过来，原始候选摘要必须不同——它记录的是「模型当时原样输出了什么」，
	// 用于审计。若它也被归一化，就证明不了原文了。
	if callA.RawCandidateDigest == callB.RawCandidateDigest {
		t.Error("原始候选摘要不应受归一化影响，它记录的是未经处理的原文")
	}

	// 除摘要外，两次解码的结果应当完全一致。
	callA.RawCandidateDigest = ""
	callB.RawCandidateDigest = ""
	a.Messages[0].ToolCalls[0] = callA
	b.Messages[0].ToolCalls[0] = callB
	if !reflect.DeepEqual(a, b) {
		t.Errorf("排版不同导致解码结果不同\n缩进版：%+v\n紧凑版：%+v", a, b)
	}
}

// 编码结果再解码必须回到同一个对象，且第二次编码与第一次逐字节相同。
func TestRoundTripIsIdempotent(t *testing.T) {
	req := decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`+weatherTool+`]}`)

	first, err := EncodeChatRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeChatRequest(first, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeChatRequest(*again)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("二次编码结果不同\n第一次：%s\n第二次：%s", first, second)
	}
}

func TestMessageText(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"字符串", `"你好"`, "你好"},
		{"空", ``, ""},
		{"null", `null`, ""},
		{"content parts", `[{"type":"text","text":"甲"},{"type":"text","text":"乙"}]`, "甲乙"},
		// Responses 的文本块叫 output_text。Message 是三协议的并集，
		// 只认一个名字会让 Responses 的回复在这里静默变成空串。
		{"Responses 的 output_text", `[{"type":"output_text","text":"完成"}]`, "完成"},
		{"两种文本块混排", `[{"type":"text","text":"甲"},{"type":"output_text","text":"乙"}]`, "甲乙"},
		{"混合多模态只取文本", `[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"x"}}]`, "看图"},
		{"纯图片", `[{"type":"image_url","image_url":{"url":"x"}}]`, ""},
		{"数字", `123`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Role: RoleUser, Content: json.RawMessage(tc.content)}
			if got := m.Text(); got != tc.want {
				t.Errorf("Text() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 只有 system 与 developer 的内容是指令，其余一律是数据。
// 这是「工具结果里的『请执行下一条命令』必须当普通文本」那条红线的判定点。
func TestRoleInstructional(t *testing.T) {
	tests := map[Role]bool{
		RoleSystem:    true,
		RoleDeveloper: true,
		RoleUser:      false,
		RoleAssistant: false,
		RoleTool:      false,
	}
	for role, want := range tests {
		if got := role.Instructional(); got != want {
			t.Errorf("%q.Instructional() = %v，期望 %v", role, got, want)
		}
	}
}
