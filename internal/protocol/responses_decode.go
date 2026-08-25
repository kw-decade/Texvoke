package protocol

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kw-decade/Texvoke/internal/ir"
)

var responsesKnownFields = map[string]bool{
	"model":                true,
	"input":                true,
	"instructions":         true,
	"tools":                true,
	"tool_choice":          true,
	"parallel_tool_calls":  true,
	"max_output_tokens":    true,
	"previous_response_id": true,
	"store":                true,
	"stream":               true,
}

// DecodeResponsesRequest 把一份 OpenAI Responses 请求归一化。
//
// 与 Chat Completions 的关键差异：
//   - 消息容器叫 input，里面是异构的 item 而不是清一色的 message；
//   - 工具调用与结果是独立 item，不寄居在 message 内部；
//   - 每个调用带两个 ID（item 的 id 与关联用的 call_id）；
//   - system 走顶层 instructions；
//   - 工具声明是扁平的，没有 function 包装。
func DecodeResponsesRequest(data []byte, opts DecodeOptions) (*ResponsesRequest, error) {
	if int64(len(data)) > opts.maxBytes() {
		return nil, fmt.Errorf("protocol: 请求体 %d 字节超过上限 %d", len(data), opts.maxBytes())
	}
	if opts.SessionID == "" || opts.RequestID == "" {
		return nil, fmt.Errorf("protocol: 解码需要非空的 session_id 与 request_id")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("protocol: 请求不是合法的 JSON 对象：%w", err)
	}

	req := &ResponsesRequest{}

	if raw, ok := top["model"]; ok {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return nil, fmt.Errorf("protocol: model 字段必须是字符串：%w", err)
		}
	}
	if req.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}

	if raw, ok := top["stream"]; ok {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, fmt.Errorf("protocol: stream 字段必须是布尔值：%w", err)
		}
	}
	if raw, ok := top["previous_response_id"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &req.PreviousResponseID); err != nil {
			return nil, fmt.Errorf("protocol: previous_response_id 必须是字符串：%w", err)
		}
	}
	if raw, ok := top["max_output_tokens"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &req.MaxOutputTokens); err != nil {
			return nil, fmt.Errorf("protocol: max_output_tokens 必须是整数：%w", err)
		}
		if req.MaxOutputTokens <= 0 {
			return nil, fmt.Errorf("protocol: max_output_tokens 必须为正，实际为 %d", req.MaxOutputTokens)
		}
	}
	if raw, ok := top["store"]; ok && string(raw) != "null" {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("protocol: store 字段必须是布尔值：%w", err)
		}
		req.Store = &b
	}
	if raw, ok := top["parallel_tool_calls"]; ok && string(raw) != "null" {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("protocol: parallel_tool_calls 字段必须是布尔值：%w", err)
		}
		req.ParallelToolCalls = &b
	}

	// 顺序要紧：input 里可能还藏着工具（Codex 的 additional_tools item），
	// 而 tool_choice 的校验依赖工具总数。先解 input，合并完工具再解 choice。
	tools, skippedTools, err := decodeResponsesTools(top["tools"])
	if err != nil {
		return nil, err
	}

	in, err := decodeResponsesInput(top["input"], top["instructions"], opts)
	if err != nil {
		return nil, err
	}
	req.Input = in.messages
	req.SkippedItemTypes = in.skippedItems

	if tools, err = mergeTools(tools, in.tools); err != nil {
		return nil, err
	}
	req.Tools = tools
	req.SkippedTools = append(skippedTools, in.skippedTools...)

	choice, err := decodeToolChoice(top["tool_choice"], len(tools))
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice

	for k, v := range top {
		if responsesKnownFields[k] {
			continue
		}
		if req.Extra == nil {
			req.Extra = make(map[string]json.RawMessage, 4)
		}
		req.Extra[k] = v
	}
	return req, nil
}

// decodeResponsesTools 解析工具声明。
//
// 形状是扁平的：type 与 name 平级，没有 Chat Completions 那层 function 包装。
func decodeResponsesTools(raw json.RawMessage) ([]ir.ToolDeclaration, []string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("protocol: tools 字段格式非法：%w", err)
	}

	out := make([]ir.ToolDeclaration, 0, len(items))
	var skipped []string
	for i, raw := range items {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, nil, fmt.Errorf("protocol: 第 %d 个工具格式非法：%w", i, err)
		}

		switch probe.Type {
		case "function":
			decl, err := decodeResponsesFunctionTool(raw, i)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, decl)

		case "custom":
			// 参数不是 JSON 对象的工具，Codex 的 exec 就是这一类：
			// 输入是裸 JavaScript 源码，描述里明写「not JSON」。
			var t struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, nil, fmt.Errorf("protocol: 第 %d 个工具格式非法：%w", i, err)
			}
			// 刻意不带 schema：给裸文本工具补一份空对象 schema，Prompt 就会
			// 照着教模型写 JSON，模型输出 {} 而不是脚本——不报错，只是永远
			// 调不动。format 里的 lark 语法也不放进来：那是给约束解码器用的，
			// 塞进 Prompt 只会让能力较弱的模型更困惑，描述本身已经说清楚了。
			decl := ir.ToolDeclaration{
				Name:        t.Name,
				Description: t.Description,
				InputForm:   ir.InputFormText,
			}
			if err := decl.Validate(); err != nil {
				return nil, nil, fmt.Errorf("protocol: 第 %d 个工具声明非法：%w", i, err)
			}
			out = append(out, decl)

		case "namespace":
			// 命名空间里嵌套的工具，真实调用名是 <namespace>.<tool>
			// （依据是 Codex 自己的 developer message：
			// 「using the recipient shown in their tool definitions, such as
			// to=functions.collaboration.spawn_agent」）。
			//
			// 但 ir.ValidDeclaredName 不允许点号——工具名里出现分隔符会让
			// ToolID 的字符串形式产生歧义，那道约束是安全边界，不该为一类
			// 目前零使用的工具放宽（真实历史会话统计：exec 1956 次、
			// wait 24 次、namespace 下的工具 0 次）。
			//
			// 所以跳过，并把名字报上去让接入方看得见。降级是可见的，不是静默的。
			skipped = append(skipped, probe.Name)

		default:
			// Responses 还支持 web_search、file_search、computer 等内建工具，
			// 它们由 OpenAI 自己执行。当前不支持，显式拒绝而不是当成客户端工具，
			// 后者会让 Runtime 以为自己该去执行它。
			return nil, nil, fmt.Errorf("protocol: 第 %d 个工具的 type 为 %q，暂只支持 function", i, probe.Type)
		}
	}
	return out, skipped, nil
}

func decodeResponsesFunctionTool(raw json.RawMessage, i int) (ir.ToolDeclaration, error) {
	var it struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &it); err != nil {
		return ir.ToolDeclaration{}, fmt.Errorf("protocol: 第 %d 个工具格式非法：%w", i, err)
	}
	schema := it.Parameters
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	schema, err := compactJSON(schema)
	if err != nil {
		return ir.ToolDeclaration{}, fmt.Errorf("protocol: 第 %d 个工具的 parameters %w", i, err)
	}
	decl := ir.ToolDeclaration{
		Name:        it.Name,
		Description: it.Description,
		InputSchema: schema,
	}
	if err := decl.Validate(); err != nil {
		return ir.ToolDeclaration{}, fmt.Errorf("protocol: 第 %d 个工具声明非法：%w", i, err)
	}
	return decl, nil
}

// mergeTools 把两处来源的工具合成一份，同名的以先到的为准。
//
// 需要它是因为工具不一定都在顶层 tools 字段里：Codex 把全部工具放在
// input 的第一个 additional_tools item 中，顶层压根没有 tools。
func mergeTools(dst, src []ir.ToolDeclaration) ([]ir.ToolDeclaration, error) {
	seen := make(map[string]bool, len(dst)+len(src))
	out := make([]ir.ToolDeclaration, 0, len(dst)+len(src))
	for _, d := range append(append([]ir.ToolDeclaration{}, dst...), src...) {
		if seen[d.Name] {
			return nil, fmt.Errorf("protocol: 工具名 %q 重复声明", d.Name)
		}
		seen[d.Name] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// responsesInput 是 input 解出来的东西。
//
// 用结构体而不是四个返回值：input 里除了消息，还可能带工具声明和两类
// 被跳过的名单，参数列表再长下去就没人看得懂哪个是哪个了。
type responsesInput struct {
	messages     []Message
	skippedItems []string
	tools        []ir.ToolDeclaration
	skippedTools []string
}

// decodeResponsesInput 把 instructions 与 input items 合并成统一的消息序列。
//
// 分组规则：连续的同类 item 合并成一条 Message，遇到类型变化就开新的一条。
// 编码时按同样的规则展开，因此往返无损——一份
// [msg, fc, fc, fco, msg] 会还原成完全相同的序列。
func decodeResponsesInput(raw, instructions json.RawMessage, opts DecodeOptions) (responsesInput, error) {
	var res responsesInput
	out := make([]Message, 0, 8)

	if len(instructions) > 0 && string(instructions) != "null" {
		sys, err := compactJSON(instructions)
		if err != nil {
			return res, fmt.Errorf("protocol: instructions 字段 %w", err)
		}
		out = append(out, Message{Role: RoleSystem, Content: sys})
	}

	if len(raw) == 0 || string(raw) == "null" {
		if len(out) == 0 {
			return res, fmt.Errorf("protocol: 请求缺少 input")
		}
		return res, fmt.Errorf("protocol: 请求只有 instructions 而没有 input")
	}

	// 简化形态：input 直接是一段文本，等价于一条 user 消息。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		c, err := compactJSON(raw)
		if err != nil {
			return res, fmt.Errorf("protocol: input %w", err)
		}
		res.messages = append(out, Message{Role: RoleUser, Content: c})
		return res, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return res, fmt.Errorf("protocol: input 既不是字符串也不是 item 数组：%w", err)
	}
	if len(items) == 0 {
		return res, fmt.Errorf("protocol: input 不能为空数组")
	}

	now := opts.now()
	seenCall := make(map[string]bool)

	// pending 累积连续的同类 item；类型一变就落盘成一条 Message。
	var pending *Message
	flush := func() error {
		if pending == nil {
			return nil
		}
		if err := pending.Validate(); err != nil {
			return err
		}
		out = append(out, *pending)
		pending = nil
		return nil
	}

	for i, raw := range items {
		var probe struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return res, fmt.Errorf("protocol: 第 %d 个 input item 格式非法：%w", i, err)
		}
		// 省略 type 的 item 按 message 处理，这是 Responses 的简写形式。
		kind := probe.Type
		if kind == "" && probe.Role != "" {
			kind = "message"
		}

		switch kind {
		case "message":
			if err := flush(); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 input item 之前的内容非法：%w", i, err)
			}
			var m struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 message item 格式非法：%w", i, err)
			}
			role := Role(m.Role)
			if !role.Valid() || role == RoleTool {
				return res, fmt.Errorf("protocol: 第 %d 个 message item 的角色为 %q，Responses 的工具结果应使用 function_call_output item", i, m.Role)
			}
			content, err := compactJSON(m.Content)
			if err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 message item 的 content %w", i, err)
			}
			msg := Message{Role: role, Content: content}
			// 空消息丢掉，不拒绝整个请求——理由见 chat_decode.go 同处注释。
			if msg.isEmptyContent() {
				continue
			}
			if err := msg.Validate(); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 message item 非法：%w", i, err)
			}
			out = append(out, msg)

		case "additional_tools":
			// 工具不一定在顶层 tools 字段里：Codex 把全部工具放在这个 item
			// 中，顶层压根没有 tools。以前跳过未知 item 时它被一起丢掉了，
			// 于是请求变成「一个工具都没有」而报错——好在失败是可见的。
			var at struct {
				Tools json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(raw, &at); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 additional_tools item 格式非法：%w", i, err)
			}
			tools, skipped, err := decodeResponsesTools(at.Tools)
			if err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 additional_tools item：%w", i, err)
			}
			res.tools = append(res.tools, tools...)
			res.skippedTools = append(res.skippedTools, skipped...)

		case "function_call":
			var fc struct {
				ID        string          `json:"id"`
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &fc); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call item 格式非法：%w", i, err)
			}
			if fc.CallID == "" {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call item 缺少 call_id", i)
			}
			if seenCall[fc.CallID] {
				return res, fmt.Errorf("protocol: call_id %q 重复", fc.CallID)
			}
			seenCall[fc.CallID] = true

			args, err := decodeArguments(fc.Arguments)
			if err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call item 的 %w", i, err)
			}
			p := ir.ToolCallProposal{
				SessionID:      opts.SessionID,
				RequestID:      opts.RequestID,
				CallID:         fc.CallID,
				ProtocolItemID: fc.ID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      fc.Name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          args,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate(fc.Arguments),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call item 非法：%w", i, err)
			}

			if pending == nil || pending.Role != RoleAssistant {
				if err := flush(); err != nil {
					return res, fmt.Errorf("protocol: 第 %d 个 item 之前的内容非法：%w", i, err)
				}
				pending = &Message{Role: RoleAssistant}
			}
			pending.ToolCalls = append(pending.ToolCalls, p)

		case "function_call_output":
			var fco struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(raw, &fco); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call_output item 格式非法：%w", i, err)
			}
			if fco.CallID == "" {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call_output item 缺少 call_id，无法关联回调用", i)
			}
			output, err := compactJSON(fco.Output)
			if err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 function_call_output item 的 output %w", i, err)
			}

			if pending == nil || pending.Role != RoleTool {
				if err := flush(); err != nil {
					return res, fmt.Errorf("protocol: 第 %d 个 item 之前的内容非法：%w", i, err)
				}
				pending = &Message{Role: RoleTool}
			}
			pending.ToolResults = append(pending.ToolResults, ToolResultBlock{
				CallID:  fco.CallID,
				Content: output,
			})

		case "custom_tool_call":
			// 裸文本工具的调用。与 function_call 的差别有三处：字段叫 input
			// 而不是 arguments、内容不是 JSON、item id 前缀是 ctc_。
			var ctc struct {
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Input  string `json:"input"`
			}
			if err := json.Unmarshal(raw, &ctc); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 格式非法：%w", i, err)
			}
			if ctc.CallID == "" {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 缺少 call_id", i)
			}
			if seenCall[ctc.CallID] {
				return res, fmt.Errorf("protocol: call_id %q 重复", ctc.CallID)
			}
			seenCall[ctc.CallID] = true

			p := ir.ToolCallProposal{
				SessionID:      opts.SessionID,
				RequestID:      opts.RequestID,
				CallID:         ctc.CallID,
				ProtocolItemID: ctc.ID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      ctc.Name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          ir.TextArguments(ctc.Input),
				ArgumentForm:       ir.InputFormText,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate([]byte(ctc.Input)),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 非法：%w", i, err)
			}

			if pending == nil || pending.Role != RoleAssistant {
				if err := flush(); err != nil {
					return res, fmt.Errorf("protocol: 第 %d 个 item 之前的内容非法：%w", i, err)
				}
				pending = &Message{Role: RoleAssistant}
			}
			pending.ToolCalls = append(pending.ToolCalls, p)

		case "custom_tool_call_output":
			// 注意 output 是数组（[{"type":"input_text","text":"..."}]），
			// 与 function_call_output 的字符串形态不同。原样保留，编码时
			// 靠 Freeform 标记还原成同一种 item。
			var ctco struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(raw, &ctco); err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call_output item 格式非法：%w", i, err)
			}
			if ctco.CallID == "" {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call_output item 缺少 call_id，无法关联回调用", i)
			}
			output, err := compactJSON(ctco.Output)
			if err != nil {
				return res, fmt.Errorf("protocol: 第 %d 个 custom_tool_call_output item 的 output %w", i, err)
			}

			if pending == nil || pending.Role != RoleTool {
				if err := flush(); err != nil {
					return res, fmt.Errorf("protocol: 第 %d 个 item 之前的内容非法：%w", i, err)
				}
				pending = &Message{Role: RoleTool}
			}
			pending.ToolResults = append(pending.ToolResults, ToolResultBlock{
				CallID: ctco.CallID, Content: output, Freeform: true,
			})

		case "reasoning":
			// 推理 item 携带模型的隐藏思维摘要与加密载荷。跳过它，并记一笔。
			//
			// 一开始这里是拒绝整个请求，理由是「不做静默降级以免改写模型的
			// 隐藏推理内容」。扫了 528 个真实 Codex 会话之后改掉了：
			// reasoning 是出现次数最高的 item 类型（比 message 还多），
			// 拒绝它等于宣布这个中间件只能跑第一轮。
			//
			// 跳过在这里不是「改写」，是承认表达不了：本项目的上游是纯文本
			// 模型，它既不产生 reasoning，转成 Chat 协议后也没有地方放。
			// 历史里出现的 reasoning 必然来自另一个上游，对当前这个没有意义。
			//
			// 不做的事：不把 summary 转成正文。那才是真正的改写——
			// 把模型没说出口的东西变成它公开说过的话。
			if !slices.Contains(res.skippedItems, "reasoning") {
				res.skippedItems = append(res.skippedItems, "reasoning")
			}

		case "":
			return res, fmt.Errorf("protocol: 第 %d 个 input item 既没有 type 也没有 role", i)

		default:
			// 不认识的 item 类型跳过，不拒掉整个请求。
			//
			// 客户端会带自己的扩展 item。为一个我们不认识的扩展把整轮对话
			// 挡在门外，代价远大于收益。
			//
			// reasoning 是唯一的例外，在上面单独拒绝——它携带模型的隐藏
			// 推理内容，静默丢弃等于替模型改写它没说出口的东西。
			if !slices.Contains(res.skippedItems, kind) {
				res.skippedItems = append(res.skippedItems, kind)
			}
		}
	}
	if err := flush(); err != nil {
		return res, fmt.Errorf("protocol: 末尾的 item 非法：%w", err)
	}
	if len(out) == 0 {
		return res, fmt.Errorf("protocol: input 没有产出任何消息")
	}
	res.messages = out
	return res, nil
}

// DecodeResponsesResponse 把上游返回的 Responses 响应归一化。
func DecodeResponsesResponse(data []byte, opts DecodeOptions) (*ResponsesResponse, error) {
	if int64(len(data)) > opts.maxBytes() {
		return nil, fmt.Errorf("protocol: 响应体 %d 字节超过上限 %d", len(data), opts.maxBytes())
	}
	if opts.SessionID == "" || opts.RequestID == "" {
		return nil, fmt.Errorf("protocol: 解码需要非空的 session_id 与 request_id")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("protocol: 响应不是合法的 JSON 对象：%w", err)
	}
	if upstreamErr := decodeUpstreamError(top); upstreamErr != nil {
		return nil, upstreamErr
	}

	resp := &ResponsesResponse{}
	if raw, ok := top["id"]; ok {
		if err := json.Unmarshal(raw, &resp.ID); err != nil {
			return nil, fmt.Errorf("protocol: id 字段必须是字符串：%w", err)
		}
	}
	if raw, ok := top["model"]; ok {
		if err := json.Unmarshal(raw, &resp.Model); err != nil {
			return nil, fmt.Errorf("protocol: model 字段必须是字符串：%w", err)
		}
	}
	if raw, ok := top["created_at"]; ok {
		if err := json.Unmarshal(raw, &resp.CreatedAt); err != nil {
			return nil, fmt.Errorf("protocol: created_at 字段必须是整数：%w", err)
		}
	}
	if raw, ok := top["status"]; ok && string(raw) != "null" {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("protocol: status 字段必须是字符串：%w", err)
		}
		resp.Status = ResponseStatus(s)
	}
	if raw, ok := top["incomplete_details"]; ok && string(raw) != "null" {
		var d struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("protocol: incomplete_details 格式非法：%w", err)
		}
		resp.IncompleteReason = d.Reason
	}
	if raw, ok := top["usage"]; ok && string(raw) != "null" {
		var u ResponsesUsage
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("protocol: usage 字段格式非法：%w", err)
		}
		resp.Usage = &u
	}

	rawOutput, ok := top["output"]
	if !ok {
		return nil, fmt.Errorf("protocol: 响应缺少 output")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawOutput, &items); err != nil {
		return nil, fmt.Errorf("protocol: output 必须是 item 数组：%w", err)
	}

	now := opts.now()
	seenCall := make(map[string]bool)

	for i, raw := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 个 output item 格式非法：%w", i, err)
		}

		switch probe.Type {
		case "message":
			var m struct {
				ID      string          `json:"id"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 message item 格式非法：%w", i, err)
			}
			if m.Role != "" && Role(m.Role) != RoleAssistant {
				return nil, fmt.Errorf("protocol: 第 %d 个 output message 的角色为 %q，期望 assistant", i, m.Role)
			}
			if resp.Content != nil {
				return nil, fmt.Errorf("protocol: 响应含多个 message item，当前只支持一个")
			}
			c, err := compactJSON(m.Content)
			if err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 message item 的 content %w", i, err)
			}
			resp.Content = c
			resp.MessageItemID = m.ID

		case "function_call":
			var fc struct {
				ID        string          `json:"id"`
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &fc); err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 function_call item 格式非法：%w", i, err)
			}
			if fc.CallID == "" {
				return nil, fmt.Errorf("protocol: 第 %d 个 function_call item 缺少 call_id", i)
			}
			if seenCall[fc.CallID] {
				return nil, fmt.Errorf("protocol: call_id %q 重复", fc.CallID)
			}
			seenCall[fc.CallID] = true

			args, err := decodeArguments(fc.Arguments)
			if err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 function_call item 的 %w", i, err)
			}
			p := ir.ToolCallProposal{
				SessionID:      opts.SessionID,
				RequestID:      opts.RequestID,
				CallID:         fc.CallID,
				ProtocolItemID: fc.ID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      fc.Name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          args,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate(fc.Arguments),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 function_call item 非法：%w", i, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, p)

		case "custom_tool_call":
			var ctc struct {
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Input  string `json:"input"`
			}
			if err := json.Unmarshal(raw, &ctc); err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 格式非法：%w", i, err)
			}
			if ctc.CallID == "" {
				return nil, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 缺少 call_id", i)
			}
			if seenCall[ctc.CallID] {
				return nil, fmt.Errorf("protocol: call_id %q 重复", ctc.CallID)
			}
			seenCall[ctc.CallID] = true

			p := ir.ToolCallProposal{
				SessionID:      opts.SessionID,
				RequestID:      opts.RequestID,
				CallID:         ctc.CallID,
				ProtocolItemID: ctc.ID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      ctc.Name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          ir.TextArguments(ctc.Input),
				ArgumentForm:       ir.InputFormText,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate([]byte(ctc.Input)),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("protocol: 第 %d 个 custom_tool_call item 非法：%w", i, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, p)

		case "reasoning":
			return nil, fmt.Errorf("protocol: 第 %d 个 output item 是 reasoning，暂不支持", i)

		default:
			return nil, fmt.Errorf("protocol: 第 %d 个 output item 的类型 %q 暂不支持", i, probe.Type)
		}
	}

	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}
