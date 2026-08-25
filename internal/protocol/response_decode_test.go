package protocol

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// 上游返回的结构化错误必须与「响应格式非法」区分开。混为一谈会让
// 「供应商禁止工具调用」被误报成「解析器有 bug」，查错方向整个跑偏。
func TestUpstreamErrorIsDistinguishable(t *testing.T) {
	body := `{"error":{"message":"Tool use is not supported for this model",
	 "type":"invalid_request_error","code":"unsupported_feature","param":"tools"}}`

	t.Run("Chat", func(t *testing.T) {
		_, err := DecodeChatResponse([]byte(body), testOpts)
		if err == nil {
			t.Fatal("必须报错")
		}
		var ue *UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("错误类型为 %T，期望 *UpstreamError——拒绝分类依赖这个类型", err)
		}
		if ue.Type != "invalid_request_error" || ue.Code != "unsupported_feature" {
			t.Errorf("机器可读的分类丢失：%+v", ue)
		}
		if !strings.Contains(ue.Message, "Tool use is not supported") {
			t.Errorf("消息为 %q", ue.Message)
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		_, err := DecodeMessagesResponse([]byte(body), testOpts)
		var ue *UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("错误类型为 %T，期望 *UpstreamError", err)
		}
	})

	t.Run("格式错误不是上游错误", func(t *testing.T) {
		_, err := DecodeChatResponse([]byte(`{"choices":"nope"}`), testOpts)
		if err == nil {
			t.Fatal("必须报错")
		}
		var ue *UpstreamError
		if errors.As(err, &ue) {
			t.Error("格式非法被误报成了上游错误")
		}
	})

	t.Run("形状不认识的 error 也当成上游错误", func(t *testing.T) {
		_, err := DecodeChatResponse([]byte(`{"error":"just a string"}`), testOpts)
		var ue *UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("错误类型为 %T——认不出形状也好过当成正常回复继续处理", err)
		}
	})
}

func TestUpstreamErrorMessage(t *testing.T) {
	tests := []struct {
		err  UpstreamError
		want string
	}{
		{UpstreamError{Message: "boom"}, "上游错误: boom"},
		{UpstreamError{Message: "boom", Type: "t"}, "上游错误 [t]: boom"},
		{UpstreamError{Message: "boom", Type: "t", Code: "c"}, "上游错误 [t/c]: boom"},
	}
	for _, tc := range tests {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q，期望 %q", got, tc.want)
		}
	}
}

func TestDecodeChatResponse(t *testing.T) {
	t.Run("纯文本回复", func(t *testing.T) {
		body := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
		 "model":"gpt-4o","choices":[{"index":0,
		 "message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],
		 "usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

		resp, err := DecodeChatResponse([]byte(body), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		if resp.ID != "chatcmpl-1" || resp.Model != "gpt-4o" || resp.Created != 1700000000 {
			t.Errorf("顶层字段有误：%+v", resp)
		}
		if resp.FinishReason != FinishStop {
			t.Errorf("finish_reason 为 %q", resp.FinishReason)
		}
		if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
			t.Errorf("usage 丢失：%+v", resp.Usage)
		}
	})

	t.Run("工具调用回复", func(t *testing.T) {
		body := `{"id":"chatcmpl-2","model":"gpt-4o","created":1,"choices":[{"index":0,
		 "message":{"role":"assistant","content":null,"tool_calls":[
		   {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
		 "finish_reason":"tool_calls"}]}`

		resp, err := DecodeChatResponse([]byte(body), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("调用数为 %d，期望 1", len(resp.ToolCalls))
		}
		if resp.ToolCalls[0].CallID != "call_1" {
			t.Errorf("call id 未原样保留：%q", resp.ToolCalls[0].CallID)
		}
		if got := string(resp.ToolCalls[0].Arguments); got != `{"city":"SF"}` {
			t.Errorf("参数为 %s", got)
		}
		// content 为 null 时不应留下四个字节的 "null" 冒充内容。
		if resp.Content != nil {
			t.Errorf("content 应为 nil，实际为 %s", resp.Content)
		}
	})

	t.Run("拒绝回复", func(t *testing.T) {
		body := `{"id":"c","model":"m","created":1,"choices":[{"index":0,
		 "message":{"role":"assistant","content":null,"refusal":"我不能协助этого"},
		 "finish_reason":"stop"}]}`
		resp, err := DecodeChatResponse([]byte(body), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		// refusal 要保留：拒绝分类需要区分「模型明确拒绝」与「模型只是没调用工具」。
		if resp.Refusal == "" {
			t.Error("refusal 字段丢失，拒绝分类将失去判据")
		}
	})
}

func TestDecodeChatResponseRejects(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"不是 JSON", `nope`, "不是合法的 JSON 对象"},
		{"缺 choices", `{"id":"c","model":"m"}`, "缺少 choices"},
		{"choices 为空", `{"id":"c","model":"m","choices":[]}`, "为空数组"},
		{
			"多个 choice",
			`{"id":"c","model":"m","choices":[
			  {"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},
			  {"index":1,"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}]}`,
			"多候选语义不明确",
		},
		{
			"角色不是 assistant",
			`{"id":"c","model":"m","choices":[{"index":0,
			  "message":{"role":"user","content":"a"},"finish_reason":"stop"}]}`,
			"期望 assistant",
		},
		{
			"带调用却报 stop",
			`{"id":"c","model":"m","choices":[{"index":0,
			  "message":{"role":"assistant","tool_calls":[
			    {"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			  "finish_reason":"stop"}]}`,
			"finish_reason 必须是",
		},
		{
			"报 tool_calls 却无调用",
			`{"id":"c","model":"m","choices":[{"index":0,
			  "message":{"role":"assistant","content":"a"},"finish_reason":"tool_calls"}]}`,
			"没有任何工具调用",
		},
		{
			"参数不是合法 JSON",
			`{"id":"c","model":"m","choices":[{"index":0,
			  "message":{"role":"assistant","tool_calls":[
			    {"id":"c1","type":"function","function":{"name":"f","arguments":"我不能执行"}}]},
			  "finish_reason":"tool_calls"}]}`,
			"不是 JSON 对象",
		},
		{
			"finish_reason 未知",
			`{"id":"c","model":"m","choices":[{"index":0,
			  "message":{"role":"assistant","content":"a"},"finish_reason":"done"}]}`,
			"finish_reason 非法",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeChatResponse([]byte(tc.body), testOpts)
			if err == nil {
				t.Fatalf("期望报错包含 %q，却解码成功了", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDecodeMessagesResponse(t *testing.T) {
	t.Run("工具调用回复", func(t *testing.T) {
		body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5",
		 "content":[{"type":"text","text":"我来查"},
		   {"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"SF"}}],
		 "stop_reason":"tool_use","stop_sequence":null,
		 "usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20}}`

		resp, err := DecodeMessagesResponse([]byte(body), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		if resp.ID != "msg_01" || resp.StopReason != StopToolUse {
			t.Errorf("顶层字段有误：%+v", resp)
		}
		if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].CallID != "toolu_01" {
			t.Fatalf("工具调用解析有误：%+v", resp.ToolCalls)
		}
		// input 是对象，解析后应逐字保留
		if got := string(resp.ToolCalls[0].Arguments); got != `{"city":"SF"}` {
			t.Errorf("参数为 %s", got)
		}
		// 文本 block 要留在 Content 里
		if !strings.Contains(string(resp.Content), "我来查") {
			t.Errorf("文本 block 丢失：%s", resp.Content)
		}
		if resp.Usage == nil || resp.Usage.CacheReadInputTokens != 20 {
			t.Errorf("usage 丢失或不全：%+v", resp.Usage)
		}
	})

	t.Run("停止序列", func(t *testing.T) {
		body := `{"id":"msg_02","type":"message","role":"assistant","model":"m",
		 "content":[{"type":"text","text":"a"}],
		 "stop_reason":"stop_sequence","stop_sequence":"END"}`
		resp, err := DecodeMessagesResponse([]byte(body), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		if resp.StopSequence != "END" {
			t.Errorf("stop_sequence 为 %q", resp.StopSequence)
		}
	})

	t.Run("空内容", func(t *testing.T) {
		body := `{"id":"msg_03","type":"message","role":"assistant","model":"m",
		 "content":[],"stop_reason":"end_turn"}`
		if _, err := DecodeMessagesResponse([]byte(body), testOpts); err != nil {
			t.Fatalf("空 content 数组应当合法：%v", err)
		}
	})
}

func TestDecodeMessagesResponseRejects(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"缺 content", `{"id":"m","model":"m","stop_reason":"end_turn"}`, "缺少 content"},
		{
			"stop_reason 借用了 Chat 的枚举",
			`{"id":"m","model":"m","content":[],"stop_reason":"stop"}`,
			"stop_reason 非法",
		},
		{
			"带调用却报 end_turn",
			`{"id":"m","model":"m","content":[
			  {"type":"tool_use","id":"t1","name":"f","input":{}}],"stop_reason":"end_turn"}`,
			"stop_reason 必须是",
		},
		{
			"报 tool_use 却无调用",
			`{"id":"m","model":"m","content":[{"type":"text","text":"a"}],"stop_reason":"tool_use"}`,
			"没有任何工具调用",
		},
		{
			"响应里出现 tool_result",
			`{"id":"m","model":"m","content":[
			  {"type":"tool_result","tool_use_id":"t1","content":"x"}],"stop_reason":"end_turn"}`,
			"不应出现 tool_result",
		},
		{
			"角色不是 assistant",
			`{"id":"m","model":"m","role":"user","content":[],"stop_reason":"end_turn"}`,
			"期望 assistant",
		},
		{
			"input 写成字符串",
			`{"id":"m","model":"m","content":[
			  {"type":"tool_use","id":"t1","name":"f","input":"{}"}],"stop_reason":"tool_use"}`,
			"不是 JSON 对象",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeMessagesResponse([]byte(tc.body), testOpts)
			if err == nil {
				t.Fatalf("期望报错包含 %q，却解码成功了", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// 响应也要能往返：Bridge 读上游响应、做完策略检查后要再渲染给客户端，
// 这一进一出不能改变语义。
func TestResponseRoundTrip(t *testing.T) {
	t.Run("Chat", func(t *testing.T) {
		original := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
		 "model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":null,
		 "tool_calls":[
		   {"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}},
		   {"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},
		 "finish_reason":"tool_calls"}],
		 "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

		resp, err := DecodeChatResponse([]byte(original), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		encoded, err := EncodeChatResponse(*resp)
		if err != nil {
			t.Fatalf("编码失败：%v", err)
		}
		again, err := DecodeChatResponse(encoded, testOpts)
		if err != nil {
			t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
		}
		if !reflect.DeepEqual(resp, again) {
			t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", resp, again)
		}
		if len(again.ToolCalls) != 2 {
			t.Error("并行调用丢失")
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		original := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5",
		 "content":[{"type":"text","text":"我来查"},
		   {"type":"tool_use","id":"toolu_a","name":"get_weather","input":{"city":"SF"}},
		   {"type":"tool_use","id":"toolu_b","name":"get_weather","input":{"city":"Tokyo"}}],
		 "stop_reason":"tool_use","stop_sequence":null,
		 "usage":{"input_tokens":1,"output_tokens":2}}`

		resp, err := DecodeMessagesResponse([]byte(original), testOpts)
		if err != nil {
			t.Fatalf("解码失败：%v", err)
		}
		encoded, err := EncodeMessagesResponse(*resp)
		if err != nil {
			t.Fatalf("编码失败：%v", err)
		}
		again, err := DecodeMessagesResponse(encoded, testOpts)
		if err != nil {
			t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
		}

		// 摘要随所见原文变化，往返比较要排除（同请求侧的理由）。
		for i := range resp.ToolCalls {
			resp.ToolCalls[i].RawCandidateDigest = ""
		}
		for i := range again.ToolCalls {
			again.ToolCalls[i].RawCandidateDigest = ""
		}
		if !reflect.DeepEqual(resp, again) {
			t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", resp, again)
		}
		if len(again.ToolCalls) != 2 {
			t.Error("并行调用丢失")
		}
	})
}

// 响应体同样要有大小上限：没有上限，一个超大响应就能把进程内存吃光。
func TestResponseSizeLimit(t *testing.T) {
	body := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant",
	 "content":"` + strings.Repeat("x", 1000) + `"},"finish_reason":"stop"}]}`
	opts := testOpts
	opts.MaxBytes = 100

	if _, err := DecodeChatResponse([]byte(body), opts); err == nil {
		t.Error("超限的 Chat 响应必须被拒绝")
	}
	if _, err := DecodeMessagesResponse([]byte(body), opts); err == nil {
		t.Error("超限的 Anthropic 响应必须被拒绝")
	}
}

func TestDecodeResponseRequiresIdentifiers(t *testing.T) {
	body := `{"id":"c","model":"m","choices":[{"index":0,
	 "message":{"role":"assistant","content":"a"},"finish_reason":"stop"}]}`
	if _, err := DecodeChatResponse([]byte(body), DecodeOptions{}); err == nil {
		t.Error("缺少标识时必须报错")
	}
	anth := `{"id":"m","model":"m","content":[],"stop_reason":"end_turn"}`
	if _, err := DecodeMessagesResponse([]byte(anth), DecodeOptions{}); err == nil {
		t.Error("缺少标识时必须报错")
	}
}

// 上游响应里的工具调用同样落在客户端命名空间，且来源标记为 native。
// 下游据此知道：这是原生透传路径，但它仍然要过同一道安全门。
func TestUpstreamCallsAreNotPrivileged(t *testing.T) {
	body := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant",
	 "tool_calls":[{"id":"c1","type":"function","function":{"name":"rm_rf","arguments":"{}"}}]},
	 "finish_reason":"tool_calls"}]}`

	resp, err := DecodeChatResponse([]byte(body), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	call := resp.ToolCalls[0]
	if call.Tool.Namespace != "client" {
		t.Errorf("命名空间为 %q，期望 client——上游原生返回的调用不享有特权", call.Tool.Namespace)
	}
	if call.SessionID != testOpts.SessionID || call.RequestID != testOpts.RequestID {
		t.Error("会话标识未写入，调用将无法归属到具体请求")
	}
	// 解码只负责还原形状，不做任何放行判断——状态机的起点仍然是 proposed。
	if err := call.Validate(); err != nil {
		t.Errorf("提案自身应当合法：%v", err)
	}
}

func TestDecodeResponseHandlesMissingOptionalFields(t *testing.T) {
	// usage 缺失是常见的（流式尾包、部分代理会剥离），不应导致解码失败。
	body := `{"id":"c","model":"m","choices":[{"index":0,
	 "message":{"role":"assistant","content":"a"},"finish_reason":"stop"}]}`
	resp, err := DecodeChatResponse([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if resp.Usage != nil {
		t.Error("上游未报告 usage 时应为 nil，而不是一个全零的结构")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["usage"]; ok {
		t.Fatal("测试样本本身不该带 usage")
	}
}
