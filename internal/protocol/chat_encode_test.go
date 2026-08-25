package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

func validResponse() ChatResponse {
	return ChatResponse{
		ID:           "chatcmpl-1",
		Model:        "m",
		Created:      1700000000,
		Content:      json.RawMessage(`"你好"`),
		FinishReason: FinishStop,
	}
}

func responseWithCall() ChatResponse {
	r := validResponse()
	r.Content = nil
	r.FinishReason = FinishToolCalls
	r.ToolCalls = []ir.ToolCallProposal{{
		SessionID: "sess-1",
		RequestID: "req-1",
		CallID:    "call_1",
		Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
		Arguments: json.RawMessage(`{"city":"SF"}`),
		Source:    ir.SourceNative,
		CreatedAt: testOpts.Now,
	}}
	return r
}

// finish_reason 与 tool_calls 必须双向一致。客户端 SDK 靠 finish_reason
// 决定要不要执行工具：带着调用却报 stop，调用会被直接忽略；报 tool_calls
// 却没有调用，客户端会空等一个永远不来的东西。
func TestResponseFinishReasonConsistency(t *testing.T) {
	t.Run("有调用却报 stop", func(t *testing.T) {
		r := responseWithCall()
		r.FinishReason = FinishStop
		if _, err := EncodeChatResponse(r); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "finish_reason 必须是") {
			t.Errorf("错误信息 %q 未指出不一致", err.Error())
		}
	})

	t.Run("报 tool_calls 却无调用", func(t *testing.T) {
		r := validResponse()
		r.FinishReason = FinishToolCalls
		if _, err := EncodeChatResponse(r); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "没有任何工具调用") {
			t.Errorf("错误信息 %q 未指出不一致", err.Error())
		}
	})

	t.Run("一致时通过", func(t *testing.T) {
		if _, err := EncodeChatResponse(responseWithCall()); err != nil {
			t.Errorf("一致的响应不应报错：%v", err)
		}
	})
}

func TestResponseValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ChatResponse)
		want   string
	}{
		{"缺 id", func(r *ChatResponse) { r.ID = "" }, "缺少 id"},
		{"缺 model", func(r *ChatResponse) { r.Model = "" }, "缺少 model"},
		{"finish_reason 未设置", func(r *ChatResponse) { r.FinishReason = "" }, "finish_reason 非法"},
		{"finish_reason 拼错", func(r *ChatResponse) { r.FinishReason = "done" }, "finish_reason 非法"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := validResponse()
			tc.mutate(&r)
			if _, err := EncodeChatResponse(r); err == nil {
				t.Fatalf("期望报错包含 %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}

	t.Run("重复的 call id", func(t *testing.T) {
		r := responseWithCall()
		r.ToolCalls = append(r.ToolCalls, r.ToolCalls[0])
		if _, err := EncodeChatResponse(r); err == nil {
			t.Fatal("重复 id 必须报错")
		}
	})
}

// arguments 在线上格式里必须是「一个内含 JSON 的字符串」而不是嵌套对象。
// 写成对象是最常见的误实现，官方 SDK 会因此解析失败。
func TestEncodedArgumentsAreAString(t *testing.T) {
	data, err := EncodeChatResponse(responseWithCall())
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("输出不是合法 JSON：%v", err)
	}

	raw := out.Choices[0].Message.ToolCalls[0].Function.Arguments
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("arguments 不是 JSON 字符串，实际为 %s——官方 SDK 会解析失败", raw)
	}
	if s != `{"city":"SF"}` {
		t.Errorf("解开字符串后为 %s，期望 {\"city\":\"SF\"}", s)
	}
}

// content 即使为空也要显式写 null。省略这个键会让部分 SDK 读到未定义值。
func TestEncodedContentIsExplicitNull(t *testing.T) {
	data, err := EncodeChatResponse(responseWithCall())
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	v, ok := msg["content"]
	if !ok {
		t.Fatal("message 中缺少 content 键")
	}
	if v != nil {
		t.Errorf("content 为 %v，期望 null", v)
	}
}

func TestEncodedResponseShape(t *testing.T) {
	data, err := EncodeChatResponse(responseWithCall())
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	if out["object"] != "chat.completion" {
		t.Errorf("object 为 %v", out["object"])
	}
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices 应为长度 1 的数组，实际 %v", out["choices"])
	}
	c := choices[0].(map[string]any)
	if c["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason 为 %v", c["finish_reason"])
	}
	if c["message"].(map[string]any)["role"] != "assistant" {
		t.Error("消息角色应为 assistant")
	}
}

// Extra 里若能覆盖已建模字段，一个来自客户端的 extra["messages"] 就能绕过
// 全部归一化和校验。这是注入面，必须挡住。
func TestExtraCannotOverrideModeledFields(t *testing.T) {
	for _, k := range []string{"model", "messages", "tools", "tool_choice", "stream", "n"} {
		t.Run(k, func(t *testing.T) {
			req := ChatRequest{
				Model:    "m",
				Messages: []Message{{Role: RoleUser, Content: json.RawMessage(`"hi"`)}},
				Extra:    map[string]json.RawMessage{k: json.RawMessage(`"injected"`)},
			}
			if _, err := EncodeChatRequest(req); err == nil {
				t.Fatalf("Extra 覆盖 %q 必须被拒绝", k)
			} else if !strings.Contains(err.Error(), "已建模字段") {
				t.Errorf("错误信息 %q 未说明原因", err.Error())
			}
		})
	}
}

// 往返一致性：解码再编码，语义必须不变。这是 Bridge 的基本要求——
// 经过它转一圈的请求，上游看到的应当和客户端发出的是同一个意思。
func TestRoundTripPreservesSemantics(t *testing.T) {
	original := `{
	  "model": "gpt-4o",
	  "messages": [
	    {"role":"system","content":"你是助手"},
	    {"role":"user","content":"旧金山天气如何"},
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"18 摄氏度"},
	    {"role":"assistant","content":"旧金山 18 度。"}
	  ],
	  "tools": [` + weatherTool + `],
	  "tool_choice": "auto",
	  "parallel_tool_calls": false,
	  "temperature": 0.3
	}`

	req := decode(t, original)
	encoded, err := EncodeChatRequest(*req)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	again, err := DecodeChatRequest(encoded, testOpts)
	if err != nil {
		t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
	}

	stripDigests(req.Messages)
	stripDigests(again.Messages)
	if !reflect.DeepEqual(req, again) {
		t.Errorf("往返后语义改变了\n首次：%+v\n再次：%+v", req, again)
	}

	// 逐项确认关键字段真的活着，而不是两次都同样地丢了。
	if len(again.Messages) != 5 {
		t.Errorf("消息数为 %d，期望 5", len(again.Messages))
	}
	if again.Messages[2].ToolCalls[0].CallID != "call_1" {
		t.Error("assistant 的 call_id 丢失")
	}
	if len(again.Messages[3].ToolResults) != 1 || again.Messages[3].ToolResults[0].CallID != "call_1" {
		t.Error("tool 消息与调用的关联丢失——结果将无法关联回调用")
	}
	if len(again.Tools) != 1 || again.Tools[0].Name != "get_weather" {
		t.Error("工具声明丢失")
	}
	if again.ParallelToolCalls == nil || *again.ParallelToolCalls {
		t.Error("parallel_tool_calls=false 丢失")
	}
	if _, ok := again.Extra["temperature"]; !ok {
		t.Error("未知字段 temperature 在往返中丢失")
	}
}

// 工具的原始 schema 是权威的，往返中一个字节都不能变——类型、必填、枚举
// 若被改写，校验就会基于一份和客户端不同的契约进行。
func TestRoundTripPreservesSchemaExactly(t *testing.T) {
	schema := `{"type":"object","properties":{"n":{"type":"integer","enum":[1,2,3]},"s":{"type":"string"}},"required":["n"],"additionalProperties":false}`
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],
	 "tools":[{"type":"function","function":{"name":"f","parameters":` + schema + `}}]}`

	req := decode(t, body)
	if got := string(req.Tools[0].InputSchema); got != schema {
		t.Errorf("schema 在解码中被改写了\n实际：%s\n期望：%s", got, schema)
	}

	encoded, err := EncodeChatRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	again := func() *ChatRequest {
		r, err := DecodeChatRequest(encoded, testOpts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()
	if got := string(again.Tools[0].InputSchema); got != schema {
		t.Errorf("schema 在往返中被改写了\n实际：%s\n期望：%s", got, schema)
	}
}

func TestEncodeToolChoiceForms(t *testing.T) {
	tests := []struct {
		choice ToolChoice
		want   string
	}{
		{ToolChoice{Mode: ToolChoiceAuto}, `"auto"`},
		{ToolChoice{Mode: ToolChoiceNone}, `"none"`},
		{ToolChoice{Mode: ToolChoiceRequired}, `"required"`},
		{ToolChoice{Mode: ToolChoiceNamed, Name: "f"}, `{"function":{"name":"f"},"type":"function"}`},
	}
	for _, tc := range tests {
		t.Run(string(tc.choice.Mode), func(t *testing.T) {
			got, err := encodeToolChoice(tc.choice)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("得到 %s，期望 %s", got, tc.want)
			}
		})
	}

	t.Run("未知模式报错", func(t *testing.T) {
		if _, err := encodeToolChoice(ToolChoice{Mode: "bogus"}); err == nil {
			t.Error("未知模式必须报错")
		}
	})
	t.Run("具名模式的非法工具名报错", func(t *testing.T) {
		if _, err := encodeToolChoice(ToolChoice{Mode: ToolChoiceNamed, Name: "bad name"}); err == nil {
			t.Error("非法工具名必须报错")
		}
	})
}

func TestEncodeRequestRejectsIncomplete(t *testing.T) {
	if _, err := EncodeChatRequest(ChatRequest{Messages: []Message{{Role: RoleUser, Content: json.RawMessage(`"x"`)}}}); err == nil {
		t.Error("缺 model 必须报错")
	}
	if _, err := EncodeChatRequest(ChatRequest{Model: "m"}); err == nil {
		t.Error("缺 messages 必须报错")
	}
}

// 内部的 Message 是三种协议的并集，Chat Completions 装不下的东西必须显式
// 失败。静默丢掉一半结果后照常发出去，模型会看到一个残缺的对话历史，
// 而客户端和运维都不会收到任何提示。
func TestEncodeRejectsWhatChatCannotExpress(t *testing.T) {
	base := func(m Message) ChatRequest {
		return ChatRequest{Model: "m", Messages: []Message{m}}
	}

	t.Run("一条 tool 消息携带多个结果", func(t *testing.T) {
		req := base(Message{
			Role: RoleTool,
			ToolResults: []ToolResultBlock{
				{CallID: "c1", Content: json.RawMessage(`"a"`)},
				{CallID: "c2", Content: json.RawMessage(`"b"`)},
			},
		})
		if _, err := EncodeChatRequest(req); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "只能携带一个结果") {
			t.Errorf("错误信息 %q 未说明原因", err.Error())
		}
	})

	t.Run("user 消息携带工具结果", func(t *testing.T) {
		// 这是 Anthropic 的合法形状，Chat Completions 里无法表达。
		req := base(Message{
			Role:        RoleUser,
			Content:     json.RawMessage(`"还有别的吗"`),
			ToolResults: []ToolResultBlock{{CallID: "c1", Content: json.RawMessage(`"a"`)}},
		})
		if _, err := EncodeChatRequest(req); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "只能由 tool 消息承载") {
			t.Errorf("错误信息 %q 未说明原因", err.Error())
		}
	})

	t.Run("结果带 is_error 标记", func(t *testing.T) {
		req := base(Message{
			Role:        RoleTool,
			ToolResults: []ToolResultBlock{{CallID: "c1", Content: json.RawMessage(`"boom"`), IsError: true}},
		})
		if _, err := EncodeChatRequest(req); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "is_error") {
			t.Errorf("错误信息 %q 未说明原因", err.Error())
		}
	})
}
