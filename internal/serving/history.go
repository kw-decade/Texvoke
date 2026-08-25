package serving

import "encoding/json"

// HasToolCallInBody 检查请求历史里是否已经有送出过的工具调用。
//
// L2 的运行时通知靠它反驳模型「调用接口不可用」的自我错觉：同一个会话里
// 明明成功过，就把这个事实摆出来。
//
// 为什么在原始 body 上再解一遍而不是用已解码的 Message：三协议的承载 item
// 形态不同但 type 值固定，一次浅解码比走三条解码路径便宜；而且这个判断在
// 解码之前就要用（会话事实要先登记，才轮到编译）。
//
// ponytail: 更干净的形态是 toolbridge.Request.HasToolHistory()，用已解码的
// Message.ToolCalls 直接判。挡在前面的是 local_shell_call——解码器当前把它
// 归进 SkippedItemTypes，换过去会丢掉 Codex 的这一类历史。要动就先补解码。
func HasToolCallInBody(body []byte) bool {
	var probe struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct{}      `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	// Responses：调用是独立的 item。
	for _, it := range probe.Input {
		switch it.Type {
		case "custom_tool_call", "function_call", "local_shell_call":
			return true
		}
	}
	for _, m := range probe.Messages {
		if m.Role != "assistant" {
			continue
		}
		// Chat：assistant 消息上的 tool_calls 数组。
		if len(m.ToolCalls) > 0 {
			return true
		}
		// Anthropic：assistant 消息 content 里的 tool_use 块。
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_use" {
					return true
				}
			}
		}
	}
	return false
}
