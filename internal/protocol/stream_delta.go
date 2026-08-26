package protocol

import (
	"encoding/json"
	"fmt"
)

// 三协议的「流式文本增量」编码。
//
// 真流式模式下，网关把上游的安全文本增量实时转成客户端协议的事件。
// 这组函数只负责**文本部分**的增量事件；工具调用与收尾事件仍由
// EncodeChatStream / EncodeAnthropicStream / EncodeResponsesStream 在
// Finish 时一次性渲染——调用（如果有）出现在输出末尾，且它们的参数必须
// 完整到达才能发，天然属于收尾阶段。
//
// 事件形状对照真实客户端的解析行为：
//   - Chat：chat.completion.chunk，delta 里只有 content
//   - Anthropic：content_block_delta + text_delta（index 由渲染器维护）
//   - Responses：response.output_text.delta（item_id 由渲染器维护）

// EncodeChatTextDelta 把一段文本增量包成 chat.completion.chunk 事件。
func EncodeChatTextDelta(enc *SSEEncoder, id, model string, created int64, text string) error {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": text},
			"finish_reason": nil,
		}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("protocol: 编码 chat 文本增量失败：%w", err)
	}
	return enc.Write(Event{Data: b})
}

// EncodeAnthropicTextDelta 把一段文本增量包成 content_block_delta 事件。
// index 是文本 block 在事件序列里的位置——真流式下文本永远是第 0 个 block
// （工具调用在它之后），由 StreamRenderer 固定传 0。
func EncodeAnthropicTextDelta(enc *SSEEncoder, index int, text string) error {
	b, err := json.Marshal(map[string]any{
		"type":  EventContentBlockDelta,
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	if err != nil {
		return fmt.Errorf("protocol: 编码 anthropic 文本增量失败：%w", err)
	}
	return enc.Write(Event{Type: EventContentBlockDelta, Data: b})
}

// EncodeResponsesTextDelta 把一段文本增量包成 response.output_text.delta 事件。
// itemID 是 message item 的标识，客户端靠它把增量拼回同一个 item；
// 必须与后续 output_item.done 里的 id 一致（不变量 8 的流式形态）。
func EncodeResponsesTextDelta(enc *SSEEncoder, itemID string, text string) error {
	b, err := json.Marshal(map[string]any{
		"type":          EventOutputTextDelta,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	})
	if err != nil {
		return fmt.Errorf("protocol: 编码 responses 文本增量失败：%w", err)
	}
	return enc.Write(Event{Type: EventOutputTextDelta, Data: b})
}
