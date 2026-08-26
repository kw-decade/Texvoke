package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 真流式的文本增量事件形状。golden 对照的是真实客户端解析器认的形态：
// Chat 客户端按 choices[0].delta.content 取值；Anthropic 按
// delta.type=text_delta；Responses 按 item_id 关联增量。

func TestEncodeChatTextDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeChatTextDelta(NewSSEEncoder(&buf), "chatcmpl_x", "m", 42, "你"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`"id":"chatcmpl_x"`,
		`"object":"chat.completion.chunk"`,
		`"delta":{"content":"你"}`,
		`"finish_reason":null`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q：\n%s", want, out)
		}
	}
}

func TestEncodeAnthropicTextDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeAnthropicTextDelta(NewSSEEncoder(&buf), 0, "你好"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"event: content_block_delta",
		`"index":0`,
		// 键序由 encoding/json 决定（字母序），只断言字段齐全。
		`"type":"text_delta"`, `"text":"你好"`, `"type":"content_block_delta"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q：\n%s", want, out)
		}
	}
}

func TestEncodeResponsesTextDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResponsesTextDelta(NewSSEEncoder(&buf), "msg_1", "片段"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"event: response.output_text.delta",
		`"item_id":"msg_1"`,
		`"output_index":0`,
		`"content_index":0`,
		`"delta":"片段"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q：\n%s", want, out)
		}
	}

	// item_id 必须与 output_item.done 里的一致——用同一个 id 走一遍完整
	// 渲染，确认两边形状对得上。
	var full bytes.Buffer
	enc := NewSSEEncoder(&full)
	if err := EncodeResponsesTextDelta(enc, "msg_9", "x"); err != nil {
		t.Fatal(err)
	}
	if err := EncodeResponsesStream(enc, ResponsesResponse{
		ID: "resp_1", Model: "m", CreatedAt: 1,
		Status:        ResponseCompleted,
		MessageItemID: "msg_9",
		Content:       json.RawMessage(`[{"type":"output_text","text":"x"}]`),
	}); err != nil {
		t.Fatal(err)
	}
	full2 := full.String()
	if !strings.Contains(full2, `"item_id":"msg_9"`) ||
		strings.Count(full2, "msg_9") < 3 { // delta + item.added + item.done
		t.Errorf("item_id 没有贯穿增量与收尾事件：\n%s", full2)
	}
}
