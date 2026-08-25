package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

func anthResponse() MessagesResponse {
	return MessagesResponse{
		ID:         "msg_01abc",
		Model:      "claude-sonnet-5",
		Content:    json.RawMessage(`[{"type":"text","text":"旧金山 18 度。"}]`),
		StopReason: StopEndTurn,
		Usage:      &AnthropicUsage{InputTokens: 120, OutputTokens: 48},
	}
}

func anthResponseWithCall() MessagesResponse {
	r := anthResponse()
	r.Content = json.RawMessage(`[{"type":"text","text":"我来查"}]`)
	r.StopReason = StopToolUse
	r.ToolCalls = []ir.ToolCallProposal{{
		SessionID: "sess-1", RequestID: "req-1", CallID: "toolu_01",
		Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
		Arguments: json.RawMessage(`{"city":"SF"}`),
		Source:    ir.SourceNative, CreatedAt: testOpts.Now,
	}}
	return r
}

// stop_reason 与工具调用必须双向一致。客户端 SDK 靠 stop_reason == "tool_use"
// 决定要不要执行工具：带着调用却报 end_turn，调用会被忽略；报 tool_use 却
// 没有调用，客户端会空等。
func TestAnthropicStopReasonConsistency(t *testing.T) {
	t.Run("有调用却报 end_turn", func(t *testing.T) {
		r := anthResponseWithCall()
		r.StopReason = StopEndTurn
		if _, err := EncodeMessagesResponse(r); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "stop_reason 必须是") {
			t.Errorf("错误信息 %q 未指出不一致", err.Error())
		}
	})

	t.Run("报 tool_use 却无调用", func(t *testing.T) {
		r := anthResponse()
		r.StopReason = StopToolUse
		if _, err := EncodeMessagesResponse(r); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "没有任何工具调用") {
			t.Errorf("错误信息 %q 未指出不一致", err.Error())
		}
	})

	t.Run("stop_sequence 与 stop_reason 不匹配", func(t *testing.T) {
		r := anthResponse()
		r.StopSequence = "END"
		if _, err := EncodeMessagesResponse(r); err == nil {
			t.Fatal("必须报错")
		} else if !strings.Contains(err.Error(), "stop_sequence") {
			t.Errorf("错误信息 %q 未指出不一致", err.Error())
		}
	})

	t.Run("一致时通过", func(t *testing.T) {
		if _, err := EncodeMessagesResponse(anthResponseWithCall()); err != nil {
			t.Errorf("一致的响应不应报错：%v", err)
		}
	})
}

func TestAnthropicResponseRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MessagesResponse)
		want   string
	}{
		{"缺 id", func(r *MessagesResponse) { r.ID = "" }, "缺少 id"},
		{"缺 model", func(r *MessagesResponse) { r.Model = "" }, "缺少 model"},
		{"stop_reason 未设置", func(r *MessagesResponse) { r.StopReason = "" }, "stop_reason 非法"},
		{"stop_reason 借用了 Chat 的枚举", func(r *MessagesResponse) { r.StopReason = "stop" }, "stop_reason 非法"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := anthResponse()
			tc.mutate(&r)
			if _, err := EncodeMessagesResponse(r); err == nil {
				t.Fatalf("期望报错包含 %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}

	t.Run("重复 call id", func(t *testing.T) {
		r := anthResponseWithCall()
		r.ToolCalls = append(r.ToolCalls, r.ToolCalls[0])
		if _, err := EncodeMessagesResponse(r); err == nil {
			t.Fatal("重复 id 必须报错")
		}
	})
}

// 响应里 tool_use 的 input 必须是 JSON 对象。写成字符串是把 Chat Completions
// 的形状带了过来，Anthropic SDK 会解析失败。
func TestAnthropicEncodedInputIsObject(t *testing.T) {
	data, err := EncodeMessagesResponse(anthResponseWithCall())
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("输出不是合法 JSON：%v", err)
	}

	var found bool
	for _, b := range out.Content {
		if b.Type != "tool_use" {
			continue
		}
		found = true
		var probe map[string]any
		if err := json.Unmarshal(b.Input, &probe); err != nil {
			t.Fatalf("input 不是 JSON 对象，实际为 %s——Anthropic SDK 会解析失败", b.Input)
		}
		if probe["city"] != "SF" {
			t.Errorf("input 内容为 %v", probe)
		}
	}
	if !found {
		t.Fatalf("输出里没有 tool_use block：%s", data)
	}
}

// 响应的 content 永远是数组，字符串形态只在请求里出现。
func TestAnthropicResponseContentIsArray(t *testing.T) {
	check := func(t *testing.T, r MessagesResponse) {
		t.Helper()
		data, err := EncodeMessagesResponse(r)
		if err != nil {
			t.Fatalf("编码失败：%v", err)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(out["content"], &arr); err != nil {
			t.Errorf("content 不是数组：%s", out["content"])
		}
	}

	t.Run("有内容", func(t *testing.T) { check(t, anthResponse()) })
	t.Run("有工具调用", func(t *testing.T) { check(t, anthResponseWithCall()) })
	t.Run("内容为空", func(t *testing.T) {
		r := anthResponse()
		r.Content = nil
		check(t, r)
	})
	t.Run("内容是字符串形态", func(t *testing.T) {
		r := anthResponse()
		r.Content = json.RawMessage(`"直接一段文本"`)
		check(t, r)
	})
}

func TestAnthropicResponseShape(t *testing.T) {
	data, err := EncodeMessagesResponse(anthResponseWithCall())
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	if out["type"] != "message" {
		t.Errorf("type 为 %v，期望 message", out["type"])
	}
	if out["role"] != "assistant" {
		t.Errorf("role 为 %v，期望 assistant", out["role"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason 为 %v", out["stop_reason"])
	}
	// stop_sequence 必须出现且为 null，Anthropic 的响应总是带这个键。
	v, ok := out["stop_sequence"]
	if !ok {
		t.Error("响应缺少 stop_sequence 键")
	}
	if v != nil {
		t.Errorf("stop_sequence 为 %v，期望 null", v)
	}
}

// 工具调用的顺序在编码时必须保持稳定：多个并行调用一旦乱序，
// 客户端按下标关联结果就会张冠李戴。
func TestAnthropicToolCallOrderStable(t *testing.T) {
	r := anthResponseWithCall()
	r.ToolCalls = append(r.ToolCalls, ir.ToolCallProposal{
		SessionID: "sess-1", RequestID: "req-1", CallID: "toolu_02",
		Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
		Arguments: json.RawMessage(`{"city":"Tokyo"}`),
		Source:    ir.SourceNative, CreatedAt: testOpts.Now,
	})

	for i := 0; i < 5; i++ {
		data, err := EncodeMessagesResponse(r)
		if err != nil {
			t.Fatal(err)
		}
		first := strings.Index(string(data), "toolu_01")
		second := strings.Index(string(data), "toolu_02")
		if first < 0 || second < 0 {
			t.Fatalf("调用 id 丢失：%s", data)
		}
		if first > second {
			t.Errorf("第 %d 次编码时调用顺序颠倒了", i)
		}
	}
}
