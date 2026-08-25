package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// anthropicKnownFields 是 MessagesRequest 显式建模的顶层字段。
var anthropicKnownFields = map[string]bool{
	"model":       true,
	"max_tokens":  true,
	"messages":    true,
	"system":      true,
	"tools":       true,
	"tool_choice": true,
	"stream":      true,
}

// DecodeMessagesRequest 把一份 Anthropic Messages 请求归一化成 MessagesRequest。
//
// 与 Chat Completions 的四处关键差异都在这里落地：
//   - system 是顶层字段而不是一条消息；
//   - 角色只有 user 与 assistant，工具结果寄居在 user 消息的 content block 里；
//   - tool_use 的 input 是真对象，不是内含 JSON 的字符串；
//   - tool_choice 永远是对象形态。
func DecodeMessagesRequest(data []byte, opts DecodeOptions) (*MessagesRequest, error) {
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

	req := &MessagesRequest{}

	if raw, ok := top["model"]; ok {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return nil, fmt.Errorf("protocol: model 字段必须是字符串：%w", err)
		}
	}
	if req.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}

	// max_tokens 在 Anthropic 是必填。缺了它上游会直接拒绝，与其把一个注定
	// 失败的请求发出去，不如在这里就报出可诊断的错误。
	raw, ok := top["max_tokens"]
	if !ok {
		return nil, fmt.Errorf("protocol: Anthropic 请求缺少必填的 max_tokens")
	}
	if err := json.Unmarshal(raw, &req.MaxTokens); err != nil {
		return nil, fmt.Errorf("protocol: max_tokens 必须是整数：%w", err)
	}
	if req.MaxTokens <= 0 {
		return nil, fmt.Errorf("protocol: max_tokens 必须为正，实际为 %d", req.MaxTokens)
	}

	if raw, ok := top["stream"]; ok {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, fmt.Errorf("protocol: stream 字段必须是布尔值：%w", err)
		}
	}

	tools, err := decodeAnthropicTools(top["tools"])
	if err != nil {
		return nil, err
	}
	req.Tools = tools

	choice, parallel, err := decodeAnthropicToolChoice(top["tool_choice"], len(tools))
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice
	req.ParallelToolCalls = parallel

	msgs, err := decodeAnthropicMessages(top["messages"], top["system"], opts)
	if err != nil {
		return nil, err
	}
	req.Messages = msgs

	for k, v := range top {
		if anthropicKnownFields[k] {
			continue
		}
		if req.Extra == nil {
			req.Extra = make(map[string]json.RawMessage, 4)
		}
		req.Extra[k] = v
	}
	return req, nil
}

// decodeAnthropicTools 解析工具声明。
//
// 形状比 Chat Completions 扁一层：没有 type/function 包装，
// 参数 schema 的键名是 input_schema 而不是 parameters。
func decodeAnthropicTools(raw json.RawMessage) ([]ir.ToolDeclaration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Type        string          `json:"type"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("protocol: tools 字段格式非法：%w", err)
	}

	seen := make(map[string]bool, len(items))
	out := make([]ir.ToolDeclaration, 0, len(items))
	for i, it := range items {
		// Anthropic 的服务端工具（web_search、computer 等）带 type 字段，
		// 由 Anthropic 自己执行，不经过本 Runtime。当前不支持，显式拒绝
		// 而不是当成普通客户端工具——后者会让 Runtime 以为自己该去执行它。
		if it.Type != "" {
			return nil, fmt.Errorf("protocol: 第 %d 个工具带 type=%q，暂不支持 Anthropic 服务端工具", i, it.Type)
		}
		if seen[it.Name] {
			return nil, fmt.Errorf("protocol: 工具名 %q 重复声明", it.Name)
		}
		seen[it.Name] = true

		schema := it.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		schema, err := compactJSON(schema)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 个工具的 input_schema %w", i, err)
		}

		decl := ir.ToolDeclaration{
			Name:        it.Name,
			Description: it.Description,
			InputSchema: schema,
		}
		if err := decl.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 个工具声明非法：%w", i, err)
		}
		out = append(out, decl)
	}
	return out, nil
}

// decodeAnthropicToolChoice 解析 tool_choice，并把 disable_parallel_tool_use
// 翻转成与 Chat Completions 一致的正向语义。
//
// 返回的第二个值为 nil 表示客户端没有就并行调用表态。
func decodeAnthropicToolChoice(raw json.RawMessage, toolCount int) (ToolChoice, *bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		// 与 Chat Completions 同样的缺省语义：没有工具时是 none 而不是 auto，
		// 免得把 client_capability_missing 误判成模型不肯调用。
		if toolCount == 0 {
			return ToolChoice{Mode: ToolChoiceNone}, nil, nil
		}
		return ToolChoice{Mode: ToolChoiceAuto}, nil, nil
	}

	var obj struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ToolChoice{}, nil, fmt.Errorf("protocol: tool_choice 必须是对象（Anthropic 的既定形状）：%w", err)
	}

	var parallel *bool
	if obj.DisableParallelToolUse != nil {
		v := !*obj.DisableParallelToolUse
		parallel = &v
	}

	switch obj.Type {
	case "auto":
		return ToolChoice{Mode: ToolChoiceAuto}, parallel, nil
	case "any":
		return ToolChoice{Mode: ToolChoiceRequired}, parallel, nil
	case "none":
		return ToolChoice{Mode: ToolChoiceNone}, parallel, nil
	case "tool":
		if !ir.ValidDeclaredName(obj.Name) {
			return ToolChoice{}, nil, fmt.Errorf("protocol: tool_choice 指定的工具名 %q 非法", obj.Name)
		}
		return ToolChoice{Mode: ToolChoiceNamed, Name: obj.Name}, parallel, nil
	default:
		return ToolChoice{}, nil, fmt.Errorf("protocol: 未知的 tool_choice 类型 %q，不做静默降级", obj.Type)
	}
}

// decodeAnthropicMessages 把顶层 system 与 messages 合并成统一的消息序列。
//
// system 转成序列首位的一条 system 消息，让下游对三种协议看到同一种形状；
// 编码回 Anthropic 时再还原成顶层字段。
func decodeAnthropicMessages(raw, system json.RawMessage, opts DecodeOptions) ([]Message, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("protocol: 请求缺少 messages")
	}
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("protocol: messages 字段格式非法：%w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("protocol: messages 不能为空数组")
	}

	out := make([]Message, 0, len(items)+1)

	if len(system) > 0 && string(system) != "null" {
		sys, err := compactJSON(system)
		if err != nil {
			return nil, fmt.Errorf("protocol: system 字段 %w", err)
		}
		out = append(out, Message{Role: RoleSystem, Content: sys})
	}

	for i, it := range items {
		role := Role(it.Role)
		// 官方 Anthropic 只认 user 与 assistant，system 必须在顶层字段。
		// 但 Claude Code 等真实客户端会直接把 system 放进 messages（CC Switch
		// 转协议时也会），与 Chat / Responses 两协议已接受的形状一致，2026-08-24
		// 实测撞到。这里容忍 system，内容并入统一序列——它仍是指令来源，走与
		// 顶层 system 相同的数据路径。其余外来角色（tool / developer / function）
		// 仍拒绝：那才是调用方把别的协议形状发过来的明确信号。
		if role != RoleUser && role != RoleAssistant && role != RoleSystem {
			return nil, fmt.Errorf("protocol: 第 %d 条消息的角色为 %q，Anthropic 只支持 user、assistant 与 system", i, it.Role)
		}

		content, calls, results, err := decodeAnthropicContent(it.Content, opts)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：%w", i, err)
		}
		if role == RoleSystem && (len(calls) > 0 || len(results) > 0) {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：system 消息不得包含 tool block", i)
		}
		if role == RoleUser && len(calls) > 0 {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：user 消息不得包含 tool_use block", i)
		}
		if role == RoleAssistant && len(results) > 0 {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：assistant 消息不得包含 tool_result block", i)
		}

		msg := Message{
			Role:        role,
			Content:     content,
			ToolCalls:   calls,
			ToolResults: results,
		}
		// 空消息丢掉，不拒绝整个请求——理由见 chat_decode.go 同处注释。
		if msg.isEmptyContent() && len(msg.ToolCalls) == 0 && len(msg.ToolResults) == 0 {
			continue
		}
		if err := msg.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息非法：%w", i, err)
		}
		out = append(out, msg)
	}
	// 丢完一条不剩，说明整个请求只有噪声——见 chat_decode.go 同处注释。
	if len(out) == 0 {
		return nil, fmt.Errorf("protocol: messages 里没有一条带内容的消息")
	}
	return out, nil
}

// decodeAnthropicContent 拆分一条消息的 content。
//
// 返回的 Content 只保留非工具 block（text、image、thinking、服务端工具结果
// 等一律原样不动），tool_use 与 tool_result 抽成结构化字段。
//
// ponytail: 抽取会把 block 顺序规范化成「其余内容在前，工具调用在后」。
// Claude 的实际输出本来就是这个顺序（先说话再调用），所以往返在现实语料上
// 是保真的；但一份 [tool_use, text] 的输入编码回去会变成 [text, tool_use]。
// 若将来发现顺序确实携带语义，改法是给 ToolCallProposal 记录原始下标，
// 编码时按下标插回原位。
func decodeAnthropicContent(raw json.RawMessage, opts DecodeOptions) (json.RawMessage, []ir.ToolCallProposal, []ToolResultBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil, nil
	}

	// 字符串形态：整条消息就是一段文本，不可能含工具 block。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		c, err := compactJSON(raw)
		return c, nil, nil, err
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, nil, fmt.Errorf("content 既不是字符串也不是 block 数组：%w", err)
	}

	var (
		kept    []json.RawMessage
		calls   []ir.ToolCallProposal
		results []ToolResultBlock
	)
	now := opts.now()
	seenCall := make(map[string]bool)

	for i, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, nil, nil, fmt.Errorf("第 %d 个 content block 格式非法：%w", i, err)
		}
		if probe.Type == "" {
			return nil, nil, nil, fmt.Errorf("第 %d 个 content block 缺少 type", i)
		}

		switch probe.Type {
		case "tool_use":
			var tu struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(b, &tu); err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 格式非法：%w", i, err)
			}
			if tu.ID == "" {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 缺少 id", i)
			}
			if seenCall[tu.ID] {
				return nil, nil, nil, fmt.Errorf("tool_use id %q 重复", tu.ID)
			}
			seenCall[tu.ID] = true

			// input 是真正的 JSON 对象，不像 Chat Completions 那样再包一层
			// 字符串。仍然要校验它确实是对象——解析不出就报错，不兜底。
			if len(tu.Input) == 0 || string(tu.Input) == "null" {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 缺少 input，无参数应写成 {}", i)
			}
			var probeObj map[string]json.RawMessage
			if err := json.Unmarshal(tu.Input, &probeObj); err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 的 input 不是 JSON 对象：%w", i, err)
			}
			args, err := compactJSON(tu.Input)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 的 input %w", i, err)
			}

			p := ir.ToolCallProposal{
				SessionID: opts.SessionID,
				RequestID: opts.RequestID,
				CallID:    tu.ID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      tu.Name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          args,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate(tu.Input),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_use block 非法：%w", i, err)
			}
			calls = append(calls, p)

		case "tool_result":
			var tr struct {
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			}
			if err := json.Unmarshal(b, &tr); err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_result block 格式非法：%w", i, err)
			}
			if tr.ToolUseID == "" {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_result block 缺少 tool_use_id，无法关联回调用", i)
			}
			c, err := compactJSON(tr.Content)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 tool_result block 的 content %w", i, err)
			}
			results = append(results, ToolResultBlock{
				CallID:  tr.ToolUseID,
				Content: c,
				IsError: tr.IsError,
			})

		default:
			// text、image、document、thinking、server_tool_use 等一律原样保留。
			// 规格九章要求不得为了解析而伪造或泄漏隐藏思维内容，所以 thinking
			// block 在这里只是被搬运，不被读取也不被改写。
			c, err := compactJSON(b)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("第 %d 个 content block %w", i, err)
			}
			kept = append(kept, c)
		}
	}

	var content json.RawMessage
	if len(kept) > 0 {
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("重新编码 content block 失败：%w", err)
		}
		content = b
	}
	return content, calls, results, nil
}

// DecodeMessagesResponse 把上游返回的 Anthropic Messages 响应归一化。
//
// 与 Chat Completions 版同理：上游返回结构化错误时返回 *UpstreamError，
// 响应形状非法则返回普通错误。混为一谈会让「供应商禁止工具调用」
// 被误报成「解析器有 bug」，查错方向整个跑偏。
func DecodeMessagesResponse(data []byte, opts DecodeOptions) (*MessagesResponse, error) {
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

	resp := &MessagesResponse{}
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
	if raw, ok := top["role"]; ok {
		var role string
		if err := json.Unmarshal(raw, &role); err != nil {
			return nil, fmt.Errorf("protocol: role 字段必须是字符串：%w", err)
		}
		if role != "" && Role(role) != RoleAssistant {
			return nil, fmt.Errorf("protocol: 响应消息的角色为 %q，期望 assistant", role)
		}
	}
	if raw, ok := top["stop_reason"]; ok && string(raw) != "null" {
		var sr string
		if err := json.Unmarshal(raw, &sr); err != nil {
			return nil, fmt.Errorf("protocol: stop_reason 字段必须是字符串：%w", err)
		}
		resp.StopReason = StopReason(sr)
	}
	if raw, ok := top["stop_sequence"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &resp.StopSequence); err != nil {
			return nil, fmt.Errorf("protocol: stop_sequence 字段必须是字符串：%w", err)
		}
	}
	if raw, ok := top["usage"]; ok && string(raw) != "null" {
		var u AnthropicUsage
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("protocol: usage 字段格式非法：%w", err)
		}
		resp.Usage = &u
	}

	rawContent, ok := top["content"]
	if !ok {
		return nil, fmt.Errorf("protocol: 响应缺少 content")
	}
	content, calls, results, err := decodeAnthropicContent(rawContent, opts)
	if err != nil {
		return nil, fmt.Errorf("protocol: 响应 content：%w", err)
	}
	// 助手的回复里不该出现工具结果——那是调用方回报给模型的东西。
	// 出现了说明中间有代理在改写响应。
	if len(results) > 0 {
		return nil, fmt.Errorf("protocol: 响应中不应出现 tool_result block")
	}
	resp.Content = content
	resp.ToolCalls = calls

	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}
