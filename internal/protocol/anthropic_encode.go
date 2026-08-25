package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// EncodeMessagesRequest 把归一化请求渲染回 Anthropic Messages 的线上格式。
func EncodeMessagesRequest(r MessagesRequest) ([]byte, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}
	if r.MaxTokens <= 0 {
		return nil, fmt.Errorf("protocol: Anthropic 请求的 max_tokens 必须为正，实际为 %d", r.MaxTokens)
	}
	if len(r.Messages) == 0 {
		return nil, fmt.Errorf("protocol: 请求缺少 messages")
	}

	out := make(map[string]json.RawMessage, len(r.Extra)+7)
	for k, v := range r.Extra {
		if anthropicKnownFields[k] {
			return nil, fmt.Errorf("protocol: Extra 不得包含已建模字段 %q", k)
		}
		out[k] = v
	}

	var err error
	if out["model"], err = json.Marshal(r.Model); err != nil {
		return nil, fmt.Errorf("protocol: 编码 model 失败：%w", err)
	}
	if out["max_tokens"], err = json.Marshal(r.MaxTokens); err != nil {
		return nil, fmt.Errorf("protocol: 编码 max_tokens 失败：%w", err)
	}

	system, msgs, err := splitSystemMessage(r.Messages)
	if err != nil {
		return nil, err
	}
	if len(system) > 0 {
		out["system"] = system
	}
	if out["messages"], err = encodeAnthropicMessages(msgs); err != nil {
		return nil, err
	}

	if len(r.Tools) > 0 {
		if out["tools"], err = encodeAnthropicTools(r.Tools); err != nil {
			return nil, err
		}
		if out["tool_choice"], err = encodeAnthropicToolChoice(r.ToolChoice, r.ParallelToolCalls); err != nil {
			return nil, err
		}
	}
	if r.Stream {
		out["stream"] = json.RawMessage(`true`)
	}
	return json.Marshal(out)
}

// splitSystemMessage 把 system 消息从序列里摘出来，还原成 Anthropic 的顶层字段。
//
// Anthropic 只有一个 system 位置，所以多条 system 消息无法表达。这里报错而不是
// 拼接合并：合并是不可逆的，客户端发出的两段独立指令被揉成一段后，
// 既无法还原也无人知晓。
func splitSystemMessage(msgs []Message) (json.RawMessage, []Message, error) {
	var system json.RawMessage
	rest := make([]Message, 0, len(msgs))

	for i, m := range msgs {
		if m.Role != RoleSystem && m.Role != RoleDeveloper {
			rest = append(rest, m)
			continue
		}
		if system != nil {
			return nil, nil, fmt.Errorf("protocol: Anthropic 只支持一个 system，实际有多条（第 %d 条是重复的）", i)
		}
		if !m.hasContent() {
			return nil, nil, fmt.Errorf("protocol: 第 %d 条 system 消息没有内容", i)
		}
		system = m.Content
	}
	if len(rest) == 0 {
		return nil, nil, fmt.Errorf("protocol: 除 system 外没有任何消息")
	}
	return system, rest, nil
}

func encodeAnthropicMessages(msgs []Message) (json.RawMessage, error) {
	items := make([]map[string]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return nil, fmt.Errorf("protocol: 第 %d 条消息的角色为 %q，Anthropic 只支持 user 与 assistant", i, m.Role)
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息非法：%w", i, err)
		}
		// Chat Completions 允许 assistant 携带结果吗？不允许；这里挡的是
		// 另一头：内部表示允许 user 带结果，但 assistant 不行。
		if m.Role == RoleAssistant && len(m.ToolResults) > 0 {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：assistant 不得携带工具结果", i)
		}

		content, err := encodeAnthropicContent(m.Content, m.ToolCalls, m.ToolResults)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：%w", i, err)
		}
		if len(content) == 0 {
			return nil, fmt.Errorf("protocol: 第 %d 条消息没有可编码的内容", i)
		}
		items = append(items, map[string]json.RawMessage{
			"role":    mustJSONRole(m.Role),
			"content": content,
		})
	}
	return json.Marshal(items)
}

// encodeAnthropicContent 把内容与工具 block 重新拼成 content 数组。
//
// 顺序不是随意的：Anthropic 要求 tool_result 出现在 user 消息的最前面，
// 所以结果排在其余内容之前；工具调用则跟在文本之后，与模型「先说话再调用」
// 的实际输出顺序一致。
func encodeAnthropicContent(content json.RawMessage, calls []ir.ToolCallProposal, results []ToolResultBlock) (json.RawMessage, error) {
	hasTools := len(calls) > 0 || len(results) > 0

	// 纯文本且没有工具 block 时保持字符串形态，不平白把请求变复杂。
	if !hasTools {
		if len(content) == 0 || string(content) == "null" {
			return nil, nil
		}
		return content, nil
	}

	blocks := make([]json.RawMessage, 0, len(calls)+len(results)+2)

	for _, r := range results {
		block := map[string]json.RawMessage{
			"type": json.RawMessage(`"tool_result"`),
		}
		b, err := json.Marshal(r.CallID)
		if err != nil {
			return nil, fmt.Errorf("编码 tool_use_id 失败：%w", err)
		}
		block["tool_use_id"] = b
		if len(r.Content) > 0 {
			block["content"] = r.Content
		}
		if r.IsError {
			block["is_error"] = json.RawMessage(`true`)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			return nil, fmt.Errorf("编码 tool_result block 失败：%w", err)
		}
		blocks = append(blocks, raw)
	}

	if len(content) > 0 && string(content) != "null" {
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			// 字符串形态的内容与工具 block 共存时，必须升格成 text block。
			raw, err := json.Marshal(map[string]string{"type": "text", "text": s})
			if err != nil {
				return nil, fmt.Errorf("编码 text block 失败：%w", err)
			}
			blocks = append(blocks, raw)
		} else {
			var existing []json.RawMessage
			if err := json.Unmarshal(content, &existing); err != nil {
				return nil, fmt.Errorf("content 既不是字符串也不是 block 数组：%w", err)
			}
			blocks = append(blocks, existing...)
		}
	}

	for _, c := range calls {
		block := map[string]json.RawMessage{
			"type":  json.RawMessage(`"tool_use"`),
			"name":  mustJSONString(c.Tool.Name),
			"input": c.Arguments,
		}
		b, err := json.Marshal(c.CallID)
		if err != nil {
			return nil, fmt.Errorf("编码 tool_use id 失败：%w", err)
		}
		block["id"] = b
		raw, err := json.Marshal(block)
		if err != nil {
			return nil, fmt.Errorf("编码 tool_use block 失败：%w", err)
		}
		blocks = append(blocks, raw)
	}

	return json.Marshal(blocks)
}

func encodeAnthropicTools(tools []ir.ToolDeclaration) (json.RawMessage, error) {
	items := make([]map[string]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		item := map[string]json.RawMessage{
			"name":         mustJSONString(t.Name),
			"input_schema": t.InputSchema,
		}
		if t.Description != "" {
			b, err := json.Marshal(t.Description)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码工具 %s 的描述失败：%w", t.Name, err)
			}
			item["description"] = b
		}
		items = append(items, item)
	}
	return json.Marshal(items)
}

// encodeAnthropicToolChoice 渲染 tool_choice，并把正向的并行语义翻回
// Anthropic 的 disable_parallel_tool_use。
func encodeAnthropicToolChoice(c ToolChoice, parallel *bool) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}

	switch c.Mode {
	case ToolChoiceAuto:
		obj["type"] = json.RawMessage(`"auto"`)
	case ToolChoiceRequired:
		obj["type"] = json.RawMessage(`"any"`)
	case ToolChoiceNone:
		obj["type"] = json.RawMessage(`"none"`)
	case ToolChoiceNamed:
		if !ir.ValidDeclaredName(c.Name) {
			return nil, fmt.Errorf("protocol: tool_choice 指定的工具名 %q 非法", c.Name)
		}
		obj["type"] = json.RawMessage(`"tool"`)
		obj["name"] = mustJSONString(c.Name)
	default:
		return nil, fmt.Errorf("protocol: 无法编码未知的 tool_choice 模式 %q", c.Mode)
	}

	if parallel != nil {
		if *parallel {
			obj["disable_parallel_tool_use"] = json.RawMessage(`false`)
		} else {
			obj["disable_parallel_tool_use"] = json.RawMessage(`true`)
		}
	}
	return json.Marshal(obj)
}

// EncodeMessagesResponse 把归一化响应渲染成 Anthropic Messages 的线上格式。
func EncodeMessagesResponse(r MessagesResponse) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	// 只在返回客户端的方向上拒绝裸文本，理由同 chat_encode 处的注释。
	if err := rejectFreeform("anthropic", r.ToolCalls); err != nil {
		return nil, err
	}
	content, err := encodeAnthropicContent(r.Content, r.ToolCalls, nil)
	if err != nil {
		return nil, err
	}
	// 响应的 content 必须是数组，即使为空。字符串形态只在请求里出现。
	if len(content) == 0 {
		content = json.RawMessage(`[]`)
	} else {
		var probe []json.RawMessage
		if err := json.Unmarshal(content, &probe); err != nil {
			var s string
			if err := json.Unmarshal(content, &s); err != nil {
				return nil, fmt.Errorf("protocol: 响应 content 形态非法：%w", err)
			}
			b, err := json.Marshal([]any{map[string]string{"type": "text", "text": s}})
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码响应 text block 失败：%w", err)
			}
			content = b
		}
	}

	out := map[string]any{
		"id":            r.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         r.Model,
		"content":       content,
		"stop_reason":   string(r.StopReason),
		"stop_sequence": nil,
	}
	if r.StopSequence != "" {
		out["stop_sequence"] = r.StopSequence
	}
	if r.Usage != nil {
		out["usage"] = r.Usage
	}
	return json.Marshal(out)
}
