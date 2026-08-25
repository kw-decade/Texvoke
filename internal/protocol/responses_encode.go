package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// EncodeResponsesRequest 把归一化请求渲染回 Responses 的线上格式。
//
// system 消息统一写成 input 里的 message item，不使用顶层 instructions——
// 见 ResponsesRequest 的说明：两者语义等价，统一到一处换来往返幂等。
func EncodeResponsesRequest(r ResponsesRequest) ([]byte, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}
	if len(r.Input) == 0 {
		return nil, fmt.Errorf("protocol: 请求缺少 input")
	}

	out := make(map[string]json.RawMessage, len(r.Extra)+8)
	for k, v := range r.Extra {
		if responsesKnownFields[k] {
			return nil, fmt.Errorf("protocol: Extra 不得包含已建模字段 %q", k)
		}
		out[k] = v
	}

	var err error
	if out["model"], err = json.Marshal(r.Model); err != nil {
		return nil, fmt.Errorf("protocol: 编码 model 失败：%w", err)
	}
	if out["input"], err = encodeResponsesInput(r.Input); err != nil {
		return nil, err
	}
	if len(r.Tools) > 0 {
		if out["tools"], err = encodeResponsesTools(r.Tools); err != nil {
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
	if r.MaxOutputTokens > 0 {
		if out["max_output_tokens"], err = json.Marshal(r.MaxOutputTokens); err != nil {
			return nil, fmt.Errorf("protocol: 编码 max_output_tokens 失败：%w", err)
		}
	}
	if r.PreviousResponseID != "" {
		if out["previous_response_id"], err = json.Marshal(r.PreviousResponseID); err != nil {
			return nil, fmt.Errorf("protocol: 编码 previous_response_id 失败：%w", err)
		}
	}
	if r.Store != nil {
		if out["store"], err = json.Marshal(*r.Store); err != nil {
			return nil, fmt.Errorf("protocol: 编码 store 失败：%w", err)
		}
	}
	if r.Stream {
		out["stream"] = json.RawMessage(`true`)
	}
	return json.Marshal(out)
}

// encodeResponsesInput 把消息序列展开成异构的 item 数组。
//
// 这是解码时分组规则的逆操作：一条带 N 个调用的 assistant 消息展开成 N 个
// function_call item，一条带 N 个结果的 tool 消息展开成 N 个
// function_call_output item。因为分组规则确定，展开后再解码能回到原样。
func encodeResponsesInput(msgs []Message) (json.RawMessage, error) {
	items := make([]json.RawMessage, 0, len(msgs))

	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息非法：%w", i, err)
		}

		// 有正文就先写一条 message item；工具调用与结果随后各自成 item。
		if m.hasContent() {
			item := map[string]json.RawMessage{
				"type":    json.RawMessage(`"message"`),
				"role":    mustJSONRole(m.Role),
				"content": m.Content,
			}
			raw, err := json.Marshal(item)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码第 %d 条消息失败：%w", i, err)
			}
			items = append(items, raw)
		}

		for _, c := range m.ToolCalls {
			item, err := responsesCallItem(c)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(item)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码调用 item 失败：%w", err)
			}
			items = append(items, raw)
		}

		for _, r := range m.ToolResults {
			// ponytail: is_error 在 Responses 里同样没有对应字段，与 Chat 一样
			// 选择显式报错而非静默丢弃。等实现跨协议路由时改成记录一次降级。
			if r.IsError {
				return nil, fmt.Errorf("protocol: 第 %d 条消息：Responses 无法表达 is_error 标记", i)
			}
			typ := `"function_call_output"`
			if r.Freeform {
				typ = `"custom_tool_call_output"`
			}
			item := map[string]json.RawMessage{
				"type": json.RawMessage(typ),
			}
			b, err := json.Marshal(r.CallID)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码 call_id 失败：%w", err)
			}
			item["call_id"] = b
			switch {
			case len(r.Content) > 0:
				item["output"] = r.Content
			case r.Freeform:
				// 裸文本工具的结果是 content part 数组，空结果就是空数组。
				item["output"] = json.RawMessage(`[]`)
			default:
				item["output"] = json.RawMessage(`""`)
			}
			raw, err := json.Marshal(item)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码工具结果 item 失败：%w", err)
			}
			items = append(items, raw)
		}
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("protocol: input 没有产出任何 item")
	}
	return json.Marshal(items)
}

// responsesCallItem 把一次调用渲染成对应的 item。
//
// 两种形态用的是两种 item：常规工具是 function_call（参数在 arguments，
// 内容是 JSON 文本），裸文本工具是 custom_tool_call（内容在 input，
// 是原样的文本）。渲染成错的那种，客户端要么解析失败，要么把一段脚本
// 当成 JSON 去解——后者更糟，因为它会静默失败。
func responsesCallItem(c ir.ToolCallProposal) (map[string]json.RawMessage, error) {
	item := map[string]json.RawMessage{
		"name": mustJSONString(c.Tool.Name),
	}
	if c.ArgumentForm.Text() {
		text, err := c.ArgumentsText()
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码调用 %s 失败：%w", c.CallID, err)
		}
		b, err := json.Marshal(text)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码调用 %s 的输入失败：%w", c.CallID, err)
		}
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		item["input"] = b
	} else {
		argStr, err := json.Marshal(string(c.Arguments))
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码调用 %s 的参数失败：%w", c.CallID, err)
		}
		item["type"] = json.RawMessage(`"function_call"`)
		item["arguments"] = argStr
	}
	b, err := json.Marshal(c.CallID)
	if err != nil {
		return nil, fmt.Errorf("protocol: 编码 call_id 失败：%w", err)
	}
	item["call_id"] = b
	// item id 只在上游给过的时候写回。凭空生成一个会让客户端拿到
	// 一个它从未见过、也无法与流式事件对上的 ID。
	if c.ProtocolItemID != "" {
		b, err := json.Marshal(c.ProtocolItemID)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码 item id 失败：%w", err)
		}
		item["id"] = b
	}
	return item, nil
}

func encodeResponsesTools(tools []ir.ToolDeclaration) (json.RawMessage, error) {
	items := make([]map[string]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		item := map[string]json.RawMessage{
			"name": mustJSONString(t.Name),
		}
		if t.InputForm.Text() {
			// custom 工具没有 parameters。写一个空 schema 上去，上游会以为
			// 它收 JSON 对象。
			item["type"] = json.RawMessage(`"custom"`)
		} else {
			item["type"] = json.RawMessage(`"function"`)
			item["parameters"] = t.InputSchema
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

// EncodeResponsesResponse 把归一化响应渲染成 Responses 的线上格式。
func EncodeResponsesResponse(r ResponsesResponse) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	output := make([]json.RawMessage, 0, len(r.ToolCalls)+1)

	if len(r.Content) > 0 && string(r.Content) != "null" {
		item := map[string]json.RawMessage{
			"type":    json.RawMessage(`"message"`),
			"role":    json.RawMessage(`"assistant"`),
			"status":  json.RawMessage(`"completed"`),
			"content": r.Content,
		}
		if r.MessageItemID != "" {
			b, err := json.Marshal(r.MessageItemID)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码 message item id 失败：%w", err)
			}
			item["id"] = b
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码 message item 失败：%w", err)
		}
		output = append(output, raw)
	}

	for _, c := range r.ToolCalls {
		item, err := responsesCallItem(c)
		if err != nil {
			return nil, err
		}
		item["status"] = json.RawMessage(`"completed"`)
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码调用 item 失败：%w", err)
		}
		output = append(output, raw)
	}

	out := map[string]any{
		"id":         r.ID,
		"object":     "response",
		"created_at": r.CreatedAt,
		"status":     string(r.Status),
		"model":      r.Model,
		"output":     output,
	}
	if r.IncompleteReason != "" {
		out["incomplete_details"] = map[string]string{"reason": r.IncompleteReason}
	} else {
		out["incomplete_details"] = nil
	}
	if r.Usage != nil {
		out["usage"] = r.Usage
	}
	return json.Marshal(out)
}
