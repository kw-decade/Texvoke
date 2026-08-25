package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// Validate 检查响应各字段是否自洽，编码前必须通过。
//
// 最要紧的是 finish_reason 与 tool_calls 的双向一致：客户端 SDK 依据
// finish_reason == "tool_calls" 来决定要不要执行工具。带着调用却报 stop，
// 客户端会直接忽略这些调用；报 tool_calls 却没有调用，客户端会空等。
// 两种都是协议层的错，必须在这里挡住而不是发出去。
func (r ChatResponse) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("protocol: 响应缺少 id")
	}
	if r.Model == "" {
		return fmt.Errorf("protocol: 响应缺少 model")
	}
	if !r.FinishReason.Valid() {
		return fmt.Errorf("protocol: 响应的 finish_reason 非法：%q", r.FinishReason)
	}
	if len(r.ToolCalls) > 0 && r.FinishReason != FinishToolCalls {
		return fmt.Errorf("protocol: 响应带 %d 个工具调用，finish_reason 必须是 %q，实际为 %q",
			len(r.ToolCalls), FinishToolCalls, r.FinishReason)
	}
	if r.FinishReason == FinishToolCalls && len(r.ToolCalls) == 0 {
		return fmt.Errorf("protocol: finish_reason 为 %q 却没有任何工具调用", FinishToolCalls)
	}

	seen := make(map[string]bool, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		if err := tc.Validate(); err != nil {
			return fmt.Errorf("protocol: 第 %d 个工具调用非法：%w", i, err)
		}
		if seen[tc.CallID] {
			return fmt.Errorf("protocol: 工具调用 id %q 重复", tc.CallID)
		}
		seen[tc.CallID] = true
	}
	return nil
}

// EncodeChatResponse 把归一化响应渲染成 Chat Completions 的线上格式。
func EncodeChatResponse(r ChatResponse) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	msg := map[string]json.RawMessage{
		"role": json.RawMessage(`"assistant"`),
	}
	// content 必须出现，即使为空也要显式写 null——省略这个键会让部分客户端
	// SDK 在读取时拿到未定义值而非 null。
	if len(r.Content) > 0 {
		msg["content"] = r.Content
	} else {
		msg["content"] = json.RawMessage(`null`)
	}
	if r.Refusal != "" {
		b, err := json.Marshal(r.Refusal)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码 refusal 失败：%w", err)
		}
		msg["refusal"] = b
	}
	if len(r.ToolCalls) > 0 {
		// 只在返回客户端的方向上拒绝裸文本：Chat 客户端会拿 arguments 去
		// JSON.parse，收到一段脚本就炸了。发给上游的方向不受此限——那边是
		// 纯文本模型，历史里的调用必须原样传达，否则多轮直接断掉。
		if err := rejectFreeform("chat", r.ToolCalls); err != nil {
			return nil, err
		}
		b, err := encodeToolCalls(r.ToolCalls)
		if err != nil {
			return nil, err
		}
		msg["tool_calls"] = b
	}

	choice := map[string]any{
		"index":         0,
		"message":       msg,
		"finish_reason": string(r.FinishReason),
	}
	out := map[string]any{
		"id":      r.ID,
		"object":  "chat.completion",
		"created": r.Created,
		"model":   r.Model,
		"choices": []any{choice},
	}
	if r.Usage != nil {
		out["usage"] = r.Usage
	}
	return json.Marshal(out)
}

// encodeToolCalls 把 IR 调用提案渲染回 Chat Completions 的 tool_calls 形状。
func encodeToolCalls(calls []ir.ToolCallProposal) (json.RawMessage, error) {
	items := make([]any, 0, len(calls))
	for _, c := range calls {
		// arguments 要重新包成字符串：Chat Completions 里它是一个内含 JSON 的
		// 字符串，不是嵌套对象。直接放对象会让官方 SDK 解析失败。
		//
		// 裸文本参数在 IR 里已经是一个 JSON 字符串标量，正好就是这个字段要的
		// 形状，再包一层会把它变成被转义的字符串的字符串。
		argStr := c.Arguments
		if !c.ArgumentForm.Text() {
			var err error
			if argStr, err = json.Marshal(string(c.Arguments)); err != nil {
				return nil, fmt.Errorf("protocol: 编码调用 %s 的参数失败：%w", c.CallID, err)
			}
		}
		items = append(items, map[string]any{
			"id":   c.CallID,
			"type": "function",
			"function": map[string]json.RawMessage{
				"name":      mustJSONString(c.Tool.Name),
				"arguments": argStr,
			},
		})
	}
	return json.Marshal(items)
}

// mustJSONString 把一个已知合法的名字编码成 JSON 字符串。
//
// 工具名在解码时已通过 ir.ValidDeclaredName 校验（只含字母、数字、下划线和
// 短横线），不含任何需要转义且可能失败的字符，因此这里不会出错。
func mustJSONString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		// 走到这里说明校验被绕过了，属于程序错误而非输入错误。
		panic("protocol: 工具名无法编码为 JSON 字符串：" + s)
	}
	return b
}

// EncodeChatRequest 把归一化请求渲染回 Chat Completions 的线上格式，
// 供路由层发往上游。
//
// Extra 里的未知字段会原样写回，但不允许覆盖本结构显式建模的字段——
// 否则一个来自客户端的 extra["messages"] 就能绕过所有归一化和校验。
func EncodeChatRequest(r ChatRequest) ([]byte, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}
	if len(r.Messages) == 0 {
		return nil, fmt.Errorf("protocol: 请求缺少 messages")
	}

	out := make(map[string]json.RawMessage, len(r.Extra)+6)
	for k, v := range r.Extra {
		if chatKnownFields[k] {
			return nil, fmt.Errorf("protocol: Extra 不得包含已建模字段 %q", k)
		}
		out[k] = v
	}

	var err error
	if out["model"], err = json.Marshal(r.Model); err != nil {
		return nil, fmt.Errorf("protocol: 编码 model 失败：%w", err)
	}
	if out["messages"], err = encodeMessages(r.Messages); err != nil {
		return nil, err
	}
	if len(r.Tools) > 0 {
		if out["tools"], err = encodeTools(r.Tools); err != nil {
			return nil, err
		}
		if out["tool_choice"], err = encodeToolChoice(r.ToolChoice); err != nil {
			return nil, err
		}
	}
	if r.ParallelToolCalls != nil {
		if out["parallel_tool_calls"], err = json.Marshal(*r.ParallelToolCalls); err != nil {
			return nil, fmt.Errorf("protocol: 编码 parallel_tool_calls 失败：%w", err)
		}
	}
	if r.Stream {
		out["stream"] = json.RawMessage(`true`)
	}
	return json.Marshal(out)
}

func encodeMessages(msgs []Message) (json.RawMessage, error) {
	items := make([]map[string]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息非法：%w", i, err)
		}
		item := map[string]json.RawMessage{
			"role": mustJSONRole(m.Role),
		}
		content := m.Content

		// 工具结果落回 Chat Completions 的形状。这里的两处报错是刻意的：
		// 内部表示是三种协议的并集，Chat 装不下的东西必须显式失败，
		// 不能悄悄丢掉一半结果后照常发出去。
		if len(m.ToolResults) > 0 {
			if m.Role != RoleTool {
				return nil, fmt.Errorf("protocol: 第 %d 条消息：Chat Completions 无法表达 %q 消息携带的工具结果，结果只能由 tool 消息承载", i, m.Role)
			}
			if len(m.ToolResults) > 1 {
				return nil, fmt.Errorf("protocol: 第 %d 条消息：Chat Completions 的一条 tool 消息只能携带一个结果，实际有 %d 个", i, len(m.ToolResults))
			}
			r := m.ToolResults[0]
			// ponytail: is_error 在 Chat Completions 里没有对应字段，这里选择
			// 报错而不是静默丢弃。当前各协议只做各自的往返，不会触发；等实现
			// 跨协议路由时，改成由 observability 记录一次明确降级再放行。
			if r.IsError {
				return nil, fmt.Errorf("protocol: 第 %d 条消息：Chat Completions 无法表达 is_error 标记", i)
			}
			b, err := json.Marshal(r.CallID)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码第 %d 条消息的 tool_call_id 失败：%w", i, err)
			}
			item["tool_call_id"] = b
			content = r.Content
		}

		if len(content) > 0 {
			item["content"] = content
		} else {
			item["content"] = json.RawMessage(`null`)
		}
		if m.Name != "" {
			b, err := json.Marshal(m.Name)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码第 %d 条消息的 name 失败：%w", i, err)
			}
			item["name"] = b
		}
		if m.Refusal != "" {
			b, err := json.Marshal(m.Refusal)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码第 %d 条消息的 refusal 失败：%w", i, err)
			}
			item["refusal"] = b
		}
		if len(m.ToolCalls) > 0 {
			b, err := encodeToolCalls(m.ToolCalls)
			if err != nil {
				return nil, err
			}
			item["tool_calls"] = b
		}
		items = append(items, item)
	}
	return json.Marshal(items)
}

func mustJSONRole(r Role) json.RawMessage {
	b, err := json.Marshal(string(r))
	if err != nil {
		panic("protocol: 角色无法编码为 JSON 字符串：" + string(r))
	}
	return b
}

func encodeTools(tools []ir.ToolDeclaration) (json.RawMessage, error) {
	items := make([]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]json.RawMessage{
			"name":       mustJSONString(t.Name),
			"parameters": t.InputSchema,
		}
		if t.Description != "" {
			b, err := json.Marshal(t.Description)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码工具 %s 的描述失败：%w", t.Name, err)
			}
			fn["description"] = b
		}
		items = append(items, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return json.Marshal(items)
}

func encodeToolChoice(c ToolChoice) (json.RawMessage, error) {
	switch c.Mode {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return json.Marshal(string(c.Mode))
	case ToolChoiceNamed:
		if !ir.ValidDeclaredName(c.Name) {
			return nil, fmt.Errorf("protocol: tool_choice 指定的工具名 %q 非法", c.Name)
		}
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": c.Name},
		})
	default:
		return nil, fmt.Errorf("protocol: 无法编码未知的 tool_choice 模式 %q", c.Mode)
	}
}
