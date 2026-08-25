package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// DefaultMaxRequestBytes 是单个请求体的默认上限。
//
// 规格三章把「readBody 无上限拼接请求体」列为必须纠正的问题：没有上限，
// 一个构造的大请求就能把进程内存吃光。
const DefaultMaxRequestBytes = 32 << 20 // 32 MiB

// DecodeOptions 是解码时需要外部提供的上下文。
type DecodeOptions struct {
	// SessionID 与 RequestID 会写进解析出的每个 ToolCallProposal。
	SessionID string
	RequestID string

	// Now 用于给历史消息里的调用提案打时间戳。留零值则取当前时间；
	// 测试传固定值可以让 Golden 比对稳定。
	Now time.Time

	// MaxBytes 为 0 时取 DefaultMaxRequestBytes。
	MaxBytes int64
}

func (o DecodeOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

func (o DecodeOptions) maxBytes() int64 {
	if o.MaxBytes <= 0 {
		return DefaultMaxRequestBytes
	}
	return o.MaxBytes
}

// compactJSON 去掉 JSON 值里的无意义空白，让解码结果与书写格式无关。
//
// 为什么要做：客户端发来的 JSON 可能是缩进的，也可能是紧凑的，而
// json.RawMessage 保留原始字节。不压缩的话，同一份请求换个缩进风格，
// 解码结果的字节就不同——往返因此不再幂等，参数的幂等键也会随排版变化，
// 让账本把同一次重试当成新调用。
//
// 压缩只删除空白，不动键序、不动数值精度、不动转义，因此不改变任何语义。
// 它不是完整的 canonical JSON（键序仍按原样保留），但消除了实践中最常见的
// 那类差异。
func compactJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("内容不是合法 JSON：%w", err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// 这些是 ChatRequest 显式建模的顶层字段，解码时从原始 map 中取走，
// 剩下的进 Extra 原样透传。
var chatKnownFields = map[string]bool{
	"model":               true,
	"messages":            true,
	"tools":               true,
	"tool_choice":         true,
	"parallel_tool_calls": true,
	"stream":              true,
	"n":                   true, // 取走后单独校验，不透传
}

// DecodeChatRequest 把一份 Chat Completions 请求归一化成 ChatRequest。
//
// 解码严格：未知的 tool_choice 取值、非 function 类型的工具、无法解析的
// 调用参数一律报错，不做静默降级。规格八章明确要求「任何未知枚举、未知字段或
// 不兼容组合都要报错或记录明确降级，不能静默转成 auto」。
func DecodeChatRequest(data []byte, opts DecodeOptions) (*ChatRequest, error) {
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

	req := &ChatRequest{}

	if raw, ok := top["model"]; ok {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return nil, fmt.Errorf("protocol: model 字段必须是字符串：%w", err)
		}
	}
	if req.Model == "" {
		return nil, fmt.Errorf("protocol: 请求缺少 model")
	}

	// n > 1 会产生多个候选回复。在工具调用场景下这没有明确语义——几个候选
	// 各自提出了不同的调用，执行哪个？静默取第一个等于替客户端做了它没授权的
	// 决定，所以显式拒绝。
	if raw, ok := top["n"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("protocol: n 字段必须是整数：%w", err)
		}
		if n != 1 {
			return nil, fmt.Errorf("protocol: 暂不支持 n=%d，工具调用场景下多候选语义不明确", n)
		}
	}

	if raw, ok := top["stream"]; ok {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, fmt.Errorf("protocol: stream 字段必须是布尔值：%w", err)
		}
	}
	if raw, ok := top["parallel_tool_calls"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("protocol: parallel_tool_calls 字段必须是布尔值：%w", err)
		}
		req.ParallelToolCalls = &b
	}

	tools, err := decodeTools(top["tools"])
	if err != nil {
		return nil, err
	}
	req.Tools = tools

	choice, err := decodeToolChoice(top["tool_choice"], len(tools))
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice

	msgs, err := decodeMessages(top["messages"], opts)
	if err != nil {
		return nil, err
	}
	req.Messages = msgs

	// 剩余字段原样保留，供路由层透传给上游。
	for k, v := range top {
		if chatKnownFields[k] {
			continue
		}
		if req.Extra == nil {
			req.Extra = make(map[string]json.RawMessage, 4)
		}
		req.Extra[k] = v
	}
	return req, nil
}

// decodeTools 解析 tools 数组。缺省或 null 返回 nil，这是合法的
// （tools=0 本身是需要向上报告的信号，不是错误）。
func decodeTools(raw json.RawMessage) ([]ir.ToolDeclaration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("protocol: tools 字段格式非法：%w", err)
	}

	seen := make(map[string]bool, len(items))
	out := make([]ir.ToolDeclaration, 0, len(items))
	for i, it := range items {
		if it.Type != "function" {
			return nil, fmt.Errorf("protocol: 第 %d 个工具的 type 为 %q，只支持 function", i, it.Type)
		}
		// 工具名重复会让「精确匹配」失去意义：同名的两份 schema，
		// 解析出的调用该按哪一份校验无从判断。
		if seen[it.Function.Name] {
			return nil, fmt.Errorf("protocol: 工具名 %q 重复声明", it.Function.Name)
		}
		seen[it.Function.Name] = true

		decl := ir.ToolDeclaration{
			Name:        it.Function.Name,
			Description: it.Function.Description,
			InputSchema: it.Function.Parameters,
		}
		// 参数 schema 缺省时按「无参数对象」处理：这是 OpenAI 的既定语义，
		// 不是我们在补默认值。
		if len(decl.InputSchema) == 0 {
			decl.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		// schema 只压缩空白，类型、必填、枚举、默认值和额外属性语义
		// 一律不动——它仍然是校验时的权威定义。
		schema, err := compactJSON(decl.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 个工具的参数 schema %w", i, err)
		}
		decl.InputSchema = schema

		if err := decl.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 个工具声明非法：%w", i, err)
		}
		out = append(out, decl)
	}
	return out, nil
}

// decodeToolChoice 解析 tool_choice。
//
// 缺省语义按 OpenAI 规范：声明了工具则为 auto，没有工具则等价于 none。
// 后者很重要——没有工具却报 auto，会让下游误以为「模型可以调用工具但没调」，
// 从而把 client_capability_missing 误判成 persona_refusal。
func decodeToolChoice(raw json.RawMessage, toolCount int) (ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if toolCount == 0 {
			return ToolChoice{Mode: ToolChoiceNone}, nil
		}
		return ToolChoice{Mode: ToolChoiceAuto}, nil
	}

	// 形式一：字符串枚举。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch ToolChoiceMode(s) {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			return ToolChoice{Mode: ToolChoiceMode(s)}, nil
		case "any":
			// Anthropic 用 any 表达「至少一个」，部分客户端会混用。
			// 明确映射而不是当成未知值，但不接受其余别名。
			return ToolChoice{Mode: ToolChoiceRequired}, nil
		default:
			return ToolChoice{}, fmt.Errorf("protocol: 未知的 tool_choice 取值 %q，不做静默降级", s)
		}
	}

	// 形式二：指定具体工具的对象。
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return ToolChoice{}, fmt.Errorf("protocol: tool_choice 既不是字符串也不是合法对象：%w", err)
	}
	if named.Type != "function" {
		return ToolChoice{}, fmt.Errorf("protocol: tool_choice 对象的 type 为 %q，只支持 function", named.Type)
	}
	if !ir.ValidDeclaredName(named.Function.Name) {
		return ToolChoice{}, fmt.Errorf("protocol: tool_choice 指定的工具名 %q 非法", named.Function.Name)
	}
	return ToolChoice{Mode: ToolChoiceNamed, Name: named.Function.Name}, nil
}

// decodeMessages 解析 messages 数组。
func decodeMessages(raw json.RawMessage, opts DecodeOptions) ([]Message, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("protocol: 请求缺少 messages")
	}
	var items []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name"`
		Refusal    string          `json:"refusal"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("protocol: messages 字段格式非法：%w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("protocol: messages 不能为空数组")
	}

	out := make([]Message, 0, len(items))
	for i, it := range items {
		role := Role(it.Role)

		// Chat Completions 里工具结果只能由 tool 消息承载。内部的 Message
		// 允许 user 也带结果（那是 Anthropic 的形状），所以这条协议专属的
		// 收窄要在解码器里做，不能指望通用的 Message.Validate。
		if it.ToolCallID != "" && role != RoleTool {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：只有 tool 消息能携带 tool_call_id，实际角色为 %q", i, it.Role)
		}
		if role == RoleTool && it.ToolCallID == "" {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：tool 消息缺少 tool_call_id，无法关联到具体调用", i)
		}

		content, err := compactJSON(it.Content)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息的 content %w", i, err)
		}
		msg := Message{
			Role:    role,
			Content: content,
			Name:    it.Name,
			Refusal: it.Refusal,
		}
		// 一条 tool 消息恰好承载一个结果：tool_call_id 是关联，content 是内容。
		// 内容移进 ToolResults 后清空 Content，避免同一份数据存两处、
		// 编码时不知道该信哪个。
		if role == RoleTool {
			msg.ToolResults = []ToolResultBlock{{CallID: it.ToolCallID, Content: content}}
			msg.Content = nil
		}

		calls, err := decodeToolCalls(it.ToolCalls, opts)
		if err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息：%w", i, err)
		}
		msg.ToolCalls = calls

		// 空消息丢掉，不拒绝整个请求。
		//
		// 真实客户端会发内容为空的 system 消息——Codex 经 CC Switch 转协议
		// 时就会产生一条。空消息不携带任何信息，丢掉是无损的；为它拒掉整个
		// 请求，等于让一段无害的噪声把可用的对话挡在门外。
		//
		// tool 消息除外：它没有结果就是真的坏了，那条 Validate 会拦下来。
		if msg.isEmptyContent() && len(msg.ToolCalls) == 0 &&
			len(msg.ToolResults) == 0 && msg.Refusal == "" && role != RoleTool {
			continue
		}

		if err := msg.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: 第 %d 条消息非法：%w", i, err)
		}
		out = append(out, msg)
	}
	// 丢完一条不剩，说明整个请求只有噪声。这时候不能装作没事——发一个
	// 没有对话的请求给上游，换回来的东西没人能解释。
	if len(out) == 0 {
		return nil, fmt.Errorf("protocol: messages 里没有一条带内容的消息")
	}
	return out, nil
}

// decodeToolCalls 解析 assistant 消息里的 tool_calls，还原成 IR 调用提案。
func decodeToolCalls(raw json.RawMessage, opts DecodeOptions) ([]ir.ToolCallProposal, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
			// Arguments 在 Chat Completions 里是一个「内含 JSON 的字符串」，
			// 不是 JSON 对象。这是该协议的既定形状，解码要多剥一层。
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("tool_calls 格式非法：%w", err)
	}

	now := opts.now()
	seen := make(map[string]bool, len(items))
	out := make([]ir.ToolCallProposal, 0, len(items))
	for i, it := range items {
		if it.Type != "function" {
			return nil, fmt.Errorf("第 %d 个 tool_call 的 type 为 %q，只支持 function", i, it.Type)
		}
		// 重复的 call ID 会让结果无法唯一关联回调用。规格九章要求这种歧义
		// 按策略拒绝，而不是取最后一个掩盖过去。
		if seen[it.ID] {
			return nil, fmt.Errorf("第 %d 个 tool_call 的 id %q 重复", i, it.ID)
		}
		seen[it.ID] = true

		args, err := decodeArguments(it.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 tool_call（id=%s）：%w", i, it.ID, err)
		}

		p := ir.ToolCallProposal{
			SessionID: opts.SessionID,
			RequestID: opts.RequestID,
			CallID:    it.ID,
			Tool: ir.ToolID{
				Namespace: ir.NamespaceClient,
				Name:      it.Function.Name,
				Version:   ir.VersionDeclared,
			},
			Arguments:          args,
			Source:             ir.SourceNative,
			RawCandidateDigest: ir.DigestRawCandidate(it.Function.Arguments),
			CreatedAt:          now,
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("第 %d 个 tool_call 非法：%w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// decodeArguments 把 Chat Completions 的字符串型 arguments 还原成 JSON 对象。
//
// 这里是规格三章那条硬要求的落点：解析失败必须是显式错误，绝不能把原文塞进
// 一个 _raw 兜底字段后继续往下走——那样等于让一段没被理解的文本进入执行路径。
func decodeArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("arguments 缺失，无参数应显式写成字符串 \"{}\"")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("arguments 必须是字符串（Chat Completions 的既定形状）：%w", err)
	}
	if s == "" {
		return nil, fmt.Errorf("arguments 是空字符串，无参数应写成 \"{}\"")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return nil, fmt.Errorf("arguments 内容不是 JSON 对象：%w", err)
	}
	// 压缩空白后再交给下游：参数的幂等键按字节计算，排版差异不应让
	// 同一次重试算出不同的键。
	return compactJSON(json.RawMessage(s))
}

// DecodeChatResponse 把上游返回的 Chat Completions 响应归一化成 ChatResponse。
//
// 这是 Level 1 原生透传的入口：上游原生返回了结构化工具调用时，把它们还原成
// IR 提案，再经同一条策略路径处理。规格六章要求原生调用同样过安全门，
// 所以这里只负责还原形状，不做任何放行判断。
//
// 上游返回结构化错误时返回 *UpstreamError，调用方可以据此做拒绝分类；
// 响应形状本身非法则返回普通错误。两者要分开，否则「供应商禁止工具调用」
// 会被误报成「解析器有 bug」。
func DecodeChatResponse(data []byte, opts DecodeOptions) (*ChatResponse, error) {
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

	resp := &ChatResponse{}
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
	if raw, ok := top["created"]; ok {
		if err := json.Unmarshal(raw, &resp.Created); err != nil {
			return nil, fmt.Errorf("protocol: created 字段必须是整数：%w", err)
		}
	}
	if raw, ok := top["usage"]; ok && string(raw) != "null" {
		var u Usage
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("protocol: usage 字段格式非法：%w", err)
		}
		resp.Usage = &u
	}

	rawChoices, ok := top["choices"]
	if !ok || len(rawChoices) == 0 || string(rawChoices) == "null" {
		return nil, fmt.Errorf("protocol: 响应缺少 choices")
	}
	var choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Refusal   string          `json:"refusal"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return nil, fmt.Errorf("protocol: choices 字段格式非法：%w", err)
	}
	switch {
	case len(choices) == 0:
		return nil, fmt.Errorf("protocol: 响应的 choices 为空数组")
	case len(choices) > 1:
		// 静默取第一个等于替调用方做了它没授权的决定：几个候选各自提出了
		// 不同的调用，执行哪个？请求侧已经拒绝 n>1，这里出现多候选说明
		// 上游没有遵守，必须报出来。
		return nil, fmt.Errorf("protocol: 响应含 %d 个 choice，工具调用场景下多候选语义不明确", len(choices))
	}

	c := choices[0]
	if c.Message.Role != "" && Role(c.Message.Role) != RoleAssistant {
		return nil, fmt.Errorf("protocol: 响应消息的角色为 %q，期望 assistant", c.Message.Role)
	}

	content, err := compactJSON(c.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("protocol: 响应 content %w", err)
	}
	if string(content) == "null" {
		content = nil
	}
	resp.Content = content
	resp.Refusal = c.Message.Refusal

	calls, err := decodeToolCalls(c.Message.ToolCalls, opts)
	if err != nil {
		return nil, fmt.Errorf("protocol: 响应的 %w", err)
	}
	resp.ToolCalls = calls
	resp.FinishReason = FinishReason(c.FinishReason)

	// 复用编码前的自洽性校验：上游若把 finish_reason 与 tool_calls 报得
	// 互相矛盾，问题要在入口暴露，而不是等它污染了下游状态机再排查。
	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}
